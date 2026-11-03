package config_test

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/candidate"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/candidate/builtin"
	"github.com/complytime-labs/crosscodex/pkg/config"
)

func FuzzInferTag(f *testing.F) {
	f.Add("true")
	f.Add("false")
	f.Add("42")
	f.Add("3.14")
	f.Add("")
	f.Add("null")
	f.Add("yes")
	f.Add("no")
	f.Add("-1")
	f.Add("+0")

	f.Fuzz(func(t *testing.T, value string) {
		// Must not panic with nil schema node.
		tag := config.ExportInferTag(nil, value)
		if tag == "" {
			t.Errorf("inferTag returned empty string for value %q with nil schema", value)
		}
	})
}

func FuzzBuildNodeFromPath(f *testing.F) {
	f.Add("tls_mode", "mutual")
	f.Add("storage_objects_backend", "s3")
	f.Add("llm_timeout", "30")
	f.Add("", "")
	f.Add("a_b_c_d_e", "value")
	f.Add("logging_level", "debug")

	f.Fuzz(func(t *testing.T, path, value string) {
		// Split on underscore to create segments, matching the env var convention.
		var segments []string
		if path != "" {
			segments = strings.Split(path, "_")
		}
		// Must not panic regardless of input.
		node := config.ExportBuildNodeFromPath(segments, value)
		if node == nil && len(segments) > 0 {
			t.Errorf("buildNodeFromPath returned nil for non-empty segments %v", segments)
		}
	})
}

func FuzzValidateConfig(f *testing.F) {
	f.Add("off", "local", "8760h")
	f.Add("mutual", "s3", "1h")
	f.Add("server-only", "local", "24h")
	f.Add("INVALID", "INVALID", "0s")
	f.Add("", "", "8760h")
	f.Add("mutual\x00", "s3\x00", "-1h")

	f.Fuzz(func(t *testing.T, tlsMode, storageBackend, expiryStr string) {
		expiry, err := time.ParseDuration(expiryStr)
		if err != nil {
			// Use a valid default when parse fails — we're testing validation, not duration parsing.
			expiry = 8760 * time.Hour
		}

		cfg := &config.Config{}
		cfg.TLS.Mode = tlsMode
		cfg.Storage.Objects.Backend = storageBackend
		cfg.Attestation.ExpiryDuration = expiry

		// Must not panic regardless of input.
		_ = config.ExportValidateConfig(cfg)
	})
}

// FuzzCandidateGeneratorConfig drives YAML through
// analysis.candidates.generators[] the way the loader decodes it. Accepted
// input must hold the validated invariants and must be usable by the builtin
// generator it names: config validation and the generators' parameter
// readers (pkg/analyzer/candidate/builtin/parameters.go) duplicate the
// type rules, and this keeps them in agreement.
func FuzzCandidateGeneratorConfig(f *testing.F) {
	f.Add("- name: semantic\n  enabled: true\n  weight: 0.5\n  config:\n    top_k: 5\n")
	f.Add("- name: keyword\n  enabled: true\n  config:\n    keywords: [policy]\n")
	f.Add("- name: semantic\n  config:\n    top_k: 1.5\n")
	f.Add("- name: semantic\n  config:\n    top_k: \"5\"\n")
	f.Add("- name: semantic\n  config:\n    top_k: 1e300\n")
	f.Add("- name: keyword\n  config:\n    keywords: 5\n")
	f.Add("- name: keyword\n  config:\n    keywords: [{a: 1}]\n")
	f.Add("- name: level\n  weight: .nan\n")
	f.Add("- name: semantic\n  config:\n    min_similarity: .inf\n")

	controls := map[string]*candidate.ControlData{
		"a": {ControlID: "a", Text: "Establish a policy", Level: "Strategic"},
		"b": {ControlID: "b", Text: "Review logs", Level: "Operational"},
	}
	matrix := struct {
		IDs    []string
		Values [][]float32
	}{IDs: []string{"a", "b"}, Values: [][]float32{{100, 80}, {80, 100}}}
	generators := map[string]candidate.Generator{
		"keyword":  builtin.NewKeywordGenerator(),
		"level":    builtin.NewLevelGenerator(),
		"semantic": builtin.NewSemanticGenerator(),
	}

	f.Fuzz(func(t *testing.T, doc string) {
		var entries []config.CandidateGeneratorEntry
		if err := yaml.Unmarshal([]byte(doc), &entries); err != nil {
			return
		}
		cfg := validConfig()
		cfg.Analysis.Candidates.Generators = entries
		if err := config.ExportValidateConfig(cfg); err != nil {
			return
		}

		for i, e := range entries {
			if math.IsNaN(e.Weight) || e.Weight < 0 || e.Weight > 1 {
				t.Fatalf("accepted generators[%d].weight %v outside [0, 1]", i, e.Weight)
			}
			gen, ok := generators[e.Name]
			if !ok {
				t.Fatalf("accepted unknown generator name %q", e.Name)
			}
			params := make(map[string]interface{}, len(e.Config)+1)
			for k, v := range e.Config {
				params[k] = v
			}
			params["weight"] = e.Weight
			req := candidate.GenerateRequest{
				SourceControls: controls, TargetControls: controls,
				EmbeddingMatrix: matrix, Parameters: params,
			}
			if _, err := gen.Generate(context.Background(), req); err != nil {
				t.Fatalf("generator %q rejected accepted config %v: %v", e.Name, e.Config, err)
			}
		}
	})
}
