//go:build !integration

package artifacts_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/analyzer/artifacts"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
)

var _ = Describe("Adjudicator queue", func() {
	var f *adjFixture
	BeforeEach(func() { f = newAdjFixture() })

	// decide records v on the fixture pair as if an earlier run had crashed
	// before projecting it.
	decide := func(status artifacts.VerdictStatus) {
		_, err := f.store.Lease(f.ctx, adjTenant, 1, 1, f.now)
		Expect(err).NotTo(HaveOccurred())
		out, err := f.store.Decide(f.ctx, adjTenant, f.key, artifacts.Verdict{Status: status, Confidence: 1,
			Evidence: artifacts.PanelEvidence{PromptName: "artifact_same_as", PromptVersion: "9.9.9"}, DecidedAt: f.now})
		Expect(err).NotTo(HaveOccurred())
		Expect(out.Outcome).To(Equal(artifacts.DecideApplied))
	}

	noLLM := func(*llmclient.CompletionRequest) (string, error) {
		return "", errors.New("the LLM must not be called")
	}

	Describe("recovery", func() {
		It("projects a decided verdict an earlier run left unprojected, before leasing", func() {
			decide(artifacts.VerdictConfirmed)
			f.llm.reply = noLLM
			res, err := f.run()
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(Equal(artifacts.AdjudicateResult{Recovered: 1}))
			Expect(f.liveSameAs()).To(HaveLen(1))
			Expect(f.liveSameAs()[0].Edge.DeterminationType).To(Equal("llm_panel"))
			Expect(f.candidateSuperseded()).To(BeTrue())
			Expect(f.store.row(adjTenant, f.key).graphApplied).To(BeTrue())
		})

		It("re-projects idempotently when the edge already exists", func() {
			_, err := f.run()
			Expect(err).NotTo(HaveOccurred())

			f.opts.Reproject = true
			res, err := f.run()
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(Equal(artifacts.AdjudicateResult{Reset: 1, Recovered: 1}))
			Expect(f.liveSameAs()).To(HaveLen(1))
			Expect(f.store.row(adjTenant, f.key).graphApplied).To(BeTrue())
		})

		It("resets only llm_panel verdicts, never a human one", func() {
			_, err := f.run()
			Expect(err).NotTo(HaveOccurred())
			human := artifacts.PairCandidate{LowGroupID: "h-a", HighGroupID: "h-b", CandidateEdgeID: "h-edge"}
			f.store.set(adjTenant, human, artifacts.VerdictConfirmed, artifacts.DeterminationHuman)
			humanKey := artifacts.PairKey(human.LowGroupID, human.HighGroupID)
			f.store.setProjected(adjTenant, humanKey)

			f.opts.Reproject = true
			res, err := f.run()
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Reset).To(Equal(1))
			Expect(res.Recovered).To(Equal(1))
			Expect(f.store.row(adjTenant, f.key).graphApplied).To(BeTrue(), "llm_panel row re-projected")
			Expect(f.store.row(adjTenant, humanKey).graphApplied).To(BeTrue(), "human row stays projected")
		})

		It("re-projects without leasing a due pair or calling the LLM", func() {
			f.opts.Reproject = true
			f.llm.reply = noLLM
			res, err := f.run()
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(Equal(artifacts.AdjudicateResult{}))
			Expect(f.llm.calls).To(BeEmpty())
			row := f.store.row(adjTenant, f.key)
			Expect(row.status).To(Equal(artifacts.VerdictPending), "the due pair waits for a normal run")
			Expect(row.leasedUntil).To(BeZero())
		})

		It("stops before projecting or leasing when the reset fails", func() {
			decide(artifacts.VerdictConfirmed)
			f.opts.Reproject = true
			f.store.err["ResetProjection"] = errors.New("db down")
			f.llm.reply = noLLM
			res, err := f.run()
			Expect(err).To(MatchError(ContainSubstring("reset projection: db down")))
			Expect(res).To(Equal(artifacts.AdjudicateResult{}))
			Expect(f.store.row(adjTenant, f.key).graphApplied).To(BeFalse(), "the decided verdict stays unprojected")
			for _, r := range f.liveSameAs() {
				Expect(r.Edge.DeterminationType).NotTo(Equal("llm_panel"), "no llm_panel edge is written")
			}
		})

		It("marks a confirmation whose groups are gone as orphaned instead of failing", func() {
			ghost := artifacts.PairCandidate{LowGroupID: "ghost-a", HighGroupID: "ghost-b", CandidateEdgeID: "ghost-edge"}
			Expect(f.store.Enqueue(f.ctx, adjTenant, []artifacts.PairCandidate{ghost}, f.now)).To(Succeed())
			_, err := f.store.Decide(f.ctx, adjTenant, artifacts.PairKey("ghost-a", "ghost-b"),
				artifacts.Verdict{Status: artifacts.VerdictConfirmed, Confidence: 1, DecidedAt: f.now})
			Expect(err).NotTo(HaveOccurred())

			res, err := f.run()
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Recovered).To(Equal(1))
			Expect(res.Orphaned).To(Equal(1))
			Expect(f.store.row(adjTenant, artifacts.PairKey("ghost-a", "ghost-b")).graphApplied).To(BeTrue())
		})

		It("recovers more verdicts than one batch holds", func() {
			var cands []artifacts.PairCandidate
			for i := range 150 {
				cands = append(cands, artifacts.PairCandidate{LowGroupID: fmt.Sprintf("r-%03d-a", i),
					HighGroupID: fmt.Sprintf("r-%03d-b", i), CandidateEdgeID: fmt.Sprintf("r-edge-%03d", i)})
			}
			Expect(f.store.Enqueue(f.ctx, adjTenant, cands, f.now)).To(Succeed())
			for _, c := range cands {
				_, err := f.store.Decide(f.ctx, adjTenant, artifacts.PairKey(c.LowGroupID, c.HighGroupID),
					artifacts.Verdict{Status: artifacts.VerdictRejected, Confidence: 1, DecidedAt: f.now})
				Expect(err).NotTo(HaveOccurred())
			}
			res, err := f.run()
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Recovered).To(Equal(150))
		})

		It("leaves a confirmation unprojected when the edge write fails, and projects it on a later healthy run", func() {
			healthy := f.g
			f.g = failingCreateEdge{GraphDB: healthy}
			res, err := f.run()
			Expect(err).To(MatchError(ContainSubstring("write SAME_AS")))
			Expect(err).To(MatchError(ContainSubstring("graph down")))
			Expect(res.Confirmed).To(Equal(1))
			r := f.store.row(adjTenant, f.key)
			Expect(r.status).To(Equal(artifacts.VerdictConfirmed))
			Expect(r.graphApplied).To(BeFalse())
			Expect(f.candidateSuperseded()).To(BeFalse())

			f.g = healthy
			res, err = f.run()
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(Equal(artifacts.AdjudicateResult{Recovered: 1}))
			Expect(f.liveSameAs()).To(HaveLen(1))
			Expect(f.candidateSuperseded()).To(BeTrue())
			Expect(f.store.row(adjTenant, f.key).graphApplied).To(BeTrue())
		})

		It("finishes on the next run a confirmation that crashed between the edge write and the supersede", func() {
			decide(artifacts.VerdictConfirmed)
			healthy := f.g
			f.g = &failingSupersede{GraphDB: healthy}
			_, err := f.run()
			Expect(err).To(MatchError(ContainSubstring("supersede")))
			f.g = healthy
			Expect(f.liveSameAs()).To(HaveLen(2), "the crash leaves the llm_panel edge and the candidate both live")

			res, err := f.run()
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(Equal(artifacts.AdjudicateResult{Recovered: 1}))
			live := f.liveSameAs()
			Expect(live).To(HaveLen(1))
			Expect(live[0].Edge.DeterminationType).To(Equal("llm_panel"))
			Expect(f.candidateSuperseded()).To(BeTrue())
			Expect(f.store.row(adjTenant, f.key).graphApplied).To(BeTrue())
		})

		It("recovers before it leases", func() {
			decide(artifacts.VerdictConfirmed)
			Expect(f.store.Enqueue(f.ctx, adjTenant, []artifacts.PairCandidate{
				{LowGroupID: "o-a", HighGroupID: "o-b", CandidateEdgeID: "o-edge"}}, f.now)).To(Succeed())
			var (
				once           sync.Once
				recoveredFirst bool
			)
			f.llm.reply = func(*llmclient.CompletionRequest) (string, error) {
				once.Do(func() { recoveredFirst = f.store.row(adjTenant, f.key).graphApplied })
				return answer("NO"), nil
			}
			res, err := f.run()
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(Equal(artifacts.AdjudicateResult{Recovered: 1, Leased: 1, Rejected: 1}))
			Expect(recoveredFirst).To(BeTrue(), "the decided verdict was projected before the panel ran")
		})

		It("stops the run when projection fails for a reason other than missing groups", func() {
			decide(artifacts.VerdictConfirmed)
			f.g = &failingSupersede{GraphDB: f.g}
			_, err := f.run()
			Expect(err).To(MatchError(ContainSubstring("supersede")))
			Expect(f.store.row(adjTenant, f.key).graphApplied).To(BeFalse())
		})
	})

	Describe("budget", func() {
		BeforeEach(func() {
			var cands []artifacts.PairCandidate
			for i := range 24 {
				cands = append(cands, artifacts.PairCandidate{LowGroupID: fmt.Sprintf("b-%03d-a", i),
					HighGroupID: fmt.Sprintf("b-%03d-b", i), CandidateEdgeID: fmt.Sprintf("b-edge-%03d", i)})
			}
			Expect(f.store.Enqueue(f.ctx, adjTenant, cands, f.now)).To(Succeed())
			f.llm.reply = sayAll("NO")
		})

		It("sends at most MaxPairs pairs to the panel per run", func() {
			f.opts.MaxPairs = 3
			res, err := f.run()
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Leased).To(Equal(3))
			Expect(f.llm.callCount()).To(Equal(3 * len(f.cfg.Models)))
		})

		It("leases across several batches until the queue is empty", func() {
			f.opts.MaxPairs = 100
			res, err := f.run()
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Leased).To(Equal(25))
			Expect(res.Rejected).To(Equal(25))
		})
	})

	Describe("expired leases", func() {
		It("skips pairs whose lease expired while an earlier pair of the batch was on the panel", func() {
			var cands []artifacts.PairCandidate
			for i := range 3 {
				cands = append(cands, artifacts.PairCandidate{LowGroupID: fmt.Sprintf("e-%03d-a", i),
					HighGroupID: fmt.Sprintf("e-%03d-b", i), CandidateEdgeID: fmt.Sprintf("e-edge-%03d", i)})
			}
			Expect(f.store.Enqueue(f.ctx, adjTenant, cands, f.now)).To(Succeed())
			// MaxPairs 4 leases all four pairs in one batch and stops the run
			// from re-leasing the skipped ones.
			f.opts.MaxPairs = 4
			var advance sync.Once // the panel's calls run concurrently
			f.llm.reply = func(*llmclient.CompletionRequest) (string, error) {
				advance.Do(func() { f.now = f.now.Add(16 * time.Minute) }) // past the 15-minute lease
				return answer("NO"), nil
			}

			res, err := f.run()
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(Equal(artifacts.AdjudicateResult{Leased: 4, Rejected: 1, Expired: 3}))
			Expect(f.llm.callCount()).To(Equal(len(f.cfg.Models)), "only the first pair reaches the panel")
			c, err := f.store.Counts(f.ctx, adjTenant, f.now)
			Expect(err).NotTo(HaveOccurred())
			Expect(c.Pending).To(Equal(3))
			Expect(c.Due).To(Equal(3), "a later run can lease the skipped pairs")
			for _, k := range f.store.keys(adjTenant) {
				if r := f.store.row(adjTenant, k); r.status == artifacts.VerdictPending {
					Expect(r.attempts).To(BeZero())
				}
			}
		})
	})

	Describe("dry run", func() {
		It("reports the queue without leasing, calling the LLM or writing", func() {
			f.opts.DryRun = true
			f.llm.reply = noLLM
			res, err := f.run()
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(Equal(artifacts.AdjudicateResult{Pending: 1, Due: 1}))
			r := f.store.row(adjTenant, f.key)
			Expect(r.leasedUntil.IsZero()).To(BeTrue())
			Expect(f.candidateSuperseded()).To(BeFalse())
		})

		It("needs no LLM client", func() {
			f.opts.DryRun = true
			res, err := artifacts.NewAdjudicator(f.g, f.store, nil, f.prompts, f.cfg).Run(f.ctx, adjTenant, f.opts)
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(Equal(artifacts.AdjudicateResult{Pending: 1, Due: 1}))
		})

		It("counts decided verdicts not yet projected", func() {
			decide(artifacts.VerdictRejected)
			f.opts.DryRun = true
			res, err := f.run()
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(Equal(artifacts.AdjudicateResult{Unprojected: 1}))
			Expect(f.candidateSuperseded()).To(BeFalse())
		})
	})

	DescribeTable("stops on a store failure, naming the step, without completing that step",
		func(method, step string, setup func(), check func()) {
			setup()
			f.store.err[method] = errors.New("db down")
			_, err := f.run()
			Expect(err).To(MatchError(ContainSubstring(step)))
			Expect(err).To(MatchError(ContainSubstring("db down")))
			check()
		},
		Entry("Counts", "Counts", "count queue",
			func() { f.opts.DryRun = true },
			func() {
				r := f.store.row(adjTenant, f.key)
				Expect(r.status).To(Equal(artifacts.VerdictPending))
				Expect(r.leasedUntil).To(BeZero())
			}),
		Entry("Lease", "Lease", "lease pairs",
			func() { f.llm.reply = noLLM },
			func() {
				Expect(f.store.row(adjTenant, f.key).leasedUntil).To(BeZero())
				Expect(f.llm.callCount()).To(BeZero())
			}),
		Entry("Unprojected", "Unprojected", "load unprojected verdicts",
			func() { decide(artifacts.VerdictConfirmed); f.llm.reply = noLLM },
			func() {
				Expect(f.store.row(adjTenant, f.key).graphApplied).To(BeFalse())
				Expect(f.candidateSuperseded()).To(BeFalse())
				Expect(f.llm.callCount()).To(BeZero())
			}),
		Entry("RecordFailure", "RecordFailure", "record failure",
			func() {
				f.llm.reply = func(*llmclient.CompletionRequest) (string, error) { return "", errors.New("down") }
			},
			func() {
				r := f.store.row(adjTenant, f.key)
				Expect(r.status).To(Equal(artifacts.VerdictPending))
				Expect(r.attempts).To(BeZero())
				Expect(r.lastError).To(BeEmpty())
			}),
		Entry("RecordDissent", "RecordDissent", "record dissent",
			func() {
				f.llm.reply = func(*llmclient.CompletionRequest) (string, error) {
					f.store.setHuman(adjTenant, f.key, artifacts.VerdictRejected)
					return answer("YES"), nil
				}
			},
			func() {
				r := f.store.row(adjTenant, f.key)
				Expect(r.dissent).To(BeNil())
				Expect(r.status).To(Equal(artifacts.VerdictRejected))
				Expect(f.candidateSuperseded()).To(BeFalse())
			}),
		Entry("MarkProjected", "MarkProjected", "projected: db down",
			func() {},
			func() {
				Expect(f.store.row(adjTenant, f.key).graphApplied).To(BeFalse())
				// The graph write is idempotent and already done; recovery
				// re-projects it, so the edge stays live.
				Expect(f.liveSameAs()).To(HaveLen(1))
				Expect(f.liveSameAs()[0].Edge.DeterminationType).To(Equal("llm_panel"))
			}),
	)
})

// failingSupersede fails every SupersedeFact.
type failingSupersede struct{ graphdb.GraphDB }

func (failingSupersede) SupersedeFact(context.Context, string, graphdb.SupersedeRequest) (bool, error) {
	return false, errors.New("graph down")
}

// failingCreateEdge fails every CreateEdge.
type failingCreateEdge struct{ graphdb.GraphDB }

func (failingCreateEdge) CreateEdge(context.Context, string, string, string, graphdb.Edge) error {
	return errors.New("graph down")
}
