package config

import "time"

// Config is the unified merge target for all configuration layers.
type Config struct {
	LLM           LLMConfig           `yaml:"llm"`
	Storage       StorageConfig       `yaml:"storage"`
	TLS           TLSConfig           `yaml:"tls"`
	Tenants       TenantsConfig       `yaml:"tenants"`
	Database      DatabaseConfig      `yaml:"database"`
	NATS          NATSConfig          `yaml:"nats"`
	Server        ServerConfig        `yaml:"server"`
	CLI           CLISettings         `yaml:"cli"`
	Logging       LoggingConfig       `yaml:"logging"`
	Auth          AuthConfig          `yaml:"auth"`
	Observability ObservabilityConfig `yaml:"observability"`
	Catalog       CatalogConfig       `yaml:"catalog"`
	Attestation   AttestationConfig   `yaml:"attestation"`
}

// LLMConfig configures the LLM gateway client.
type LLMConfig struct {
	GatewayURL     string   `yaml:"gateway_url"`
	GatewayMode    bool     `yaml:"gateway_mode"`
	DefaultModel   string   `yaml:"default_model"`
	EmbeddingModel string   `yaml:"embedding_model"`
	APIKeyRef      string   `yaml:"api_key_ref"`
	AllowedModels  []string `yaml:"allowed_models"`
	MaxRetries     int      `yaml:"max_retries"`
	Timeout        int      `yaml:"timeout"`
}

// StorageConfig configures storage backends.
type StorageConfig struct {
	Objects ObjectStorageConfig `yaml:"objects"`
}

// ObjectStorageConfig configures the object storage provider.
type ObjectStorageConfig struct {
	Backend  string `yaml:"backend"`
	BasePath string `yaml:"base_path"`
	Bucket   string `yaml:"bucket"`
	Region   string `yaml:"region"`
	Endpoint string `yaml:"endpoint"`
}

// TLSConfig configures TLS certificates and mode.
type TLSConfig struct {
	Mode        string                 `yaml:"mode"`
	CA          string                 `yaml:"ca"`
	Cert        string                 `yaml:"cert"`
	Key         string                 `yaml:"key"`
	FIPS        FIPSConfig             `yaml:"fips"`
	CipherAllow []string               `yaml:"cipher_allow"` // Substring-match cipher allowlist
	CipherDeny  []string               `yaml:"cipher_deny"`  // Substring-match cipher denylist
	Targets     map[string]TLSOverride `yaml:"targets"`
}

// FIPSConfig controls FIPS 140 enforcement.
type FIPSConfig struct {
	Enabled bool `yaml:"enabled"`
}

// TLSOverride holds per-target TLS overrides that merge with global TLS defaults.
type TLSOverride struct {
	Mode string `yaml:"mode"`
	CA   string `yaml:"ca"`
	Cert string `yaml:"cert"`
	Key  string `yaml:"key"`
}

// TenantsConfig configures multi-tenant behavior.
type TenantsConfig struct {
	Enabled        bool     `yaml:"enabled"`
	DefaultTenant  string   `yaml:"default_tenant"`
	AllowedTenants []string `yaml:"allowed_tenants"`
}

// AuthConfig configures authentication methods.
type AuthConfig struct {
	X509Mappings []X509MappingConfig `yaml:"x509_mappings"`
}

// X509MappingConfig maps X.509 certificate field patterns to tenant and roles.
type X509MappingConfig struct {
	Match  X509MatchConfig `yaml:"match"`
	Tenant string          `yaml:"tenant"`
	Roles  []string        `yaml:"roles"`
}

// X509MatchConfig holds glob patterns for X.509 certificate field matching.
type X509MatchConfig struct {
	CN           string `yaml:"cn"`
	Organization string `yaml:"organization"`
	OrgUnit      string `yaml:"org_unit"`
	SANEmail     string `yaml:"san_email"`
	SANDNS       string `yaml:"san_dns"`
	SANURI       string `yaml:"san_uri"`
}

// DatabaseConfig configures PostgreSQL connections.
//
// Two DSNs support the three-role security model (see pkg/db/doc.go):
//   - DSN connects as app_user for relational data behind RLS.
//   - GraphDSN connects as graph_user for AGE cypher queries.
//     graph_user owns per-tenant graph schemas but has no relational access.
type DatabaseConfig struct {
	DSN        string   `yaml:"dsn"`
	GraphDSN   string   `yaml:"graph_dsn"`
	Extensions []string `yaml:"extensions"`
	MaxConns   int      `yaml:"max_conns"`
	SSLMode    string   `yaml:"ssl_mode"`
}

// NATSConfig configures NATS JetStream connection.
type NATSConfig struct {
	URL      string             `yaml:"url"`      // External NATS URL; empty = embedded mode
	Cluster  string             `yaml:"cluster"`  // Cluster name (external mode)
	TLS      bool               `yaml:"tls"`      // Enable TLS
	Embedded NATSEmbeddedConfig `yaml:"embedded"` // Embedded server settings
	Streams  NATSStreamsConfig  `yaml:"streams"`  // JetStream stream settings
}

// NATSEmbeddedConfig configures the embedded NATS server.
type NATSEmbeddedConfig struct {
	StoreDir string `yaml:"store_dir"` // JetStream storage dir; empty = $XDG_STATE_HOME/crosscodex/nats/
}

