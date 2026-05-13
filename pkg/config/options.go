package config

// Option configures a Loader.
type Option func(*loaderOptions)

type loaderOptions struct {
	configPath string
	envPrefix  string
	profile    string
	projectDir string
	overrides  map[string]string
}

// WithConfigPath overrides all file-based config with a single path.
func WithConfigPath(path string) Option {
	return func(o *loaderOptions) {
		o.configPath = path
	}
}

// WithEnvPrefix sets the environment variable prefix (default: CROSSCODEX).
func WithEnvPrefix(prefix string) Option {
	return func(o *loaderOptions) {
		o.envPrefix = prefix
	}
}

// WithProfile selects a named profile (e.g., "local", "distributed").
func WithProfile(name string) Option {
	return func(o *loaderOptions) {
		o.profile = name
	}
}

// WithProjectDir sets the project directory for .crosscodex/config.yaml lookup.
// Defaults to the current working directory.
func WithProjectDir(dir string) Option {
	return func(o *loaderOptions) {
		o.projectDir = dir
	}
}

// WithOverrides applies CLI flag overrides as dot-separated key-value pairs.
// These take highest priority (layer 9).
func WithOverrides(overrides map[string]string) Option {
	return func(o *loaderOptions) {
		o.overrides = overrides
	}
}
