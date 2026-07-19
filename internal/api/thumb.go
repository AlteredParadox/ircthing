// ircthing — a self-hosted, always-connected web IRC client.
// Copyright (C) 2026 AlteredParadox
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or (at your
// option) any later version.
//
// This program is distributed in the hope that it will be useful, but WITHOUT
// ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or
// FITNESS FOR A PARTICULAR PURPOSE. See the GNU Affero General Public License
// for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.

package api

import (
	"bytes"
	"image"
	"image/color"
	_ "image/gif" // register the GIF decoder (first frame only)
	"image/jpeg"
	"image/png"
	"net/http"
)

// Image thumbnails: fetch an image through the proxy, downscale it
// server-side, and serve a small re-encoded version so the browser never
// pulls full-size remote images and memory stays bounded. Only formats
// the standard library can decode (JPEG/PNG/GIF) are supported; anything
// else returns 415 and the client shows no thumbnail.

const (
	maxImageBytes = 10 * 1024 * 1024
	thumbMaxDim   = 400
	// maxDecodeBytes caps the decoded bitmap size to defuse decompression
	// bombs (a small file can declare enormous dimensions) AND deep bit
	// depths (16-bit images are 8 bytes/pixel). ~36 MB covers 9 MP at
	// 4 bytes/pixel, or ~4.5 MP at 8 — most shared content; full-res
	// photos usually exceed the byte cap on the wire anyway. With
	// mediaSlots request-wide slots the media path's worst case stays
	// bounded transiently and the steady-state RSS target is unaffected.
	maxDecodeBytes = 36 * 1024 * 1024
	// maxImageDim caps each declared dimension. Defense in depth: it keeps
	// the decoded-size multiplication below (per-dimension <= 65535, x8 bpp
	// = well under int64) from ever overflowing and wrapping past
	// maxDecodeBytes. Go's own DecodeConfig already rejects dimensions whose
	// byte product overflows ("dimension overflow"), so this is belt-and-
	// suspenders and rejects absurd (but Go-accepted, e.g. 100000^2) dims
	// before the area math. 65535 admits every realistic image.
	maxImageDim   = 65535
	maxThumbCache = 128
	// maxThumbCacheEntry bounds a single cached thumbnail. 128 entries x
	// 48 KiB ~ 6 MiB worst-case resident cache — a small fraction of the
	// 72 MiB RSS target — vs ~64 MiB at the old 512 KiB per-entry cap.
	// Real 400px thumbnails re-encode to well under this.
	maxThumbCacheEntry = 48 * 1024
)

type thumbResult struct {
	contentType string
	data        []byte
}

// bytesPerPixel estimates a decoded image's per-pixel cost from its
// color model: 16-bit models decode to 8 bytes/pixel, everything else to
// at most 4. Used to bound decoded memory, not just pixel count.
func bytesPerPixel(m color.Model) int64 {
	switch m {
	case color.RGBA64Model, color.NRGBA64Model, color.Gray16Model:
		return 8
	case color.CMYKModel:
		// Go's JPEG decoder holds the YCbCr planes (3 B/px) + blackPix (1 B/px)
		// AND a separate CMYK result (4 B/px) live simultaneously during
		// applyBlack — an ~8 B/px peak, double the output bitmap. Modeling it as
		// 4 let a max-dimension CMYK JPEG pass the check yet decode near 72 MiB.
		return 8
	default:
		return 4
	}
}

// isProgressiveJPEG reports whether b is a progressive JPEG (SOF2). It walks
// the marker segments of the header rather than substring-scanning for
// 0xFFC2 (which would false-positive inside entropy-coded data), stopping at
// the first scan (SOS) by which point any frame header has appeared.
func isProgressiveJPEG(b []byte) bool {
	if len(b) < 2 || b[0] != 0xFF || b[1] != 0xD8 { // SOI
		return false
	}
	for i := 2; i+1 < len(b); {
		if b[i] != 0xFF { // not at a marker: resync
			i++
			continue
		}
		marker := b[i+1]
		if marker == 0xFF { // fill byte
			i++
			continue
		}
		i += 2
		switch {
		case marker == 0xC2: // SOF2: progressive
			return true
		case marker == 0xDA: // SOS: entropy data begins, no frame header past here
			return false
		case marker == 0x01 || (marker >= 0xD0 && marker <= 0xD9):
			// TEM / RSTn / SOI / EOI: standalone, no length payload
			continue
		default: // segment carrying a 2-byte length
			next := skipSegment(b, i)
			if next < 0 {
				return false
			}
			i = next
		}
	}
	return false
}

// skipSegment returns the index just past a marker segment's length-prefixed
// payload starting at i, or -1 if the 2-byte length is missing or invalid.
func skipSegment(b []byte, i int) int {
	if i+1 >= len(b) {
		return -1
	}
	segLen := int(b[i])<<8 | int(b[i+1])
	if segLen < 2 {
		return -1
	}
	return i + segLen
}

// decodableFormat validates that body is an image we are willing to fully
// decode within maxDecodeBytes, returning its format. ok is false when the
// header is unreadable, the dimensions are out of range, or the modeled
// decode would exceed the memory cap. Bounding decoded BYTES (not just
// pixels) matters: a 16-bit-depth image decodes to 8 bytes/pixel
// (RGBA64/NRGBA64/Gray16), double the assumed 4, so a pixel-only cap would
// let it use twice the intended memory.
func decodableFormat(body []byte) (format string, ok bool) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(body))
	// The dimension caps are checked FIRST (short-circuit) so the byte
	// product below can never overflow int64.
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 ||
		cfg.Width > maxImageDim || cfg.Height > maxImageDim {
		return "", false
	}
	// Per-pixel decode cost is the output bitmap PLUS, for a progressive
	// JPEG, the decoder's full up-front DCT coefficient allocation: image/jpeg
	// holds ~256 bytes per 8x8 block per component (~12 bytes/pixel at 4:4:4),
	// entirely separate from the result. Modeling only the output bitmap lets
	// a small progressive JPEG blow past maxDecodeBytes at image.Decode time.
	perPixel := bytesPerPixel(cfg.ColorModel)
	if format == "jpeg" && isProgressiveJPEG(body) {
		// ~4 B/px of full-res coefficient storage PER COMPONENT (256 B per 8x8
		// block / 64 px). 3 components for YCbCr, 4 for CMYK/YCCK.
		comps := int64(3)
		if cfg.ColorModel == color.CMYKModel {
			comps = 4
		}
		perPixel += 4 * comps
	}
	if int64(cfg.Width)*int64(cfg.Height)*perPixel > maxDecodeBytes {
		return "", false
	}
	return format, true
}

