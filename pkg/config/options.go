package config

// Option configures a Loader.
type Option func(*loaderOptions)

// loaderOptions holds configuration for the Loader.
type loaderOptions struct {
	configPath string
	envPrefix  string
}

// WithConfigPath sets the configuration file path.
func WithConfigPath(path string) Option {
	return func(o *loaderOptions) {
		o.configPath = path
	}
}

// WithEnvPrefix sets the environment variable prefix.
func WithEnvPrefix(prefix string) Option {
	return func(o *loaderOptions) {
		o.envPrefix = prefix
	}
}
