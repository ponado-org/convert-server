package server

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"testing"

	"convert-server/internal/convert"
)

// TestIntegrationConvertEndpoint exercises the full HTTP path - multipart
// decoding, the real vips pipeline, and response headers - rather than a fake
// converter. It skips hosts without libvips.
func TestIntegrationConvertEndpoint(t *testing.T) {
	if _, err := exec.LookPath("vips"); err != nil {
		t.Skip("vips not installed; skipping integration test")
	}
	converter, err := convert.New(convert.Options{VipsBin: "vips"})
	if err != nil {
		t.Fatal(err)
	}
	s := New(&fakeRenderer{}, converter, Options{MaxBodyBytes: 5 << 20})

	img := image.NewRGBA(image.Rect(0, 0, 32, 24))
	for y := 0; y < 24; y++ {
		for x := 0; x < 32; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 7), G: uint8(y * 9), B: 90, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}

	req := multipartConvertRequest(t, func(mw *multipart.Writer) {
		_ = mw.WriteField("to", "webp")
		w, _ := mw.CreateFormFile("file", "upload.png")
		_, _ = w.Write(buf.Bytes())
	})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("content-type"); got != "image/webp" {
		t.Fatalf("content-type = %q", got)
	}
	data := rec.Body.Bytes()
	if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		t.Fatalf("body is not WebP (%d bytes)", len(data))
	}
}
