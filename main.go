// Command convert-server is a small self-hosted conversion service: it renders
// Typst templates to PDF and converts uploaded files (images, PDFs, Office
// documents) into browser-viewable output. The wire contract and deployment
// notes live in README.md; the implementation is split into:
//
//	internal/config   environment parsing
//	internal/typst    typst invocation, shadow dirs, timeouts, concurrency
//	internal/convert  vips + Gotenberg pipelines, format detection
//	internal/server   HTTP handlers + JSON/multipart request decoding
package main

import (
	"log"
	"net/http"

	"convert-server/internal/config"
	"convert-server/internal/convert"
	"convert-server/internal/server"
	"convert-server/internal/typst"
)

// version is the convert-server release; set via
// -ldflags "-X main.version=..." at image build time.
var version = "dev"

func main() {
	cfg, err := config.FromEnv()
	if err != nil {
		log.Fatal(err)
	}

	renderer, err := typst.New(cfg.TypstBin, cfg.MaxConcurrency)
	if err != nil {
		log.Fatal(err)
	}
	converter, err := convert.New(convert.Options{
		VipsBin:        cfg.VipsBin,
		GotenbergURL:   cfg.GotenbergURL,
		MaxConcurrency: cfg.MaxConcurrency,
	})
	if err != nil {
		log.Fatal(err)
	}

	if cfg.MaxConcurrency > 0 {
		log.Printf("max concurrent conversions per backend: %d", cfg.MaxConcurrency)
	} else {
		log.Printf("max concurrent conversions per backend: unlimited")
	}
	if cfg.GotenbergURL == "" {
		log.Printf("gotenberg: disabled (set CONVERT_GOTENBERG_URL for Office conversions)")
	} else {
		log.Printf("gotenberg: %s", cfg.GotenbergURL)
	}

	typstVersion := renderer.Version()
	if typstVersion == "" {
		log.Printf("warning: could not determine typst version")
	}
	vipsVersion := ""
	vipsLabel := "disabled"
	if converter.VipsEnabled() {
		vipsVersion = converter.VipsVersion()
		vipsLabel = vipsVersion
		if vipsVersion == "" {
			vipsLabel = "unknown"
			log.Printf("warning: could not determine vips version")
		}
	} else {
		log.Printf("vips: disabled (typst-only image; image conversion unavailable)")
	}

	srv := server.New(renderer, converter, server.Options{
		ServiceVersion: version,
		TypstVersion:   typstVersion,
		VipsVersion:    vipsVersion,
		MaxBodyBytes:   cfg.MaxBodyBytes,
	})
	addr := "0.0.0.0:" + cfg.Port
	log.Printf("%s %s listening on %s (typst: %s, vips: %s)", server.Name, version, addr, typstVersion, vipsLabel)
	if err := http.ListenAndServe(addr, srv.Handler()); err != nil {
		log.Fatal(err)
	}
}
