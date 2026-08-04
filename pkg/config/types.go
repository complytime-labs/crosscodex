package config

import (
	"fmt"
	"strings"
	"time"
)

// Config is the unified merge target for all configuration layers.
type Config struct {
	LLM           LLMConfig           `yaml:"llm"           json:"llm"`
	Storage       StorageConfig       `yaml:"storage"       json:"storage"`
	TLS           TLSConfig           `yaml:"tls"           json:"tls"`
	Tenants       TenantsConfig       `yaml:"tenants"       json:"tenants"`
	Database      DatabaseConfig      `yaml:"database"      json:"database"`
	NATS          NATSConfig          `yaml:"nats"          json:"nats"`
	Server        ServerConfig        `yaml:"server"        json:"server"`
	CLI           CLISettings         `yaml:"cli"           json:"cli"`
	Logging       LoggingConfig       `yaml:"logging"       json:"logging"`
	Auth          AuthConfig          `yaml:"auth"          json:"auth"`
	Observability ObservabilityConfig `yaml:"observability" json:"observability"`
	Catalog       CatalogConfig       `yaml:"catalog"       json:"catalog"`
	Attestation   AttestationConfig   `yaml:"attestation"   json:"attestation"`
	Prompt        PromptConfig        `yaml:"prompt"        json:"prompt"`
	Analysis      AnalysisConfig      `yaml:"analysis"      json:"analysis"`
	Worker        WorkerConfig        `yaml:"worker"        json:"worker"`
	Pipeline      PipelineConfig      `yaml:"pipeline"      json:"pipeline"`
	Synthesis     SynthesisConfig     `yaml:"synthesis"     json:"synthesis"`
}

// LLMConfig configures the LLM gateway client.
type LLMConfig struct {
	GatewayURL     string   `yaml:"gateway_url" json:"gateway_url"`
	GatewayMode    bool     `yaml:"gateway_mode" json:"gateway_mode"`
	DefaultModel   string   `yaml:"default_model" json:"default_model"`
	EmbeddingModel string   `yaml:"embedding_model" json:"embedding_model"`
	APIKeyRef      string   `yaml:"api_key_ref" json:"api_key_ref"`
	AllowedModels  []string `yaml:"allowed_models" json:"allowed_models"`
	MaxRetries     int      `yaml:"max_retries" json:"max_retries"`
	Timeout        int      `yaml:"timeout" json:"timeout"`
	// TenantOverrides maps tenant IDs (must satisfy pkg/tenant.ValidateTenantID)
	// to per-tenant LLM config overrides. Nil pointer fields inherit the global
	// value. AllowedModels replaces (not merges with) the global list when
	// set.
	TenantOverrides map[string]LLMOverride `yaml:"tenant_overrides" json:"tenant_overrides"`
}

// LLMOverride allows per-tenant LLM settings.
// Nil pointer fields inherit the global LLMConfig value.
// AllowedModels replaces (not merges with) the global list when non-nil.
type LLMOverride struct {
	GatewayURL     *string  `yaml:"gateway_url" json:"gateway_url"`
	DefaultModel   *string  `yaml:"default_model" json:"default_model"`
	EmbeddingModel *string  `yaml:"embedding_model" json:"embedding_model"`
	APIKeyRef      *string  `yaml:"api_key_ref" json:"api_key_ref"`
	AllowedModels  []string `yaml:"allowed_models" json:"allowed_models"`
	MaxRetries     *int     `yaml:"max_retries" json:"max_retries"`
	Timeout        *int     `yaml:"timeout" json:"timeout"`
}

// LLMTenantConfig holds the fully resolved LLM settings for a tenant.
// Returned by ForTenant after applying per-tenant overrides to global defaults.
type LLMTenantConfig struct {
	GatewayURL string
	// GatewayMode is always inherited from the global LLMConfig and cannot be
	// overridden per-tenant. When true, client-side retry is disabled.
	GatewayMode    bool
	DefaultModel   string
	EmbeddingModel string
	APIKeyRef      string
	AllowedModels  []string
	MaxRetries     int
	Timeout        int
}

// ForTenant returns the effective LLM settings for a tenant.
// Fields set in TenantOverrides take precedence; nil fields inherit global values.
func (c *LLMConfig) ForTenant(tenantID string) LLMTenantConfig {
	tc := LLMTenantConfig{
		GatewayURL:     c.GatewayURL,
		GatewayMode:    c.GatewayMode,
		DefaultModel:   c.DefaultModel,
		EmbeddingModel: c.EmbeddingModel,
		APIKeyRef:      c.APIKeyRef,
		AllowedModels:  c.AllowedModels,
		MaxRetries:     c.MaxRetries,
		Timeout:        c.Timeout,
	}
	if override, ok := c.TenantOverrides[tenantID]; ok {
		if override.GatewayURL != nil {
			tc.GatewayURL = *override.GatewayURL
		}
		if override.DefaultModel != nil {
			tc.DefaultModel = *override.DefaultModel
		}
		if override.EmbeddingModel != nil {
			tc.EmbeddingModel = *override.EmbeddingModel
		}
		if override.APIKeyRef != nil {
			tc.APIKeyRef = *override.APIKeyRef
		}
		if override.AllowedModels != nil {
			tc.AllowedModels = override.AllowedModels
		}
		if override.MaxRetries != nil {
			tc.MaxRetries = *override.MaxRetries
		}
		if override.Timeout != nil {
			tc.Timeout = *override.Timeout
		}
	}
	return tc
}

