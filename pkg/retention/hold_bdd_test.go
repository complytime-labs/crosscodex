package retention_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/retention"
)

var _ = Describe("Hold.ActiveAt", func() {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	It("is active when released_at and expires_at are both nil", func() {
		h := retention.Hold{}
		Expect(h.ActiveAt(now)).To(BeTrue())
	})

	It("is inactive when released_at is set", func() {
		h := retention.Hold{ReleasedAt: &past}
		Expect(h.ActiveAt(now)).To(BeFalse())
	})

	It("is active when expires_at is in the future", func() {
		h := retention.Hold{ExpiresAt: &future}
		Expect(h.ActiveAt(now)).To(BeTrue())
	})

	It("is inactive when expires_at is in the past", func() {
		h := retention.Hold{ExpiresAt: &past}
		Expect(h.ActiveAt(now)).To(BeFalse())
	})

	It("is inactive when expires_at equals now", func() {
		h := retention.Hold{ExpiresAt: &now}
		Expect(h.ActiveAt(now)).To(BeFalse())
	})

	It("released_at takes precedence over a future expires_at", func() {
		h := retention.Hold{ReleasedAt: &past, ExpiresAt: &future}
		Expect(h.ActiveAt(now)).To(BeFalse())
	})
})

var _ = Describe("Hold.Covers", func() {
	before := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h := retention.Hold{Scope: retention.HoldScope{CreatedBefore: &before, TenantID: "t1"}}

	It("covers a matching-tenant candidate created before the cutoff", func() {
		c := retention.Candidate{TenantID: "t1", CreatedAt: before.Add(-time.Hour)}
		Expect(h.Covers(c)).To(BeTrue())
	})

	It("does not cover a different tenant", func() {
		Expect(h.Covers(retention.Candidate{TenantID: "t2", CreatedAt: before.Add(-time.Hour)})).To(BeFalse())
	})

	It("does not cover a candidate created at or after the cutoff", func() {
		Expect(h.Covers(retention.Candidate{TenantID: "t1", CreatedAt: before})).To(BeFalse())
	})

	It("covers any candidate when scope is entirely empty", func() {
		empty := retention.Hold{}
		c := retention.Candidate{TenantID: "any", CreatedAt: before.Add(24 * time.Hour)}
		Expect(empty.Covers(c)).To(BeTrue())
	})

	It("matches on job_id when JobID scope is set", func() {
		jobBefore := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
		jh := retention.Hold{Scope: retention.HoldScope{JobID: "job-abc"}}
		match := retention.Candidate{ID: "job-abc", CreatedAt: jobBefore}
		nomatch := retention.Candidate{ID: "job-xyz", CreatedAt: jobBefore}
		Expect(jh.Covers(match)).To(BeTrue())
		Expect(jh.Covers(nomatch)).To(BeFalse())
	})
})

var _ = Describe("HoldChecker.Held", func() {
	before := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	It("returns false when there are no holds", func() {
		checker := retention.NewHoldChecker(nil)
		c := retention.Candidate{TenantID: "t1", CreatedAt: before.Add(-time.Hour)}
		Expect(checker.Held(c)).To(BeFalse())
	})

	It("returns true when any hold covers the candidate", func() {
		h := retention.Hold{Scope: retention.HoldScope{TenantID: "t1"}}
		checker := retention.NewHoldChecker([]retention.Hold{h})
		c := retention.Candidate{TenantID: "t1", CreatedAt: before}
		Expect(checker.Held(c)).To(BeTrue())
	})

	It("returns false when no hold covers the candidate", func() {
		h := retention.Hold{Scope: retention.HoldScope{TenantID: "t1"}}
		checker := retention.NewHoldChecker([]retention.Hold{h})
		c := retention.Candidate{TenantID: "t2", CreatedAt: before}
		Expect(checker.Held(c)).To(BeFalse())
	})

	It("returns true when the second hold in the list covers the candidate", func() {
		h1 := retention.Hold{Scope: retention.HoldScope{TenantID: "t1"}}
		h2 := retention.Hold{Scope: retention.HoldScope{TenantID: "t2"}}
		checker := retention.NewHoldChecker([]retention.Hold{h1, h2})
		c := retention.Candidate{TenantID: "t2", CreatedAt: before}
		Expect(checker.Held(c)).To(BeTrue())
	})
})
