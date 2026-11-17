package cleaning

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/crosswalk"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
)

// Sentinels the engine returns. Those with a fixed message carry it whole;
// the others are wrapped with what failed and what to do.
var (
	// ErrTokenMismatch marks an apply whose recomputed plan is not the one
	// previewed, or whose findings changed while it was written.
	ErrTokenMismatch = errors.New("the graph, findings or cleaning policy changed since the preview, " +
		"so this apply was refused and nothing was written. Preview again and apply the new token")
	// ErrUnprojected marks a request refused because recorded findings are
	// not yet applied to the graph.
	ErrUnprojected = errors.New("recorded cleaning changes are not yet applied to the graph")
	// ErrSigningDisabled marks a write refused because no attestation signer
	// is configured.
	ErrSigningDisabled = errors.New("cleaning writes are signed; set attestation.private_key_path and attestation.public_key_path")
	// ErrInvalidTransition marks a decision the finding's status does not
	// allow.
	ErrInvalidTransition = errors.New("decision not allowed from the finding's status")
	// ErrInvalidDecision marks a malformed decision request: an unknown
	// verdict, or retire verdicts on a dismiss or restore.
	ErrInvalidDecision = errors.New("invalid decision")
	// ErrVerdictNotInFinding marks a retire verdict the finding does not
	// concern.
	ErrVerdictNotInFinding = errors.New("verdict is not in the finding")
	// ErrRetireNeedsVerdicts marks a retire of a finding without targets that
	// names no verdicts.
	ErrRetireNeedsVerdicts = errors.New("this finding has no verdicts a retire would remove, so the decision must name them: " +
		"pass --retire-verdict <job>:<source>:<target> for each verdict to retire")
	// ErrInvalidScope marks a scope with exactly one catalog set.
	ErrInvalidScope = errors.New("invalid cleaning scope")
	// ErrTenantMismatch marks a request whose tenant is not the one its
	// context authenticated. The message names neither tenant.
	ErrTenantMismatch = errors.New("the request's tenant is not the authenticated tenant, so it was refused and nothing was read or written. " +
		"Send the request for your own tenant")
	// ErrProjectionIncomplete marks a graph update that stopped part-way
	// after its changes were recorded. The recorded changes stand; Resume
	// applies the rest. It is always returned inside a *ProjectionError.
	ErrProjectionIncomplete = errors.New("the graph update stopped part-way")
)

// resumeHint is the instruction every refusal over unprojected findings
// gives.
const resumeHint = "run 'crosscodex admin cleaning run --resume'"

// CurrentFunc returns the record a stage would write if it ran now; ok is
// false when the stage is not known now, which makes it unknown, never
// stale.
type CurrentFunc func(ctx context.Context, tenantID, stage string) (StageRecord, bool, error)

// EngineOption configures an Engine.
type EngineOption func(*Engine)

// WithClock sets the engine's clock, which stamps decisions and therefore
// retire and restore times. Tests use it; the default is time.Now.
func WithClock(now func() time.Time) EngineOption {
	return func(e *Engine) { e.now = now }
}

// Engine runs cleaning passes and records decisions for one deployment. It
// is safe for concurrent use: the store refuses a write planned from a state
// that has since changed, and projection is repeatable, because each graph
// change is timed by its decision and restored edge IDs are derived from it.
type Engine struct {
	store   FindingStore
	g       graphdb.GraphDB
	cfg     *config.CleaningConfig
	current CurrentFunc
	signer  attestation.Generator
	now     func() time.Time
	tel     engineTelemetry
}