// NATSStreamsConfig configures JetStream audit stream retention.
type NATSStreamsConfig struct {
	AuditLLMRetention    time.Duration `yaml:"audit_llm_retention"`    // Default: 2160h (90 days)
	AuditEventsRetention time.Duration `yaml:"audit_events_retention"` // Default: 720h (30 days)
	// AuditDecisions is always indefinite; no config knob.
}

// ServerConfig holds daemon-specific settings.
type ServerConfig struct {
	GRPCAddr string `yaml:"grpc_addr"`
	HTTPAddr string `yaml:"http_addr"`
	Workers  int    `yaml:"workers"`
}

// CLISettings holds CLI-specific settings.
type CLISettings struct {
	Output   string `yaml:"output"`
	NoColor  bool   `yaml:"no_color"`
	Endpoint string `yaml:"endpoint"`
}

// LoggingConfig configures structured logging.
type LoggingConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// ObservabilityConfig configures OpenTelemetry tracing and metrics export.
//
// A shared Endpoint serves as the default OTLP endpoint for all signals.
// Per-signal Endpoint fields override the shared default when non-empty.
// Empty resolved endpoint = signal disabled (no-op provider, no error).
type ObservabilityConfig struct {
	Endpoint string                     `yaml:"endpoint"`
	Protocol string                     `yaml:"protocol"`
	Tracing  ObservabilityTracingConfig `yaml:"tracing"`
	Metrics  ObservabilityMetricsConfig `yaml:"metrics"`
}

// ObservabilityTracingConfig configures the tracing signal.
type ObservabilityTracingConfig struct {
	Endpoint   string  `yaml:"endpoint"`
	Protocol   string  `yaml:"protocol"`
	SampleRate float64 `yaml:"sample_rate"`
}

// ObservabilityMetricsConfig configures the metrics signal.
type ObservabilityMetricsConfig struct {
	Endpoint string `yaml:"endpoint"`
	Protocol string `yaml:"protocol"`
	Interval string `yaml:"interval"`
}

// CatalogConfig configures the catalog parsing and structuring pipeline.
type CatalogConfig struct {
	Structuring StructuringConfig `yaml:"structuring"`
}

// StructuringConfig configures document-to-OSCAL structuring behavior.
type StructuringConfig struct {
	SectionPattern     string   `yaml:"section_pattern"`
	Decompose          bool     `yaml:"decompose"`
	MinDecomposeWords  int      `yaml:"min_decompose_words"`
	FilterByKeywords   bool     `yaml:"filter_by_keywords"`
	Keywords           []string `yaml:"keywords"`
	ChunkChars         int      `yaml:"chunk_chars"`
	MaxValidationChars int      `yaml:"max_validation_chars"`
	AllowedFormats     []string `yaml:"allowed_formats"`
	MaxHeadingRepeats  int      `yaml:"max_heading_repeats"`
}

// AttestationConfig configures in-toto attestation generation and verification.
//
// FIPS mode is not configured here. Attestation FIPS enforcement is derived
// from tls.fips.enabled -- a single deployment-wide posture. The service layer
// reads TLSConfig.FIPS.Enabled and passes attestation.WithFIPSMode() accordingly.
type AttestationConfig struct {
	Enabled           bool                           `yaml:"enabled"`
	PrivateKeyPath    string                         `yaml:"private_key_path"`
	PublicKeyPath     string                         `yaml:"public_key_path"`
	ExpiryDuration    time.Duration                  `yaml:"expiry_duration"`
	IncludeByProducts bool                           `yaml:"include_byproducts"`
	TenantOverrides   map[string]AttestationOverride `yaml:"tenant_overrides"`
}

// AttestationOverride allows per-tenant attestation settings.
// Nil pointer fields inherit the global AttestationConfig value.
type AttestationOverride struct {
	Enabled           *bool          `yaml:"enabled"`
	PrivateKeyPath    *string        `yaml:"private_key_path"`
	PublicKeyPath     *string        `yaml:"public_key_path"`
	ExpiryDuration    *time.Duration `yaml:"expiry_duration"`
	IncludeByProducts *bool          `yaml:"include_byproducts"`
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

// DaemonConfig is the derived view for crosscodexd.
type DaemonConfig struct {
	GRPCAddr      string
	HTTPAddr      string
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
}

// ClientConfig is the derived view for the crosscodex CLI.
type ClientConfig struct {
	Output   string
	NoColor  bool
	Endpoint string
	LLM      LLMConfig
	TLS      TLSConfig
	Logging  LoggingConfig
}

// ServiceConfig returns the daemon-oriented view of this configuration.
func (c *Config) ServiceConfig() DaemonConfig {
	return DaemonConfig{
		GRPCAddr:      c.Server.GRPCAddr,
		HTTPAddr:      c.Server.HTTPAddr,
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
	}
}

// CLIConfig returns the CLI-oriented view of this configuration.
func (c *Config) CLIConfig() ClientConfig {
	return ClientConfig{
		Output:   c.CLI.Output,
		NoColor:  c.CLI.NoColor,
		Endpoint: c.CLI.Endpoint,
		LLM:      c.LLM,
		TLS:      c.TLS,
		Logging:  c.Logging,
	}
}
