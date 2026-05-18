package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestApplyEnvVars_ScalarOverride(t *testing.T) {
	t.Setenv("CROSSCODEX_LLM_GATEWAY_URL", "https://env:9000")
	t.Setenv("CROSSCODEX_LLM_TIMEOUT", "60")

	base := mustParseYAML(t, "llm:\n  gateway_url: \"https://file:4000\"\n  timeout: 30\n")

	result, err := applyEnvVars(base, "CROSSCODEX")
	if err != nil {
		t.Fatalf("applyEnvVars error: %v", err)
	}

	cfg := mustUnmarshalNode[struct {
		LLM struct {
			GatewayURL string `yaml:"gateway_url"`
			Timeout    int    `yaml:"timeout"`
		} `yaml:"llm"`
	}](t, result)

	if cfg.LLM.GatewayURL != "https://env:9000" {
		t.Errorf("gateway_url = %q, want env override", cfg.LLM.GatewayURL)
	}
	if cfg.LLM.Timeout != 60 {
		t.Errorf("timeout = %d, want 60 from env", cfg.LLM.Timeout)
	}
}

func TestApplyEnvVars_NestedPath(t *testing.T) {
	t.Setenv("CROSSCODEX_STORAGE_OBJECTS_BACKEND", "s3")
	t.Setenv("CROSSCODEX_STORAGE_OBJECTS_BUCKET", "my-bucket")

	base := mustParseYAML(t, "storage:\n  objects:\n    backend: local\n")

	result, err := applyEnvVars(base, "CROSSCODEX")
	if err != nil {
		t.Fatalf("applyEnvVars error: %v", err)
	}

	cfg := mustUnmarshalNode[struct {
		Storage struct {
			Objects struct {
				Backend string `yaml:"backend"`
				Bucket  string `yaml:"bucket"`
			} `yaml:"objects"`
		} `yaml:"storage"`
	}](t, result)

	if cfg.Storage.Objects.Backend != "s3" {
		t.Errorf("backend = %q, want s3", cfg.Storage.Objects.Backend)
	}
	if cfg.Storage.Objects.Bucket != "my-bucket" {
		t.Errorf("bucket = %q, want my-bucket", cfg.Storage.Objects.Bucket)
	}
}

func TestApplyEnvVars_NoMatchingVars(t *testing.T) {
	base := mustParseYAML(t, "llm:\n  gateway_url: \"https://file:4000\"\n")

	result, err := applyEnvVars(base, "CROSSCODEX")
	if err != nil {
		t.Fatalf("applyEnvVars error: %v", err)
	}

	cfg := mustUnmarshalNode[struct {
		LLM struct {
			GatewayURL string `yaml:"gateway_url"`
		} `yaml:"llm"`
	}](t, result)

	if cfg.LLM.GatewayURL != "https://file:4000" {
		t.Errorf("gateway_url = %q, want preserved from file", cfg.LLM.GatewayURL)
	}
}

func TestApplyEnvVars_BoolValue(t *testing.T) {
	t.Setenv("CROSSCODEX_TLS_FIPS_ENABLED", "true")

	base := mustParseYAML(t, "tls:\n  fips:\n    enabled: false\n")

	result, err := applyEnvVars(base, "CROSSCODEX")
	if err != nil {
		t.Fatalf("applyEnvVars error: %v", err)
	}

	cfg := mustUnmarshalNode[struct {
		TLS struct {
			FIPS struct {
				Enabled bool `yaml:"enabled"`
			} `yaml:"fips"`
		} `yaml:"tls"`
	}](t, result)

	if !cfg.TLS.FIPS.Enabled {
		t.Error("fips.enabled = false, want true from env")
	}
}

func TestInferTag_NegativeInteger(t *testing.T) {
	schema := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: "0"}

	if got := inferTag(schema, "-5"); got != "!!int" {
		t.Errorf("inferTag(!!int, \"-5\") = %q, want !!int", got)
	}
	if got := inferTag(schema, "+10"); got != "!!int" {
		t.Errorf("inferTag(!!int, \"+10\") = %q, want !!int", got)
	}
	if got := inferTag(schema, ""); got != "!!str" {
		t.Errorf("inferTag(!!int, \"\") = %q, want !!str", got)
	}
	if got := inferTag(schema, "-"); got != "!!str" {
		t.Errorf("inferTag(!!int, \"-\") = %q, want !!str", got)
	}
	if got := inferTag(schema, "abc"); got != "!!str" {
		t.Errorf("inferTag(!!int, \"abc\") = %q, want !!str", got)
	}
}

func TestApplyEnvVars_NonNumericForIntField(t *testing.T) {
	t.Setenv("CROSSCODEX_LLM_TIMEOUT", "not-a-number")

	base := mustParseYAML(t, "llm:\n  timeout: 30\n")

	result, err := applyEnvVars(base, "CROSSCODEX")
	if err != nil {
		t.Fatalf("applyEnvVars error: %v", err)
	}

	cfg := mustUnmarshalNode[struct {
		LLM struct {
			Timeout interface{} `yaml:"timeout"`
		} `yaml:"llm"`
	}](t, result)

	if cfg.LLM.Timeout == 30 {
		t.Error("timeout should have been overridden by env var, not kept as 30")
	}
}

func TestApplyEnvVars_NilBase(t *testing.T) {
	t.Setenv("CROSSCODEX_LLM_GATEWAY_URL", "https://env:9000")

	result, err := applyEnvVars(nil, "CROSSCODEX")
	if err != nil {
		t.Fatalf("applyEnvVars error: %v", err)
	}

	cfg := mustUnmarshalNode[struct {
		LLM struct {
			GatewayURL string `yaml:"gateway_url"`
		} `yaml:"llm"`
	}](t, result)

	if cfg.LLM.GatewayURL != "https://env:9000" {
		t.Errorf("gateway_url = %q, want env value", cfg.LLM.GatewayURL)
	}
}
