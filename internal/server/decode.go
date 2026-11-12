package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"convert-server/internal/convert"
	"convert-server/internal/typst"
)

// multipartMemory caps the RAM used while parsing multipart bodies; larger
// parts spill to temp files. The request body itself is already capped.
const multipartMemory = 8 << 20

// renderPayload is the /typst/render wire shape. JSON carries files
// base64-encoded; multipart carries them as binary parts. See fileMap.
type renderPayload struct {
	Template     string            `json:"template"`
	MainFilePath string            `json:"mainFilePath"`
	Files        fileMap           `json:"files"`
	FontPaths    []string          `json:"fontPaths"`
	Inputs       map[string]string `json:"inputs"`
	TimeoutMs    *int64            `json:"timeoutMs"`
}

func (p renderPayload) toRequest() typst.Request {
	return typst.Request{
		Template:     p.Template,
		MainFilePath: p.MainFilePath,
		Files:        map[string][]byte(p.Files),
		FontPaths:    p.FontPaths,
		Inputs:       p.Inputs,
		TimeoutMs:    p.TimeoutMs,
	}
}

// fileMap is `{ path: base64 }` on the JSON wire; raw bytes internally.
type fileMap map[string][]byte

func (m *fileMap) UnmarshalJSON(data []byte) error {
	var raw map[string]string
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	out := make(fileMap, len(raw))
	for path, b64 := range raw {
		decoded, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return fmt.Errorf("file %s is not valid base64", path)
		}
		out[path] = decoded
	}
	*m = out
	return nil
}

// decodeRender dispatches on Content-Type. Both encodings map onto the same
// typst.Request; clients pick the one that fits their payload.
func (s *Server) decodeRender(w http.ResponseWriter, r *http.Request) (typst.Request, error) {
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes)

	mediaType, _, err := mime.ParseMediaType(r.Header.Get("content-type"))
	if err == nil && mediaType == "multipart/form-data" {
		return decodeRenderMultipart(r)
	}

	var p renderPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		return typst.Request{}, err
	}
	return p.toRequest(), nil
}

// decodeRenderMultipart maps multipart/form-data onto a typst.Request:
//
//	text fields: template, mainFilePath, timeoutMs,
//	             inputs    = JSON object string (optional),
//	             fontPaths = JSON array string (optional)
//	file parts:  land flat in the shadow root by filename, e.g.
//	             `curl -F 'files=@data.json'` -> /data.json
func decodeRenderMultipart(r *http.Request) (typst.Request, error) {
	if err := r.ParseMultipartForm(multipartMemory); err != nil {
		return typst.Request{}, err
	}
	defer r.MultipartForm.RemoveAll()

	var p renderPayload
	p.Template = r.FormValue("template")
	p.MainFilePath = r.FormValue("mainFilePath")

	if raw := strings.TrimSpace(r.FormValue("timeoutMs")); raw != "" {
		ms, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return typst.Request{}, fmt.Errorf("timeoutMs is not a number: %q", raw)
		}
		p.TimeoutMs = &ms
	}
	if raw := strings.TrimSpace(r.FormValue("inputs")); raw != "" {
		if err := json.Unmarshal([]byte(raw), &p.Inputs); err != nil {
			return typst.Request{}, fmt.Errorf("inputs is not a JSON object: %w", err)
		}
	}
	if raw := strings.TrimSpace(r.FormValue("fontPaths")); raw != "" {
		if err := json.Unmarshal([]byte(raw), &p.FontPaths); err != nil {
			return typst.Request{}, fmt.Errorf("fontPaths is not a JSON array: %w", err)
		}
	}

	p.Files = fileMap{}
	for field, headers := range r.MultipartForm.File {
		for _, fh := range headers {
			name := sanitizeFilename(fh.Filename)
			if name == "" {
				return typst.Request{}, fmt.Errorf("file part %q has an invalid filename %q", field, fh.Filename)
			}
			path := "/" + name
			f, err := fh.Open()
			if err != nil {
				return typst.Request{}, fmt.Errorf("read %s: %w", path, err)
			}
			data, err := io.ReadAll(f)
			_ = f.Close()
			if err != nil {
				return typst.Request{}, fmt.Errorf("read %s: %w", path, err)
			}
			p.Files[path] = data
		}
	}
	if len(p.Files) == 0 {
		p.Files = nil
	}
	return p.toRequest(), nil
}

