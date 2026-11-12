// Package convert turns uploaded files into browser-viewable output.
//
// Images are handled by libvips (including HEIC/AVIF/TIFF/WebP/SVG and
// PDF -> raster through the poppler plugin). Office documents and other
// LibreOffice formats are delegated to a Gotenberg instance over HTTP.
// Typst templates have their own endpoint and live in internal/typst.
package convert

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Timeout bounds for one conversion.
const (
	DefaultTimeout = 30 * time.Second
	MinTimeout     = 1 * time.Second
	MaxTimeout     = 120 * time.Second
)

// Rasterisation bounds.
const (
	DefaultDPI     = 150
	MinDPI         = 36
	MaxDPI         = 600
	DefaultQuality = 85
	MinQuality     = 1
	MaxQuality     = 100
	MaxDimension   = 20000
)

// maxResponseBytes caps what a backend may return (Gotenberg PDFs can be
// large, but not unbounded).
const maxResponseBytes = 256 << 20

// Errors are sentinels so the HTTP layer can choose a status code.
var (
	ErrUnsupported   = errors.New("unsupported format")
	ErrTimeout       = errors.New("conversion timed out")
	ErrBackend       = errors.New("conversion backend failed")
	ErrNotConfigured = errors.New("conversion backend not configured")
)

// Request describes one conversion.
type Request struct {
	// Filename supplies the extension used to detect the input format.
	Filename string
	// Data is the raw uploaded file.
	Data []byte
	// To is one of pdf, png, jpg, webp. Empty means the browser-safe default.
	To string
	// Page is the 1-based page/frame for PDF, TIFF, HEIC and GIF inputs.
	Page int
	// DPI is used when rasterising PDF or SVG input.
	DPI int
	// MaxWidth/MaxHeight downscale only; zero means no bound.
	MaxWidth  int
	MaxHeight int
	// Quality is the JPEG/WebP quality (1-100).
	Quality int
	// TimeoutMs overrides DefaultTimeout when within [MinTimeout, MaxTimeout].
	TimeoutMs *int64
}

// Result is the converted payload and its media type.
type Result struct {
	Data        []byte
	ContentType string
	Format      string
}

// Options configures a Converter.
type Options struct {
	// VipsBin is the libvips CLI name or path (default "vips").
	VipsBin string
	// GotenbergURL enables Office/text conversions when non-empty, e.g.
	// http://gotenberg:3000.
	GotenbergURL string
	// MaxConcurrency caps parallel conversions; <= 0 means unlimited.
	MaxConcurrency int
	// HTTPClient overrides the Gotenberg client (tests).
	HTTPClient *http.Client
}

// Converter is safe for concurrent use; create one per process.
type Converter struct {
	vipsBin   string
	gotenberg *url.URL
	client    *http.Client
	sem       chan struct{} // nil means unlimited
}

// New resolves the vips binary and parses the Gotenberg URL. A missing vips
// binary is not fatal: the Typst endpoint and PDF passthrough still work, and
// image conversions report ErrNotConfigured. This is what makes the typst-only
// image variant possible.
func New(opts Options) (*Converter, error) {
	bin := opts.VipsBin
	if bin == "" {
		bin = "vips"
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		resolved = ""
	}

	c := &Converter{vipsBin: resolved, client: opts.HTTPClient}
	if c.client == nil {
		c.client = &http.Client{}
	}
	if opts.GotenbergURL != "" {
		u, err := url.Parse(opts.GotenbergURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("invalid CONVERT_GOTENBERG_URL %q", opts.GotenbergURL)
		}
		c.gotenberg = u
	}
	if opts.MaxConcurrency > 0 {
		c.sem = make(chan struct{}, opts.MaxConcurrency)
	}
	return c, nil
}

// VipsEnabled reports whether the image/PDF raster backend is available.
func (c *Converter) VipsEnabled() bool { return c.vipsBin != "" }