// StorageConfig configures storage backends.
type StorageConfig struct {
	Objects ObjectStorageConfig `yaml:"objects" json:"objects"`
}

// ObjectStorageConfig configures the object storage provider.
type ObjectStorageConfig struct {
	Backend  string `yaml:"backend" json:"backend"`
	BasePath string `yaml:"base_path" json:"base_path"`
	Bucket   string `yaml:"bucket" json:"bucket"`
	Region   string `yaml:"region" json:"region"`
	Endpoint string `yaml:"endpoint" json:"endpoint"`
}

// TLSConfig configures TLS certificates and mode.
type TLSConfig struct {
	Mode        string                 `yaml:"mode" json:"mode"`
	CA          string                 `yaml:"ca" json:"ca"`
	Cert        string                 `yaml:"cert" json:"cert"`
	Key         string                 `yaml:"key" json:"key"`
	FIPS        FIPSConfig             `yaml:"fips" json:"fips"`
	CipherAllow []string               `yaml:"cipher_allow" json:"cipher_allow"` // Substring-match cipher allowlist
	CipherDeny  []string               `yaml:"cipher_deny" json:"cipher_deny"`   // Substring-match cipher denylist
	Targets     map[string]TLSOverride `yaml:"targets" json:"targets"`
}

// FIPSConfig controls FIPS 140 enforcement.
type FIPSConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
}

// TLSOverride holds per-target TLS overrides that merge with global TLS defaults.
type TLSOverride struct {
	Mode string `yaml:"mode" json:"mode"`
	CA   string `yaml:"ca" json:"ca"`
	Cert string `yaml:"cert" json:"cert"`
	Key  string `yaml:"key" json:"key"`
}

// TenantsConfig configures multi-tenant behavior.
type TenantsConfig struct {
	Enabled        bool     `yaml:"enabled" json:"enabled"`
	DefaultTenant  string   `yaml:"default_tenant" json:"default_tenant"`
	AllowedTenants []string `yaml:"allowed_tenants" json:"allowed_tenants"`
}

// AuthConfig configures authentication methods.
type AuthConfig struct {
	X509Mappings []X509MappingConfig `yaml:"x509_mappings" json:"x509_mappings"`
}

// X509MappingConfig maps X.509 certificate field patterns to tenant and roles.
type X509MappingConfig struct {
	Match  X509MatchConfig `yaml:"match" json:"match"`
	Tenant string          `yaml:"tenant" json:"tenant"`
	Roles  []string        `yaml:"roles" json:"roles"`
}

// X509MatchConfig holds glob patterns for X.509 certificate field matching.
type X509MatchConfig struct {
	CN           string `yaml:"cn" json:"cn"`
	Organization string `yaml:"organization" json:"organization"`
	OrgUnit      string `yaml:"org_unit" json:"org_unit"`
	SANEmail     string `yaml:"san_email" json:"san_email"`
	SANDNS       string `yaml:"san_dns" json:"san_dns"`
	SANURI       string `yaml:"san_uri" json:"san_uri"`
}

// DatabaseConfig configures PostgreSQL connections.
//
// Two DSNs support the three-role security model (see pkg/db/doc.go):
//   - DSN connects as app_user for relational data behind RLS.
//   - GraphDSN connects as graph_user for AGE cypher queries.
//     graph_user owns per-tenant graph schemas but has no relational access.
type DatabaseConfig struct {
	DSN        string   `yaml:"dsn" json:"dsn"`
	GraphDSN   string   `yaml:"graph_dsn" json:"graph_dsn"`
	Extensions []string `yaml:"extensions" json:"extensions"`
	MaxConns   int      `yaml:"max_conns" json:"max_conns"`
	SSLMode    string   `yaml:"ssl_mode" json:"ssl_mode"`
}

// NATSConfig configures NATS JetStream connection.
type NATSConfig struct {
	URL      string             `yaml:"url" json:"url"`           // External NATS URL; empty = embedded mode
	Cluster  string             `yaml:"cluster" json:"cluster"`   // Cluster name (external mode)
	TLS      bool               `yaml:"tls" json:"tls"`           // Enable TLS
	Embedded NATSEmbeddedConfig `yaml:"embedded" json:"embedded"` // Embedded server settings
	Streams  NATSStreamsConfig  `yaml:"streams" json:"streams"`   // JetStream stream settings
}

