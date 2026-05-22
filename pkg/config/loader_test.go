package config

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoader_DefaultsOnly(t *testing.T) {
	cfg := loadDefaults(t)

	if cfg.LLM.Timeout != 30 {
		t.Errorf("LLM.Timeout = %d, want default 30", cfg.LLM.Timeout)
	}
	if cfg.Storage.Objects.Backend != "local" {
		t.Errorf("Storage.Objects.Backend = %q, want default local", cfg.Storage.Objects.Backend)
	}
	if cfg.TLS.Mode != "off" {
		t.Errorf("TLS.Mode = %q, want default off", cfg.TLS.Mode)
	}
}

func TestLoader_UserConfigOverridesDefaults(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpHome)

	userDir := filepath.Join(tmpHome, "crosscodex")
	writeFile(t, filepath.Join(userDir, "config.yaml"), "llm:\n  gateway_url: \"https://user:4000\"\n  timeout: 60\n")

	loader := NewLoader()
	cfg, err := loader.Load(context.Background())
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}

	if cfg.LLM.GatewayURL != "https://user:4000" {
		t.Errorf("LLM.GatewayURL = %q, want user config value", cfg.LLM.GatewayURL)
	}
	if cfg.LLM.Timeout != 60 {
		t.Errorf("LLM.Timeout = %d, want 60 from user config", cfg.LLM.Timeout)
	}
	if cfg.Storage.Objects.Backend != "local" {
		t.Errorf("Storage.Objects.Backend = %q, want default preserved", cfg.Storage.Objects.Backend)
	}
}

func TestLoader_DropInsOverrideUserConfig(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpHome)

	userDir := filepath.Join(tmpHome, "crosscodex")
	writeFile(t, filepath.Join(userDir, "config.yaml"), "llm:\n  gateway_url: \"https://user:4000\"\n  timeout: 60\n")
	writeFile(t, filepath.Join(userDir, "conf.d", "10-override.yaml"), "llm:\n  gateway_url: \"https://dropin:5000\"\n")

	loader := NewLoader()
	cfg, err := loader.Load(context.Background())
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}

	if cfg.LLM.GatewayURL != "https://dropin:5000" {
		t.Errorf("LLM.GatewayURL = %q, want drop-in override", cfg.LLM.GatewayURL)
	}
	if cfg.LLM.Timeout != 60 {
		t.Errorf("LLM.Timeout = %d, want preserved from user config", cfg.LLM.Timeout)
	}
}

func TestLoader_ProjectConfigOverridesUser(t *testing.T) {
	tmpHome := t.TempDir()
	projectDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpHome)

	writeFile(t, filepath.Join(tmpHome, "crosscodex", "config.yaml"), "llm:\n  gateway_url: \"https://user:4000\"\n  timeout: 60\n")
	writeFile(t, filepath.Join(projectDir, ".crosscodex", "config.yaml"), "llm:\n  gateway_url: \"https://project:7000\"\n")

	loader := NewLoader()
	cfg, err := loader.Load(context.Background(), WithProjectDir(projectDir))
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}

	if cfg.LLM.GatewayURL != "https://project:7000" {
		t.Errorf("LLM.GatewayURL = %q, want project override", cfg.LLM.GatewayURL)
	}
	if cfg.LLM.Timeout != 60 {
		t.Errorf("LLM.Timeout = %d, want preserved from user config", cfg.LLM.Timeout)
	}
}

func TestLoader_EnvOverridesProjectConfig(t *testing.T) {
	projectDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("CROSSCODEX_LLM_GATEWAY_URL", "https://env:9000")

	writeFile(t, filepath.Join(projectDir, ".crosscodex", "config.yaml"), "llm:\n  gateway_url: \"https://project:7000\"\n  timeout: 45\n")

	loader := NewLoader()
	cfg, err := loader.Load(context.Background(), WithProjectDir(projectDir))
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}

	if cfg.LLM.GatewayURL != "https://env:9000" {
		t.Errorf("LLM.GatewayURL = %q, want env override", cfg.LLM.GatewayURL)
	}
	if cfg.LLM.Timeout != 45 {
		t.Errorf("LLM.Timeout = %d, want preserved from project config", cfg.LLM.Timeout)
	}
}

func TestLoader_OverridesAreHighestPriority(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("CROSSCODEX_LLM_GATEWAY_URL", "https://env:9000")

	loader := NewLoader()
	cfg, err := loader.Load(context.Background(), WithOverrides(map[string]string{
		"llm.gateway_url": "https://flag:1111",
	}))
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}

	if cfg.LLM.GatewayURL != "https://flag:1111" {
		t.Errorf("LLM.GatewayURL = %q, want CLI flag override", cfg.LLM.GatewayURL)
	}
}

