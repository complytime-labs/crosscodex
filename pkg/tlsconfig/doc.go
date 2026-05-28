// Package tlsconfig provides shared TLS configuration for CrossCodex services.
//
// It resolves global TLS settings with per-target deep-merge overrides,
// producing stdlib *tls.Config instances. FIPS cipher suite enforcement
// auto-discovers safe suites via tls.CipherSuites() and filters for GCM.
//
// Example usage:
//
//	tlsCfg, err := tlsconfig.BuildTLSConfig(cfg.TLS, "nats")
//	if err != nil {
//	    return fmt.Errorf("build NATS TLS config: %w", err)
//	}
//	if tlsCfg == nil {
//	    // TLS mode is "off", proceed without TLS
//	}
//
//	// FIPS verification:
//	status, err := tlsconfig.VerifyFIPSBuild()
//	if err != nil {
//	    return fmt.Errorf("FIPS check failed: %w", err)
//	}
//	fmt.Printf("FIPS: %v, provider: %s\n", status.Enabled, status.Provider)
package tlsconfig
