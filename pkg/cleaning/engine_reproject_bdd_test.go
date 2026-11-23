//go:build !integration

package cleaning_test

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/types"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/cleaning"
	"github.com/complytime-labs/crosscodex/pkg/cleaning/cleaningtest"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/graphdb/memdriver"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// rebuiltAt is when a rebuilt graph's edges start: after every decision.
var rebuiltAt = engNow.Add(time.Hour)

// rebuiltGraph is the engine fixture's graph after a rebuild: the same
// controls, and SEMANTIC_MATCH edges of job on the given pairs under new
// IDs ("r-<job>-<src>-<tgt>"), starting at validFrom.
func rebuiltGraph(ctx context.Context, job string, validFrom time.Time, pairs ...[2]graphdb.Node) graphdb.GraphDB {
	g := memdriver.New()
	Expect(g.CreateGraph(ctx, engTenant)).To(Succeed())
	for _, n := range []graphdb.Node{a1, a2, b1, b2} {
		n.ValidFrom = t1
		Expect(g.CreateNode(ctx, engTenant, n)).To(Succeed())
	}
	for _, p := range pairs {
		Expect(g.CreateEdge(ctx, engTenant, p[0].ID, p[1].ID, graphdb.Edge{
			ID: "r-" + job + "-" + p[0].ID + "-" + p[1].ID, Label: results.SemanticMatchEdgeLabel, ValidFrom: validFrom,
			Properties: map[string]any{"job_id": job, "relationship_type": "CONFLICTS_WITH"},
		})).To(Succeed())
	}
	return g
}

// byID returns items in finding ID order, as a report holds them.
func byID(items ...cleaning.ReprojectItem) []cleaning.ReprojectItem {
	slices.SortFunc(items, func(x, y cleaning.ReprojectItem) int { return cmp.Compare(x.FindingID, y.FindingID) })
	return items
}

// racingStore changes one finding's stored edge IDs just before re-projection
// writes it, as a reviewer's decision recorded meanwhile would.
type racingStore struct {
	*cleaningtest.Fake
	findingID string
}

func (r racingStore) race(ctx context.Context, tenantID, findingID string) {
	if findingID == r.findingID {
		Expect(r.Fake.MarkProjected(ctx, tenantID, findingID, []string{"edge-concurrent"})).To(Succeed())
	}
}

func (r racingStore) MarkStale(ctx context.Context, tenantID string, d cleaning.Decision, expectState string) error {
	r.race(ctx, tenantID, d.FindingID)
	return r.Fake.MarkStale(ctx, tenantID, d, expectState)
}

func (r racingStore) MarkReprojected(ctx context.Context, tenantID, findingID, expectState string, edgeIDs []string) error {
	r.race(ctx, tenantID, findingID)
	return r.Fake.MarkReprojected(ctx, tenantID, findingID, expectState, edgeIDs)
}

// allPairs are the four control pairs job j1 maps.
var allPairs = [][2]graphdb.Node{{a1, b1}, {a1, b2}, {a2, b1}, {a2, b2}}