func (s *Server) handleThumb(w http.ResponseWriter, r *http.Request) {
	if !s.previewsEnabled() {
		http.Error(w, "previews disabled", http.StatusForbidden)
		return
	}
	target, net, ok := mediaRequest(w, r)
	if !ok {
		return
	}
	ck := net + "\x00" + target
	if t, ok := s.thumbCache.get(ck); ok {
		writeThumb(w, t)
		return
	}

	// One request-wide slot covers the whole memory-heavy span — the
	// 10 MiB body fetch, the decode, and the re-encode — so in-flight
	// bytes and bitmaps are bounded together, not just the decode.
	if !s.acquireMedia(r.Context()) {
		http.Error(w, "busy, retry later", http.StatusServiceUnavailable)
		return
	}
	defer s.releaseMedia()
	// Re-check after the slot wait: previews may have been disabled while this
	// request was parked, and it must not fetch after that.
	if !s.previewsEnabled() {
		http.Error(w, "previews disabled", http.StatusForbidden)
		return
	}
	// Resolve egress AFTER the wait so a proxy/tunnel configured on the network
	// while we were parked is honored (a stale direct resolution would leak the
	// IP). Fail closed on an unresolvable network (see egressForNetwork).
	f := s.imageFetcherForNetwork(r.Context(), net)
	if f == nil {
		http.Error(w, "thumbnail unavailable", http.StatusBadGateway)
		return
	}

	ct, body, err := f.get(r.Context(), target)
	if err != nil {
		http.Error(w, "thumbnail unavailable", http.StatusBadGateway)
		return
	}
	if !isImageType(ct) && !isImageType(http.DetectContentType(body)) {
		http.Error(w, "not an image", http.StatusUnsupportedMediaType)
		return
	}

	// Reject oversized decodes from the cheap header read, before
	// committing to a full decode.
	format, ok := decodableFormat(body)
	if !ok {
		http.Error(w, "unsupported image", http.StatusUnsupportedMediaType)
		return
	}

	src, _, err := image.Decode(bytes.NewReader(body))
	if err != nil {
		http.Error(w, "decode failed", http.StatusUnsupportedMediaType)
		return
	}

	out := thumbnail(src, thumbMaxDim)
	res, err := encodeThumb(out, format)
	if err != nil {
		http.Error(w, "encode failed", http.StatusInternalServerError)
		return
	}
	// Hard SERVING cap, not just cache admission: the browser's thumbnail cache
	// is bounded by count, not bytes, so serving an oversized thumbnail (a
	// high-entropy 400×400 re-encode can reach ~640 KB) would let it bloat far
	// past the intended budget. Refuse it — the client falls back to no
	// thumbnail, exactly as it does for any thumb failure — which keeps browser
	// blob residency at count × maxThumbCacheEntry.
	if len(res.data) > maxThumbCacheEntry {
		http.Error(w, "thumbnail too large", http.StatusRequestEntityTooLarge)
		return
	}
	s.thumbCache.put(ck, res)
	writeThumb(w, res)
}

