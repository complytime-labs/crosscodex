//go:build !integration

package feedback_test

import (
	"context"
	"encoding/json"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/feedback"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/cleaning"
	"github.com/complytime-labs/crosscodex/pkg/cleaning/cleaningtest"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// Rebuilt edge IDs: the graph-rebuild materializer writes a verdict's edges
// under another ID kind than the analysis did.
const (
	rbAnalyzer = "rb-an-j1-a1-b1"
	rbSynth    = "rb-s-j1-a1-b1"
)

var _ = Describe("Service.Reproject", func() {
	var (
		ctx    context.Context
		store  *fakeStore
		g      *failingSupersede
		signer attestation.Generator
	)

	serviceOver := func(gdb graphdb.GraphDB, jobs *cleaningtest.Fake, sig attestation.Generator) *feedback.Service {
		return feedback.NewService(store, svcCfg, feedback.WithClock(ticking()),
			feedback.WithDeterminations(gdb, cleaning.NewEngine(jobs, gdb, &config.CleaningConfig{}, nil, nil), sig))
	}
	determine := func(mappingID string, valid bool) {
		_, err := serviceOver(g, cleaningJobs(), signer).RecordDetermination(reviewerCtx(),
			connect.NewRequest(determineReq(svcTenant, mappingID, valid)))
		Expect(err).NotTo(HaveOccurred())
	}
	// rebuilt is a fresh graph holding the fixture controls and edges.
	rebuilt := func(edges ...graphdb.Edge) graphdb.GraphDB {
		gdb := svcNodes(ctx)
		for _, e := range edges {
			Expect(gdb.CreateEdge(ctx, svcTenant, "a1", "b1", e)).To(Succeed())
		}
		return gdb
	}
	rebuiltLive := func(from time.Time) graphdb.GraphDB {
		return rebuilt(matchEdge(rbAnalyzer, svcJob, "EQUIVALENT", false, from), matchEdge(rbSynth, svcJob, "EQUIVALENT", true, from))
	}
	review := func(mappingID string) feedback.Review {
		r, ok, err := store.Review(ctx, svcTenant, mappingID)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		return r
	}
	rebuiltState := func(gdb graphdb.GraphDB) svcState {
		st := snapshot(store, gdb)
		for _, id := range []string{rbAnalyzer, rbSynth} {
			if e, err := gdb.GetEdge(ctx, svcTenant, id); err == nil {
				st.Edges[id] = e.ValidTo
			}
		}
		return st
	}

	BeforeEach(func() {
		var err error
		ctx, err = tenant.WithTenant(context.Background(), svcTenant)
		Expect(err).NotTo(HaveOccurred())
		store = svcStore()
		g = &failingSupersede{GraphDB: svcGraph(ctx)}
		signer = newSigner()
	})

	It("retires an invalid review's rebuilt live edges, previewing first, and a second run changes nothing", func() {
		determine(mA1B1, false)
		decided := review(mA1B1).DecidedAt
		g2 := rebuiltLive(edgesFrom)
		svc := serviceOver(g2, cleaningJobs(), signer)

		before := rebuiltState(g2)
		preview, err := svc.Reproject(ctx, svcTenant, true)
		Expect(err).NotTo(HaveOccurred())
		want := feedback.ReprojectReport{Retired: 1, Items: []feedback.ReprojectItem{{
			MappingID: mA1B1, Status: feedback.ReviewInvalid, Action: cleaning.ReprojectRetire, EdgeIDs: []string{rbAnalyzer, rbSynth},
		}}}
		Expect(preview).To(Equal(want))
		Expect(rebuiltState(g2)).To(Equal(before))

		rep, err := svc.Reproject(ctx, svcTenant, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(rep).To(Equal(want))
		st := rebuiltState(g2)
		Expect(st.Live).To(BeEmpty())
		Expect(st.Edges[rbAnalyzer]).To(HaveValue(Equal(decided)))
		Expect(st.Edges[rbSynth]).To(HaveValue(Equal(decided)))
		r := review(mA1B1)
		Expect(r.EdgeIDs).To(Equal([]string{rbAnalyzer, rbSynth}))
		Expect(r.DecidedAt).To(Equal(decided))

		again, err := svc.Reproject(ctx, svcTenant, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(again).To(Equal(feedback.ReprojectReport{InSync: 1, Items: []feedback.ReprojectItem{{
			MappingID: mA1B1, Status: feedback.ReviewInvalid, Action: cleaning.ReprojectInSync, EdgeIDs: []string{rbAnalyzer, rbSynth},
		}}}))
		Expect(rebuiltState(g2)).To(Equal(st))
	})

	It("retires a rebuilt edge at its valid_from when that is after the determination", func() {
		determine(mA1B1, false)
		later := svcStart.Add(time.Hour)
		g2 := rebuiltLive(later)
		_, err := serviceOver(g2, cleaningJobs(), signer).Reproject(ctx, svcTenant, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(rebuiltState(g2).Edges[rbSynth]).To(HaveValue(Equal(later)))
	})

	It("relinks a restoring valid review to its verdict's rebuilt live edges", func() {
		determine(mA1B1, false)
		determine(mA1B1, true)
		g2 := rebuiltLive(edgesFrom)
		svc := serviceOver(g2, cleaningJobs(), signer)
		rep, err := svc.Reproject(ctx, svcTenant, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(rep).To(Equal(feedback.ReprojectReport{Relinked: 1, Items: []feedback.ReprojectItem{{
			MappingID: mA1B1, Status: feedback.ReviewValid, Action: cleaning.ReprojectRelink, EdgeIDs: []string{rbAnalyzer, rbSynth},
		}}}))
		Expect(review(mA1B1).EdgeIDs).To(Equal([]string{rbAnalyzer, rbSynth}))
		Expect(rebuiltState(g2).Live).To(Equal([]string{rbAnalyzer, rbSynth}))

		again, err := svc.Reproject(ctx, svcTenant, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(again.InSync).To(Equal(1))
	})

	It("keeps a restoring valid review whose verdict cleaning retired in sync while the original edge exists", func() {
		determine(mA1B1, false)
		determine(mA1B1, true)
		g2 := rebuilt(matchEdge(mA1B1, svcJob, "EQUIVALENT", true, edgesFrom))
		_, err := cleaning.RetireEdges(ctx, g2, svcTenant, []string{mA1B1}, svcStart.Add(time.Hour))
		Expect(err).NotTo(HaveOccurred())
		stored := review(mA1B1).EdgeIDs

		rep, err := serviceOver(g2, cleaningJobs(), signer).Reproject(ctx, svcTenant, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(rep).To(Equal(feedback.ReprojectReport{InSync: 1, Items: []feedback.ReprojectItem{{
			MappingID: mA1B1, Status: feedback.ReviewValid, Action: cleaning.ReprojectInSync, EdgeIDs: stored,
		}}}))
		Expect(review(mA1B1).Status).To(Equal(feedback.ReviewValid))
	})

	It("marks a review stale with a signed determination when the graph was rebuilt from new jobs, and a second run changes nothing", func() {
		determine(mA1B1, false)
		prev := review(mA1B1)
		g2 := rebuilt(matchEdge("e-j2-a1-b1", "j2", "EQUIVALENT", true, edgesFrom))
		svc := serviceOver(g2, cleaningJobs(), signer)

		rep, err := svc.Reproject(ctx, svcTenant, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(rep).To(Equal(feedback.ReprojectReport{Staled: 1, Items: []feedback.ReprojectItem{{
			MappingID: mA1B1, Status: feedback.ReviewInvalid, Action: cleaning.ReprojectStale, EdgeIDs: prev.EdgeIDs,
		}}}))
		r := review(mA1B1)
		Expect(r.Status).To(Equal(feedback.ReviewStale))
		Expect(r.GraphApplied).To(BeTrue())

		ds := store.Determinations(svcTenant)
		d := ds[len(ds)-1]
		Expect(d.ID).To(Equal(r.DeterminationID))
		Expect(d.Outcome).To(Equal(feedback.ReviewStale))
		Expect(d.DecidedBy).To(Equal(feedback.DecidedByReproject))
		Expect(d.Rationale).To(ContainSubstring("the graph no longer holds verdict j1:a1:b1"))
		var req map[string]any
		Expect(json.Unmarshal(d.Request, &req)).To(Succeed())
		Expect(req["previous_determination"]).To(Equal(prev.DeterminationID))
		link, err := signer.Verify(ctx, d.Attestation)
		Expect(err).NotTo(HaveOccurred())
		Expect(link.Step).To(Equal("feedback.reproject"))
		Expect(link.Products).To(Equal([]attestation.Artifact{{URI: "crosscodex:mapping/" + mA1B1, Digest: mA1B1}}))

		before := rebuiltState(g2)
		again, err := svc.Reproject(ctx, svcTenant, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(again).To(Equal(feedback.ReprojectReport{}))
		Expect(rebuiltState(g2)).To(Equal(before))
	})

	DescribeTable("judges a review whose job is no longer completed by the graph's edges of its verdict, live or not",
		func(rebuild func() graphdb.GraphDB, want cleaning.ReprojectAction) {
			determine(mA1B1, false)
			rep, err := serviceOver(rebuild(), &cleaningtest.Fake{}, signer).Reproject(ctx, svcTenant, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(rep.Items).To(HaveLen(1))
			Expect(rep.Items[0].Action).To(Equal(want))
		},
		Entry("in sync while the graph holds an edge of the verdict", func() graphdb.GraphDB { return rebuiltLive(edgesFrom) }, cleaning.ReprojectInSync),
		Entry("stale once the graph holds none", func() graphdb.GraphDB {
			return rebuilt(matchEdge("e-j2-a1-b1", "j2", "EQUIVALENT", true, edgesFrom))
		}, cleaning.ReprojectStale),
	)

	It("skips valid reviews that restored nothing", func() {
		determine(mA1B2, true)
		rep, err := serviceOver(rebuilt(), cleaningJobs(), signer).Reproject(ctx, svcTenant, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(rep).To(Equal(feedback.ReprojectReport{}))
	})

	It("previews without a signer", func() {
		determine(mA1B1, false)
		rep, err := serviceOver(rebuiltLive(edgesFrom), cleaningJobs(), nil).Reproject(ctx, svcTenant, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(rep.Retired).To(Equal(1))
	})

	Describe("refusals", func() {
		It("refuses while a determination is unprojected and changes nothing", func() {
			g.fail = true
			_, err := serviceOver(g, cleaningJobs(), signer).RecordDetermination(reviewerCtx(),
				connect.NewRequest(determineReq(svcTenant, mA1B1, false)))
			Expect(err).To(HaveOccurred())
			g.fail = false

			before := snapshot(store, g)
			_, err = serviceOver(g, cleaningJobs(), signer).Reproject(ctx, svcTenant, true)
			Expect(err).To(MatchError(feedback.ErrUnprojected))
			Expect(err.Error()).To(ContainSubstring("1 mapping determinations are recorded but not yet applied to the graph, " +
				"so re-projection was refused and nothing was changed"))
			Expect(err.Error()).To(ContainSubstring("then rerun --reproject"))
			Expect(snapshot(store, g)).To(Equal(before))
		})

		It("refuses without a signer unless dry-run, and changes nothing", func() {
			determine(mA1B1, false)
			g2 := rebuiltLive(edgesFrom)
			before := rebuiltState(g2)
			_, err := serviceOver(g2, cleaningJobs(), nil).Reproject(ctx, svcTenant, false)
			Expect(err).To(MatchError(feedback.ErrSigningDisabled))
			Expect(err.Error()).To(ContainSubstring("mapping determinations are signed; set attestation.private_key_path"))
			Expect(rebuiltState(g2)).To(Equal(before))
		})

		It("refuses without WithDeterminations and changes nothing", func() {
			determine(mA1B1, false)
			before := snapshot(store, g)
			_, err := feedback.NewService(store, svcCfg).Reproject(ctx, svcTenant, false)
			Expect(err).To(MatchError(feedback.ErrDeterminationsUnavailable))
			Expect(err.Error()).To(ContainSubstring("need the crosscodexd daemon (role all, or gateway with a graph role)"))
			Expect(snapshot(store, g)).To(Equal(before))
		})

		It("refuses another tenant's re-projection, naming neither tenant, and changes nothing", func() {
			determine(mA1B1, false)
			g2 := rebuiltLive(edgesFrom)
			before := rebuiltState(g2)
			_, err := serviceOver(g2, cleaningJobs(), signer).Reproject(ctx, svcOther, false)
			Expect(err).To(MatchError(cleaning.ErrTenantMismatch))
			Expect(err.Error()).NotTo(ContainSubstring(svcOther))
			Expect(err.Error()).NotTo(ContainSubstring(svcTenant))
			Expect(rebuiltState(g2)).To(Equal(before))
		})

		DescribeTable("refuses a review that changed during re-projection, naming it, and changes nothing",
			func(rebuild func() graphdb.GraphDB) {
				determine(mA1B1, false)
				g2 := rebuild()
				before := rebuiltState(g2)
				svc := feedback.NewService(changedOnReprojectStore{store}, svcCfg, feedback.WithClock(ticking()),
					feedback.WithDeterminations(g2, cleaning.NewEngine(cleaningJobs(), g2, &config.CleaningConfig{}, nil, nil), signer))
				_, err := svc.Reproject(ctx, svcTenant, false)
				Expect(err).To(MatchError(feedback.ErrReviewChanged))
				Expect(err).To(MatchError(ContainSubstring("mapping " + mA1B1 + " changed during re-projection")))
				Expect(err).To(MatchError(ContainSubstring("rerun --reproject")))
				Expect(rebuiltState(g2)).To(Equal(before), "the review and its verdict's edges are unchanged")
			},
			Entry("a retire", func() graphdb.GraphDB { return rebuiltLive(edgesFrom) }),
			Entry("a stale marking", func() graphdb.GraphDB {
				return rebuilt(matchEdge("e-j2-a1-b1", "j2", "EQUIVALENT", true, edgesFrom))
			}),
		)
	})

	Describe("across several reviews", func() {
		const rbSynthB2 = "rb-s-j1-a1-b2"
		// twoInvalid records invalid determinations for a1->b1 and a1->b2 and
		// returns a rebuilt graph holding live edges for both verdicts.
		twoInvalid := func() graphdb.GraphDB {
			determine(mA1B1, false)
			determine(mA1B2, false)
			g2 := rebuiltLive(edgesFrom)
			Expect(g2.CreateEdge(ctx, svcTenant, "a1", "b2", matchEdge(rbSynthB2, svcJob, "EQUIVALENT", true, edgesFrom))).To(Succeed())
			return g2
		}
		over := func(st feedback.Store, gdb graphdb.GraphDB, jobs cleaning.FindingStore) *feedback.Service {
			return feedback.NewService(st, svcCfg, feedback.WithClock(ticking()),
				feedback.WithDeterminations(gdb, cleaning.NewEngine(jobs, gdb, &config.CleaningConfig{}, nil, nil), signer))
		}

		It("names how many reviews stand when a write fails part-way, leaves the rest, and a rerun finishes them", func() {
			g2 := twoInvalid()
			_, err := over(&failSecondMark{fakeStore: store}, g2, cleaningJobs()).Reproject(ctx, svcTenant, false)
			Expect(err).To(MatchError(errDriver))
			Expect(err).To(MatchError(ContainSubstring("1 of 2 reviews were re-projected before it and stand")))
			Expect(err).To(MatchError(ContainSubstring("run the re-projection again to finish")))
			Expect(rebuiltState(g2).Live).To(HaveLen(1), "the first review's edges are retired, the second's are untouched")

			rep, err := over(store, g2, cleaningJobs()).Reproject(ctx, svcTenant, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(rep.Retired).To(Equal(1))
			Expect(rep.InSync).To(Equal(1))
			Expect(rebuiltState(g2).Live).To(BeEmpty())
		})

		It("returns a verdict-read failure that is not a missing job, wrapped, and changes nothing", func() {
			g2 := twoInvalid()
			before := rebuiltState(g2)
			_, err := over(store, g2, failingCompletedJobs{FindingStore: cleaningJobs()}).Reproject(ctx, svcTenant, false)
			Expect(err).To(MatchError(errDriver))
			Expect(err).To(MatchError(ContainSubstring("read job " + svcJob + "'s live edges")))
			Expect(rebuiltState(g2)).To(Equal(before))
		})
	})
})

// failSecondMark fails the second re-projection write with a driver error.
type failSecondMark struct {
	*fakeStore
	calls int
}

func (f *failSecondMark) MarkReprojected(ctx context.Context, tenantID, mappingID, expectDetermination string, edgeIDs []string) error {
	f.calls++
	if f.calls == 2 {
		return errDriver
	}
	return f.fakeStore.MarkReprojected(ctx, tenantID, mappingID, expectDetermination, edgeIDs)
}

// failingCompletedJobs fails the verdict read's job lookup with a driver
// error, which is not cleaning.ErrVerdictJobNotFound.
type failingCompletedJobs struct{ cleaning.FindingStore }

func (failingCompletedJobs) CompletedJobs(context.Context, string) ([]cleaning.Job, error) {
	return nil, errDriver
}

// changedOnReprojectStore refuses every re-projection write as if a newer
// determination had landed meanwhile.
type changedOnReprojectStore struct{ *fakeStore }

func (changedOnReprojectStore) MarkReprojected(_ context.Context, _, mappingID, _ string, _ []string) error {
	return feedback.ReviewChanged(mappingID, "a-newer-determination")
}

func (changedOnReprojectStore) MarkStale(_ context.Context, _ string, d feedback.Determination, _ string) error {
	return feedback.ReviewChanged(d.MappingID, "a-newer-determination")
}

// Cleaning findings and mapping reviews are re-projected by two engines, each
// replaying its own latest decision; the CLI runs cleaning's first. Whichever
// runs second finds the verdict's rebuilt edges already retired by the other,
// under edge IDs it never stored, and must not take the verdict for gone.
var _ = Describe("Cleaning and feedback re-projection together", func() {
	var (
		ctx      context.Context
		fstore   *fakeStore
		cstore   *cleaningtest.Fake
		signer   attestation.Generator
		fbClock  func() time.Time
		cleanNow time.Time
	)
	conflict := cleaning.FindingID(cleaning.Finding{Signal: cleaning.SignalConflictsWith,
		Concerns: []cleaning.VerdictKey{{JobID: svcJob, SourceID: "a1", TargetID: "b1"}}})

	// conflictGraph holds a1->b1 of job j1 in both stages as CONFLICTS_WITH,
	// so the cleaning policy below retires it.
	conflictGraph := func(analyzerID, synthID string) graphdb.GraphDB {
		gdb := svcNodes(ctx)
		Expect(gdb.CreateEdge(ctx, svcTenant, "a1", "b1", matchEdge(analyzerID, svcJob, "CONFLICTS_WITH", false, edgesFrom))).To(Succeed())
		Expect(gdb.CreateEdge(ctx, svcTenant, "a1", "b1", matchEdge(synthID, svcJob, "CONFLICTS_WITH", true, edgesFrom))).To(Succeed())
		return gdb
	}
	cleaningOver := func(gdb graphdb.GraphDB) *cleaning.Engine {
		report := config.SignalPolicy{Action: config.CleaningActionReport}
		cfg := &config.CleaningConfig{MaxRetirePerRun: 1000, Signals: config.CleaningSignals{
			EquivalenceFanout: report, CrossJobContradiction: report,
			LowAgreement:  config.SignalPolicy{Action: config.CleaningActionReport, MinAgreementFraction: 0.5},
			ConflictsWith: config.SignalPolicy{Action: config.CleaningActionRetire},
			StalePrompt:   report, StaleModel: report,
		}}
		return cleaning.NewEngine(cstore, gdb, cfg, nil, signer, cleaning.WithClock(func() time.Time { return cleanNow }))
	}
	feedbackOver := func(gdb graphdb.GraphDB) *feedback.Service {
		return feedback.NewService(fstore, svcCfg, feedback.WithClock(fbClock), feedback.WithDeterminations(gdb, cleaningOver(gdb), signer))
	}
	determine := func(gdb graphdb.GraphDB, valid bool) {
		_, err := feedbackOver(gdb).RecordDetermination(reviewerCtx(), connect.NewRequest(determineReq(svcTenant, mA1B1, valid)))
		Expect(err).NotTo(HaveOccurred())
	}
	cleanRetire := func(gdb graphdb.GraphDB, at time.Time) {
		cleanNow = at
		eng := cleaningOver(gdb)
		plan, err := eng.Preview(ctx, svcTenant, cleaning.Scope{})
		Expect(err).NotTo(HaveOccurred())
		_, err = eng.Apply(ctx, svcTenant, cleaning.Scope{}, plan.Token)
		Expect(err).NotTo(HaveOccurred())
		f, _, err := cstore.GetFinding(ctx, svcTenant, conflict)
		Expect(err).NotTo(HaveOccurred())
		Expect(f.Status).To(Equal(cleaning.StatusRetired))
	}
	cleanRestore := func(gdb graphdb.GraphDB, at time.Time) {
		cleanNow = at
		eng := cleaningOver(gdb)
		_, _, token, err := eng.GetFinding(ctx, svcTenant, conflict)
		Expect(err).NotTo(HaveOccurred())
		_, _, err = eng.Decide(ctx, svcTenant, cleaning.DecideRequest{
			FindingID: conflict, Verdict: cleaning.VerdictRestore, Rationale: "reviewed", ExpectState: token, Reviewer: svcReviewer,
		})
		Expect(err).NotTo(HaveOccurred())
	}
	// reprojectBoth runs both engines for real in the given order and
	// returns their reports.
	reprojectBoth := func(gdb graphdb.GraphDB, cleaningFirst bool) (cleaning.ReprojectReport, feedback.ReprojectReport) {
		var cRep cleaning.ReprojectReport
		var fRep feedback.ReprojectReport
		runCleaning := func() {
			var err error
			cRep, err = cleaningOver(gdb).Reproject(ctx, svcTenant, false)
			Expect(err).NotTo(HaveOccurred())
		}
		runFeedback := func() {
			var err error
			fRep, err = feedbackOver(gdb).Reproject(ctx, svcTenant, false)
			Expect(err).NotTo(HaveOccurred())
		}
		if cleaningFirst {
			runCleaning()
			runFeedback()
		} else {
			runFeedback()
			runCleaning()
		}
		return cRep, fRep
	}
	type bothState struct {
		Feedback svcState
		Finding  cleaning.StoredFinding
		Decided  []cleaning.Decision
	}
	state := func(gdb graphdb.GraphDB) bothState {
		f, ds, err := cstore.GetFinding(ctx, svcTenant, conflict)
		Expect(err).NotTo(HaveOccurred())
		return bothState{Feedback: snapshot(fstore, gdb), Finding: f, Decided: ds}
	}

	BeforeEach(func() {
		var err error
		ctx, err = tenant.WithTenant(context.Background(), svcTenant)
		Expect(err).NotTo(HaveOccurred())
		fstore = svcStore()
		cstore = cleaningJobs()
		cstore.Reviewers = map[string]map[string]string{svcTenant: {svcReviewer: cleaningtest.ReviewerActive}}
		cstore.Now = func() time.Time { return cleanNow }
		signer = newSigner()
		fbClock = ticking()
	})

	DescribeTable("marks nothing stale and leaves the verdict retired, whichever engine runs first",
		func(history func(gdb graphdb.GraphDB), cleaningFirst bool, findingStatus cleaning.Status, reviewStatus feedback.ReviewStatus) {
			history(conflictGraph("an-j1-a1-b1", mA1B1))
			rb := conflictGraph(rbAnalyzer, rbSynth)

			cRep, fRep := reprojectBoth(rb, cleaningFirst)
			Expect(cRep.Staled).To(BeZero(), "cleaning report %+v", cRep)
			Expect(fRep.Staled).To(BeZero(), "feedback report %+v", fRep)
			st := state(rb)
			Expect(st.Finding.Status).To(Equal(findingStatus))
			Expect(st.Decided).NotTo(ContainElement(HaveField("Verdict", cleaning.VerdictStale)))
			Expect(st.Feedback.Reviews).To(ConsistOf(HaveField("Status", reviewStatus)))
			Expect(st.Feedback.Determinations).NotTo(ContainElement(HaveField("Outcome", feedback.ReviewStale)))
			Expect(st.Feedback.Live).To(BeEmpty(), "the verdict's rebuilt edges are retired")
			for _, id := range []string{rbAnalyzer, rbSynth} {
				e, err := rb.GetEdge(ctx, svcTenant, id)
				Expect(err).NotTo(HaveOccurred())
				Expect(e.ValidTo).NotTo(BeNil(), "edge %s", id)
			}

			cRep, fRep = reprojectBoth(rb, cleaningFirst)
			Expect(cRep.InSync).To(Equal(len(cRep.Items)), "cleaning report %+v", cRep)
			Expect(fRep.InSync).To(Equal(len(fRep.Items)), "feedback report %+v", fRep)
			Expect(state(rb)).To(Equal(st))
		},
		Entry("a reviewer restored, then cleaning retired; cleaning first, as the CLI runs them",
			func(gdb graphdb.GraphDB) {
				determine(gdb, false)
				determine(gdb, true)
				cleanRetire(gdb, svcStart.Add(time.Hour))
			}, true, cleaning.StatusRetired, feedback.ReviewValid),
		Entry("a reviewer restored, then cleaning retired; feedback first",
			func(gdb graphdb.GraphDB) {
				determine(gdb, false)
				determine(gdb, true)
				cleanRetire(gdb, svcStart.Add(time.Hour))
			}, false, cleaning.StatusRetired, feedback.ReviewValid),
		Entry("cleaning retired and restored, then a reviewer retired; feedback first (the mirror)",
			func(gdb graphdb.GraphDB) {
				cleanRetire(gdb, svcStart.Add(-time.Hour))
				cleanRestore(gdb, svcStart.Add(-30*time.Minute))
				determine(gdb, false)
			}, false, cleaning.StatusRestored, feedback.ReviewInvalid),
		Entry("cleaning retired and restored, then a reviewer retired; cleaning first",
			func(gdb graphdb.GraphDB) {
				cleanRetire(gdb, svcStart.Add(-time.Hour))
				cleanRestore(gdb, svcStart.Add(-30*time.Minute))
				determine(gdb, false)
			}, true, cleaning.StatusRestored, feedback.ReviewInvalid),
	)
})

// The vote and review queue paths never need a graph: a service without
// WithDeterminations serves them, as the embedded daemon does.
var _ = Describe("Service without determinations", func() {
	It("serves votes", func() {
		svc := feedback.NewService(svcStore(), svcCfg)
		_, err := svc.SubmitVote(reviewerCtx(), connect.NewRequest(&pb.SubmitVoteRequest{
			TenantContext: tc(svcTenant), JobId: svcJob, MappingId: mA1B1, VoteType: pb.VoteType_VOTE_TYPE_AGREE,
		}))
		Expect(err).NotTo(HaveOccurred())
	})

	It("serves the review queue", func() {
		svc := feedback.NewService(svcStore(), svcCfg)
		resp, err := svc.GetReviewQueue(reviewerCtx(), connect.NewRequest(&pb.GetReviewQueueRequest{TenantContext: tc(svcTenant), JobId: svcJob}))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.GetItems()).NotTo(BeEmpty())
	})
})
