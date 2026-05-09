package config

import "context"

// Loader handles configuration resolution from multiple sources.
//
// Implementations must resolve configuration with the following precedence:
//  1. Command-line flags (highest)
//  2. Environment variables
//  3. Configuration files
//  4. Default values (lowest)
type Loader interface {
	// Load resolves and validates configuration from all sources.
	// Returns an error if configuration is invalid or required values are missing.
	Load(ctx context.Context, opts ...Option) (*Config, error)
}
