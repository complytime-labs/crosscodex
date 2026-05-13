package config

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
	Mode    string                 `yaml:"mode"`
	CA      string                 `yaml:"ca"`
	Cert    string                 `yaml:"cert"`
	Key     string                 `yaml:"key"`
	FIPS    FIPSConfig             `yaml:"fips"`
	Targets map[string]TLSOverride `yaml:"targets"`
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

// DatabaseConfig configures PostgreSQL connection.
type DatabaseConfig struct {
	DSN        string   `yaml:"dsn"`
	Extensions []string `yaml:"extensions"`
	MaxConns   int      `yaml:"max_conns"`
	SSLMode    string   `yaml:"ssl_mode"`
}

// NATSConfig configures NATS JetStream connection.
type NATSConfig struct {
	URL     string `yaml:"url"`
	Cluster string `yaml:"cluster"`
	TLS     bool   `yaml:"tls"`
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
