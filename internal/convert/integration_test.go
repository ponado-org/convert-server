package convert

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os/exec"
	"strings"
	"testing"
)

// realConverter skips the test when the host has no libvips. These tests
// exercise the actual CLI arguments against real files, complementing the
// stub-based unit tests in convert_test.go.
func realConverter(t *testing.T) *Converter {
	t.Helper()
	if _, err := exec.LookPath("vips"); err != nil {
		t.Skip("vips not installed; skipping integration test")
	}
	c, err := New(Options{VipsBin: "vips"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func vipsSupports(t *testing.T, operation string) bool {
	t.Helper()
	out, err := exec.Command("vips", "-l").Output()
	if err != nil {
		t.Skipf("vips -l failed: %v", err)
	}
	return strings.Contains(string(out), operation)
}

func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 48))
	for y := 0; y < 48; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 4), G: uint8(y * 5), B: 120, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func testJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 48))
	for y := 0; y < 48; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, color.RGBA{R: 200, G: uint8(x * 3), B: uint8(y * 4), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestIntegrationPNGToWebP(t *testing.T) {
	if !vipsSupports(t, "webpsave") {
		t.Skip("vips has no webp saver")
	}
	c := realConverter(t)

	res, err := c.Convert(Request{Filename: "in.png", Data: testPNG(t), To: "webp", Quality: 75})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Data) < 12 || string(res.Data[0:4]) != "RIFF" || string(res.Data[8:12]) != "WEBP" {
		t.Fatalf("not a WebP payload: % x", res.Data[:min(16, len(res.Data))])
	}
	if res.ContentType != "image/webp" || res.Format != "webp" {
		t.Fatalf("result = %+v", res)
	}
}

func TestIntegrationPNGToPDF(t *testing.T) {
	c := realConverter(t)

	res, err := c.Convert(Request{Filename: "in.png", Data: testPNG(t), To: "pdf"})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(res.Data, []byte("%PDF-")) || !bytes.Contains(res.Data, []byte("%%EOF")) {
		t.Fatalf("not a PDF payload (%d bytes)", len(res.Data))
	}
	if res.ContentType != "application/pdf" || res.Format != "pdf" {
		t.Fatalf("result = %+v", res)
	}
}

func TestIntegrationSVGToPNG(t *testing.T) {
	if !vipsSupports(t, "svgload") {
		t.Skip("vips has no svg loader")
	}
	c := realConverter(t)

	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="60" height="40"><rect width="60" height="40" fill="green"/></svg>`)
	res, err := c.Convert(Request{Filename: "logo.svg", Data: svg})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(res.Data))
	if err != nil {
		t.Fatalf("output is not PNG: %v", err)
	}
	// Default DPI is 150, so a 60x40 SVG should be slightly larger than 60x40.
	if cfg.Width <= 60 || cfg.Height <= 40 {
		t.Fatalf("unexpected raster size %dx%d", cfg.Width, cfg.Height)
	}
}

func TestIntegrationPDFToPNG(t *testing.T) {
	if !vipsSupports(t, "pdfload") {
		t.Skip("vips has no PDF loader (vips-poppler missing)")
	}
	c := realConverter(t)

	pdf, err := jpegToPDF(testJPEG(t))
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Convert(Request{Filename: "in.pdf", Data: pdf, To: "png", DPI: 96, Page: 1})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(res.Data))
	if err != nil {
		t.Fatalf("output is not PNG: %v", err)
	}
	if cfg.Width != 64 || cfg.Height != 48 {
		t.Fatalf("unexpected raster size %dx%d, want 64x48", cfg.Width, cfg.Height)
	}
}
