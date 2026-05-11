// Package config provides configuration loading and validation for CrossCodex services.
//
// Configuration is resolved from multiple sources with the following precedence:
//  1. Command-line flags
//  2. Environment variables
//  3. Configuration files (XDG-compliant locations)
//  4. Default values
//
// Example usage:
//
//	loader := config.NewLoader()
//	cfg, err := loader.Load(ctx)
//	if err != nil {
//	    return err
//	}
//	fmt.Printf("LLM endpoint: %s\n", cfg.LLM.Endpoint)
package config