var _ = Describe("Engine.Reproject", func() {
	var (
		ctx     context.Context
		g       graphdb.GraphDB
		store   *cleaningtest.Fake
		signer  attestation.Generator
		cfg     *config.CleaningConfig
		current cleaning.CurrentFunc
		now     time.Time
	)

	j2 := cleaning.Job{ID: "j2", CompletedAt: t2, Catalogs: engJob.Catalogs}
	// jobFindingID is the stale_prompt finding about job j1.
	jobFindingID := cleaning.FindingID(cleaning.Finding{Signal: cleaning.SignalStalePrompt, Evidence: cleaning.Evidence{Job: &cleaning.JobEvidence{ID: "j1"}}})

	engineOver := func(st cleaning.FindingStore, gdb graphdb.GraphDB, sig attestation.Generator) *cleaning.Engine {
		return cleaning.NewEngine(st, gdb, cfg, current, sig, cleaning.WithClock(func() time.Time { return now }))
	}
	engine := func(gdb graphdb.GraphDB, sig attestation.Generator) *cleaning.Engine {
		return engineOver(store, gdb, sig)
	}
	reproject := func(gdb graphdb.GraphDB, dryRun bool) cleaning.ReprojectReport {
		rep, err := engine(gdb, signer).Reproject(ctx, engTenant, dryRun)
		Expect(err).NotTo(HaveOccurred())
		return rep
	}
	finding := func(id string) (cleaning.StoredFinding, []cleaning.Decision) {
		f, ds, err := store.GetFinding(ctx, engTenant, id)
		Expect(err).NotTo(HaveOccurred())
		return f, ds
	}
	// decide records a reviewer's decision on the fixture graph a minute after
	// the previous one, and returns the finding afterwards.
	decide := func(id, verdict string) cleaning.StoredFinding {
		now = now.Add(time.Minute)
		_, _, token, err := engine(g, signer).GetFinding(ctx, engTenant, id)
		Expect(err).NotTo(HaveOccurred())
		f, _, err := engine(g, signer).Decide(ctx, engTenant, cleaning.DecideRequest{
			FindingID: id, Verdict: verdict, Rationale: "reviewed", ExpectState: token, Reviewer: reviewer,
		})
		Expect(err).NotTo(HaveOccurred())
		return f
	}
	// restoreB1 restores conflictB1 in the fixture graph and returns its live
	// successor's ID.
	restoreB1 := func() string {
		restored := decide(conflictB1, cleaning.VerdictRestore)
		Expect(restored.EdgeIDs).To(HaveLen(1))
		return restored.EdgeIDs[0]
	}
	actions := func(rep cleaning.ReprojectReport) map[string]cleaning.ReprojectAction {
		out := map[string]cleaning.ReprojectAction{}
		for _, it := range rep.Items {
			out[it.FindingID] = it.Action
		}
		return out
	}

	BeforeEach(func() {
		var err error
		ctx, err = tenant.WithTenant(context.Background(), engTenant)
		Expect(err).NotTo(HaveOccurred())
		g = engineGraph(ctx)
		store = engineStore()
		kp, err := attestation.NewEphemeralKeyProvider()
		Expect(err).NotTo(HaveOccurred())
		signer, err = attestation.NewGenerator(kp)
		Expect(err).NotTo(HaveOccurred())
		cfg, current, now = engineConfig(), nil, engNow
	})

	JustBeforeEach(func() {
		// The fixture policy retires both conflicts_with findings and queues
		// the equivalence fanout, at engNow.
		eng := engine(g, signer)
		plan, err := eng.Preview(ctx, engTenant, cleaning.Scope{})
		Expect(err).NotTo(HaveOccurred())
		_, err = eng.Apply(ctx, engTenant, cleaning.Scope{}, plan.Token)
		Expect(err).NotTo(HaveOccurred())
	})

	It("finds every retired and restored finding in sync with the graph it was projected into, and changes nothing", func() {
		successor := restoreB1()
		before := snapshot(ctx, store, g)
		rep := reproject(g, false)
		Expect(rep).To(Equal(cleaning.ReprojectReport{InSync: 2, Items: byID(
			cleaning.ReprojectItem{FindingID: conflictB1, Status: cleaning.StatusRestored, Action: cleaning.ReprojectInSync, EdgeIDs: []string{successor}},
			cleaning.ReprojectItem{FindingID: conflictB2, Status: cleaning.StatusRetired, Action: cleaning.ReprojectInSync, EdgeIDs: []string{"e-j1-a2-b2"}},
		)}))
		Expect(snapshot(ctx, store, g)).To(Equal(before))
	})

	DescribeTable("retires a retired finding's rebuilt live edges and stores their IDs",
		func(validFrom, wantValidTo time.Time) {
			rebuilt := rebuiltGraph(ctx, "j1", validFrom, [2]graphdb.Node{a2, b1}, [2]graphdb.Node{a2, b2}, [2]graphdb.Node{a1, b1})
			rep := reproject(rebuilt, false)
			Expect(rep).To(Equal(cleaning.ReprojectReport{Retired: 2, Items: byID(
				cleaning.ReprojectItem{FindingID: conflictB1, Status: cleaning.StatusRetired, Action: cleaning.ReprojectRetire, EdgeIDs: []string{"r-j1-a2-b1"}},
				cleaning.ReprojectItem{FindingID: conflictB2, Status: cleaning.StatusRetired, Action: cleaning.ReprojectRetire, EdgeIDs: []string{"r-j1-a2-b2"}},
			)}))
			Expect(liveEdgeIDs(ctx, rebuilt)).To(Equal([]string{"r-j1-a1-b1"}))
			for _, id := range []string{"r-j1-a2-b1", "r-j1-a2-b2"} {
				e, err := rebuilt.GetEdge(ctx, engTenant, id)
				Expect(err).NotTo(HaveOccurred())
				Expect(e.ValidTo).To(HaveValue(BeTemporally("==", wantValidTo)))
			}
			f, ds := finding(conflictB1)
			Expect(f.Status).To(Equal(cleaning.StatusRetired))
			Expect(f.GraphApplied).To(BeTrue())
			Expect(f.EdgeIDs).To(Equal([]string{"r-j1-a2-b1"}))
			Expect(ds).To(HaveLen(1), "re-projection records no decision for a retire")
		},
		Entry("at an edge's valid_from when it starts after the retire decision", rebuiltAt, rebuiltAt),
		Entry("at the retire decision's time when the edge started before it", t1, engNow),
	)

	It("relinks a restored finding to its verdict's rebuilt live edges", func() {
		restoreB1()
		rebuilt := rebuiltGraph(ctx, "j1", rebuiltAt, [2]graphdb.Node{a2, b1})
		rep := reproject(rebuilt, false)
		Expect(rep.Items).To(ConsistOf(
			cleaning.ReprojectItem{FindingID: conflictB1, Status: cleaning.StatusRestored, Action: cleaning.ReprojectRelink, EdgeIDs: []string{"r-j1-a2-b1"}},
			And(HaveField("FindingID", conflictB2), HaveField("Action", cleaning.ReprojectStale)),
		))
		Expect(rep.Relinked).To(Equal(1))
		Expect(liveEdgeIDs(ctx, rebuilt)).To(Equal([]string{"r-j1-a2-b1"}))
		f, _ := finding(conflictB1)
		Expect(f.Status).To(Equal(cleaning.StatusRestored))
		Expect(f.EdgeIDs).To(Equal([]string{"r-j1-a2-b1"}))
	})

	DescribeTable("marks a finding whose verdicts are gone stale with a signed decision naming them",
		func(jobs []cleaning.Job) {
			store.Jobs[engTenant] = jobs
			rebuilt := rebuiltGraph(ctx, "j2", rebuiltAt, [2]graphdb.Node{a2, b1})
			before, _ := finding(conflictB1)

			rep := reproject(rebuilt, false)
			missing := []cleaning.VerdictKey{key("j1", a2, b1)}
			Expect(rep.Staled).To(Equal(2))
			Expect(rep.Items).To(ContainElement(cleaning.ReprojectItem{
				FindingID: conflictB1, Status: cleaning.StatusRetired, Action: cleaning.ReprojectStale,
				EdgeIDs: []string{"e-j1-a2-b1"}, Missing: missing,
			}))
			Expect(liveEdgeIDs(ctx, rebuilt)).To(Equal([]string{"r-j2-a2-b1"}), "a stale marking changes no edge")

			f, ds := finding(conflictB1)
			want := before
			want.Status, want.UpdatedAt = cleaning.StatusStale, engNow
			Expect(f).To(Equal(want), "status stale, targets and edge IDs kept, still projected")
			// Both decisions are stamped engNow, so the store orders them by ID.
			Expect(ds).To(HaveLen(2))
			i := slices.IndexFunc(ds, func(d cleaning.Decision) bool { return d.Verdict == cleaning.VerdictStale })
			Expect(i).NotTo(Equal(-1))
			d := ds[i]
			reason := "the graph no longer holds verdicts j1:a2:b1: it was rebuilt from new jobs. " +
				"Decide again on the findings a new cleaning pass raises"
			Expect(d).To(And(
				HaveField("FindingID", conflictB1), HaveField("Verdict", cleaning.VerdictStale),
				HaveField("DecidedBy", cleaning.DecidedByReproject), HaveField("RunID", ""),
				HaveField("Rationale", reason), HaveField("DecidedAt", engNow)))
			sum := sha256.Sum256(d.Request)
			Expect(d.ID).To(Equal(hex.EncodeToString(sum[:])))
			var req map[string]any
			Expect(json.Unmarshal(d.Request, &req)).To(Succeed())
			Expect(req).To(Equal(map[string]any{
				"tenant": engTenant, "finding_id": conflictB1, "verdict": "stale", "reason": reason,
				"missing": []any{"j1:a2:b1"}, "evidence_hash": before.EvidenceHash, "decided_at": engNow.Format(time.RFC3339Nano),
			}))
			link, err := signer.Verify(ctx, d.Attestation)
			Expect(err).NotTo(HaveOccurred())
			Expect(link.Step).To(Equal("cleaning.reproject"))
			Expect(link.Materials).To(ConsistOf(attestation.Artifact{URI: "crosscodex:cleaning-decision/" + d.ID, Digest: d.ID}))
			Expect(link.Products).To(ConsistOf(attestation.Artifact{URI: "crosscodex:finding/" + conflictB1, Digest: before.EvidenceHash}))
		},
		Entry("when retention purged the old job", []cleaning.Job{j2}),
		Entry("when the old job is still completed but the graph was rebuilt from new jobs", []cleaning.Job{engJob, j2}),
	)

	It("computes the same report on a dry run, without a signer, and writes nothing", func() {
		restoreB1()
		rebuilt := rebuiltGraph(ctx, "j1", rebuiltAt, [2]graphdb.Node{a2, b1})
		before := snapshot(ctx, store, rebuilt)
		dry, err := engine(rebuilt, nil).Reproject(ctx, engTenant, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(snapshot(ctx, store, rebuilt)).To(Equal(before))
		Expect(dry.Relinked + dry.Staled).To(Equal(2))
		Expect(reproject(rebuilt, false)).To(Equal(dry))
	})

	DescribeTable("finds every remaining finding in sync on a second run and changes nothing",
		func(rebuild func() graphdb.GraphDB) {
			restoreB1()
			rebuilt := rebuild()
			first := reproject(rebuilt, false)
			Expect(first.InSync).To(BeZero())
			before := snapshot(ctx, store, rebuilt)
			second := reproject(rebuilt, false)
			for _, it := range second.Items {
				Expect(it.Action).To(Equal(cleaning.ReprojectInSync), "finding %s", it.FindingID)
			}
			Expect(second.InSync).To(Equal(first.Retired + first.Relinked))
			Expect(snapshot(ctx, store, rebuilt)).To(Equal(before))
		},
		Entry("after a relink and a retire", func() graphdb.GraphDB {
			return rebuiltGraph(ctx, "j1", rebuiltAt, [2]graphdb.Node{a2, b1}, [2]graphdb.Node{a2, b2})
		}),
		Entry("after stale markings", func() graphdb.GraphDB {
			store.Jobs[engTenant] = []cleaning.Job{j2}
			return rebuiltGraph(ctx, "j2", rebuiltAt, [2]graphdb.Node{a2, b1})
		}),
	)

	DescribeTable("refuses while a finding is unprojected, saying to resume, and changes nothing",
		func(dryRun bool) {
			row := storedOf(planFinding(cleaning.SignalConflictsWith, keys(key("j9", a1, b1)), keys(key("j9", a1, b1))), cleaning.StatusRetired)
			store = engineStore(row)
			before := snapshot(ctx, store, g)
			_, err := engine(g, signer).Reproject(ctx, engTenant, dryRun)
			Expect(err).To(MatchError(cleaning.ErrUnprojected))
			Expect(err).To(MatchError(ContainSubstring("Run 'crosscodex admin cleaning run --resume' first")))
			Expect(snapshot(ctx, store, g)).To(Equal(before))
		},
		Entry("a dry run", true),
		Entry("a run", false),
	)

	It("refuses a run without a signer before writing anything", func() {
		rebuilt := rebuiltGraph(ctx, "j1", rebuiltAt, [2]graphdb.Node{a2, b1})
		before := snapshot(ctx, store, rebuilt)
		_, err := engine(rebuilt, nil).Reproject(ctx, engTenant, false)
		Expect(err).To(MatchError(cleaning.ErrSigningDisabled))
		Expect(err).To(MatchError(ContainSubstring("set attestation.private_key_path and attestation.public_key_path")))
		Expect(snapshot(ctx, store, rebuilt)).To(Equal(before))
	})

	It("returns a graph outage naming the finding and how to finish, and writes nothing", func() {
		before := snapshot(ctx, store, g)
		_, err := engine(failingGetEdge{GraphDB: g}, signer).Reproject(ctx, engTenant, false)
		Expect(err).To(MatchError(errInjected))
		Expect(err).To(MatchError(ContainSubstring("0 of 2 findings were re-projected before it")))
		Expect(err).To(MatchError(ContainSubstring("run the re-projection again to finish")))
		Expect(snapshot(ctx, store, g)).To(Equal(before))
	})

	DescribeTable("refuses a decision on a stale finding and changes nothing",
		func(verdict string) {
			store.Jobs[engTenant] = []cleaning.Job{j2}
			rebuilt := rebuiltGraph(ctx, "j2", rebuiltAt, [2]graphdb.Node{a2, b1})
			reproject(rebuilt, false)
			eng := engine(rebuilt, signer)
			_, _, token, err := eng.GetFinding(ctx, engTenant, conflictB1)
			Expect(err).NotTo(HaveOccurred())
			before := snapshot(ctx, store, rebuilt)
			_, _, err = eng.Decide(ctx, engTenant, cleaning.DecideRequest{
				FindingID: conflictB1, Verdict: verdict, Rationale: "try anyway", ExpectState: token, Reviewer: reviewer,
			})
			Expect(err).To(MatchError(cleaning.ErrInvalidTransition))
			Expect(err).To(MatchError(ContainSubstring("finding " + conflictB1 + " is stale: its verdicts are no longer in the graph " +
				"(see its stale decision). Decide on the findings a new cleaning pass raises")))
			Expect(snapshot(ctx, store, rebuilt)).To(Equal(before))
		},
		Entry("retire", cleaning.VerdictRetire),
		Entry("restore", cleaning.VerdictRestore),
		Entry("dismiss", cleaning.VerdictDismiss),
	)

	It("refuses a reviewer's stale verdict as not a decision a reviewer makes", func() {
		eng := engine(g, signer)
		_, _, token, err := eng.GetFinding(ctx, engTenant, fanoutID)
		Expect(err).NotTo(HaveOccurred())
		before := snapshot(ctx, store, g)
		_, _, err = eng.Decide(ctx, engTenant, cleaning.DecideRequest{
			FindingID: fanoutID, Verdict: cleaning.VerdictStale, Rationale: "gone", ExpectState: token, Reviewer: reviewer,
		})
		Expect(err).To(MatchError(cleaning.ErrInvalidDecision))
		Expect(err).To(MatchError(ContainSubstring("retire, dismiss or restore")))
		Expect(snapshot(ctx, store, g)).To(Equal(before))
	})

	It("retires a retired finding's stored edges that are live again, though their job is no longer completed", func() {
		store.Jobs[engTenant] = nil
		restoredBackup := engineGraph(ctx)
		rep := reproject(restoredBackup, false)
		Expect(actions(rep)).To(Equal(map[string]cleaning.ReprojectAction{
			conflictB1: cleaning.ReprojectRetire, conflictB2: cleaning.ReprojectRetire,
		}))
		Expect(liveEdgeIDs(ctx, restoredBackup)).To(Equal([]string{"e-j1-a1-b1", "e-j1-a1-b2", "s-j1-a1-b2"}))
	})

	It("finishes a retire whose supersede failed after its edge IDs were recorded", func() {
		rebuilt := rebuiltGraph(ctx, "j1", rebuiltAt, [2]graphdb.Node{a2, b1}, [2]graphdb.Node{a2, b2})
		_, err := engine(&failNthSupersede{GraphDB: rebuilt, n: 1}, signer).Reproject(ctx, engTenant, false)
		Expect(err).To(MatchError(errInjected))
		Expect(err).To(MatchError(ContainSubstring("run the re-projection again to finish")))
		first := min(conflictB1, conflictB2)
		f, _ := finding(first)
		Expect(f.EdgeIDs).To(HaveLen(1), "the rebuilt edge ID is recorded before the supersede")
		Expect(liveEdgeIDs(ctx, rebuilt)).To(ContainElement(f.EdgeIDs[0]))

		rep := reproject(rebuilt, false)
		Expect(rep.Retired).To(Equal(2))
		Expect(liveEdgeIDs(ctx, rebuilt)).To(BeEmpty())
	})

	It("marks a restored finding stale when its verdicts are gone", func() {
		restoreB1()
		store.Jobs[engTenant] = []cleaning.Job{j2}
		rep := reproject(rebuiltGraph(ctx, "j2", rebuiltAt, [2]graphdb.Node{a2, b1}), false)
		Expect(rep.Items).To(ContainElement(And(
			HaveField("FindingID", conflictB1), HaveField("Status", cleaning.StatusRestored), HaveField("Action", cleaning.ReprojectStale))))
		f, _ := finding(conflictB1)
		Expect(f.Status).To(Equal(cleaning.StatusStale))
	})

	DescribeTable("refuses a finding that changed during re-projection, naming it, while the findings before it stand",
		func(prepare func() graphdb.GraphDB, firstDone types.GomegaMatcher, wantLive func(raced string) []string) {
			rebuilt := prepare()
			first, raced := min(conflictB1, conflictB2), max(conflictB1, conflictB2)
			_, err := engineOver(racingStore{Fake: store, findingID: raced}, rebuilt, signer).Reproject(ctx, engTenant, false)
			Expect(err).To(MatchError(cleaning.ErrStateChanged))
			Expect(err).To(MatchError(ContainSubstring("finding " + raced + " changed during re-projection")))
			Expect(err).To(MatchError(ContainSubstring("rerun --reproject")))
			r, ds := finding(raced)
			Expect(r.Status).To(Equal(cleaning.StatusRetired))
			Expect(r.EdgeIDs).To(Equal([]string{"edge-concurrent"}), "the concurrent change is kept")
			Expect(ds).To(HaveLen(1))
			Expect(liveEdgeIDs(ctx, rebuilt)).To(Equal(wantLive(raced)), "the raced finding's verdict edges are unchanged")
			f, _ := finding(first)
			Expect(f).To(firstDone, "the finding before it was re-projected and stands")
		},
		Entry("a retire", func() graphdb.GraphDB {
			return rebuiltGraph(ctx, "j1", rebuiltAt, [2]graphdb.Node{a2, b1}, [2]graphdb.Node{a2, b2})
		}, HaveField("EdgeIDs", HaveExactElements(HavePrefix("r-j1-"))), func(raced string) []string {
			return []string{map[string]string{conflictB1: "r-j1-a2-b1", conflictB2: "r-j1-a2-b2"}[raced]}
		}),
		Entry("a stale marking", func() graphdb.GraphDB {
			store.Jobs[engTenant] = []cleaning.Job{j2}
			return rebuiltGraph(ctx, "j2", rebuiltAt, [2]graphdb.Node{a2, b1})
		}, HaveField("Status", cleaning.StatusStale), func(string) []string { return []string{"r-j2-a2-b1"} }),
	)

	// Feedback re-projection governs mapping reviews apart from findings, and
	// the CLI runs it after cleaning's. Either engine may thus retire a
	// verdict's rebuilt edge under an edge ID the other never stored.
	DescribeTable("keeps a finding whose verdict's only edge another engine retired in sync, and marks nothing stale",
		func(restore, purged bool) {
			if restore {
				restoreB1()
			}
			if purged {
				store.Jobs[engTenant] = nil
			}
			rebuilt := rebuiltGraph(ctx, "j1", rebuiltAt, [2]graphdb.Node{a2, b1})
			otherAt := rebuiltAt.Add(time.Minute)
			_, err := cleaning.RetireEdges(ctx, rebuilt, engTenant, []string{"r-j1-a2-b1"}, otherAt)
			Expect(err).NotTo(HaveOccurred())
			before, ds := finding(conflictB1)

			rep := reproject(rebuilt, false)
			Expect(rep.Items).To(ContainElement(cleaning.ReprojectItem{
				FindingID: conflictB1, Status: before.Status, Action: cleaning.ReprojectInSync, EdgeIDs: before.EdgeIDs,
			}))
			after, dsAfter := finding(conflictB1)
			Expect(after).To(Equal(before))
			Expect(dsAfter).To(Equal(ds), "no stale decision")
			e, err := rebuilt.GetEdge(ctx, engTenant, "r-j1-a2-b1")
			Expect(err).NotTo(HaveOccurred())
			Expect(e.ValidTo).To(HaveValue(BeTemporally("==", otherAt)))

			Expect(actions(reproject(rebuilt, false))).To(HaveKeyWithValue(conflictB1, cleaning.ReprojectInSync))
		},
		Entry("a restored finding", true, false),
		Entry("a retired finding", false, false),
		Entry("a retired finding whose job retention purged", false, true),
	)

	Describe("findings that share verdicts", func() {
		// The policy also retires j1's stale_prompt finding, whose targets are
		// all four of j1's verdicts, so it shares a2->b1 with conflictB1 and
		// a2->b2 with conflictB2. Every policy decision is at engNow.
		BeforeEach(func() {
			cfg.Signals.StalePrompt = config.SignalPolicy{Action: config.CleaningActionRetire}
			store.Jobs[engTenant] = []cleaning.Job{{ID: "j1", CompletedAt: t1, Catalogs: engJob.Catalogs,
				Stages: map[string]*cleaning.StageRecord{"relationship": {PromptName: "relationship", PromptVersion: "1"}}}}
			current = func(context.Context, string, string) (cleaning.StageRecord, bool, error) {
				return cleaning.StageRecord{PromptName: "relationship", PromptVersion: "2"}, true, nil
			}
		})

		It("retires a job-level retired finding's rebuilt edges through its stored targets", func() {
			job, _ := finding(jobFindingID)
			Expect(job.Status).To(Equal(cleaning.StatusRetired))
			Expect(job.Targets).To(HaveLen(4))
			rebuilt := rebuiltGraph(ctx, "j1", rebuiltAt, allPairs...)
			rep := reproject(rebuilt, false)
			Expect(actions(rep)).To(HaveKeyWithValue(jobFindingID, cleaning.ReprojectRetire))
			Expect(liveEdgeIDs(ctx, rebuilt)).To(BeEmpty())
			job, _ = finding(jobFindingID)
			Expect(job.EdgeIDs).To(ContainElements("r-j1-a1-b1", "r-j1-a1-b2"))
		})

		Context("when a reviewer restored a shared verdict after the job-level retire", func() {
			var successor string
			JustBeforeEach(func() { successor = restoreB1() })

			It("finds every finding in sync on the unchanged graph and changes nothing", func() {
				before := snapshot(ctx, store, g)
				rep := reproject(g, false)
				for _, it := range rep.Items {
					Expect(it.Action).To(Equal(cleaning.ReprojectInSync), "finding %s", it.FindingID)
				}
				Expect(snapshot(ctx, store, g)).To(Equal(before))
				Expect(liveEdgeIDs(ctx, g)).To(ContainElement(successor))
			})

			It("keeps the restored verdict live in a rebuilt graph and retires the others", func() {
				rebuilt := rebuiltGraph(ctx, "j1", rebuiltAt, allPairs...)
				rep := reproject(rebuilt, false)
				Expect(liveEdgeIDs(ctx, rebuilt)).To(Equal([]string{"r-j1-a2-b1"}))
				Expect(actions(rep)).To(HaveKeyWithValue(conflictB1, cleaning.ReprojectRelink))
				Expect(actions(rep)).To(HaveKeyWithValue(jobFindingID, cleaning.ReprojectRetire))
				b1, _ := finding(conflictB1)
				Expect(b1.EdgeIDs).To(Equal([]string{"r-j1-a2-b1"}))

				before := snapshot(ctx, store, rebuilt)
				for _, it := range reproject(rebuilt, false).Items {
					Expect(it.Action).To(Equal(cleaning.ReprojectInSync), "finding %s", it.FindingID)
				}
				Expect(snapshot(ctx, store, rebuilt)).To(Equal(before))
			})
		})

		Context("when a reviewer retired a shared verdict after the job-level finding was restored", func() {
			JustBeforeEach(func() {
				restoreB1()
				decide(jobFindingID, cleaning.VerdictRestore)
				decide(conflictB1, cleaning.VerdictRetire)
			})

			It("finds every finding in sync on the unchanged graph and changes nothing", func() {
				before := snapshot(ctx, store, g)
				rep := reproject(g, false)
				for _, it := range rep.Items {
					Expect(it.Action).To(Equal(cleaning.ReprojectInSync), "finding %s", it.FindingID)
				}
				Expect(snapshot(ctx, store, g)).To(Equal(before))
			})

			It("retires the retired verdict in a rebuilt graph and keeps the restored ones live", func() {
				rebuilt := rebuiltGraph(ctx, "j1", rebuiltAt, allPairs...)
				rep := reproject(rebuilt, false)
				Expect(liveEdgeIDs(ctx, rebuilt)).To(Equal([]string{"r-j1-a1-b1", "r-j1-a1-b2", "r-j1-a2-b2"}))
				Expect(actions(rep)).To(HaveKeyWithValue(conflictB1, cleaning.ReprojectRetire))
				Expect(actions(rep)).To(HaveKeyWithValue(jobFindingID, cleaning.ReprojectRelink))

				before := snapshot(ctx, store, rebuilt)
				for _, it := range reproject(rebuilt, false).Items {
					Expect(it.Action).To(Equal(cleaning.ReprojectInSync), "finding %s", it.FindingID)
				}
				Expect(snapshot(ctx, store, rebuilt)).To(Equal(before))
			})
		})
	})

	// Decision IDs are content hashes, so an engine-level spec cannot choose
	// which of two simultaneous decisions has the lower ID; this drives the
	// choice directly with both input orders.
	DescribeTable("picks the decision that governs a shared verdict",
		func(xAt, yAt time.Time, xID, yID string, xFirst bool, want string) {
			shared := cleaning.VerdictKey{JobID: "j1", SourceID: "a2", TargetID: "b1"}
			fs := []cleaning.StoredFinding{{ID: "x", Targets: []cleaning.VerdictKey{shared}}, {ID: "y", Targets: []cleaning.VerdictKey{shared}}}
			ds := []cleaning.Decision{{ID: xID, FindingID: "x", DecidedAt: xAt}, {ID: yID, FindingID: "y", DecidedAt: yAt}}
			if !xFirst {
				slices.Reverse(fs)
				slices.Reverse(ds)
			}
			Expect(cleaning.Governors(fs, ds)).To(Equal(map[cleaning.VerdictKey]string{shared: want}))
		},
		Entry("the later decision, listed first", engNow.Add(time.Minute), engNow, "d2", "d1", true, "x"),
		Entry("the later decision, listed second", engNow.Add(time.Minute), engNow, "d2", "d1", false, "x"),
		Entry("on equal times the lower decision ID, listed first", engNow, engNow, "d1", "d2", true, "x"),
		Entry("on equal times the lower decision ID, listed second", engNow, engNow, "d1", "d2", false, "x"),
	)

	It("records a span with the tenant and operation", func() {
		tp, err := telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(tp.Shutdown(context.Background())).To(Succeed()) })
		eng := cleaning.NewEngine(store, g, engineConfig(), nil, signer, cleaning.WithEngineTelemetry(tp.TracerProvider(), tp.MeterProvider()))
		_, err = eng.Reproject(ctx, engTenant, true)
		Expect(err).NotTo(HaveOccurred())
		span := telemetrytest.FindSpan(tp.GetSpans(), "cleaning.Reproject")
		Expect(span).NotTo(BeNil())
		Expect(spanString(span, "tenant.id")).To(Equal(engTenant))
		Expect(spanString(span, "cleaning.operation")).To(Equal("reproject"))
	})
})
