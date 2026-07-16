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
	// maxDecodePixels caps the decoded bitmap to defuse decompression
	// bombs: a small file can declare enormous dimensions. 9 MP covers
	// ~3000x3000 (most shared content; full-res photos usually exceed
	// the byte cap anyway) at ~36 MB RGBA. With mediaSlots request-wide
	// slots the media path's worst case is bounded at roughly
	// slots x (body + bitmap) ~ 2 x 46 MB transiently — the steady-state
	// RSS target is unaffected.
	maxDecodePixels = 9 * 1000 * 1000
	maxThumbCache   = 128
)

type thumbResult struct {
	contentType string
	data        []byte
}

func (s *Server) handleThumb(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("url")
	if len(target) == 0 || len(target) > 2048 {
		http.Error(w, "bad url", http.StatusBadRequest)
		return
	}
	if t, ok := s.thumbCache.get(target); ok {
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

	ct, body, err := s.imageFetcher.get(r.Context(), target)
	if err != nil {
		http.Error(w, "thumbnail unavailable", http.StatusBadGateway)
		return
	}
	if !isImageType(ct) && !isImageType(http.DetectContentType(body)) {
		http.Error(w, "not an image", http.StatusUnsupportedMediaType)
		return
	}

	// Reject oversized dimensions from the cheap header read, before
	// committing to a full decode.
	cfg, format, err := image.DecodeConfig(bytes.NewReader(body))
	if err != nil || cfg.Width*cfg.Height > maxDecodePixels || cfg.Width == 0 || cfg.Height == 0 {
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
	if len(res.data) <= 512*1024 { // don't cache pathologically large results
		s.thumbCache.put(target, res)
	}
	writeThumb(w, res)
}

func writeThumb(w http.ResponseWriter, t thumbResult) {
	w.Header().Set("Content-Type", t.contentType)
	w.Header().Set("Cache-Control", "private, max-age=86400")
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
