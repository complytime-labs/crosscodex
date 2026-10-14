//go:build !integration

package artifacts_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/codes"

	"github.com/complytime-labs/crosscodex/internal/analyzer/artifacts"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
)

func (f *adjFixture) decideHuman(status artifacts.VerdictStatus, by string, at time.Time) {
	_, err := decideCurrent(f.ctx, f.store, adjTenant, artifacts.PairRef(f.key),
		artifacts.HumanVerdict{Status: status, Reviewer: by, DecidedAt: at})
	Expect(err).NotTo(HaveOccurred())
}

// project runs a projection pass the way `admin adjudicate artifacts decide`
// does: with no LLM client and no prompt registry.
func (f *adjFixture) project() (artifacts.AdjudicateResult, error) {
	return artifacts.NewAdjudicator(f.g, f.store, nil, nil, f.cfg).Project(f.ctx, adjTenant, f.opts.Now)
}

func humanEdgeID(low, high string, at time.Time) string {
	return graphdb.DerivedID("same-as", "human", low, high, graphdb.FormatTime(at))
}

var _ = Describe("Adjudicator human verdicts", func() {
	var f *adjFixture
	BeforeEach(func() { f = newAdjFixture() })

	It("projects a human confirmation: a human SAME_AS edge, candidate superseded", func() {
		f.decideHuman(artifacts.VerdictConfirmed, "alice", f.now)
		res, err := f.project()
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(artifacts.AdjudicateResult{Recovered: 1}))
		live := f.liveSameAs()
		Expect(live).To(HaveLen(1))
		Expect(live[0].Source.ID).To(Equal(f.low))
		Expect(live[0].Target.ID).To(Equal(f.high))
		e := live[0].Edge
		Expect(e.ID).To(Equal(humanEdgeID(f.low, f.high, f.now)))
		Expect(e.DeterminationType).To(Equal("human"))
		Expect(e.DeterminedBy).To(Equal("alice"))
		Expect(e.Confidence).To(Equal(1.0))
		Expect(e.Supersedes).To(Equal(f.candidate))
		Expect(e.ValidFrom).To(BeTemporally("==", f.now))
		Expect(f.candidateSuperseded()).To(BeTrue())
		Expect(f.store.row(adjTenant, f.key).graphApplied).To(BeTrue())
	})

	It("projects a human rejection: no live SAME_AS edge remains", func() {
		f.decideHuman(artifacts.VerdictRejected, "alice", f.now)
		res, err := f.project()
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(artifacts.AdjudicateResult{Recovered: 1}))
		Expect(f.liveSameAs()).To(BeEmpty())
		c, err := f.g.GetEdge(f.ctx, adjTenant, f.candidate)
		Expect(err).NotTo(HaveOccurred())
		Expect(*c.ValidTo).To(BeTemporally("==", f.now))
		Expect(f.store.row(adjTenant, f.key).graphApplied).To(BeTrue())
	})

	It("overrides a projected panel confirmation, superseding the llm_panel edge", func() {
		_, err := f.run()
		Expect(err).NotTo(HaveOccurred())
		f.decideHuman(artifacts.VerdictRejected, "bob", f.now.Add(time.Hour))
		_, err = f.project()
		Expect(err).NotTo(HaveOccurred())
		Expect(f.liveSameAs()).To(BeEmpty())
		llm, err := f.g.GetEdge(f.ctx, adjTenant, graphdb.DerivedID("same-as", "llm_panel", f.low, f.high))
		Expect(err).NotTo(HaveOccurred())
		Expect(*llm.ValidTo).To(BeTemporally("==", f.now.Add(time.Hour)))
		c, err := f.g.GetEdge(f.ctx, adjTenant, f.candidate)
		Expect(err).NotTo(HaveOccurred())
		Expect(*c.ValidTo).To(BeTemporally("==", f.now), "the panel's supersede time is kept")
	})

	It("writes a fresh edge when a human reverses and re-confirms", func() {
		t1, t2, t3 := f.now, f.now.Add(time.Hour), f.now.Add(2*time.Hour)
		f.decideHuman(artifacts.VerdictConfirmed, "alice", t1)
		_, err := f.project()
		Expect(err).NotTo(HaveOccurred())
		f.decideHuman(artifacts.VerdictRejected, "bob", t2)
		_, err = f.project()
		Expect(err).NotTo(HaveOccurred())
		f.decideHuman(artifacts.VerdictConfirmed, "alice", t3)
		_, err = f.project()
		Expect(err).NotTo(HaveOccurred())
		live := f.liveSameAs()
		Expect(live).To(HaveLen(1))
		Expect(live[0].Edge.ID).To(Equal(humanEdgeID(f.low, f.high, t3)))
		first, err := f.g.GetEdge(f.ctx, adjTenant, humanEdgeID(f.low, f.high, t1))
		Expect(err).NotTo(HaveOccurred())
		Expect(*first.ValidTo).To(BeTemporally("==", t2), "history is kept, not rewritten")
	})

	It("writes a fresh edge when a human re-affirms a confirmation a later panel rejected", func() {
		t1, t2, t3 := f.now, f.now.Add(time.Hour), f.now.Add(2*time.Hour)
		f.decideHuman(artifacts.VerdictConfirmed, "alice", t1)
		_, err := f.project()
		Expect(err).NotTo(HaveOccurred())
		Expect(f.store.RecordDissent(f.ctx, adjTenant, f.key, artifacts.Verdict{Status: artifacts.VerdictRejected, Confidence: 1, DecidedAt: t2})).To(Succeed())
		f.decideHuman(artifacts.VerdictConfirmed, "carol", t3)
		res, err := f.project()
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(artifacts.AdjudicateResult{Recovered: 1}))
		live := f.liveSameAs()
		Expect(live).To(HaveLen(1))
		Expect(live[0].Edge.ID).To(Equal(humanEdgeID(f.low, f.high, t3)))
		Expect(live[0].Edge.DeterminedBy).To(Equal("carol"))
		first, err := f.g.GetEdge(f.ctx, adjTenant, humanEdgeID(f.low, f.high, t1))
		Expect(err).NotTo(HaveOccurred())
		Expect(*first.ValidTo).To(BeTemporally("==", t3), "the earlier human edge ends at the re-affirmation")
		Expect(f.store.row(adjTenant, f.key).graphApplied).To(BeTrue())
	})

	DescribeTable("is idempotent: re-projecting keeps the live edges and the valid_to of superseded ones",
		func(status artifacts.VerdictStatus, live int) {
			f.decideHuman(status, "alice", f.now)
			_, err := f.project()
			Expect(err).NotTo(HaveOccurred())
			f.store.setUnprojected(adjTenant, f.key)
			res, err := f.project()
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(Equal(artifacts.AdjudicateResult{Recovered: 1}))
			Expect(f.liveSameAs()).To(HaveLen(live))
			c, err := f.g.GetEdge(f.ctx, adjTenant, f.candidate)
			Expect(err).NotTo(HaveOccurred())
			Expect(*c.ValidTo).To(BeTemporally("==", f.now))
			Expect(f.store.row(adjTenant, f.key).graphApplied).To(BeTrue())
		},
		Entry("confirmation", artifacts.VerdictConfirmed, 1),
		Entry("rejection", artifacts.VerdictRejected, 0),
	)

	It("never ends an edge before it began when the human decision time precedes the edge", func() {
		_, err := f.run()
		Expect(err).NotTo(HaveOccurred())
		skewed := f.now.Add(-time.Minute) // the reviewer's clock runs behind the adjudicator's
		f.decideHuman(artifacts.VerdictRejected, "bob", skewed)
		_, err = f.project()
		Expect(err).NotTo(HaveOccurred())
		Expect(f.liveSameAs()).To(BeEmpty())
		llm, err := f.g.GetEdge(f.ctx, adjTenant, graphdb.DerivedID("same-as", "llm_panel", f.low, f.high))
		Expect(err).NotTo(HaveOccurred())
		Expect(llm.ValidFrom).To(BeTemporally("==", f.now))
		Expect(*llm.ValidTo).To(BeTemporally("==", f.now), "superseded at its own valid_from, not before it")
	})

	It("counts a confirmation whose groups are gone as orphaned and marks it projected", func() {
		f.store.set(adjTenant, artifacts.PairCandidate{LowGroupID: "gone-a", HighGroupID: "gone-b", CandidateEdgeID: "gone-edge"},
			artifacts.VerdictPending, artifacts.DeterminationTokenOverlap)
		gone := artifacts.PairKey("gone-a", "gone-b")
		_, err := decideCurrent(f.ctx, f.store, adjTenant, artifacts.PairRef(gone),
			artifacts.HumanVerdict{Status: artifacts.VerdictConfirmed, Reviewer: "alice", DecidedAt: f.now})
		Expect(err).NotTo(HaveOccurred())
		res, err := f.project()
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(artifacts.AdjudicateResult{Recovered: 1, Orphaned: 1}))
		Expect(f.store.row(adjTenant, gone).graphApplied).To(BeTrue())
	})

	It("projects nothing and touches no LLM when the queue is empty", func() {
		res, err := f.project()
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(artifacts.AdjudicateResult{}))
		Expect(f.liveSameAs()).To(HaveLen(1), "the candidate stays live")
		Expect(f.store.row(adjTenant, f.key).status).To(Equal(artifacts.VerdictPending))
	})

	DescribeTable("stops on a graph failure and leaves the human verdict queued",
		func(status artifacts.VerdictStatus, wrap func(graphdb.GraphDB) graphdb.GraphDB, step string) {
			f.decideHuman(status, "alice", f.now)
			_, err := artifacts.NewAdjudicator(wrap(f.g), f.store, nil, nil, f.cfg).Project(f.ctx, adjTenant, f.opts.Now)
			Expect(err).To(MatchError(ContainSubstring(step)))
			Expect(err).To(MatchError(ContainSubstring("pair " + artifacts.PairRef(f.key))))
			Expect(err).NotTo(MatchError(ContainSubstring(f.key)), "errors name pairs by ref, never by raw key")
			Expect(err).To(MatchError(ContainSubstring("graph down")))
			Expect(f.store.row(adjTenant, f.key).graphApplied).To(BeFalse())
			Expect(f.candidateSuperseded()).To(BeFalse())
		},
		Entry("writing the human edge", artifacts.VerdictConfirmed,
			func(g graphdb.GraphDB) graphdb.GraphDB { return failingCreateEdge{g} }, "write human SAME_AS for"),
		Entry("listing the live SAME_AS edges", artifacts.VerdictRejected,
			func(g graphdb.GraphDB) graphdb.GraphDB { return failingTraverse{g} }, "list SAME_AS edges for"),
		Entry("superseding a live edge", artifacts.VerdictRejected,
			func(g graphdb.GraphDB) graphdb.GraphDB { return failingSupersede{g} }, "supersede"),
	)

	It("stops when the store cannot load the queue", func() {
		f.decideHuman(artifacts.VerdictRejected, "alice", f.now)
		f.store.err["Unprojected"] = errors.New("db down")
		_, err := f.project()
		Expect(err).To(MatchError(ContainSubstring("load unprojected verdicts: db down")))
		Expect(f.candidateSuperseded()).To(BeFalse())
	})

	Describe("the amendment-10 race", func() {
		It("human decides after Decide, before MarkProjected: the run supersedes its own llm_panel edge", func() {
			rs := &racingStore{fakeVerdictStore: f.store}
			rs.hook = func() { f.decideHuman(artifacts.VerdictRejected, "bob", f.now) }
			res, err := artifacts.NewAdjudicator(f.g, rs, f.llm, f.prompts, f.cfg).Run(f.ctx, adjTenant, f.opts)
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(Equal(artifacts.AdjudicateResult{Leased: 1, Confirmed: 1, Fenced: 1, Recovered: 1}),
				"end-of-run recovery projects the human verdict")
			Expect(f.liveSameAs()).To(BeEmpty())
			r := f.store.row(adjTenant, f.key)
			Expect(r.determination).To(Equal(artifacts.DeterminationHuman))
			Expect(r.status).To(Equal(artifacts.VerdictRejected))
			Expect(r.graphApplied).To(BeTrue())
		})

		It("human decides and projects after MarkProjected: the human projection supersedes the llm_panel edge", func() {
			_, err := f.run()
			Expect(err).NotTo(HaveOccurred())
			f.decideHuman(artifacts.VerdictRejected, "bob", f.now.Add(time.Minute))
			res, err := f.project()
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(Equal(artifacts.AdjudicateResult{Recovered: 1}))
			Expect(f.liveSameAs()).To(BeEmpty())
		})

		It("a concurrent pass skips the pair the run holds: the run's fence re-queues the human verdict and recovery supersedes the llm_panel edge", func() {
			rs := &racingStore{fakeVerdictStore: f.store}
			rs.hook = func() {
				f.decideHuman(artifacts.VerdictRejected, "bob", f.now)
				res, err := f.project() // run B, before run A's MarkProjected
				Expect(err).NotTo(HaveOccurred())
				Expect(res).To(Equal(artifacts.AdjudicateResult{}), "run A's claim hides the pair from run B")
				Expect(f.store.row(adjTenant, f.key).graphApplied).To(BeFalse())
			}
			res, err := artifacts.NewAdjudicator(f.g, rs, f.llm, f.prompts, f.cfg).Run(f.ctx, adjTenant, f.opts)
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(Equal(artifacts.AdjudicateResult{Leased: 1, Confirmed: 1, Fenced: 1, Recovered: 1}))
			Expect(f.liveSameAs()).To(BeEmpty())
			Expect(f.store.row(adjTenant, f.key).graphApplied).To(BeTrue())
		})

		It("human decides before the run claims its panel verdict: the llm_panel edge is never written", func() {
			humanAt := f.now.Add(time.Minute)
			rs := &racingStore{fakeVerdictStore: f.store}
			rs.claimHook = func() { f.decideHuman(artifacts.VerdictRejected, "bob", humanAt) }
			res, err := artifacts.NewAdjudicator(f.g, rs, f.llm, f.prompts, f.cfg).Run(f.ctx, adjTenant, f.opts)
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(Equal(artifacts.AdjudicateResult{Leased: 1, Confirmed: 1, Fenced: 1, Recovered: 1}))
			_, err = f.g.GetEdge(f.ctx, adjTenant, graphdb.DerivedID("same-as", "llm_panel", f.low, f.high))
			Expect(err).To(MatchError(graphdb.ErrEdgeNotFound))
			c, err := f.g.GetEdge(f.ctx, adjTenant, f.candidate)
			Expect(err).NotTo(HaveOccurred())
			Expect(*c.ValidTo).To(BeTemporally("==", humanAt), "the human projection supersedes the candidate")
			Expect(f.store.row(adjTenant, f.key).graphApplied).To(BeTrue())
		})

		It("a concurrent pass skips the pair while the run writes its llm_panel edge: the human verdict is projected after it", func() {
			humanAt := f.now.Add(time.Minute)
			hg := &racingGraph{GraphDB: f.g, determination: artifacts.DeterminationLLMPanel}
			hg.hook = func() {
				f.decideHuman(artifacts.VerdictRejected, "bob", humanAt)
				res, err := f.project() // run B, before run A writes its edge
				Expect(err).NotTo(HaveOccurred())
				Expect(res).To(Equal(artifacts.AdjudicateResult{}), "run A's claim hides the pair from run B")
			}
			res, err := artifacts.NewAdjudicator(hg, f.store, f.llm, f.prompts, f.cfg).Run(f.ctx, adjTenant, f.opts)
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(Equal(artifacts.AdjudicateResult{Leased: 1, Confirmed: 1, Fenced: 1, Recovered: 1}))
			Expect(f.liveSameAs()).To(BeEmpty())
			c, err := f.g.GetEdge(f.ctx, adjTenant, f.candidate)
			Expect(err).NotTo(HaveOccurred())
			Expect(*c.ValidTo).To(BeTemporally("==", f.now), "the panel superseded the candidate first")
			llm, err := f.g.GetEdge(f.ctx, adjTenant, graphdb.DerivedID("same-as", "llm_panel", f.low, f.high))
			Expect(err).NotTo(HaveOccurred())
			Expect(*llm.ValidTo).To(BeTemporally("==", humanAt))
			Expect(f.store.row(adjTenant, f.key).graphApplied).To(BeTrue())
		})

		It("a run that crashes after writing its llm_panel edge leaves it to a pass after the claim expires", func() {
			panelAt, humanAt := f.now, f.now.Add(time.Minute)
			hg := &racingGraph{GraphDB: f.g, determination: artifacts.DeterminationLLMPanel}
			hg.hook = func() {
				f.decideHuman(artifacts.VerdictRejected, "bob", humanAt)
				res, err := f.project() // the decide command's pass
				Expect(err).NotTo(HaveOccurred())
				Expect(res).To(Equal(artifacts.AdjudicateResult{}), "the run's claim hides the pair")
				f.store.err["MarkProjected"] = errors.New("crash") // the run dies before releasing its claim
			}
			_, err := artifacts.NewAdjudicator(hg, f.store, f.llm, f.prompts, f.cfg).Run(f.ctx, adjTenant, f.opts)
			Expect(err).To(MatchError(ContainSubstring("crash")))
			delete(f.store.err, "MarkProjected")
			live := f.liveSameAs()
			Expect(live).To(HaveLen(1))
			Expect(live[0].Edge.DeterminationType).To(Equal("llm_panel"), "the crash strands the panel's edge for now")
			Expect(f.store.row(adjTenant, f.key).graphApplied).To(BeFalse(), "the human verdict is still queued")

			f.now = panelAt.Add(f.cfg.LeaseDuration - time.Second)
			res, err := f.project()
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(Equal(artifacts.AdjudicateResult{}), "the claim is still live")

			f.now = panelAt.Add(f.cfg.LeaseDuration)
			res, err = f.project()
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(Equal(artifacts.AdjudicateResult{Recovered: 1}))
			Expect(f.liveSameAs()).To(BeEmpty())
			llm, err := f.g.GetEdge(f.ctx, adjTenant, graphdb.DerivedID("same-as", "llm_panel", f.low, f.high))
			Expect(err).NotTo(HaveOccurred())
			Expect(*llm.ValidTo).To(BeTemporally("==", humanAt))
			c, err := f.g.GetEdge(f.ctx, adjTenant, f.candidate)
			Expect(err).NotTo(HaveOccurred())
			Expect(*c.ValidTo).To(BeTemporally("==", panelAt))
			r := f.store.row(adjTenant, f.key)
			Expect(r.determination).To(Equal(artifacts.DeterminationHuman))
			Expect(r.graphApplied).To(BeTrue())
		})

		It("a reversal fenced inside a projection pass is projected before the pass returns", func() {
			t1, t2 := f.now, f.now.Add(time.Minute)
			f.decideHuman(artifacts.VerdictConfirmed, "alice", t1)
			hg := &racingGraph{GraphDB: f.g, determination: artifacts.DeterminationHuman}
			hg.hook = func() {
				// A reviewer reverses the confirmation before pass A writes its
				// edge; pass B cannot project the reversal meanwhile.
				f.decideHuman(artifacts.VerdictRejected, "bob", t2)
				res, err := f.project()
				Expect(err).NotTo(HaveOccurred())
				Expect(res).To(Equal(artifacts.AdjudicateResult{}), "pass A's claim hides the pair from pass B")
			}
			res, err := artifacts.NewAdjudicator(hg, f.store, nil, nil, f.cfg).Project(f.ctx, adjTenant, f.opts.Now)
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(Equal(artifacts.AdjudicateResult{Recovered: 2, Fenced: 1}))
			Expect(f.liveSameAs()).To(BeEmpty(), "the graph matches the rejection, the latest human verdict")
			late, err := f.g.GetEdge(f.ctx, adjTenant, humanEdgeID(f.low, f.high, t1))
			Expect(err).NotTo(HaveOccurred())
			Expect(*late.ValidTo).To(BeTemporally("==", t2))
			r := f.store.row(adjTenant, f.key)
			Expect(r.status).To(Equal(artifacts.VerdictRejected))
			Expect(r.graphApplied).To(BeTrue())
		})
	})

	Describe("projection claims", func() {
		It("skips a verdict another pass has claimed, and projects it once that claim expires", func() {
			f.decideHuman(artifacts.VerdictRejected, "alice", f.now)
			rs := &racingStore{fakeVerdictStore: f.store}
			rs.claimHook = func() {
				// Another pass claims the verdict between this pass's
				// Unprojected and its claim.
				p := artifacts.Projection{Key: f.key, DeterminationType: artifacts.DeterminationHuman,
					Status: artifacts.VerdictRejected, DecidedAt: f.now}
				out, err := f.store.ClaimProjection(f.ctx, adjTenant, p, f.now.Add(time.Minute), f.now)
				Expect(err).NotTo(HaveOccurred())
				Expect(out).To(Equal(artifacts.ClaimAcquired))
			}
			res, err := artifacts.NewAdjudicator(f.g, rs, nil, nil, f.cfg).Project(f.ctx, adjTenant, f.opts.Now)
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(Equal(artifacts.AdjudicateResult{Busy: 1}))
			Expect(f.candidateSuperseded()).To(BeFalse(), "nothing is written")
			Expect(f.store.row(adjTenant, f.key).graphApplied).To(BeFalse())

			f.now = f.now.Add(time.Minute)
			res, err = f.project()
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(Equal(artifacts.AdjudicateResult{Recovered: 1}))
			Expect(f.candidateSuperseded()).To(BeTrue())
		})

		It("counts a projection whose claim expired and was taken over as fenced, and leaves the pair to the pass that took it over", func() {
			f.decideHuman(artifacts.VerdictConfirmed, "alice", f.now)
			hg := &racingGraph{GraphDB: f.g, determination: artifacts.DeterminationHuman}
			hg.hook = func() {
				// Pass A stalls past lease_duration; pass B takes the pair over.
				f.now = f.now.Add(f.cfg.LeaseDuration)
				res, err := f.project()
				Expect(err).NotTo(HaveOccurred())
				Expect(res).To(Equal(artifacts.AdjudicateResult{Recovered: 1}))
			}
			res, err := artifacts.NewAdjudicator(hg, f.store, nil, nil, f.cfg).Project(f.ctx, adjTenant, f.opts.Now)
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(Equal(artifacts.AdjudicateResult{Recovered: 1, Fenced: 1}))
			Expect(f.liveSameAs()).To(HaveLen(1))
			r := f.store.row(adjTenant, f.key)
			Expect(r.graphApplied).To(BeTrue(), "pass B marked it; pass A's late release changed nothing")
			Expect(r.claimedUntil).To(BeZero())
		})

		It("releases the claim when a projection fails before writing, so the next pass retries at once", func() {
			f.decideHuman(artifacts.VerdictRejected, "alice", f.now)
			_, err := artifacts.NewAdjudicator(failingTraverse{f.g}, f.store, nil, nil, f.cfg).Project(f.ctx, adjTenant, f.opts.Now)
			Expect(err).To(MatchError(ContainSubstring("list SAME_AS edges for pair " + artifacts.PairRef(f.key) + ": graph down")))
			Expect(f.store.row(adjTenant, f.key).claimedUntil).To(BeZero())
			res, err := f.project()
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(Equal(artifacts.AdjudicateResult{Recovered: 1}))
			Expect(f.candidateSuperseded()).To(BeTrue())
		})

		It("keeps the claim when a graph write fails, since the write may have landed", func() {
			f.decideHuman(artifacts.VerdictConfirmed, "alice", f.now)
			_, err := artifacts.NewAdjudicator(failingCreateEdge{f.g}, f.store, nil, nil, f.cfg).Project(f.ctx, adjTenant, f.opts.Now)
			Expect(err).To(MatchError(ContainSubstring("write human SAME_AS for pair " + artifacts.PairRef(f.key))))
			res, err := f.project()
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(Equal(artifacts.AdjudicateResult{}), "the claim is held until lease_duration passes")
			f.now = f.now.Add(f.cfg.LeaseDuration)
			res, err = f.project()
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(Equal(artifacts.AdjudicateResult{Recovered: 1}))
			Expect(f.liveSameAs()).To(HaveLen(1))
		})

		It("reports both errors when releasing the claim after a failure fails too", func() {
			f.decideHuman(artifacts.VerdictRejected, "alice", f.now)
			f.store.err["ReleaseClaim"] = errors.New("db down")
			_, err := artifacts.NewAdjudicator(failingTraverse{f.g}, f.store, nil, nil, f.cfg).Project(f.ctx, adjTenant, f.opts.Now)
			Expect(err).To(MatchError(ContainSubstring("list SAME_AS edges for pair " + artifacts.PairRef(f.key) + ": graph down")))
			Expect(err).To(MatchError(ContainSubstring("release projection claim on pair " + artifacts.PairRef(f.key) + ": db down")))
			Expect(f.store.row(adjTenant, f.key).claimedUntil).NotTo(BeZero(), "the claim then expires on its own")
		})

		It("names a pair by ref, so a hostile artifact name in its key cannot reach a terminal through an error", func() {
			hostile := artifacts.PairCandidate{LowGroupID: "evil\x1b]0;x\x07-a", HighGroupID: "evil\x1b]0;x\x07-b", CandidateEdgeID: "evil-edge"}
			f.store.set(adjTenant, hostile, artifacts.VerdictPending, artifacts.DeterminationTokenOverlap)
			key := artifacts.PairKey(hostile.LowGroupID, hostile.HighGroupID)
			_, err := decideCurrent(f.ctx, f.store, adjTenant, artifacts.PairRef(key),
				artifacts.HumanVerdict{Status: artifacts.VerdictConfirmed, Reviewer: "alice", DecidedAt: f.now})
			Expect(err).NotTo(HaveOccurred())
			f.store.err["ClaimProjection"] = errors.New("db down")
			_, err = f.project()
			Expect(err).To(MatchError(ContainSubstring("claim projection of pair " + artifacts.PairRef(key) + ": db down")))
			Expect(err.Error()).NotTo(ContainSubstring(key))
			Expect(err.Error()).NotTo(ContainSubstring("\x1b"))
			Expect(err.Error()).NotTo(ContainSubstring("\x07"))
		})

		It("stops when Project has no clock", func() {
			_, err := artifacts.NewAdjudicator(f.g, f.store, nil, nil, f.cfg).Project(f.ctx, adjTenant, nil)
			Expect(err).To(MatchError(ContainSubstring("now must be set")))
		})

		It("stops when the store cannot claim, leaving the verdict queued", func() {
			f.decideHuman(artifacts.VerdictRejected, "alice", f.now)
			f.store.err["ClaimProjection"] = errors.New("db down")
			_, err := f.project()
			Expect(err).To(MatchError(ContainSubstring("claim projection of pair " + artifacts.PairRef(f.key) + ": db down")))
			Expect(f.candidateSuperseded()).To(BeFalse())
			Expect(f.store.row(adjTenant, f.key).graphApplied).To(BeFalse())
		})
	})

	Describe("telemetry", func() {
		var tp *telemetrytest.TestProvider
		BeforeEach(func() {
			var err error
			tp, err = telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { Expect(tp.Shutdown(context.Background())).To(Succeed()) })
		})

		It("traces a projection pass with the tenant and logs its summary", func() {
			var logs bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			DeferCleanup(func() { slog.SetDefault(prev) })
			f.decideHuman(artifacts.VerdictRejected, "alice", f.now)
			_, err := artifacts.NewAdjudicator(f.g, f.store, nil, nil, f.cfg,
				artifacts.WithAdjudicatorTelemetry(tp.TracerProvider(), tp.MeterProvider())).Project(f.ctx, adjTenant, f.opts.Now)
			Expect(err).NotTo(HaveOccurred())
			span := telemetrytest.FindSpan(tp.GetSpans(), "artifacts.adjudicate.project")
			Expect(span).NotTo(BeNil())
			v, ok := telemetrytest.SpanAttribute(span, "tenant.id")
			Expect(ok).To(BeTrue())
			Expect(v.AsString()).To(Equal(adjTenant))
			v, ok = telemetrytest.SpanAttribute(span, "adjudicate.recovered")
			Expect(ok).To(BeTrue())
			Expect(v.AsInt64()).To(Equal(int64(1)))
			Expect(span.Status().Code).To(Equal(codes.Ok))
			Expect(logs.String()).To(MatchRegexp(`msg="artifact verdict projection finished".*stopped_early=false recovered=1 orphaned=0 fenced=0 busy=0`))
		})

		It("marks the projection span as failed when the pass stops", func() {
			f.store.err["Unprojected"] = errors.New("db down")
			_, err := artifacts.NewAdjudicator(f.g, f.store, nil, nil, f.cfg,
				artifacts.WithAdjudicatorTelemetry(tp.TracerProvider(), tp.MeterProvider())).Project(f.ctx, adjTenant, f.opts.Now)
			Expect(err).To(HaveOccurred())
			span := telemetrytest.FindSpan(tp.GetSpans(), "artifacts.adjudicate.project")
			Expect(span).NotTo(BeNil())
			Expect(span.Status().Code).To(Equal(codes.Error))
		})

		It("counts and logs a fence trip", func() {
			var logs bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			DeferCleanup(func() { slog.SetDefault(prev) })
			rs := &racingStore{fakeVerdictStore: f.store}
			rs.hook = func() { f.decideHuman(artifacts.VerdictRejected, "bob", f.now) }
			_, err := artifacts.NewAdjudicator(f.g, rs, f.llm, f.prompts, f.cfg,
				artifacts.WithAdjudicatorTelemetry(tp.TracerProvider(), tp.MeterProvider())).Run(f.ctx, adjTenant, f.opts)
			Expect(err).NotTo(HaveOccurred())
			Expect(counterByAttr(tp, "artifacts.adjudications.total", "outcome", "fenced")).To(Equal(int64(1)))
			Expect(logs.String()).To(MatchRegexp(`msg="artifact pair adjudicated".*outcome=fenced`))
			Expect(logs.String()).To(MatchRegexp(`msg="artifact adjudication run finished".*recovered=1 .*orphaned=0 fenced=1 `))
		})
	})
})

