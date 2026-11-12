package convert

import (
	"archive/zip"
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestConverter installs a fake vips CLI. It records its arguments and
// writes a placeholder image for copy/thumbnail/colourspace operations.
func newTestConverter(t *testing.T, gotenbergURL string) (*Converter, string) {
	t.Helper()
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	t.Setenv("ARGS_FILE", argsFile)

	stub := filepath.Join(t.TempDir(), "vips")
	script := `#!/bin/sh
printf '%s\n' "$@" >> "$ARGS_FILE"
case "$1" in
  --version) echo "vips-test"; exit 0 ;;
  copy|thumbnail|colourspace)
    out="$3"
    out="${out%%\[*}"
    printf 'FAKE-IMAGE' > "$out"
    exit 0 ;;
esac
exit 0
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	c, err := New(Options{VipsBin: stub, GotenbergURL: gotenbergURL})
	if err != nil {
		t.Fatal(err)
	}
	return c, argsFile
}

func readArgs(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestDetect(t *testing.T) {
	docx := zipBytes(t, map[string]string{"word/document.xml": "<doc/>"})
	odt := zipBytes(t, map[string]string{
		"mimetype":    "application/vnd.oasis.opendocument.text",
		"content.xml": "<doc/>",
	})

	cases := []struct {
		name string
		file string
		data []byte
		want File
	}{
		{"heic extension", "IMG_0001.HEIC", nil, File{"heic", KindImage}},
		{"jpeg alias", "photo.jpeg", nil, File{"jpg", KindImage}},
		{"tif alias", "scan.tif", nil, File{"tiff", KindImage}},
		{"docx extension", "report.docx", nil, File{"docx", KindDocument}},
		{"odt extension", "letter.odt", nil, File{"odt", KindDocument}},
		{"svg extension", "logo.svg", nil, File{"svg", KindImage}},
		{"pdf extension", "file.pdf", nil, File{"pdf", KindPDF}},
		{"png magic", "upload", []byte("\x89PNG\r\n\x1a\nrest"), File{"png", KindImage}},
		{"pdf magic", "upload", []byte("%PDF-1.7\n"), File{"pdf", KindPDF}},
		{"heic magic", "upload", append([]byte{0, 0, 0, 0x18, 'f', 't', 'y', 'p', 'h', 'e', 'i', 'c'}, make([]byte, 8)...), File{"heic", KindImage}},
		{"svg text", "upload", []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"></svg>`), File{"svg", KindImage}},
		{"html text", "upload", []byte(`<!doctype html><html></html>`), File{"html", KindDocument}},
		{"docx zip", "upload", docx, File{"docx", KindDocument}},
		{"odt zip", "upload", odt, File{"odt", KindDocument}},
		{"plain text", "upload", []byte("hello world\n"), File{"txt", KindDocument}},
		{"garbage", "upload", []byte{0, 1, 2, 3}, File{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Detect(tc.file, tc.data); got != tc.want {
				t.Fatalf("Detect(%q) = %+v, want %+v", tc.file, got, tc.want)
			}
		})
	}
}

func zipBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestNormalizeTarget(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"", "", true},
		{"auto", "", true},
		{"PDF", "pdf", true},
		{"jpeg", "jpg", true},
		{"webp", "webp", true},
		{"gif", "", false},
	}
	for _, tc := range cases {
		got, ok := NormalizeTarget(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("NormalizeTarget(%q) = %q,%v want %q,%v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestConvertPassesThroughBrowserSafeImage(t *testing.T) {
	c, _ := newTestConverter(t, "")
	data := []byte("\x89PNG\r\n\x1a\noriginal")

	res, err := c.Convert(Request{Filename: "image.png", Data: data})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(res.Data, data) || res.ContentType != "image/png" || res.Format != "png" {
		t.Fatalf("result = %+v", res)
	}
}

func TestConvertExplicitSameFormatPassthrough(t *testing.T) {
	c, _ := newTestConverter(t, "")
	data := []byte("\x89PNG\r\n\x1a\noriginal")

	res, err := c.Convert(Request{Filename: "image.png", Data: data, To: "png"})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(res.Data, data) || res.Format != "png" {
		t.Fatalf("result = %+v", res)
	}
}

func TestConvertImageToPNG(t *testing.T) {
	c, argsFile := newTestConverter(t, "")
	data := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F'}

	res, err := c.Convert(Request{Filename: "photo.jpg", Data: data, To: "png", Quality: 77})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Data) != "FAKE-IMAGE" || res.ContentType != "image/png" || res.Format != "png" {
		t.Fatalf("result = %+v", res)
	}
	args := readArgs(t, argsFile)
	for _, want := range []string{"colourspace", "out.png[compression=6,keep=none]", "srgb"} {
		if !strings.Contains(args, want) {
			t.Errorf("vips args missing %q:\n%s", want, args)
		}
	}
}

func TestConvertPDFPageToPNG(t *testing.T) {
	c, argsFile := newTestConverter(t, "")

	_, err := c.Convert(Request{
		Filename: "doc.pdf",
		Data:     []byte("%PDF-1.7\nfake"),
		To:       "png",
		Page:     2,
		DPI:      300,
	})
	if err != nil {
		t.Fatal(err)
	}
	args := readArgs(t, argsFile)
	for _, want := range []string{"colourspace", "input.pdf[page=1,dpi=300]", "out.png[compression=6,keep=none]", "srgb"} {
		if !strings.Contains(args, want) {
			t.Errorf("vips args missing %q:\n%s", want, args)
		}
	}
}

func TestConvertImageResize(t *testing.T) {
	c, argsFile := newTestConverter(t, "")
	data := []byte("\x89PNG\r\n\x1a\noriginal")

	_, err := c.Convert(Request{
		Filename:  "photo.png",
		Data:      data,
		To:        "webp",
		MaxWidth:  1200,
		MaxHeight: 800,
		Quality:   80,
	})
	if err != nil {
		t.Fatal(err)
	}
	args := readArgs(t, argsFile)
	for _, want := range []string{"thumbnail", "1200", "--height", "800", "--size", "down", "colourspace", "out.webp[Q=80,keep=none,effort=4]", "srgb"} {
		if !strings.Contains(args, want) {
			t.Errorf("vips args missing %q:\n%s", want, args)
		}
	}
}

func TestNewWithoutVips(t *testing.T) {
	c, err := New(Options{VipsBin: "convert-server-missing-vips"})
	if err != nil {
		t.Fatalf("New() should tolerate a missing vips binary: %v", err)
	}
	if c.VipsEnabled() {
		t.Fatal("VipsEnabled() = true, want false")
	}
	if c.VipsVersion() != "" {
		t.Fatalf("VipsVersion() = %q, want empty", c.VipsVersion())
	}

	_, err = c.Convert(Request{
		Filename: "in.png",
		Data:     []byte("\x89PNG\r\n\x1a\nimage"),
		To:       "webp",
	})
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("image conversion error = %v, want ErrNotConfigured", err)
	}

	// PDF passthrough does not need vips.
	res, err := c.Convert(Request{
		Filename: "in.pdf",
		Data:     []byte("%PDF-1.7\npdf"),
		To:       "pdf",
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Data) != "%PDF-1.7\npdf" {
		t.Fatalf("passthrough = %q", res.Data)
	}
}

func TestGotenbergWorksWithoutVips(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("%PDF-FROM-GOTENBERG"))
	}))
	defer srv.Close()

	c, err := New(Options{VipsBin: "convert-server-missing-vips", GotenbergURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Convert(Request{
		Filename: "report.docx",
		Data:     zipBytes(t, map[string]string{"word/document.xml": "<doc/>"}),
		To:       "pdf",
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Data) != "%PDF-FROM-GOTENBERG" || res.ContentType != "application/pdf" {
		t.Fatalf("result = %+v", res)
	}
}

func TestConvertDocumentNeedsGotenberg(t *testing.T) {
	c, _ := newTestConverter(t, "")
	_, err := c.Convert(Request{
		Filename: "report.docx",
		Data:     zipBytes(t, map[string]string{"word/document.xml": "<doc/>"}),
		To:       "pdf",
	})
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Convert() error = %v, want ErrNotConfigured", err)
	}
}

func TestConvertDocumentViaGotenberg(t *testing.T) {
	var path, filename, payload string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		f, fh, err := r.FormFile("files")
		if err != nil {
			t.Errorf("form file: %v", err)
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		defer f.Close()
		data, _ := io.ReadAll(f)
		filename, payload = fh.Filename, string(data)
		w.Header().Set("content-type", "application/pdf")
		_, _ = w.Write([]byte("%PDF-FROM-GOTENBERG"))
	}))
	defer srv.Close()

	c, _ := newTestConverter(t, srv.URL)
	docx := zipBytes(t, map[string]string{"word/document.xml": "<doc/>"})

	res, err := c.Convert(Request{Filename: "report.docx", Data: docx, To: "pdf"})
	if err != nil {
		t.Fatal(err)
	}
	if path != "/forms/libreoffice/convert" || filename != "report.docx" || payload != string(docx) {
		t.Fatalf("gotenberg request = path %q, name %q, payload %d bytes", path, filename, len(payload))
	}
	if string(res.Data) != "%PDF-FROM-GOTENBERG" || res.ContentType != "application/pdf" {
		t.Fatalf("result = %+v", res)
	}
}

