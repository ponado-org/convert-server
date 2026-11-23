// Package server exposes the HTTP surface of convert-server:
//
//	GET  /health        -> "ok"
//	GET  /version       -> service, Go, typst, vips and Gotenberg status
//	GET  /formats       -> accepted inputs and output targets
//	POST /typst/render  -> application/pdf | application/json error
//	POST /convert       -> converted bytes | application/json error
//
// /typst/render accepts both application/json (base64 files) and
// multipart/form-data (binary parts). See decode.go.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"runtime"
	"strings"
	"time"

	"convert-server/internal/convert"
	"convert-server/internal/typst"
)

// Name is the service name reported by /version and used in logs.
const Name = "convert-server"

// gotenbergPrefix mounts the transparent Gotenberg reverse proxy. It is
// stripped before forwarding, so /gotenberg/forms/pdf/merge reaches
// ${CONVERT_GOTENBERG_URL}/forms/pdf/merge.
const gotenbergPrefix = "/gotenberg"

// Renderer is the Typst compile backend; internal/typst satisfies it.
type Renderer interface {
	Render(typst.Request) ([]byte, error)
}

// Converter is the file conversion backend; internal/convert satisfies it.
type Converter interface {
	Convert(convert.Request) (convert.Result, error)
	VipsEnabled() bool
	GotenbergEnabled() bool
}

// Options configures a Server.
type Options struct {
	// ServiceVersion is the convert-server release (set at build time via
	// -ldflags "-X main.version=..."); informational only.
	ServiceVersion string
	// TypstVersion and VipsVersion are engine banners for /version.
	TypstVersion string
	VipsVersion  string
	// MaxBodyBytes caps request bodies.
	MaxBodyBytes int64
	// GotenbergURL enables the /gotenberg reverse proxy when non-nil.
	GotenbergURL *url.URL
}

// Server serves the wire contract for one renderer, one converter and one
// configuration.
type Server struct {
	renderer       Renderer
	converter      Converter
	serviceVersion string
	typstVersion   string
	vipsVersion    string
	maxBodyBytes   int64
	gotenbergProxy *httputil.ReverseProxy
}

// New wires a server.
func New(renderer Renderer, converter Converter, opts Options) *Server {
	serviceVersion := opts.ServiceVersion
	if serviceVersion == "" {
		serviceVersion = "dev"
	}
	maxBody := opts.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = 25 << 20
	}
	s := &Server{
		renderer:       renderer,
		converter:      converter,
		serviceVersion: serviceVersion,
		typstVersion:   opts.TypstVersion,
		vipsVersion:    opts.VipsVersion,
		maxBodyBytes:   maxBody,
	}
	if opts.GotenbergURL != nil {
		s.gotenbergProxy = newGotenbergProxy(*opts.GotenbergURL, maxBody)
	}
	return s
}

// newGotenbergProxy forwards /gotenberg/<path> to <target>/<path> unchanged:
// method, query, headers and body. Only the mount prefix is stripped.
func newGotenbergProxy(target url.URL, maxBodyBytes int64) *httputil.ReverseProxy {
	proxy := httputil.NewSingleHostReverseProxy(&target)
	proxy.Director = nil
	proxy.Rewrite = func(pr *httputil.ProxyRequest) {
		pr.Out.URL.Path = strings.TrimPrefix(pr.Out.URL.Path, gotenbergPrefix)
		pr.Out.URL.RawPath = ""
		pr.SetURL(&target)
		pr.Out.Host = target.Host
		pr.SetXForwarded()
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
		log.Printf("gotenberg proxy %s %s -> %d", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode)
		return nil
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) || strings.Contains(err.Error(), "request body too large") {
			apiError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("request body exceeds %dMB", maxBodyBytes>>20))
			return
		}
		status := http.StatusBadGateway
		if errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		log.Printf("gotenberg proxy %s %s failed: %v", r.Method, r.URL.Path, err)
		apiError(w, status, "gotenberg proxy failed: "+err.Error())
	}
	return proxy
}

// Handler returns the routed HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /version", s.handleVersion)
	mux.HandleFunc("GET /formats", s.handleFormats)
	mux.HandleFunc("POST /typst/render", s.handleRender)
	mux.HandleFunc("POST /convert", s.handleConvert)
	mux.Handle(gotenbergPrefix+"/", s.handleGotenbergProxy())
	return mux
}

