package config

import "testing"

func TestDeepMerge_ScalarOverride(t *testing.T) {
	base := mustParseYAML(t, "llm:\n  gateway_url: \"http://base:4000\"\n  timeout: 30\n")
	overlay := mustParseYAML(t, "llm:\n  gateway_url: \"http://overlay:5000\"\n")

	merged, err := deepMerge(base, overlay)
	if err != nil {
		t.Fatalf("deepMerge error: %v", err)
	}

	cfg := mustUnmarshalNode[struct {
		LLM struct {
			GatewayURL string `yaml:"gateway_url"`
			Timeout    int    `yaml:"timeout"`
		} `yaml:"llm"`
	}](t, merged)

	if cfg.LLM.GatewayURL != "http://overlay:5000" {
		t.Errorf("gateway_url = %q, want %q", cfg.LLM.GatewayURL, "http://overlay:5000")
	}
	if cfg.LLM.Timeout != 30 {
		t.Errorf("timeout = %d, want %d (should be preserved from base)", cfg.LLM.Timeout, 30)
	}
}

func TestDeepMerge_NewKeyAdded(t *testing.T) {
	base := mustParseYAML(t, "llm:\n  gateway_url: \"http://base:4000\"\n")
	overlay := mustParseYAML(t, "storage:\n  objects:\n    backend: s3\n")

	merged, err := deepMerge(base, overlay)
	if err != nil {
		t.Fatalf("deepMerge error: %v", err)
	}

	cfg := mustUnmarshalNode[struct {
		LLM struct {
			GatewayURL string `yaml:"gateway_url"`
		} `yaml:"llm"`
		Storage struct {
			Objects struct {
				Backend string `yaml:"backend"`
			} `yaml:"objects"`
		} `yaml:"storage"`
	}](t, merged)

	if cfg.LLM.GatewayURL != "http://base:4000" {
		t.Errorf("gateway_url = %q, want preserved from base", cfg.LLM.GatewayURL)
	}
	if cfg.Storage.Objects.Backend != "s3" {
		t.Errorf("backend = %q, want %q", cfg.Storage.Objects.Backend, "s3")
	}
}

func TestDeepMerge_SliceReplace(t *testing.T) {
	base := mustParseYAML(t, "database:\n  extensions:\n    - age\n    - vector\n")
	overlay := mustParseYAML(t, "database:\n  extensions:\n    - pgcrypto\n")

	merged, err := deepMerge(base, overlay)
	if err != nil {
		t.Fatalf("deepMerge error: %v", err)
	}

	cfg := mustUnmarshalNode[struct {
		Database struct {
			Extensions []string `yaml:"extensions"`
		} `yaml:"database"`
	}](t, merged)

	if len(cfg.Database.Extensions) != 1 || cfg.Database.Extensions[0] != "pgcrypto" {
		t.Errorf("extensions = %v, want [pgcrypto] (overlay replaces)", cfg.Database.Extensions)
	}
}

func TestDeepMerge_NestedMaps(t *testing.T) {
	base := mustParseYAML(t, "tls:\n  mode: server-only\n  ca: /etc/ca.crt\n  targets:\n    ingestion:\n      mode: mutual\n")
	overlay := mustParseYAML(t, "tls:\n  targets:\n    ingestion:\n      cert: /etc/ingestion.crt\n    catalog:\n      mode: mutual\n")

	merged, err := deepMerge(base, overlay)
	if err != nil {
		t.Fatalf("deepMerge error: %v", err)
	}

	cfg := mustUnmarshalNode[struct {
		TLS struct {
			Mode    string                 `yaml:"mode"`
			CA      string                 `yaml:"ca"`
			Targets map[string]TLSOverride `yaml:"targets"`
		} `yaml:"tls"`
	}](t, merged)

	if cfg.TLS.Mode != "server-only" {
		t.Errorf("tls.mode = %q, want %q", cfg.TLS.Mode, "server-only")
	}
	if cfg.TLS.CA != "/etc/ca.crt" {
		t.Errorf("tls.ca = %q, want preserved", cfg.TLS.CA)
	}
	if cfg.TLS.Targets["ingestion"].Mode != "mutual" {
		t.Errorf("targets.ingestion.mode = %q, want %q", cfg.TLS.Targets["ingestion"].Mode, "mutual")
	}
	if cfg.TLS.Targets["ingestion"].Cert != "/etc/ingestion.crt" {
		t.Errorf("targets.ingestion.cert = %q, want merged from overlay", cfg.TLS.Targets["ingestion"].Cert)
	}
	if cfg.TLS.Targets["catalog"].Mode != "mutual" {
		t.Errorf("targets.catalog.mode = %q, want added from overlay", cfg.TLS.Targets["catalog"].Mode)
	}
}

func TestDeepMerge_NilBase(t *testing.T) {
	overlay := mustParseYAML(t, "llm:\n  gateway_url: \"http://new:4000\"\n")

	merged, err := deepMerge(nil, overlay)
	if err != nil {
		t.Fatalf("deepMerge error: %v", err)
	}

	cfg := mustUnmarshalNode[struct {
		LLM struct {
			GatewayURL string `yaml:"gateway_url"`
		} `yaml:"llm"`
	}](t, merged)

	if cfg.LLM.GatewayURL != "http://new:4000" {
		t.Errorf("gateway_url = %q, want %q", cfg.LLM.GatewayURL, "http://new:4000")
	}
}

func TestDeepMerge_NilOverlay(t *testing.T) {
	base := mustParseYAML(t, "llm:\n  gateway_url: \"http://base:4000\"\n")

	merged, err := deepMerge(base, nil)
	if err != nil {
		t.Fatalf("deepMerge error: %v", err)
	}

	cfg := mustUnmarshalNode[struct {
		LLM struct {
			GatewayURL string `yaml:"gateway_url"`
		} `yaml:"llm"`
	}](t, merged)

	if cfg.LLM.GatewayURL != "http://base:4000" {
		t.Errorf("gateway_url = %q, want preserved", cfg.LLM.GatewayURL)
	}
}

func TestDeepMerge_BothNil(t *testing.T) {
	merged, err := deepMerge(nil, nil)
	if err != nil {
		t.Fatalf("deepMerge error: %v", err)
	}
	if merged != nil {
		t.Error("expected nil when both inputs are nil")
	}
}