// NATSEmbeddedConfig configures the embedded NATS server.
type NATSEmbeddedConfig struct {
	StoreDir string `yaml:"store_dir" json:"store_dir"` // JetStream storage dir; empty = $XDG_STATE_HOME/crosscodex/nats/
}

// NATSStreamsConfig configures JetStream audit stream retention.
type NATSStreamsConfig struct {
	AuditLLMRetention    time.Duration `yaml:"audit_llm_retention" json:"audit_llm_retention"`       // Default: 2160h (90 days)
	AuditEventsRetention time.Duration `yaml:"audit_events_retention" json:"audit_events_retention"` // Default: 720h (30 days)
	// AuditDecisions is always indefinite; no config knob.
}

// ServerConfig holds daemon-specific settings.
type ServerConfig struct {
	Addr          string `yaml:"addr" json:"addr"`
	Workers       int    `yaml:"workers" json:"workers"`
	MaxUploadSize int    `yaml:"max_upload_size" json:"max_upload_size"`
}

// CLISettings holds CLI-specific settings.
type CLISettings struct {
	Output          string `yaml:"output" json:"output"`
	NoColor         bool   `yaml:"no_color" json:"no_color"`
	Endpoint        string `yaml:"endpoint" json:"endpoint"`
	UploadChunkSize int    `yaml:"upload_chunk_size" json:"upload_chunk_size"`
}

// LoggingConfig configures structured logging.
type LoggingConfig struct {
	Level  string `yaml:"level" json:"level"`
	Format string `yaml:"format" json:"format"`
}

// ObservabilityConfig configures OpenTelemetry tracing and metrics export.
//
// A shared Endpoint serves as the default OTLP endpoint for all signals.
// Per-signal Endpoint fields override the shared default when non-empty.
// Empty resolved endpoint = signal disabled (no-op provider, no error).
type ObservabilityConfig struct {
	Endpoint string                     `yaml:"endpoint" json:"endpoint"`
	Protocol string                     `yaml:"protocol" json:"protocol"`
	Tracing  ObservabilityTracingConfig `yaml:"tracing" json:"tracing"`
	Metrics  ObservabilityMetricsConfig `yaml:"metrics" json:"metrics"`
}

// ObservabilityTracingConfig configures the tracing signal.
type ObservabilityTracingConfig struct {
	Endpoint   string  `yaml:"endpoint" json:"endpoint"`
	Protocol   string  `yaml:"protocol" json:"protocol"`
	SampleRate float64 `yaml:"sample_rate" json:"sample_rate"`
}

// ObservabilityMetricsConfig configures the metrics signal.
type ObservabilityMetricsConfig struct {
	Endpoint string `yaml:"endpoint" json:"endpoint"`
	Protocol string `yaml:"protocol" json:"protocol"`
	Interval string `yaml:"interval" json:"interval"`
}

// CatalogConfig configures the catalog parsing and structuring pipeline.
type CatalogConfig struct {
	Structuring StructuringConfig `yaml:"structuring" json:"structuring"`
}

// StructuringConfig configures document-to-OSCAL structuring behavior.
type StructuringConfig struct {
	SectionPattern     string   `yaml:"section_pattern" json:"section_pattern"`
	Decompose          bool     `yaml:"decompose" json:"decompose"`
	MinDecomposeWords  int      `yaml:"min_decompose_words" json:"min_decompose_words"`
	FilterByKeywords   bool     `yaml:"filter_by_keywords" json:"filter_by_keywords"`
	Keywords           []string `yaml:"keywords" json:"keywords"`
	ChunkChars         int      `yaml:"chunk_chars" json:"chunk_chars"`
	MaxValidationChars int      `yaml:"max_validation_chars" json:"max_validation_chars"`
	AllowedFormats     []string `yaml:"allowed_formats" json:"allowed_formats"`
	MaxHeadingRepeats  int      `yaml:"max_heading_repeats" json:"max_heading_repeats"`
}

// AttestationConfig configures in-toto attestation generation and verification.
//
// FIPS mode is not configured here. Attestation FIPS enforcement is derived
// from tls.fips.enabled -- a single deployment-wide posture. The service layer
// reads TLSConfig.FIPS.Enabled and passes attestation.WithFIPSMode() accordingly.
type AttestationConfig struct {
	Enabled           bool                           `yaml:"enabled" json:"enabled"`
	PrivateKeyPath    string                         `yaml:"private_key_path" json:"private_key_path"`
	PublicKeyPath     string                         `yaml:"public_key_path" json:"public_key_path"`
	ExpiryDuration    time.Duration                  `yaml:"expiry_duration" json:"expiry_duration"`
	IncludeByProducts bool                           `yaml:"include_byproducts" json:"include_byproducts"`
	TenantOverrides   map[string]AttestationOverride `yaml:"tenant_overrides" json:"tenant_overrides"`
}

