// Package config provides layered, XDG-compliant configuration for CrossCodex.
//
// Configuration is resolved from nine sources with the following precedence
// (highest wins):
//
//  1. Compiled defaults (lowest)
//  2. System config (/etc/crosscodex/config.yaml)
//  3. System drop-ins (/etc/crosscodex/conf.d/*.yaml)
//  4. User config ($XDG_CONFIG_HOME/crosscodex/config.yaml)
//  5. User drop-ins ($XDG_CONFIG_HOME/crosscodex/conf.d/*.yaml)
//  6. Profile selection (--profile <name>)
//  7. Project config (.crosscodex/config.yaml)
//  8. Environment variables (CROSSCODEX_* prefix)
//  9. CLI flags (highest)
//
// Each layer deep-merges over the previous: maps merge recursively, scalars
// and slices replace. Drop-in files load in lexicographic order.
//
// Example usage:
//
//	loader := config.NewLoader()
//	cfg, err := loader.Load(ctx, config.WithProfile("local"))
//	if err != nil {
//	    return err
//	}
//	fmt.Printf("LLM endpoint: %s\n", cfg.LLM.GatewayURL)
package config
