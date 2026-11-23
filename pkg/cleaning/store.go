package cleaning

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

const (
	// maxStoredJSONBytes bounds the evidence and concerns columns a store
	// writes. PostgreSQL renders jsonb with a space after every ':' and ','
	// and never escapes more than encoding/json does, so a value Go encodes
	// within half of maxEvidenceBytes reads back within the bound
	// ParseEvidence and decodeKeys enforce.
	maxStoredJSONBytes = maxEvidenceBytes / 2
	// maxListBytes bounds the targets and edge_ids columns on read. They list
	// a retired job-level finding's verdicts and edges, which grow with the
	// job, so their bound is PostgreSQL's own: a jsonb value holds at most
	// 2^28-1 bytes. No row the database accepts is refused on read.
	maxListBytes = 1 << 28
	// maxStoredListBytes bounds the targets and edge_ids columns a store
	// writes, half of maxListBytes for the same rendering reason.
	maxStoredListBytes = maxListBytes / 2
)

// storedListLimit is the targets and edge_ids write bound,
// maxStoredListBytes. It is a variable only so that specs can lower it
// (export_test.go) instead of building a 128 MiB value; nothing else sets it.
var storedListLimit = maxStoredListBytes

// Sentinels a FindingStore returns, wrapped with an actionable message.
var (
	// ErrFindingTooLarge marks a finding whose stored JSON would exceed the
	// bound within which its row reads back. BuildPlan returns it at preview,
	// before any write.
	ErrFindingTooLarge = errors.New("finding too large to store")
	// ErrFindingNotFound marks a finding ID the caller's tenant does not
	// have. Another tenant's finding is reported the same way.
	ErrFindingNotFound = errors.New("finding not found")
	// ErrStateChanged marks a decision quoting a state token the finding no
	// longer has: it changed since the decider read it.
	ErrStateChanged = errors.New("finding changed since it was read")
	// ErrReviewerNotRegistered marks a decider with no reviewers row in the
	// tenant.
	ErrReviewerNotRegistered = errors.New("reviewer not registered")
	// ErrReviewerDeleted marks a decider whose reviewers row is deleted.
	ErrReviewerDeleted = errors.New("reviewer deleted")
)

// Decision verdicts, as edge_finding_decisions.verdict stores them.
const (
	VerdictRetire  = "retire"
	VerdictDismiss = "dismiss"
	VerdictRestore = "restore"
	// VerdictStale is recorded only by re-projection (Engine.Reproject),
	// always with DecidedByReproject, when a finding's verdicts are gone
	// from a rebuilt graph. No reviewer can decide it.
	VerdictStale = "stale"
)

const (
	// DecidedByPolicy is Decision.DecidedBy for a decision a cleaning run's
	// policy made; a reviewer's decision holds the reviewer ID instead.
	DecidedByPolicy = "policy"
	// DecidedByReproject is Decision.DecidedBy for re-projection's
	// VerdictStale decision, and for no other.
	DecidedByReproject = "reproject"
)

// Decision is one append-only edge_finding_decisions row.
type Decision struct {
	// ID is the hex SHA-256 of Request, the canonical request.
	ID        string
	FindingID string
	Verdict   string // VerdictRetire, VerdictDismiss, VerdictRestore or VerdictStale
	// DecidedBy is DecidedByPolicy, DecidedByReproject or a reviewer ID. RunID is the plan token
	// of the run that made a policy decision, empty for a reviewer's.
	DecidedBy, RunID, Rationale string
	// Request and Attestation are stored and returned byte for byte.
	Request, Attestation []byte
	// DecidedAt is stored at microsecond precision; zero means the time of
	// the write.
	DecidedAt time.Time
}

// FindingFilter selects findings for ListFindings. Empty fields match
// everything. SourceCatalog and TargetCatalog name one unordered catalog pair,
// as Scope does: set both or neither.
type FindingFilter struct {
	Signal                       Signal
	Status                       Status
	SourceCatalog, TargetCatalog string
	// PageSize is the most findings a page holds; it must be positive. The
	// caller bounds it.
	PageSize int
	// PageToken is the next-page token a previous call returned; empty
	// starts at the first finding.
	PageToken string
}

// RunUpsert is one finding a run writes and the state the run planned it
// from: ExpectState is the StateToken of the stored finding, or "" when the
// finding must not be stored yet.
type RunUpsert struct {
	Finding     StoredFinding
	ExpectState string
}