// AttestationOverride allows per-tenant attestation settings.
// Nil pointer fields inherit the global AttestationConfig value.
type AttestationOverride struct {
	Enabled           *bool          `yaml:"enabled" json:"enabled"`
	PrivateKeyPath    *string        `yaml:"private_key_path" json:"private_key_path"`
	PublicKeyPath     *string        `yaml:"public_key_path" json:"public_key_path"`
	ExpiryDuration    *time.Duration `yaml:"expiry_duration" json:"expiry_duration"`
	IncludeByProducts *bool          `yaml:"include_byproducts" json:"include_byproducts"`
}

// AttestationTenantConfig holds the fully resolved attestation settings for a tenant.
// Returned by ForTenant after applying per-tenant overrides to global defaults.
type AttestationTenantConfig struct {
	Enabled           bool
	PrivateKeyPath    string
	PublicKeyPath     string
	ExpiryDuration    time.Duration
	IncludeByProducts bool
}

// ForTenant returns the effective attestation settings for a tenant.
// Fields set in TenantOverrides take precedence; nil fields inherit global values.
func (a *AttestationConfig) ForTenant(tenantID string) AttestationTenantConfig {
	tc := AttestationTenantConfig{
		Enabled:           a.Enabled,
		PrivateKeyPath:    a.PrivateKeyPath,
		PublicKeyPath:     a.PublicKeyPath,
		ExpiryDuration:    a.ExpiryDuration,
		IncludeByProducts: a.IncludeByProducts,
	}
	if override, ok := a.TenantOverrides[tenantID]; ok {
		if override.Enabled != nil {
			tc.Enabled = *override.Enabled
		}
		if override.PrivateKeyPath != nil {
			tc.PrivateKeyPath = *override.PrivateKeyPath
		}
		if override.PublicKeyPath != nil {
			tc.PublicKeyPath = *override.PublicKeyPath
		}
		if override.ExpiryDuration != nil {
			tc.ExpiryDuration = *override.ExpiryDuration
		}
		if override.IncludeByProducts != nil {
			tc.IncludeByProducts = *override.IncludeByProducts
		}
	}
	return tc
}

// PromptConfig configures prompt template resolution and rendering.
type PromptConfig struct {
	CaptureContent  bool                      `yaml:"capture_content" json:"capture_content"`
	AllowCommands   bool                      `yaml:"allow_commands" json:"allow_commands"`
	LayerPaths      []string                  `yaml:"layer_paths" json:"layer_paths"`
	Layers          PromptLayerConfig         `yaml:"layers" json:"layers"`
	TenantOverrides map[string]PromptOverride `yaml:"tenant_overrides" json:"tenant_overrides"`
}

// PromptLayerConfig controls the prompt layer stack.
type PromptLayerConfig struct {
	Enabled bool               `yaml:"enabled" json:"enabled"`
	Order   []PromptLayerEntry `yaml:"order" json:"order"`
}

// PromptLayerEntry configures a single layer in the prompt resolution stack.
type PromptLayerEntry struct {
	ID            string `yaml:"id" json:"id"`
	Merge         string `yaml:"merge" json:"merge"`
	SliceStrategy string `yaml:"slice_strategy" json:"slice_strategy"`
}

// PromptOverride allows per-tenant prompt settings.
// Nil pointer fields inherit the global PromptConfig value.
type PromptOverride struct {
	CaptureContent *bool `yaml:"capture_content" json:"capture_content"`
	AllowCommands  *bool `yaml:"allow_commands" json:"allow_commands"`
}

// PromptTenantConfig holds the fully resolved prompt settings for a tenant.
type PromptTenantConfig struct {
	CaptureContent bool
	AllowCommands  bool
}

// ForTenant returns the effective prompt settings for a tenant.
// Fields set in TenantOverrides take precedence; nil fields inherit global values.
func (p *PromptConfig) ForTenant(tenantID string) PromptTenantConfig {
	tc := PromptTenantConfig{
		CaptureContent: p.CaptureContent,
		AllowCommands:  p.AllowCommands,
	}
	if override, ok := p.TenantOverrides[tenantID]; ok {
		if override.CaptureContent != nil {
			tc.CaptureContent = *override.CaptureContent
		}
		if override.AllowCommands != nil {
			tc.AllowCommands = *override.AllowCommands
		}
	}
	return tc
}

