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
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"testing"
	"time"
)

// makePNG builds an w×h test image (a diagonal gradient) as PNG bytes.
func makePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), 128, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestThumbnailDownscales(t *testing.T) {
	src, _, err := image.Decode(bytes.NewReader(makePNG(t, 1000, 600)))
	if err != nil {
		t.Fatal(err)
	}
	out := thumbnail(src, thumbMaxDim)
	b := out.Bounds()
	if b.Dx() != thumbMaxDim {
		t.Fatalf("width = %d, want %d", b.Dx(), thumbMaxDim)
	}
	if b.Dy() != 600*thumbMaxDim/1000 {
		t.Fatalf("height = %d, want %d (aspect preserved)", b.Dy(), 600*thumbMaxDim/1000)
	}
}

func TestThumbnailTallImage(t *testing.T) {
	src, _, _ := image.Decode(bytes.NewReader(makePNG(t, 300, 1200)))
	out := thumbnail(src, thumbMaxDim)
	b := out.Bounds()
	if b.Dy() != thumbMaxDim {
		t.Fatalf("height = %d, want %d", b.Dy(), thumbMaxDim)
	}
	if b.Dx() != 300*thumbMaxDim/1200 {
		t.Fatalf("width = %d, want %d", b.Dx(), 300*thumbMaxDim/1200)
	}
}

func TestThumbnailNoUpscale(t *testing.T) {
	src, _, _ := image.Decode(bytes.NewReader(makePNG(t, 100, 80)))
	out := thumbnail(src, thumbMaxDim)
	if out.Bounds().Dx() != 100 || out.Bounds().Dy() != 80 {
		t.Fatalf("upscaled small image to %v", out.Bounds())
	}
}

