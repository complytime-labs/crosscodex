package config

import "testing"

func TestDefaultConfig_HasSensibleValues(t *testing.T) {
	node, err := defaultNode()
	if err != nil {
		t.Fatalf("defaultNode error: %v", err)
	}

	cfg := mustUnmarshalNode[Config](t, node)

	tests := []struct {
		name string
		got  any
		want any
	}{
		{"llm.timeout", cfg.LLM.Timeout, 30},
		{"storage.objects.backend", cfg.Storage.Objects.Backend, "local"},
		{"tls.mode", cfg.TLS.Mode, "off"},
		{"database.max_conns", cfg.Database.MaxConns, 10},
		{"database.ssl_mode", cfg.Database.SSLMode, "prefer"},
		{"server.grpc_addr", cfg.Server.GRPCAddr, ":50051"},
		{"server.http_addr", cfg.Server.HTTPAddr, ":8080"},
		{"server.workers", cfg.Server.Workers, 4},
		{"cli.output", cfg.CLI.Output, "table"},
		{"logging.level", cfg.Logging.Level, "info"},
		{"logging.format", cfg.Logging.Format, "text"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("%s = %v, want %v", tt.name, tt.got, tt.want)
			}
		})
	}
}