// AnalysisConfig configures the analysis service.
type AnalysisConfig struct {
	Engine         EngineConfig         `yaml:"engine" json:"engine"`
	Classification ClassificationConfig `yaml:"classification" json:"classification"`
	Embedding      EmbeddingConfig      `yaml:"embedding" json:"embedding"`
	Relationship   RelationshipConfig   `yaml:"relationship" json:"relationship"`
	Candidates     CandidateConfig      `yaml:"candidates" json:"candidates"`
	Requires       RequiresConfig       `yaml:"requires" json:"requires"`
	Artifacts      ArtifactsConfig      `yaml:"artifacts" json:"artifacts"`
}

// EngineConfig configures the analysis engine orchestrator.
type EngineConfig struct {
	TaskTimeout  time.Duration `yaml:"task_timeout" json:"task_timeout"`
	MaxRetries   int           `yaml:"max_retries" json:"max_retries"`
	RetryBackoff time.Duration `yaml:"retry_backoff" json:"retry_backoff"`
}

// Validate checks EngineConfig for consistency and required fields.
// Returns ErrInvalidConfig on validation failure.
func (c *EngineConfig) Validate() error {
	if c.TaskTimeout <= 0 || c.TaskTimeout > 30*time.Minute {
		return fmt.Errorf("analysis.engine.task_timeout %s must be in range (0, 30m]: %w",
			c.TaskTimeout, ErrInvalidConfig)
	}
	if c.MaxRetries < 0 || c.MaxRetries > 10 {
		return fmt.Errorf("analysis.engine.max_retries %d must be in range [0, 10]: %w",
			c.MaxRetries, ErrInvalidConfig)
	}
	if c.RetryBackoff < 0 || c.RetryBackoff > 5*time.Minute {
		return fmt.Errorf("analysis.engine.retry_backoff %s must be in range [0, 5m]: %w",
			c.RetryBackoff, ErrInvalidConfig)
	}
	return nil
}

// ClassificationConfig configures the classification analyzer.
type ClassificationConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Model is the LLM model to use for classification. Empty string inherits
	// from LLMConfig.DefaultModel at the service layer.
	Model         string  `yaml:"model" json:"model"`
	MaxTextLength int     `yaml:"max_text_length" json:"max_text_length"`
	Temperature   float64 `yaml:"temperature" json:"temperature"`
	MaxTokens     int     `yaml:"max_tokens" json:"max_tokens"`
}

// EmbeddingConfig configures the embedding analyzer.
type EmbeddingConfig struct {
	Enabled   bool     `yaml:"enabled" json:"enabled"`
	Models    []string `yaml:"models" json:"models"`         // Embedding model names; must be non-empty when enabled
	MaxChars  int      `yaml:"max_chars" json:"max_chars"`   // Max runes per control text before truncation; 0 = no limit
	BatchSize int      `yaml:"batch_size" json:"batch_size"` // Controls per LLM embedding batch call; must be positive
}

// RelationshipConfig configures relationship analysis parameters.
type RelationshipConfig struct {
	Enabled             bool     `yaml:"enabled" json:"enabled"`
	Models              []string `yaml:"models" json:"models"`                             // LLM models for panel voting; must be non-empty when enabled
	TopK                int      `yaml:"top_k" json:"top_k"`                               // Number of most-similar control pairs to retain; must be positive
	MaxSourceChars      int      `yaml:"max_source_chars" json:"max_source_chars"`         // Max runes for source control text; must be positive
	MaxTargetChars      int      `yaml:"max_target_chars" json:"max_target_chars"`         // Max runes for target control text; must be positive
	MaxTokens           int      `yaml:"max_tokens" json:"max_tokens"`                     // Max tokens for LLM response; must be positive
	SamplesPerModel     int      `yaml:"samples_per_model" json:"samples_per_model"`       // Votes per model per pair; must be positive
	SamplingTemperature float64  `yaml:"sampling_temperature" json:"sampling_temperature"` // Temperature for multi-sample; [0.0, 2.0]
	ActionableTypes     []string `yaml:"actionable_types" json:"actionable_types"`         // Relationship types counted for coverage
}

// CandidateConfig configures candidate generation for prerequisite detection.
type CandidateConfig struct {
	Generators []CandidateGeneratorEntry `yaml:"generators" json:"generators"` // Ordered list of candidate generators
}

// CandidateGeneratorEntry configures a single candidate generator.
type CandidateGeneratorEntry struct {
	Name    string                 `yaml:"name" json:"name"`       // Generator name (e.g., "semantic", "keyword", "level")
	Enabled bool                   `yaml:"enabled" json:"enabled"` // Enable this generator
	Weight  float64                `yaml:"weight" json:"weight"`   // Weight for aggregation; default 1.0 if 0
	Config  map[string]interface{} `yaml:"config" json:"config"`   // Generator-specific configuration
}

