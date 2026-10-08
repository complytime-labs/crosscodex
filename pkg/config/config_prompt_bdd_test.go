//go:build !integration

package config_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

var _ = Describe("PromptConfig", func() {
	Context("defaults", func() {
		It("loads default prompt config values", func() {
			tmpHome := GinkgoT().TempDir()
			GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

			loader := config.NewLoader()
			cfg, err := loader.Load(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.Prompt.CaptureContent).To(BeTrue())
			Expect(cfg.Prompt.AllowCommands).To(BeFalse())
			Expect(cfg.Prompt.LayerPaths).To(BeEmpty())
			Expect(cfg.Prompt.Layers.Enabled).To(BeTrue())
			Expect(cfg.Prompt.Layers.Order).To(BeEmpty())
		})
	})

	Context("validation", func() {
		// validBaseConfig returns a Config with valid defaults for all existing
		// sections so validatePrompt tests only fail on prompt config issues.
		validBaseConfig := func() *config.Config {
			return &config.Config{
				TLS:         config.TLSConfig{Mode: "off"},
				Storage:     config.StorageConfig{Objects: config.ObjectStorageConfig{Backend: "local"}},
				Logging:     config.LoggingConfig{Level: "info", Format: "text"},
				Attestation: config.AttestationConfig{ExpiryDuration: 8760 * time.Hour},
				Graph:       config.GraphConfig{MaxBulkEdges: config.DefaultGraphMaxBulkEdges},
				Analysis: config.AnalysisConfig{
					Engine:         config.EngineConfig{TaskTimeout: 5 * time.Minute, MaxRetries: 3, RetryBackoff: time.Second},
					Classification: config.ClassificationConfig{MaxTextLength: 2000, MaxTokens: 20},
					Embedding:      config.EmbeddingConfig{Enabled: true, Models: []string{"snowflake-arctic-embed2"}, MaxChars: 1500, BatchSize: 50},
					Relationship:   config.RelationshipConfig{TopK: 20, MaxSourceChars: 1500, MaxTargetChars: 800, MaxTokens: 300, SamplesPerModel: 1, SamplingTemperature: 0.3},
					Candidates:     config.CandidateConfig{EmbedModel: "snowflake-arctic-embed2"},
				},
				Synthesis: config.SynthesisConfig{
					ConfidenceThreshold:   0.5,
					MaxMappingsPerControl: 10,
					Viability:             config.ViabilityConfig{TypeMismatchFactor: 0.8, SkipLevelFactor: 0.7, IntegralToFactor: 1.1},
					Assessment:            config.AssessmentConfig{IQRGood: 20, IQRPoor: 10, NoRelHigh: 0.97, NoRelLow: 0.80, ContestedWarn: 0.20, ActionableWarn: 0.30},
				},
				Backup: config.BackupConfig{
					MaxAge: config.BackupMaxAgeConfig{Postgres: 26 * time.Hour, Objects: 26 * time.Hour, NATS: 26 * time.Hour},
				},
			}
		}

		It("rejects invalid merge mode in layer order", func() {
			cfg := validBaseConfig()
			cfg.Prompt.Layers.Order = []config.PromptLayerEntry{
				{ID: "embedded", Merge: "invalid"},
			}
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
			Expect(err.Error()).To(ContainSubstring("merge"))
		})

		It("rejects invalid slice_strategy in layer order", func() {
			cfg := validBaseConfig()
			cfg.Prompt.Layers.Order = []config.PromptLayerEntry{
				{ID: "embedded", SliceStrategy: "bogus"},
			}
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
			Expect(err.Error()).To(ContainSubstring("slice_strategy"))
		})

		It("rejects unknown layer IDs", func() {
			cfg := validBaseConfig()
			cfg.Prompt.Layers.Order = []config.PromptLayerEntry{
				{ID: "nonexistent"},
			}
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
			Expect(err.Error()).To(ContainSubstring("nonexistent"))
		})

		It("rejects duplicate layer IDs", func() {
			cfg := validBaseConfig()
			cfg.Prompt.Layers.Order = []config.PromptLayerEntry{
				{ID: "embedded"},
				{ID: "embedded"},
			}
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
			Expect(err.Error()).To(ContainSubstring("duplicate"))
		})

		It("accepts valid layer order with known IDs", func() {
			cfg := validBaseConfig()
			cfg.Prompt.Layers.Order = []config.PromptLayerEntry{
				{ID: "embedded", Merge: "merge"},
				{ID: "user", Merge: "replace", SliceStrategy: "append"},
				{ID: "project"},
				{ID: "cli", SliceStrategy: "deep_copy"},
			}
			err := config.ExportValidateConfig(cfg)
			Expect(err).NotTo(HaveOccurred())
		})

		It("accepts layer IDs that match a layer_paths entry", func() {
			cfg := validBaseConfig()
			cfg.Prompt.LayerPaths = []string{"/custom/layer"}
			cfg.Prompt.Layers.Order = []config.PromptLayerEntry{
				{ID: "/custom/layer"},
			}
			err := config.ExportValidateConfig(cfg)
			Expect(err).NotTo(HaveOccurred())
		})

		// loadWithPromptOverride loads a config file whose only setting is a
		// prompt.tenant_overrides entry under key.
		loadWithPromptOverride := func(key string) (*config.Config, error) {
			GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())
			path := filepath.Join(GinkgoT().TempDir(), "config.yaml")
			writeTestFile(path, "prompt:\n  tenant_overrides:\n    \""+key+"\":\n      capture_content: false\n")
			return config.NewLoader().Load(context.Background(), config.WithConfigPath(path))
		}

		DescribeTable("rejects a tenant_overrides key that is not a valid tenant ID",
			func(key string) {
				_, err := loadWithPromptOverride(key)
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
				Expect(err.Error()).To(ContainSubstring("prompt.tenant_overrides key %q", key))
				Expect(err.Error()).To(ContainSubstring(tenant.IDRule))
			},
			Entry("uppercase", "INVALID"),
			Entry("53 characters, one over the graph-name limit", "a"+strings.Repeat("b", 52)),
		)

		DescribeTable("accepts a tenant_overrides key that is a valid tenant ID",
			func(key string) {
				cfg, err := loadWithPromptOverride(key)
				Expect(err).NotTo(HaveOccurred())
				Expect(cfg.Prompt.TenantOverrides).To(HaveKey(key))
			},
			Entry("a typical ID", "acme-corp"),
			Entry("the longest ID, 52 characters", "a"+strings.Repeat("b", 51)),
		)
	})

	Context("ForTenant", func() {
		It("inherits global values when no override exists", func() {
			cfg := config.PromptConfig{
				CaptureContent: true,
				AllowCommands:  false,
			}
			tc := cfg.ForTenant("tenant-xyz")
			Expect(tc.CaptureContent).To(BeTrue())
			Expect(tc.AllowCommands).To(BeFalse())
		})

		It("applies per-tenant overrides", func() {
			f := false
			t := true
			cfg := config.PromptConfig{
				CaptureContent: true,
				AllowCommands:  false,
				TenantOverrides: map[string]config.PromptOverride{
					"tenant-abc": {
						CaptureContent: &f,
						AllowCommands:  &t,
					},
				},
			}
			tc := cfg.ForTenant("tenant-abc")
			Expect(tc.CaptureContent).To(BeFalse())
			Expect(tc.AllowCommands).To(BeTrue())
		})

		It("returns global values for unknown tenant", func() {
			cfg := config.PromptConfig{
				CaptureContent: true,
				AllowCommands:  true,
				TenantOverrides: map[string]config.PromptOverride{
					"tenant-abc": {
						CaptureContent: boolPtr(false),
					},
				},
			}
			tc := cfg.ForTenant("tenant-other")
			Expect(tc.CaptureContent).To(BeTrue())
			Expect(tc.AllowCommands).To(BeTrue())
		})

		It("partially overrides when only some fields are set", func() {
			cfg := config.PromptConfig{
				CaptureContent: true,
				AllowCommands:  false,
				TenantOverrides: map[string]config.PromptOverride{
					"tenant-partial": {
						AllowCommands: boolPtr(true),
					},
				},
			}
			tc := cfg.ForTenant("tenant-partial")
			Expect(tc.CaptureContent).To(BeTrue()) // inherited
			Expect(tc.AllowCommands).To(BeTrue())  // overridden
		})
	})
})

func boolPtr(b bool) *bool { return &b }