func TestThumbnailAveragesColor(t *testing.T) {
	// A checkerboard of pure red/blue must downscale toward a purple
	// average, proving box averaging (not nearest-neighbor).
	img := image.NewRGBA(image.Rect(0, 0, 100, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 100; x++ {
			if (x+y)%2 == 0 {
				img.Set(x, y, color.RGBA{255, 0, 0, 255})
			} else {
				img.Set(x, y, color.RGBA{0, 0, 255, 255})
			}
		}
	}
	out := thumbnail(img, 10)
	r, g, b, _ := out.At(5, 5).RGBA()
	if r>>8 < 80 || r>>8 > 175 || b>>8 < 80 || b>>8 > 175 || g>>8 > 40 {
		t.Fatalf("center pixel not averaged: r=%d g=%d b=%d", r>>8, g>>8, b>>8)
	}
}

func TestEncodeThumb(t *testing.T) {
	opaque := image.NewRGBA(image.Rect(0, 0, 10, 10))
	for y := 0; y < 10; y++ {
		for x := 0; x < 10; x++ {
			opaque.Set(x, y, color.RGBA{100, 150, 200, 255})
		}
	}
	png, err := encodeThumb(opaque, "png")
	if err != nil || png.contentType != "image/png" {
		t.Fatalf("png: %+v %v", png.contentType, err)
	}
	jpg, err := encodeThumb(opaque, "jpeg")
	if err != nil || jpg.contentType != "image/jpeg" {
		t.Fatalf("jpeg: %+v %v", jpg.contentType, err)
	}
	// GIF sources re-encode as PNG (may carry transparency).
	g, _ := encodeThumb(opaque, "gif")
	if g.contentType != "image/png" {
		t.Fatalf("gif re-encode = %q", g.contentType)
	}
	// A NON-OPAQUE image forces PNG regardless of source format, or the
	// transparency would render dark/black through JPEG.
	clear := image.NewRGBA(image.Rect(0, 0, 10, 10)) // zero-valued: alpha 0
	if tr, _ := encodeThumb(clear, "webp"); tr.contentType != "image/png" {
		t.Fatalf("transparent re-encode = %q, want image/png", tr.contentType)
	}
}

func TestDecodeByteGate(t *testing.T) {
	big := makePNG(t, 100, 100)
	cfg, _, err := image.DecodeConfig(bytes.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	// A small image is under the byte cap.
	if int64(cfg.Width)*int64(cfg.Height)*bytesPerPixel(cfg.ColorModel) > maxDecodeBytes {
		t.Fatal("test image unexpectedly over the byte cap")
	}
	// A plausible decompression bomb (20000x20000 @ 4 B/px = 1.6 GB) is
	// rejected.
	if int64(20000)*20000*4 <= maxDecodeBytes {
		t.Fatal("maxDecodeBytes too high to stop a bomb")
	}
	// bytesPerPixel distinguishes 16-bit depth: an 8 B/px image hits the
	// cap at half the pixels a 4 B/px one does.
	if bytesPerPixel(color.RGBA64Model) != 8 || bytesPerPixel(color.RGBAModel) != 4 {
		t.Fatal("bytesPerPixel wrong for 16-bit vs 8-bit models")
	}
	// A 3000x3000 16-bit image (8 B/px = 72 MB) is now rejected, where a
	// pixel-only cap of 9 MP would have allowed it.
	if int64(3000)*3000*bytesPerPixel(color.NRGBA64Model) <= maxDecodeBytes {
		t.Fatal("16-bit 3000x3000 should exceed the byte cap")
	}
}

// minimalVP8LWebP is a valid 1x1 lossless (VP8L) WebP. Go has no WebP encoder,
// so the fixture is raw bytes. Since the VP8L restriction (its decoder's peak
// memory is metadata-driven, see webpUsesVP8L) it must be REJECTED by the
// decode gate.
var minimalVP8LWebP = []byte{
	0x52, 0x49, 0x46, 0x46, 0x1a, 0x00, 0x00, 0x00, 0x57, 0x45, 0x42, 0x50, 0x56, 0x50, 0x38, 0x4c,
	0x0d, 0x00, 0x00, 0x00, 0x2f, 0x00, 0x00, 0x00, 0x10, 0x07, 0x10, 0x11, 0x11, 0x88, 0x88, 0xfe,
	0x07, 0x00,
}

// lossyWebP is a 16x16 lossy VP8 WebP (no alpha) — the form the thumbnailer
// still accepts. Generated once with Pillow/libwebp (quality=75).
var lossyWebP = []byte{
	0x52, 0x49, 0x46, 0x46, 0x4e, 0x00, 0x00, 0x00, 0x57, 0x45, 0x42, 0x50, 0x56, 0x50, 0x38, 0x20,
	0x42, 0x00, 0x00, 0x00, 0xf0, 0x01, 0x00, 0x9d, 0x01, 0x2a, 0x10, 0x00, 0x10, 0x00, 0x02, 0x00,
	0x34, 0x25, 0xb0, 0x02, 0x74, 0x01, 0x0f, 0x0c, 0x12, 0xf2, 0xca, 0x80, 0x00, 0xfe, 0xfc, 0xc9,
	0x5e, 0xd9, 0x36, 0xe6, 0x26, 0xa6, 0xef, 0x73, 0x5a, 0x6b, 0xdf, 0xe4, 0xb7, 0xe3, 0xd0, 0x94,
	0x56, 0x77, 0xcb, 0x40, 0xf6, 0xb5, 0x27, 0x57, 0xe4, 0x6a, 0x7f, 0xf5, 0x12, 0x0a, 0xbf, 0xfe,
	0xfb, 0x04, 0x89, 0x16, 0x40, 0x00,
}

// lossyAlphaWebP is the same 16x16 lossy VP8 frame with a VP8L-COMPRESSED
// ALPH chunk (VP8X + ALPH whose first payload byte has compression=1) — the
// alpha path that routes through the unbounded VP8L decoder, so it must be
// rejected. Generated with Pillow/libwebp from an RGBA source.
var lossyAlphaWebP = []byte{
	0x52, 0x49, 0x46, 0x46, 0x7c, 0x00, 0x00, 0x00, 0x57, 0x45, 0x42, 0x50, 0x56, 0x50, 0x38, 0x58,
	0x0a, 0x00, 0x00, 0x00, 0x10, 0x00, 0x00, 0x00, 0x0f, 0x00, 0x00, 0x0f, 0x00, 0x00, 0x41, 0x4c,
	0x50, 0x48, 0x13, 0x00, 0x00, 0x00, 0x01, 0x0f, 0xf0, 0xc0, 0xff, 0x88, 0x88, 0x20, 0x16, 0x4c,
	0xe6, 0x2f, 0xdd, 0x9d, 0x41, 0x44, 0xff, 0x23, 0x17, 0x00, 0x56, 0x50, 0x38, 0x20, 0x42, 0x00,
	0x00, 0x00, 0xf0, 0x01, 0x00, 0x9d, 0x01, 0x2a, 0x10, 0x00, 0x10, 0x00, 0x02, 0x00, 0x34, 0x25,
	0xb0, 0x02, 0x74, 0x01, 0x0f, 0x0c, 0x12, 0xf2, 0xca, 0x80, 0x00, 0xfe, 0xfc, 0xc9, 0x5e, 0xd9,
	0x36, 0xe6, 0x26, 0xa6, 0xef, 0x73, 0x5a, 0x6b, 0xdf, 0xe4, 0xb7, 0xe3, 0xd0, 0x94, 0x56, 0x77,
	0xcb, 0x40, 0xf6, 0xb5, 0x27, 0x57, 0xe4, 0x6a, 0x7f, 0xf5, 0x12, 0x0a, 0xbf, 0xfe, 0xfb, 0x04,
	0x89, 0x16, 0x40, 0x00,
}

// uncompressedAlphaWebP is lossyAlphaWebP with its ALPH payload rewritten to
// compression method 0 (raw 16x16 alpha bytes, half-transparent left side):
// no VP8L bitstream anywhere, so it stays accepted — and its thumbnail must
// re-encode as PNG, not JPEG, to keep the transparency.
var uncompressedAlphaWebP = func() []byte {
	b := []byte{
		0x52, 0x49, 0x46, 0x46, 0x6a, 0x01, 0x00, 0x00, 0x57, 0x45, 0x42, 0x50, 0x56, 0x50, 0x38, 0x58,
		0x0a, 0x00, 0x00, 0x00, 0x10, 0x00, 0x00, 0x00, 0x0f, 0x00, 0x00, 0x0f, 0x00, 0x00,
	}
	// ALPH chunk: fourCC, LE32 length (1 header byte + 256 alpha bytes = 257),
	// header byte 0 (uncompressed), then row-major alpha.
	b = append(b, 'A', 'L', 'P', 'H', 0x01, 0x01, 0x00, 0x00, 0x00)
	for range 16 {
		for x := range 16 {
			a := byte(0xff)
			if x < 8 {
				a = 0x80
			}
			b = append(b, a)
		}
	}
	b = append(b, 0x00) // pad: 257-byte payload rounds to even
	// The same lossy VP8 frame as lossyWebP.
	return append(b, lossyWebP[12:]...)
}()

func TestWebPLossyDecodes(t *testing.T) {
	format, ok := decodableFormat(lossyWebP)
	if !ok || format != "webp" {
		t.Fatalf("decodableFormat = (%q, %v), want (webp, true)", format, ok)
	}
	// isImageType must agree the content type is decodable, and reject the
	// formats we have no decoder for (avif, x-icon) so they don't route to a
	// thumbnail that fails.
	if !isImageType("image/webp") {
		t.Fatal("isImageType(image/webp) = false")
	}
	if isImageType("image/avif") || isImageType("image/x-icon") {
		t.Fatal("isImageType claims a format with no registered decoder")
	}
	// End to end: decode → downscale → re-encode, as handleThumb does. An
	// opaque lossy frame re-encodes as JPEG.
	src, _, err := image.Decode(bytes.NewReader(lossyWebP))
	if err != nil {
		t.Fatalf("image.Decode(webp): %v", err)
	}
	res, err := encodeThumb(thumbnail(src, thumbMaxDim), format)
	if err != nil {
		t.Fatalf("encodeThumb(webp): %v", err)
	}
	if res.contentType != "image/jpeg" {
		t.Fatalf("opaque webp re-encode = %q, want image/jpeg", res.contentType)
	}
}

// The VP8L decoder's peak allocation is chosen by encoded metadata (up to
// ~167 MiB of Huffman groups regardless of declared dimensions), so lossless
// bodies and VP8L-compressed alpha must be refused before any decode.
func TestWebPVP8LRejected(t *testing.T) {
	if !webpUsesVP8L(minimalVP8LWebP) {
		t.Fatal("webpUsesVP8L missed a VP8L image chunk")
	}
	if !webpUsesVP8L(lossyAlphaWebP) {
		t.Fatal("webpUsesVP8L missed a VP8L-compressed ALPH chunk")
	}
	if webpUsesVP8L(lossyWebP) || webpUsesVP8L(uncompressedAlphaWebP) {
		t.Fatal("webpUsesVP8L false positive on plain-VP8 / uncompressed-alpha bodies")
	}
	if webpUsesVP8L([]byte("not a webp at all")) {
		t.Fatal("webpUsesVP8L false positive on a non-RIFF body")
	}
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"lossless VP8L", minimalVP8LWebP},
		{"compressed ALPH", lossyAlphaWebP},
	} {
		if format, ok := decodableFormat(tc.body); ok {
			t.Errorf("%s: decodableFormat = (%q, true), want rejected", tc.name, format)
		}
	}
}

