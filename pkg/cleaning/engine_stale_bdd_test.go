//go:build !integration

package cleaning_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/cleaning"
	"github.com/complytime-labs/crosscodex/pkg/cleaning/cleaningtest"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/graphdb/memdriver"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// staleStage is the stage job j1 recorded; staleJob records prompt version 1
// for it, and the engine's current version decides whether j1 is stale.
const staleStage = "relationship"

var staleJob = cleaning.Job{
	ID: "j1", CompletedAt: t1, Catalogs: engJob.Catalogs,
	Stages: map[string]*cleaning.StageRecord{staleStage: {PromptName: staleStage, PromptVersion: "1"}},
}

// staleGraph holds one j1 SUBSET_OF verdict, edge "e-j1-<src>-<tgt>", from
// every cat-a control in sources onto every cat-b control in targets. The
// verdicts in low have agreement 0.2, the others 0.9.
func staleGraph(ctx context.Context, sources, targets []string, low ...string) graphdb.GraphDB {
	g := memdriver.New()
	Expect(g.CreateGraph(ctx, engTenant)).To(Succeed())
	for _, side := range []struct {
		ids     []string
		catalog string
	}{{sources, "cat-a"}, {targets, "cat-b"}} {
		for _, id := range side.ids {
			n := ctrl(id, side.catalog, id)
			n.ValidFrom = t1
			Expect(g.CreateNode(ctx, engTenant, n)).To(Succeed())
		}
	}
	for _, s := range sources {
		for _, t := range targets {
			id := "e-j1-" + s + "-" + t
			agree := "0.9"
			if slices.Contains(low, id) {
				agree = "0.2"
			}
			Expect(g.CreateEdge(ctx, engTenant, s, t, graphdb.Edge{
				ID: id, Label: results.SemanticMatchEdgeLabel, ValidFrom: t1,
				Properties: map[string]any{"job_id": "j1", "relationship_type": "SUBSET_OF", "agreement_fraction": agree},
			})).To(Succeed())
		}
	}
	return g
}

