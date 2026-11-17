//go:build !integration

package cleaning_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/cleaning"
	"github.com/complytime-labs/crosscodex/pkg/cleaning/cleaningtest"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/crosswalk"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/graphdb/memdriver"
	"github.com/complytime-labs/crosscodex/pkg/jobconfig"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

const (
	engTenant = "tenant-engine"
	reviewer  = "rev-1"
	resumeMsg = "run 'crosscodex admin cleaning run --resume'"
)

// engNow is the engine's and the store's clock: after every fixture edge's
// valid_from.
var engNow = time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)

var errInjected = errors.New("injected graph failure")

// engineGraph holds job j1's verdicts from cat-a onto cat-b: a1 is EQUIVALENT
// to b1 and to b2 (an equivalence fanout; j1:a1:b2 also has a non-viable
// synthesized edge), and a2 CONFLICTS_WITH b1 and b2 (one conflicts_with
// finding each).
func engineGraph(ctx context.Context) graphdb.GraphDB {
	g := memdriver.New()
	Expect(g.CreateGraph(ctx, engTenant)).To(Succeed())
	for _, n := range []graphdb.Node{a1, a2, b1, b2} {
		n.ValidFrom = t1
		Expect(g.CreateNode(ctx, engTenant, n)).To(Succeed())
	}
	for _, e := range []struct{ id, src, tgt, typ, stage string }{
		{"e-j1-a1-b1", "a1", "b1", "EQUIVALENT", ""},
		{"e-j1-a1-b2", "a1", "b2", "EQUIVALENT", ""},
		{"s-j1-a1-b2", "a1", "b2", "EQUIVALENT", results.StageSynthesized},
		{"e-j1-a2-b1", "a2", "b1", "CONFLICTS_WITH", ""},
		{"e-j1-a2-b2", "a2", "b2", "CONFLICTS_WITH", ""},
	} {
		props := map[string]any{"job_id": "j1", "relationship_type": e.typ}
		if e.stage != "" {
			props[results.StageProperty], props["is_viable"] = e.stage, false
		}
		Expect(g.CreateEdge(ctx, engTenant, e.src, e.tgt, graphdb.Edge{
			ID: e.id, Label: results.SemanticMatchEdgeLabel, ValidFrom: t1, Properties: props,
		})).To(Succeed())
	}
	return g
}

// engineConfig queues equivalence fanout for review and retires
// conflicts_with; every other signal reports.
func engineConfig() *config.CleaningConfig {
	report := config.SignalPolicy{Action: config.CleaningActionReport}
	return &config.CleaningConfig{MaxRetirePerRun: 1000, Signals: config.CleaningSignals{
		EquivalenceFanout:     config.SignalPolicy{Action: config.CleaningActionHuman},
		CrossJobContradiction: report,
		LowAgreement:          config.SignalPolicy{Action: config.CleaningActionReport, MinAgreementFraction: 0.5},
		ConflictsWith:         config.SignalPolicy{Action: config.CleaningActionRetire},
		StalePrompt:           report, StaleModel: report,
	}}
}

var engJob = cleaning.Job{ID: "j1", CompletedAt: t1, Catalogs: jobconfig.Catalogs{SourceID: "cat-a", TargetID: "cat-b"}}

func engineStore(seed ...cleaning.StoredFinding) *cleaningtest.Fake {
	return &cleaningtest.Fake{
		Seed:      map[string][]cleaning.StoredFinding{engTenant: seed},
		Jobs:      map[string][]cleaning.Job{engTenant: {engJob}},
		Reviewers: map[string]map[string]string{engTenant: {reviewer: cleaningtest.ReviewerActive}},
		Now:       func() time.Time { return engNow },
	}
}

func clock() time.Time { return engNow }

var (
	fanoutID = cleaning.FindingID(cleaning.Finding{
		Signal: cleaning.SignalEquivalenceFanout, Concerns: []cleaning.VerdictKey{key("j1", a1, b1), key("j1", a1, b2)},
	})
	conflictB1 = cleaning.FindingID(cleaning.Finding{Signal: cleaning.SignalConflictsWith, Concerns: []cleaning.VerdictKey{key("j1", a2, b1)}})
	conflictB2 = cleaning.FindingID(cleaning.Finding{Signal: cleaning.SignalConflictsWith, Concerns: []cleaning.VerdictKey{key("j1", a2, b2)}})
)

// engineState is everything an engine call may write.
type engineState struct {
	Findings  []cleaning.StoredFinding
	Decisions [][]cleaning.Decision
	Live      []string
}

func liveEdgeIDs(ctx context.Context, g graphdb.GraphDB) []string {
	rels, err := g.QueryRelationships(ctx, engTenant, graphdb.RelationshipQuery{EdgeLabel: results.SemanticMatchEdgeLabel})
	Expect(err).NotTo(HaveOccurred())
	ids := make([]string, len(rels))
	for i, r := range rels {
		ids[i] = r.Edge.ID
	}
	slices.Sort(ids)
	return ids
}

func snapshot(ctx context.Context, store *cleaningtest.Fake, g graphdb.GraphDB) engineState {
	fs, err := store.Findings(ctx, engTenant, cleaning.Scope{})
	Expect(err).NotTo(HaveOccurred())
	s := engineState{Findings: fs, Live: liveEdgeIDs(ctx, g)}
	for _, f := range fs {
		_, ds, err := store.GetFinding(ctx, engTenant, f.ID)
		Expect(err).NotTo(HaveOccurred())
		s.Decisions = append(s.Decisions, ds)
	}
	return s
}

