package config

import (
	"errors"
	"testing"
)

func TestValidate_ValidConfig(t *testing.T) {
	cfg := &Config{
		LLM: LLMConfig{
			GatewayURL: "http://localhost:4000", // DevSkim: ignore DS162092 — test fixture
			Timeout:    30,
		},
		Storage: StorageConfig{
			Objects: ObjectStorageConfig{Backend: "local"},
		},
		TLS: TLSConfig{Mode: "off"},
		Database: DatabaseConfig{
			DSN:     "postgres://localhost:5432/crosscodex", // DevSkim: ignore DS162092 — test fixture
			SSLMode: "prefer",
		},
		Logging: LoggingConfig{Level: "info", Format: "text"},
	}

	if err := validate(cfg, nil); err != nil {
		t.Errorf("validate returned error for valid config: %v", err)
	}
}

func TestValidate_MutualTLSWithAllFields(t *testing.T) {
	cfg := &Config{
		TLS: TLSConfig{
			Mode: "mutual",
			CA:   "/etc/ca.crt",
			Cert: "/etc/server.crt",
			Key:  "/etc/server.key",
		},
		Storage: StorageConfig{Objects: ObjectStorageConfig{Backend: "local"}},
		Logging: LoggingConfig{Level: "info", Format: "text"},
	}

	if err := validate(cfg, nil); err != nil {
		t.Errorf("expected no error for valid mutual TLS config: %v", err)
	}
}

func TestValidate_InvalidConfigs(t *testing.T) {
	base := func() Config {
		return Config{
			TLS:     TLSConfig{Mode: "off"},
			Storage: StorageConfig{Objects: ObjectStorageConfig{Backend: "local"}},
			Logging: LoggingConfig{Level: "info", Format: "text"},
		}
	}

	tests := []struct {
		name   string
		modify func(*Config)
	}{
		{
			name:   "invalid TLS mode",
			modify: func(c *Config) { c.TLS.Mode = "invalid" },
		},
		{
			name:   "mutual TLS missing cert and key",
			modify: func(c *Config) { c.TLS.Mode = "mutual"; c.TLS.CA = "/etc/ca.crt" },
		},
		{
			name:   "mutual TLS missing CA",
			modify: func(c *Config) { c.TLS.Mode = "mutual"; c.TLS.Cert = "/etc/server.crt"; c.TLS.Key = "/etc/server.key" },
		},
		{
			name:   "server-only TLS missing key",
			modify: func(c *Config) { c.TLS.Mode = "server-only"; c.TLS.Cert = "/etc/server.crt" },
		},
		{
			name:   "server-only TLS missing cert",
			modify: func(c *Config) { c.TLS.Mode = "server-only"; c.TLS.Key = "/etc/server.key" },
		},
		{
			name:   "invalid storage backend",
			modify: func(c *Config) { c.Storage.Objects.Backend = "azure" },
		},
		{
			name:   "invalid log level",
			modify: func(c *Config) { c.Logging.Level = "verbose" },
		},
		{
			name:   "invalid log format",
			modify: func(c *Config) { c.Logging.Format = "yaml" },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base()
			tt.modify(&cfg)

			err := validate(&cfg, nil)
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !errors.Is(err, ErrInvalidConfig) {
				t.Errorf("error = %v, want ErrInvalidConfig", err)
			}
		})
	}
}
