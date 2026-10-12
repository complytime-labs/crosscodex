package artifacts_test

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/analyzer/artifacts"
)

// verdictStoreContractEnv is one empty tenant in a VerdictStore under test.
type verdictStoreContractEnv struct {
	store    artifacts.VerdictStore
	ctx      context.Context // carries tenantID
	tenantID string
	// setHuman records a human verdict on a queued pair, which no
	// VerdictStore method may do.
	setHuman func(key string, status artifacts.VerdictStatus)
	// row reads the stored row, bypassing the store under test, so specs can
	// see effects the interface does not return.
	row func(key string) contractRow
}

// contractRow is the part of a stored row the VerdictStore rules talk about.
type contractRow struct {
	status       artifacts.VerdictStatus
	graphApplied bool // projected into the graph
	attempts     int
	hasDissent   bool
}

// verdictStoreContract specifies the rules on artifacts.VerdictStore's doc
// comment: leasing and lease expiry, pending-only decisions, human ownership
// (Lease, Decide, Unprojected, MarkProjected, ResetProjection, RecordFailure
// and RecordDissent), projection listing order and limit, projection reset,
// lease-scoped failure recording, re-enqueue, Counts and tenant mismatch.
// Concurrent leasing and persistence details (timestamps, last_error) are
// covered only by the PG integration specs. It runs against fakeVerdictStore in unit tests and against
// PGVerdictStore under the integration build tag, so the fake the
// adjudicator specs rely on cannot drift from production.
func verdictStoreContract(newEnv func() verdictStoreContractEnv) {
	var e verdictStoreContractEnv
	t0 := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	ev := artifacts.PanelEvidence{PromptName: "artifact_same_as", PromptVersion: "1.0.0", Models: []string{"m1"},
		ValidVotes: 1, TotalVotes: 1}
	cand := func(i int) artifacts.PairCandidate {
		return artifacts.PairCandidate{LowGroupID: fmt.Sprintf("c-%03d-a", i), HighGroupID: fmt.Sprintf("c-%03d-b", i),
			CandidateEdgeID: fmt.Sprintf("c-edge-%03d", i), SimilarityScore: 0.75,
			Context: artifacts.PairContext{Type: "document", LowName: "a", HighName: "b"}}
	}
	key := func(c artifacts.PairCandidate) string { return artifacts.PairKey(c.LowGroupID, c.HighGroupID) }
	verdict := func(s artifacts.VerdictStatus) artifacts.Verdict {
		return artifacts.Verdict{Status: s, Confidence: 1, Evidence: ev, DecidedAt: t0}
	}

	BeforeEach(func() { e = newEnv() })

	enqueue := func(cs ...artifacts.PairCandidate) {
		Expect(e.store.Enqueue(e.ctx, e.tenantID, cs, t0)).To(Succeed())
	}
	lease := func(at time.Time) []artifacts.LeasedPair {
		pairs, err := e.store.Lease(e.ctx, e.tenantID, 10, time.Minute, at)
		Expect(err).NotTo(HaveOccurred())
		return pairs
	}

	It("leases a due pair to one caller until its lease expires", func() {
		c := cand(1)
		enqueue(c)
		first := lease(t0)
		Expect(first).To(HaveLen(1))
		Expect(first[0].Key).To(Equal(key(c)))
		Expect(first[0].LeasedUntil).To(BeTemporally("==", t0.Add(time.Minute)))
		Expect(lease(t0.Add(30 * time.Second))).To(BeEmpty())
		Expect(lease(t0.Add(2 * time.Minute))).To(HaveLen(1))
	})

	It("applies a decision only to a pending pair", func() {
		c := cand(1)
		enqueue(c)
		out, err := e.store.Decide(e.ctx, e.tenantID, key(c), verdict(artifacts.VerdictConfirmed))
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(Equal(artifacts.Decision{Outcome: artifacts.DecideApplied, CandidateEdgeID: c.CandidateEdgeID}))
		out, err = e.store.Decide(e.ctx, e.tenantID, key(c), verdict(artifacts.VerdictRejected))
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(Equal(artifacts.Decision{Outcome: artifacts.DecideStale}))
		closed, err := e.store.ClosedPairs(e.ctx, e.tenantID)
		Expect(err).NotTo(HaveOccurred())
		Expect(closed).To(Equal(map[string]bool{key(c): true}))
		proj, err := e.store.Unprojected(e.ctx, e.tenantID, 10)
		Expect(err).NotTo(HaveOccurred())
		Expect(proj).To(HaveLen(1))
		Expect(proj[0].Status).To(Equal(artifacts.VerdictConfirmed))
	})

	It("never leases, decides, lists or resets a human-owned pair", func() {
		c := cand(1)
		enqueue(c)
		e.setHuman(key(c), artifacts.VerdictPending)
		Expect(lease(t0)).To(BeEmpty())
		out, err := e.store.Decide(e.ctx, e.tenantID, key(c), verdict(artifacts.VerdictConfirmed))
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(Equal(artifacts.Decision{Outcome: artifacts.DecideHumanOwned}))

		e.setHuman(key(c), artifacts.VerdictConfirmed)
		Expect(e.store.MarkProjected(e.ctx, e.tenantID, key(c))).To(Succeed())
		proj, err := e.store.Unprojected(e.ctx, e.tenantID, 10)
		Expect(err).NotTo(HaveOccurred())
		Expect(proj).To(BeEmpty())
		n, err := e.store.ResetProjection(e.ctx, e.tenantID)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(BeZero())
		counts, err := e.store.Counts(e.ctx, e.tenantID, t0)
		Expect(err).NotTo(HaveOccurred())
		Expect(counts).To(Equal(artifacts.QueueCounts{}))
	})

	It("leaves a human-owned pair unprojected and its attempts at zero", func() {
		c := cand(1)
		enqueue(c)
		p := lease(t0)[0]
		e.setHuman(key(c), artifacts.VerdictPending)
		Expect(e.store.RecordFailure(e.ctx, e.tenantID, key(c), artifacts.Failure{Err: "x", At: t0,
			NextAttemptAt: t0, LeasedUntil: p.LeasedUntil})).To(Succeed())
		Expect(e.row(key(c)).attempts).To(BeZero())

		e.setHuman(key(c), artifacts.VerdictConfirmed)
		Expect(e.store.MarkProjected(e.ctx, e.tenantID, key(c))).To(Succeed())
		Expect(e.row(key(c)).graphApplied).To(BeFalse())
	})

	It("stores dissent only on a human-owned pair", func() {
		human, auto := cand(1), cand(2)
		enqueue(human, auto)
		e.setHuman(key(human), artifacts.VerdictConfirmed)
		Expect(e.store.RecordDissent(e.ctx, e.tenantID, key(human), verdict(artifacts.VerdictRejected))).To(Succeed())
		Expect(e.store.RecordDissent(e.ctx, e.tenantID, key(auto), verdict(artifacts.VerdictRejected))).To(Succeed())
		Expect(e.row(key(human)).hasDissent).To(BeTrue())
		Expect(e.row(key(human)).status).To(Equal(artifacts.VerdictConfirmed), "dissent must not change the human verdict")
		Expect(e.row(key(auto)).hasDissent).To(BeFalse())
	})

	It("counts pending, due and unprojected pairs, leaving human-owned ones out", func() {
		due, later, decided, human := cand(1), cand(2), cand(3), cand(4)
		enqueue(due, decided, human)
		Expect(e.store.Enqueue(e.ctx, e.tenantID, []artifacts.PairCandidate{later}, t0.Add(time.Hour))).To(Succeed())
		_, err := e.store.Decide(e.ctx, e.tenantID, key(decided), verdict(artifacts.VerdictConfirmed))
		Expect(err).NotTo(HaveOccurred())
		e.setHuman(key(human), artifacts.VerdictPending)
		counts, err := e.store.Counts(e.ctx, e.tenantID, t0)
		Expect(err).NotTo(HaveOccurred())
		Expect(counts).To(Equal(artifacts.QueueCounts{Pending: 2, Due: 1, Unprojected: 1}))
	})

	It("lists the oldest decisions first and no more than asked", func() {
		first, second, third := cand(1), cand(2), cand(3)
		enqueue(first, second, third)
		for _, d := range []struct {
			c  artifacts.PairCandidate
			at time.Duration
		}{{second, 3 * time.Minute}, {first, 2 * time.Minute}, {third, time.Minute}} {
			v := verdict(artifacts.VerdictConfirmed)
			v.DecidedAt = t0.Add(d.at)
			_, err := e.store.Decide(e.ctx, e.tenantID, key(d.c), v)
			Expect(err).NotTo(HaveOccurred())
		}
		proj, err := e.store.Unprojected(e.ctx, e.tenantID, 2)
		Expect(err).NotTo(HaveOccurred())
		Expect(proj).To(HaveLen(2))
		Expect(proj[0].Key).To(Equal(key(third)))
		Expect(proj[1].Key).To(Equal(key(first)))
	})

	It("lists decided verdicts until marked projected, and again after ResetProjection", func() {
		confirmed, undecided := cand(1), cand(2)
		enqueue(confirmed, undecided)
		_, err := e.store.Decide(e.ctx, e.tenantID, key(confirmed), verdict(artifacts.VerdictConfirmed))
		Expect(err).NotTo(HaveOccurred())
		_, err = e.store.Decide(e.ctx, e.tenantID, key(undecided), verdict(artifacts.VerdictUndecided))
		Expect(err).NotTo(HaveOccurred())
		proj, err := e.store.Unprojected(e.ctx, e.tenantID, 10)
		Expect(err).NotTo(HaveOccurred())
		Expect(proj).To(ConsistOf(HaveField("Key", key(confirmed))))

		Expect(e.store.MarkProjected(e.ctx, e.tenantID, key(confirmed))).To(Succeed())
		proj, err = e.store.Unprojected(e.ctx, e.tenantID, 10)
		Expect(err).NotTo(HaveOccurred())
		Expect(proj).To(BeEmpty())

		n, err := e.store.ResetProjection(e.ctx, e.tenantID)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(1))
		proj, err = e.store.Unprojected(e.ctx, e.tenantID, 10)
		Expect(err).NotTo(HaveOccurred())
		Expect(proj).To(ConsistOf(HaveField("Key", key(confirmed))))
		n, err = e.store.ResetProjection(e.ctx, e.tenantID)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(BeZero())
	})

	It("records a failure only under the caller's lease", func() {
		c := cand(1)
		enqueue(c)
		p := lease(t0)[0]
		Expect(e.store.RecordFailure(e.ctx, e.tenantID, key(c), artifacts.Failure{Err: "x", At: t0,
			NextAttemptAt: t0, LeasedUntil: p.LeasedUntil.Add(time.Second)})).To(Succeed())
		Expect(lease(t0.Add(30*time.Second))).To(BeEmpty(), "a foreign lease's failure must not release the pair")

		Expect(e.store.RecordFailure(e.ctx, e.tenantID, key(c), artifacts.Failure{Err: "x", At: t0,
			NextAttemptAt: t0.Add(5 * time.Minute), LeasedUntil: p.LeasedUntil})).To(Succeed())
		Expect(lease(t0.Add(time.Second))).To(BeEmpty(), "not due until NextAttemptAt")
		again := lease(t0.Add(5 * time.Minute))
		Expect(again).To(HaveLen(1))
		Expect(again[0].Attempts).To(Equal(1))
	})

	It("moves a re-enqueued pending pair to the new candidate, but not a decided one", func() {
		pending, decided := cand(1), cand(2)
		enqueue(pending, decided)
		_, err := e.store.Decide(e.ctx, e.tenantID, key(decided), verdict(artifacts.VerdictConfirmed))
		Expect(err).NotTo(HaveOccurred())
		pending2, decided2 := pending, decided
		pending2.CandidateEdgeID, pending2.SimilarityScore = "c-edge-new-1", 0.9
		decided2.CandidateEdgeID = "c-edge-new-2"
		enqueue(pending2, decided2)

		leased := lease(t0)
		Expect(leased).To(HaveLen(1))
		Expect(leased[0].CandidateEdgeID).To(Equal("c-edge-new-1"))
		Expect(leased[0].SimilarityScore).To(Equal(0.9))
		proj, err := e.store.Unprojected(e.ctx, e.tenantID, 10)
		Expect(err).NotTo(HaveOccurred())
		Expect(proj).To(HaveLen(1))
		Expect(proj[0].CandidateEdgeID).To(Equal(decided.CandidateEdgeID))
	})

	It("refuses a tenant argument that differs from the context", func() {
		_, err := e.store.Lease(e.ctx, e.tenantID+"-other", 10, time.Minute, t0)
		Expect(err).To(MatchError(ContainSubstring("does not match")))
	})
}
