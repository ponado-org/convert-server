package server

import (
	"bytes"
	"encoding/base64"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func jsonRequest(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/typst/render", strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	return req
}

func multipartRequest(t *testing.T, build func(*multipart.Writer)) *http.Request {
	t.Helper()
	req, _ := buildMultipart(t, "/typst/render", build)
	return req
}

func jsonConvertRequest(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/convert", strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	return req
}

func multipartConvertRequest(t *testing.T, build func(*multipart.Writer)) *http.Request {
	t.Helper()
	req, _ := buildMultipart(t, "/convert", build)
	return req
}

func buildMultipart(t *testing.T, path string, build func(*multipart.Writer)) (*http.Request, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	build(mw)
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("content-type", mw.FormDataContentType())
	return req, &buf
}

func TestDecodeJSONReq(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte("{}"))
	s := newTestServer(&fakeRenderer{}, &fakeConverter{}, Options{MaxBodyBytes: 1 << 20})

	req, err := s.decodeRender(httptest.NewRecorder(), jsonRequest(
		`{"template":"= hi","files":{"/data.json":"`+b64+`"},"inputs":{"a":"b"}}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	if string(req.Files["/data.json"]) != "{}" {
		t.Errorf("files[/data.json] = %q, want {}", req.Files["/data.json"])
	}
	if req.Inputs["a"] != "b" {
		t.Errorf("inputs[a] = %q, want b", req.Inputs["a"])
	}
}

func TestDecodeJSONReqRejectsBadBase64(t *testing.T) {
	s := newTestServer(&fakeRenderer{}, &fakeConverter{}, Options{MaxBodyBytes: 1 << 20})
	_, err := s.decodeRender(httptest.NewRecorder(), jsonRequest(
		`{"template":"= hi","files":{"/data.json":"!!!not-base64"}}`,
	))
	if err == nil || !strings.Contains(err.Error(), "base64") {
		t.Fatalf("decodeRender() error = %v, want base64 rejection", err)
	}
}

func TestDecodeMultipartReq(t *testing.T) {
	s := newTestServer(&fakeRenderer{}, &fakeConverter{}, Options{MaxBodyBytes: 1 << 20})

	req := multipartRequest(t, func(mw *multipart.Writer) {
		_ = mw.WriteField("template", "= hi")
		_ = mw.WriteField("mainFilePath", "/main.typ")
		_ = mw.WriteField("timeoutMs", "5000")
		_ = mw.WriteField("inputs", `{"version":"1.2.0"}`)
		_ = mw.WriteField("fontPaths", `["/fonts"]`)

		w, _ := mw.CreateFormFile("files", "data.json")
		_, _ = w.Write([]byte(`{"n":1}`))
		w, _ = mw.CreateFormFile("files", "logo.png")
		_, _ = w.Write([]byte("PNG"))
		// filename with traversal segments must flatten to the base name
		w, _ = mw.CreateFormFile("files", "../../evil.txt")
		_, _ = w.Write([]byte("x"))
	})

	decoded, err := s.decodeRender(httptest.NewRecorder(), req)
	if err != nil {
		t.Fatal(err)
	}

	if decoded.Template != "= hi" {
		t.Errorf("template = %q", decoded.Template)
	}
	if decoded.MainFilePath != "/main.typ" {
		t.Errorf("mainFilePath = %q", decoded.MainFilePath)
	}
	if decoded.TimeoutMs == nil || *decoded.TimeoutMs != 5000 {
		t.Errorf("timeoutMs = %v, want 5000", decoded.TimeoutMs)
	}
	if decoded.Inputs["version"] != "1.2.0" {
		t.Errorf("inputs[version] = %q", decoded.Inputs["version"])
	}
	if len(decoded.FontPaths) != 1 || decoded.FontPaths[0] != "/fonts" {
		t.Errorf("fontPaths = %v", decoded.FontPaths)
	}
	if string(decoded.Files["/data.json"]) != `{"n":1}` {
		t.Errorf("files[/data.json] = %q", decoded.Files["/data.json"])
	}
	if string(decoded.Files["/logo.png"]) != "PNG" {
		t.Errorf("files[/logo.png] = %q", decoded.Files["/logo.png"])
	}
	if string(decoded.Files["/evil.txt"]) != "x" {
		t.Errorf("files[/evil.txt] = %q, want traversal flattened", decoded.Files["/evil.txt"])
	}
	if _, escaped := decoded.Files["/../../evil.txt"]; escaped {
		t.Error("nested traversal path survived decoding")
	}
}

func TestDecodeMultipartRejectsBadInputs(t *testing.T) {
	s := newTestServer(&fakeRenderer{}, &fakeConverter{}, Options{MaxBodyBytes: 1 << 20})

	req := multipartRequest(t, func(mw *multipart.Writer) {
		_ = mw.WriteField("template", "= hi")
		_ = mw.WriteField("inputs", "not-json")
	})
	if _, err := s.decodeRender(httptest.NewRecorder(), req); err == nil {
		t.Fatal("invalid inputs JSON accepted")
	}
}

func TestDecodeConvertJSON(t *testing.T) {
	s := newTestServer(&fakeRenderer{}, &fakeConverter{}, Options{MaxBodyBytes: 1 << 20})
	b64 := base64.StdEncoding.EncodeToString([]byte("DATA"))

	decoded, err := s.decodeConvert(httptest.NewRecorder(), jsonConvertRequest(
		`{"filename":"a.jpeg","data":"`+b64+`","to":"jpeg","page":3,"dpi":96,"maxHeight":800,"quality":70}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Filename != "a.jpeg" || string(decoded.Data) != "DATA" {
		t.Errorf("decoded = %+v", decoded)
	}
	if decoded.To != "jpg" {
		t.Errorf("to = %q, want jpg", decoded.To)
	}
	if decoded.Page != 3 || decoded.DPI != 96 || decoded.MaxHeight != 800 || decoded.Quality != 70 {
		t.Errorf("decoded = %+v", decoded)
	}
}

func TestDecodeConvertJSONRejectsBadBase64(t *testing.T) {
	s := newTestServer(&fakeRenderer{}, &fakeConverter{}, Options{MaxBodyBytes: 1 << 20})
	_, err := s.decodeConvert(httptest.NewRecorder(), jsonConvertRequest(
		`{"filename":"a.png","data":"!!!","to":"png"}`,
	))
	if err == nil || !strings.Contains(err.Error(), "base64") {
		t.Fatalf("decodeConvert() error = %v, want base64 rejection", err)
	}
}

func TestDecodeConvertMultipartAcceptsFilesField(t *testing.T) {
	s := newTestServer(&fakeRenderer{}, &fakeConverter{}, Options{MaxBodyBytes: 1 << 20})

	req := multipartConvertRequest(t, func(mw *multipart.Writer) {
		_ = mw.WriteField("to", "webp")
		w, _ := mw.CreateFormFile("files", "../../doc.docx")
		_, _ = w.Write([]byte("DOCX"))
	})

	decoded, err := s.decodeConvert(httptest.NewRecorder(), req)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Filename != "doc.docx" || string(decoded.Data) != "DOCX" || decoded.To != "webp" {
		t.Errorf("decoded = %+v", decoded)
	}
}

func TestSanitizeFilename(t *testing.T) {
	cases := map[string]string{
		"photo.heic":              "photo.heic",
		"../../evil.txt":          "evil.txt",
		`C:\Users\me\report.docx`: "report.docx",
		"bad\x00name.txt":         "badname.txt",
		"   ":                     "",
		"..":                      "",
	}
	for in, want := range cases {
		if got := sanitizeFilename(in); got != want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDecodeConvertRejectsBadNumber(t *testing.T) {
	s := newTestServer(&fakeRenderer{}, &fakeConverter{}, Options{MaxBodyBytes: 1 << 20})

	req := multipartConvertRequest(t, func(mw *multipart.Writer) {
		_ = mw.WriteField("dpi", "high")
		w, _ := mw.CreateFormFile("file", "a.png")
		_, _ = w.Write([]byte("PNG"))
	})
	if _, err := s.decodeConvert(httptest.NewRecorder(), req); err == nil {
		t.Fatal("invalid dpi accepted")
	}
}
