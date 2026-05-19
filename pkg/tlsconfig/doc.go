// Package tlsconfig provides TLS configuration with FIPS enforcement.
//
// Handles certificate loading, validation, and FIPS mode compliance checks.
//
// Example usage:
//
//	builder := tlsconfig.NewBuilder()
//	serverTLS, err := builder.BuildServer(ctx, "server.crt", "server.key", "ca.crt")
//	if err != nil {
//	    return err
//	}
//
//	if cfg.TLS.FIPSEnabled {
//	    if err := builder.ValidateFIPS(ctx); err != nil {
//	        return fmt.Errorf("FIPS validation failed: %w", err)
//	    }
//	}
package tlsconfig
