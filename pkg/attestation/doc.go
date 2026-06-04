// Package attestation provides in-toto attestation generation and verification.
//
// Creates cryptographically signed in-toto layout and link attestations backed
// by in-toto-golang. Link attestations automatically embed the OTel trace ID
// from the active span context, enabling correlation between distributed traces,
// NATS audit streams, and cryptographic attestation chains.
//
// Example usage:
//
//	kp := &attestation.FileKeyProvider{
//	    PrivateKeyPath: "signing.pem",
//	    PublicKeyPath:  "verification.pem",
//	}
//	gen, err := attestation.NewGenerator(kp, attestation.WithTelemetry(tracer, meter))
//	if err != nil {
//	    return err
//	}
//
//	link, err := gen.CreateLink(ctx, "catalog.ingest", materials, products)
//	if err != nil {
//	    return err
//	}
//	// link.TraceID contains the OTel trace ID for audit correlation
//	// link.Raw contains the signed DSSE envelope as JSON
package attestation