// RunRecord is one append-only edge_cleaning_runs row: the signed record of
// one applied cleaning run, kept whether or not the run made a decision, so
// every status a run set (reported, pending, cleared, retired) traces to the
// policy and plan that set it.
type RunRecord struct {
	// PlanToken is the run's Plan.Token, the hex SHA-256 of Plan.
	PlanToken string
	// Plan is the run's Plan.Canonical: the scope, the effective policy and
	// each finding's change. It and Attestation, the run's signed
	// cleaning.run in-toto link, are stored and returned byte for byte.
	Plan, Attestation []byte
	// AppliedAt is when the run was applied. Each policy decision of the
	// run has it as DecidedAt, and the decision's foreign key matches the
	// two as stored, so pass a time already at microsecond precision (as
	// Engine.stamp does): the driver truncates finer times.
	AppliedAt time.Time
}

// Validate reports whether a store may record r: a plan and an attestation,
// a plan token that is the plan's SHA-256, and an applied time.
func (r RunRecord) Validate() error {
	if len(r.Plan) == 0 || len(r.Attestation) == 0 {
		return fmt.Errorf("run %s: plan and attestation are required so the run can be verified later; nothing was written", r.PlanToken)
	}
	if sum := sha256.Sum256(r.Plan); hex.EncodeToString(sum[:]) != r.PlanToken {
		return fmt.Errorf("run %s: the plan token is not the SHA-256 of the stored plan, so the record could not be verified; "+
			"record Plan.Canonical with its Plan.Token; nothing was written", r.PlanToken)
	}
	if r.AppliedAt.IsZero() {
		return fmt.Errorf("run %s: an applied time is required; nothing was written", r.PlanToken)
	}
	return nil
}

// RunWrite is what an applied cleaning run records, in one transaction: Run
// first, then Upserts, then Decisions, which may refer to the upserted
// findings and are the run's policy decisions.
type RunWrite struct {
	Run       RunRecord
	Upserts   []RunUpsert
	Decisions []Decision
}

// Validate reports whether a store may record w: Run passes
// RunRecord.Validate, each upserted finding StoredFinding.Validate, and each
// decision Decision.Validate and is a policy decision of Run (its RunID is
// Run.PlanToken and its DecidedAt Run.AppliedAt). It does not check stored
// state.
func (w RunWrite) Validate() error {
	if err := w.Run.Validate(); err != nil {
		return err
	}
	for _, u := range w.Upserts {
		if err := u.Finding.Validate(); err != nil {
			return err
		}
	}
	for _, d := range w.Decisions {
		if d.DecidedBy != DecidedByPolicy {
			return fmt.Errorf("decision %s is decided by %q: ApplyRun records only policy decisions; record a reviewer's decision with Decide; nothing was written",
				d.ID, d.DecidedBy)
		}
		if err := d.Validate(); err != nil {
			return err
		}
		if d.RunID != w.Run.PlanToken || !d.DecidedAt.Equal(w.Run.AppliedAt) {
			return fmt.Errorf("decision %s names run %s at %s, not the run being applied (%s at %s): "+
				"a policy decision is recorded with the run that made it, stamped with its applied time; nothing was written",
				d.ID, d.RunID, d.DecidedAt.Format(time.RFC3339Nano), w.Run.PlanToken, w.Run.AppliedAt.Format(time.RFC3339Nano))
		}
	}
	return nil
}

