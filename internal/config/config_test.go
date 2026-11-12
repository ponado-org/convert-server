package config

import (
	"runtime"
	"testing"
)

func TestFromEnvDefaults(t *testing.T) {
	for _, k := range []string{
		"PORT", "CONVERT_TYPST_BIN", "CONVERT_VIPS_BIN", "CONVERT_GOTENBERG_URL",
		"CONVERT_MAX_CONCURRENCY", "CONVERT_MAX_BODY_MB",
	} {
		t.Setenv(k, "")
	}

	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != defaultPort {
		t.Errorf("Port = %q, want %q", cfg.Port, defaultPort)
	}
	if cfg.TypstBin != "typst" {
		t.Errorf("TypstBin = %q, want typst", cfg.TypstBin)
	}
	if cfg.VipsBin != "vips" {
		t.Errorf("VipsBin = %q, want vips", cfg.VipsBin)
	}
	if cfg.GotenbergURL != "" {
		t.Errorf("GotenbergURL = %q, want empty", cfg.GotenbergURL)
	}
	if cfg.MaxConcurrency != runtime.NumCPU() {
		t.Errorf("MaxConcurrency = %d, want %d", cfg.MaxConcurrency, runtime.NumCPU())
	}
	if cfg.MaxBodyBytes != defaultBodyMB<<20 {
		t.Errorf("MaxBodyBytes = %d, want %d", cfg.MaxBodyBytes, int64(defaultBodyMB)<<20)
	}
}

func TestFromEnvValues(t *testing.T) {
	t.Setenv("PORT", "9999")
	t.Setenv("CONVERT_TYPST_BIN", "/opt/typst")
	t.Setenv("CONVERT_VIPS_BIN", "/opt/vips")
	t.Setenv("CONVERT_GOTENBERG_URL", "http://gotenberg:3000")
	t.Setenv("CONVERT_MAX_CONCURRENCY", "3")
	t.Setenv("CONVERT_MAX_BODY_MB", "10")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != "9999" || cfg.TypstBin != "/opt/typst" || cfg.VipsBin != "/opt/vips" {
		t.Errorf("cfg = %+v", cfg)
	}
	if cfg.GotenbergURL != "http://gotenberg:3000" || cfg.MaxConcurrency != 3 {
		t.Errorf("cfg = %+v", cfg)
	}
	if cfg.MaxBodyBytes != 10<<20 {
		t.Errorf("MaxBodyBytes = %d, want %d", cfg.MaxBodyBytes, int64(10)<<20)
	}
}

func TestFromEnvInvalid(t *testing.T) {
	t.Setenv("CONVERT_MAX_CONCURRENCY", "many")
	if _, err := FromEnv(); err == nil {
		t.Error("invalid CONVERT_MAX_CONCURRENCY accepted")
	}

	t.Setenv("CONVERT_MAX_CONCURRENCY", "1")
	t.Setenv("CONVERT_MAX_BODY_MB", "0")
	if _, err := FromEnv(); err == nil {
		t.Error("CONVERT_MAX_BODY_MB=0 accepted")
	}
}
