package config

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
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
	Graph         GraphConfig         `yaml:"graph"         json:"graph"`
	Synthesis     SynthesisConfig     `yaml:"synthesis"     json:"synthesis"`
	Retention     RetentionConfig     `yaml:"retention"     json:"retention"`
	Backup        BackupConfig        `yaml:"backup"        json:"backup"`
	Role          string              `yaml:"role"          json:"role"`
	Health        HealthConfig        `yaml:"health"        json:"health"`
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
	// Timeout bounds each LLM gateway request, in seconds. Zero means
	// llmclient.DefaultTimeout (30s) for both http and https gateways.
	Timeout int `yaml:"timeout" json:"timeout"`
	// ModelRevisions maps a model alias, as the gateway resolves it, to an
	// operator-declared revision that is part of every result-cache key for
	// the alias (analysis.result_cache). Change it to force misses when the
	// gateway's model name cannot show a change, such as an Ollama re-pull
	// of the same tag. Global-only: the alias mapping lives in the shared
	// gateway, so tenants cannot remap it.
	ModelRevisions map[string]string `yaml:"model_revisions" json:"model_revisions"`
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
	Enabled       bool   `yaml:"enabled" json:"enabled"`
	DefaultTenant string `yaml:"default_tenant" json:"default_tenant"`
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
// Five DSNs support the role security model (see pkg/db/doc.go):
//   - DSN connects as app_user for relational data behind RLS. The daemon
//     refuses to run as any other kind of role (pkg/db.CheckRestrictedRole).
//   - GraphDSN connects as graph_user for AGE cypher queries.
//     graph_user owns per-tenant graph schemas but has no relational access.
//   - PurgeDSN connects as purge_user (member of retention_purge), the only
//     credential the delete-path immutability triggers accept for the
//     sanctioned retention purge. Empty disables DB purge.
//   - TenantAdminDSN connects as tenant_admin for
//     `crosscodexd admin tenant`; the daemon never uses it. Global-only: a
//     tenant cannot administer tenants. Empty makes those commands refuse.
//   - MigrationDSN connects as the schema owner for `crosscodexd migrate`,
//     which applies migrations, sets the restricted roles' passwords and
//     provisions tenants.default_tenant. The daemon never uses it and refuses
//     to run as an owner, superuser or BYPASSRLS role
//     (pkg/db.CheckRestrictedRole). Global-only. Empty makes
//     `crosscodexd migrate` refuse.
type DatabaseConfig struct {
	DSN            string   `yaml:"dsn" json:"dsn"`
	MigrationDSN   string   `yaml:"migration_dsn" json:"migration_dsn"`
	GraphDSN       string   `yaml:"graph_dsn" json:"graph_dsn"`
	PurgeDSN       string   `yaml:"purge_dsn" json:"purge_dsn"`
	TenantAdminDSN string   `yaml:"tenant_admin_dsn" json:"tenant_admin_dsn"`
	Extensions     []string `yaml:"extensions" json:"extensions"`
	MaxConns       int      `yaml:"max_conns" json:"max_conns"`
	SSLMode        string   `yaml:"ssl_mode" json:"ssl_mode"`
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

// HealthConfig configures the dedicated health-check listener used by the
// worker and graph roles (the gateway and all roles already serve /healthz
// via internal/gateway.Server).
type HealthConfig struct {
	Addr string `yaml:"addr" json:"addr"`
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
// When attestation is enabled for a tenant (ForTenant), every pipeline job of
// that tenant is attested or fails: the daemon refuses to start unless each
// enabled scope has a loadable key pair (RequireKeys, then key loading in
// internal/pipeline.NewAttestors). When it is disabled, no signer is built
// and jobs complete without attestation.
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
	// StorageRetries is how many more times the pipeline tries to store an
	// attestation (layout, link or bundle) in object storage after a failed
	// write, before it fails the job. Range [0, 10]. Global-only: storage
	// is shared by every tenant.
	StorageRetries int `yaml:"storage_retries" json:"storage_retries"`
	// StorageRetryDelay is the wait before each of those retries. Range
	// [0, 1m], so one write adds at most 10m of retry delay. Global-only.
	StorageRetryDelay time.Duration `yaml:"storage_retry_delay" json:"storage_retry_delay"`
}

// RequireKeys reports, as one FieldError per scope, every scope (the global
// settings, or a tenant override) that enables attestation without both
// key paths. A tenant that only inherits a global problem is not reported
// again.
//
// crosscodexd calls this, through internal/pipeline.NewAttestors, in the
// roles that run the pipeline. Config loading does not: the crosscodex CLI
// and the other roles load the same configuration and never sign.
func (a *AttestationConfig) RequireKeys() error {
	var p problems
	const remedy = "the pipeline signs each job's in-toto attestation with this key pair. " +
		"Set both paths to an ECDSA key pair, or set %s: false to run jobs without attestation: %w"
	globalMissing := a.Enabled && (a.PrivateKeyPath == "" || a.PublicKeyPath == "")
	if globalMissing {
		p.add("attestation.private_key_path",
			"attestation.enabled is true but attestation.private_key_path and attestation.public_key_path are not both set: "+remedy,
			"attestation.enabled", ErrMissingRequired)
	}
	for _, tenantID := range slices.Sorted(maps.Keys(a.TenantOverrides)) {
		override := a.TenantOverrides[tenantID]
		tc := a.ForTenant(tenantID)
		if !tc.Enabled || (tc.PrivateKeyPath != "" && tc.PublicKeyPath != "") {
			continue
		}
		ownsProblem := override.Enabled != nil || override.PrivateKeyPath != nil || override.PublicKeyPath != nil
		if globalMissing && !ownsProblem {
			continue
		}
		prefix := mapKeyPath("attestation.tenant_overrides", tenantID)
		p.add(prefix+".private_key_path",
			"attestation is enabled for tenant %q but it has no key pair: set %s.private_key_path and %s.public_key_path, "+
				"or the global attestation.private_key_path and attestation.public_key_path it inherits; "+remedy,
			tenantID, prefix, prefix, prefix+".enabled", ErrMissingRequired)
	}
	return p.err()
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
	Engine               EngineConfig               `yaml:"engine" json:"engine"`
	Classification       ClassificationConfig       `yaml:"classification" json:"classification"`
	Embedding            EmbeddingConfig            `yaml:"embedding" json:"embedding"`
	Relationship         RelationshipConfig         `yaml:"relationship" json:"relationship"`
	Candidates           CandidateConfig            `yaml:"candidates" json:"candidates"`
	Requires             RequiresConfig             `yaml:"requires" json:"requires"`
	Artifacts            ArtifactsConfig            `yaml:"artifacts" json:"artifacts"`
	ArtifactAdjudication ArtifactAdjudicationConfig `yaml:"artifact_adjudication" json:"artifact_adjudication"`
	ResultCache          ResultCacheConfig          `yaml:"result_cache" json:"result_cache"`
}

// ResultCacheConfig configures LLM result reuse across a tenant's jobs
// (#243). Every field is tenant-overridable.
type ResultCacheConfig struct {
	// Enabled makes a job look its LLM requests up before sending them and
	// store their results. False is the kill switch: jobs neither read nor
	// write the cache. A job started with --no-cache skips lookups but
	// still stores.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// LookupBatchSize is how many cache keys one lookup query reads, 1 to
	// 10000.
	LookupBatchSize int `yaml:"lookup_batch_size" json:"lookup_batch_size"`
	// TenantOverrides maps tenant IDs (pkg/tenant.ValidateTenantID) to
	// per-tenant settings. Nil fields inherit the global value.
	TenantOverrides map[string]ResultCacheOverride `yaml:"tenant_overrides" json:"tenant_overrides"`
}

// ResultCacheOverride is one tenant's result cache settings.
type ResultCacheOverride struct {
	Enabled         *bool `yaml:"enabled" json:"enabled"`
	LookupBatchSize *int  `yaml:"lookup_batch_size" json:"lookup_batch_size"`
}

// ResultCacheTenantConfig holds the resolved result cache settings for a
// tenant.
type ResultCacheTenantConfig struct {
	Enabled         bool
	LookupBatchSize int
}

// ForTenant returns the effective result cache settings for a tenant.
// Fields set in TenantOverrides take precedence; nil fields inherit global
// values.
func (c *ResultCacheConfig) ForTenant(tenantID string) ResultCacheTenantConfig {
	tc := ResultCacheTenantConfig{Enabled: c.Enabled, LookupBatchSize: c.LookupBatchSize}
	if override, ok := c.TenantOverrides[tenantID]; ok {
		if override.Enabled != nil {
			tc.Enabled = *override.Enabled
		}
		if override.LookupBatchSize != nil {
			tc.LookupBatchSize = *override.LookupBatchSize
		}
	}
	return tc
}

// EngineConfig configures the analysis engine orchestrator.
type EngineConfig struct {
	// TaskTimeout is how long an analyzer stage waits while the whole LLM
	// worker pool is silent before it fails. Any task result of the stage,
	// or any worker progress tick, resets the timer. A worker ticks after
	// any task result it publishes, success or error (including an error
	// that will be retried), for any stage, job, tenant or daemon; it sends
	// no tick when the result publish fails or the task is dropped before
	// any result. It does not cap the stage's total duration: a stage of
	// thousands of LLM tasks runs as long as the pool keeps finishing tasks,
	// even while it works through another stage's queue. So it does not
	// catch a lost task while the pool stays busy; pipeline.stage_timeout
	// does, and bounds how long ticks can delay this timeout. Range (0, 30m].
	TaskTimeout  time.Duration `yaml:"task_timeout" json:"task_timeout"`
	MaxRetries   int           `yaml:"max_retries" json:"max_retries"`
	RetryBackoff time.Duration `yaml:"retry_backoff" json:"retry_backoff"`
}

// Validate checks EngineConfig for consistency and required fields.
// Returns ErrInvalidConfig on validation failure.
func (c *EngineConfig) Validate() error { return c.validate(nil) }

func (c *EngineConfig) validate(tracker *sourceTracker) error {
	var p problems
	if c.TaskTimeout <= 0 || c.TaskTimeout > 30*time.Minute {
		p.add("analysis.engine.task_timeout", "analysis.engine.task_timeout %s%s must be in range (0, 30m]: %w",
			c.TaskTimeout, formatSource(tracker, "analysis.engine.task_timeout"), ErrInvalidConfig)
	}
	if c.MaxRetries < 0 || c.MaxRetries > 10 {
		p.add("analysis.engine.max_retries", "analysis.engine.max_retries %d%s must be in range [0, 10]: %w",
			c.MaxRetries, formatSource(tracker, "analysis.engine.max_retries"), ErrInvalidConfig)
	}
	if c.RetryBackoff < 0 || c.RetryBackoff > 5*time.Minute {
		p.add("analysis.engine.retry_backoff", "analysis.engine.retry_backoff %s%s must be in range [0, 5m]: %w",
			c.RetryBackoff, formatSource(tracker, "analysis.engine.retry_backoff"), ErrInvalidConfig)
	}
	return p.err()
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
	// MinEmbeddingCoverage is the minimum fraction [0.0, 1.0] of a job's
	// controls that must have a corresponding embeddings row before candidate
	// generation proceeds. Below this, candidate generation fails the stage
	// loudly instead of silently producing near-empty candidates. Default: 0.8.
	MinEmbeddingCoverage float64 `yaml:"min_embedding_coverage" json:"min_embedding_coverage"`
	// EmbedModel is the embeddings model CandidateGenerator.Generate reads the
	// similarity matrix for. Must be one of EmbeddingConfig.Models (validated
	// in validateAnalysis) — a mismatch would silently read an empty
	// similarity matrix instead of failing loudly.
	EmbedModel string `yaml:"embed_model" json:"embed_model"`
}

// Validate checks CandidateConfig for consistency and required fields.
// Returns ErrInvalidConfig on validation failure.
func (c *CandidateConfig) Validate() error { return c.validate(nil) }

func (c *CandidateConfig) validate(tracker *sourceTracker) error {
	var p problems
	if math.IsNaN(c.MinEmbeddingCoverage) || c.MinEmbeddingCoverage < 0.0 || c.MinEmbeddingCoverage > 1.0 {
		p.add("analysis.candidates.min_embedding_coverage", "analysis.candidates.min_embedding_coverage %g%s must be in range [0.0, 1.0]: %w",
			c.MinEmbeddingCoverage, formatSource(tracker, "analysis.candidates.min_embedding_coverage"), ErrInvalidConfig)
	}
	if c.EmbedModel == "" {
		p.add("analysis.candidates.embed_model", "analysis.candidates.embed_model%s must not be empty: %w",
			formatSource(tracker, "analysis.candidates.embed_model"), ErrInvalidConfig)
	}
	return p.err()
}

// CandidateGeneratorEntry configures a single candidate generator.
type CandidateGeneratorEntry struct {
	Name    string                 `yaml:"name" json:"name"`       // Generator name (e.g., "semantic", "keyword", "level")
	Enabled bool                   `yaml:"enabled" json:"enabled"` // Enable this generator
	Weight  float64                `yaml:"weight" json:"weight"`   // Weight for aggregation, [0.0, 1.0]; 0 means the default 1.0
	Config  map[string]interface{} `yaml:"config" json:"config"`   // Generator-specific settings; allowed keys per generator in candidateGeneratorSettings (validate.go)
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
func (c *RequiresConfig) Validate() error { return c.validate(nil) }

func (c *RequiresConfig) validate(tracker *sourceTracker) error {
	if !c.Enabled {
		return nil
	}
	const pfx = "analysis.requires."
	var p problems
	if len(c.Models) == 0 {
		p.add(pfx+"models", "analysis.requires.models%s must not be empty when enabled: %w",
			formatSource(tracker, pfx+"models"), ErrInvalidConfig)
	}
	if c.SamplesPerModel <= 0 {
		p.add(pfx+"samples_per_model", "analysis.requires.samples_per_model %d%s must be positive: %w",
			c.SamplesPerModel, formatSource(tracker, pfx+"samples_per_model"), ErrInvalidConfig)
	} else if !c.AllowEvenSamples && c.SamplesPerModel%2 == 0 {
		p.add(pfx+"samples_per_model", "analysis.requires.samples_per_model %d%s must be odd unless allow_even_samples=true: %w",
			c.SamplesPerModel, formatSource(tracker, pfx+"samples_per_model"), ErrInvalidConfig)
	}
	if math.IsNaN(c.ConsensusThreshold) || c.ConsensusThreshold < 0.5 || c.ConsensusThreshold > 1.0 {
		p.add(pfx+"consensus_threshold", "analysis.requires.consensus_threshold %g%s must be in range [0.5, 1.0]: %w",
			c.ConsensusThreshold, formatSource(tracker, pfx+"consensus_threshold"), ErrInvalidConfig)
	}
	if math.IsNaN(c.MaxErrorRate) || c.MaxErrorRate < 0.0 || c.MaxErrorRate > 1.0 {
		p.add(pfx+"max_error_rate", "analysis.requires.max_error_rate %g%s must be in range [0.0, 1.0]: %w",
			c.MaxErrorRate, formatSource(tracker, pfx+"max_error_rate"), ErrInvalidConfig)
	}
	if c.MaxSourceChars <= 0 {
		p.add(pfx+"max_source_chars", "analysis.requires.max_source_chars %d%s must be positive: %w",
			c.MaxSourceChars, formatSource(tracker, pfx+"max_source_chars"), ErrInvalidConfig)
	}
	if c.MaxTargetChars <= 0 {
		p.add(pfx+"max_target_chars", "analysis.requires.max_target_chars %d%s must be positive: %w",
			c.MaxTargetChars, formatSource(tracker, pfx+"max_target_chars"), ErrInvalidConfig)
	}
	if c.MaxTokens <= 0 {
		p.add(pfx+"max_tokens", "analysis.requires.max_tokens %d%s must be positive: %w",
			c.MaxTokens, formatSource(tracker, pfx+"max_tokens"), ErrInvalidConfig)
	}
	if math.IsNaN(c.SamplingTemperature) || c.SamplingTemperature < 0.0 || c.SamplingTemperature > 2.0 {
		p.add(pfx+"sampling_temperature", "analysis.requires.sampling_temperature %g%s must be in range [0.0, 2.0]: %w",
			c.SamplingTemperature, formatSource(tracker, pfx+"sampling_temperature"), ErrInvalidConfig)
	}
	return p.err()
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
func (c *ArtifactsConfig) Validate() error { return c.validate(nil) }

func (c *ArtifactsConfig) validate(tracker *sourceTracker) error {
	if !c.Enabled {
		return nil
	}
	const pfx = "analysis.artifacts."
	var p problems
	if len(c.Models) == 0 {
		p.add(pfx+"models", "analysis.artifacts.models%s must not be empty when enabled: %w",
			formatSource(tracker, pfx+"models"), ErrInvalidConfig)
	}
	if c.SamplesPerModel <= 0 {
		p.add(pfx+"samples_per_model", "analysis.artifacts.samples_per_model %d%s must be positive: %w",
			c.SamplesPerModel, formatSource(tracker, pfx+"samples_per_model"), ErrInvalidConfig)
	}
	if c.MaxTokens <= 0 {
		p.add(pfx+"max_tokens", "analysis.artifacts.max_tokens %d%s must be positive: %w",
			c.MaxTokens, formatSource(tracker, pfx+"max_tokens"), ErrInvalidConfig)
	}
	if c.MaxTextChars <= 0 {
		p.add(pfx+"max_text_chars", "analysis.artifacts.max_text_chars %d%s must be positive: %w",
			c.MaxTextChars, formatSource(tracker, pfx+"max_text_chars"), ErrInvalidConfig)
	}
	if math.IsNaN(c.SamplingTemperature) || c.SamplingTemperature < 0.0 || c.SamplingTemperature > 2.0 {
		p.add(pfx+"sampling_temperature", "analysis.artifacts.sampling_temperature %g%s must be in range [0.0, 2.0]: %w",
			c.SamplingTemperature, formatSource(tracker, pfx+"sampling_temperature"), ErrInvalidConfig)
	}
	if math.IsNaN(c.FuzzyThreshold) || c.FuzzyThreshold <= 0.0 || c.FuzzyThreshold > 1.0 {
		p.add(pfx+"fuzzy_threshold", "analysis.artifacts.fuzzy_threshold %g%s must be in range (0.0, 1.0]: %w",
			c.FuzzyThreshold, formatSource(tracker, pfx+"fuzzy_threshold"), ErrInvalidConfig)
	}
	return p.err()
}

// ArtifactAdjudicationConfig configures `crosscodexd admin adjudicate
// artifacts`, the LLM panel that confirms or rejects the reconciler's
// token-overlap SAME_AS candidates.
type ArtifactAdjudicationConfig struct {
	Enabled             bool          `yaml:"enabled" json:"enabled"`                           // The command refuses to run unless true
	Models              []string      `yaml:"models" json:"models"`                             // LLM models on the panel; must be non-empty when enabled
	SamplesPerModel     int           `yaml:"samples_per_model" json:"samples_per_model"`       // Votes per model per pair; positive and odd unless allow_even_samples
	AllowEvenSamples    bool          `yaml:"allow_even_samples" json:"allow_even_samples"`     // Allow an even samples_per_model
	SamplingTemperature float64       `yaml:"sampling_temperature" json:"sampling_temperature"` // [0.0, 2.0]; 0 is used when samples_per_model is 1
	MaxTokens           int           `yaml:"max_tokens" json:"max_tokens"`                     // Max tokens per reply; must be positive
	ConsensusThreshold  float64       `yaml:"consensus_threshold" json:"consensus_threshold"`   // Minimum consensus fraction for a verdict [0.5, 1.0]
	MaxErrorRate        float64       `yaml:"max_error_rate" json:"max_error_rate"`             // Max fraction of failed votes [0.0, 1.0]; 0 disables the check
	MaxAttempts         int           `yaml:"max_attempts" json:"max_attempts"`                 // Failed panels before a pair is abandoned; must be positive
	LeaseDuration       time.Duration `yaml:"lease_duration" json:"lease_duration"`             // How long a run holds each leased pair, and each verdict it projects; must be positive, even when disabled: decide uses it as the projection claim TTL
	LeaseBatch          int           `yaml:"lease_batch" json:"lease_batch"`                   // Pairs leased per store round trip; must be positive
	PanelConcurrency    int           `yaml:"panel_concurrency" json:"panel_concurrency"`       // Completions in flight per pair; must be positive
	RetryBaseDelay      time.Duration `yaml:"retry_base_delay" json:"retry_base_delay"`         // Wait after a pair's first failed panel, doubling per failure; must be positive
	RetryMaxDelay       time.Duration `yaml:"retry_max_delay" json:"retry_max_delay"`           // Cap on the retry wait; must be at least retry_base_delay
}

// Validate checks ArtifactAdjudicationConfig. Only lease_duration is checked
// when it is disabled: `admin adjudicate artifacts decide` runs either way
// and uses it as the projection claim TTL. Errors wrap ErrInvalidConfig.
func (c *ArtifactAdjudicationConfig) Validate() error { return c.validate(nil) }

func (c *ArtifactAdjudicationConfig) validate(tracker *sourceTracker) error {
	const pfx = "analysis.artifact_adjudication."
	var p problems
	if c.LeaseDuration <= 0 {
		p.add(pfx+"lease_duration", "analysis.artifact_adjudication.lease_duration %s%s must be positive (e.g. 15m): %w",
			c.LeaseDuration, formatSource(tracker, pfx+"lease_duration"), ErrInvalidConfig)
	}
	if !c.Enabled {
		return p.err()
	}
	if len(c.Models) == 0 {
		p.add(pfx+"models", "analysis.artifact_adjudication.models%s must not be empty when enabled: %w",
			formatSource(tracker, pfx+"models"), ErrInvalidConfig)
	}
	if c.SamplesPerModel <= 0 {
		p.add(pfx+"samples_per_model", "analysis.artifact_adjudication.samples_per_model %d%s must be positive: %w",
			c.SamplesPerModel, formatSource(tracker, pfx+"samples_per_model"), ErrInvalidConfig)
	} else if !c.AllowEvenSamples && c.SamplesPerModel%2 == 0 {
		p.add(pfx+"samples_per_model", "analysis.artifact_adjudication.samples_per_model %d%s must be odd unless allow_even_samples=true: %w",
			c.SamplesPerModel, formatSource(tracker, pfx+"samples_per_model"), ErrInvalidConfig)
	}
	if math.IsNaN(c.SamplingTemperature) || c.SamplingTemperature < 0.0 || c.SamplingTemperature > 2.0 {
		p.add(pfx+"sampling_temperature", "analysis.artifact_adjudication.sampling_temperature %g%s must be in range [0.0, 2.0]: %w",
			c.SamplingTemperature, formatSource(tracker, pfx+"sampling_temperature"), ErrInvalidConfig)
	}
	if c.MaxTokens <= 0 {
		p.add(pfx+"max_tokens", "analysis.artifact_adjudication.max_tokens %d%s must be positive: %w",
			c.MaxTokens, formatSource(tracker, pfx+"max_tokens"), ErrInvalidConfig)
	}
	if math.IsNaN(c.ConsensusThreshold) || c.ConsensusThreshold < 0.5 || c.ConsensusThreshold > 1.0 {
		p.add(pfx+"consensus_threshold", "analysis.artifact_adjudication.consensus_threshold %g%s must be in range [0.5, 1.0]: %w",
			c.ConsensusThreshold, formatSource(tracker, pfx+"consensus_threshold"), ErrInvalidConfig)
	}
	if math.IsNaN(c.MaxErrorRate) || c.MaxErrorRate < 0.0 || c.MaxErrorRate > 1.0 {
		p.add(pfx+"max_error_rate", "analysis.artifact_adjudication.max_error_rate %g%s must be in range [0.0, 1.0]: %w",
			c.MaxErrorRate, formatSource(tracker, pfx+"max_error_rate"), ErrInvalidConfig)
	}
	if c.MaxAttempts <= 0 {
		p.add(pfx+"max_attempts", "analysis.artifact_adjudication.max_attempts %d%s must be positive: %w",
			c.MaxAttempts, formatSource(tracker, pfx+"max_attempts"), ErrInvalidConfig)
	}
	if c.LeaseBatch <= 0 {
		p.add(pfx+"lease_batch", "analysis.artifact_adjudication.lease_batch %d%s must be positive: %w",
			c.LeaseBatch, formatSource(tracker, pfx+"lease_batch"), ErrInvalidConfig)
	}
	if c.PanelConcurrency <= 0 {
		p.add(pfx+"panel_concurrency", "analysis.artifact_adjudication.panel_concurrency %d%s must be positive: %w",
			c.PanelConcurrency, formatSource(tracker, pfx+"panel_concurrency"), ErrInvalidConfig)
	}
	if c.RetryBaseDelay <= 0 {
		p.add(pfx+"retry_base_delay", "analysis.artifact_adjudication.retry_base_delay %s%s must be positive (e.g. 1m): %w",
			c.RetryBaseDelay, formatSource(tracker, pfx+"retry_base_delay"), ErrInvalidConfig)
	} else if c.RetryMaxDelay < c.RetryBaseDelay {
		// Compared only against a valid base delay, so one mistake is one problem.
		p.add(pfx+"retry_max_delay", "analysis.artifact_adjudication.retry_max_delay %s%s must not be below retry_base_delay %s: %w",
			c.RetryMaxDelay, formatSource(tracker, pfx+"retry_max_delay"), c.RetryBaseDelay, ErrInvalidConfig)
	}
	return p.err()
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

	// MaxAbandonedCollections caps how many abandoned analysis collections
	// each worker remembers, for up to pipeline.stage_timeout each, so it
	// can drop their queued tasks before calling the LLM. When more
	// collections are abandoned within one stage_timeout, the oldest are
	// forgotten and their remaining tasks run and are billed for nothing.
	// An entry costs about 430 bytes with UUID job IDs, so the default 10000
	// is about 4.3 MB and the maximum 100000 about 43 MB (see
	// internal/worker/abandoned.go for the arithmetic). Range [1, 100000]. Global-only: one worker
	// serves every tenant, so the set and its cap are shared.
	MaxAbandonedCollections int `yaml:"max_abandoned_collections" json:"max_abandoned_collections"`

	// LLM holds the global LLM configuration with optional per-tenant overrides.
	// Wired from the top-level LLMConfig at the service layer; not duplicated here.
	// Set by the service binary via ServiceConfig().Worker.LLM = cfg.LLM.
	LLM LLMConfig `yaml:"-" json:"-"`
}

// Validate checks WorkerConfig for consistency and required fields.
// Returns ErrInvalidConfig on validation failure.
func (c *WorkerConfig) Validate() error { return c.validate(nil) }

func (c *WorkerConfig) validate(tracker *sourceTracker) error {
	var p problems
	if strings.TrimSpace(c.QueueGroup) == "" && c.QueueGroup != "" {
		p.add("worker.queue_group", "worker.queue_group %q%s contains only whitespace: use a non-empty group name or leave empty to use the default %q: %w",
			c.QueueGroup, formatSource(tracker, "worker.queue_group"), "llm-workers", ErrInvalidConfig)
	}
	if c.MaxAbandonedCollections < 1 || c.MaxAbandonedCollections > MaxAbandonedCollectionsLimit {
		p.add("worker.max_abandoned_collections", "worker.max_abandoned_collections %d%s must be in range [1, %d]: it caps how many abandoned collections each worker remembers; "+
			"set a positive count such as the default %d, and raise it if worker.tasks.dropped stays low while abandons are frequent: %w",
			c.MaxAbandonedCollections, formatSource(tracker, "worker.max_abandoned_collections"), MaxAbandonedCollectionsLimit, 10000, ErrInvalidConfig)
	}
	return p.err()
}

// MaxAbandonedCollectionsLimit is the largest accepted
// worker.max_abandoned_collections; it keeps one worker's abandoned set
// within about 43 MB with UUID job IDs.
const MaxAbandonedCollectionsLimit = 100_000

// PipelineConfig configures the pipeline orchestration service.
type PipelineConfig struct {
	MaxConcurrentJobs int `yaml:"max_concurrent_jobs" json:"max_concurrent_jobs"`
	// StageTimeout is the hard cap on how long one analyzer stage waits for
	// its task results, however busy the worker pool is. Worker progress
	// ticks reset analysis.engine.task_timeout but never this cap, so it is
	// what fails a stage whose task was lost (work runs on core NATS, with
	// no redelivery) while other stages keep the pool busy. Range (0, 168h]:
	// zero is rejected so no stage is unbounded. A real catalog's pair stage
	// can take hours (requires needed 2h12m for 621 LLM tasks on NIST CSF
	// 2.0), hence the 24h default. It also bounds how long each LLM worker
	// remembers an abandoned collection (worker.WithAbandonTTL), since no
	// collection outlives it. Global-only.
	StageTimeout time.Duration `yaml:"stage_timeout" json:"stage_timeout"`
	// Addr is the bind address for the standalone "pipeline" role's own
	// Connect RPC listener (see internal/pipeline.Server). Empty by
	// default -- only required when running --role pipeline.
	Addr string `yaml:"addr" json:"addr"`
	// Endpoint is where the standalone "gateway" role's Connect client
	// dials to reach a separately-deployed pipeline role (see
	// internal/gateway.NewConnectPipelineBackend). Empty by default --
	// only required when running --role gateway with pipeline split into
	// its own role. RoleAll ignores this and wires pipeline in-process.
	Endpoint string `yaml:"endpoint" json:"endpoint"`
}

// Validate checks PipelineConfig for consistency.
func (c *PipelineConfig) Validate() error { return c.validate(nil) }

func (c *PipelineConfig) validate(tracker *sourceTracker) error {
	var p problems
	if c.MaxConcurrentJobs < 0 || c.MaxConcurrentJobs > 100 {
		p.add("pipeline.max_concurrent_jobs", "pipeline.max_concurrent_jobs %d%s must be in range [0, 100]: %w",
			c.MaxConcurrentJobs, formatSource(tracker, "pipeline.max_concurrent_jobs"), ErrInvalidConfig)
	}
	if c.StageTimeout <= 0 || c.StageTimeout > 168*time.Hour {
		p.add("pipeline.stage_timeout", "pipeline.stage_timeout %s%s must be in range (0, 168h]: it caps how long each analyzer stage may run, so set a positive duration such as 24h: %w",
			c.StageTimeout, formatSource(tracker, "pipeline.stage_timeout"), ErrInvalidConfig)
	}
	return p.err()
}

// DefaultGraphMaxBulkEdges is the compiled default for graph.max_bulk_edges,
// also applied by internal/graph when no limit is configured. On Apache AGE a
// 1000-edge batch commits in about 0.1 s against a 25k-node graph; the batch's
// single CREATE statement grows faster than linearly (about 0.6 s at 5000 and
// 2 s at 10000), and one write transaction stays open for all of it.
const DefaultGraphMaxBulkEdges = 1000

// maxGraphMaxBulkEdges caps graph.max_bulk_edges where one AGE write
// transaction already takes seconds.
const maxGraphMaxBulkEdges = 10000

// GraphConfig configures the graph service (role "graph").
//
// Every field is global-only: there is no tenant_overrides map. MaxBulkEdges
// bounds how long one request holds a write transaction and connection on the
// graph database all tenants share, so it is sized per deployment.
type GraphConfig struct {
	// MaxBulkEdges is the most edges one BulkCreateEdges RPC may carry; larger
	// requests are rejected with InvalidArgument. Range [1, 10000].
	MaxBulkEdges int `yaml:"max_bulk_edges" json:"max_bulk_edges"`
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
	Role          string
	Health        HealthConfig
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
		Role:          c.Role,
		Health:        c.Health,
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

// RetentionTiers holds per-data-class retention durations expressed as strings.
// Each value is parsed by ParseRetention: "indefinite", "<n>y", "<n>d", or a
// Go duration string (e.g. "720h"). An empty string means "use the default".
type RetentionTiers struct {
	Attestation    string `yaml:"attestation" json:"attestation"`
	JobResults     string `yaml:"job_results" json:"job_results"`
	Catalogs       string `yaml:"catalogs" json:"catalogs"`
	Embeddings     string `yaml:"embeddings" json:"embeddings"`
	AuditDecisions string `yaml:"audit_decisions" json:"audit_decisions"`
	AuditLLM       string `yaml:"audit_llm" json:"audit_llm"`
	AuditEvents    string `yaml:"audit_events" json:"audit_events"`
}

// ArchiveConfig configures the archive backend used for tiered data movement.
type ArchiveConfig struct {
	Backend string `yaml:"backend" json:"backend"` // "local", "s3", or "" (disabled)
	S3      struct {
		Bucket       string `yaml:"bucket" json:"bucket"`
		StorageClass string `yaml:"storage_class" json:"storage_class"`
	} `yaml:"s3" json:"s3"`
	Local struct {
		Path string `yaml:"path" json:"path"`
	} `yaml:"local" json:"local"`
}

// RetentionConfig controls data lifecycle: default tiers, archive backend, and
// optional per-tenant tier overrides.
type RetentionConfig struct {
	Defaults     RetentionTiers            `yaml:"defaults" json:"defaults"`
	ScanSchedule string                    `yaml:"scan_schedule" json:"scan_schedule"` // cron expression; empty = disabled
	Archive      ArchiveConfig             `yaml:"archive" json:"archive"`
	Tenants      map[string]RetentionTiers `yaml:"tenants" json:"tenants"`
}

// ParseRetention parses a retention duration string.
//
// Accepted forms:
//   - "indefinite" — data is kept forever (returns indefinite=true, duration=0)
//   - "<n>y"       — n × 365 days
//   - "<n>d"       — n × 24 hours
//   - any string accepted by time.ParseDuration (e.g. "720h")
//
// A negative non-zero duration is rejected.
func ParseRetention(s string) (time.Duration, bool, error) {
	if s == "indefinite" {
		return 0, true, nil
	}
	if rest, ok := strings.CutSuffix(s, "y"); ok {
		n, err := strconv.Atoi(rest)
		if err != nil {
			return 0, false, fmt.Errorf("retention %q: %w", s, ErrInvalidConfig)
		}
		d := time.Duration(n) * 365 * 24 * time.Hour
		if d < 0 {
			return 0, false, fmt.Errorf("retention %q: negative duration: %w", s, ErrInvalidConfig)
		}
		return d, false, nil
	}
	if rest, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(rest)
		if err != nil {
			return 0, false, fmt.Errorf("retention %q: %w", s, ErrInvalidConfig)
		}
		d := time.Duration(n) * 24 * time.Hour
		if d < 0 {
			return 0, false, fmt.Errorf("retention %q: negative duration: %w", s, ErrInvalidConfig)
		}
		return d, false, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, false, fmt.Errorf("retention %q: %w", s, ErrInvalidConfig)
	}
	if d < 0 {
		return 0, false, fmt.Errorf("retention %q: negative duration: %w", s, ErrInvalidConfig)
	}
	return d, false, nil
}

var validArchiveBackends = map[string]bool{"": true, "local": true, "s3": true}

// validateRetentionTiers parses every non-empty field of a RetentionTiers
// and returns an error for each that does not parse, each a FieldError for
// <parent>.<field>.
func validateRetentionTiers(tiers RetentionTiers, parent string, tracker *sourceTracker) error {
	fields := []struct {
		name  string
		value string
	}{
		{"attestation", tiers.Attestation},
		{"job_results", tiers.JobResults},
		{"catalogs", tiers.Catalogs},
		{"embeddings", tiers.Embeddings},
		{"audit_decisions", tiers.AuditDecisions},
		{"audit_llm", tiers.AuditLLM},
		{"audit_events", tiers.AuditEvents},
	}
	var p problems
	for _, f := range fields {
		if f.value == "" {
			continue
		}
		if _, _, err := ParseRetention(f.value); err != nil {
			path := parent + "." + f.name
			p.add(path, "%s%s: %w", path, formatSource(tracker, path), err)
		}
	}
	return p.err()
}

// Validate checks RetentionConfig for correctness.
// Returns ErrInvalidConfig on validation failure.
func (rc *RetentionConfig) Validate() error { return rc.validate(nil) }

func (rc *RetentionConfig) validate(tracker *sourceTracker) error {
	var p problems
	if !validArchiveBackends[rc.Archive.Backend] {
		p.add("retention.archive.backend", "retention.archive.backend %q%s must be one of local, s3, or empty: %w",
			rc.Archive.Backend, formatSource(tracker, "retention.archive.backend"), ErrInvalidConfig)
	}
	p = append(p, validateRetentionTiers(rc.Defaults, "retention.defaults", tracker))
	for _, tenantID := range slices.Sorted(maps.Keys(rc.Tenants)) {
		p = append(p, validateRetentionTiers(rc.Tenants[tenantID], mapKeyPath("retention.tenants", tenantID), tracker))
	}
	return p.err()
}

// For returns the effective RetentionTiers for a tenant. Non-empty fields in
// the tenant's override replace the corresponding default field; empty fields
// fall back to the default.
func (rc *RetentionConfig) For(tenantID string) RetentionTiers {
	result := rc.Defaults
	override, ok := rc.Tenants[tenantID]
	if !ok {
		return result
	}
	if override.Attestation != "" {
		result.Attestation = override.Attestation
	}
	if override.JobResults != "" {
		result.JobResults = override.JobResults
	}
	if override.Catalogs != "" {
		result.Catalogs = override.Catalogs
	}
	if override.Embeddings != "" {
		result.Embeddings = override.Embeddings
	}
	if override.AuditDecisions != "" {
		result.AuditDecisions = override.AuditDecisions
	}
	if override.AuditLLM != "" {
		result.AuditLLM = override.AuditLLM
	}
	if override.AuditEvents != "" {
		result.AuditEvents = override.AuditEvents
	}
	return result
}
