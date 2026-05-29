package config

import "time"

// Config is the unified merge target for all configuration layers.
type Config struct {
	LLM      LLMConfig      `yaml:"llm"`
	Storage  StorageConfig  `yaml:"storage"`
	TLS      TLSConfig      `yaml:"tls"`
	Tenants  TenantsConfig  `yaml:"tenants"`
	Database DatabaseConfig `yaml:"database"`
	NATS     NATSConfig     `yaml:"nats"`
	Server   ServerConfig   `yaml:"server"`
	CLI      CLISettings    `yaml:"cli"`
	Logging  LoggingConfig  `yaml:"logging"`
	Auth     AuthConfig     `yaml:"auth"`
}

// LLMConfig configures the LLM gateway client.
type LLMConfig struct {
	GatewayURL     string `yaml:"gateway_url"`
	DefaultModel   string `yaml:"default_model"`
	EmbeddingModel string `yaml:"embedding_model"`
	APIKey         string `yaml:"api_key"`
	Timeout        int    `yaml:"timeout"`
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

// DaemonConfig is the derived view for crosscodexd.
type DaemonConfig struct {
	GRPCAddr string
	HTTPAddr string
	Workers  int
	LLM      LLMConfig
	Storage  StorageConfig
	TLS      TLSConfig
	Tenants  TenantsConfig
	Database DatabaseConfig
	NATS     NATSConfig
	Logging  LoggingConfig
	Auth     AuthConfig
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
		GRPCAddr: c.Server.GRPCAddr,
		HTTPAddr: c.Server.HTTPAddr,
		Workers:  c.Server.Workers,
		LLM:      c.LLM,
		Storage:  c.Storage,
		TLS:      c.TLS,
		Tenants:  c.Tenants,
		Database: c.Database,
		NATS:     c.NATS,
		Logging:  c.Logging,
		Auth:     c.Auth,
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