func TestLoader_ProfileLoadsCorrectly(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpHome)

	writeFile(t, filepath.Join(tmpHome, "crosscodex", "profiles", "local.yaml"), "server:\n  workers: 2\nllm:\n  gateway_url: \"https://local:4000\"\n")

	loader := NewLoader()
	cfg, err := loader.Load(context.Background(), WithProfile("local"))
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}

	if cfg.Server.Workers != 2 {
		t.Errorf("Server.Workers = %d, want 2 from profile", cfg.Server.Workers)
	}
	if cfg.LLM.GatewayURL != "https://local:4000" {
		t.Errorf("LLM.GatewayURL = %q, want profile value", cfg.LLM.GatewayURL)
	}
}

func TestLoader_ProfileNotFound(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	loader := NewLoader()
	_, err := loader.Load(context.Background(), WithProfile("nonexistent"))
	if err == nil {
		t.Fatal("expected error for missing profile")
	}
	if !errors.Is(err, ErrProfileNotFound) {
		t.Errorf("error = %v, want ErrProfileNotFound", err)
	}
}

func TestLoader_ValidationFailure(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpHome)

	writeFile(t, filepath.Join(tmpHome, "crosscodex", "config.yaml"), "tls:\n  mode: \"bogus\"\n")

	loader := NewLoader()
	_, err := loader.Load(context.Background())
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("error = %v, want ErrInvalidConfig", err)
	}
}

func TestLoader_ValidationErrorIncludesSourceFile(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpHome)

	badFile := filepath.Join(tmpHome, "crosscodex", "conf.d", "99-bad.yaml")
	writeFile(t, badFile, "tls:\n  mode: \"invalid-mode\"\n")

	loader := NewLoader()
	_, err := loader.Load(context.Background())
	if err == nil {
		t.Fatal("expected validation error")
	}

	msg := err.Error()
	if !strings.Contains(msg, "99-bad.yaml") {
		t.Errorf("error message should reference source file, got: %s", msg)
	}
	if !strings.Contains(msg, "invalid-mode") {
		t.Errorf("error message should include the bad value, got: %s", msg)
	}
}

func TestLoader_ValidationErrorIncludesEnvVar(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("CROSSCODEX_LOGGING_LEVEL", "verbose")

	loader := NewLoader()
	_, err := loader.Load(context.Background())
	if err == nil {
		t.Fatal("expected validation error")
	}

	msg := err.Error()
	if !strings.Contains(msg, "CROSSCODEX_LOGGING_LEVEL") {
		t.Errorf("error message should reference env var, got: %s", msg)
	}
}

func TestLoader_WithConfigPathSkipsLayeredResolution(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpHome)

	writeFile(t, filepath.Join(tmpHome, "crosscodex", "config.yaml"), "llm:\n  gateway_url: \"https://user-should-be-skipped:4000\"\n")

	singleFile := filepath.Join(t.TempDir(), "custom.yaml")
	writeFile(t, singleFile, "llm:\n  gateway_url: \"https://custom:8000\"\n")

	loader := NewLoader()
	cfg, err := loader.Load(context.Background(), WithConfigPath(singleFile))
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}

	if cfg.LLM.GatewayURL != "https://custom:8000" {
		t.Errorf("LLM.GatewayURL = %q, want custom file value", cfg.LLM.GatewayURL)
	}
}

func TestLoader_FullPrecedenceStack(t *testing.T) {
	tmpHome := t.TempDir()
	projectDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpHome)
	t.Setenv("CROSSCODEX_LOGGING_LEVEL", "debug")

	userDir := filepath.Join(tmpHome, "crosscodex")

	writeFile(t, filepath.Join(userDir, "config.yaml"), "llm:\n  gateway_url: \"https://user:4000\"\n  timeout: 60\nlogging:\n  level: warn\n")
	writeFile(t, filepath.Join(userDir, "conf.d", "10-team.yaml"), "llm:\n  timeout: 45\n")
	writeFile(t, filepath.Join(userDir, "profiles", "local.yaml"), "server:\n  workers: 1\n")
	writeFile(t, filepath.Join(projectDir, ".crosscodex", "config.yaml"), "llm:\n  gateway_url: \"https://project:7000\"\n")

	loader := NewLoader()
	cfg, err := loader.Load(context.Background(),
		WithProfile("local"),
		WithProjectDir(projectDir),
		WithOverrides(map[string]string{
			"nats.url": "nats://flag:4222",
		}),
	)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}

	if cfg.LLM.GatewayURL != "https://project:7000" {
		t.Errorf("LLM.GatewayURL = %q, want project override", cfg.LLM.GatewayURL)
	}
	if cfg.LLM.Timeout != 45 {
		t.Errorf("LLM.Timeout = %d, want drop-in override 45", cfg.LLM.Timeout)
	}
	if cfg.Server.Workers != 1 {
		t.Errorf("Server.Workers = %d, want 1 from profile", cfg.Server.Workers)
	}
	if cfg.Logging.Level != "debug" {
		t.Errorf("Logging.Level = %q, want debug from env", cfg.Logging.Level)
	}
	if cfg.NATS.URL != "nats://flag:4222" {
		t.Errorf("NATS.URL = %q, want CLI flag override", cfg.NATS.URL)
	}
	if cfg.TLS.Mode != "off" {
		t.Errorf("TLS.Mode = %q, want default off", cfg.TLS.Mode)
	}
}

