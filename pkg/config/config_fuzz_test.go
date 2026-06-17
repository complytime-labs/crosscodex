package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

func FuzzInferTag(f *testing.F) {
	f.Add("true")
	f.Add("false")
	f.Add("42")
	f.Add("3.14")
	f.Add("")
	f.Add("null")
	f.Add("yes")
	f.Add("no")
	f.Add("-1")
	f.Add("+0")

	f.Fuzz(func(t *testing.T, value string) {
		// Must not panic with nil schema node.
		tag := config.ExportInferTag(nil, value)
		if tag == "" {
			t.Errorf("inferTag returned empty string for value %q with nil schema", value)
		}
	})
}

func FuzzBuildNodeFromPath(f *testing.F) {
	f.Add("tls_mode", "mutual")
	f.Add("storage_objects_backend", "s3")
	f.Add("llm_timeout", "30")
	f.Add("", "")
	f.Add("a_b_c_d_e", "value")
	f.Add("logging_level", "debug")

	f.Fuzz(func(t *testing.T, path, value string) {
		// Split on underscore to create segments, matching the env var convention.
		var segments []string
		if path != "" {
			segments = strings.Split(path, "_")
		}
		// Must not panic regardless of input.
		node := config.ExportBuildNodeFromPath(segments, value)
		if node == nil && len(segments) > 0 {
			t.Errorf("buildNodeFromPath returned nil for non-empty segments %v", segments)
		}
	})
}

func FuzzValidateConfig(f *testing.F) {
	f.Add("off", "local", "8760h")
	f.Add("mutual", "s3", "1h")
	f.Add("server-only", "local", "24h")
	f.Add("INVALID", "INVALID", "0s")
	f.Add("", "", "8760h")
	f.Add("mutual\x00", "s3\x00", "-1h")

	f.Fuzz(func(t *testing.T, tlsMode, storageBackend, expiryStr string) {
		expiry, err := time.ParseDuration(expiryStr)
		if err != nil {
			// Use a valid default when parse fails — we're testing validation, not duration parsing.
			expiry = 8760 * time.Hour
		}

		cfg := &config.Config{}
		cfg.TLS.Mode = tlsMode
		cfg.Storage.Objects.Backend = storageBackend
		cfg.Attestation.ExpiryDuration = expiry

		// Must not panic regardless of input.
		_ = config.ExportValidateConfig(cfg)
	})
}