// FindingStore is the authoritative, tenant-scoped record of findings and
// their decisions. Every method needs a tenant in ctx equal to tenantID, and
// is refused otherwise. Every write is all or nothing.
type FindingStore interface {
	// CompletedJobs returns the tenant's completed jobs sorted by ID, every
	// user's: the result is tenant-wide by contract, whatever user ctx
	// carries. Callers are gated (an admin runs passes, a reviewer decides),
	// and the verdicts those jobs wrote to the graph are tenant-visible
	// already. CompletedAt is the latest stage completion time, Catalogs
	// comes from the job's stored config and Stages from each stage's
	// stage_record (a nil entry when it is NULL). A config without catalogs,
	// a non-NULL stage_record that does not parse (an empty one included), or
	// a job without a stage completion time is an error naming the job.
	CompletedJobs(ctx context.Context, tenantID string) ([]Job, error)
	// Findings returns every finding, whatever its status, on a catalog pair
	// scope covers, sorted by ID.
	Findings(ctx context.Context, tenantID string, scope Scope) ([]StoredFinding, error)
	// ListFindings returns one page of findings matching f, sorted by ID,
	// and the token of the next page, empty after the last.
	ListFindings(ctx context.Context, tenantID string, f FindingFilter) ([]StoredFinding, string, error)
	// GetFinding returns a finding and its decisions, oldest first. A
	// finding the tenant does not have wraps ErrFindingNotFound.
	GetFinding(ctx context.Context, tenantID, findingID string) (StoredFinding, []Decision, error)
	// ApplyRun records w.Run, writes each upsert's Finding (UpdatedAt is set
	// by the store, any other field is written as given), then inserts
	// w.Decisions, all or nothing. w must pass RunWrite.Validate, and no run
	// with the same PlanToken and AppliedAt may be stored. Each upsert's
	// ExpectState must still hold, checked under a row lock: "" means the
	// finding is not stored, otherwise it is the stored finding's
	// StateToken; a run planned from another state wraps ErrStateChanged.
	// Each decision must refer to a stored finding and have an ID not
	// already stored.
	ApplyRun(ctx context.Context, tenantID string, w RunWrite) error
	// Decide records a reviewer's decision d, whose DecidedBy must be an
	// active reviewer of the tenant (else ErrReviewerNotRegistered or
	// ErrReviewerDeleted). The finding d.FindingID must still have the state
	// token expectState (else ErrStateChanged). The finding then takes
	// next's Status, Targets and EdgeIDs with GraphApplied false; next.ID
	// must equal d.FindingID and next must pass StoredFinding.Validate. d
	// must pass Decision.Validate and be a reviewer's: DecidedBy is not
	// DecidedByPolicy or DecidedByReproject and RunID is empty.
	Decide(ctx context.Context, tenantID string, d Decision, expectState string, next StoredFinding) error
	// MarkStale records re-projection's decision d and moves the finding
	// d.FindingID to StatusStale, keeping its Targets and EdgeIDs, with
	// GraphApplied true: the graph holds nothing left to change. d must pass
	// Decision.Validate and be VerdictStale by DecidedByReproject without a
	// RunID. The finding must still have the state token expectState (else
	// ErrStateChanged), checked under a row lock.
	MarkStale(ctx context.Context, tenantID string, d Decision, expectState string) error
	// MarkReprojected replaces a projected finding's EdgeIDs with edgeIDs
	// and changes nothing else, UpdatedAt included: the finding's state and
	// its latest change are unchanged, only the graph's edge IDs for it are.
	// The finding must still have the state token expectState (else
	// ErrStateChanged) and GraphApplied true (else ErrUnprojected), both
	// checked under a row lock, so a decision recorded since re-projection
	// read the finding is never overwritten.
	MarkReprojected(ctx context.Context, tenantID, findingID, expectState string, edgeIDs []string) error
	// Unprojected returns the findings whose graph changes are not applied,
	// sorted by ID.
	Unprojected(ctx context.Context, tenantID string) ([]StoredFinding, error)
	// MarkProjected records that a finding's graph changes are applied and
	// replaces its EdgeIDs with edgeIDs.
	MarkProjected(ctx context.Context, tenantID, findingID string, edgeIDs []string) error
	// PendingCount returns how many findings are in the review queue.
	PendingCount(ctx context.Context, tenantID string) (int64, error)
}

// checkPair refuses a catalog pair with exactly one side set: Scope's rule
// would match no finding, which is never what the caller meant.
func checkPair(source, target string) error {
	if (source == "") != (target == "") {
		return fmt.Errorf("catalog pair %q/%q has one side only: set both the source and the target catalog, or neither to cover every pair", source, target)
	}
	return nil
}

// statuses are the Status values a finding may have.
var statuses = []Status{StatusReported, StatusPending, StatusRetired, StatusDismissed, StatusRestored, StatusCleared, StatusStale}

// Statuses returns every Status a finding may have.
func Statuses() []Status { return slices.Clone(statuses) }

// Valid reports whether s is one of Statuses.
func (s Status) Valid() bool { return slices.Contains(statuses, s) }

