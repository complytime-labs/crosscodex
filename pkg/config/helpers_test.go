package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func mustParseYAML(t *testing.T, input string) *yaml.Node {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(input), &doc); err != nil {
		t.Fatalf("failed to parse YAML: %v", err)
	}
	return doc.Content[0]
}

func mustUnmarshalNode[T any](t *testing.T, node *yaml.Node) T {
	t.Helper()
	out, err := yaml.Marshal(node)
	if err != nil {
		t.Fatalf("failed to marshal node: %v", err)
	}
	var v T
	if err := yaml.Unmarshal(out, &v); err != nil {
		t.Fatalf("failed to unmarshal node: %v", err)
	}
	return v
}

// loadDefaults sets XDG_CONFIG_HOME to a temp dir, creates a Loader,
// and returns the config produced by loading defaults only.
func loadDefaults(t *testing.T) *Config {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	loader := NewLoader()
	cfg, err := loader.Load(context.Background())
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	return cfg
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir error: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write error: %v", err)
	}
}
