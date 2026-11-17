package cleaning

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

// ErrRetireCapExceeded marks a plan that would retire more verdicts than
// cleaning.max_retire_per_run allows.
var ErrRetireCapExceeded = errors.New("retire cap exceeded")

// retireCapError carries the counts for ErrRetireCapExceeded's message.
type retireCapError struct{ count, limit int }

func (e retireCapError) Error() string {
	return fmt.Sprintf("this run would retire %d verdicts, more than cleaning.max_retire_per_run (%d). "+
		"Narrow the run with --source-catalog/--target-catalog, or raise the setting for this tenant", e.count, e.limit)
}

func (e retireCapError) Unwrap() error { return ErrRetireCapExceeded }

// Status is a finding's state; see the design spec's "Finding status".
type Status string

const (
	StatusReported  Status = "reported"  // recorded, no action pending
	StatusPending   Status = "pending"   // in the review queue
	StatusRetired   Status = "retired"   // target verdicts superseded
	StatusDismissed Status = "dismissed" // a reviewer judged it not a problem
	StatusRestored  Status = "restored"  // a reviewer reversed a retire
	StatusCleared   Status = "cleared"   // a later pass no longer detects it
)

// Action is what the policy does with a signal's findings. Values match the
// config.CleaningAction* constants.
type Action string

const (
	ActionReport Action = config.CleaningActionReport
	ActionHuman  Action = config.CleaningActionHuman
	ActionRetire Action = config.CleaningActionRetire
)

// desired is the status a pass gives a finding under each action.
var desired = map[Action]Status{ActionReport: StatusReported, ActionHuman: StatusPending, ActionRetire: StatusRetired}

// Change says why a finding is in a plan.
type Change string

const (
	ChangeNew      Change = "new"      // not stored, or stored and cleared
	ChangeChanged  Change = "changed"  // reported or pending, evidence changed
	ChangeReraised Change = "reraised" // dismissed or restored, evidence changed
	ChangeStatus   Change = "status"   // reported or pending, policy now wants the other status
	ChangeCleared  Change = "cleared"  // reported or pending within scope, no longer detected
)

// StoredFinding is a finding's persisted state.
type StoredFinding struct {
	ID           string
	Signal       Signal
	Evidence     Evidence
	EvidenceHash string
	Concerns     []VerdictKey
	Targets      []VerdictKey
	// EdgeIDs are the targets' current edge IDs.
	EdgeIDs       []string
	Status        Status
	GraphApplied  bool
	SourceCatalog string
	TargetCatalog string
	UpdatedAt     time.Time
}

// PlannedFinding is a finding a run would write, and the transition it makes.
// From is empty for a finding not stored before.
type PlannedFinding struct {
	Finding
	ID, EvidenceHash string
	Action           Action
	Change           Change
	From, To         Status
	// EdgeIDs are the edges of the finding's targets, sorted and deduplicated.
	EdgeIDs []string
}

// Scope limits a run to one unordered catalog pair; both fields empty covers
// every mapped pair.
type Scope struct {
	SourceCatalog string `json:"source_catalog"`
	TargetCatalog string `json:"target_catalog"`
}

// covers reports whether a finding on catalogs (source, target) is within s,
// in either direction.
func (s Scope) covers(source, target string) bool {
	if s.SourceCatalog == "" && s.TargetCatalog == "" {
		return true
	}
	return (source == s.SourceCatalog && target == s.TargetCatalog) ||
		(source == s.TargetCatalog && target == s.SourceCatalog)
}

// Plan is what a cleaning run would write. Token identifies it so an apply
// can prove it is applying what was previewed.
type Plan struct {
	Scope    Scope
	Findings []PlannedFinding // sorted by ID
	// Retire is the sorted union of the targets of findings going to retired;
	// RetireEdgeIDs is the sorted union of their edges.
	Retire        []VerdictKey
	RetireEdgeIDs []string
	Summary       Summary
	Token         string
}