// loadVerdicts returns the j1 verdicts LoadPair reads between cat-a and cat-b
// in stage, non-viable ones included.
func loadVerdicts(ctx context.Context, g graphdb.GraphDB, stage string) []crosswalk.EdgeVerdict {
	pairs, _, err := crosswalk.LoadPair(ctx, g, engTenant, crosswalk.PairQuery{
		A: "cat-a", B: "cat-b", Jobs: map[string]time.Time{"j1": t1}, Stage: stage,
		IncludeNonViable: true, IncludeNoRelationship: true,
	})
	Expect(err).NotTo(HaveOccurred())
	var out []crosswalk.EdgeVerdict
	for _, p := range pairs {
		out = append(out, p.Verdicts...)
	}
	return out
}

func verdictOn(src, tgt string) func(crosswalk.EdgeVerdict) bool {
	return func(v crosswalk.EdgeVerdict) bool { return v.SourceID == src && v.TargetID == tgt }
}

// failNthSupersede fails the nth SupersedeFact call and records the edges the
// others superseded.
type failNthSupersede struct {
	graphdb.GraphDB
	n, calls   int
	superseded []string
}

func (f *failNthSupersede) SupersedeFact(ctx context.Context, tenantID string, req graphdb.SupersedeRequest) (bool, error) {
	f.calls++
	if f.calls == f.n {
		return false, errInjected
	}
	ok, err := f.GraphDB.SupersedeFact(ctx, tenantID, req)
	if ok && err == nil {
		f.superseded = append(f.superseded, req.EdgeID)
	}
	return ok, err
}

// supersededOnGet reports one edge as superseded to GetEdge only, as if a
// writer superseded it after the plan was read.
type supersededOnGet struct {
	graphdb.GraphDB
	edgeID string
}

func (s supersededOnGet) GetEdge(ctx context.Context, tenantID, edgeID string) (*graphdb.EdgeWithEndpoints, error) {
	e, err := s.GraphDB.GetEdge(ctx, tenantID, edgeID)
	if err == nil && edgeID == s.edgeID {
		at := engNow
		e.ValidTo = &at
	}
	return e, err
}

func spanString(span sdktrace.ReadOnlySpan, key string) string {
	v, ok := telemetrytest.SpanAttribute(span, key)
	Expect(ok).To(BeTrue(), "span %s has attribute %s", span.Name(), key)
	return v.AsString()
}

// countQueries counts QueryRelationships calls.
type countQueries struct {
	graphdb.GraphDB
	calls int
}

func (c *countQueries) QueryRelationships(ctx context.Context, tenantID string, q graphdb.RelationshipQuery) ([]graphdb.Relationship, error) {
	c.calls++
	return c.GraphDB.QueryRelationships(ctx, tenantID, q)
}