// VipsVersion returns the `vips --version` banner, or "" when unavailable.
func (c *Converter) VipsVersion() string {
	if !c.VipsEnabled() {
		return ""
	}
	out, err := exec.Command(c.vipsBin, "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// GotenbergEnabled reports whether Office conversions are configured.
func (c *Converter) GotenbergEnabled() bool { return c.gotenberg != nil }

// TimeoutFor resolves a request's timeout, clamping to [MinTimeout, MaxTimeout].
func TimeoutFor(req Request) time.Duration {
	t := DefaultTimeout
	if req.TimeoutMs != nil {
		if ms := *req.TimeoutMs; ms >= MinTimeout.Milliseconds() && ms <= MaxTimeout.Milliseconds() {
			t = time.Duration(ms) * time.Millisecond
		}
	}
	return t
}

// Convert runs the request end to end.
func (c *Converter) Convert(req Request) (Result, error) {
	if len(req.Data) == 0 {
		return Result{}, fmt.Errorf("%w: empty file", ErrUnsupported)
	}

	file := Detect(req.Filename, req.Data)
	if file.Format == "" {
		name := req.Filename
		if name == "" {
			name = "upload"
		}
		return Result{}, fmt.Errorf("%w: cannot determine format of %s", ErrUnsupported, name)
	}

	target, ok := NormalizeTarget(req.To)
	if !ok {
		return Result{}, fmt.Errorf("%w: target %q (want pdf, png, jpg or webp)", ErrUnsupported, req.To)
	}
	explicitTarget := target != ""
	if !explicitTarget {
		target = file.DefaultTarget()
	}

	// Already browser-viewable and no conversion requested: return the bytes
	// untouched, preserving animation (GIF) and avoiding a pointless re-encode.
	if !explicitTarget && (file.Passthrough() || file.Format == "pdf") {
		return Result{Data: req.Data, ContentType: ContentType(file.Format), Format: file.Format}, nil
	}
	if target == "pdf" && file.Format == "pdf" {
		return Result{Data: req.Data, ContentType: "application/pdf", Format: "pdf"}, nil
	}
	// Same browser-safe format with no transform options: no point re-encoding.
	if target == file.Format && file.Passthrough() && !hasTransform(req) {
		return Result{Data: req.Data, ContentType: ContentType(file.Format), Format: file.Format}, nil
	}

	// Everything below this point either rasterises or transcodes through
	// vips; document -> PDF is the only exception (Gotenberg handles it).
	needsVips := file.Kind == KindImage ||
		(file.Format == "pdf" && target != "pdf") ||
		(file.Kind == KindDocument && target != "pdf")
	if needsVips && !c.VipsEnabled() {
		return Result{}, fmt.Errorf("%w: image/PDF conversion requires libvips (rebuild with WITH_VIPS=true)", ErrNotConfigured)
	}

	timeout := TimeoutFor(req)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if err := c.acquire(ctx); err != nil {
		return Result{}, fmt.Errorf("%w after %dms waiting for a free slot", ErrTimeout, timeout.Milliseconds())
	}
	defer c.release()

	switch {
	case file.Kind == KindImage && target == "pdf":
		return c.imageToPDF(ctx, file, req)
	case file.Kind == KindImage:
		return c.imageTo(ctx, file, req, target)
	case file.Format == "pdf":
		return c.pdfToImage(ctx, req.Data, req, target)
	default:
		return c.document(ctx, file, req, target)
	}
}

func (c *Converter) imageTo(ctx context.Context, file File, req Request, target string) (Result, error) {
	dir, err := os.MkdirTemp("", "convert-server-")
	if err != nil {
		return Result{}, fmt.Errorf("temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	input := filepath.Join(dir, "input."+file.Format)
	if err := os.WriteFile(input, req.Data, 0o600); err != nil {
		return Result{}, fmt.Errorf("write input: %w", err)
	}
	output := filepath.Join(dir, "out."+target)
	if err := c.renderImage(ctx, input, output, file, req, target); err != nil {
		return Result{}, err
	}
	data, err := os.ReadFile(output)
	if err != nil {
		return Result{}, fmt.Errorf("read output: %w", err)
	}
	return Result{Data: data, ContentType: ContentType(target), Format: target}, nil
}

func (c *Converter) imageToPDF(ctx context.Context, file File, req Request) (Result, error) {
	dir, err := os.MkdirTemp("", "convert-server-")
	if err != nil {
		return Result{}, fmt.Errorf("temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	input := filepath.Join(dir, "input."+file.Format)
	if err := os.WriteFile(input, req.Data, 0o600); err != nil {
		return Result{}, fmt.Errorf("write input: %w", err)
	}

	src := inputSpec(input, file, req)
	if req.MaxWidth > 0 || req.MaxHeight > 0 {
		resized := filepath.Join(dir, "resized.png")
		if err := c.thumbnail(ctx, src, resized, req); err != nil {
			return Result{}, err
		}
		src = resized
	}

	// Normalising through sRGB keeps the PDF image a plain DeviceRGB JPEG,
	// which also flattens alpha onto white.
	jpg := filepath.Join(dir, "page.jpg")
	args := []string{"colourspace", src, outputSpec(jpg, "jpg", qualityFor(req)), "srgb"}
	if err := c.run(ctx, args...); err != nil {
		return Result{}, err
	}
	data, err := os.ReadFile(jpg)
	if err != nil {
		return Result{}, fmt.Errorf("read jpeg: %w", err)
	}
	pdf, err := jpegToPDF(data)
	if err != nil {
		return Result{}, err
	}
	return Result{Data: pdf, ContentType: "application/pdf", Format: "pdf"}, nil
}

func (c *Converter) pdfToImage(ctx context.Context, pdf []byte, req Request, target string) (Result, error) {
	dir, err := os.MkdirTemp("", "convert-server-")
	if err != nil {
		return Result{}, fmt.Errorf("temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	input := filepath.Join(dir, "input.pdf")
	if err := os.WriteFile(input, pdf, 0o600); err != nil {
		return Result{}, fmt.Errorf("write pdf: %w", err)
	}
	output := filepath.Join(dir, "out."+target)
	if err := c.renderImage(ctx, input, output, File{Format: "pdf", Kind: KindPDF}, req, target); err != nil {
		return Result{}, err
	}
	data, err := os.ReadFile(output)
	if err != nil {
		return Result{}, fmt.Errorf("read output: %w", err)
	}
	return Result{Data: data, ContentType: ContentType(target), Format: target}, nil
}

func (c *Converter) document(ctx context.Context, file File, req Request, target string) (Result, error) {
	pdf, err := c.documentToPDF(ctx, file, req)
	if err != nil {
		return Result{}, err
	}
	if target == "pdf" {
		return Result{Data: pdf, ContentType: "application/pdf", Format: "pdf"}, nil
	}
	return c.pdfToImage(ctx, pdf, req, target)
}

func (c *Converter) documentToPDF(ctx context.Context, file File, req Request) ([]byte, error) {
	if c.gotenberg == nil {
		return nil, fmt.Errorf("%w: set CONVERT_GOTENBERG_URL to convert %s", ErrNotConfigured, file.Format)
	}

	route := "libreoffice/convert"
	name := filepath.Base(strings.TrimSpace(req.Filename))
	if file.Format == "html" {
		route = "chromium/convert/html"
		name = "index.html"
	}
	if name == "" || name == "." || name == ".." {
		name = "upload." + file.Format
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("files", name)
	if err != nil {
		return nil, fmt.Errorf("build gotenberg request: %w", err)
	}
	if _, err := part.Write(req.Data); err != nil {
		return nil, fmt.Errorf("build gotenberg request: %w", err)
	}
	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("build gotenberg request: %w", err)
	}

	endpoint := *c.gotenberg
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/forms/" + route
	endpoint.RawQuery = ""
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), &body)
	if err != nil {
		return nil, fmt.Errorf("build gotenberg request: %w", err)
	}
	httpReq.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := c.client.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: gotenberg did not answer in time", ErrTimeout)
		}
		return nil, fmt.Errorf("%w: gotenberg: %v", ErrBackend, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("%w: gotenberg %s: %s", ErrBackend, resp.Status, strings.TrimSpace(string(msg)))
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: reading gotenberg response: %v", ErrBackend, err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: gotenberg returned an empty body", ErrBackend)
	}
	if len(data) > maxResponseBytes {
		return nil, fmt.Errorf("%w: gotenberg response exceeds %dMB", ErrBackend, maxResponseBytes>>20)
	}
	return data, nil
}

// renderImage rasterises/transcodes an image through sRGB. The colourspace
// step also normalises 16-bit input to 8-bit output, which is what browsers
// want for previews.
func (c *Converter) renderImage(ctx context.Context, input, output string, file File, req Request, target string) error {
	src := inputSpec(input, file, req)
	dst := outputSpec(output, target, qualityFor(req))
	if req.MaxWidth > 0 || req.MaxHeight > 0 {
		staged := output + ".stage.png"
		if err := c.thumbnail(ctx, src, staged, req); err != nil {
			return err
		}
		defer os.Remove(staged)
		src = staged
	}
	return c.run(ctx, "colourspace", src, dst, "srgb")
}

func (c *Converter) thumbnail(ctx context.Context, src, dst string, req Request) error {
	width, height := resizeBounds(req)
	return c.run(ctx, "thumbnail", src, dst,
		strconv.Itoa(width), "--height", strconv.Itoa(height), "--size", "down")
}

func (c *Converter) run(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, c.vipsBin, args...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("%w: vips timed out", ErrTimeout)
	}
	msg := strings.TrimSpace(string(out))
	if len(msg) > 2048 {
		msg = msg[:2048]
	}
	if msg == "" {
		msg = err.Error()
	}
	return fmt.Errorf("vips %s failed: %s", args[0], msg)
}

// inputSpec adds loader options to the filename, e.g. page and dpi.
func inputSpec(path string, file File, req Request) string {
	var opts []string
	if multiPage(file.Format) {
		page := req.Page
		if page < 1 {
			page = 1
		}
		opts = append(opts, "page="+strconv.Itoa(page-1))
	}
	if file.Format == "pdf" || file.Format == "svg" || file.Format == "svgz" {
		opts = append(opts, "dpi="+strconv.Itoa(dpiFor(req)))
	}
	if len(opts) == 0 {
		return path
	}
	return path + "[" + strings.Join(opts, ",") + "]"
}

func outputSpec(path, target string, quality int) string {
	switch target {
	case "jpg":
		return fmt.Sprintf("%s[Q=%d,keep=none,background=255 255 255]", path, quality)
	case "webp":
		return fmt.Sprintf("%s[Q=%d,keep=none,effort=4]", path, quality)
	case "png":
		return path + "[compression=6,keep=none]"
	default:
		return path
	}
}

func multiPage(format string) bool {
	switch format {
	case "pdf", "tiff", "heic", "heif", "avif", "gif":
		return true
	}
	return false
}

func hasTransform(req Request) bool {
	return req.Page > 0 || req.DPI > 0 || req.MaxWidth > 0 || req.MaxHeight > 0 || req.Quality > 0
}

func dpiFor(req Request) int {
	if req.DPI >= MinDPI && req.DPI <= MaxDPI {
		return req.DPI
	}
	return DefaultDPI
}

func qualityFor(req Request) int {
	if req.Quality >= MinQuality && req.Quality <= MaxQuality {
		return req.Quality
	}
	return DefaultQuality
}

func resizeBounds(req Request) (width, height int) {
	width, height = req.MaxWidth, req.MaxHeight
	if width <= 0 || width > MaxDimension {
		width = MaxDimension
	}
	if height <= 0 || height > MaxDimension {
		height = MaxDimension
	}
	return width, height
}

func (c *Converter) acquire(ctx context.Context) error {
	if c.sem == nil {
		return nil
	}
	select {
	case c.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Converter) release() {
	if c.sem != nil {
		<-c.sem
	}
}

// jpegToPDF wraps one JPEG image in a minimal PDF page. The page is sized at
// 96 DPI so a browser renders the image at roughly its pixel size.
func jpegToPDF(data []byte) ([]byte, error) {
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode jpeg: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, fmt.Errorf("decode jpeg: bad dimensions %dx%d", cfg.Width, cfg.Height)
	}
	width := float64(cfg.Width) * 72 / 96
	height := float64(cfg.Height) * 72 / 96

	var b bytes.Buffer
	offsets := make([]int, 6)
	obj := func(n int, format string, args ...any) {
		offsets[n] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n", n)
		fmt.Fprintf(&b, format, args...)
		b.WriteString("\nendobj\n")
	}

	b.WriteString("%PDF-1.4\n%\xE2\xE3\xCF\xD3\n")
	obj(1, "<< /Type /Catalog /Pages 2 0 R >>")
	obj(2, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	obj(3, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %.2f %.2f] /Resources << /XObject << /Im0 5 0 R >> >> /Contents 4 0 R >>",
		width, height)
	content := fmt.Sprintf("q %.2f 0 0 %.2f 0 0 cm /Im0 Do Q\n", width, height)
	obj(4, "<< /Length %d >>\nstream\n%sendstream", len(content), content)
	offsets[5] = b.Len()
	fmt.Fprintf(&b, "5 0 obj\n<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /DCTDecode /Length %d >>\nstream\n",
		cfg.Width, cfg.Height, len(data))
	b.Write(data)
	b.WriteString("\nendstream\nendobj\n")

	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 6\n0000000000 65535 f \n")
	for i := 1; i <= 5; i++ {
		fmt.Fprintf(&b, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&b, "trailer\n<< /Size 6 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", xref)
	return b.Bytes(), nil
}