func TestLoader_ServiceConfigAndCLIConfig(t *testing.T) {
	cfg := loadDefaults(t)

	sc := cfg.ServiceConfig()
	if sc.GRPCAddr != ":50051" {
		t.Errorf("ServiceConfig.GRPCAddr = %q, want default", sc.GRPCAddr)
	}

	cc := cfg.CLIConfig()
	if cc.Output != "table" {
		t.Errorf("CLIConfig.Output = %q, want default", cc.Output)
	}
}

func TestLoader_TLSPerTargetMergesWithGlobal(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpHome)

	userDir := filepath.Join(tmpHome, "crosscodex")
	writeFile(t, filepath.Join(userDir, "config.yaml"),
		"tls:\n  mode: server-only\n  ca: /etc/ca.crt\n  cert: /etc/server.crt\n  key: /etc/server.key\n  targets:\n    ingestion:\n      mode: mutual\n")
	writeFile(t, filepath.Join(userDir, "conf.d", "10-tls.yaml"),
		"tls:\n  targets:\n    ingestion:\n      cert: /etc/ingestion.crt\n      key: /etc/ingestion.key\n    catalog:\n      mode: mutual\n      cert: /etc/catalog.crt\n      key: /etc/catalog.key\n")

	loader := NewLoader()
	cfg, err := loader.Load(context.Background())
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}

	if cfg.TLS.Mode != "server-only" {
		t.Errorf("TLS.Mode = %q, want global default preserved", cfg.TLS.Mode)
	}
	if cfg.TLS.CA != "/etc/ca.crt" {
		t.Errorf("TLS.CA = %q, want global value preserved", cfg.TLS.CA)
	}
	ingestion := cfg.TLS.Targets["ingestion"]
	if ingestion.Mode != "mutual" {
		t.Errorf("targets.ingestion.mode = %q, want mutual from user config", ingestion.Mode)
	}
	if ingestion.Cert != "/etc/ingestion.crt" {
		t.Errorf("targets.ingestion.cert = %q, want value from drop-in", ingestion.Cert)
	}
	if ingestion.Key != "/etc/ingestion.key" {
		t.Errorf("targets.ingestion.key = %q, want value from drop-in", ingestion.Key)
	}
	catalog := cfg.TLS.Targets["catalog"]
	if catalog.Mode != "mutual" {
		t.Errorf("targets.catalog.mode = %q, want mutual from drop-in", catalog.Mode)
	}
}

func TestLoader_MalformedConfigFile(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpHome)

	writeFile(t, filepath.Join(tmpHome, "crosscodex", "config.yaml"), ":\n  - :\n  broken: [yaml\n")

	loader := NewLoader()
	_, err := loader.Load(context.Background())
	if err == nil {
		t.Fatal("expected error for malformed config file")
	}
	if !errors.Is(err, ErrLoadFailed) {
		t.Errorf("error = %v, want ErrLoadFailed", err)
	}
}

func TestLoader_MalformedDropInFile(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpHome)

	userDir := filepath.Join(tmpHome, "crosscodex")
	writeFile(t, filepath.Join(userDir, "conf.d", "10-good.yaml"), "llm:\n  timeout: 60\n")
	writeFile(t, filepath.Join(userDir, "conf.d", "20-bad.yaml"), "not: [valid: yaml\n")

	loader := NewLoader()
	_, err := loader.Load(context.Background())
	if err == nil {
		t.Fatal("expected error for malformed drop-in file")
	}
	if !errors.Is(err, ErrLoadFailed) {
		t.Errorf("error = %v, want ErrLoadFailed", err)
	}
}

func TestLoader_InvalidProfileName(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	loader := NewLoader()
	_, err := loader.Load(context.Background(), WithProfile("../../etc/passwd"))
	if err == nil {
		t.Fatal("expected error for path-traversal profile name")
	}
	if !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("error = %v, want ErrInvalidConfig", err)
	}
}

func TestLoader_WithConfigPathNonexistent(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	loader := NewLoader()
	cfg, err := loader.Load(context.Background(), WithConfigPath("/nonexistent/path/config.yaml"))
	if err != nil {
		t.Fatalf("Load error: %v (nonexistent config path should fall through to defaults)", err)
	}
	if cfg.TLS.Mode != "off" {
		t.Errorf("TLS.Mode = %q, want default when config path doesn't exist", cfg.TLS.Mode)
	}
}

func TestNewLoader_ReturnsNonNil(t *testing.T) {
	loader := NewLoader()
	if loader == nil {
		t.Fatal("NewLoader returned nil")
	}
}
