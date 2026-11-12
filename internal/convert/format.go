package convert

import (
	"archive/zip"
	"bytes"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// Kind groups formats by the pipeline that handles them.
type Kind uint8

const (
	KindUnknown Kind = iota
	KindImage
	KindPDF
	KindDocument
)

// File is a detected input format.
type File struct {
	Format string
	Kind   Kind
}

// DefaultTarget is the browser-safe output used when the caller does not ask
// for one: images become PNG, everything else becomes PDF.
func (f File) DefaultTarget() string {
	if f.Kind == KindImage {
		return "png"
	}
	return "pdf"
}

// Passthrough reports whether the input can be returned as-is for browser
// viewing. SVG is deliberately excluded: serving untrusted SVG inline from
// this origin would allow script execution.
func (f File) Passthrough() bool {
	switch f.Format {
	case "png", "jpg", "gif", "webp":
		return true
	}
	return false
}

// formats maps every accepted extension to its kind. The canonical extension
// is the map key (jpeg -> jpg, tif -> tiff, htm -> html).
var formats = map[string]Kind{
	"pdf": KindPDF,

	"png":  KindImage,
	"jpg":  KindImage,
	"gif":  KindImage,
	"webp": KindImage,
	"tiff": KindImage,
	"heic": KindImage,
	"heif": KindImage,
	"avif": KindImage,
	"jxl":  KindImage,
	"svg":  KindImage,
	"svgz": KindImage,
	"bmp":  KindImage,
	"ico":  KindImage,
	"psd":  KindImage,
	"tga":  KindImage,

	"doc":  KindDocument,
	"docx": KindDocument,
	"docm": KindDocument,
	"dot":  KindDocument,
	"dotx": KindDocument,
	"odt":  KindDocument,
	"ott":  KindDocument,
	"rtf":  KindDocument,
	"txt":  KindDocument,
	"csv":  KindDocument,
	"tsv":  KindDocument,
	"xls":  KindDocument,
	"xlsx": KindDocument,
	"xlsm": KindDocument,
	"ods":  KindDocument,
	"ots":  KindDocument,
	"ppt":  KindDocument,
	"pptx": KindDocument,
	"pptm": KindDocument,
	"odp":  KindDocument,
	"otp":  KindDocument,
	"html": KindDocument,
}

// SupportedInputs returns the accepted formats grouped by kind.
func SupportedInputs() map[string][]string {
	out := map[string][]string{"image": {}, "document": {}, "pdf": {}}
	for format, kind := range formats {
		switch kind {
		case KindImage:
			out["image"] = append(out["image"], format)
		case KindPDF:
			out["pdf"] = append(out["pdf"], format)
		case KindDocument:
			out["document"] = append(out["document"], format)
		}
	}
	for _, list := range out {
		sort.Strings(list)
	}
	return out
}

// NormalizeTarget validates a requested output format. The empty string (or
// "auto") means "pick the browser-safe default".
func NormalizeTarget(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "auto":
		return "", true
	case "pdf":
		return "pdf", true
	case "png":
		return "png", true
	case "jpg", "jpeg":
		return "jpg", true
	case "webp":
		return "webp", true
	default:
		return "", false
	}
}

// ContentType maps an output format to its HTTP media type.
func ContentType(format string) string {
	switch format {
	case "pdf":
		return "application/pdf"
	case "png":
		return "image/png"
	case "jpg":
		return "image/jpeg"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	case "svg", "svgz":
		return "image/svg+xml"
	case "tiff":
		return "image/tiff"
	case "heic", "heif":
		return "image/heic"
	case "avif":
		return "image/avif"
	case "bmp":
		return "image/bmp"
	case "ico":
		return "image/x-icon"
	case "jxl":
		return "image/jxl"
	default:
		return "application/octet-stream"
	}
}

