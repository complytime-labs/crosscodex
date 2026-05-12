package config

import (
	"path/filepath"
	"testing"
)

func TestLoadDropIns_LexicographicOrder(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "10-base.yaml"), "llm:\n  gateway_url: \"http://base:4000\"\n  timeout: 30\n")
	writeFile(t, filepath.Join(dir, "20-override.yaml"), "llm:\n  gateway_url: \"http://override:5000\"\n")

	result, err := loadDropIns(dir)
	if err != nil {
		t.Fatalf("loadDropIns error: %v", err)
	}

	cfg := mustUnmarshalNode[struct {
		LLM struct {
			GatewayURL string `yaml:"gateway_url"`
			Timeout    int    `yaml:"timeout"`
		} `yaml:"llm"`
	}](t, result)

	if cfg.LLM.GatewayURL != "http://override:5000" {
		t.Errorf("gateway_url = %q, want override value", cfg.LLM.GatewayURL)
	}
	if cfg.LLM.Timeout != 30 {
		t.Errorf("timeout = %d, want base value preserved", cfg.LLM.Timeout)
	}
}

func TestLoadDropIns_SkipsNonYAML(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "10-base.yaml"), "llm:\n  gateway_url: \"http://base:4000\"\n")
	writeFile(t, filepath.Join(dir, "README.md"), "This is not config")
	writeFile(t, filepath.Join(dir, "backup.yaml.bak"), "llm:\n  gateway_url: \"http://should-be-ignored:4000\"\n")

	result, err := loadDropIns(dir)
	if err != nil {
		t.Fatalf("loadDropIns error: %v", err)
	}

	cfg := mustUnmarshalNode[struct {
		LLM struct {
			GatewayURL string `yaml:"gateway_url"`
		} `yaml:"llm"`
	}](t, result)

	if cfg.LLM.GatewayURL != "http://base:4000" {
		t.Errorf("gateway_url = %q, want only .yaml files loaded", cfg.LLM.GatewayURL)
	}
}

func TestLoadDropIns_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	result, err := loadDropIns(dir)
	if err != nil {
		t.Fatalf("loadDropIns error: %v", err)
	}
	if result != nil {
		t.Error("expected nil for empty directory")
	}
}

func TestLoadDropIns_MissingDir(t *testing.T) {
	result, err := loadDropIns("/nonexistent/path/conf.d")
	if err != nil {
		t.Fatalf("loadDropIns should not error on missing dir: %v", err)
	}
	if result != nil {
		t.Error("expected nil for missing directory")
	}
}

func TestLoadDropIns_MalformedYAMLFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "10-good.yaml"), "llm:\n  timeout: 30\n")
	writeFile(t, filepath.Join(dir, "20-bad.yaml"), ":\n  - :\n  invalid: [yaml\n")

	_, err := loadDropIns(dir)
	if err == nil {
		t.Fatal("expected error for malformed YAML drop-in")
	}
}