func writeThumb(w http.ResponseWriter, t thumbResult) {
	w.Header().Set("Content-Type", t.contentType)
	// 30 min, matching the server-side thumbCache TTL. A longer browser cache
	// (was 1 day) would keep a redacted image's thumbnail reachable in the
	// browser long after the server purged it. private: never proxy-cacheable.
	w.Header().Set("Cache-Control", "private, max-age=1800")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(t.data)
}

// encodeThumb re-encodes a thumbnail: PNG for formats that may carry
// transparency (png/gif), JPEG otherwise.
func encodeThumb(img image.Image, srcFormat string) (thumbResult, error) {
	var buf bytes.Buffer
	if srcFormat == "png" || srcFormat == "gif" {
		if err := png.Encode(&buf, img); err != nil {
			return thumbResult{}, err
		}
		return thumbResult{contentType: "image/png", data: buf.Bytes()}, nil
	}
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 82}); err != nil {
		return thumbResult{}, err
	}
	return thumbResult{contentType: "image/jpeg", data: buf.Bytes()}, nil
}

// thumbnail downscales src so its longest side is at most maxDim,
// preserving aspect ratio. Images already within bounds are returned
// unchanged (we never upscale). Downscaling uses box averaging: each
// destination pixel is the mean of the source pixels it covers, which
// visits every source pixel about once — O(src) and good enough for
// thumbnails without pulling in an image library.
func thumbnail(src image.Image, maxDim int) image.Image {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw <= maxDim && sh <= maxDim {
		return src
	}
	dw, dh := sw, sh
	if sw >= sh {
		dw, dh = maxDim, max(1, sh*maxDim/sw)
	} else {
		dw, dh = max(1, sw*maxDim/sh), maxDim
	}

	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for dy := 0; dy < dh; dy++ {
		sy0 := b.Min.Y + dy*sh/dh
		sy1 := b.Min.Y + (dy+1)*sh/dh
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		for dx := 0; dx < dw; dx++ {
			sx0 := b.Min.X + dx*sw/dw
			sx1 := b.Min.X + (dx+1)*sw/dw
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}
			dst.Set(dx, dy, boxAverage(src, sx0, sx1, sy0, sy1))
		}
	}
	return dst
}

// boxAverage returns the mean color of the source rectangle
// [sx0,sx1) × [sy0,sy1), in 16-bit channels.
func boxAverage(src image.Image, sx0, sx1, sy0, sy1 int) color.RGBA64 {
	var r, g, b, a, n uint64
	for sy := sy0; sy < sy1; sy++ {
		for sx := sx0; sx < sx1; sx++ {
			cr, cg, cb, ca := src.At(sx, sy).RGBA() // 16-bit channels
			r, g, b, a, n = r+uint64(cr), g+uint64(cg), b+uint64(cb), a+uint64(ca), n+1
		}
	}
	if n == 0 {
		n = 1
	}
	return color.RGBA64{R: uint16(r / n), G: uint16(g / n), B: uint16(b / n), A: uint16(a / n)}
}
