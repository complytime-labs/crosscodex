package artifacts_test

import (
	"context"
	"fmt"
	"strings"
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
	// setHuman makes a queued pair human-owned with the given status, as
	// DecideHuman does, including states DecideHuman cannot produce (a
	// human-owned pending pair).
	setHuman func(key string, status artifacts.VerdictStatus)
	// row reads the stored row, bypassing the store under test, so specs can
	// see effects the interface does not return.
	row func(key string) contractRow
}

// contractRow is the part of a stored row the VerdictStore rules talk about.
type contractRow struct {
	status        artifacts.VerdictStatus
	determination string
	determinedBy  string
	graphApplied  bool // projected into the graph
	attempts      int
	leased        bool
	hasDissent    bool
	dissentStatus artifacts.VerdictStatus // "" without dissent
	claimedUntil  time.Time               // zero: no projection claim
}

// verdictStoreContract specifies the rules on artifacts.VerdictStore's doc
// comment: leasing and lease expiry, pending-only decisions, human ownership
// (Lease, Decide, RecordFailure and RecordDissent), human verdicts, the
// review queue and name search, projection listing order and limit, projection claims, the
// MarkProjected fence,
// projection reset, lease-scoped failure recording, re-enqueue, Counts and
// tenant mismatch. ErrPairRefAmbiguous is not exercised: it needs two pairs
// whose keys share a 64-bit sha256 prefix, which cannot be produced cheaply.
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
	human := func(s artifacts.VerdictStatus, by string, at time.Time) artifacts.HumanVerdict {
		return artifacts.HumanVerdict{Status: s, Reviewer: by, DecidedAt: at}
	}

	BeforeEach(func() { e = newEnv() })

	enqueue := func(cs ...artifacts.PairCandidate) {
		Expect(e.store.Enqueue(e.ctx, e.tenantID, cs, t0)).To(Succeed())
	}
	// claimUntil is the claim the helpers below take at t0.
	claimUntil := t0.Add(time.Minute)
	claim := func(p artifacts.Projection, until, now time.Time) artifacts.ClaimOutcome {
		out, err := e.store.ClaimProjection(e.ctx, e.tenantID, p, until, now)
		Expect(err).NotTo(HaveOccurred())
		return out
	}
	mark := func(p artifacts.Projection, until time.Time) bool {
		ok, err := e.store.MarkProjected(e.ctx, e.tenantID, p, until)
		Expect(err).NotTo(HaveOccurred())
		return ok
	}
	// project claims p at t0 and releases the claim with MarkProjected,
	// returning MarkProjected's report.
	project := func(p artifacts.Projection) bool {
		Expect(claim(p, claimUntil, t0)).To(Equal(artifacts.ClaimAcquired))
		return mark(p, claimUntil)
	}
	unprojected := func(n int, now time.Time) []artifacts.Projection {
		proj, err := e.store.Unprojected(e.ctx, e.tenantID, n, now)
		Expect(err).NotTo(HaveOccurred())
		return proj
	}
	panelP := func(c artifacts.PairCandidate, s artifacts.VerdictStatus, at time.Time) artifacts.Projection {
		return artifacts.Projection{Key: key(c), DeterminationType: artifacts.DeterminationLLMPanel, Status: s, DecidedAt: at}
	}
	humanP := func(c artifacts.PairCandidate, s artifacts.VerdictStatus, at time.Time) artifacts.Projection {
		return artifacts.Projection{Key: key(c), DeterminationType: artifacts.DeterminationHuman, Status: s, DecidedAt: at}
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
		proj, err := e.store.Unprojected(e.ctx, e.tenantID, 10, t0)
		Expect(err).NotTo(HaveOccurred())
		Expect(proj).To(HaveLen(1))
		Expect(proj[0].Status).To(Equal(artifacts.VerdictConfirmed))
	})

	It("never leases or decides a human-owned pair", func() {
		c := cand(1)
		enqueue(c)
		e.setHuman(key(c), artifacts.VerdictPending)
		Expect(lease(t0)).To(BeEmpty())
		out, err := e.store.Decide(e.ctx, e.tenantID, key(c), verdict(artifacts.VerdictConfirmed))
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(Equal(artifacts.Decision{Outcome: artifacts.DecideHumanOwned}))
		Expect(e.row(key(c)).status).To(Equal(artifacts.VerdictPending))
		counts, err := e.store.Counts(e.ctx, e.tenantID, t0)
		Expect(err).NotTo(HaveOccurred())
		Expect(counts).To(Equal(artifacts.QueueCounts{}))
	})

	It("never counts a failure on a human-owned pair", func() {
		c := cand(1)
		enqueue(c)
		p := lease(t0)[0]
		e.setHuman(key(c), artifacts.VerdictPending)
		Expect(e.store.RecordFailure(e.ctx, e.tenantID, key(c), artifacts.Failure{Err: "x", At: t0,
			NextAttemptAt: t0, LeasedUntil: p.LeasedUntil})).To(Succeed())
		Expect(e.row(key(c)).attempts).To(BeZero())
	})

	It("stores dissent only on a human-owned pair", func() {
		human, auto := cand(1), cand(2)
		enqueue(human, auto)
		_, err := e.store.DecideHuman(e.ctx, e.tenantID, artifacts.PairRef(key(human)), artifacts.HumanVerdict{
			Status: artifacts.VerdictConfirmed, Reviewer: "alice", DecidedAt: t0})
		Expect(err).NotTo(HaveOccurred())
		Expect(e.store.RecordDissent(e.ctx, e.tenantID, key(human), verdict(artifacts.VerdictRejected))).To(Succeed())
		Expect(e.store.RecordDissent(e.ctx, e.tenantID, key(auto), verdict(artifacts.VerdictRejected))).To(Succeed())
		Expect(e.row(key(human)).hasDissent).To(BeTrue())
		Expect(e.row(key(human)).status).To(Equal(artifacts.VerdictConfirmed), "dissent must not change the human verdict")
		Expect(e.row(key(auto)).hasDissent).To(BeFalse())
	})

	It("counts pending and due pairs without human-owned ones, and unprojected verdicts of either kind", func() {
		due, later, decided, humanPending, humanConfirmed := cand(1), cand(2), cand(3), cand(4), cand(5)
		enqueue(due, decided, humanPending, humanConfirmed)
		Expect(e.store.Enqueue(e.ctx, e.tenantID, []artifacts.PairCandidate{later}, t0.Add(time.Hour))).To(Succeed())
		_, err := e.store.Decide(e.ctx, e.tenantID, key(decided), verdict(artifacts.VerdictConfirmed))
		Expect(err).NotTo(HaveOccurred())
		e.setHuman(key(humanPending), artifacts.VerdictPending)
		_, err = e.store.DecideHuman(e.ctx, e.tenantID, artifacts.PairRef(key(humanConfirmed)), human(artifacts.VerdictConfirmed, "alice", t0))
		Expect(err).NotTo(HaveOccurred())
		counts, err := e.store.Counts(e.ctx, e.tenantID, t0)
		Expect(err).NotTo(HaveOccurred())
		Expect(counts).To(Equal(artifacts.QueueCounts{Pending: 2, Due: 1, Unprojected: 2}))
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
		proj, err := e.store.Unprojected(e.ctx, e.tenantID, 2, t0)
		Expect(err).NotTo(HaveOccurred())
		Expect(proj).To(HaveLen(2))
		Expect(proj[0].Key).To(Equal(key(third)))
		Expect(proj[1].Key).To(Equal(key(first)))
	})

	It("lists decided verdicts until marked projected, and again after ResetProjection", func() {
		confirmed, undecided, humanConfirmed := cand(1), cand(2), cand(3)
		enqueue(confirmed, undecided, humanConfirmed)
		_, err := e.store.Decide(e.ctx, e.tenantID, key(confirmed), verdict(artifacts.VerdictConfirmed))
		Expect(err).NotTo(HaveOccurred())
		_, err = e.store.Decide(e.ctx, e.tenantID, key(undecided), verdict(artifacts.VerdictUndecided))
		Expect(err).NotTo(HaveOccurred())
		_, err = e.store.DecideHuman(e.ctx, e.tenantID, artifacts.PairRef(key(humanConfirmed)), human(artifacts.VerdictConfirmed, "alice", t0))
		Expect(err).NotTo(HaveOccurred())
		bothListed := ConsistOf(
			SatisfyAll(HaveField("Key", key(confirmed)), HaveField("DeterminationType", artifacts.DeterminationLLMPanel),
				HaveField("Evidence", ev)),
			SatisfyAll(HaveField("Key", key(humanConfirmed)), HaveField("DeterminationType", artifacts.DeterminationHuman),
				HaveField("Evidence", artifacts.PanelEvidence{}), HaveField("Confidence", 1.0)))
		proj, err := e.store.Unprojected(e.ctx, e.tenantID, 10, t0)
		Expect(err).NotTo(HaveOccurred())
		Expect(proj).To(bothListed)

		Expect(project(panelP(confirmed, artifacts.VerdictConfirmed, t0))).To(BeTrue())
		Expect(project(humanP(humanConfirmed, artifacts.VerdictConfirmed, t0))).To(BeTrue())
		proj, err = e.store.Unprojected(e.ctx, e.tenantID, 10, t0)
		Expect(err).NotTo(HaveOccurred())
		Expect(proj).To(BeEmpty())

		n, err := e.store.ResetProjection(e.ctx, e.tenantID)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(2), "llm_panel and human verdicts are both reset")
		proj, err = e.store.Unprojected(e.ctx, e.tenantID, 10, t0)
		Expect(err).NotTo(HaveOccurred())
		Expect(proj).To(bothListed)
		n, err = e.store.ResetProjection(e.ctx, e.tenantID)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(BeZero())
	})

	DescribeTable("never claims or marks a pair projected unless it holds a confirmed or rejected verdict",
		func(status artifacts.VerdictStatus) {
			c := cand(1)
			enqueue(c)
			if status != artifacts.VerdictPending {
				_, err := e.store.Decide(e.ctx, e.tenantID, key(c), verdict(status))
				Expect(err).NotTo(HaveOccurred())
			}
			p := panelP(c, artifacts.VerdictConfirmed, t0)
			Expect(claim(p, claimUntil, t0)).To(Equal(artifacts.ClaimReplaced))
			Expect(mark(p, claimUntil)).To(BeFalse())
			r := e.row(key(c))
			Expect(r.graphApplied).To(BeFalse())
			Expect(r.claimedUntil).To(BeZero())
		},
		Entry("pending", artifacts.VerdictPending),
		Entry("undecided", artifacts.VerdictUndecided),
	)

	It("records a human verdict on a pending pair and queues it for projection", func() {
		c := cand(1)
		enqueue(c)
		lease(t0)
		d, err := e.store.DecideHuman(e.ctx, e.tenantID, artifacts.PairRef(key(c)), human(artifacts.VerdictConfirmed, "alice", t0))
		Expect(err).NotTo(HaveOccurred())
		Expect(d).To(Equal(artifacts.HumanDecision{Outcome: artifacts.HumanApplied, Key: key(c)}))
		r := e.row(key(c))
		Expect(r.status).To(Equal(artifacts.VerdictConfirmed))
		Expect(r.determination).To(Equal(artifacts.DeterminationHuman))
		Expect(r.determinedBy).To(Equal("alice"))
		Expect(r.leased).To(BeFalse())
		Expect(r.hasDissent).To(BeFalse(), "a token_overlap row has no panel verdict to keep")
		proj, err := e.store.Unprojected(e.ctx, e.tenantID, 10, t0)
		Expect(err).NotTo(HaveOccurred())
		Expect(proj).To(ConsistOf(SatisfyAll(
			HaveField("Key", key(c)), HaveField("DeterminationType", artifacts.DeterminationHuman),
			HaveField("DeterminedBy", "alice"), HaveField("Status", artifacts.VerdictConfirmed),
			HaveField("CandidateEdgeID", c.CandidateEdgeID))))
	})

	DescribeTable("moves an llm_panel verdict to dissent when a human overrides it",
		func(panel artifacts.VerdictStatus) {
			c := cand(1)
			enqueue(c)
			_, err := e.store.Decide(e.ctx, e.tenantID, key(c), verdict(panel))
			Expect(err).NotTo(HaveOccurred())
			_, err = e.store.DecideHuman(e.ctx, e.tenantID, artifacts.PairRef(key(c)), human(artifacts.VerdictRejected, "bob", t0.Add(time.Hour)))
			Expect(err).NotTo(HaveOccurred())
			r := e.row(key(c))
			Expect(r.status).To(Equal(artifacts.VerdictRejected))
			Expect(r.determination).To(Equal(artifacts.DeterminationHuman))
			Expect(r.dissentStatus).To(Equal(panel))
			Expect(r.graphApplied).To(BeFalse())
		},
		Entry("confirmed", artifacts.VerdictConfirmed),
		Entry("undecided", artifacts.VerdictUndecided),
	)

	It("leaves a pair unchanged when a human repeats the same verdict", func() {
		c := cand(1)
		enqueue(c)
		ref := artifacts.PairRef(key(c))
		_, err := e.store.DecideHuman(e.ctx, e.tenantID, ref, human(artifacts.VerdictConfirmed, "alice", t0))
		Expect(err).NotTo(HaveOccurred())
		Expect(project(humanP(c, artifacts.VerdictConfirmed, t0))).To(BeTrue())
		d, err := e.store.DecideHuman(e.ctx, e.tenantID, ref, human(artifacts.VerdictConfirmed, "carol", t0.Add(time.Hour)))
		Expect(err).NotTo(HaveOccurred())
		Expect(d).To(Equal(artifacts.HumanDecision{Outcome: artifacts.HumanUnchanged, Key: key(c)}))
		Expect(e.row(key(c)).determinedBy).To(Equal("alice"))
		Expect(e.row(key(c)).graphApplied).To(BeTrue())
	})

	It("re-affirms a human verdict a panel contradicted afterwards, taking it off the review queue", func() {
		c := cand(1)
		enqueue(c)
		ref := artifacts.PairRef(key(c))
		_, err := e.store.DecideHuman(e.ctx, e.tenantID, ref, human(artifacts.VerdictConfirmed, "alice", t0))
		Expect(err).NotTo(HaveOccurred())
		Expect(project(humanP(c, artifacts.VerdictConfirmed, t0))).To(BeTrue())
		dissent := verdict(artifacts.VerdictRejected)
		dissent.DecidedAt = t0.Add(time.Hour)
		Expect(e.store.RecordDissent(e.ctx, e.tenantID, key(c), dissent)).To(Succeed())
		items, err := e.store.ReviewQueue(e.ctx, e.tenantID, 10)
		Expect(err).NotTo(HaveOccurred())
		Expect(items).To(ConsistOf(HaveField("Key", key(c))), "precondition: the dispute is listed")

		at := t0.Add(2 * time.Hour)
		d, err := e.store.DecideHuman(e.ctx, e.tenantID, ref, human(artifacts.VerdictConfirmed, "carol", at))
		Expect(err).NotTo(HaveOccurred())
		Expect(d).To(Equal(artifacts.HumanDecision{Outcome: artifacts.HumanReaffirmed, Key: key(c)}))
		r := e.row(key(c))
		Expect(r.status).To(Equal(artifacts.VerdictConfirmed))
		Expect(r.determination).To(Equal(artifacts.DeterminationHuman))
		Expect(r.determinedBy).To(Equal("carol"))
		Expect(r.graphApplied).To(BeFalse())
		Expect(r.dissentStatus).To(Equal(artifacts.VerdictRejected), "the panel's opinion is kept")
		items, err = e.store.ReviewQueue(e.ctx, e.tenantID, 10)
		Expect(err).NotTo(HaveOccurred())
		Expect(items).To(BeEmpty())
		Expect(unprojected(10, t0)).To(ConsistOf(SatisfyAll(
			HaveField("Key", key(c)), HaveField("DeterminationType", artifacts.DeterminationHuman),
			HaveField("Status", artifacts.VerdictConfirmed), HaveField("DeterminedBy", "carol"),
			HaveField("DecidedAt", BeTemporally("==", at)))))
	})

	DescribeTable("leaves a repeated human verdict unchanged unless a panel contradicted it afterwards",
		func(setup func(c artifacts.PairCandidate)) {
			c := cand(1)
			enqueue(c)
			setup(c)
			Expect(project(humanP(c, artifacts.VerdictConfirmed, t0.Add(time.Hour)))).To(BeTrue())
			d, err := e.store.DecideHuman(e.ctx, e.tenantID, artifacts.PairRef(key(c)), human(artifacts.VerdictConfirmed, "carol", t0.Add(3*time.Hour)))
			Expect(err).NotTo(HaveOccurred())
			Expect(d.Outcome).To(Equal(artifacts.HumanUnchanged))
			r := e.row(key(c))
			Expect(r.determinedBy).To(Equal("alice"))
			Expect(r.graphApplied).To(BeTrue())
		},
		Entry("an overridden panel rejection, which predates the human verdict", func(c artifacts.PairCandidate) {
			_, err := e.store.Decide(e.ctx, e.tenantID, key(c), verdict(artifacts.VerdictRejected))
			Expect(err).NotTo(HaveOccurred())
			_, err = e.store.DecideHuman(e.ctx, e.tenantID, artifacts.PairRef(key(c)), human(artifacts.VerdictConfirmed, "alice", t0.Add(time.Hour)))
			Expect(err).NotTo(HaveOccurred())
		}),
		Entry("a later panel that agrees", func(c artifacts.PairCandidate) {
			_, err := e.store.DecideHuman(e.ctx, e.tenantID, artifacts.PairRef(key(c)), human(artifacts.VerdictConfirmed, "alice", t0.Add(time.Hour)))
			Expect(err).NotTo(HaveOccurred())
			v := verdict(artifacts.VerdictConfirmed)
			v.DecidedAt = t0.Add(2 * time.Hour)
			Expect(e.store.RecordDissent(e.ctx, e.tenantID, key(c), v)).To(Succeed())
		}),
		Entry("a later panel that is undecided", func(c artifacts.PairCandidate) {
			_, err := e.store.DecideHuman(e.ctx, e.tenantID, artifacts.PairRef(key(c)), human(artifacts.VerdictConfirmed, "alice", t0.Add(time.Hour)))
			Expect(err).NotTo(HaveOccurred())
			v := verdict(artifacts.VerdictUndecided)
			v.DecidedAt = t0.Add(2 * time.Hour)
			Expect(e.store.RecordDissent(e.ctx, e.tenantID, key(c), v)).To(Succeed())
		}),
	)

	It("lets a human reverse an earlier human verdict, keeping the panel's dissent", func() {
		c := cand(1)
		enqueue(c)
		_, err := e.store.Decide(e.ctx, e.tenantID, key(c), verdict(artifacts.VerdictConfirmed))
		Expect(err).NotTo(HaveOccurred())
		ref := artifacts.PairRef(key(c))
		_, err = e.store.DecideHuman(e.ctx, e.tenantID, ref, human(artifacts.VerdictRejected, "alice", t0))
		Expect(err).NotTo(HaveOccurred())
		d, err := e.store.DecideHuman(e.ctx, e.tenantID, ref, human(artifacts.VerdictConfirmed, "bob", t0.Add(time.Hour)))
		Expect(err).NotTo(HaveOccurred())
		Expect(d.Outcome).To(Equal(artifacts.HumanApplied))
		r := e.row(key(c))
		Expect(r.status).To(Equal(artifacts.VerdictConfirmed))
		Expect(r.determinedBy).To(Equal("bob"))
		Expect(r.dissentStatus).To(Equal(artifacts.VerdictConfirmed), "the panel's verdict stays the dissent")
	})

	DescribeTable("refuses a human verdict it cannot apply, changing nothing",
		func(status artifacts.VerdictStatus, reviewer string, at time.Time, refOf func(string) string, want any) {
			c := cand(1)
			enqueue(c)
			_, err := e.store.DecideHuman(e.ctx, e.tenantID, refOf(key(c)), human(status, reviewer, at))
			Expect(err).To(MatchError(want))
			r := e.row(key(c))
			Expect(r.status).To(Equal(artifacts.VerdictPending))
			Expect(r.determination).To(Equal(artifacts.DeterminationTokenOverlap))
		},
		Entry("unknown ref", artifacts.VerdictConfirmed, "alice", t0, func(string) string { return "0000000000000000" }, artifacts.ErrPairNotFound),
		Entry("malformed ref", artifacts.VerdictConfirmed, "alice", t0, func(string) string { return "not-a-ref" }, ContainSubstring("lowercase hex")),
		Entry("undecided is not a human verdict", artifacts.VerdictUndecided, "alice", t0, artifacts.PairRef, ContainSubstring("confirmed or rejected")),
		Entry("pending is not a human verdict", artifacts.VerdictPending, "alice", t0, artifacts.PairRef, ContainSubstring("confirmed or rejected")),
		Entry("invalid reviewer", artifacts.VerdictConfirmed, "a\x1bb", t0, artifacts.PairRef, ContainSubstring("control, format or line-separator")),
		Entry("no decision time", artifacts.VerdictConfirmed, "alice", time.Time{}, artifacts.PairRef, ContainSubstring("decision time is not set")),
	)

	It("lists undecided, abandoned and disputed human pairs for review, oldest first, up to n", func() {
		undecided, abandoned, pending, agreed, disputed := cand(1), cand(2), cand(3), cand(4), cand(5)
		for i, c := range []artifacts.PairCandidate{disputed, undecided, abandoned, pending, agreed} {
			Expect(e.store.Enqueue(e.ctx, e.tenantID, []artifacts.PairCandidate{c}, t0.Add(time.Duration(i-10)*time.Second))).To(Succeed())
		}
		_, err := e.store.Decide(e.ctx, e.tenantID, key(undecided), verdict(artifacts.VerdictUndecided))
		Expect(err).NotTo(HaveOccurred())
		for _, lp := range lease(t0) {
			if lp.Key == key(abandoned) {
				Expect(e.store.RecordFailure(e.ctx, e.tenantID, lp.Key, artifacts.Failure{Err: "x", At: t0, NextAttemptAt: t0, Abandon: true, LeasedUntil: lp.LeasedUntil})).To(Succeed())
			}
		}
		for _, c := range []artifacts.PairCandidate{agreed, disputed} {
			_, err = e.store.DecideHuman(e.ctx, e.tenantID, artifacts.PairRef(key(c)), human(artifacts.VerdictConfirmed, "alice", t0))
			Expect(err).NotTo(HaveOccurred())
		}
		later := func(s artifacts.VerdictStatus) artifacts.Verdict {
			v := verdict(s)
			v.DecidedAt = t0.Add(time.Hour)
			return v
		}
		Expect(e.store.RecordDissent(e.ctx, e.tenantID, key(agreed), later(artifacts.VerdictConfirmed))).To(Succeed())
		Expect(e.store.RecordDissent(e.ctx, e.tenantID, key(disputed), later(artifacts.VerdictRejected))).To(Succeed())

		items, err := e.store.ReviewQueue(e.ctx, e.tenantID, 10)
		Expect(err).NotTo(HaveOccurred())
		Expect(items).To(HaveExactElements(
			SatisfyAll(HaveField("Key", key(disputed)), HaveField("Ref", artifacts.PairRef(key(disputed))),
				HaveField("Status", artifacts.VerdictConfirmed), HaveField("DissentStatus", artifacts.VerdictRejected),
				HaveField("DeterminationType", artifacts.DeterminationHuman), HaveField("SimilarityScore", 0.75),
				HaveField("Context.LowName", "a")),
			SatisfyAll(HaveField("Key", key(undecided)), HaveField("Status", artifacts.VerdictUndecided),
				HaveField("DeterminationType", artifacts.DeterminationLLMPanel), HaveField("DissentStatus", artifacts.VerdictStatus(""))),
			SatisfyAll(HaveField("Key", key(abandoned)), HaveField("Status", artifacts.VerdictAbandoned)),
		))
		items, err = e.store.ReviewQueue(e.ctx, e.tenantID, 2)
		Expect(err).NotTo(HaveOccurred())
		Expect(items).To(HaveExactElements(HaveField("Key", key(disputed)), HaveField("Key", key(undecided))))
	})

	DescribeTable("lists a human verdict for review only when the panel disagreed after it",
		func(setup func(c artifacts.PairCandidate), listed bool) {
			c := cand(1)
			enqueue(c)
			setup(c)
			items, err := e.store.ReviewQueue(e.ctx, e.tenantID, 10)
			Expect(err).NotTo(HaveOccurred())
			if listed {
				Expect(items).To(ConsistOf(SatisfyAll(HaveField("Key", key(c)), HaveField("DissentStatus", artifacts.VerdictRejected))))
			} else {
				Expect(items).To(BeEmpty())
			}
		},
		Entry("not when a human confirms an undecided panel pair", func(c artifacts.PairCandidate) {
			_, err := e.store.Decide(e.ctx, e.tenantID, key(c), verdict(artifacts.VerdictUndecided))
			Expect(err).NotTo(HaveOccurred())
			_, err = e.store.DecideHuman(e.ctx, e.tenantID, artifacts.PairRef(key(c)), human(artifacts.VerdictConfirmed, "alice", t0.Add(time.Hour)))
			Expect(err).NotTo(HaveOccurred())
		}, false),
		Entry("not when a human rejects a panel confirmation", func(c artifacts.PairCandidate) {
			_, err := e.store.Decide(e.ctx, e.tenantID, key(c), verdict(artifacts.VerdictConfirmed))
			Expect(err).NotTo(HaveOccurred())
			_, err = e.store.DecideHuman(e.ctx, e.tenantID, artifacts.PairRef(key(c)), human(artifacts.VerdictRejected, "alice", t0.Add(time.Hour)))
			Expect(err).NotTo(HaveOccurred())
		}, false),
		Entry("when a later panel rejects a human confirmation", func(c artifacts.PairCandidate) {
			_, err := e.store.DecideHuman(e.ctx, e.tenantID, artifacts.PairRef(key(c)), human(artifacts.VerdictConfirmed, "alice", t0))
			Expect(err).NotTo(HaveOccurred())
			v := verdict(artifacts.VerdictRejected)
			v.DecidedAt = t0.Add(time.Microsecond)
			Expect(e.store.RecordDissent(e.ctx, e.tenantID, key(c), v)).To(Succeed())
		}, true),
		Entry("not when a later panel is undecided on a human confirmation", func(c artifacts.PairCandidate) {
			_, err := e.store.DecideHuman(e.ctx, e.tenantID, artifacts.PairRef(key(c)), human(artifacts.VerdictConfirmed, "alice", t0))
			Expect(err).NotTo(HaveOccurred())
			v := verdict(artifacts.VerdictUndecided)
			v.DecidedAt = t0.Add(time.Hour)
			Expect(e.store.RecordDissent(e.ctx, e.tenantID, key(c), v)).To(Succeed())
		}, false),
		Entry("not when a rejecting panel opinion predates the human confirmation", func(c artifacts.PairCandidate) {
			_, err := e.store.DecideHuman(e.ctx, e.tenantID, artifacts.PairRef(key(c)), human(artifacts.VerdictConfirmed, "alice", t0))
			Expect(err).NotTo(HaveOccurred())
			Expect(e.store.RecordDissent(e.ctx, e.tenantID, key(c), verdict(artifacts.VerdictRejected))).To(Succeed())
		}, false),
	)

	DescribeTable("refuses a non-positive review batch size",
		func(n int) {
			_, err := e.store.ReviewQueue(e.ctx, e.tenantID, n)
			Expect(err).To(MatchError(ContainSubstring("must be positive")))
		},
		Entry("zero", 0),
		Entry("negative", -1),
	)

	// named is cand(i) with the given group names. Names are ASCII: the fake
	// folds case with strings.ToLower, PostgreSQL's ILIKE with the database
	// locale, and the two agree only on ASCII.
	named := func(i int, low, high string) artifacts.PairCandidate {
		c := cand(i)
		c.Context.LowName, c.Context.HighName = low, high
		return c
	}
	search := func(name string, n int) []artifacts.ReviewItem {
		items, err := e.store.SearchPairs(e.ctx, e.tenantID, name, n)
		Expect(err).NotTo(HaveOccurred())
		return items
	}

	It("finds pairs in any status whose low or high name contains the text, case-insensitively, oldest first, up to n", func() {
		pending := named(1, "Access Policy", "Access Control Policy")
		panelConfirmed := named(2, "Audit Log", "access policy register")
		panelRejected := named(3, "Backup POLICY", "ACCESS POLICY v2")
		humanRejected := named(4, "Old access policy", "New Plan")
		overridden := named(5, "access policy draft", "Draft")
		unrelated := named(6, "Incident Report", "Weekly Report")
		for i, c := range []artifacts.PairCandidate{overridden, humanRejected, unrelated, panelRejected, panelConfirmed, pending} {
			Expect(e.store.Enqueue(e.ctx, e.tenantID, []artifacts.PairCandidate{c}, t0.Add(time.Duration(i-10)*time.Second))).To(Succeed())
		}
		for _, d := range []struct {
			c artifacts.PairCandidate
			s artifacts.VerdictStatus
		}{{panelConfirmed, artifacts.VerdictConfirmed}, {panelRejected, artifacts.VerdictRejected}, {overridden, artifacts.VerdictConfirmed}} {
			_, err := e.store.Decide(e.ctx, e.tenantID, key(d.c), verdict(d.s))
			Expect(err).NotTo(HaveOccurred())
		}
		for _, c := range []artifacts.PairCandidate{humanRejected, overridden} {
			_, err := e.store.DecideHuman(e.ctx, e.tenantID, artifacts.PairRef(key(c)), human(artifacts.VerdictRejected, "alice", t0.Add(time.Hour)))
			Expect(err).NotTo(HaveOccurred())
		}

		Expect(search("aCCess POLICY", 10)).To(HaveExactElements(
			SatisfyAll(HaveField("Key", key(overridden)), HaveField("Status", artifacts.VerdictRejected),
				HaveField("DeterminationType", artifacts.DeterminationHuman), HaveField("DissentStatus", artifacts.VerdictConfirmed)),
			SatisfyAll(HaveField("Key", key(humanRejected)), HaveField("Status", artifacts.VerdictRejected),
				HaveField("DeterminationType", artifacts.DeterminationHuman), HaveField("DissentStatus", artifacts.VerdictStatus(""))),
			SatisfyAll(HaveField("Key", key(panelRejected)), HaveField("Status", artifacts.VerdictRejected),
				HaveField("DeterminationType", artifacts.DeterminationLLMPanel)),
			SatisfyAll(HaveField("Key", key(panelConfirmed)), HaveField("Ref", artifacts.PairRef(key(panelConfirmed))),
				HaveField("Status", artifacts.VerdictConfirmed), HaveField("DeterminationType", artifacts.DeterminationLLMPanel),
				HaveField("SimilarityScore", 0.75), HaveField("Context.LowName", "Audit Log"),
				HaveField("Context.HighName", "access policy register")),
			SatisfyAll(HaveField("Key", key(pending)), HaveField("Status", artifacts.VerdictPending),
				HaveField("DeterminationType", artifacts.DeterminationTokenOverlap)),
		))
		Expect(search("access policy", 2)).To(HaveExactElements(HaveField("Key", key(overridden)), HaveField("Key", key(humanRejected))))
		Expect(search("no such artifact", 10)).To(BeEmpty())
	})

	DescribeTable("matches %, _ and \\ in the search text only literally",
		func(text, literal, decoy string) {
			match, other := named(1, literal, "x"), named(2, "y", decoy)
			enqueue(match, other)
			Expect(search(text, 10)).To(ConsistOf(HaveField("Key", key(match))))
		},
		Entry("percent", "100%", "100% coverage", "100 coverage plan"),
		Entry("percent alone", "%", "50% off", "fifty off"),
		Entry("underscore", "a_b", "file a_b", "file axb"),
		Entry("backslash", `a\b`, `dir a\b`, "dir ab"),
		Entry("backslash before a wildcard", `\%`, `odd \% name`, "odd % name"),
	)

	DescribeTable("refuses an invalid search with an actionable error",
		func(name string, n int, want string) {
			_, err := e.store.SearchPairs(e.ctx, e.tenantID, name, n)
			Expect(err).To(MatchError(ContainSubstring(want)))
		},
		Entry("zero limit", "policy", 0, "must be positive"),
		Entry("negative limit", "policy", -1, "must be positive"),
		Entry("empty name", "", 10, "name is empty"),
		Entry("blank name", " \t ", 10, "name is empty"),
		Entry("name past the cap", strings.Repeat("a", 201), 10, "at most 200 bytes"),
		Entry("invalid UTF-8", "a\xffb", 10, "not valid UTF-8"),
		Entry("control character", "a\x1bb", 10, "control, format or line-separator"),
		Entry("bidi override", "a\u202eb", 10, "control, format or line-separator"),
		Entry("line separator", "a\u2028b", 10, "control, format or line-separator"),
	)

	It("refuses a search whose tenant argument differs from the context", func() {
		enqueue(cand(1))
		_, err := e.store.SearchPairs(e.ctx, e.tenantID+"-other", "a", 10)
		Expect(err).To(MatchError(ContainSubstring("does not match")))
	})

	It("fences MarkProjected to the verdict that was projected", func() {
		c := cand(1)
		enqueue(c)
		_, err := e.store.Decide(e.ctx, e.tenantID, key(c), verdict(artifacts.VerdictConfirmed))
		Expect(err).NotTo(HaveOccurred())
		panel := panelP(c, artifacts.VerdictConfirmed, t0)
		Expect(claim(panel, claimUntil, t0)).To(Equal(artifacts.ClaimAcquired))
		_, err = e.store.DecideHuman(e.ctx, e.tenantID, artifacts.PairRef(key(c)), human(artifacts.VerdictRejected, "bob", t0.Add(30*time.Second)))
		Expect(err).NotTo(HaveOccurred())
		Expect(mark(panel, claimUntil)).To(BeFalse(), "the llm_panel verdict was replaced while claimed")
		r := e.row(key(c))
		Expect(r.graphApplied).To(BeFalse(), "the human verdict stays queued")
		Expect(r.claimedUntil).To(BeZero(), "the claim is released")
		Expect(unprojected(10, t0)).To(ConsistOf(HaveField("DeterminationType", artifacts.DeterminationHuman)))

		Expect(claim(humanP(c, artifacts.VerdictRejected, t0), claimUntil, t0)).To(Equal(artifacts.ClaimReplaced),
			"same determination, different decision time")
		Expect(project(humanP(c, artifacts.VerdictRejected, t0.Add(30*time.Second)))).To(BeTrue())
		Expect(e.row(key(c)).graphApplied).To(BeTrue())
	})

	It("fences on the status, for a reversal recorded with the same decision time", func() {
		c := cand(1)
		enqueue(c)
		ref := artifacts.PairRef(key(c))
		_, err := e.store.DecideHuman(e.ctx, e.tenantID, ref, human(artifacts.VerdictConfirmed, "alice", t0))
		Expect(err).NotTo(HaveOccurred())
		confirmed := humanP(c, artifacts.VerdictConfirmed, t0)
		Expect(claim(confirmed, claimUntil, t0)).To(Equal(artifacts.ClaimAcquired))
		_, err = e.store.DecideHuman(e.ctx, e.tenantID, ref, human(artifacts.VerdictRejected, "bob", t0))
		Expect(err).NotTo(HaveOccurred())
		Expect(mark(confirmed, claimUntil)).To(BeFalse(), "the confirmation was replaced by a rejection")
		Expect(e.row(key(c)).graphApplied).To(BeFalse())
	})

	It("compares decided_at at microsecond precision", func() {
		c := cand(1)
		enqueue(c)
		v := verdict(artifacts.VerdictConfirmed)
		v.DecidedAt = t0.Add(456789123 * time.Nanosecond)
		_, err := e.store.Decide(e.ctx, e.tenantID, key(c), v)
		Expect(err).NotTo(HaveOccurred())
		Expect(claim(panelP(c, artifacts.VerdictConfirmed, t0.Add(456790000*time.Nanosecond)), claimUntil, t0)).
			To(Equal(artifacts.ClaimReplaced), "the next microsecond is a different verdict")
		Expect(project(panelP(c, artifacts.VerdictConfirmed, t0.Add(456789999*time.Nanosecond)))).
			To(BeTrue(), "times in the same microsecond are equal")
	})

	It("grants a projection claim to one caller until it expires, and hides the claimed pair meanwhile", func() {
		c := cand(1)
		enqueue(c)
		_, err := e.store.Decide(e.ctx, e.tenantID, key(c), verdict(artifacts.VerdictConfirmed))
		Expect(err).NotTo(HaveOccurred())
		p := panelP(c, artifacts.VerdictConfirmed, t0)
		Expect(claim(p, claimUntil, t0)).To(Equal(artifacts.ClaimAcquired))
		Expect(e.row(key(c)).claimedUntil).To(BeTemporally("==", claimUntil))
		Expect(unprojected(10, t0.Add(30*time.Second))).To(BeEmpty(), "a live claim hides the pair")
		Expect(claim(p, t0.Add(2*time.Minute), t0.Add(30*time.Second))).To(Equal(artifacts.ClaimBusy))

		Expect(unprojected(10, claimUntil)).To(HaveLen(1), "a claim ends at its time")
		later := t0.Add(3 * time.Minute)
		Expect(claim(p, later, claimUntil)).To(Equal(artifacts.ClaimAcquired), "an expired claim is taken over")
		Expect(mark(p, claimUntil)).To(BeFalse(), "the expired claim cannot release the new one")
		r := e.row(key(c))
		Expect(r.graphApplied).To(BeFalse())
		Expect(r.claimedUntil).To(BeTemporally("==", later), "nothing changed")
		Expect(mark(p, later)).To(BeTrue())
		r = e.row(key(c))
		Expect(r.graphApplied).To(BeTrue())
		Expect(r.claimedUntil).To(BeZero())
		Expect(claim(p, t0.Add(4*time.Minute), later)).To(Equal(artifacts.ClaimBusy), "a projected pair is not claimed again")
	})

	It("changes nothing when MarkProjected names a claim the pair does not hold", func() {
		c := cand(1)
		enqueue(c)
		_, err := e.store.Decide(e.ctx, e.tenantID, key(c), verdict(artifacts.VerdictConfirmed))
		Expect(err).NotTo(HaveOccurred())
		p := panelP(c, artifacts.VerdictConfirmed, t0)
		Expect(mark(p, claimUntil)).To(BeFalse(), "an unclaimed pair")
		Expect(e.row(key(c)).graphApplied).To(BeFalse())
		Expect(claim(p, claimUntil, t0)).To(Equal(artifacts.ClaimAcquired))
		Expect(mark(p, claimUntil.Add(time.Microsecond))).To(BeFalse(), "a foreign claim")
		r := e.row(key(c))
		Expect(r.graphApplied).To(BeFalse())
		Expect(r.claimedUntil).To(BeTemporally("==", claimUntil))
		ok, err := e.store.MarkProjected(e.ctx, e.tenantID, artifacts.Projection{Key: "nope", DeterminationType: artifacts.DeterminationLLMPanel,
			Status: artifacts.VerdictConfirmed, DecidedAt: t0}, claimUntil)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse(), "a pair that is not queued")
	})

	It("releases only the caller's own claim, leaving the pair queued and claimable at once", func() {
		c := cand(1)
		enqueue(c)
		_, err := e.store.Decide(e.ctx, e.tenantID, key(c), verdict(artifacts.VerdictConfirmed))
		Expect(err).NotTo(HaveOccurred())
		p := panelP(c, artifacts.VerdictConfirmed, t0)
		Expect(claim(p, claimUntil, t0)).To(Equal(artifacts.ClaimAcquired))

		Expect(e.store.ReleaseClaim(e.ctx, e.tenantID, key(c), claimUntil.Add(time.Microsecond))).To(Succeed())
		Expect(e.row(key(c)).claimedUntil).To(BeTemporally("==", claimUntil), "a foreign claim is kept")
		Expect(e.store.ReleaseClaim(e.ctx, e.tenantID, "nope", claimUntil)).To(Succeed(), "an unknown pair is a no-op")

		Expect(e.store.ReleaseClaim(e.ctx, e.tenantID, key(c), claimUntil)).To(Succeed())
		r := e.row(key(c))
		Expect(r.claimedUntil).To(BeZero())
		Expect(r.graphApplied).To(BeFalse(), "releasing writes no projection state")
		Expect(unprojected(10, t0)).To(HaveLen(1))
		Expect(claim(p, claimUntil, t0)).To(Equal(artifacts.ClaimAcquired), "claimable again without waiting")
	})

	It("refuses a claim that ends before it starts, and a claim on a pair that is not queued", func() {
		c := cand(1)
		enqueue(c)
		_, err := e.store.Decide(e.ctx, e.tenantID, key(c), verdict(artifacts.VerdictConfirmed))
		Expect(err).NotTo(HaveOccurred())
		_, err = e.store.ClaimProjection(e.ctx, e.tenantID, panelP(c, artifacts.VerdictConfirmed, t0), t0, t0)
		Expect(err).To(MatchError(ContainSubstring("must be after now")))
		Expect(e.row(key(c)).claimedUntil).To(BeZero())
		_, err = e.store.ClaimProjection(e.ctx, e.tenantID, artifacts.Projection{Key: "nope", DeterminationType: artifacts.DeterminationLLMPanel,
			Status: artifacts.VerdictConfirmed, DecidedAt: t0}, claimUntil, t0)
		Expect(err).To(MatchError(ContainSubstring("pair is not queued")))
	})

	It("keeps a claim through a human verdict and reports it while it is live", func() {
		c := cand(1)
		enqueue(c)
		_, err := e.store.Decide(e.ctx, e.tenantID, key(c), verdict(artifacts.VerdictConfirmed))
		Expect(err).NotTo(HaveOccurred())
		Expect(claim(panelP(c, artifacts.VerdictConfirmed, t0), claimUntil, t0)).To(Equal(artifacts.ClaimAcquired))
		ref := artifacts.PairRef(key(c))
		d, err := e.store.DecideHuman(e.ctx, e.tenantID, ref, human(artifacts.VerdictRejected, "bob", t0.Add(30*time.Second)))
		Expect(err).NotTo(HaveOccurred())
		Expect(d.ClaimedUntil).To(BeTemporally("==", claimUntil))
		Expect(e.row(key(c)).claimedUntil).To(BeTemporally("==", claimUntil))
		rejected := humanP(c, artifacts.VerdictRejected, t0.Add(30*time.Second))
		Expect(claim(rejected, t0.Add(2*time.Minute), t0.Add(40*time.Second))).To(Equal(artifacts.ClaimBusy),
			"the panel claimant is still writing")

		d, err = e.store.DecideHuman(e.ctx, e.tenantID, ref, human(artifacts.VerdictConfirmed, "carol", claimUntil))
		Expect(err).NotTo(HaveOccurred())
		Expect(d.Outcome).To(Equal(artifacts.HumanApplied))
		Expect(d.ClaimedUntil).To(BeZero(), "the claim ended at the decision time")
		Expect(e.row(key(c)).claimedUntil).To(BeTemporally("==", claimUntil), "an expired claim is kept too")

		d, err = e.store.DecideHuman(e.ctx, e.tenantID, ref, human(artifacts.VerdictConfirmed, "dave", t0))
		Expect(err).NotTo(HaveOccurred())
		Expect(d.Outcome).To(Equal(artifacts.HumanUnchanged))
		Expect(d.ClaimedUntil).To(BeTemporally("==", claimUntil), "a repeated verdict reports a claim live at its decision time too")
	})

	It("leaves claims alone on Decide and ResetProjection", func() {
		claimed, projected := cand(1), cand(2)
		enqueue(claimed, projected)
		for _, c := range []artifacts.PairCandidate{claimed, projected} {
			_, err := e.store.Decide(e.ctx, e.tenantID, key(c), verdict(artifacts.VerdictConfirmed))
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(project(panelP(projected, artifacts.VerdictConfirmed, t0))).To(BeTrue())
		Expect(claim(panelP(claimed, artifacts.VerdictConfirmed, t0), claimUntil, t0)).To(Equal(artifacts.ClaimAcquired))
		out, err := e.store.Decide(e.ctx, e.tenantID, key(claimed), verdict(artifacts.VerdictRejected))
		Expect(err).NotTo(HaveOccurred())
		Expect(out.Outcome).To(Equal(artifacts.DecideStale))
		n, err := e.store.ResetProjection(e.ctx, e.tenantID)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(1), "only the projected pair; a claimed pair is never projected")
		Expect(e.row(key(claimed)).claimedUntil).To(BeTemporally("==", claimUntil))
		Expect(unprojected(10, t0)).To(ConsistOf(HaveField("Key", key(projected))))
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
		proj, err := e.store.Unprojected(e.ctx, e.tenantID, 10, t0)
		Expect(err).NotTo(HaveOccurred())
		Expect(proj).To(HaveLen(1))
		Expect(proj[0].CandidateEdgeID).To(Equal(decided.CandidateEdgeID))
	})

	It("refuses a tenant argument that differs from the context", func() {
		_, err := e.store.Lease(e.ctx, e.tenantID+"-other", 10, time.Minute, t0)
		Expect(err).To(MatchError(ContainSubstring("does not match")))
	})
}