func TestConvertHTMLUsesChromiumRoute(t *testing.T) {
	var path, filename string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = r.ParseMultipartForm(1 << 20)
		_, fh, err := r.FormFile("files")
		if err == nil {
			filename = fh.Filename
		}
		_, _ = w.Write([]byte("%PDF"))
	}))
	defer srv.Close()

	c, _ := newTestConverter(t, srv.URL)
	if _, err := c.Convert(Request{
		Filename: "page.html",
		Data:     []byte("<!doctype html><html></html>"),
		To:       "pdf",
	}); err != nil {
		t.Fatal(err)
	}
	if path != "/forms/chromium/convert/html" || filename != "index.html" {
		t.Fatalf("path = %q, name = %q", path, filename)
	}
}

func TestConvertDocumentToImage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("%PDF-FROM-GOTENBERG"))
	}))
	defer srv.Close()

	c, _ := newTestConverter(t, srv.URL)
	docx := zipBytes(t, map[string]string{"word/document.xml": "<doc/>"})

	res, err := c.Convert(Request{Filename: "report.docx", Data: docx, To: "png"})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Data) != "FAKE-IMAGE" || res.ContentType != "image/png" {
		t.Fatalf("result = %+v", res)
	}
}

func TestConvertGotenbergError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "conversion failed", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c, _ := newTestConverter(t, srv.URL)
	_, err := c.Convert(Request{
		Filename: "report.docx",
		Data:     zipBytes(t, map[string]string{"word/document.xml": "<doc/>"}),
		To:       "pdf",
	})
	if !errors.Is(err, ErrBackend) {
		t.Fatalf("Convert() error = %v, want ErrBackend", err)
	}
}

func TestConvertEmptyFile(t *testing.T) {
	c, _ := newTestConverter(t, "")
	if _, err := c.Convert(Request{Filename: "a.png"}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Convert() error = %v, want ErrUnsupported", err)
	}
}

func TestJPEGToPDF(t *testing.T) {
	var jpg bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 2, 3))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	if err := jpeg.Encode(&jpg, img, nil); err != nil {
		t.Fatal(err)
	}

	pdf, err := jpegToPDF(jpg.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	text := string(pdf)
	for _, want := range []string{"%PDF-1.4", "/Width 2", "/Height 3", "/Filter /DCTDecode", "%%EOF"} {
		if !strings.Contains(text, want) {
			t.Errorf("pdf missing %q", want)
		}
	}
}

func TestResizeBounds(t *testing.T) {
	w, h := resizeBounds(Request{})
	if w != MaxDimension || h != MaxDimension {
		t.Fatalf("resizeBounds() = %d,%d", w, h)
	}
	w, h = resizeBounds(Request{MaxWidth: 500})
	if w != 500 || h != MaxDimension {
		t.Fatalf("resizeBounds() = %d,%d", w, h)
	}
}

func TestOutputSpec(t *testing.T) {
	if got := outputSpec("out.jpg", "jpg", 91); got != "out.jpg[Q=91,keep=none,background=255 255 255]" {
		t.Errorf("jpg spec = %q", got)
	}
	if got := outputSpec("out.webp", "webp", 60); got != "out.webp[Q=60,keep=none,effort=4]" {
		t.Errorf("webp spec = %q", got)
	}
}

func TestMultipartFileField(t *testing.T) {
	// Guard the Gotenberg wire contract: LibreOffice expects `files`.
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_, fh, err := r.FormFile("files")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		got = fh.Filename
		_, _ = w.Write([]byte("%PDF"))
	}))
	defer srv.Close()

	c, _ := newTestConverter(t, srv.URL)
	if _, err := c.Convert(Request{
		Filename: "a.odt",
		Data:     zipBytes(t, map[string]string{"mimetype": "application/vnd.oasis.opendocument.text"}),
		To:       "pdf",
	}); err != nil {
		t.Fatal(err)
	}
	if got != "a.odt" {
		t.Fatalf("gotenberg filename = %q", got)
	}
}