// RequiresConfig configures the requires analyzer for prerequisite detection.
type RequiresConfig struct {
	Enabled             bool     `yaml:"enabled" json:"enabled"`
	Models              []string `yaml:"models" json:"models"`                             // LLM models for panel voting; must be non-empty when enabled
	SamplesPerModel     int      `yaml:"samples_per_model" json:"samples_per_model"`       // Votes per model per pair; must be positive and odd unless allow_even_samples=true
	AllowEvenSamples    bool     `yaml:"allow_even_samples" json:"allow_even_samples"`     // Allow even samples_per_model (default: false, requires odd for tie-breaking)
	SamplingTemperature float64  `yaml:"sampling_temperature" json:"sampling_temperature"` // Temperature for multi-sample; [0.0, 2.0]
	MaxTokens           int      `yaml:"max_tokens" json:"max_tokens"`                     // Max tokens for LLM response; must be positive
	ConsensusThreshold  float64  `yaml:"consensus_threshold" json:"consensus_threshold"`   // Minimum confidence fraction [0.5, 1.0]
	MaxErrorRate        float64  `yaml:"max_error_rate" json:"max_error_rate"`             // Maximum fraction of votes that can fail [0.0, 1.0]
	MaxSourceChars      int      `yaml:"max_source_chars" json:"max_source_chars"`         // Max runes for source control text; must be positive
	MaxTargetChars      int      `yaml:"max_target_chars" json:"max_target_chars"`         // Max runes for target control text; must be positive
}

// Validate checks RequiresConfig for consistency and required fields.
// Returns ErrInvalidConfig on validation failure.
func (c *RequiresConfig) Validate() error {
	if !c.Enabled {
		return nil
	}

	if len(c.Models) == 0 {
		return fmt.Errorf("analysis.requires.models must not be empty when enabled: %w", ErrInvalidConfig)
	}

	if c.SamplesPerModel <= 0 {
		return fmt.Errorf("analysis.requires.samples_per_model %d must be positive: %w",
			c.SamplesPerModel, ErrInvalidConfig)
	}

	if !c.AllowEvenSamples && c.SamplesPerModel%2 == 0 {
		return fmt.Errorf("analysis.requires.samples_per_model %d must be odd unless allow_even_samples=true: %w",
			c.SamplesPerModel, ErrInvalidConfig)
	}

	if c.ConsensusThreshold < 0.5 || c.ConsensusThreshold > 1.0 {
		return fmt.Errorf("analysis.requires.consensus_threshold %g must be in range [0.5, 1.0]: %w",
			c.ConsensusThreshold, ErrInvalidConfig)
	}

	if c.MaxErrorRate < 0.0 || c.MaxErrorRate > 1.0 {
		return fmt.Errorf("analysis.requires.max_error_rate %g must be in range [0.0, 1.0]: %w",
			c.MaxErrorRate, ErrInvalidConfig)
	}

	if c.MaxSourceChars <= 0 {
		return fmt.Errorf("analysis.requires.max_source_chars %d must be positive: %w",
			c.MaxSourceChars, ErrInvalidConfig)
	}

	if c.MaxTargetChars <= 0 {
		return fmt.Errorf("analysis.requires.max_target_chars %d must be positive: %w",
			c.MaxTargetChars, ErrInvalidConfig)
	}

	if c.MaxTokens <= 0 {
		return fmt.Errorf("analysis.requires.max_tokens %d must be positive: %w",
			c.MaxTokens, ErrInvalidConfig)
	}

	if c.SamplingTemperature < 0.0 || c.SamplingTemperature > 2.0 {
		return fmt.Errorf("analysis.requires.sampling_temperature %g must be in range [0.0, 2.0]: %w",
			c.SamplingTemperature, ErrInvalidConfig)
	}

	return nil
}

// ArtifactsConfig configures the artifacts analyzer for observable artifact extraction.
type ArtifactsConfig struct {
	Enabled             bool     `yaml:"enabled" json:"enabled"`
	Models              []string `yaml:"models" json:"models"`                             // LLM models for panel voting; must be non-empty when enabled
	SamplesPerModel     int      `yaml:"samples_per_model" json:"samples_per_model"`       // Votes per model per control; must be positive
	SamplingTemperature float64  `yaml:"sampling_temperature" json:"sampling_temperature"` // Temperature for multi-sample; [0.0, 2.0]
	MaxTokens           int      `yaml:"max_tokens" json:"max_tokens"`                     // Max tokens for LLM response; must be positive
	MaxTextChars        int      `yaml:"max_text_chars" json:"max_text_chars"`             // Max runes for requirement text; must be positive
	FuzzyThreshold      float64  `yaml:"fuzzy_threshold" json:"fuzzy_threshold"`           // Token-set overlap threshold (0.0, 1.0]; default 0.6
}