// Validate reports whether a store may write f: a known signal and status,
// a subject that matches the signal (a job-level signal's evidence names a
// valid job ID and it concerns no verdict; any other signal's names no job),
// an ID that is FindingID of its signal and subject, verdict keys
// ParseVerdictKey would accept (concerns, targets and evidence), and columns
// that each encode within their stored-JSON bound (ErrFindingTooLarge). A
// store reads every row back strictly and rows are never deleted, so a row it
// could not read would block every read of the tenant's findings; refusing it
// at write time keeps reads working.
func (f StoredFinding) Validate() error {
	if !slices.Contains(signalOrder, f.Signal) {
		return fmt.Errorf("finding %s: unknown signal %q; use one of the cleaning.signals keys; nothing was written", f.ID, f.Signal)
	}
	if !slices.Contains(statuses, f.Status) {
		return fmt.Errorf("finding %s: unknown status %q; nothing was written", f.ID, f.Status)
	}
	switch job := f.Evidence.Job; {
	case f.Signal.JobLevel() && job == nil:
		return fmt.Errorf("finding %s: signal %s is raised per job, so its evidence must name the job; build it as Detect does; nothing was written", f.ID, f.Signal)
	case !f.Signal.JobLevel() && job != nil:
		return fmt.Errorf("finding %s: signal %s is raised per verdict, so its evidence must not name a job; nothing was written", f.ID, f.Signal)
	case job != nil && (len(f.Concerns) > 0 || len(f.Evidence.Verdicts) > 0):
		return fmt.Errorf("finding %s: a job-level finding concerns its job, not verdicts, so it has no concerns or evidence verdicts; nothing was written", f.ID)
	case job != nil:
		if err := validateKeyPart("job", job.ID); err != nil {
			return fmt.Errorf("finding %s: evidence job: %w; nothing was written", f.ID, err)
		}
	}
	if want := FindingID(Finding{Signal: f.Signal, Concerns: f.Concerns, Evidence: f.Evidence}); f.ID != want {
		return fmt.Errorf("finding %s: ID is not FindingID of its signal and subject (%s); build stored findings with FindingID; nothing was written", f.ID, want)
	}
	for _, set := range []struct {
		name string
		keys []VerdictKey
	}{{"concern", f.Concerns}, {"target", f.Targets}} {
		for i, k := range set.keys {
			if err := k.validate(); err != nil {
				return fmt.Errorf("finding %s: %s %d: %w; nothing was written", f.ID, set.name, i, err)
			}
		}
	}
	for i, v := range f.Evidence.Verdicts {
		if err := v.Key.validate(); err != nil {
			return fmt.Errorf("finding %s: evidence verdict %d: %w; nothing was written", f.ID, i, err)
		}
	}
	return checkStoredSize(f, "A cleaning run refuses this at preview and a decision before it is signed; "+
		"preview or decide again for what to do")
}

// checkStoredSize refuses a finding any of whose JSON columns would exceed
// its bound, wrapping ErrFindingTooLarge with the column, the size, the bound
// and what to do: listRemedy for targets and edge IDs, which grow with what
// is retired, so the caller says what its own path can change.
func checkStoredSize(f StoredFinding, listRemedy string) error {
	subject := string(f.Signal)
	if f.Evidence.Job != nil {
		subject += ", job " + f.Evidence.Job.ID
	}
	// Evidence and concerns are bounded by the signal, so only a defect
	// makes them this large.
	noSetting := "No setting shrinks it: clean other catalog pairs with --source-catalog/--target-catalog meanwhile, " +
		"and report this finding's signal and size as a defect"
	for _, col := range []struct {
		name   string
		value  any
		limit  int
		remedy string
	}{
		{"evidence", f.Evidence, maxStoredJSONBytes, noSetting},
		{"concerns", f.Concerns, maxStoredJSONBytes, noSetting},
		{"targets", f.Targets, storedListLimit, listRemedy},
		{"edge_ids", f.EdgeIDs, storedListLimit, listRemedy},
	} {
		b, err := json.Marshal(col.value)
		if err != nil {
			return fmt.Errorf("finding %s: encode %s: %w; nothing was written", f.ID, col.name, err)
		}
		if len(b) > col.limit {
			return fmt.Errorf("finding %s (%s): %w: the %s column would hold %d bytes of JSON, over the %d-byte limit within which a stored finding reads back, so nothing was written. %s",
				f.ID, subject, ErrFindingTooLarge, col.name, len(b), col.limit, col.remedy)
		}
	}
	return nil
}

// Validate reports whether a store may record d: a known verdict, a request
// and an attestation, and consistent provenance: a policy decision carries
// the run ID that made it and any other carries none, and VerdictStale is
// decided by DecidedByReproject, which decides nothing else.
func (d Decision) Validate() error {
	if !slices.Contains([]string{VerdictRetire, VerdictDismiss, VerdictRestore, VerdictStale}, d.Verdict) {
		return fmt.Errorf("decision %s: unknown verdict %q; use %q, %q, %q or %q; nothing was written",
			d.ID, d.Verdict, VerdictRetire, VerdictDismiss, VerdictRestore, VerdictStale)
	}
	if (d.Verdict == VerdictStale) != (d.DecidedBy == DecidedByReproject) {
		return fmt.Errorf("decision %s: verdict %q decided by %q: only re-projection records a %q decision, always as %q, and it records no other verdict; nothing was written",
			d.ID, d.Verdict, d.DecidedBy, VerdictStale, DecidedByReproject)
	}
	if d.Request == nil || d.Attestation == nil {
		return fmt.Errorf("decision %s: request and attestation are required so the decision can be verified later; nothing was written", d.ID)
	}
	if (d.DecidedBy == DecidedByPolicy) != (d.RunID != "") {
		return fmt.Errorf("decision %s: decided by %q with run ID %q: a policy decision needs the run ID of the run that made it, and a reviewer's decision has none; nothing was written",
			d.ID, d.DecidedBy, d.RunID)
	}
	return nil
}
