package config

import "context"

// Loader resolves configuration from nine layers in priority order:
//
//  1. Compiled defaults (lowest)
//  2. System config (/etc/crosscodex/config.yaml)
//  3. System drop-ins (/etc/crosscodex/conf.d/*.yaml)
//  4. User config ($XDG_CONFIG_HOME/crosscodex/config.yaml)
//  5. User drop-ins ($XDG_CONFIG_HOME/crosscodex/conf.d/*.yaml)
//  6. Profile (~/.config/crosscodex/profiles/<name>.yaml)
//  7. Project config (.crosscodex/config.yaml)
//  8. Environment variables (CROSSCODEX_*)
//  9. CLI flag overrides (highest)
type Loader interface {
	Load(ctx context.Context, opts ...Option) (*Config, error)
}
