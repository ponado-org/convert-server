// Package config loads convert-server settings from the environment.
package config

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
)

const (
	defaultPort   = "3100"
	defaultBodyMB = 50
)

// Config is the fully resolved service configuration.
type Config struct {
	// Port is the TCP port to listen on (PORT, default 3100).
	Port string
	// TypstBin is the typst binary name or path (CONVERT_TYPST_BIN,
	// default "typst").
	TypstBin string
	// VipsBin is the libvips CLI name or path (CONVERT_VIPS_BIN,
	// default "vips").
	VipsBin string
	// GotenbergURL enables Office/text conversions when set
	// (CONVERT_GOTENBERG_URL, e.g. http://gotenberg:3000).
	GotenbergURL string
	// GotenbergProxy exposes /gotenberg/* as a reverse proxy to GotenbergURL
	// (CONVERT_GOTENBERG_PROXY, default true). It has no effect when
	// GotenbergURL is empty.
	GotenbergProxy bool
	// MaxConcurrency caps parallel conversions; <= 0 means unlimited
	// (CONVERT_MAX_CONCURRENCY, default NumCPU).
	MaxConcurrency int
	// MaxBodyBytes caps request bodies (CONVERT_MAX_BODY_MB, default 50MB).
	MaxBodyBytes int64
}

// FromEnv reads the configuration, returning an error for malformed values
// instead of silently falling back to defaults.
func FromEnv() (Config, error) {
	cfg := Config{
		Port:           defaultPort,
		TypstBin:       "typst",
		VipsBin:        "vips",
		GotenbergProxy: true,
		MaxConcurrency: runtime.NumCPU(),
		MaxBodyBytes:   defaultBodyMB << 20,
	}

	if v := strings.TrimSpace(os.Getenv("PORT")); v != "" {
		cfg.Port = v
	}
	if v := strings.TrimSpace(os.Getenv("CONVERT_TYPST_BIN")); v != "" {
		cfg.TypstBin = v
	}
	if v := strings.TrimSpace(os.Getenv("CONVERT_VIPS_BIN")); v != "" {
		cfg.VipsBin = v
	}
	if v := strings.TrimSpace(os.Getenv("CONVERT_GOTENBERG_URL")); v != "" {
		cfg.GotenbergURL = v
	}

	if v := strings.TrimSpace(os.Getenv("CONVERT_GOTENBERG_PROXY")); v != "" {
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("invalid CONVERT_GOTENBERG_PROXY %q: %w", v, err)
		}
		cfg.GotenbergProxy = enabled
	}

	if v := strings.TrimSpace(os.Getenv("CONVERT_MAX_CONCURRENCY")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("invalid CONVERT_MAX_CONCURRENCY %q: %w", v, err)
		}
		cfg.MaxConcurrency = n
	}

	if v := strings.TrimSpace(os.Getenv("CONVERT_MAX_BODY_MB")); v != "" {
		mb, err := strconv.Atoi(v)
		if err != nil || mb <= 0 {
			return Config{}, fmt.Errorf("invalid CONVERT_MAX_BODY_MB %q: want a positive integer", v)
		}
		cfg.MaxBodyBytes = int64(mb) << 20
	}

	return cfg, nil
}