var _ = Describe("Engine", func() {
	var (
		ctx    context.Context
		g      graphdb.GraphDB
		store  *cleaningtest.Fake
		cfg    *config.CleaningConfig
		signer attestation.Generator
		eng    *cleaning.Engine
	)

	newEngine := func(sig attestation.Generator, opts ...cleaning.EngineOption) *cleaning.Engine {
		return cleaning.NewEngine(store, g, cfg, nil, sig, append([]cleaning.EngineOption{cleaning.WithClock(clock)}, opts...)...)
	}
	snap := func() engineState { return snapshot(ctx, store, g) }
	stateOf := func(id string) string {
		_, _, token, err := eng.GetFinding(ctx, engTenant, id)
		Expect(err).NotTo(HaveOccurred())
		return token
	}
	applyFixture := func() cleaning.Plan {
		plan, err := eng.Preview(ctx, engTenant, cleaning.Scope{})
		Expect(err).NotTo(HaveOccurred())
		applied, err := eng.Apply(ctx, engTenant, cleaning.Scope{}, plan.Token)
		Expect(err).NotTo(HaveOccurred())
		return applied
	}

	BeforeEach(func() {
		var err error
		ctx, err = tenant.WithTenant(context.Background(), engTenant)
		Expect(err).NotTo(HaveOccurred())
		g = engineGraph(ctx)
		store = engineStore()
		cfg = engineConfig()
		kp, err := attestation.NewEphemeralKeyProvider()
		Expect(err).NotTo(HaveOccurred())
		signer, err = attestation.NewGenerator(kp)
		Expect(err).NotTo(HaveOccurred())
		eng = newEngine(signer)
	})

	Describe("Preview", func() {
		It("plans every finding and writes nothing", func() {
			before := snap()
			plan, err := eng.Preview(ctx, engTenant, cleaning.Scope{})
			Expect(err).NotTo(HaveOccurred())

			to := map[string]cleaning.Status{}
			for _, p := range plan.Findings {
				to[p.ID] = p.To
			}
			Expect(to).To(Equal(map[string]cleaning.Status{
				fanoutID: cleaning.StatusPending, conflictB1: cleaning.StatusRetired, conflictB2: cleaning.StatusRetired,
			}))
			Expect(plan.RetireEdgeIDs).To(Equal([]string{"e-j1-a2-b1", "e-j1-a2-b2"}))
			Expect(plan.Token).NotTo(BeEmpty())
			Expect(snap()).To(Equal(before))
		})

		It("calls current once per distinct stage name and leaves out a stage it does not know", func() {
			recorded := &cleaning.StageRecord{PromptName: "relationship", PromptVersion: "1"}
			store.Jobs[engTenant] = []cleaning.Job{
				{ID: "j1", CompletedAt: t1, Catalogs: engJob.Catalogs, Stages: map[string]*cleaning.StageRecord{"relationship": recorded, "synthesis": nil}},
				{ID: "j2", CompletedAt: t2, Catalogs: engJob.Catalogs, Stages: map[string]*cleaning.StageRecord{"relationship": recorded}},
			}
			var calls []string
			current := func(_ context.Context, tenantID, stage string) (cleaning.StageRecord, bool, error) {
				Expect(tenantID).To(Equal(engTenant))
				calls = append(calls, stage)
				if stage == "synthesis" {
					return cleaning.StageRecord{}, false, nil
				}
				return cleaning.StageRecord{PromptName: "relationship", PromptVersion: "2"}, true, nil
			}
			e := cleaning.NewEngine(store, g, cfg, current, signer, cleaning.WithClock(clock))

			plan, err := e.Preview(ctx, engTenant, cleaning.Scope{})
			Expect(err).NotTo(HaveOccurred())
			Expect(calls).To(ConsistOf("relationship", "synthesis"))
			stale := slices.IndexFunc(plan.Findings, func(p cleaning.PlannedFinding) bool { return p.Signal == cleaning.SignalStalePrompt })
			Expect(stale).NotTo(Equal(-1), "the relationship stage's prompt changed from 1 to 2")
			Expect(plan.Findings[stale].Evidence.Detail).To(HaveKeyWithValue("stages", "relationship"))
		})

		It("walks a catalog pair once when jobs map it in both directions", func() {
			counter := &countQueries{GraphDB: g}
			g = counter
			store.Jobs[engTenant] = []cleaning.Job{
				engJob,
				{ID: "j2", CompletedAt: t2, Catalogs: jobconfig.Catalogs{SourceID: "cat-b", TargetID: "cat-a", Bidirectional: true}},
			}
			_, err := newEngine(signer).Preview(ctx, engTenant, cleaning.Scope{})
			Expect(err).NotTo(HaveOccurred())
			// One pair: two stages, each read in both directions.
			Expect(counter.calls).To(Equal(4))
		})

		It("refuses a stored finding with an unknown status, naming it, and writes nothing", func() {
			row := storedOf(planFinding(cleaning.SignalLowAgreement, []cleaning.VerdictKey{key("j9", a1, b1)}, nil), "archived")
			row.GraphApplied = true
			store = engineStore(row)
			eng = newEngine(signer)
			before := snap()

			_, err := eng.Preview(ctx, engTenant, cleaning.Scope{})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(`stored finding ` + row.ID + ` has unknown status "archived", so the run was refused and nothing was written`))
			Expect(err.Error()).To(ContainSubstring("set its status back to one of reported, pending, retired, dismissed, restored or cleared"))
			Expect(snap()).To(Equal(before))
		})
	})

	Describe("Apply", func() {
		It("retires what the preview showed and records one policy decision per retired finding sharing one link", func() {
			plan, err := eng.Preview(ctx, engTenant, cleaning.Scope{})
			Expect(err).NotTo(HaveOccurred())
			applied, err := eng.Apply(ctx, engTenant, cleaning.Scope{}, plan.Token)
			Expect(err).NotTo(HaveOccurred())
			Expect(applied.Token).To(Equal(plan.Token))

			Expect(liveEdgeIDs(ctx, g)).To(Equal([]string{"e-j1-a1-b1", "e-j1-a1-b2", "s-j1-a1-b2"}))
			for _, id := range []string{"e-j1-a2-b1", "e-j1-a2-b2"} {
				e, err := g.GetEdge(ctx, engTenant, id)
				Expect(err).NotTo(HaveOccurred())
				Expect(e.ValidTo).To(HaveValue(BeTemporally("==", engNow)))
			}

			s := snap()
			status := map[string]cleaning.Status{}
			for _, f := range s.Findings {
				Expect(f.GraphApplied).To(BeTrue(), "finding %s projected", f.ID)
				status[f.ID] = f.Status
			}
			Expect(status).To(Equal(map[string]cleaning.Status{
				fanoutID: cleaning.StatusPending, conflictB1: cleaning.StatusRetired, conflictB2: cleaning.StatusRetired,
			}))

			var decisions []cleaning.Decision
			for _, ds := range s.Decisions {
				decisions = append(decisions, ds...)
			}
			Expect(decisions).To(HaveLen(2))
			Expect(decisions[0].Attestation).To(Equal(decisions[1].Attestation))
			for _, d := range decisions {
				Expect(d.DecidedBy).To(Equal(cleaning.DecidedByPolicy))
				Expect(d.RunID).To(Equal(plan.Token))
				Expect(d.Verdict).To(Equal(cleaning.VerdictRetire))
				sum := sha256.Sum256(d.Request)
				Expect(d.ID).To(Equal(hex.EncodeToString(sum[:])))
			}

			link, err := signer.Verify(ctx, decisions[0].Attestation)
			Expect(err).NotTo(HaveOccurred())
			Expect(link.Step).To(Equal("cleaning.run"))
			Expect(link.Materials).To(ConsistOf(attestation.Artifact{URI: "crosscodex:cleaning-plan", Digest: plan.Token}))
			var products []attestation.Artifact
			for _, p := range plan.Findings {
				products = append(products, attestation.Artifact{URI: "crosscodex:finding/" + p.ID, Digest: p.EvidenceHash})
			}
			Expect(link.Products).To(ConsistOf(products))
		})

		It("yields no changes when the run is previewed again", func() {
			applyFixture()
			again, err := eng.Preview(ctx, engTenant, cleaning.Scope{})
			Expect(err).NotTo(HaveOccurred())
			Expect(again.Findings).To(BeEmpty())
		})

		It("instruments the run and signs a link whose byproducts carry the run's trace ID", func() {
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { Expect(tp.Shutdown(context.Background())).To(Succeed()) })
			eng = newEngine(signer, cleaning.WithEngineTelemetry(tp.TracerProvider(), tp.MeterProvider()))

			applyFixture()

			span := telemetrytest.FindSpan(tp.GetSpans(), "cleaning.Apply")
			Expect(span).NotTo(BeNil())
			Expect(spanString(span, "tenant.id")).To(Equal(engTenant))
			Expect(spanString(span, "cleaning.operation")).To(Equal("apply"))

			_, ds, err := store.GetFinding(ctx, engTenant, conflictB1)
			Expect(err).NotTo(HaveOccurred())
			link, err := signer.Verify(ctx, ds[0].Attestation)
			Expect(err).NotTo(HaveOccurred())
			Expect(link.ByProducts).To(HaveKeyWithValue("trace_id", span.SpanContext().TraceID().String()))

			metrics := tp.GetMetrics()
			total := telemetrytest.FindMetric(metrics, "cleaning.findings.total")
			Expect(total).NotTo(BeNil())
			Expect(telemetrytest.CounterValue(total)).To(Equal(int64(3)))
			duration := telemetrytest.FindMetric(metrics, "cleaning.operation.duration_ms")
			Expect(duration).NotTo(BeNil())
			Expect(telemetrytest.Float64HistogramCount(duration)).To(BeNumerically(">=", 2), "preview and apply")
			pending := telemetrytest.FindMetric(metrics, "cleaning.findings.pending")
			Expect(pending).NotTo(BeNil())
			Expect(telemetrytest.GaugeValue(pending)).To(Equal(int64(1)))
		})

		DescribeTable("refuses the run before any write and leaves findings and graph unchanged",
			func(arrange func() (*cleaning.Engine, string), want error, msg string) {
				e, token := arrange()
				before := snap()
				_, err := e.Apply(ctx, engTenant, cleaning.Scope{}, token)
				Expect(err).To(MatchError(want))
				Expect(err.Error()).To(ContainSubstring(msg))
				Expect(snap()).To(Equal(before))
			},
			Entry("a stale plan token", func() (*cleaning.Engine, string) { return eng, "token-of-an-older-preview" },
				cleaning.ErrTokenMismatch, "changed since the preview, so this apply was refused and nothing was written. Preview again and apply the new token"),
			Entry("no signer", func() (*cleaning.Engine, string) {
				plan, err := eng.Preview(ctx, engTenant, cleaning.Scope{})
				Expect(err).NotTo(HaveOccurred())
				return newEngine(nil), plan.Token
			}, cleaning.ErrSigningDisabled, "set attestation.private_key_path and attestation.public_key_path"),
			Entry("a plan over the retire cap", func() (*cleaning.Engine, string) {
				cfg.MaxRetirePerRun = 1
				return newEngine(signer), "any"
			}, cleaning.ErrRetireCapExceeded, "would retire 2 verdicts, more than cleaning.max_retire_per_run (1)"),
			Entry("a target edge superseded between preview and apply", func() (*cleaning.Engine, string) {
				plan, err := eng.Preview(ctx, engTenant, cleaning.Scope{})
				Expect(err).NotTo(HaveOccurred())
				_, err = g.SupersedeFact(ctx, engTenant, graphdb.SupersedeRequest{EdgeID: "e-j1-a2-b1", SupersededAt: engNow})
				Expect(err).NotTo(HaveOccurred())
				return eng, plan.Token
			}, cleaning.ErrTokenMismatch, "Preview again"),
			Entry("a target edge found superseded by the pre-write check", func() (*cleaning.Engine, string) {
				plan, err := eng.Preview(ctx, engTenant, cleaning.Scope{})
				Expect(err).NotTo(HaveOccurred())
				g = supersededOnGet{GraphDB: g, edgeID: "e-j1-a2-b2"}
				return newEngine(signer), plan.Token
			}, cleaning.ErrEdgeNotLive, `edge "e-j1-a2-b2"`),
			Entry("findings that changed while the run was applied", func() (*cleaning.Engine, string) {
				plan, err := eng.Preview(ctx, engTenant, cleaning.Scope{})
				Expect(err).NotTo(HaveOccurred())
				store.ApplyRunFunc = func(context.Context, string, cleaning.RunWrite) error {
					return cleaning.ErrStateChanged
				}
				return eng, plan.Token
			}, cleaning.ErrStateChanged, "Preview again and apply the new token"),
		)

		DescribeTable("refuses an apply after the policy changed since the preview and writes nothing",
			func(change func()) {
				plan, err := eng.Preview(ctx, engTenant, cleaning.Scope{})
				Expect(err).NotTo(HaveOccurred())
				change()
				before := snap()
				_, err = eng.Apply(ctx, engTenant, cleaning.Scope{}, plan.Token)
				Expect(err).To(MatchError(cleaning.ErrTokenMismatch))
				Expect(err.Error()).To(ContainSubstring("cleaning policy changed since the preview, so this apply was refused and nothing was written. " +
					"Preview again and apply the new token"))
				Expect(snap()).To(Equal(before))
			},
			Entry("one signal's action", func() { cfg.Signals.LowAgreement.Action = config.CleaningActionHuman }),
			Entry("a tenant override", func() {
				limit := 5
				cfg.TenantOverrides = map[string]config.CleaningOverride{engTenant: {MaxRetirePerRun: &limit}}
			}),
		)
	})

	Describe("tenant guard", func() {
		const otherTenant = "tenant-other"
		otherCtx := func() context.Context {
			c, err := tenant.WithTenant(context.Background(), otherTenant)
			Expect(err).NotTo(HaveOccurred())
			return c
		}
		noTenant := context.Background
		preview := func(c context.Context) error { _, err := eng.Preview(c, engTenant, cleaning.Scope{}); return err }
		apply := func(c context.Context) error { _, err := eng.Apply(c, engTenant, cleaning.Scope{}, "any"); return err }
		decide := func(c context.Context) error {
			_, _, err := eng.Decide(c, engTenant, cleaning.DecideRequest{
				FindingID: fanoutID, Verdict: cleaning.VerdictDismiss, Rationale: "r", ExpectState: "any", Reviewer: reviewer,
			})
			return err
		}

		DescribeTable("refuses a request outside its authenticated tenant, names no tenant, and changes nothing",
			func(callCtx func() context.Context, run func(context.Context) error, want error, msg string) {
				before := snap()
				err := run(callCtx())
				Expect(err).To(MatchError(want))
				Expect(err.Error()).To(ContainSubstring(msg))
				Expect(err.Error()).NotTo(ContainSubstring(otherTenant))
				Expect(err.Error()).NotTo(ContainSubstring(engTenant))
				Expect(snap()).To(Equal(before))
			},
			Entry("preview for another tenant", otherCtx, preview, cleaning.ErrTenantMismatch, "Send the request for your own tenant"),
			Entry("apply for another tenant", otherCtx, apply, cleaning.ErrTenantMismatch, "Send the request for your own tenant"),
			Entry("decide for another tenant", otherCtx, decide, cleaning.ErrTenantMismatch, "Send the request for your own tenant"),
			Entry("preview without a tenant", noTenant, preview, tenant.ErrNoTenant, "carries no authenticated tenant"),
			Entry("apply without a tenant", noTenant, apply, tenant.ErrNoTenant, "carries no authenticated tenant"),
			Entry("decide without a tenant", noTenant, decide, tenant.ErrNoTenant, "carries no authenticated tenant"),
		)
	})

	DescribeTable("refuses a scope with one catalog",
		func(run func(*cleaning.Engine, cleaning.Scope) error, scope cleaning.Scope) {
			before := snap()
			err := run(eng, scope)
			Expect(err).To(MatchError(cleaning.ErrInvalidScope))
			Expect(err.Error()).To(ContainSubstring("--source-catalog"))
			Expect(err.Error()).To(ContainSubstring("--target-catalog"))
			Expect(snap()).To(Equal(before))
		},
		Entry("preview with a source catalog only", func(e *cleaning.Engine, s cleaning.Scope) error {
			_, err := e.Preview(ctx, engTenant, s)
			return err
		}, cleaning.Scope{SourceCatalog: "cat-a"}),
		Entry("apply with a target catalog only", func(e *cleaning.Engine, s cleaning.Scope) error {
			_, err := e.Apply(ctx, engTenant, s, "any")
			return err
		}, cleaning.Scope{TargetCatalog: "cat-b"}),
	)

	Describe("with an unprojected finding stored", func() {
		BeforeEach(func() {
			row := storedOf(planFinding(cleaning.SignalConflictsWith, []cleaning.VerdictKey{key("j1", a2, b1)}, []cleaning.VerdictKey{key("j1", a2, b1)}), cleaning.StatusRetired)
			store = engineStore(row)
			eng = newEngine(signer)
		})

		DescribeTable("refuses until the run is resumed and changes nothing",
			func(run func() error) {
				before := snap()
				err := run()
				Expect(err).To(MatchError(cleaning.ErrUnprojected))
				Expect(err.Error()).To(ContainSubstring("1 findings from an earlier run or decision are recorded but not yet applied to the graph. " +
					"Run 'crosscodex admin cleaning run --resume' first"))
				Expect(snap()).To(Equal(before))
			},
			Entry("preview", func() error { _, err := eng.Preview(ctx, engTenant, cleaning.Scope{}); return err }),
			Entry("apply", func() error { _, err := eng.Apply(ctx, engTenant, cleaning.Scope{}, "any"); return err }),
			Entry("decide", func() error {
				_, _, err := eng.Decide(ctx, engTenant, cleaning.DecideRequest{
					FindingID: conflictB1, Verdict: cleaning.VerdictRestore, Rationale: "r", ExpectState: "any", Reviewer: reviewer,
				})
				return err
			}),
		)
	})

	Describe("Resume", func() {
		It("finishes a projection that failed part-way without superseding an edge twice", func() {
			failing := &failNthSupersede{GraphDB: g, n: 2}
			g = failing
			eng = newEngine(signer)
			plan, err := eng.Preview(ctx, engTenant, cleaning.Scope{})
			Expect(err).NotTo(HaveOccurred())

			_, err = eng.Apply(ctx, engTenant, cleaning.Scope{}, plan.Token)
			Expect(err).To(MatchError(errInjected))
			Expect(err).To(MatchError(cleaning.ErrProjectionIncomplete))
			Expect(err.Error()).To(ContainSubstring("1 of 2 findings applied to the graph; " + resumeMsg + " to finish"))
			var pe *cleaning.ProjectionError
			Expect(errors.As(err, &pe)).To(BeTrue())
			Expect(pe.Applied).To(Equal(1))
			Expect(pe.Total).To(Equal(2))
			Expect(pe.DecisionID).To(BeEmpty())
			Expect(pe.Public()).To(Equal("the graph update stopped part-way: 1 of 2 findings applied to the graph; " + resumeMsg + " to finish"))
			_, err = eng.Preview(ctx, engTenant, cleaning.Scope{})
			Expect(err).To(MatchError(cleaning.ErrUnprojected))

			n, err := eng.Resume(ctx, engTenant)
			Expect(err).NotTo(HaveOccurred())
			Expect(n).To(Equal(1))
			Expect(failing.superseded).To(ConsistOf("e-j1-a2-b1", "e-j1-a2-b2"))
			for _, id := range failing.superseded {
				e, err := g.GetEdge(ctx, engTenant, id)
				Expect(err).NotTo(HaveOccurred())
				Expect(e.ValidTo).To(HaveValue(BeTemporally("==", engNow)))
			}
			again, err := eng.Preview(ctx, engTenant, cleaning.Scope{})
			Expect(err).NotTo(HaveOccurred())
			Expect(again.Findings).To(BeEmpty())
		})

		It("refuses an unprojected finding with an unknown status, naming it, and changes nothing", func() {
			row := storedOf(planFinding(cleaning.SignalConflictsWith, []cleaning.VerdictKey{key("j1", a2, b1)}, []cleaning.VerdictKey{key("j1", a2, b1)}), "archived")
			row.EdgeIDs = []string{"e-j1-a2-b1"}
			store = engineStore(row)
			eng = newEngine(signer)
			before := snap()

			_, err := eng.Resume(ctx, engTenant)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("0 of 1 findings applied to the graph; " + resumeMsg + " to finish"))
			Expect(err.Error()).To(ContainSubstring(`finding ` + row.ID + ` has unknown status "archived", so its graph change is unknown and nothing was applied for it`))
			Expect(snap()).To(Equal(before))
		})

		DescribeTable("marks a finding that changes no edge projected without touching the graph",
			func(status cleaning.Status) {
				row := storedOf(planFinding(cleaning.SignalConflictsWith, []cleaning.VerdictKey{key("j1", a2, b1)}, []cleaning.VerdictKey{key("j1", a2, b1)}), status)
				row.EdgeIDs = []string{"e-j1-a2-b1"}
				store = engineStore(row)
				eng = newEngine(signer)
				live := liveEdgeIDs(ctx, g)

				n, err := eng.Resume(ctx, engTenant)
				Expect(err).NotTo(HaveOccurred())
				Expect(n).To(Equal(1))
				got, _, _, err := eng.GetFinding(ctx, engTenant, row.ID)
				Expect(err).NotTo(HaveOccurred())
				Expect(got.GraphApplied).To(BeTrue())
				Expect(got.Status).To(Equal(status))
				Expect(got.EdgeIDs).To(Equal([]string{"e-j1-a2-b1"}))
				Expect(liveEdgeIDs(ctx, g)).To(Equal(live))
			},
			Entry("reported", cleaning.StatusReported),
			Entry("pending", cleaning.StatusPending),
			Entry("dismissed", cleaning.StatusDismissed),
			Entry("cleared", cleaning.StatusCleared),
		)

		It("projects nothing when every finding is applied", func() {
			applyFixture()
			n, err := eng.Resume(ctx, engTenant)
			Expect(err).NotTo(HaveOccurred())
			Expect(n).To(BeZero())
		})
	})

	Describe("Decide", func() {
		BeforeEach(func() { applyFixture() })

		It("keeps a policy-retired finding restored by a reviewer: a later pass neither re-raises nor retires it", func() {
			restored, _, err := eng.Decide(ctx, engTenant, cleaning.DecideRequest{
				FindingID: conflictB1, Verdict: cleaning.VerdictRestore, Rationale: "the conflict is real and intended",
				ExpectState: stateOf(conflictB1), Reviewer: reviewer,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(restored.Status).To(Equal(cleaning.StatusRestored))
			Expect(restored.EdgeIDs).To(HaveLen(1))
			Expect(restored.EdgeIDs).NotTo(ContainElement("e-j1-a2-b1"))

			plan, err := eng.Preview(ctx, engTenant, cleaning.Scope{})
			Expect(err).NotTo(HaveOccurred())
			Expect(plan.Findings).To(BeEmpty(), "conflicts_with is still retire, but the restored finding's evidence is unchanged")
			_, err = eng.Apply(ctx, engTenant, cleaning.Scope{}, plan.Token)
			Expect(err).NotTo(HaveOccurred())

			got, ds, _, err := eng.GetFinding(ctx, engTenant, conflictB1)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Status).To(Equal(cleaning.StatusRestored))
			Expect(ds).To(HaveLen(2), "the policy retire and the reviewer's restore only")
			Expect(liveEdgeIDs(ctx, g)).To(ContainElement(restored.EdgeIDs[0]))
		})

		It("retires the verdicts a reviewer names with a signed decision, and a restore makes them load again", func() {
			got, id, err := eng.Decide(ctx, engTenant, cleaning.DecideRequest{
				FindingID: fanoutID, Verdict: cleaning.VerdictRetire, Rationale: "b2 is the narrower control",
				ExpectState: stateOf(fanoutID), RetireVerdicts: []cleaning.VerdictKey{key("j1", a1, b2)}, Reviewer: reviewer,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Status).To(Equal(cleaning.StatusRetired))
			Expect(got.GraphApplied).To(BeTrue())
			Expect(got.Targets).To(Equal([]cleaning.VerdictKey{key("j1", a1, b2)}))
			Expect(got.EdgeIDs).To(Equal([]string{"e-j1-a1-b2", "s-j1-a1-b2"}))
			Expect(liveEdgeIDs(ctx, g)).To(Equal([]string{"e-j1-a1-b1"}))
			Expect(slices.ContainsFunc(loadVerdicts(ctx, g, results.StageAnalyzer), verdictOn("a1", "b2"))).To(BeFalse())

			_, ds, _, err := eng.GetFinding(ctx, engTenant, fanoutID)
			Expect(err).NotTo(HaveOccurred())
			Expect(ds).To(HaveLen(1))
			Expect(ds[0].ID).To(Equal(id))
			Expect(ds[0].DecidedBy).To(Equal(reviewer))
			Expect(ds[0].RunID).To(BeEmpty())
			sum := sha256.Sum256(ds[0].Request)
			Expect(id).To(Equal(hex.EncodeToString(sum[:])))
			link, err := signer.Verify(ctx, ds[0].Attestation)
			Expect(err).NotTo(HaveOccurred())
			Expect(link.Step).To(Equal("cleaning.decide"))

			restored, _, err := eng.Decide(ctx, engTenant, cleaning.DecideRequest{
				FindingID: fanoutID, Verdict: cleaning.VerdictRestore, Rationale: "both are equivalent after all",
				ExpectState: stateOf(fanoutID), Reviewer: reviewer,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(restored.Status).To(Equal(cleaning.StatusRestored))
			Expect(restored.GraphApplied).To(BeTrue())
			Expect(restored.EdgeIDs).To(HaveLen(2))
			Expect(restored.EdgeIDs).NotTo(ContainElements("e-j1-a1-b2", "s-j1-a1-b2"))

			for stage, retired := range map[string]string{results.StageAnalyzer: "e-j1-a1-b2", results.StageSynthesized: "s-j1-a1-b2"} {
				vs := loadVerdicts(ctx, g, stage)
				i := slices.IndexFunc(vs, verdictOn("a1", "b2"))
				Expect(i).NotTo(Equal(-1), "the %s verdict loads again", stage)
				Expect(vs[i].Edge.Supersedes).To(Equal(retired))
				Expect(restored.EdgeIDs).To(ContainElement(vs[i].Edge.ID))
			}
		})

		It("dismisses a pending finding and updates the pending gauge", func() {
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { Expect(tp.Shutdown(context.Background())).To(Succeed()) })
			eng = newEngine(signer, cleaning.WithEngineTelemetry(tp.TracerProvider(), tp.MeterProvider()))

			got, _, err := eng.Decide(ctx, engTenant, cleaning.DecideRequest{
				FindingID: fanoutID, Verdict: cleaning.VerdictDismiss, Rationale: "the catalogs overlap by design",
				ExpectState: stateOf(fanoutID), Reviewer: reviewer,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Status).To(Equal(cleaning.StatusDismissed))
			Expect(got.GraphApplied).To(BeTrue())
			Expect(liveEdgeIDs(ctx, g)).To(Equal([]string{"e-j1-a1-b1", "e-j1-a1-b2", "s-j1-a1-b2"}))

			span := telemetrytest.FindSpan(tp.GetSpans(), "cleaning.Decide")
			Expect(span).NotTo(BeNil())
			Expect(spanString(span, "cleaning.operation")).To(Equal("decide"))
			pending := telemetrytest.FindMetric(tp.GetMetrics(), "cleaning.findings.pending")
			Expect(pending).NotTo(BeNil())
			Expect(telemetrytest.GaugeValue(pending)).To(BeZero())
		})

		It("reports a decision whose graph update failed as recorded, with the count and the resume instruction", func() {
			g = &failNthSupersede{GraphDB: g, n: 1}
			eng = newEngine(signer)
			live := liveEdgeIDs(ctx, g)

			_, _, err := eng.Decide(ctx, engTenant, cleaning.DecideRequest{
				FindingID: fanoutID, Verdict: cleaning.VerdictRetire, Rationale: "b2 is the narrower control",
				ExpectState: stateOf(fanoutID), RetireVerdicts: []cleaning.VerdictKey{key("j1", a1, b2)}, Reviewer: reviewer,
			})
			Expect(err).To(MatchError(cleaning.ErrProjectionIncomplete))
			Expect(err).To(MatchError(errInjected))
			got, ds, _, gerr := eng.GetFinding(ctx, engTenant, fanoutID)
			Expect(gerr).NotTo(HaveOccurred())
			Expect(got.Status).To(Equal(cleaning.StatusRetired))
			Expect(got.GraphApplied).To(BeFalse())
			Expect(ds).To(HaveExactElements(HaveField("Verdict", cleaning.VerdictRetire)))
			Expect(liveEdgeIDs(ctx, g)).To(Equal(live))
			var pe *cleaning.ProjectionError
			Expect(errors.As(err, &pe)).To(BeTrue())
			Expect(pe.Public()).To(Equal("decision " + ds[0].ID + " was recorded and stands, but the graph update stopped part-way: " +
				"0 of 1 findings applied to the graph; " + resumeMsg + " to finish"))
			Expect(pe.Public()).NotTo(ContainSubstring(errInjected.Error()))
		})

		DescribeTable("refuses the decision and leaves findings and graph unchanged",
			func(arrange func() (*cleaning.Engine, cleaning.DecideRequest), want error, msg string) {
				e, r := arrange()
				before := snap()
				_, _, err := e.Decide(ctx, engTenant, r)
				Expect(err).To(MatchError(want))
				Expect(err.Error()).To(ContainSubstring(msg))
				Expect(snap()).To(Equal(before))
			},
			Entry("no signer", func() (*cleaning.Engine, cleaning.DecideRequest) {
				return newEngine(nil), cleaning.DecideRequest{
					FindingID: fanoutID, Verdict: cleaning.VerdictDismiss, Rationale: "r", ExpectState: stateOf(fanoutID), Reviewer: reviewer,
				}
			}, cleaning.ErrSigningDisabled, "set attestation.private_key_path and attestation.public_key_path"),
			Entry("a transition the status does not allow", func() (*cleaning.Engine, cleaning.DecideRequest) {
				return eng, cleaning.DecideRequest{
					FindingID: fanoutID, Verdict: cleaning.VerdictRestore, Rationale: "r", ExpectState: stateOf(fanoutID), Reviewer: reviewer,
				}
			}, cleaning.ErrInvalidTransition, `is "pending"; restore is allowed only from retired`),
			Entry("a decision on a cleared finding", func() (*cleaning.Engine, cleaning.DecideRequest) {
				row := storedOf(planFinding(cleaning.SignalLowAgreement, []cleaning.VerdictKey{key("j9", a1, b1)}, []cleaning.VerdictKey{key("j9", a1, b1)}), cleaning.StatusCleared)
				row.GraphApplied = true
				store = engineStore(row)
				e := newEngine(signer)
				return e, cleaning.DecideRequest{
					FindingID: row.ID, Verdict: cleaning.VerdictRetire, Rationale: "r", ExpectState: cleaning.StateToken(row), Reviewer: reviewer,
				}
			}, cleaning.ErrInvalidTransition, `is "cleared"; retire is allowed only from reported, pending, dismissed or restored`),
			Entry("a retire verdict outside the finding", func() (*cleaning.Engine, cleaning.DecideRequest) {
				return eng, cleaning.DecideRequest{
					FindingID: fanoutID, Verdict: cleaning.VerdictRetire, Rationale: "r", ExpectState: stateOf(fanoutID), Reviewer: reviewer,
					RetireVerdicts: []cleaning.VerdictKey{key("j1", a1, b2), key("j1", a2, b1)},
				}
			}, cleaning.ErrVerdictNotInFinding, "j1:a2:b1"),
			Entry("a fanout retire that names no verdicts", func() (*cleaning.Engine, cleaning.DecideRequest) {
				return eng, cleaning.DecideRequest{
					FindingID: fanoutID, Verdict: cleaning.VerdictRetire, Rationale: "r", ExpectState: stateOf(fanoutID), Reviewer: reviewer,
				}
			}, cleaning.ErrRetireNeedsVerdicts, "--retire-verdict"),
			Entry("a stale state token", func() (*cleaning.Engine, cleaning.DecideRequest) {
				return eng, cleaning.DecideRequest{
					FindingID: fanoutID, Verdict: cleaning.VerdictDismiss, Rationale: "r", ExpectState: "token-of-an-older-read", Reviewer: reviewer,
				}
			}, cleaning.ErrStateChanged, "Reload it with 'crosscodex admin cleaning show"),
			Entry("an unknown verdict", func() (*cleaning.Engine, cleaning.DecideRequest) {
				return eng, cleaning.DecideRequest{
					FindingID: fanoutID, Verdict: "approve", Rationale: "r", ExpectState: stateOf(fanoutID), Reviewer: reviewer,
				}
			}, cleaning.ErrInvalidDecision, `verdict "approve" is not retire, dismiss or restore; pass one of them as --verdict`),
			Entry("an empty rationale", func() (*cleaning.Engine, cleaning.DecideRequest) {
				return eng, cleaning.DecideRequest{
					FindingID: fanoutID, Verdict: cleaning.VerdictDismiss, ExpectState: stateOf(fanoutID), Reviewer: reviewer,
				}
			}, cleaning.ErrInvalidDecision, "the rationale is empty, and every decision records why it was made. "+
				"Pass --rationale with the reason for this decision; nothing was written"),
			Entry("a whitespace-only rationale on a retire", func() (*cleaning.Engine, cleaning.DecideRequest) {
				return eng, cleaning.DecideRequest{
					FindingID: fanoutID, Verdict: cleaning.VerdictRetire, Rationale: " \t\n", ExpectState: stateOf(fanoutID), Reviewer: reviewer,
					RetireVerdicts: []cleaning.VerdictKey{key("j1", a1, b2)},
				}
			}, cleaning.ErrInvalidDecision, "Pass --rationale with the reason for this decision"),
			Entry("retire verdicts on a dismiss", func() (*cleaning.Engine, cleaning.DecideRequest) {
				return eng, cleaning.DecideRequest{
					FindingID: fanoutID, Verdict: cleaning.VerdictDismiss, Rationale: "r", ExpectState: stateOf(fanoutID), Reviewer: reviewer,
					RetireVerdicts: []cleaning.VerdictKey{key("j1", a1, b2)},
				}
			}, cleaning.ErrInvalidDecision, "--retire-verdict names verdicts to retire, so it applies only to --verdict retire, not dismiss"),
			Entry("retire verdicts on a restore", func() (*cleaning.Engine, cleaning.DecideRequest) {
				return eng, cleaning.DecideRequest{
					FindingID: conflictB1, Verdict: cleaning.VerdictRestore, Rationale: "r", ExpectState: stateOf(conflictB1), Reviewer: reviewer,
					RetireVerdicts: []cleaning.VerdictKey{key("j1", a2, b1)},
				}
			}, cleaning.ErrInvalidDecision, "applies only to --verdict retire, not restore"),
			Entry("a retire verdict superseded between reading the finding and deciding", func() (*cleaning.Engine, cleaning.DecideRequest) {
				r := cleaning.DecideRequest{
					FindingID: fanoutID, Verdict: cleaning.VerdictRetire, Rationale: "r", ExpectState: stateOf(fanoutID), Reviewer: reviewer,
					RetireVerdicts: []cleaning.VerdictKey{key("j1", a1, b2)},
				}
				_, err := g.SupersedeFact(ctx, engTenant, graphdb.SupersedeRequest{EdgeID: "e-j1-a1-b2", SupersededAt: engNow})
				Expect(err).NotTo(HaveOccurred())
				_, err = g.SupersedeFact(ctx, engTenant, graphdb.SupersedeRequest{EdgeID: "s-j1-a1-b2", SupersededAt: engNow})
				Expect(err).NotTo(HaveOccurred())
				return eng, r
			}, cleaning.ErrEdgeNotLive, "Reload the finding and decide again; nothing was written"),
			Entry("a retire edge found superseded by the pre-write check", func() (*cleaning.Engine, cleaning.DecideRequest) {
				r := cleaning.DecideRequest{
					FindingID: fanoutID, Verdict: cleaning.VerdictRetire, Rationale: "r", ExpectState: stateOf(fanoutID), Reviewer: reviewer,
					RetireVerdicts: []cleaning.VerdictKey{key("j1", a1, b2)},
				}
				g = supersededOnGet{GraphDB: g, edgeID: "s-j1-a1-b2"}
				return newEngine(signer), r
			}, cleaning.ErrEdgeNotLive, "Reload the finding and decide again; nothing was written"),
		)
	})
})
