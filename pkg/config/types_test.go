package config

import "testing"

func TestConfig_ServiceConfig(t *testing.T) {
	cfg := Config{
		LLM: LLMConfig{
			GatewayURL:     "http://localhost:4000",
			DefaultModel:   "qwen3:8b",
			EmbeddingModel: "qwen3-embedding",
			Timeout:        30,
		},
		Server: ServerConfig{
			GRPCAddr: ":50051",
			HTTPAddr: ":8080",
			Workers:  4,
		},
		Storage: StorageConfig{
			Objects: ObjectStorageConfig{
				Backend:  "local",
				BasePath: "/var/lib/crosscodex",
			},
		},
		Database: DatabaseConfig{
			DSN:        "postgres://localhost:5432/crosscodex",
			Extensions: []string{"age", "vector"},
		},
	}

	sc := cfg.ServiceConfig()

	if sc.GRPCAddr != ":50051" {
		t.Errorf("GRPCAddr = %q, want %q", sc.GRPCAddr, ":50051")
	}
	if sc.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want %q", sc.HTTPAddr, ":8080")
	}
	if sc.Workers != 4 {
		t.Errorf("Workers = %d, want %d", sc.Workers, 4)
	}
	if sc.LLM.GatewayURL != "http://localhost:4000" {
		t.Errorf("LLM.GatewayURL = %q, want %q", sc.LLM.GatewayURL, "http://localhost:4000")
	}
	if sc.Database.DSN != "postgres://localhost:5432/crosscodex" {
		t.Errorf("Database.DSN = %q, want %q", sc.Database.DSN, "postgres://localhost:5432/crosscodex")
	}
}

func TestConfig_CLIConfig(t *testing.T) {
	cfg := Config{
		LLM: LLMConfig{
			GatewayURL:   "http://localhost:4000",
			DefaultModel: "qwen3:8b",
			Timeout:      30,
		},
		CLI: CLISettings{
			Output:   "table",
			NoColor:  true,
			Endpoint: "http://localhost:8080",
		},
	}

	cc := cfg.CLIConfig()

	if cc.Output != "table" {
		t.Errorf("Output = %q, want %q", cc.Output, "table")
	}
	if !cc.NoColor {
		t.Error("NoColor = false, want true")
	}
	if cc.Endpoint != "http://localhost:8080" {
		t.Errorf("Endpoint = %q, want %q", cc.Endpoint, "http://localhost:8080")
	}
	if cc.LLM.GatewayURL != "http://localhost:4000" {
		t.Errorf("LLM.GatewayURL = %q, want %q", cc.LLM.GatewayURL, "http://localhost:4000")
	}
}
