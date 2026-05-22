package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestNATSConfigDeserialization(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		yaml    string
		wantURL string
		wantDir string
		wantLLM time.Duration
		wantEvt time.Duration
	}{
		{
			name: "embedded mode defaults",
			yaml: `
url: ""
embedded:
  store_dir: ""
streams:
  audit_llm_retention: 2160h
  audit_events_retention: 720h
`,
			wantURL: "",
			wantDir: "",
			wantLLM: 2160 * time.Hour,
			wantEvt: 720 * time.Hour,
		},
		{
			name: "external mode with custom retention",
			yaml: `
url: "nats://nats.example.com:4222"
cluster: "prod"
tls: true
embedded:
  store_dir: "/var/lib/crosscodex/nats"
streams:
  audit_llm_retention: 4320h
  audit_events_retention: 168h
`,
			wantURL: "nats://nats.example.com:4222",
			wantDir: "/var/lib/crosscodex/nats",
			wantLLM: 4320 * time.Hour,
			wantEvt: 168 * time.Hour,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var cfg NATSConfig
			if err := yaml.Unmarshal([]byte(tt.yaml), &cfg); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if cfg.URL != tt.wantURL {
				t.Errorf("URL = %q, want %q", cfg.URL, tt.wantURL)
			}
			if cfg.Embedded.StoreDir != tt.wantDir {
				t.Errorf("Embedded.StoreDir = %q, want %q", cfg.Embedded.StoreDir, tt.wantDir)
			}
			if cfg.Streams.AuditLLMRetention != tt.wantLLM {
				t.Errorf("Streams.AuditLLMRetention = %v, want %v", cfg.Streams.AuditLLMRetention, tt.wantLLM)
			}
			if cfg.Streams.AuditEventsRetention != tt.wantEvt {
				t.Errorf("Streams.AuditEventsRetention = %v, want %v", cfg.Streams.AuditEventsRetention, tt.wantEvt)
			}
		})
	}
}
