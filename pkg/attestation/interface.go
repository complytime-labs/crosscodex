package attestation

import (
	"context"
	"crypto"
)

// Generator creates in-toto attestations.
//
// Implementations must follow the in-toto specification for
// layout and link generation.
type Generator interface {
	// CreateLayout creates a supply chain layout.
	CreateLayout(ctx context.Context, stages []Step) (*Layout, error)

	// CreateLink creates an execution record for a pipeline step.
	CreateLink(ctx context.Context, step Step, materials, products []Artifact) (*Link, error)

	// Sign signs an attestation payload with the provided key.
	Sign(ctx context.Context, payload any, key crypto.Signer) ([]byte, error)

	// Verify verifies an attestation signature.
	Verify(ctx context.Context, payload []byte, signature []byte, publicKey crypto.PublicKey) error
}
