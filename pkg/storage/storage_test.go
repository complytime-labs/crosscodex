package storage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

func TestNewFromConfig(t *testing.T) {
	t.Run("local backend with XDG default", func(t *testing.T) {
		xdgDir := t.TempDir()
		t.Setenv("XDG_DATA_HOME", xdgDir)

		cfg := config.ObjectStorageConfig{
			Backend: "local",
		}
		p, err := NewFromConfig(cfg, "test-tenant")
		if err != nil {
			t.Fatalf("NewFromConfig() error = %v", err)
		}
		defer func() { _ = p.Close() }()

		expectedDir := filepath.Join(xdgDir, "crosscodex", "objects", "test-tenant")
		info, err := os.Stat(expectedDir)
		if err != nil {
			t.Fatalf("expected tenant dir %s to exist: %v", expectedDir, err)
		}
		if !info.IsDir() {
			t.Errorf("expected %s to be a directory", expectedDir)
		}
	})

	t.Run("local backend with HOME fallback", func(t *testing.T) {
		homeDir := t.TempDir()
		t.Setenv("HOME", homeDir)
		t.Setenv("XDG_DATA_HOME", "")

		cfg := config.ObjectStorageConfig{
			Backend: "local",
		}
		p, err := NewFromConfig(cfg, "test-tenant")
		if err != nil {
			t.Fatalf("NewFromConfig() error = %v", err)
		}
		defer func() { _ = p.Close() }()

		expectedDir := filepath.Join(homeDir, ".local", "share", "crosscodex", "objects", "test-tenant")
		if _, err := os.Stat(expectedDir); err != nil {
			t.Fatalf("expected tenant dir at XDG default %s: %v", expectedDir, err)
		}
	})

	t.Run("local backend explicit path overrides XDG", func(t *testing.T) {
		xdgDir := t.TempDir()
		t.Setenv("XDG_DATA_HOME", xdgDir)

		explicitDir := t.TempDir()
		cfg := config.ObjectStorageConfig{
			Backend:  "local",
			BasePath: explicitDir,
		}
		p, err := NewFromConfig(cfg, "test-tenant")
		if err != nil {
			t.Fatalf("NewFromConfig() error = %v", err)
		}
		defer func() { _ = p.Close() }()

		// Explicit path should be used, not XDG
		if _, err := os.Stat(filepath.Join(explicitDir, "test-tenant")); err != nil {
			t.Fatalf("expected tenant dir under explicit path: %v", err)
		}
		// XDG path should NOT have been created
		xdgObjects := filepath.Join(xdgDir, "crosscodex", "objects")
		if _, err := os.Stat(xdgObjects); !os.IsNotExist(err) {
			t.Errorf("XDG path should not exist when explicit BasePath is set")
		}
	})

	t.Run("local backend", func(t *testing.T) {
		dir := t.TempDir()
		cfg := config.ObjectStorageConfig{
			Backend:  "local",
			BasePath: dir,
		}
		p, err := NewFromConfig(cfg, "test-tenant")
		if err != nil {
			t.Fatalf("NewFromConfig() error = %v", err)
		}
		defer func() { _ = p.Close() }()
	})

	t.Run("unsupported backend", func(t *testing.T) {
		cfg := config.ObjectStorageConfig{
			Backend: "gcs",
		}
		_, err := NewFromConfig(cfg, "test-tenant")
		if err == nil {
			t.Fatal("expected error for unsupported backend")
		}
	})

	t.Run("empty backend", func(t *testing.T) {
		cfg := config.ObjectStorageConfig{}
		_, err := NewFromConfig(cfg, "test-tenant")
		if err == nil {
			t.Fatal("expected error for empty backend")
		}
	})

	t.Run("local with empty tenant", func(t *testing.T) {
		cfg := config.ObjectStorageConfig{
			Backend:  "local",
			BasePath: t.TempDir(),
		}
		_, err := NewFromConfig(cfg, "")
		if err == nil {
			t.Fatal("expected error for empty tenant")
		}
	})
}

func TestXdgDataHome(t *testing.T) {
	t.Run("uses XDG_DATA_HOME when set", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", "/custom/data")
		got := xdgDataHome()
		if got != "/custom/data" {
			t.Errorf("xdgDataHome() = %q, want %q", got, "/custom/data")
		}
	})

	t.Run("falls back to HOME/.local/share", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", "")
		t.Setenv("HOME", "/home/testuser")
		got := xdgDataHome()
		want := "/home/testuser/.local/share"
		if !strings.HasSuffix(got, ".local/share") || got != want {
			t.Errorf("xdgDataHome() = %q, want %q", got, want)
		}
	})
}