var _ = Describe("Engine with job-level staleness findings", func() {
	var (
		ctx     context.Context
		g       graphdb.GraphDB
		store   *cleaningtest.Fake
		cfg     *config.CleaningConfig
		eng     *cleaning.Engine
		version string // the stage's current prompt version
	)

	newEngine := func() *cleaning.Engine {
		kp, err := attestation.NewEphemeralKeyProvider()
		Expect(err).NotTo(HaveOccurred())
		signer, err := attestation.NewGenerator(kp)
		Expect(err).NotTo(HaveOccurred())
		current := func(_ context.Context, _, stage string) (cleaning.StageRecord, bool, error) {
			return cleaning.StageRecord{PromptName: stage, PromptVersion: version}, stage == staleStage, nil
		}
		return cleaning.NewEngine(store, g, cfg, current, signer, cleaning.WithClock(clock))
	}
	snap := func() engineState { return snapshot(ctx, store, g) }
	staleID := cleaning.FindingID(cleaning.Finding{
		Signal: cleaning.SignalStalePrompt, Evidence: cleaning.Evidence{Job: &cleaning.JobEvidence{ID: "j1"}},
	})
	preview := func(scope cleaning.Scope) cleaning.Plan {
		plan, err := eng.Preview(ctx, engTenant, scope)
		Expect(err).NotTo(HaveOccurred())
		return plan
	}
	apply := func() cleaning.Plan {
		plan, err := eng.Apply(ctx, engTenant, cleaning.Scope{}, preview(cleaning.Scope{}).Token)
		Expect(err).NotTo(HaveOccurred())
		return plan
	}
	stored := func(id string) cleaning.StoredFinding {
		f, _, err := store.GetFinding(ctx, engTenant, id)
		Expect(err).NotTo(HaveOccurred())
		return f
	}
	decide := func(id, verdict string, retire ...cleaning.VerdictKey) (cleaning.StoredFinding, error) {
		_, _, token, err := eng.GetFinding(ctx, engTenant, id)
		Expect(err).NotTo(HaveOccurred())
		f, _, err := eng.Decide(ctx, engTenant, cleaning.DecideRequest{
			FindingID: id, Verdict: verdict, Rationale: "reviewed", ExpectState: token, Reviewer: reviewer, RetireVerdicts: retire,
		})
		return f, err
	}
	planned := func(p cleaning.Plan, s cleaning.Signal) []cleaning.PlannedFinding {
		var out []cleaning.PlannedFinding
		for _, f := range p.Findings {
			if f.Signal == s {
				out = append(out, f)
			}
		}
		return out
	}

	BeforeEach(func() {
		var err error
		ctx, err = tenant.WithTenant(context.Background(), engTenant)
		Expect(err).NotTo(HaveOccurred())
		version = "2"
		report := config.SignalPolicy{Action: config.CleaningActionReport}
		cfg = &config.CleaningConfig{MaxRetirePerRun: 1000, Signals: config.CleaningSignals{
			EquivalenceFanout: report, CrossJobContradiction: report, ConflictsWith: report, StaleModel: report,
			LowAgreement: config.SignalPolicy{Action: config.CleaningActionHuman, MinAgreementFraction: 0.5},
			StalePrompt:  config.SignalPolicy{Action: config.CleaningActionHuman},
		}}
		g = staleGraph(ctx, []string{"a1", "a2"}, []string{"b1", "b2"}, "e-j1-a1-b2")
		store = engineStore()
		store.Jobs[engTenant] = []cleaning.Job{staleJob}
		eng = newEngine()
	})

	It("retires a 2,000-verdict job under retire and restores every edge it retired", func() {
		var sources, targets []string
		for i := range 50 {
			sources = append(sources, fmt.Sprintf("a%02d", i))
		}
		for i := range 40 {
			targets = append(targets, fmt.Sprintf("b%02d", i))
		}
		g = staleGraph(ctx, sources, targets)
		original := liveEdgeIDs(ctx, g)
		Expect(original).To(HaveLen(2000))
		cfg.MaxRetirePerRun = 2000
		cfg.Signals.StalePrompt.Action = config.CleaningActionRetire
		eng = newEngine()

		plan := preview(cleaning.Scope{})
		Expect(planned(plan, cleaning.SignalStalePrompt)).To(HaveLen(1))
		Expect(plan.Retire).To(HaveLen(2000))
		Expect(plan.RetireEdgeIDs).To(Equal(original))
		apply()
		Expect(liveEdgeIDs(ctx, g)).To(BeEmpty())
		retired := stored(staleID)
		Expect(retired.Status).To(Equal(cleaning.StatusRetired))
		Expect(retired.EdgeIDs).To(Equal(original))
		Expect(retired.Evidence.Job.VerdictsInScope).To(Equal(2000))
		Expect(retired.Evidence.Verdicts).To(BeEmpty())

		restored, err := decide(staleID, cleaning.VerdictRestore)
		Expect(err).NotTo(HaveOccurred())
		Expect(restored.Status).To(Equal(cleaning.StatusRestored))
		rels, err := g.QueryRelationships(ctx, engTenant, graphdb.RelationshipQuery{EdgeLabel: results.SemanticMatchEdgeLabel})
		Expect(err).NotTo(HaveOccurred())
		var supersedes []string
		for _, r := range rels {
			supersedes = append(supersedes, r.Edge.Supersedes)
		}
		slices.Sort(supersedes)
		Expect(supersedes).To(Equal(original), "every retired edge has a live successor")
	})

	It("keeps a dismissal while another finding retires and restores one of the job's verdicts", func() {
		apply()
		lowID := lowIDOf(key("j1", a1, b2))
		Expect(stored(lowID).Status).To(Equal(cleaning.StatusPending))
		_, err := decide(staleID, cleaning.VerdictDismiss)
		Expect(err).NotTo(HaveOccurred())

		_, err = decide(lowID, cleaning.VerdictRetire)
		Expect(err).NotTo(HaveOccurred())
		Expect(planned(preview(cleaning.Scope{}), cleaning.SignalStalePrompt)).To(BeEmpty())
		Expect(stored(staleID).Status).To(Equal(cleaning.StatusDismissed))

		_, err = decide(lowID, cleaning.VerdictRestore)
		Expect(err).NotTo(HaveOccurred())
		Expect(planned(preview(cleaning.Scope{}), cleaning.SignalStalePrompt)).To(BeEmpty())
		Expect(stored(staleID).Status).To(Equal(cleaning.StatusDismissed))
	})

	It("re-raises a dismissed finding under the same ID when the stage's current value changes", func() {
		apply()
		_, err := decide(staleID, cleaning.VerdictDismiss)
		Expect(err).NotTo(HaveOccurred())
		Expect(planned(preview(cleaning.Scope{}), cleaning.SignalStalePrompt)).To(BeEmpty())

		version = "3"
		stale := planned(preview(cleaning.Scope{}), cleaning.SignalStalePrompt)
		Expect(stale).To(HaveLen(1))
		Expect(stale[0].ID).To(Equal(staleID))
		Expect(stale[0].Change).To(Equal(cleaning.ChangeReraised))
		Expect(stale[0].Evidence.Detail).To(HaveKeyWithValue("relationship.current", "3"))
	})

	It("gives the job's finding the same ID in a scoped and an unscoped run", func() {
		unscoped := planned(preview(cleaning.Scope{}), cleaning.SignalStalePrompt)
		scoped := planned(preview(cleaning.Scope{SourceCatalog: "cat-b", TargetCatalog: "cat-a"}), cleaning.SignalStalePrompt)
		Expect(unscoped).To(HaveLen(1))
		Expect(scoped).To(HaveLen(1))
		Expect(scoped[0].ID).To(Equal(staleID))
		Expect(unscoped[0].ID).To(Equal(staleID))
	})

	It("counts the expanded verdicts against the retire cap and writes nothing when over it", func() {
		cfg.Signals.StalePrompt.Action = config.CleaningActionRetire
		cfg.MaxRetirePerRun = 3
		eng = newEngine()
		before := snap()

		_, err := eng.Preview(ctx, engTenant, cleaning.Scope{})
		Expect(err).To(MatchError(cleaning.ErrRetireCapExceeded))
		Expect(err).To(MatchError(ContainSubstring("this run would retire 4 verdicts, more than cleaning.max_retire_per_run (3). " +
			"Narrow the run with --source-catalog/--target-catalog, or raise the setting for this tenant")))
		_, err = eng.Apply(ctx, engTenant, cleaning.Scope{}, "any-token")
		Expect(err).To(MatchError(cleaning.ErrRetireCapExceeded))
		Expect(snap()).To(Equal(before))
	})

	It("refuses an apply under retire after one of the job's verdicts was retired since the preview, and writes nothing", func() {
		cfg.Signals.StalePrompt.Action = config.CleaningActionRetire
		eng = newEngine()
		plan := preview(cleaning.Scope{})
		Expect(plan.Retire).To(ContainElement(key("j1", a2, b2)))
		_, err := g.SupersedeFact(ctx, engTenant, graphdb.SupersedeRequest{EdgeID: "e-j1-a2-b2", SupersededAt: engNow})
		Expect(err).NotTo(HaveOccurred())
		before := snap()

		_, err = eng.Apply(ctx, engTenant, cleaning.Scope{}, plan.Token)
		Expect(err).To(MatchError(cleaning.ErrTokenMismatch))
		Expect(err).To(MatchError(ContainSubstring("the graph, findings or cleaning policy changed since the preview, " +
			"so this apply was refused and nothing was written. Preview again and apply the new token")))
		Expect(snap()).To(Equal(before))
	})

	Describe("a reviewer's retire", func() {
		BeforeEach(func() { apply() })

		It("signs the count and hash of the verdicts it retires, recomputable from the stored targets", func() {
			f, err := decide(staleID, cleaning.VerdictRetire)
			Expect(err).NotTo(HaveOccurred())
			Expect(f.Targets).To(HaveLen(4))
			_, ds, err := store.GetFinding(ctx, engTenant, staleID)
			Expect(err).NotTo(HaveOccurred())
			var req struct {
				RetireVerdicts      []string `json:"retire_verdicts"`
				RetireTargetsCount  int      `json:"retire_targets_count"`
				RetireTargetsSHA256 string   `json:"retire_targets_sha256"`
			}
			Expect(json.Unmarshal(ds[len(ds)-1].Request, &req)).To(Succeed())
			Expect(req.RetireVerdicts).To(BeEmpty())
			Expect(req.RetireTargetsCount).To(Equal(len(f.Targets)))
			Expect(req.RetireTargetsSHA256).To(Equal(targetsHash(f.Targets)))
		})

		It("signs an empty retire set on a dismiss", func() {
			_, err := decide(staleID, cleaning.VerdictDismiss)
			Expect(err).NotTo(HaveOccurred())
			_, ds, err := store.GetFinding(ctx, engTenant, staleID)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(ds[len(ds)-1].Request)).To(ContainSubstring(`"retire_targets_count":0,"retire_targets_sha256":""`))
		})

		It("refuses a retire whose stored targets would exceed their bound before signing, saying to name fewer verdicts", func() {
			DeferCleanup(cleaning.SetStoredListLimit(100))
			before := snap()
			_, err := decide(staleID, cleaning.VerdictRetire)
			Expect(err).To(MatchError(cleaning.ErrFindingTooLarge))
			Expect(err).To(MatchError(ContainSubstring("the targets column would hold")))
			Expect(err).To(MatchError(ContainSubstring("Name fewer verdicts with --retire-verdict")))
			Expect(snap()).To(Equal(before))

			_, err = decide(staleID, cleaning.VerdictRetire, key("j1", a2, b2))
			Expect(err).NotTo(HaveOccurred())
		})

		It("expands to the job's live verdicts at decide time", func() {
			_, err := decide(lowIDOf(key("j1", a1, b2)), cleaning.VerdictRetire)
			Expect(err).NotTo(HaveOccurred())
			f, err := decide(staleID, cleaning.VerdictRetire)
			Expect(err).NotTo(HaveOccurred())
			Expect(f.Targets).To(Equal([]cleaning.VerdictKey{key("j1", a1, b1), key("j1", a2, b1), key("j1", a2, b2)}))
			Expect(f.EdgeIDs).To(Equal([]string{"e-j1-a1-b1", "e-j1-a2-b1", "e-j1-a2-b2"}))
			Expect(liveEdgeIDs(ctx, g)).To(BeEmpty())
		})

		It("retires only the named subset of the job's live verdicts", func() {
			f, err := decide(staleID, cleaning.VerdictRetire, key("j1", a2, b2))
			Expect(err).NotTo(HaveOccurred())
			Expect(f.Targets).To(Equal([]cleaning.VerdictKey{key("j1", a2, b2)}))
			Expect(liveEdgeIDs(ctx, g)).To(Equal([]string{"e-j1-a1-b1", "e-j1-a1-b2", "e-j1-a2-b1"}))
		})

		DescribeTable("refuses a verdict outside the job's live verdicts and changes nothing",
			func(setup func() cleaning.VerdictKey, want string) {
				k := setup()
				before := snap()
				_, err := decide(staleID, cleaning.VerdictRetire, k)
				Expect(err).To(MatchError(cleaning.ErrVerdictNotInFinding))
				Expect(err).To(MatchError(ContainSubstring(want)))
				Expect(err).To(MatchError(ContainSubstring("name only the job's live verdicts; nothing was written")))
				Expect(snap()).To(Equal(before))
			},
			Entry("another job's verdict", func() cleaning.VerdictKey { return key("j2", a1, b1) },
				"j2:a1:b1 is not a live verdict of job j1"),
			Entry("a verdict of the job already retired", func() cleaning.VerdictKey {
				_, err := decide(lowIDOf(key("j1", a1, b2)), cleaning.VerdictRetire)
				Expect(err).NotTo(HaveOccurred())
				return key("j1", a1, b2)
			}, "j1:a1:b2 is not a live verdict of job j1"),
		)

		It("refuses a job with no live verdicts and changes nothing", func() {
			_, err := decide(staleID, cleaning.VerdictRetire)
			Expect(err).NotTo(HaveOccurred())
			_, err = decide(staleID, cleaning.VerdictRestore)
			Expect(err).NotTo(HaveOccurred())
			for _, id := range liveEdgeIDs(ctx, g) {
				_, err := g.SupersedeFact(ctx, engTenant, graphdb.SupersedeRequest{EdgeID: id, SupersededAt: engNow.Add(time.Minute)})
				Expect(err).NotTo(HaveOccurred())
			}
			before := snap()
			_, err = decide(staleID, cleaning.VerdictRetire)
			Expect(err).To(MatchError(cleaning.ErrEdgeNotLive))
			Expect(err).To(MatchError(ContainSubstring("job j1 has no live verdict between catalogs cat-a and cat-b, so there is nothing to retire")))
			Expect(snap()).To(Equal(before))
		})
	})
})

// lowIDOf is the ID of the low_agreement finding on k.
func lowIDOf(k cleaning.VerdictKey) string {
	return cleaning.FindingID(cleaning.Finding{Signal: cleaning.SignalLowAgreement, Concerns: []cleaning.VerdictKey{k}})
}

// targetsHash is retire_targets_sha256 recomputed independently: the hex
// SHA-256 of the sorted <job>:<source>:<target> keys, each prefixed with its
// decimal length and ':'.
func targetsHash(ks []cleaning.VerdictKey) string {
	parts := make([]string, len(ks))
	for i, k := range ks {
		parts[i] = k.String()
	}
	slices.Sort(parts)
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(strconv.Itoa(len(p)) + ":" + p)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