// Validate checks ArtifactsConfig for consistency and required fields.
func (c *ArtifactsConfig) Validate() error {
	if !c.Enabled {
		return nil
	}

	if len(c.Models) == 0 {
		return fmt.Errorf("analysis.artifacts.models must not be empty when enabled: %w", ErrInvalidConfig)
	}

	if c.SamplesPerModel <= 0 {
		return fmt.Errorf("analysis.artifacts.samples_per_model %d must be positive: %w",
			c.SamplesPerModel, ErrInvalidConfig)
	}

	if c.MaxTokens <= 0 {
		return fmt.Errorf("analysis.artifacts.max_tokens %d must be positive: %w",
			c.MaxTokens, ErrInvalidConfig)
	}

	if c.MaxTextChars <= 0 {
		return fmt.Errorf("analysis.artifacts.max_text_chars %d must be positive: %w",
			c.MaxTextChars, ErrInvalidConfig)
	}

	if c.SamplingTemperature < 0.0 || c.SamplingTemperature > 2.0 {
		return fmt.Errorf("analysis.artifacts.sampling_temperature %g must be in range [0.0, 2.0]: %w",
			c.SamplingTemperature, ErrInvalidConfig)
	}

	if c.FuzzyThreshold <= 0.0 || c.FuzzyThreshold > 1.0 {
		return fmt.Errorf("analysis.artifacts.fuzzy_threshold %g must be in range (0.0, 1.0]: %w",
			c.FuzzyThreshold, ErrInvalidConfig)
	}

	return nil
}

// WorkerConfig configures the LLM worker service.
//
// Drain timeout: To bound Stop() duration, set a NATS drain timeout on the
// underlying connection using natsbus.ClientConfig.DrainTimeout before
// creating the worker. The worker itself does not expose a separate timeout
// because the NATS client-level setting applies to all drain operations.
type WorkerConfig struct {
	// QueueGroup is the NATS queue group name. All workers in the same group
	// receive round-robin task distribution. Defaults to "llm-workers".
	QueueGroup string `yaml:"queue_group" json:"queue_group"`

	// LLM holds the global LLM configuration with optional per-tenant overrides.
	// Wired from the top-level LLMConfig at the service layer; not duplicated here.
	// Set by the service binary via ServiceConfig().Worker.LLM = cfg.LLM.
	LLM LLMConfig `yaml:"-" json:"-"`
}

// Validate checks WorkerConfig for consistency and required fields.
// Returns ErrInvalidConfig on validation failure.
func (c *WorkerConfig) Validate() error {
	if strings.TrimSpace(c.QueueGroup) == "" && c.QueueGroup != "" {
		return fmt.Errorf("worker.queue_group %q contains only whitespace: use a non-empty group name or leave empty to use the default %q: %w",
			c.QueueGroup, "llm-workers", ErrInvalidConfig)
	}
	return nil
}

// PipelineConfig configures the pipeline orchestration service.
type PipelineConfig struct {
	MaxConcurrentJobs int           `yaml:"max_concurrent_jobs" json:"max_concurrent_jobs"`
	StageTimeout      time.Duration `yaml:"stage_timeout" json:"stage_timeout"`
}

// Validate checks PipelineConfig for consistency.
func (c *PipelineConfig) Validate() error {
	if c.MaxConcurrentJobs < 0 || c.MaxConcurrentJobs > 100 {
		return fmt.Errorf("pipeline.max_concurrent_jobs %d must be in range [0, 100]: %w",
			c.MaxConcurrentJobs, ErrInvalidConfig)
	}
	if c.StageTimeout < 0 || c.StageTimeout > 2*time.Hour {
		return fmt.Errorf("pipeline.stage_timeout %s must be in range [0, 2h]: %w",
			c.StageTimeout, ErrInvalidConfig)
	}
	return nil
}

// SynthesisConfig configures the synthesis service.
type SynthesisConfig struct {
	Viability             ViabilityConfig              `yaml:"viability" json:"viability"`
	Assessment            AssessmentConfig             `yaml:"assessment" json:"assessment"`
	ConfidenceThreshold   float64                      `yaml:"confidence_threshold" json:"confidence_threshold"`
	MaxMappingsPerControl int                          `yaml:"max_mappings_per_control" json:"max_mappings_per_control"`
	TenantOverrides       map[string]SynthesisOverride `yaml:"tenant_overrides" json:"tenant_overrides"`
}

// ViabilityConfig configures viability computation factors.
type ViabilityConfig struct {
	TypeMismatchFactor float64 `yaml:"type_mismatch_factor" json:"type_mismatch_factor"` // default 0.8
	SkipLevelFactor    float64 `yaml:"skip_level_factor" json:"skip_level_factor"`       // default 0.7
	IntegralToFactor   float64 `yaml:"integral_to_factor" json:"integral_to_factor"`     // default 1.1
}