// A WebP whose alpha survives the VP8L restriction (uncompressed ALPH) decodes
// to a non-opaque NYCbCrA; the thumbnail must select PNG or the transparency
// renders dark/black through JPEG.
func TestWebPAlphaKeepsPNG(t *testing.T) {
	format, ok := decodableFormat(uncompressedAlphaWebP)
	if !ok || format != "webp" {
		t.Fatalf("decodableFormat = (%q, %v), want (webp, true)", format, ok)
	}
	src, _, err := image.Decode(bytes.NewReader(uncompressedAlphaWebP))
	if err != nil {
		t.Fatalf("image.Decode(alpha webp): %v", err)
	}
	if imageOpaque(src) {
		t.Fatal("fixture decoded opaque; premise wrong")
	}
	res, err := encodeThumb(thumbnail(src, thumbMaxDim), format)
	if err != nil {
		t.Fatalf("encodeThumb: %v", err)
	}
	if res.contentType != "image/png" {
		t.Fatalf("alpha webp re-encode = %q, want image/png", res.contentType)
	}
}

func TestIsProgressiveJPEG(t *testing.T) {
	// A stdlib-encoded JPEG is baseline (SOF0), never progressive.
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	if isProgressiveJPEG(buf.Bytes()) {
		t.Fatal("baseline jpeg.Encode output detected as progressive")
	}

	cases := []struct {
		name string
		b    []byte
		want bool
	}{
		{"progressive SOF2", []byte{0xFF, 0xD8, 0xFF, 0xC2, 0x00, 0x03, 0x00}, true},
		{"baseline SOF0 then SOS", []byte{0xFF, 0xD8, 0xFF, 0xC0, 0x00, 0x03, 0x00, 0xFF, 0xDA, 0x00, 0x02}, false},
		{"SOF2 after an APP0 segment", []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x04, 0x00, 0x00, 0xFF, 0xC2, 0x00, 0x03, 0x00}, true},
		{"SOS before any SOF", []byte{0xFF, 0xD8, 0xFF, 0xDA, 0x00, 0x02}, false},
		{"not a jpeg", []byte{0x89, 0x50, 0x4E, 0x47}, false},
		{"too short", []byte{0xFF}, false},
	}
	for _, tc := range cases {
		if got := isProgressiveJPEG(tc.b); got != tc.want {
			t.Errorf("%s: isProgressiveJPEG = %v, want %v", tc.name, got, tc.want)
		}
	}

	// The gate charges progressive JPEG the coefficient overhead: a ~4 MP
	// progressive image (4M * 16 = 64 MB) exceeds maxDecodeBytes, where the
	// same pixels baseline (4M * 4 = 16 MB) pass.
	const px = 4_000_000
	if int64(px)*(bytesPerPixel(color.RGBAModel)+12) <= maxDecodeBytes {
		t.Fatal("progressive per-pixel factor too low to bound the coefficient allocation")
	}
	if int64(px)*bytesPerPixel(color.RGBAModel) > maxDecodeBytes {
		t.Fatal("baseline 4MP should pass the cap; test premise wrong")
	}
}