// Detect identifies a file from its extension, falling back to magic bytes
// when the name has no usable extension. Extensions win because Office
// containers are too ambiguous to sniff reliably (ZIP/OLE).
func Detect(filename string, data []byte) File {
	if format := canonicalExt(filename); format != "" {
		if kind, ok := formats[format]; ok {
			return File{Format: format, Kind: kind}
		}
	}
	return sniff(data)
}

func canonicalExt(filename string) string {
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(strings.TrimSpace(filename))), ".")
	switch ext {
	case "jpeg":
		return "jpg"
	case "tif":
		return "tiff"
	case "htm":
		return "html"
	default:
		return ext
	}
}

func sniff(data []byte) File {
	if len(data) == 0 {
		return File{}
	}

	switch {
	case bytes.HasPrefix(data, []byte("%PDF-")):
		return File{"pdf", KindPDF}
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return File{"png", KindImage}
	case len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF:
		return File{"jpg", KindImage}
	case bytes.HasPrefix(data, []byte("GIF87a")), bytes.HasPrefix(data, []byte("GIF89a")):
		return File{"gif", KindImage}
	case len(data) >= 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return File{"webp", KindImage}
	case bytes.HasPrefix(data, []byte("II*\x00")), bytes.HasPrefix(data, []byte("MM\x00*")):
		return File{"tiff", KindImage}
	case bytes.HasPrefix(data, []byte("BM")):
		return File{"bmp", KindImage}
	case bytes.HasPrefix(data, []byte("\x00\x00\x01\x00")):
		return File{"ico", KindImage}
	}

	if len(data) >= 12 && string(data[4:8]) == "ftyp" {
		switch brand := string(data[8:12]); {
		case brand == "avif", brand == "avis":
			return File{"avif", KindImage}
		case strings.HasPrefix(brand, "hei"), strings.HasPrefix(brand, "hev"), brand == "mif1", brand == "msf1":
			return File{"heic", KindImage}
		}
	}

	if file := sniffZip(data); file.Format != "" {
		return file
	}

	head := data
	if len(head) > 4096 {
		head = head[:4096]
	}
	lower := bytes.ToLower(head)
	switch {
	case bytes.HasPrefix(lower, []byte("{\\rtf")):
		return File{"rtf", KindDocument}
	case bytes.Contains(lower, []byte("<!doctype html")), bytes.Contains(lower, []byte("<html")):
		return File{"html", KindDocument}
	case bytes.Contains(lower, []byte("<svg")):
		return File{"svg", KindImage}
	}

	if looksLikeText(head) {
		return File{"txt", KindDocument}
	}
	return File{}
}

// sniffZip identifies OOXML and ODF containers without relying on the file
// name, which matters for uploads that arrive with a generic name.
func sniffZip(data []byte) File {
	if !bytes.HasPrefix(data, []byte("PK\x03\x04")) {
		return File{}
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return File{}
	}
	names := make(map[string]bool, len(zr.File))
	for _, f := range zr.File {
		names[f.Name] = true
	}
	switch {
	case names["word/document.xml"]:
		return File{"docx", KindDocument}
	case names["xl/workbook.xml"]:
		return File{"xlsx", KindDocument}
	case names["ppt/presentation.xml"]:
		return File{"pptx", KindDocument}
	}
	if names["mimetype"] {
		for _, f := range zr.File {
			if f.Name != "mimetype" {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				return File{}
			}
			buf := make([]byte, 128)
			n, _ := rc.Read(buf)
			_ = rc.Close()
			switch strings.TrimSpace(string(buf[:n])) {
			case "application/vnd.oasis.opendocument.text":
				return File{"odt", KindDocument}
			case "application/vnd.oasis.opendocument.spreadsheet":
				return File{"ods", KindDocument}
			case "application/vnd.oasis.opendocument.presentation":
				return File{"odp", KindDocument}
			}
		}
	}
	return File{}
}

func looksLikeText(sample []byte) bool {
	if bytes.IndexByte(sample, 0) >= 0 {
		return false
	}
	return utf8.Valid(sample)
}
