// Package attestation provides in-toto attestation generation and validation.
//
// Creates cryptographically signed attestations for supply chain security.
//
// Example usage:
//
//	gen := attestation.NewGenerator()
//	layout, err := gen.CreateLayout(ctx, stages)
//	if err != nil {
//	    return err
//	}
//
//	signed, err := gen.Sign(ctx, layout, privateKey)
package attestation
