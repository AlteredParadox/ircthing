package api

import (
	"bytes"
	"context"
	"image"
	"image/color"
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
	req, _ := http.NewRequestWithContext(rctx, "GET", ts.URL+"/api/thumb?url=http://example.com/x.png&net="+testNet, nil)
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
