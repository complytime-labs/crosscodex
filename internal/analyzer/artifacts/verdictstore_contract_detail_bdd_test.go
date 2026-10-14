package artifacts_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/analyzer/artifacts"
)

// reviewDetailContract specifies what a reviewer sees and what DecideHuman's
// expect token guards: the vote split on ReviewQueue and SearchPairs items,
// PairDetail, and Expect. It runs against fakeVerdictStore and PGVerdictStore
// like verdictStoreContract. Undecodable stored JSON and RLS are PG-only
// specs: the fake stores typed values and one tenant per env.
func reviewDetailContract(newEnv func() verdictStoreContractEnv) {
	var e verdictStoreContractEnv
	t0 := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	yes, no := true, false
	panelEv := artifacts.PanelEvidence{PromptName: "artifact_same_as", PromptVersion: "1.0.0", Models: []string{"m1", "m2", "m3"},
		Votes: []artifacts.VoteRecord{
			{VoterID: "m1", Decision: &yes, Confidence: "high", Justification: "same product"},
			{VoterID: "m2", Decision: &no, Confidence: "medium", Justification: "different edition"},
			{VoterID: "m3"},
		}, ValidVotes: 2, TotalVotes: 3}
	split := artifacts.VoteSplit{Same: 1, Valid: 2, Total: 3}
	pair := func(low, high string) artifacts.PairCandidate {
		return artifacts.PairCandidate{LowGroupID: low, HighGroupID: high, CandidateEdgeID: "edge-" + low + "-" + high,
			SimilarityScore: 0.75, Context: artifacts.PairContext{Type: "document", LowName: "n-" + low, HighName: "n-" + high}}
	}
	cand := func(i int) artifacts.PairCandidate {
		return pair(fmt.Sprintf("d-%03d-a", i), fmt.Sprintf("d-%03d-b", i))
	}
	key := func(c artifacts.PairCandidate) string { return artifacts.PairKey(c.LowGroupID, c.HighGroupID) }
	ref := func(c artifacts.PairCandidate) string { return artifacts.PairRef(key(c)) }

	BeforeEach(func() { e = newEnv() })

	enqueue := func(cs ...artifacts.PairCandidate) {
		Expect(e.store.Enqueue(e.ctx, e.tenantID, cs, t0)).To(Succeed())
	}
	decide := func(c artifacts.PairCandidate, s artifacts.VerdictStatus, at time.Time) {
		out, err := e.store.Decide(e.ctx, e.tenantID, key(c), artifacts.Verdict{Status: s, Confidence: 0.5, Evidence: panelEv, DecidedAt: at})
		Expect(err).NotTo(HaveOccurred())
		Expect(out.Outcome).To(Equal(artifacts.DecideApplied))
	}
	abandon := func(c artifacts.PairCandidate, msg string) {
		pairs, err := e.store.Lease(e.ctx, e.tenantID, 10, time.Minute, t0)
		Expect(err).NotTo(HaveOccurred())
		for _, p := range pairs {
			if p.Key == key(c) {
				Expect(e.store.RecordFailure(e.ctx, e.tenantID, p.Key, artifacts.Failure{Err: msg, At: t0, NextAttemptAt: t0,
					Abandon: true, LeasedUntil: p.LeasedUntil})).To(Succeed())
				return
			}
		}
		Fail("pair was not leased")
	}
	queueItem := func(c artifacts.PairCandidate) artifacts.ReviewItem {
		items, err := e.store.ReviewQueue(e.ctx, e.tenantID, 100)
		Expect(err).NotTo(HaveOccurred())
		for _, it := range items {
			if it.Key == key(c) {
				return it
			}
		}
		Fail("pair is not on the review queue")
		return artifacts.ReviewItem{}
	}
	searchItem := func(c artifacts.PairCandidate) artifacts.ReviewItem {
		items, err := e.store.SearchPairs(e.ctx, e.tenantID, c.Context.LowName, 100)
		Expect(err).NotTo(HaveOccurred())
		Expect(items).To(HaveLen(1))
		return items[0]
	}

	Describe("vote split", func() {
		It("reports the split of an undecided panel verdict on the review queue", func() {
			c := cand(1)
			enqueue(c)
			decide(c, artifacts.VerdictUndecided, t0)
			it := queueItem(c)
			Expect(it.Votes).To(Equal(&split))
			Expect(it.VotesUnreadable).To(BeFalse())
		})

		It("reports the split of a confirmed panel verdict found by name", func() {
			c := cand(1)
			enqueue(c)
			decide(c, artifacts.VerdictConfirmed, t0)
			Expect(searchItem(c).Votes).To(Equal(&split))
		})

		It("has no split for a pending, an abandoned or a human-owned pair", func() {
			pending, abandoned, human := cand(1), cand(2), cand(3)
			enqueue(pending, abandoned, human)
			abandon(abandoned, "boom")
			decide(human, artifacts.VerdictConfirmed, t0)
			_, err := decideCurrent(e.ctx, e.store, e.tenantID, ref(human),
				artifacts.HumanVerdict{Status: artifacts.VerdictRejected, Reviewer: "alice", DecidedAt: t0.Add(time.Hour)})
			Expect(err).NotTo(HaveOccurred())
			Expect(searchItem(pending).Votes).To(BeNil())
			Expect(queueItem(abandoned).Votes).To(BeNil())
			Expect(searchItem(human).Votes).To(BeNil())
		})
	})

	detail := func(c artifacts.PairCandidate, related int) artifacts.PairDetail {
		d, err := e.store.PairDetail(e.ctx, e.tenantID, ref(c), related)
		Expect(err).NotTo(HaveOccurred())
		Expect(artifacts.ValidateExpect(d.Expect)).To(Succeed())
		return d
	}

	Describe("PairDetail", func() {
		It("shows a pending pair with no verdict", func() {
			c := cand(1)
			enqueue(c)
			d := detail(c, 10)
			Expect(d.Ref).To(Equal(ref(c)))
			Expect(d.Key).To(Equal(key(c)))
			Expect(d.Status).To(Equal(artifacts.VerdictPending))
			Expect(d.DeterminationType).To(Equal(artifacts.DeterminationTokenOverlap))
			Expect(d.Context).To(Equal(c.Context))
			Expect(d.SimilarityScore).To(Equal(c.SimilarityScore))
			Expect(d.DecidedAt.IsZero()).To(BeTrue())
			Expect(d.Evidence).To(BeNil())
			Expect(d.Votes).To(BeNil())
			Expect(d.Dissent).To(BeNil())
			Expect(d.Related).To(BeEmpty())
			Expect(d.RelatedTotal).To(BeZero())
		})

		DescribeTable("shows a panel verdict's evidence and vote split",
			func(s artifacts.VerdictStatus) {
				c := cand(1)
				enqueue(c)
				decide(c, s, t0)
				d := detail(c, 10)
				Expect(d.Status).To(Equal(s))
				Expect(d.DeterminationType).To(Equal(artifacts.DeterminationLLMPanel))
				Expect(d.DeterminedBy).To(Equal("artifact-adjudicator"))
				Expect(d.Confidence).To(HaveValue(Equal(0.5)))
				Expect(d.DecidedAt).To(BeTemporally("==", t0))
				Expect(d.Evidence).To(HaveValue(Equal(panelEv)))
				Expect(d.EvidenceErr).NotTo(HaveOccurred())
				Expect(d.Votes).To(Equal(&split))
			},
			Entry("confirmed", artifacts.VerdictConfirmed),
			Entry("rejected", artifacts.VerdictRejected),
			Entry("undecided", artifacts.VerdictUndecided),
		)

		It("shows why an abandoned pair failed", func() {
			c := cand(1)
			enqueue(c)
			abandon(c, "panel timed out")
			d := detail(c, 10)
			Expect(d.Status).To(Equal(artifacts.VerdictAbandoned))
			Expect(d.LastError).To(Equal("panel timed out"))
		})

		It("shows the panel verdict a human overrode as dissent, not disputed", func() {
			c := cand(1)
			enqueue(c)
			decide(c, artifacts.VerdictConfirmed, t0)
			_, err := decideCurrent(e.ctx, e.store, e.tenantID, ref(c),
				artifacts.HumanVerdict{Status: artifacts.VerdictRejected, Reviewer: "alice", DecidedAt: t0.Add(time.Hour)})
			Expect(err).NotTo(HaveOccurred())
			d := detail(c, 10)
			Expect(d.Status).To(Equal(artifacts.VerdictRejected))
			Expect(d.DeterminationType).To(Equal(artifacts.DeterminationHuman))
			Expect(d.DeterminedBy).To(Equal("alice"))
			Expect(d.Evidence).To(BeNil())
			Expect(d.Votes).To(BeNil())
			Expect(d.Disputed).To(BeFalse())
			Expect(d.Dissent).NotTo(BeNil())
			Expect(d.Dissent.Status).To(Equal(artifacts.VerdictConfirmed))
			Expect(d.Dissent.Confidence).To(Equal(0.5))
			Expect(d.Dissent.DeterminedBy).To(Equal("artifact-adjudicator"))
			Expect(d.Dissent.DecidedAt).To(BeTemporally("==", t0))
			Expect(d.Dissent.Evidence).To(HaveValue(Equal(panelEv)))
			Expect(d.Dissent.EvidenceErr).NotTo(HaveOccurred())
			Expect(d.DissentStatus).To(Equal(artifacts.VerdictConfirmed))
		})

		DescribeTable("shows a panel opinion given after a human verdict, disputed only when it disagrees",
			func(dissent artifacts.VerdictStatus, disputed bool) {
				c := cand(1)
				enqueue(c)
				_, err := decideCurrent(e.ctx, e.store, e.tenantID, ref(c),
					artifacts.HumanVerdict{Status: artifacts.VerdictConfirmed, Reviewer: "alice", DecidedAt: t0})
				Expect(err).NotTo(HaveOccurred())
				Expect(e.store.RecordDissent(e.ctx, e.tenantID, key(c),
					artifacts.Verdict{Status: dissent, Confidence: 0.5, Evidence: panelEv, DecidedAt: t0.Add(time.Hour)})).To(Succeed())
				d := detail(c, 10)
				Expect(d.Disputed).To(Equal(disputed))
				Expect(d.Dissent.Status).To(Equal(dissent))
				Expect(d.Dissent.DecidedAt).To(BeTemporally("==", t0.Add(time.Hour)))
			},
			Entry("disagrees", artifacts.VerdictRejected, true),
			Entry("agrees", artifacts.VerdictConfirmed, false),
		)

		Describe("related pairs", func() {
			// x shares group r-1 with y (as y's low group) and with v (as v's
			// high group), and r-2 with z; w shares neither.
			x, y, z, v, w := pair("r-1", "r-2"), pair("r-1", "r-3"), pair("r-2", "r-4"), pair("r-0", "r-1"), pair("r-3", "r-4")
			related := func(c artifacts.PairCandidate, s artifacts.VerdictStatus, determination string) artifacts.RelatedPair {
				return artifacts.RelatedPair{Ref: ref(c), Status: s, DeterminationType: determination,
					LowName: c.Context.LowName, HighName: c.Context.HighName}
			}
			// byKey orders candidates by pair key in byte order, the tie-break
			// after decided_at.
			byKey := func(cs ...artifacts.PairCandidate) []artifacts.PairCandidate {
				slices.SortFunc(cs, func(a, b artifacts.PairCandidate) int { return strings.Compare(key(a), key(b)) })
				return cs
			}

			BeforeEach(func() {
				enqueue(x, y, z, v, w)
				decide(z, artifacts.VerdictConfirmed, t0.Add(time.Hour))
			})

			It("lists the other pairs that share a group, newest decision first and undecided last, without the pair itself", func() {
				d := detail(x, 10)
				Expect(d.RelatedTotal).To(Equal(3))
				want := []artifacts.RelatedPair{related(z, artifacts.VerdictConfirmed, artifacts.DeterminationLLMPanel)}
				for _, c := range byKey(y, v) {
					want = append(want, related(c, artifacts.VerdictPending, artifacts.DeterminationTokenOverlap))
				}
				Expect(d.Related).To(Equal(want))
			})

			It("orders pairs decided at the same time by key", func() {
				decide(y, artifacts.VerdictRejected, t0)
				decide(v, artifacts.VerdictConfirmed, t0)
				d := detail(x, 10)
				Expect(d.Related).To(HaveLen(3))
				Expect(d.Related[0].Ref).To(Equal(ref(z)))
				sorted := byKey(y, v)
				Expect([]string{d.Related[1].Ref, d.Related[2].Ref}).To(Equal([]string{ref(sorted[0]), ref(sorted[1])}))
			})

			It("caps the list at the limit and still counts them all", func() {
				d := detail(x, 1)
				Expect(d.Related).To(HaveLen(1))
				Expect(d.Related[0].Ref).To(Equal(ref(z)))
				Expect(d.RelatedTotal).To(Equal(3))
			})
		})

		DescribeTable("refuses a lookup it cannot answer",
			func(refOf func() string, related int, want any) {
				enqueue(cand(1))
				_, err := e.store.PairDetail(e.ctx, e.tenantID, refOf(), related)
				Expect(err).To(MatchError(want))
			},
			Entry("unknown ref", func() string { return "0000000000000000" }, 10, artifacts.ErrPairNotFound),
			Entry("malformed ref", func() string { return "not-a-ref" }, 10, ContainSubstring("lowercase hex")),
			Entry("zero related limit", func() string { return ref(cand(1)) }, 0, ContainSubstring("must be positive")),
		)

		It("refuses a tenant argument that differs from the context", func() {
			_, err := e.store.PairDetail(e.ctx, "someone-else", "0000000000000000", 10)
			Expect(err).To(MatchError(ContainSubstring("does not match")))
		})
	})

	Describe("DecideHuman expect token", func() {
		viewed := func(c artifacts.PairCandidate) string { return detail(c, 1).Expect }
		decideWith := func(c artifacts.PairCandidate, s artifacts.VerdictStatus, by, expect string, at time.Time) (artifacts.HumanDecision, error) {
			return e.store.DecideHuman(e.ctx, e.tenantID, ref(c), artifacts.HumanVerdict{Status: s, Reviewer: by, DecidedAt: at, Expect: expect})
		}
		// expectRefusedUnchanged asserts the safety-mechanism triple: an
		// error, an actionable message naming the current verdict, and the
		// row unchanged.
		expectRefusedUnchanged := func(c artifacts.PairCandidate, err error, now artifacts.VerdictStatus, by string) {
			Expect(err).To(MatchError(artifacts.ErrPairChanged))
			var changed *artifacts.PairChangedError
			Expect(errors.As(err, &changed)).To(BeTrue())
			Expect(changed.Status).To(Equal(now))
			Expect(changed.DeterminedBy).To(Equal(by))
			Expect(err.Error()).To(ContainSubstring("it is now " + string(now)))
		}

		It("records a verdict whose token matches the pair", func() {
			c := cand(1)
			enqueue(c)
			d, err := decideWith(c, artifacts.VerdictConfirmed, "alice", viewed(c), t0)
			Expect(err).NotTo(HaveOccurred())
			Expect(d.Outcome).To(Equal(artifacts.HumanApplied))
		})

		It("refuses a stale token after another reviewer decided, keeping their verdict", func() {
			c := cand(1)
			enqueue(c)
			stale := viewed(c)
			_, err := decideWith(c, artifacts.VerdictRejected, "bob", viewed(c), t0)
			Expect(err).NotTo(HaveOccurred())
			before := e.row(key(c))
			_, err = decideWith(c, artifacts.VerdictConfirmed, "alice", stale, t0.Add(time.Minute))
			expectRefusedUnchanged(c, err, artifacts.VerdictRejected, "bob")
			Expect(e.row(key(c))).To(Equal(before))
		})

		It("refuses a stale token after the panel decided", func() {
			c := cand(1)
			enqueue(c)
			stale := viewed(c)
			decide(c, artifacts.VerdictConfirmed, t0)
			before := e.row(key(c))
			_, err := decideWith(c, artifacts.VerdictRejected, "alice", stale, t0.Add(time.Minute))
			expectRefusedUnchanged(c, err, artifacts.VerdictConfirmed, "artifact-adjudicator")
			Expect(e.row(key(c))).To(Equal(before))
		})

		It("refuses to re-affirm with a token taken before the panel's later dissent, leaving the pair disputed", func() {
			c := cand(1)
			enqueue(c)
			_, err := decideWith(c, artifacts.VerdictConfirmed, "alice", viewed(c), t0)
			Expect(err).NotTo(HaveOccurred())
			stale := viewed(c)
			Expect(e.store.RecordDissent(e.ctx, e.tenantID, key(c),
				artifacts.Verdict{Status: artifacts.VerdictRejected, Confidence: 1, Evidence: panelEv, DecidedAt: t0.Add(time.Hour)})).To(Succeed())
			before := e.row(key(c))
			_, err = decideWith(c, artifacts.VerdictConfirmed, "alice", stale, t0.Add(2*time.Hour))
			expectRefusedUnchanged(c, err, artifacts.VerdictConfirmed, "alice")
			Expect(e.row(key(c))).To(Equal(before))
			Expect(detail(c, 1).Disputed).To(BeTrue())
		})

		It("accepts a stale token as unchanged when the pair already holds the same undisputed human verdict", func() {
			c := cand(1)
			enqueue(c)
			stale := viewed(c)
			_, err := decideWith(c, artifacts.VerdictConfirmed, "alice", stale, t0)
			Expect(err).NotTo(HaveOccurred())
			before := e.row(key(c))
			d, err := decideWith(c, artifacts.VerdictConfirmed, "alice", stale, t0.Add(time.Minute))
			Expect(err).NotTo(HaveOccurred())
			Expect(d.Outcome).To(Equal(artifacts.HumanUnchanged))
			Expect(e.row(key(c))).To(Equal(before))
		})

		It("accepts a stale token as unchanged when another reviewer already recorded the same verdict, keeping theirs", func() {
			c := cand(1)
			enqueue(c)
			stale := viewed(c)
			_, err := decideWith(c, artifacts.VerdictConfirmed, "bob", viewed(c), t0)
			Expect(err).NotTo(HaveOccurred())
			before := e.row(key(c))
			d, err := decideWith(c, artifacts.VerdictConfirmed, "alice", stale, t0.Add(time.Minute))
			Expect(err).NotTo(HaveOccurred())
			Expect(d.Outcome).To(Equal(artifacts.HumanUnchanged))
			Expect(e.row(key(c))).To(Equal(before))
			Expect(e.row(key(c)).determinedBy).To(Equal("bob"))
		})

		DescribeTable("rejects a missing or malformed token before reading the pair",
			func(expect string) {
				c := cand(1)
				enqueue(c)
				before := e.row(key(c))
				_, err := decideWith(c, artifacts.VerdictConfirmed, "alice", expect, t0)
				Expect(err).To(MatchError(ContainSubstring("copy the EXPECT line")))
				Expect(e.row(key(c))).To(Equal(before))
			},
			Entry("empty", ""),
			Entry("uppercase", "0123456789ABCDEF"),
			Entry("short", "0123"),
		)

		DescribeTable("keeps the token through queue and projection bookkeeping",
			func(touch func(c artifacts.PairCandidate)) {
				c := cand(1)
				enqueue(c)
				before := viewed(c)
				touch(c)
				Expect(viewed(c)).To(Equal(before))
			},
			Entry("a lease", func(artifacts.PairCandidate) {
				_, err := e.store.Lease(e.ctx, e.tenantID, 10, time.Minute, t0)
				Expect(err).NotTo(HaveOccurred())
			}),
			Entry("a recorded, retried failure", func(c artifacts.PairCandidate) {
				pairs, err := e.store.Lease(e.ctx, e.tenantID, 10, time.Minute, t0)
				Expect(err).NotTo(HaveOccurred())
				Expect(e.store.RecordFailure(e.ctx, e.tenantID, key(c), artifacts.Failure{Err: "x", At: t0,
					NextAttemptAt: t0.Add(time.Hour), LeasedUntil: pairs[0].LeasedUntil})).To(Succeed())
			}),
		)

		It("keeps the token through a projection claim and mark", func() {
			c := cand(1)
			enqueue(c)
			decide(c, artifacts.VerdictConfirmed, t0)
			before := viewed(c)
			p := artifacts.Projection{Key: key(c), DeterminationType: artifacts.DeterminationLLMPanel, Status: artifacts.VerdictConfirmed, DecidedAt: t0}
			out, err := e.store.ClaimProjection(e.ctx, e.tenantID, p, t0.Add(time.Minute), t0)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(Equal(artifacts.ClaimAcquired))
			Expect(viewed(c)).To(Equal(before))
			ok, err := e.store.MarkProjected(e.ctx, e.tenantID, p, t0.Add(time.Minute))
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeTrue())
			Expect(viewed(c)).To(Equal(before))
		})

		It("changes the token when a re-enqueue moves a pending pair to a new score", func() {
			c := cand(1)
			enqueue(c)
			before := viewed(c)
			c.SimilarityScore = 0.9
			enqueue(c)
			Expect(viewed(c)).NotTo(Equal(before))
		})
	})
}