// NewEngine returns an engine over store and g with the policy cfg. current
// resolves the stages' records for stale_prompt and stale_model; nil leaves
// every stage unknown. A nil signer refuses Apply and Decide with
// ErrSigningDisabled; Preview, Resume and the reads still work.
func NewEngine(store FindingStore, g graphdb.GraphDB, cfg *config.CleaningConfig, current CurrentFunc, signer attestation.Generator, opts ...EngineOption) *Engine {
	e := &Engine{store: store, g: g, cfg: cfg, current: current, signer: signer, now: time.Now}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// stamp is the engine's current time as the store keeps it: UTC at
// microsecond precision, so a time read back equals the one signed.
func (e *Engine) stamp() time.Time {
	return e.now().Round(time.Microsecond).UTC()
}

// Preview returns the plan a run over scope would apply, writing nothing.
func (e *Engine) Preview(ctx context.Context, tenantID string, scope Scope) (_ Plan, err error) {
	ctx, finish, err := e.begin(ctx, "Preview", "preview", tenantID)
	if err != nil {
		return Plan{}, err
	}
	defer func() { finish(err) }()
	slog.InfoContext(ctx, "cleaning preview started", "tenant.id", tenantID,
		"source_catalog", scope.SourceCatalog, "target_catalog", scope.TargetCatalog)
	plan, _, err := e.plan(ctx, tenantID, scope)
	if err != nil {
		return Plan{}, err
	}
	logPlan(ctx, "cleaning preview finished", tenantID, plan)
	return plan, nil
}

// Apply recomputes the plan for scope and, when its token is expectToken,
// records it and applies its retirements to the graph. Every check (token,
// signer, retire cap, live target edges) passes before anything is written.
// Retired findings are recorded unprojected with one policy decision each,
// all sharing one signed cleaning.run link; then the graph is updated. A
// graph failure returns a *ProjectionError and leaves the rest for Resume.
func (e *Engine) Apply(ctx context.Context, tenantID string, scope Scope, expectToken string) (_ Plan, err error) {
	ctx, finish, err := e.begin(ctx, "Apply", "apply", tenantID)
	if err != nil {
		return Plan{}, err
	}
	defer func() { finish(err) }()
	slog.InfoContext(ctx, "cleaning apply started", "tenant.id", tenantID,
		"source_catalog", scope.SourceCatalog, "target_catalog", scope.TargetCatalog)

	plan, stored, err := e.plan(ctx, tenantID, scope)
	if err != nil {
		return Plan{}, err
	}
	if plan.Token != expectToken {
		return Plan{}, fmt.Errorf("apply cleaning run: %w", ErrTokenMismatch)
	}
	if e.signer == nil {
		return Plan{}, fmt.Errorf("apply cleaning run: %w", ErrSigningDisabled)
	}
	if err := CheckLive(ctx, e.g, tenantID, plan.RetireEdgeIDs); err != nil {
		return Plan{}, fmt.Errorf("apply cleaning run: a verdict to retire is no longer live, so nothing was written. Preview again and apply the new token: %w", err)
	}
	if len(plan.Findings) == 0 {
		logPlan(ctx, "cleaning apply finished", tenantID, plan)
		return plan, nil
	}

	w, err := e.runWrite(ctx, tenantID, plan, stored)
	if err != nil {
		return Plan{}, err
	}
	if err := e.store.ApplyRun(ctx, tenantID, w); err != nil {
		if errors.Is(err, ErrStateChanged) {
			return Plan{}, fmt.Errorf("apply cleaning run: %w: %w", ErrTokenMismatch, err)
		}
		return Plan{}, fmt.Errorf("apply cleaning run: %w", err)
	}
	if e.tel.findings != nil {
		for _, p := range plan.Findings {
			e.tel.findings.Add(ctx, 1, metric.WithAttributes(
				attribute.String("signal", string(p.Signal)), attribute.String("action", string(p.Action))))
		}
	}
	e.recordPending(ctx, tenantID)

	if _, err := e.project(ctx, tenantID); err != nil {
		return Plan{}, err
	}
	logPlan(ctx, "cleaning apply finished", tenantID, plan)
	return plan, nil
}

// Resume applies to the graph every recorded finding not yet applied, and
// returns how many it applied. It needs no token: it only applies decisions
// already recorded and attested.
func (e *Engine) Resume(ctx context.Context, tenantID string) (_ int, err error) {
	ctx, finish, err := e.begin(ctx, "Resume", "resume", tenantID)
	if err != nil {
		return 0, err
	}
	defer func() { finish(err) }()
	slog.InfoContext(ctx, "cleaning resume started", "tenant.id", tenantID)
	n, err := e.project(ctx, tenantID)
	if err != nil {
		return n, err
	}
	slog.InfoContext(ctx, "cleaning resume finished", "tenant.id", tenantID, "projected", n)
	return n, nil
}

// ListFindings returns one page of the tenant's findings; see
// FindingStore.ListFindings.
func (e *Engine) ListFindings(ctx context.Context, tenantID string, f FindingFilter) (_ []StoredFinding, _ string, err error) {
	ctx, finish, err := e.begin(ctx, "ListFindings", "list_findings", tenantID)
	if err != nil {
		return nil, "", err
	}
	defer func() { finish(err) }()
	return e.store.ListFindings(ctx, tenantID, f)
}

// GetFinding returns a finding, its decisions oldest first, and the state
// token a decision on it must quote.
func (e *Engine) GetFinding(ctx context.Context, tenantID, findingID string) (_ StoredFinding, _ []Decision, _ string, err error) {
	ctx, finish, err := e.begin(ctx, "GetFinding", "get_finding", tenantID)
	if err != nil {
		return StoredFinding{}, nil, "", err
	}
	defer func() { finish(err) }()
	f, ds, err := e.store.GetFinding(ctx, tenantID, findingID)
	if err != nil {
		return StoredFinding{}, nil, "", err
	}
	return f, ds, StateToken(f), nil
}

func logPlan(ctx context.Context, msg, tenantID string, p Plan) {
	slog.InfoContext(ctx, msg, "tenant.id", tenantID, "findings", len(p.Findings), "retire_verdicts", len(p.Retire),
		"retire_edges", len(p.RetireEdgeIDs), "verdicts", p.Summary.Verdicts, "pairs", p.Summary.Pairs,
		"unknown_prompt", p.Summary.UnknownPrompt, "unknown_model", p.Summary.UnknownModel,
		"unaddressable_verdicts", p.Summary.UnaddressableVerdicts, "skipped_edges", p.Summary.SkippedEdges)
}

// refuseUnprojected fails while any finding's graph change is outstanding:
// it would change the graph under a plan or a decision.
func (e *Engine) refuseUnprojected(ctx context.Context, tenantID string) error {
	uns, err := e.store.Unprojected(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("list unprojected findings: %w", err)
	}
	if len(uns) > 0 {
		return fmt.Errorf("%w: %d findings from an earlier run or decision are recorded but not yet applied to the graph. "+
			"Run 'crosscodex admin cleaning run --resume' first", ErrUnprojected, len(uns))
	}
	return nil
}

// plan detects findings over scope and plans them against the stored ones,
// which it also returns by ID.
func (e *Engine) plan(ctx context.Context, tenantID string, scope Scope) (Plan, map[string]StoredFinding, error) {
	if (scope.SourceCatalog == "") != (scope.TargetCatalog == "") {
		return Plan{}, nil, fmt.Errorf("%w: source catalog %q, target catalog %q. Set both --source-catalog and --target-catalog "+
			"to clean one catalog pair, or neither to clean every mapped pair", ErrInvalidScope, scope.SourceCatalog, scope.TargetCatalog)
	}
	if err := e.refuseUnprojected(ctx, tenantID); err != nil {
		return Plan{}, nil, err
	}
	jobs, err := e.store.CompletedJobs(ctx, tenantID)
	if err != nil {
		return Plan{}, nil, fmt.Errorf("load completed jobs: %w", err)
	}
	byJob := make(map[string]Job, len(jobs))
	for _, j := range jobs {
		byJob[j.ID] = j
	}

	var pairs []Pair
	var skipped int64
	for _, cp := range catalogPairs(scope, jobs) {
		ps, n, err := e.loadPairs(ctx, tenantID, byJob, cp[0], cp[1])
		if err != nil {
			return Plan{}, nil, err
		}
		pairs = append(pairs, ps...)
		skipped += n
	}
	current, err := e.currentRecords(ctx, tenantID, jobs)
	if err != nil {
		return Plan{}, nil, err
	}
	pol := e.cfg.ForTenant(tenantID)
	found, sum := Detect(pairs, byJob, current, pol)
	sum.SkippedEdges = skipped

	stored, err := e.store.Findings(ctx, tenantID, scope)
	if err != nil {
		return Plan{}, nil, fmt.Errorf("load stored findings: %w", err)
	}
	byID := make(map[string]StoredFinding, len(stored))
	for _, s := range stored {
		if !slices.Contains(statuses, s.Status) {
			return Plan{}, nil, fmt.Errorf("stored finding %s has unknown status %q, so the run was refused and nothing was written. "+
				"Its edge_findings row was changed outside crosscodex; set its status back to one of %s", s.ID, s.Status, joinOr(statuses))
		}
		byID[s.ID] = s
	}
	plan, err := BuildPlan(found, stored, pol, scope, sum)
	if err != nil {
		return Plan{}, nil, fmt.Errorf("plan cleaning run: %w", err)
	}
	return plan, byID, nil
}

// catalogPairs returns scope's catalog pair, or, unscoped, every distinct
// unordered pair the jobs mapped in job order. LoadPair reads both
// directions, so a reversed or bidirectional job adds no second walk.
func catalogPairs(scope Scope, jobs []Job) [][2]string {
	if scope.SourceCatalog != "" {
		return [][2]string{{scope.SourceCatalog, scope.TargetCatalog}}
	}
	var out [][2]string
	for _, j := range jobs {
		p := [2]string{j.Catalogs.SourceID, j.Catalogs.TargetID}
		if p[1] < p[0] {
			p[0], p[1] = p[1], p[0]
		}
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// loadPairs reads every verdict of the jobs between catalogs a and b in both
// stages, non-viable and no-relationship edges included because a verdict
// owns them, and returns them joined with the count of edges LoadPair
// skipped.
func (e *Engine) loadPairs(ctx context.Context, tenantID string, jobs map[string]Job, a, b string) ([]Pair, int64, error) {
	completed := make(map[string]time.Time, len(jobs))
	for id, j := range jobs {
		completed[id] = j.CompletedAt
	}
	var stages [2][]crosswalk.PairEdges
	var skipped int64
	for i, stage := range []string{results.StageAnalyzer, results.StageSynthesized} {
		pe, n, err := crosswalk.LoadPair(ctx, e.g, tenantID, crosswalk.PairQuery{
			A: a, B: b, Jobs: completed, Stage: stage, IncludeNonViable: true, IncludeNoRelationship: true,
		})
		if err != nil {
			return nil, 0, fmt.Errorf("load %s verdicts between catalogs %s and %s: %w", stage, a, b, err)
		}
		stages[i] = pe
		skipped += n
	}
	return BuildPairs(stages[0], stages[1]), skipped, nil
}

// currentRecords calls current once per distinct stage name the jobs
// recorded, leaving out stages it does not know.
func (e *Engine) currentRecords(ctx context.Context, tenantID string, jobs []Job) (Current, error) {
	cur := Current{}
	if e.current == nil {
		return cur, nil
	}
	names := map[string]bool{}
	for _, j := range jobs {
		for stage := range j.Stages {
			names[stage] = true
		}
	}
	for _, stage := range slices.Sorted(maps.Keys(names)) {
		rec, ok, err := e.current(ctx, tenantID, stage)
		if err != nil {
			return nil, fmt.Errorf("resolve the current prompt and models of stage %s: %w", stage, err)
		}
		if ok {
			cur[stage] = rec
		}
	}
	return cur, nil
}

// policyRequest is the canonical request of a policy decision: what the run
// decided about one finding. Its SHA-256 is the decision ID.
type policyRequest struct {
	Tenant       string   `json:"tenant"`
	FindingID    string   `json:"finding_id"`
	Verdict      string   `json:"verdict"`
	RunID        string   `json:"run_id"`
	EvidenceHash string   `json:"evidence_hash"`
	EdgeIDs      []string `json:"edge_ids"`
	DecidedBy    string   `json:"decided_by"`
	DecidedAt    string   `json:"decided_at"`
}

// runWrite builds what an apply records: every planned finding against the
// state it was planned from, and for each retired one a policy decision. One
// cleaning.run link attests the run: its material is the plan (digest: the
// token, the SHA-256 of the canonical plan) and each finding is a product
// with its evidence hash.
func (e *Engine) runWrite(ctx context.Context, tenantID string, plan Plan, stored map[string]StoredFinding) (RunWrite, error) {
	products := make([]attestation.Artifact, len(plan.Findings))
	for i, p := range plan.Findings {
		products[i] = attestation.Artifact{URI: "crosscodex:finding/" + p.ID, Digest: p.EvidenceHash}
	}
	link, err := e.signer.CreateLink(ctx, "cleaning.run",
		[]attestation.Artifact{{URI: "crosscodex:cleaning-plan", Digest: plan.Token}}, products)
	if err != nil {
		return RunWrite{}, fmt.Errorf("sign cleaning run: %w", err)
	}

	decidedAt := e.stamp()
	var w RunWrite
	for _, p := range plan.Findings {
		expect := ""
		if s, ok := stored[p.ID]; ok {
			expect = StateToken(s)
		}
		w.Upserts = append(w.Upserts, RunUpsert{ExpectState: expect, Finding: StoredFinding{
			ID: p.ID, Signal: p.Signal, Evidence: p.Evidence, EvidenceHash: p.EvidenceHash,
			Concerns: p.Concerns, Targets: p.Targets, EdgeIDs: p.EdgeIDs, Status: p.To,
			GraphApplied: p.To != StatusRetired, SourceCatalog: p.SourceCatalog, TargetCatalog: p.TargetCatalog,
		}})
		if p.To != StatusRetired {
			continue
		}
		edges := p.EdgeIDs
		if edges == nil {
			edges = []string{}
		}
		req, err := json.Marshal(policyRequest{
			Tenant: tenantID, FindingID: p.ID, Verdict: VerdictRetire, RunID: plan.Token, EvidenceHash: p.EvidenceHash,
			EdgeIDs: edges, DecidedBy: DecidedByPolicy, DecidedAt: decidedAt.Format(time.RFC3339Nano),
		})
		if err != nil {
			return RunWrite{}, fmt.Errorf("encode the decision on finding %s: %w", p.ID, err)
		}
		sum := sha256.Sum256(req)
		w.Decisions = append(w.Decisions, Decision{
			ID: hex.EncodeToString(sum[:]), FindingID: p.ID, Verdict: VerdictRetire, DecidedBy: DecidedByPolicy, RunID: plan.Token,
			Rationale: fmt.Sprintf("cleaning.signals.%s.action is retire", p.Signal),
			Request:   req, Attestation: link.Raw, DecidedAt: decidedAt,
		})
	}
	return w, nil
}

// joinOr renders values as "a, b or c".
func joinOr[T ~string](values []T) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = string(v)
	}
	if len(parts) < 2 {
		return strings.Join(parts, "")
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " or " + parts[len(parts)-1]
}