// convertJSON is the JSON wire shape of /convert.
type convertJSON struct {
	Filename  string `json:"filename"`
	Data      string `json:"data"` // base64
	To        string `json:"to"`
	Page      int    `json:"page"`
	DPI       int    `json:"dpi"`
	MaxWidth  int    `json:"maxWidth"`
	MaxHeight int    `json:"maxHeight"`
	Quality   int    `json:"quality"`
	TimeoutMs *int64 `json:"timeoutMs"`
}

func (p convertJSON) toRequest() (convert.Request, error) {
	if strings.TrimSpace(p.Data) == "" {
		return convert.Request{}, fmt.Errorf("data is required (base64)")
	}
	data, err := base64.StdEncoding.DecodeString(p.Data)
	if err != nil {
		return convert.Request{}, fmt.Errorf("data is not valid base64")
	}
	to, ok := convert.NormalizeTarget(p.To)
	if !ok {
		return convert.Request{}, fmt.Errorf("to must be one of pdf, png, jpg or webp")
	}
	return convert.Request{
		Filename:  p.Filename,
		Data:      data,
		To:        to,
		Page:      p.Page,
		DPI:       p.DPI,
		MaxWidth:  p.MaxWidth,
		MaxHeight: p.MaxHeight,
		Quality:   p.Quality,
		TimeoutMs: p.TimeoutMs,
	}, nil
}

// decodeConvert accepts multipart/form-data with a `file` (or `files`) part,
// or JSON with base64 `data`. Multipart is the intended path for uploads.
func (s *Server) decodeConvert(w http.ResponseWriter, r *http.Request) (convert.Request, error) {
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes)

	mediaType, _, err := mime.ParseMediaType(r.Header.Get("content-type"))
	if err == nil && mediaType == "multipart/form-data" {
		return decodeConvertMultipart(r)
	}

	var p convertJSON
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		return convert.Request{}, err
	}
	return p.toRequest()
}

func decodeConvertMultipart(r *http.Request) (convert.Request, error) {
	if err := r.ParseMultipartForm(multipartMemory); err != nil {
		return convert.Request{}, err
	}
	defer r.MultipartForm.RemoveAll()

	var file io.ReadCloser
	var headerFilename string
	for _, field := range []string{"file", "files"} {
		f, fh, err := r.FormFile(field)
		if err == nil {
			file, headerFilename = f, fh.Filename
			break
		}
		if err != http.ErrMissingFile {
			return convert.Request{}, fmt.Errorf("read file part %q: %w", field, err)
		}
	}
	if file == nil {
		return convert.Request{}, fmt.Errorf("file part is required")
	}
	data, err := io.ReadAll(file)
	_ = file.Close()
	if err != nil {
		return convert.Request{}, fmt.Errorf("read upload: %w", err)
	}

	name := sanitizeFilename(headerFilename)
	if name == "" {
		name = "upload"
	}

	to, ok := convert.NormalizeTarget(r.FormValue("to"))
	if !ok {
		return convert.Request{}, fmt.Errorf("to must be one of pdf, png, jpg or webp")
	}

	page, err := intField(r, "page")
	if err != nil {
		return convert.Request{}, err
	}
	dpi, err := intField(r, "dpi")
	if err != nil {
		return convert.Request{}, err
	}
	maxWidth, err := intField(r, "maxWidth")
	if err != nil {
		return convert.Request{}, err
	}
	maxHeight, err := intField(r, "maxHeight")
	if err != nil {
		return convert.Request{}, err
	}
	quality, err := intField(r, "quality")
	if err != nil {
		return convert.Request{}, err
	}

	var timeout *int64
	if raw := strings.TrimSpace(r.FormValue("timeoutMs")); raw != "" {
		ms, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return convert.Request{}, fmt.Errorf("timeoutMs is not a number: %q", raw)
		}
		timeout = &ms
	}

	return convert.Request{
		Filename:  name,
		Data:      data,
		To:        to,
		Page:      page,
		DPI:       dpi,
		MaxWidth:  maxWidth,
		MaxHeight: maxHeight,
		Quality:   quality,
		TimeoutMs: timeout,
	}, nil
}

func intField(r *http.Request, name string) (int, error) {
	raw := strings.TrimSpace(r.FormValue(name))
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s is not a number: %q", name, raw)
	}
	return n, nil
}

// sanitizeFilename flattens client paths (including Windows separators) and
// removes control characters before the name is used on disk or forwarded to
// Gotenberg.
func sanitizeFilename(name string) string {
	name = strings.ReplaceAll(strings.TrimSpace(name), "\\", "/")
	name = filepath.Base(name)
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	switch name {
	case "", ".", "..":
		return ""
	}
	return name
}
