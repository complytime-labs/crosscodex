package config_test

import (
	"time"

	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

// expectOnlyFieldError asserts that err reports exactly one problem, a
// FieldError for the setting at path, and nothing untagged. A rejected-value
// spec that calls it fails if its validator forgets the path.
func expectOnlyFieldError(err error, path string) {
	fields := config.FieldErrors(err)
	ExpectWithOffset(1, fields).To(HaveLen(1), "want one FieldError for %s in: %v", path, err)
	ExpectWithOffset(1, fields[0].Path).To(Equal(path))
	ExpectWithOffset(1, config.FieldErrorsComplete(err)).To(BeTrue(), "untagged problem in: %v", err)
}

// validConfig returns a Config that passes ExportValidateConfig with every
// section populated, so a spec can mutate only the field it tests and still
// reach that field's validation. Each call returns an independent value.
func validConfig() *config.Config {
	return &config.Config{
		TLS:     config.TLSConfig{Mode: "off"},
		Storage: config.StorageConfig{Objects: config.ObjectStorageConfig{Backend: "local"}},
		Logging: config.LoggingConfig{Level: "info", Format: "text"},
		LLM:     config.LLMConfig{Timeout: 30, MaxRetries: 3},
		Attestation: config.AttestationConfig{
			Enabled:           true,
			ExpiryDuration:    8760 * time.Hour,
			IncludeByProducts: true,
		},
		Pipeline: config.PipelineConfig{MaxConcurrentJobs: 10, StageTimeout: 5 * time.Minute},
		Worker:   config.WorkerConfig{MaxAbandonedCollections: 10000, MaxInFlight: 1},
		Graph:    config.GraphConfig{MaxBulkEdges: config.DefaultGraphMaxBulkEdges},
		Analysis: config.AnalysisConfig{
			Engine:               config.EngineConfig{TaskTimeout: 5 * time.Minute, MaxRetries: 3, RetryBackoff: time.Second},
			Classification:       config.ClassificationConfig{Enabled: true, MaxTextLength: 2000, MaxTokens: 20},
			Embedding:            config.EmbeddingConfig{Enabled: true, Models: []string{"snowflake-arctic-embed2"}, MaxChars: 1500, BatchSize: 50},
			Relationship:         config.RelationshipConfig{MaxSourceChars: 1500, MaxTargetChars: 800, MaxTokens: 300, SamplesPerModel: 1, SamplingTemperature: 0.3, ConfidenceWeights: config.RelationshipConfidenceWeights{High: 1.0, Medium: 0.67, Low: 0.33}},
			Candidates:           config.CandidateConfig{EmbedModel: "snowflake-arctic-embed2", MaxPerSource: 10},
			ArtifactAdjudication: config.ArtifactAdjudicationConfig{LeaseDuration: 15 * time.Minute},
			ResultCache:          config.ResultCacheConfig{Enabled: true, LookupBatchSize: 1000},
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