// racingStore runs hook once, just before the adjudicator's first
// MarkProjected of an llm_panel verdict (after Decide, the claim and the
// graph write), and claimHook once, just before the first ClaimProjection of
// any verdict. Either may be nil. The hooks may call the embedded fake's
// methods: they take its mutex, and the intercepted call runs only after the
// hook returns.
type racingStore struct {
	*fakeVerdictStore
	hook, claimHook func()
	once, claimOnce sync.Once
}

func (r *racingStore) ClaimProjection(ctx context.Context, tenantID string, p artifacts.Projection, until, now time.Time) (artifacts.ClaimOutcome, error) {
	if r.claimHook != nil {
		r.claimOnce.Do(r.claimHook)
	}
	return r.fakeVerdictStore.ClaimProjection(ctx, tenantID, p, until, now)
}

func (r *racingStore) MarkProjected(ctx context.Context, tenantID string, p artifacts.Projection, claimedUntil time.Time) (bool, error) {
	if r.hook != nil && p.DeterminationType == artifacts.DeterminationLLMPanel {
		r.once.Do(r.hook)
	}
	return r.fakeVerdictStore.MarkProjected(ctx, tenantID, p, claimedUntil)
}

// racingGraph runs hook once, just before the first CreateEdge of an edge
// with the given determination type: after the verdict is recorded, before
// the projection writes its edge.
type racingGraph struct {
	graphdb.GraphDB
	determination string
	hook          func()
	once          sync.Once
}

func (r *racingGraph) CreateEdge(ctx context.Context, tenant, sourceID, targetID string, e graphdb.Edge) error {
	if e.DeterminationType == r.determination {
		r.once.Do(r.hook)
	}
	return r.GraphDB.CreateEdge(ctx, tenant, sourceID, targetID, e)
}

// failingGetEdge fails every GetEdge.
type failingGetEdge struct{ graphdb.GraphDB }

func (failingGetEdge) GetEdge(context.Context, string, string) (*graphdb.EdgeWithEndpoints, error) {
	return nil, errors.New("graph down")
}

// failingTraverse fails every Traverse.
type failingTraverse struct{ graphdb.GraphDB }

func (failingTraverse) Traverse(context.Context, string, graphdb.TraversalQuery) ([]graphdb.Path, error) {
	return nil, errors.New("graph down")
}
