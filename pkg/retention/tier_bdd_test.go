package retention_test

import (
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/retention"
)

func TestRetentionBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Retention Engine BDD Suite")
}

var _ = Describe("NewPolicy", func() {
	It("returns an error when a default tier string is invalid", func() {
		cfg := config.RetentionConfig{
			Defaults: config.RetentionTiers{JobResults: "notaduration"},
		}
		_, err := retention.NewPolicy(cfg)
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("Policy.Expired", func() {
	cfg := config.RetentionConfig{Defaults: config.RetentionTiers{JobResults: "7y", Attestation: "indefinite"}}
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	It("expires job_results older than 7y", func() {
		p, _ := retention.NewPolicy(cfg)
		Expect(p.Expired(retention.ClassJobResults, "t1", now.Add(-8*365*24*time.Hour), now)).To(BeTrue())
	})
	It("never expires indefinite classes", func() {
		p, _ := retention.NewPolicy(cfg)
		Expect(p.Expired(retention.ClassAttestation, "t1", now.Add(-100*365*24*time.Hour), now)).To(BeFalse())
	})

	It("respects per-tenant override: 10y tenant not expired at 8y", func() {
		cfgWithOverride := config.RetentionConfig{
			Defaults: config.RetentionTiers{JobResults: "7y"},
			Tenants: map[string]config.RetentionTiers{
				"enterprise": {JobResults: "10y"},
			},
		}
		p, err := retention.NewPolicy(cfgWithOverride)
		Expect(err).NotTo(HaveOccurred())
		// 8 years elapsed — less than the tenant's 10y override, must not be expired.
		Expect(p.Expired(retention.ClassJobResults, "enterprise", now.Add(-8*365*24*time.Hour), now)).To(BeFalse())
		// 11 years elapsed — exceeds 10y, must be expired.
		Expect(p.Expired(retention.ClassJobResults, "enterprise", now.Add(-11*365*24*time.Hour), now)).To(BeTrue())
		// Default tenant still uses 7y: 8y is expired.
		Expect(p.Expired(retention.ClassJobResults, "default", now.Add(-8*365*24*time.Hour), now)).To(BeTrue())
	})
})