// adobeAPP14 builds an Adobe APP14 segment: FF EE, len=0x000E (2 len + 12
// payload), "Adobe", version(2) flags0(2) flags1(2) transform(1). transform 0
// means RGB (3-component). Both jpegDecodesToRGBA and image/jpeg key on this.
func adobeAPP14(transform byte) []byte {
	seg := append([]byte{0xFF, 0xEE, 0x00, 0x0E}, []byte("Adobe")...)
	return append(seg, 0, 0, 0, 0, 0, 0, transform)
}

// patchJPEGToRGBIDs rewrites the 3 SOF0 component IDs *and* the 3 SOS component
// selectors of a baseline JPEG to 'R','G','B'. Go's image/jpeg then decodes it to
// *image.RGBA with NO Adobe marker present — the case DecodeConfig reports as
// RGBAModel that a whole-file APP14 scan cannot see.
func patchJPEGToRGBIDs(t *testing.T, b []byte) []byte {
	t.Helper()
	out := append([]byte(nil), b...)
	for i := 2; i+1 < len(out); {
		if out[i] != 0xFF {
			i++
			continue
		}
		m := out[i+1]
		if m == 0xFF {
			i++
			continue
		}
		if m == 0xD8 || (m >= 0xD0 && m <= 0xD9) || m == 0x01 {
			i += 2
			continue
		}
		seglen := int(out[i+2])<<8 | int(out[i+3])
		switch m {
		case 0xC0: // SOF0: len,precision,height,width,ncomp, then comp{id,samp,q}
			base := i + 2 + 2 + 1 + 2 + 2 + 1
			out[base+0], out[base+3], out[base+6] = 'R', 'G', 'B'
		case 0xDA: // SOS: len,ns, then scan-comp{selector,tables}
			base := i + 2 + 2 + 1
			out[base+0], out[base+2], out[base+4] = 'R', 'G', 'B'
			return out
		}
		i += 2 + seglen
	}
	t.Fatal("no SOS marker found to patch")
	return nil
}

