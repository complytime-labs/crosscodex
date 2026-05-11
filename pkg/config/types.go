package config

// Config represents the complete resolved configuration for CrossCodex services.
type Config struct {
	LLM      LLMConfig      // LLM gateway configuration
	Storage  StorageConfig  // Object storage configuration
	TLS      TLSConfig      // TLS and certificate configuration
	Tenants  TenantsConfig  // Multi-tenant configuration
	Database DatabaseConfig // Database connection configuration
	NATS     NATSConfig     // NATS messaging configuration
}

// LLMConfig configures the LLM gateway client.
type LLMConfig struct {
	Endpoint string // Gateway endpoint URL
	APIKey   string // API key for authentication
	Timeout  int    // Request timeout in seconds
}

// StorageConfig configures the object storage provider.
type StorageConfig struct {
	Provider string // "local" or "s3"
	BasePath string // Base path for local storage
	Bucket   string // S3 bucket name
	Region   string // S3 region
}

// TLSConfig configures TLS certificates and FIPS mode.
type TLSConfig struct {
	Enabled     bool   // Enable TLS
	CertFile    string // Path to certificate file
	KeyFile     string // Path to private key file
	CAFile      string // Path to CA certificate file
	FIPSEnabled bool   // Enforce FIPS mode
}

// TenantsConfig configures multi-tenant behavior.
type TenantsConfig struct {
	Enabled        bool     // Enable multi-tenancy
	DefaultTenant  string   // Default tenant ID
	AllowedTenants []string // Whitelist of allowed tenant IDs
}

// DatabaseConfig configures PostgreSQL connection.
type DatabaseConfig struct {
	Host     string // Database host
	Port     int    // Database port
	Database string // Database name
	User     string // Database user
	Password string // Database password
	SSLMode  string // SSL mode (disable, require, verify-ca, verify-full)
}

// NATSConfig configures NATS JetStream connection.
type NATSConfig struct {
	URL     string // NATS server URL
	Cluster string // Cluster name
	TLS     bool   // Enable TLS
}
