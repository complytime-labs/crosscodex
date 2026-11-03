package pipeline

import (
	"fmt"
	"maps"
	"slices"
	"time"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/config"
)

// Attestor signs the attestations of one tenant's jobs.
type Attestor struct {
	Generator attestation.Generator
	// ExpiresIn is the validity of each job's layout
	// (attestation.expiry_duration).
	ExpiresIn time.Duration
}

// Attestors decides which tenants' jobs are attested, and how. A job whose
// tenant resolves to no Attestor runs unattested; one that resolves to an
// Attestor completes only once its whole attestation is stored, and fails
// otherwise.
type Attestors struct {
	// Default attests the jobs of every tenant not in Tenants. Nil when
	// attestation.enabled is false.
	Default *Attestor
	// Tenants overrides Default per tenant (attestation.tenant_overrides);
	// a nil entry leaves that tenant's jobs unattested.
	Tenants map[string]*Attestor
	// StorageRetries and StorageRetryDelay bound how long a failed write of
	// a layout, link or bundle is retried before the job fails
	// (attestation.storage_retries, attestation.storage_retry_delay).
	StorageRetries    int
	StorageRetryDelay time.Duration
}

// forTenant returns the Attestor for tenantID's jobs, or nil when they are
// not attested.
func (a Attestors) forTenant(tenantID string) *Attestor {
	if att, ok := a.Tenants[tenantID]; ok {
		return att
	}
	return a.Default
}

// NewAttestors builds the Attestors cfg describes. It fails closed: an
// enabled scope without both key paths (cfg.RequireKeys), or whose keys do
// not load, match, or (when fips is true, from tls.fips.enabled) use a
// FIPS-approved curve, fails the call, so the daemon refuses to start
// instead of failing every job. Each distinct key pair is read once.
// A disabled scope gets no Attestor and no keys are read for it.
func NewAttestors(cfg config.AttestationConfig, fips bool, tracer trace.Tracer, meter metric.Meter) (Attestors, error) {
	if err := cfg.RequireKeys(); err != nil {
		return Attestors{}, err
	}
	out := Attestors{StorageRetries: cfg.StorageRetries, StorageRetryDelay: cfg.StorageRetryDelay}
	keys := make(map[[2]string]attestation.KeyProvider)
	build := func(scope string, tc config.AttestationTenantConfig) (*Attestor, error) {
		if !tc.Enabled {
			return nil, nil
		}
		pair := [2]string{tc.PrivateKeyPath, tc.PublicKeyPath}
		kp, ok := keys[pair]
		if !ok {
			var err error
			kp, err = attestation.LoadFileKeys(tc.PrivateKeyPath, tc.PublicKeyPath, fips)
			if err != nil {
				return nil, fmt.Errorf("%s: load the attestation key pair: %w", scope, err)
			}
			keys[pair] = kp
		}
		opts := []attestation.Option{
			attestation.WithFIPSMode(fips),
			attestation.WithIncludeByProducts(tc.IncludeByProducts),
		}
		if tracer != nil && meter != nil {
			opts = append(opts, attestation.WithTelemetry(tracer, meter))
		}
		gen, err := attestation.NewGenerator(kp, opts...)
		if err != nil {
			return nil, fmt.Errorf("%s: create the attestation generator: %w", scope, err)
		}
		return &Attestor{Generator: gen, ExpiresIn: tc.ExpiryDuration}, nil
	}

	// "" is never a valid tenant ID, so it resolves to the global settings.
	var err error
	out.Default, err = build("attestation", cfg.ForTenant(""))
	if err != nil {
		return Attestors{}, err
	}
	for _, tenantID := range slices.Sorted(maps.Keys(cfg.TenantOverrides)) {
		att, err := build(fmt.Sprintf("attestation.tenant_overrides[%q]", tenantID), cfg.ForTenant(tenantID))
		if err != nil {
			return Attestors{}, err
		}
		if out.Tenants == nil {
			out.Tenants = make(map[string]*Attestor, len(cfg.TenantOverrides))
		}
		out.Tenants[tenantID] = att
	}
	return out, nil
}
