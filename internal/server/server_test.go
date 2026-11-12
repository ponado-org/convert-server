package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"convert-server/internal/convert"
	"convert-server/internal/typst"
)

type fakeRenderer struct {
	pdf   []byte
	err   error
	calls int
	last  typst.Request
}

func (f *fakeRenderer) Render(req typst.Request) ([]byte, error) {
	f.calls++
	f.last = req
	if f.err != nil {
		return nil, f.err
	}
	return f.pdf, nil
}

type fakeConverter struct {
	result           convert.Result
	err              error
	calls            int
	last             convert.Request
	vipsEnabled      bool
	gotenbergEnabled bool
}

func (f *fakeConverter) Convert(req convert.Request) (convert.Result, error) {
	f.calls++
	f.last = req
	if f.err != nil {
		return convert.Result{}, f.err
	}
	return f.result, nil
}

func (f *fakeConverter) VipsEnabled() bool      { return f.vipsEnabled }
func (f *fakeConverter) GotenbergEnabled() bool { return f.gotenbergEnabled }

func newTestServer(renderer Renderer, converter Converter, opts Options) *Server {
	return New(renderer, converter, opts)
}

func TestHandleRenderJSON(t *testing.T) {
	fake := &fakeRenderer{pdf: []byte("PDF-BYTES")}
	s := newTestServer(fake, &fakeConverter{}, Options{TypstVersion: "typst test", MaxBodyBytes: 1 << 20})

	b64 := base64.StdEncoding.EncodeToString([]byte(`{"n":1}`))
	req := jsonRequest(`{"template":"= hi","files":{"/data.json":"` + b64 + `"}}`)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("content-type"); got != "application/pdf" {
		t.Fatalf("content-type = %q", got)
	}
	if rec.Body.String() != "PDF-BYTES" {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if string(fake.last.Files["/data.json"]) != `{"n":1}` {
		t.Fatalf("renderer got files[/data.json] = %q", fake.last.Files["/data.json"])
	}
}

func TestHandleRenderMultipart(t *testing.T) {
	fake := &fakeRenderer{pdf: []byte("PDF-BYTES")}
	s := newTestServer(fake, &fakeConverter{}, Options{TypstVersion: "typst test", MaxBodyBytes: 1 << 20})

	req := multipartRequest(t, func(mw *multipart.Writer) {
		_ = mw.WriteField("template", "= hi")
		w, _ := mw.CreateFormFile("files", "data.json")
		_, _ = w.Write([]byte(`{"n":1}`))
	})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "PDF-BYTES" {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if fake.last.Template != "= hi" || string(fake.last.Files["/data.json"]) != `{"n":1}` {
		t.Fatalf("renderer got %+v", fake.last)
	}
}

func TestHandleRenderRequiresTemplate(t *testing.T) {
	s := newTestServer(&fakeRenderer{}, &fakeConverter{}, Options{MaxBodyBytes: 1 << 20})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, jsonRequest(`{"files":{}}`))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleRenderErrorStatuses(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"compile error", errors.New("typst compile failed: boom"), http.StatusUnprocessableEntity},
		{"timeout", errors.New("typst compile timed out after 1000ms"), http.StatusRequestTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(&fakeRenderer{err: tc.err}, &fakeConverter{}, Options{MaxBodyBytes: 1 << 20})
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, jsonRequest(`{"template":"= hi"}`))

			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("error body is not JSON: %v", err)
			}
			if !strings.Contains(body["error"], tc.err.Error()) {
				t.Fatalf("error = %q, want it to contain %q", body["error"], tc.err.Error())
			}
		})
	}
}

func TestHandleRenderRejectsOversizeBody(t *testing.T) {
	s := newTestServer(&fakeRenderer{}, &fakeConverter{}, Options{MaxBodyBytes: 16})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, jsonRequest(`{"template":"longer than sixteen bytes"}`))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

func TestHandleConvertMultipart(t *testing.T) {
	fake := &fakeConverter{result: convert.Result{
		Data:        []byte("PNG-BYTES"),
		ContentType: "image/png",
		Format:      "png",
	}}
	s := newTestServer(&fakeRenderer{}, fake, Options{MaxBodyBytes: 1 << 20})

	req := multipartConvertRequest(t, func(mw *multipart.Writer) {
		_ = mw.WriteField("to", "png")
		_ = mw.WriteField("page", "2")
		_ = mw.WriteField("dpi", "300")
		_ = mw.WriteField("maxWidth", "1200")
		_ = mw.WriteField("quality", "90")
		w, _ := mw.CreateFormFile("file", "photo.heic")
		_, _ = w.Write([]byte("HEIC-DATA"))
	})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("content-type"); got != "image/png" {
		t.Fatalf("content-type = %q", got)
	}
	if rec.Body.String() != "PNG-BYTES" {
		t.Fatalf("body = %q", rec.Body.String())
	}
	last := fake.last
	if last.Filename != "photo.heic" || string(last.Data) != "HEIC-DATA" || last.To != "png" {
		t.Fatalf("converter got %+v", last)
	}
	if last.Page != 2 || last.DPI != 300 || last.MaxWidth != 1200 || last.Quality != 90 {
		t.Fatalf("converter options = %+v", last)
	}
}