// AssessmentConfig configures quality assessment thresholds.
type AssessmentConfig struct {
	IQRGood        float64 `yaml:"iqr_good" json:"iqr_good"`               // default 20.0
	IQRPoor        float64 `yaml:"iqr_poor" json:"iqr_poor"`               // default 10.0
	NoRelHigh      float64 `yaml:"no_rel_high" json:"no_rel_high"`         // default 0.97
	NoRelLow       float64 `yaml:"no_rel_low" json:"no_rel_low"`           // default 0.80
	ContestedWarn  float64 `yaml:"contested_warn" json:"contested_warn"`   // default 0.20
	ActionableWarn float64 `yaml:"actionable_warn" json:"actionable_warn"` // default 0.30
}

// SynthesisOverride allows per-tenant synthesis settings.
// Nil pointer fields inherit the global SynthesisConfig value.
type SynthesisOverride struct {
	ConfidenceThreshold   *float64          `yaml:"confidence_threshold" json:"confidence_threshold"`
	MaxMappingsPerControl *int              `yaml:"max_mappings_per_control" json:"max_mappings_per_control"`
	Viability             *ViabilityConfig  `yaml:"viability" json:"viability"`
	Assessment            *AssessmentConfig `yaml:"assessment" json:"assessment"`
}

// SynthesisTenantConfig is the fully-resolved view for a specific tenant.
type SynthesisTenantConfig struct {
	Viability             ViabilityConfig
	Assessment            AssessmentConfig
	ConfidenceThreshold   float64
	MaxMappingsPerControl int
}

// ForTenant resolves the fully-typed view for a specific tenant.
func (c *SynthesisConfig) ForTenant(tenantID string) SynthesisTenantConfig {
	tc := SynthesisTenantConfig{
		Viability:             c.Viability,
		Assessment:            c.Assessment,
		ConfidenceThreshold:   c.ConfidenceThreshold,
		MaxMappingsPerControl: c.MaxMappingsPerControl,
	}
	if override, ok := c.TenantOverrides[tenantID]; ok {
		if override.ConfidenceThreshold != nil {
			tc.ConfidenceThreshold = *override.ConfidenceThreshold
		}
		if override.MaxMappingsPerControl != nil {
			tc.MaxMappingsPerControl = *override.MaxMappingsPerControl
		}
		if override.Viability != nil {
			tc.Viability = *override.Viability
		}
		if override.Assessment != nil {
			tc.Assessment = *override.Assessment
		}
	}
	return tc
}

// DaemonConfig is the derived view for crosscodexd.
type DaemonConfig struct {
	Addr          string
	Workers       int
	LLM           LLMConfig
	Storage       StorageConfig
	TLS           TLSConfig
	Tenants       TenantsConfig
	Database      DatabaseConfig
	NATS          NATSConfig
	Logging       LoggingConfig
	Auth          AuthConfig
	Observability ObservabilityConfig
	Catalog       CatalogConfig
	Attestation   AttestationConfig
	Prompt        PromptConfig
	Analysis      AnalysisConfig
	Worker        WorkerConfig
	Synthesis     SynthesisConfig
	Pipeline      PipelineConfig
}

// ClientConfig is the derived view for the crosscodex CLI.
type ClientConfig struct {
	Output          string        `json:"output"`
	NoColor         bool          `json:"nocolor"`
	Endpoint        string        `json:"endpoint"`
	UploadChunkSize int           `json:"upload_chunk_size"`
	LLM             LLMConfig     `json:"llm"`
	TLS             TLSConfig     `json:"tls"`
	Logging         LoggingConfig `json:"logging"`
	Prompt          PromptConfig  `json:"prompt"`
}

// ServiceConfig returns the daemon-oriented view of this configuration.
func (c *Config) ServiceConfig() DaemonConfig {
	return DaemonConfig{
		Addr:          c.Server.Addr,
		Workers:       c.Server.Workers,
		LLM:           c.LLM,
		Storage:       c.Storage,
		TLS:           c.TLS,
		Tenants:       c.Tenants,
		Database:      c.Database,
		NATS:          c.NATS,
		Logging:       c.Logging,
		Auth:          c.Auth,
		Observability: c.Observability,
		Catalog:       c.Catalog,
		Attestation:   c.Attestation,
		Prompt:        c.Prompt,
		Analysis:      c.Analysis,
		Worker:        WorkerConfig{QueueGroup: c.Worker.QueueGroup, LLM: c.LLM},
		Synthesis:     c.Synthesis,
		Pipeline:      c.Pipeline,
	}
}

// CLIConfig returns the CLI-oriented view of this configuration.
func (c *Config) CLIConfig() ClientConfig {
	return ClientConfig{
		Output:          c.CLI.Output,
		NoColor:         c.CLI.NoColor,
		Endpoint:        c.CLI.Endpoint,
		UploadChunkSize: c.CLI.UploadChunkSize,
		LLM:             c.LLM,
		TLS:             c.TLS,
		Logging:         c.Logging,
		Prompt:          c.Prompt,
	}
}
