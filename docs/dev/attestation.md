# Cryptographic Attestation

This document covers the in-toto attestation system for compliance audit trails, its planned integration with OpenTelemetry traces and NATS audit streams, and the current implementation status.

> **Status**: The `pkg/attestation` package is implemented. It provides in-toto attestation generation and verification backed by in-toto-golang v0.11.0, with automatic OTel trace correlation via `ByProducts["trace_id"]`.

## Overview

CrossCodex uses [in-toto](https://in-toto.io/) attestations to provide cryptographic proof of every compliance-critical operation. An auditor should be able to verify that a compliance mapping was produced by a specific model, from specific input materials, at a specific time, and trace that attestation back to the full distributed trace of the operation.

Three systems work together:

1. **OpenTelemetry traces** record what happened and when (see [Telemetry](telemetry.md)).
2. **NATS audit streams** persist the operational record with integrity guarantees (see [Audit Streams](audit-streams.md)).
3. **in-toto attestations** provide cryptographic proof that each step was performed by an authorized process with the expected inputs and outputs.

The bridge between these systems is the OTel trace ID. Every attestation embeds the trace ID of the operation it attests to, and every audit message carries the same trace ID in its provenance headers. An auditor can start from any of the three systems and reach the other two.

## In-Toto Concepts

### Layouts

A layout defines the expected supply chain workflow — which steps should execute, in what order, and who is authorized to perform each step. In CrossCodex, the Pipeline service signs the layout declaring the authorized stages and functionaries for a compliance processing run.

### Links

A link is an execution record for a single step. It captures:

- **Materials**: input artifacts with their SHA-256 digests (e.g., the OSCAL catalog being analyzed)
- **Products**: output artifacts with their digests (e.g., the generated compliance mapping)
- **Command**: what was executed
- **By-products**: additional metadata, including the OTel trace ID

### Verification

Given a layout and a set of links, verification confirms that every declared step was performed by an authorized functionary, that the chain of materials-to-products is unbroken, and that all signatures are valid.

## Package Structure

The `pkg/attestation` package defines the following:

### Generator Interface

```go
type Generator interface {
    CreateLayout(ctx context.Context, stages []Step) (*Layout, error)
    CreateLink(ctx context.Context, step Step, materials, products []Artifact) (*Link, error)
    Sign(ctx context.Context, payload any, key crypto.Signer) ([]byte, error)
    Verify(ctx context.Context, payload []byte, signature []byte, publicKey crypto.PublicKey) error
}
```

### Types

| Type         | Purpose                                                                 |
|--------------|-------------------------------------------------------------------------|
| `Layout`     | Supply chain workflow definition (steps, inspections, keys, expiration) |
| `Step`       | Pipeline step with expected materials and products                      |
| `Inspection` | Post-execution verification (run command, check success criteria)       |
| `Link`       | Execution record for a completed step                                   |
| `Artifact`   | File or artifact with URI and SHA-256 digest                            |
| `Signature`  | Key ID and signature bytes                                              |

### Errors

| Error                   | Meaning                                            |
|-------------------------|----------------------------------------------------|
| `ErrInvalidLayout`      | The layout is malformed or missing required fields |
| `ErrSignatureFailed`    | Signing the attestation payload failed             |
| `ErrVerificationFailed` | Signature verification did not pass                |
| `ErrExpired`            | The attestation or layout has expired              |

## Planned Workflow

### Operations That Require Attestations **(Planned)**

Four categories of compliance-critical operations must generate attestations when the services are implemented:

| Operation         | Service              | What the Attestation Proves                                    |
|-------------------|----------------------|----------------------------------------------------------------|
| Catalog ingestion | Ingestion Service    | OSCAL document authenticity, validation result, source hash    |
| Control mappings  | Analysis / Synthesis | AI-generated mapping accuracy, model used, input/output hashes |
| Risk assessments  | Analysis Service     | Evaluation results, evidence collected, criteria applied       |
| Policy violations | (enforcement)        | Actions taken, audit trail, affected controls                  |

### Attestation-Trace Bridge **(Planned)**

Every attestation must embed the OTel trace ID from the active span context. The proto field `AuditMetadata.correlation_id` (defined in `api/proto/crosscodex/v1/common.proto`) is documented as "OpenTelemetry trace ID" and serves this purpose.

The planned flow:

```go
func (s *ComplianceService) GenerateMapping(ctx context.Context, req MappingRequest) error {
    ctx, span := tracer.Start(ctx, "compliance.GenerateMapping")
    defer span.End()

    // ... perform mapping operation ...

    link, err := s.attestor.CreateLink(ctx, step, materials, products)
    if err != nil {
        return err
    }

    // Embed the trace ID in the link's by-products
    link.ByProducts["trace_id"] = telemetry.TraceIDFromContext(ctx)
    link.ByProducts["model"] = req.LLMModel
    link.ByProducts["timestamp"] = time.Now().UTC().Format(time.RFC3339Nano)

    signed, err := s.attestor.Sign(ctx, link, s.signingKey)
    if err != nil {
        return err
    }

    // Store the signed attestation under content-addressed path
    attestKey := storage.ContentKey(signed)
    return s.storage.Put(ctx, attestKey, signed)
}
```

The `telemetry.TraceIDFromContext` helper is already implemented and tested in `pkg/telemetry/correlation.go`.

### Attestation Storage **(Planned)**

Signed attestation bundles are stored in object storage under content-addressed paths. `pkg/storage` already provides a `ContentKey` function that generates paths like `attestation/<sha256-hash>.json`.

### Reading Attestations **(Planned)**

An auditor verifying a compliance determination would:

1. Find the compliance decision in the `AUDIT_DECISIONS` NATS stream.
2. Extract the `X-Trace-Id` provenance header to get the trace ID.
3. Search object storage for attestation bundles that reference that trace ID.
4. Verify the attestation signatures against the known public keys.
5. Confirm the materials-to-products chain matches the expected workflow.
6. Optionally, view the full trace in Jaeger using the same trace ID to see timing, service interactions, and error details.

## Existing Infrastructure

Several components already provide the foundation for attestations:

### Provenance Headers (Implemented)

`pkg/natsbus` injects five provenance headers on every published message, including `X-Trace-Id`, `X-Span-Id`, and `X-Content-SHA256`. Messages without valid provenance are rejected. See [Audit Streams](audit-streams.md) for details.

### Trace Correlation Helpers (Implemented)

`pkg/telemetry` provides `TraceIDFromContext(ctx)` and `SpanIDFromContext(ctx)` to extract the current trace and span IDs as hex strings. `pkg/authn` already uses `TraceIDFromContext` to populate the `SessionID` field in audit events, demonstrating the pattern that attestations will follow.

### Content-Addressed Storage (Implemented)

`pkg/storage` provides a `ContentKey` function that computes a content-addressed path (`attestation/<sha256-hash>.json`) from a byte slice. The path can be used with any storage backend.

### Proto Definitions (Defined, Not Populated)

`AuditMetadata.correlation_id` is defined in `common.proto` as "OpenTelemetry trace ID". `ProvenanceMetadata.attestation_link_id` is defined as "in-toto link reference". Neither field is populated in Go code because the services that would set them do not exist yet.

## Implementation Roadmap

The attestation system depends on several components that are not yet built:

1. **`pkg/attestation` implementation**: A concrete `Generator` implementation, likely backed by [in-toto-golang](https://github.com/in-toto/in-toto-golang) or [Sigstore](https://sigstore.dev/) libraries.
2. **Service implementations**: The internal services (`internal/ingestion`, `internal/catalog`, `internal/analysis`, `internal/synthesis`) that would call the attestation API.
3. **`AuditMetadata.correlation_id` population**: Each service must set this field from `telemetry.TraceIDFromContext(ctx)` when constructing protobuf messages.
4. **Verification CLI**: A `crosscodex verify` command that takes a compliance report and validates the attestation chain.

## Security Considerations

- Attestation signing keys must be protected. Use `pkg/tlsconfig/pki` for key generation in development and a proper key management system (HSM, Vault) in production.
- The attestation chain depends on the integrity of the OTel trace ID. If trace context is dropped or spoofed, the link between the attestation and the operational record is broken. TLS transport encryption and provenance header validation (enforced by `pkg/natsbus`) mitigate this.
- Layouts define who is authorized to perform each step. A compromised signing key allows unauthorized attestations. Key rotation and revocation procedures should be defined before production deployment.
- Attestation bundles stored in object storage are content-addressed (keyed by hash), making them immutable. Deleting an attestation requires deleting the storage object, which should be restricted to platform administrators.