func TestHandleConvertJSON(t *testing.T) {
	fake := &fakeConverter{result: convert.Result{
		Data:        []byte("PDF-BYTES"),
		ContentType: "application/pdf",
		Format:      "pdf",
	}}
	s := newTestServer(&fakeRenderer{}, fake, Options{MaxBodyBytes: 1 << 20})

	b64 := base64.StdEncoding.EncodeToString([]byte("DOCX-DATA"))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, jsonConvertRequest(
		`{"filename":"report.docx","data":"`+b64+`","to":"pdf","timeoutMs":5000}`,
	))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "PDF-BYTES" {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if fake.last.Filename != "report.docx" || string(fake.last.Data) != "DOCX-DATA" || fake.last.To != "pdf" {
		t.Fatalf("converter got %+v", fake.last)
	}
}

func TestHandleConvertRequiresFile(t *testing.T) {
	s := newTestServer(&fakeRenderer{}, &fakeConverter{}, Options{MaxBodyBytes: 1 << 20})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, multipartConvertRequest(t, func(mw *multipart.Writer) {
		_ = mw.WriteField("to", "pdf")
	}))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleConvertInvalidTarget(t *testing.T) {
	s := newTestServer(&fakeRenderer{}, &fakeConverter{}, Options{MaxBodyBytes: 1 << 20})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, multipartConvertRequest(t, func(mw *multipart.Writer) {
		_ = mw.WriteField("to", "gif")
		w, _ := mw.CreateFormFile("file", "a.png")
		_, _ = w.Write([]byte("PNG"))
	}))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleConvertErrorStatuses(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"unsupported", fmt.Errorf("%w: rar", convert.ErrUnsupported), http.StatusUnsupportedMediaType},
		{"timeout", fmt.Errorf("%w: vips", convert.ErrTimeout), http.StatusRequestTimeout},
		{"not configured", fmt.Errorf("%w: gotenberg", convert.ErrNotConfigured), http.StatusNotImplemented},
		{"backend", fmt.Errorf("%w: gotenberg 500", convert.ErrBackend), http.StatusBadGateway},
		{"conversion", errors.New("vips copy failed"), http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(&fakeRenderer{}, &fakeConverter{err: tc.err}, Options{MaxBodyBytes: 1 << 20})
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, multipartConvertRequest(t, func(mw *multipart.Writer) {
				w, _ := mw.CreateFormFile("file", "a.png")
				_, _ = w.Write([]byte("PNG"))
			}))

			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestHealthVersionFormats(t *testing.T) {
	s := newTestServer(&fakeRenderer{}, &fakeConverter{
		vipsEnabled:      true,
		gotenbergEnabled: true,
	}, Options{
		ServiceVersion: "1.2.3",
		TypstVersion:   "typst 0.15.1",
		VipsVersion:    "vips-8.18.7",
		MaxBodyBytes:   1 << 20,
	})

	health := httptest.NewRecorder()
	s.Handler().ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health", nil))
	if health.Code != http.StatusOK || health.Body.String() != "ok\n" {
		t.Fatalf("health = %d %q", health.Code, health.Body.String())
	}

	version := httptest.NewRecorder()
	s.Handler().ServeHTTP(version, httptest.NewRequest(http.MethodGet, "/version", nil))
	var body map[string]string
	if err := json.Unmarshal(version.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["service"] != Name || body["typst"] != "typst 0.15.1" || body["version"] != "1.2.3" {
		t.Fatalf("version = %v", body)
	}
	if body["vips"] != "vips-8.18.7" || body["gotenberg"] != "enabled" {
		t.Fatalf("version = %v", body)
	}

	formats := httptest.NewRecorder()
	s.Handler().ServeHTTP(formats, httptest.NewRequest(http.MethodGet, "/formats", nil))
	var caps struct {
		Inputs struct {
			Image    []string `json:"image"`
			Document []string `json:"document"`
			PDF      []string `json:"pdf"`
		} `json:"inputs"`
		Outputs   []string `json:"outputs"`
		Gotenberg bool     `json:"gotenberg"`
	}
	if err := json.Unmarshal(formats.Body.Bytes(), &caps); err != nil {
		t.Fatal(err)
	}
	if !contains(caps.Inputs.Image, "heic") || !contains(caps.Inputs.Document, "docx") {
		t.Fatalf("formats = %+v", caps)
	}
	if !contains(caps.Outputs, "pdf") || !caps.Gotenberg {
		t.Fatalf("formats = %+v", caps)
	}
}

func TestFormatsTypstOnly(t *testing.T) {
	s := newTestServer(&fakeRenderer{}, &fakeConverter{}, Options{MaxBodyBytes: 1 << 20})

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/formats", nil))
	var caps struct {
		Inputs struct {
			Image    []string `json:"image"`
			Document []string `json:"document"`
			PDF      []string `json:"pdf"`
		} `json:"inputs"`
		Outputs   []string `json:"outputs"`
		Vips      bool     `json:"vips"`
		Gotenberg bool     `json:"gotenberg"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &caps); err != nil {
		t.Fatal(err)
	}
	if caps.Vips || caps.Gotenberg {
		t.Fatalf("backends = %+v", caps)
	}
	if caps.Inputs.Image != nil || caps.Inputs.Document != nil {
		t.Fatalf("inputs = %+v", caps.Inputs)
	}
	if len(caps.Outputs) != 1 || caps.Outputs[0] != "pdf" {
		t.Fatalf("outputs = %v", caps.Outputs)
	}
	if len(caps.Inputs.PDF) != 1 || caps.Inputs.PDF[0] != "pdf" {
		t.Fatalf("pdf inputs = %v", caps.Inputs.PDF)
	}
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