// insertAPP14BeforeEOI splices an Adobe APP14 in right before EOI (0xFFD9), i.e.
// AFTER the entropy-coded scan. DecodeConfig stops at the first SOS and never sees
// it (reports YCbCrModel), but the full decode honors it and produces *image.RGBA.
func insertAPP14BeforeEOI(t *testing.T, b []byte) []byte {
	t.Helper()
	idx := bytes.LastIndex(b, []byte{0xFF, 0xD9})
	if idx < 0 {
		t.Fatal("no EOI marker")
	}
	out := append([]byte(nil), b[:idx]...)
	out = append(out, adobeAPP14(0)...)
	return append(out, b[idx:]...)
}

// The decode surcharge keeps a JPEG that Go decodes to RGBA (component planes AND
// the RGBA result live at once) from decoding past maxDecodeBytes. Go reaches RGBA
// three ways; the gate must charge all of them and only them. Fixtures are patched
// real JPEGs whose FULL decode is asserted to actually be *image.RGBA (F3/Medium-1).
func TestCoexistingBufferSurcharges(t *testing.T) {
	// jpegDecodesToRGBA marker detection (used for the late-APP14 branch only).
	if !jpegDecodesToRGBA(adobeAPP14(0)) {
		t.Error("Adobe transform 0 (RGB) not detected")
	}
	if jpegDecodesToRGBA(adobeAPP14(1)) || jpegDecodesToRGBA(adobeAPP14(2)) {
		t.Error("Adobe transform 1/2 (YCbCr/YCCK) wrongly detected as RGB")
	}

	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 16), uint8(y * 16), 100, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	baseline := buf.Bytes()

	// assertDecode returns the DecodeConfig model and the concrete decoded type so
	// each case validates its own premise (GPT's objection: the old test never
	// decoded, so it couldn't prove the body actually became RGBA).
	decodeInfo := func(b []byte) (color.Model, string) {
		cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
		if err != nil {
			t.Fatalf("DecodeConfig: %v", err)
		}
		di, _, err := image.Decode(bytes.NewReader(b))
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		return cfg.ColorModel, fmt.Sprintf("%T", di)
	}

	cases := []struct {
		name        string
		body        []byte
		wantCfg     color.Model
		wantDecoded string // concrete type the FULL decode must produce
		wantScan    bool   // jpegDecodesToRGBA (APP14 whole-file scan) result
		wantExtra   int64
	}{
		// Plain YCbCr (JFIF, IDs 1/2/3): no surcharge, keeps the 9 MP budget.
		{"baseline YCbCr", baseline, color.YCbCrModel, "*image.YCbCr", false, 0},
		// Component-ID RGB: DecodeConfig already reports RGBA, NO Adobe marker — the
		// case the old code missed (scan returns false, yet it decodes to RGBA).
		{"component-ID RGB", patchJPEGToRGBIDs(t, baseline), color.RGBAModel, "*image.RGBA", false, 4},
		// Late APP14: DecodeConfig reports YCbCr but the full decode is RGBA; only
		// the whole-file scan catches it.
		{"late APP14", insertAPP14BeforeEOI(t, baseline), color.YCbCrModel, "*image.RGBA", true, 4},
	}
	for _, tc := range cases {
		gotCfg, gotDecoded := decodeInfo(tc.body)
		if gotCfg != tc.wantCfg {
			t.Errorf("%s: DecodeConfig model = %T, want %T", tc.name, gotCfg, tc.wantCfg)
		}
		if gotDecoded != tc.wantDecoded {
			t.Errorf("%s: full decode = %s, want %s", tc.name, gotDecoded, tc.wantDecoded)
		}
		if got := jpegDecodesToRGBA(tc.body); got != tc.wantScan {
			t.Errorf("%s: jpegDecodesToRGBA = %v, want %v", tc.name, got, tc.wantScan)
		}
		// The surcharge is keyed on DecodeConfig's model (what the gate actually
		// sees at decode time), exactly as decodableFormat calls it.
		if got := jpegDecodeSurcharge(gotCfg, tc.body); got != tc.wantExtra {
			t.Errorf("%s: surcharge = %d, want %d", tc.name, got, tc.wantExtra)
		}
	}

	// PNG interlace byte at offset 28 drives the Adam7 surcharge.
	png := func(interlace byte) []byte {
		b := make([]byte, 29)
		copy(b, []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A})
		b[28] = interlace
		return b
	}
	if !isAdam7PNG(png(1)) || isAdam7PNG(png(0)) || isAdam7PNG([]byte{0x89, 'P'}) {
		t.Error("isAdam7PNG detection wrong")
	}

	// The surcharge bites: an 8 MP RGB JPEG (4+4=8 B/px) exceeds the cap where the
	// same pixels as plain YCbCr (4 B/px) pass.
	const px = 8_000_000
	if int64(px)*decodePerPixel("jpeg", color.RGBAModel, baseline) <= maxDecodeBytes {
		t.Fatal("RGB-JPEG per-pixel too low to bound the coexisting RGBA copy")
	}
	if int64(px)*decodePerPixel("jpeg", color.YCbCrModel, baseline) > maxDecodeBytes {
		t.Fatal("8MP plain YCbCr should pass; test premise wrong")
	}
}

// The media budget is request-wide: with every slot held, a thumbnail
// request is refused (bounded wait honors context cancellation) instead
// of fetching and queueing unbounded work.
func TestMediaBudgetSaturated(t *testing.T) {
	ts, srv := newTestServerWithRef(t)
	for i := 0; i < mediaSlots; i++ {
		srv.mediaSem <- struct{}{}
	}
	defer func() {
		for i := 0; i < mediaSlots; i++ {
			<-srv.mediaSem
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if srv.acquireMedia(ctx) {
		t.Fatal("acquireMedia succeeded with all slots held")
	}

	old := mediaAcquireWait
	mediaAcquireWait = 50 * time.Millisecond
	defer func() { mediaAcquireWait = old }()
	cookie := sessionCookieOf(t, login(t, ts, "AlteredParadox", "hunter2"))
	rctx, rcancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer rcancel()
	body := `{"url":"http://example.com/x.png","net":"` + testNet + `"}`
	req, _ := http.NewRequestWithContext(rctx, "POST", ts.URL+"/api/thumb", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", ts.URL)
	req.AddCookie(cookie)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}