// hashParts returns the hex SHA-256 of parts, each length-prefixed so that no
// two part lists share an encoding even when a part holds a separator.
func hashParts(parts ...string) string {
	var b []byte
	for _, p := range parts {
		b = strconv.AppendInt(b, int64(len(p)), 10)
		b = append(b, ':')
		b = append(b, p...)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// jobSubject is the part FindingID puts between a job-level finding's signal
// and its job ID. A verdict-keyed finding has a verdict key there, which
// always holds two ':', so the two kinds never share an encoding.
const jobSubject = "job"

// FindingID is a finding's identity, and its subject_key: the hex SHA-256 of
// its signal and the sorted CLI forms of its Concerns, or for a job-level
// finding of its signal, jobSubject and the job ID. Nothing else affects it,
// so a re-run updates the stored finding, and a job-level finding keeps its
// ID whichever of the job's verdicts are live or in scope.
func FindingID(f Finding) string {
	if f.Evidence.Job != nil {
		return hashParts(string(f.Signal), jobSubject, f.Evidence.Job.ID)
	}
	parts := make([]string, len(f.Concerns))
	for i, k := range f.Concerns {
		parts[i] = k.String()
	}
	slices.Sort(parts)
	return hashParts(append([]string{string(f.Signal)}, parts...)...)
}

// EvidenceHash is the hex SHA-256 of e's canonical JSON: fields in
// declaration order, map keys sorted, times in UTC. Each verdict's EdgeIDs are
// left out: they are graph identity, not evidence, and a restore changes them,
// which must not re-raise the restored finding. Edge changes stay guarded by
// StateToken and the plan token, which both hash edge IDs. Of a job-level
// finding's job only the ID and completion time are hashed: with the detail
// (the stale stages and their recorded and current values) they are what
// makes it stale, while its verdict count changes whenever another finding
// retires or restores one of its verdicts, and its catalog names only
// display the job. It fails only when e holds a value JSON cannot encode,
// such as a NaN agreement.
func EvidenceHash(e Evidence) (string, error) {
	e.Verdicts = slices.Clone(e.Verdicts)
	for i := range e.Verdicts {
		e.Verdicts[i].JobCompletedAt = e.Verdicts[i].JobCompletedAt.UTC()
		e.Verdicts[i].EdgeIDs = nil
	}
	if e.Job != nil {
		e.Job = &JobEvidence{ID: e.Job.ID, CompletedAt: e.Job.CompletedAt.UTC()}
	}
	b, err := json.Marshal(e)
	if err != nil {
		return "", fmt.Errorf("encode evidence: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// StateToken is the hex SHA-256 of a stored finding's status, evidence hash
// and sorted edge IDs. A decision quotes it so that one made against a
// finding that changed since it was read can be refused.
func StateToken(f StoredFinding) string {
	edges := slices.Clone(f.EdgeIDs)
	slices.Sort(edges)
	return hashParts(append([]string{string(f.Status), f.EvidenceHash}, edges...)...)
}

// actionFor returns pol's action for signal s.
func actionFor(pol config.CleaningTenantConfig, s Signal) (Action, error) {
	var p config.SignalPolicy
	switch s {
	case SignalEquivalenceFanout:
		p = pol.Signals.EquivalenceFanout
	case SignalCrossJobContradiction:
		p = pol.Signals.CrossJobContradiction
	case SignalLowAgreement:
		p = pol.Signals.LowAgreement
	case SignalConflictsWith:
		p = pol.Signals.ConflictsWith
	case SignalStalePrompt:
		p = pol.Signals.StalePrompt
	case SignalStaleModel:
		p = pol.Signals.StaleModel
	default:
		return "", fmt.Errorf("unknown cleaning signal %q", s)
	}
	a := Action(p.Action)
	if _, ok := desired[a]; !ok {
		return "", fmt.Errorf("cleaning.signals.%s.action %q is not report, human or retire. Fix the cleaning policy for this tenant", s, p.Action)
	}
	return a, nil
}

// sortedUnique returns ids sorted without duplicates; nil when empty.
func sortedUnique(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	out := slices.Clone(ids)
	slices.Sort(out)
	return slices.Compact(out)
}

// targetEdgeIDs collects the edge IDs of f's targets: a job-level finding's
// TargetEdgeIDs, else its evidence's.
func targetEdgeIDs(f Finding) []string {
	if f.Evidence.Job != nil {
		return sortedUnique(f.TargetEdgeIDs)
	}
	var ids []string
	for _, v := range f.Evidence.Verdicts {
		if slices.Contains(f.Targets, v.Key) {
			ids = append(ids, v.EdgeIDs...)
		}
	}
	return sortedUnique(ids)
}

// BuildPlan compares freshly detected findings with stored ones under pol and
// returns what a run over scope would write. A finding gets the status its
// signal's action wants (report: reported, human: pending, retire: retired):
//
//   - not stored, or stored cleared: ChangeNew;
//   - stored reported or pending: ChangeChanged when the evidence hash
//     differs, else ChangeStatus when the status differs, else omitted;
//   - stored dismissed or restored: ChangeReraised when the evidence hash
//     differs, else omitted;
//   - stored retired: omitted.
//
// A stored reported or pending finding that was not detected and whose
// catalogs scope covers becomes cleared (ChangeCleared). Other undetected
// findings are omitted.
//
// A job-level finding keeps its Targets (the job's live verdicts in scope) and
// their edges only when it goes to retired, so a reported or pending one
// stores neither.
//
// It fails when a signal's action is not report, human or retire, a retire
// action meets a finding without targets, a stored status is unknown, two
// detected or two stored findings share an ID, the plan would retire more
// verdicts than pol.MaxRetirePerRun (ErrRetireCapExceeded), or a finding
// would not fit the store (ErrFindingTooLarge), so that no limit surfaces
// first at apply. Token hashes the
// scope, pol and each finding's ID, evidence hash, action, change, new status
// and edge IDs; sum is carried along but not hashed.
func BuildPlan(found []Finding, stored []StoredFinding, pol config.CleaningTenantConfig, scope Scope, sum Summary) (Plan, error) {
	byID := make(map[string]StoredFinding, len(stored))
	for _, s := range stored {
		if _, dup := byID[s.ID]; dup {
			return Plan{}, fmt.Errorf("two stored findings share ID %s", s.ID)
		}
		byID[s.ID] = s
	}

	var planned []PlannedFinding
	detected := make(map[string]bool, len(found))
	for _, f := range found {
		id := FindingID(f)
		if detected[id] {
			return Plan{}, fmt.Errorf("two detected findings share ID %s (signal %s)", id, f.Signal)
		}
		detected[id] = true
		p, keep, err := planDetected(f, id, byID, pol)
		if err != nil {
			return Plan{}, err
		}
		if keep {
			planned = append(planned, p)
		}
	}

	for _, s := range stored {
		if detected[s.ID] || (s.Status != StatusReported && s.Status != StatusPending) || !scope.covers(s.SourceCatalog, s.TargetCatalog) {
			continue
		}
		action, err := actionFor(pol, s.Signal)
		if err != nil {
			return Plan{}, fmt.Errorf("stored finding %s: %w", s.ID, err)
		}
		planned = append(planned, PlannedFinding{
			Finding: Finding{
				Signal: s.Signal, Concerns: s.Concerns, Targets: s.Targets, Evidence: s.Evidence,
				SourceCatalog: s.SourceCatalog, TargetCatalog: s.TargetCatalog,
			},
			ID: s.ID, EvidenceHash: s.EvidenceHash, Action: action, Change: ChangeCleared,
			From: s.Status, To: StatusCleared, EdgeIDs: sortedUnique(s.EdgeIDs),
		})
	}
	slices.SortFunc(planned, func(x, y PlannedFinding) int { return cmp.Compare(x.ID, y.ID) })

	var retire []VerdictKey
	var retireEdges []string
	for _, p := range planned {
		if p.To == StatusRetired {
			retire = append(retire, p.Targets...)
			retireEdges = append(retireEdges, p.EdgeIDs...)
		}
	}
	slices.SortFunc(retire, compareKeys)
	retire = slices.Compact(retire)
	if len(retire) > pol.MaxRetirePerRun {
		return Plan{}, retireCapError{count: len(retire), limit: pol.MaxRetirePerRun}
	}
	for _, p := range planned {
		// A retire is refused from retired, so a reviewer gets one subset.
		if err := checkStoredSize(StoredFinding{
			ID: p.ID, Signal: p.Signal, Evidence: p.Evidence, Concerns: p.Concerns, Targets: p.Targets, EdgeIDs: p.EdgeIDs,
		}, fmt.Sprintf("Set cleaning.signals.%s.action to report or human for this tenant and apply; "+
			"a reviewer may then retire one subset that fits with 'crosscodex admin cleaning decide --retire-verdict'", p.Signal)); err != nil {
			return Plan{}, err
		}
	}

	token, err := planToken(scope, pol, planned)
	if err != nil {
		return Plan{}, err
	}
	return Plan{
		Scope: scope, Findings: planned, Retire: retire, RetireEdgeIDs: sortedUnique(retireEdges),
		Summary: sum, Token: token,
	}, nil
}

// planDetected plans one detected finding against its stored state; keep is
// false when the finding is omitted.
func planDetected(f Finding, id string, byID map[string]StoredFinding, pol config.CleaningTenantConfig) (p PlannedFinding, keep bool, err error) {
	action, err := actionFor(pol, f.Signal)
	if err != nil {
		return PlannedFinding{}, false, err
	}
	if action == ActionRetire && len(f.Targets) == 0 {
		return PlannedFinding{}, false, fmt.Errorf("signal %s has action retire, but its finding %s names no verdicts to retire. "+
			"Set cleaning.signals.%s.action to report or human", f.Signal, id, f.Signal)
	}
	hash, err := EvidenceHash(f.Evidence)
	if err != nil {
		return PlannedFinding{}, false, fmt.Errorf("finding %s: %w", id, err)
	}
	if f.Evidence.Job != nil && action != ActionRetire {
		f.Targets, f.TargetEdgeIDs = nil, nil
	}
	p = PlannedFinding{Finding: f, ID: id, EvidenceHash: hash, Action: action, To: desired[action], EdgeIDs: targetEdgeIDs(f)}

	s, ok := byID[id]
	if !ok {
		p.Change = ChangeNew
		return p, true, nil
	}
	p.From = s.Status
	changed := hash != s.EvidenceHash
	switch s.Status {
	case StatusCleared:
		p.Change = ChangeNew
	case StatusReported, StatusPending:
		switch {
		case changed:
			p.Change = ChangeChanged
		case s.Status != p.To:
			p.Change = ChangeStatus
		default:
			return PlannedFinding{}, false, nil
		}
	case StatusDismissed, StatusRestored:
		if !changed {
			return PlannedFinding{}, false, nil
		}
		p.Change = ChangeReraised
	case StatusRetired:
		return PlannedFinding{}, false, nil
	default:
		return PlannedFinding{}, false, fmt.Errorf("stored finding %s has unknown status %q", id, s.Status)
	}
	return p, true, nil
}

// planToken is the hex SHA-256 of the canonical JSON of the scope, the
// effective policy and each finding as [ID, EvidenceHash, Action, Change, To,
// EdgeIDs], in the given (ID-sorted) order.
func planToken(scope Scope, pol config.CleaningTenantConfig, planned []PlannedFinding) (string, error) {
	findings := make([][]any, len(planned))
	for i, p := range planned {
		edges := p.EdgeIDs
		if edges == nil {
			edges = []string{}
		}
		findings[i] = []any{p.ID, p.EvidenceHash, p.Action, p.Change, p.To, edges}
	}
	b, err := json.Marshal(struct {
		Scope    Scope                       `json:"scope"`
		Policy   config.CleaningTenantConfig `json:"policy"`
		Findings [][]any                     `json:"findings"`
	}{scope, pol, findings})
	if err != nil {
		return "", fmt.Errorf("encode plan token: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