func (s *Server) handleGotenbergProxy() http.Handler {
	if s.gotenbergProxy == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			apiError(w, http.StatusNotImplemented, "gotenberg proxy is not configured")
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > s.maxBodyBytes {
			apiError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("request body exceeds %dMB", s.maxBodyBytes>>20))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes)
		s.gotenbergProxy.ServeHTTP(w, r)
	})
}

func (s *Server) handleRender(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	req, err := s.decodeRender(w, r)
	if err != nil {
		s.writeDecodeError(w, err)
		return
	}
	if req.Template == "" {
		apiError(w, http.StatusBadRequest, "template is required")
		return
	}

	pdf, err := s.renderer.Render(req)
	if err != nil {
		status := http.StatusUnprocessableEntity
		if strings.Contains(err.Error(), "timed out") {
			status = http.StatusRequestTimeout
		}
		log.Printf("render failed in %s: %v", time.Since(start).Round(time.Millisecond), err)
		apiError(w, status, err.Error())
		return
	}

	log.Printf("render ok in %s (%d shadow files, %d inputs, %d bytes pdf)",
		time.Since(start).Round(time.Millisecond), len(req.Files), len(req.Inputs), len(pdf))

	writeBody(w, "application/pdf", pdf)
}

func (s *Server) handleConvert(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	req, err := s.decodeConvert(w, r)
	if err != nil {
		s.writeDecodeError(w, err)
		return
	}

	res, err := s.converter.Convert(req)
	if err != nil {
		status := convertStatus(err)
		log.Printf("convert failed in %s (%s -> %s): %v",
			time.Since(start).Round(time.Millisecond), req.Filename, targetLabel(req.To), err)
		apiError(w, status, err.Error())
		return
	}

	log.Printf("convert ok in %s (%s -> %s, %d bytes)",
		time.Since(start).Round(time.Millisecond), req.Filename, res.Format, len(res.Data))

	writeBody(w, res.ContentType, res.Data)
}

func (s *Server) writeDecodeError(w http.ResponseWriter, err error) {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		apiError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("request body exceeds %dMB", s.maxBodyBytes>>20))
		return
	}
	apiError(w, http.StatusBadRequest, "invalid request: "+err.Error())
}

func writeBody(w http.ResponseWriter, contentType string, data []byte) {
	w.Header().Set("content-type", contentType)
	w.Header().Set("content-disposition", "inline")
	w.Header().Set("x-content-type-options", "nosniff")
	w.Header().Set("cache-control", "no-store")
	_, _ = w.Write(data)
}

func convertStatus(err error) int {
	switch {
	case errors.Is(err, convert.ErrUnsupported):
		return http.StatusUnsupportedMediaType
	case errors.Is(err, convert.ErrTimeout):
		return http.StatusRequestTimeout
	case errors.Is(err, convert.ErrNotConfigured):
		return http.StatusNotImplemented
	case errors.Is(err, convert.ErrBackend):
		return http.StatusBadGateway
	default:
		return http.StatusUnprocessableEntity
	}
}

func targetLabel(to string) string {
	if to == "" {
		return "auto"
	}
	return to
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("content-type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("content-type", "application/json; charset=utf-8")
	vips := s.vipsVersion
	if !s.converter.VipsEnabled() {
		vips = "disabled"
	} else if vips == "" {
		vips = "unknown"
	}
	gotenberg := "disabled"
	if s.converter.GotenbergEnabled() {
		gotenberg = "enabled"
	}
	_ = json.NewEncoder(w).Encode(map[string]string{
		"service":   Name,
		"version":   s.serviceVersion,
		"go":        runtime.Version(),
		"typst":     s.typstVersion,
		"vips":      vips,
		"gotenberg": gotenberg,
	})
}

func (s *Server) handleFormats(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("content-type", "application/json; charset=utf-8")

	inputs := convert.SupportedInputs()
	outputs := []string{"pdf", "png", "jpg", "webp"}
	if !s.converter.VipsEnabled() {
		// Without libvips only PDF passthrough and Gotenberg -> PDF remain.
		delete(inputs, "image")
		outputs = []string{"pdf"}
	}
	if !s.converter.GotenbergEnabled() {
		delete(inputs, "document")
	}

	_ = json.NewEncoder(w).Encode(map[string]any{
		"inputs":    inputs,
		"outputs":   outputs,
		"typst":     true,
		"vips":      s.converter.VipsEnabled(),
		"gotenberg": s.converter.GotenbergEnabled(),
	})
}

func apiError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
