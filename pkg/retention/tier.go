package retention

import (
	"time"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

// Policy resolves retention durations for (DataClass, tenant) pairs.
// Construct with NewPolicy; the zero value is not usable.
type Policy struct {
	cfg config.RetentionConfig
}

// NewPolicy validates all configured retention tier strings in cfg and returns
// a ready-to-use Policy. Returns an error if any non-empty tier string in the
// defaults or any per-tenant override cannot be parsed by config.ParseRetention.
func NewPolicy(cfg config.RetentionConfig) (Policy, error) {
	if err := cfg.Validate(); err != nil {
		return Policy{}, err
	}
	return Policy{cfg: cfg}, nil
}

// Expired reports whether a candidate of the given class and tenant has
// exceeded its retention window as of now.
//
// A class configured with "indefinite" retention (or an empty duration string)
// is never considered expired. A candidate is expired when
// now.Sub(createdAt) > configured duration.
func (p Policy) Expired(class DataClass, tenantID string, createdAt, now time.Time) bool {
	tiers := p.cfg.For(tenantID)
	s := tierField(tiers, class)
	if s == "" {
		// No duration configured: treat as indefinite.
		return false
	}
	d, indefinite, err := config.ParseRetention(s)
	if err != nil || indefinite {
		return false
	}
	return now.Sub(createdAt) > d
}

// tierField returns the retention duration string for class from tiers.
func tierField(tiers config.RetentionTiers, class DataClass) string {
	switch class {
	case ClassJobResults:
		return tiers.JobResults
	case ClassCatalogs:
		return tiers.Catalogs
	case ClassEmbeddings:
		return tiers.Embeddings
	case ClassAttestation:
		return tiers.Attestation
	default:
		return ""
	}
}
