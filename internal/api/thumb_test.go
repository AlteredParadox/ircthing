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
	img := image.NewRGBA(image.Rect(0, 0, 10, 10))
	png, err := encodeThumb(img, "png")
	if err != nil || png.contentType != "image/png" {
		t.Fatalf("png: %+v %v", png.contentType, err)
	}
	jpg, err := encodeThumb(img, "jpeg")
	if err != nil || jpg.contentType != "image/jpeg" {
		t.Fatalf("jpeg: %+v %v", jpg.contentType, err)
	}
	// GIF sources re-encode as PNG (may carry transparency).
	g, _ := encodeThumb(img, "gif")
	if g.contentType != "image/png" {
		t.Fatalf("gif re-encode = %q", g.contentType)
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

// jpegAdobeRGB and isAdam7PNG drive the decode surcharges that keep a crafted
// RGB/Adobe JPEG or Adam7 PNG from decoding past maxDecodeBytes (F3). The stdlib
// encoders never emit these, so the headers are crafted.
func TestCoexistingBufferDetectors(t *testing.T) {
	// APP14 "Adobe" segment: FF EE, len=0x0010(16), "Adobe", ver(2) flags0(2)
	// flags1(2), transform(1). len covers the 2 length bytes + 12 payload + 2.
	adobe := func(transform byte) []byte {
		seg := append([]byte{0xFF, 0xEE, 0x00, 0x0E}, []byte("Adobe")...)
		seg = append(seg, 0, 0, 0, 0, 0, 0, transform) // ver, flags0, flags1, transform
		return append([]byte{0xFF, 0xD8}, append(seg, 0xFF, 0xDA, 0x00, 0x02)...)
	}
	jpegCases := []struct {
		name string
		b    []byte
		want bool
	}{
		{"adobe transform 0 (RGB)", adobe(0), true},
		{"adobe transform 1 (YCbCr)", adobe(1), false},
		{"adobe transform 2 (YCCK)", adobe(2), false},
		{"no adobe marker", []byte{0xFF, 0xD8, 0xFF, 0xDA, 0x00, 0x02}, false},
		{"not a jpeg", []byte{0x89, 0x50}, false},
	}
	for _, tc := range jpegCases {
		if got := jpegAdobeRGB(tc.b); got != tc.want {
			t.Errorf("jpegAdobeRGB(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}

	// PNG: interlace byte at offset 28.
	png := func(interlace byte) []byte {
		b := make([]byte, 29)
		copy(b, []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A})
		b[28] = interlace
		return b
	}
	if !isAdam7PNG(png(1)) {
		t.Error("interlace=1 not detected as Adam7")
	}
	if isAdam7PNG(png(0)) {
		t.Error("interlace=0 wrongly detected as Adam7")
	}
	if isAdam7PNG([]byte{0x89, 'P'}) {
		t.Error("short buffer detected as Adam7")
	}

	// The surcharges bite: an 8 MP RGB JPEG (8 B/px) and an 8 MP Adam7 RGBA PNG
	// (8 B/px) both exceed the cap, where the same pixels as plain YCbCr/non-
	// interlaced (4 B/px) pass.
	const px = 8_000_000
	if int64(px)*(bytesPerPixel(color.YCbCrModel)+4) <= maxDecodeBytes {
		t.Fatal("RGB-JPEG surcharge too low to bound the RGBA copy")
	}
	if int64(px)*bytesPerPixel(color.YCbCrModel) > maxDecodeBytes {
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
