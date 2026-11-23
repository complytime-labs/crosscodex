package cleaning

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/complytime-labs/crosscodex/pkg/attestation"
)

// DecideRequest is a reviewer's decision on one finding. Verdict is
// VerdictRetire, VerdictDismiss or VerdictRestore; ExpectState is the state
// token the reviewer read (GetFinding). RetireVerdicts, for a retire only,
// names the verdicts to retire, a subset of the finding's Concerns, or of a
// job-level finding's job's live verdicts; empty retires the finding's
// Targets, or all of the job's live verdicts. Reviewer is the authenticated
// subject; the caller sets it, never the request.
type DecideRequest struct {
	FindingID, Verdict, Rationale, ExpectState string
	RetireVerdicts                             []VerdictKey
	Reviewer                                   string
}

// transitions lists, per verdict, the statuses it may be decided from, and
// decided is the status it leads to. See the design spec's "Transitions".
var (
	verdicts    = []string{VerdictRetire, VerdictDismiss, VerdictRestore}
	transitions = map[string][]Status{
		VerdictRetire:  {StatusReported, StatusPending, StatusDismissed, StatusRestored},
		VerdictDismiss: {StatusReported, StatusPending},
		VerdictRestore: {StatusRetired},
	}
	decided = map[string]Status{VerdictRetire: StatusRetired, VerdictDismiss: StatusDismissed, VerdictRestore: StatusRestored}
)

// decideRequest is the canonical request of a reviewer's decision. Its
// SHA-256 is the decision ID, and the signed cleaning.decide link names it.
type decideRequest struct {
	Tenant         string   `json:"tenant"`
	FindingID      string   `json:"finding_id"`
	Verdict        string   `json:"verdict"`
	RetireVerdicts []string `json:"retire_verdicts"`
	Rationale      string   `json:"rationale"`
	Reviewer       string   `json:"reviewer"`
	ExpectState    string   `json:"expect_state"`
	DecidedAt      string   `json:"decided_at"`
	// RetireTargetsCount and RetireTargetsSHA256 record what a retire
	// removes, which for a job-level finding is expanded at decide time, so
	// an auditor can prove it after the finding row is overwritten: the
	// count of target keys and hashParts of their sorted
	// <job>:<source>:<target> forms. 0 and "" for dismiss and restore.
	RetireTargetsCount  int    `json:"retire_targets_count"`
	RetireTargetsSHA256 string `json:"retire_targets_sha256"`
}

// Decide records r against the finding, signs it with one cleaning.decide
// link, and applies its graph change: a retire supersedes the live edges of
// the retired verdicts, a restore recreates the retired edges, a dismiss
// changes no edge. It returns the finding as stored afterwards and the
// decision ID. It is refused for an empty rationale, while any finding is
// unprojected, without a signer, for a transition the finding's status does
// not allow, when the finding changed since r.ExpectState was read, and for a
// retire whose verdicts are outside the finding, missing when the finding has
// no targets, or no longer live. A graph failure after the decision is
// recorded returns a *ProjectionError naming the recorded decision.
func (e *Engine) Decide(ctx context.Context, tenantID string, r DecideRequest) (_ StoredFinding, _ string, err error) {
	ctx, finish, err := e.begin(ctx, "Decide", "decide", tenantID)
	if err != nil {
		return StoredFinding{}, "", err
	}
	defer func() { finish(err) }()

	if strings.TrimSpace(r.Rationale) == "" {
		return StoredFinding{}, "", fmt.Errorf("%w: finding %s: the rationale is empty, and every decision records why it was made. "+
			"Pass --rationale with the reason for this decision; nothing was written", ErrInvalidDecision, r.FindingID)
	}
	if err := e.refuseUnprojected(ctx, tenantID); err != nil {
		return StoredFinding{}, "", err
	}
	if e.signer == nil {
		return StoredFinding{}, "", fmt.Errorf("decide finding %s: %w", r.FindingID, ErrSigningDisabled)
	}
	f, _, err := e.store.GetFinding(ctx, tenantID, r.FindingID)
	if err != nil {
		return StoredFinding{}, "", fmt.Errorf("decide finding %s: %w", r.FindingID, err)
	}
	if err := checkTransition(f, r.Verdict); err != nil {
		return StoredFinding{}, "", err
	}
	if StateToken(f) != r.ExpectState {
		return StoredFinding{}, "", staleDecision(f.ID, ErrStateChanged)
	}

	next := f
	next.Status = decided[r.Verdict]
	retireVerdicts := sortedKeys(r.RetireVerdicts)
	retireVerdicts = slices.Compact(retireVerdicts)
	if r.Verdict == VerdictRetire {
		if next.Targets, next.EdgeIDs, err = e.retireSet(ctx, tenantID, f, retireVerdicts); err != nil {
			return StoredFinding{}, "", err
		}
	} else if len(retireVerdicts) > 0 {
		return StoredFinding{}, "", fmt.Errorf("%w: finding %s: --retire-verdict names verdicts to retire, so it applies only to --verdict %s, not %s. "+
			"Drop it, or decide %s instead", ErrInvalidDecision, f.ID, VerdictRetire, r.Verdict, VerdictRetire)
	}

	if r.Verdict == VerdictRetire {
		if err := checkStoredSize(next, "Name fewer verdicts with --retire-verdict"); err != nil {
			return StoredFinding{}, "", fmt.Errorf("decide finding %s: %w", f.ID, err)
		}
	}
	d, err := e.signDecision(ctx, tenantID, f, r, retireVerdicts, next.Targets)
	if err != nil {
		return StoredFinding{}, "", err
	}
	if err := e.store.Decide(ctx, tenantID, d, r.ExpectState, next); err != nil {
		if errors.Is(err, ErrStateChanged) {
			return StoredFinding{}, "", staleDecision(f.ID, err)
		}
		return StoredFinding{}, "", fmt.Errorf("decide finding %s: %w", f.ID, err)
	}
	e.recordPending(ctx, tenantID)
	slog.InfoContext(ctx, "cleaning decision recorded", "tenant.id", tenantID, "finding", f.ID,
		"verdict", d.Verdict, "decision", d.ID, "edges", len(next.EdgeIDs))
	if _, err := e.project(ctx, tenantID); err != nil {
		var pe *ProjectionError
		if errors.As(err, &pe) {
			pe.DecisionID = d.ID
		}
		return StoredFinding{}, "", err
	}

	got, _, err := e.store.GetFinding(ctx, tenantID, f.ID)
	if err != nil {
		return StoredFinding{}, "", fmt.Errorf("reload finding %s after the decision: %w", f.ID, err)
	}
	return got, d.ID, nil
}

// checkTransition refuses a verdict the finding's status does not allow,
// naming the status, where the verdict is allowed from and what is allowed
// now.
func checkTransition(f StoredFinding, verdict string) error {
	from, ok := transitions[verdict]
	if !ok {
		return fmt.Errorf("%w: finding %s: verdict %q is not %s; pass one of them as --verdict", ErrInvalidDecision, f.ID, verdict, joinOr(verdicts))
	}
	if slices.Contains(from, f.Status) {
		return nil
	}
	if f.Status == StatusStale {
		return fmt.Errorf("%w: finding %s is stale: its verdicts are no longer in the graph (see its stale decision). "+
			"Decide on the findings a new cleaning pass raises", ErrInvalidTransition, f.ID)
	}
	var now []string
	for _, v := range verdicts {
		if slices.Contains(transitions[v], f.Status) {
			now = append(now, v)
		}
	}
	allowed := "no decision is allowed now"
	if len(now) > 0 {
		allowed = "allowed now: " + joinOr(now)
	}
	return fmt.Errorf("%w: finding %s is %q; %s is allowed only from %s (%s)", ErrInvalidTransition, f.ID, f.Status, verdict, joinOr(from), allowed)
}

func staleDecision(findingID string, err error) error {
	return fmt.Errorf("decide finding %s: it changed since it was read, so this decision was refused and nothing was written. "+
		"Reload it with 'crosscodex admin cleaning show %s' and decide again: %w", findingID, findingID, err)
}

// retireTargets returns the verdicts a retire of f removes: given, each of
// which must be one of f's concerns, or else f's targets.
func retireTargets(f StoredFinding, given []VerdictKey) ([]VerdictKey, error) {
	if len(given) == 0 {
		if len(f.Targets) == 0 {
			return nil, fmt.Errorf("finding %s (%s): %w", f.ID, f.Signal, ErrRetireNeedsVerdicts)
		}
		return f.Targets, nil
	}
	for _, k := range given {
		if !slices.Contains(f.Concerns, k) {
			concerns := make([]string, len(f.Concerns))
			for i, c := range f.Concerns {
				concerns[i] = c.String()
			}
			return nil, fmt.Errorf("%w: %s is not one of finding %s's verdicts (%s); name only verdicts the finding concerns",
				ErrVerdictNotInFinding, k, f.ID, strings.Join(concerns, ", "))
		}
	}
	return given, nil
}

// retireSet returns what a retire of f removes and the targets' sorted live
// edges, re-reading the finding's catalog pair: for a job-level finding,
// given (each one of the job's live verdicts there) or else all of them, which
// is how its targets expand at decide time; otherwise retireTargets. A target
// without a live edge is refused: it was retired since the finding was read,
// or its edges are gone.
func (e *Engine) retireSet(ctx context.Context, tenantID string, f StoredFinding, given []VerdictKey) ([]VerdictKey, []string, error) {
	var targets []VerdictKey
	if f.Evidence.Job == nil {
		var err error
		if targets, err = retireTargets(f, given); err != nil {
			return nil, nil, err
		}
	}
	jobs, err := e.store.CompletedJobs(ctx, tenantID)
	if err != nil {
		return nil, nil, fmt.Errorf("decide finding %s: load completed jobs: %w", f.ID, err)
	}
	byJob := make(map[string]Job, len(jobs))
	for _, j := range jobs {
		byJob[j.ID] = j
	}
	pairs, _, err := e.loadPairs(ctx, tenantID, byJob, f.SourceCatalog, f.TargetCatalog)
	if err != nil {
		return nil, nil, fmt.Errorf("decide finding %s: %w", f.ID, err)
	}
	edgesOf := map[VerdictKey][]string{}
	for _, p := range pairs {
		for _, v := range p.Verdicts {
			edgesOf[v.Key] = v.EdgeIDs
		}
	}
	if f.Evidence.Job != nil {
		if targets, err = jobRetireTargets(f, given, edgesOf); err != nil {
			return nil, nil, err
		}
	}

	var edges []string
	for _, k := range targets {
		if len(edgesOf[k]) == 0 {
			return nil, nil, fmt.Errorf("decide finding %s: verdict %s: %w: it has no live edge in the graph, so it was retired since the finding was read "+
				"or its edges are gone. Reload the finding and decide again; nothing was written", f.ID, k, ErrEdgeNotLive)
		}
		edges = append(edges, edgesOf[k]...)
	}
	edges = sortedUnique(edges)
	if err := CheckLive(ctx, e.g, tenantID, edges); err != nil {
		return nil, nil, fmt.Errorf("decide finding %s: a verdict to retire is no longer live. Reload the finding and decide again; nothing was written: %w", f.ID, err)
	}
	return targets, edges, nil
}

// jobRetireTargets returns what a retire of job-level finding f removes:
// given, each of which must be a live verdict of f's job in edgesOf, or else
// every such verdict, sorted.
func jobRetireTargets(f StoredFinding, given []VerdictKey, edgesOf map[VerdictKey][]string) ([]VerdictKey, error) {
	job := f.Evidence.Job.ID
	var live []VerdictKey
	for k := range edgesOf {
		if k.JobID == job {
			live = append(live, k)
		}
	}
	if len(live) == 0 {
		return nil, fmt.Errorf("decide finding %s: %w: job %s has no live verdict between catalogs %s and %s, so there is nothing to retire; nothing was written",
			f.ID, ErrEdgeNotLive, job, f.SourceCatalog, f.TargetCatalog)
	}
	slices.SortFunc(live, compareKeys)
	if len(given) == 0 {
		return live, nil
	}
	for _, k := range given {
		if _, found := slices.BinarySearchFunc(live, k, compareKeys); !found {
			return nil, fmt.Errorf("%w: %s is not a live verdict of job %s, the subject of finding %s (%d live verdicts); "+
				"name only the job's live verdicts; nothing was written", ErrVerdictNotInFinding, k, job, f.ID, len(live))
		}
	}
	return given, nil
}

// signDecision builds the reviewer's decision: its canonical request, the ID
// that hashes it, and a cleaning.decide link whose material is the request
// and whose product is the finding with its evidence hash. targets are what a
// retire removes; the request records their count and hash.
func (e *Engine) signDecision(ctx context.Context, tenantID string, f StoredFinding, r DecideRequest, retireVerdicts, targets []VerdictKey) (Decision, error) {
	decidedAt := e.stamp()
	keys := make([]string, len(retireVerdicts))
	for i, k := range retireVerdicts {
		keys[i] = k.String()
	}
	dr := decideRequest{
		Tenant: tenantID, FindingID: f.ID, Verdict: r.Verdict, RetireVerdicts: keys, Rationale: r.Rationale,
		Reviewer: r.Reviewer, ExpectState: r.ExpectState, DecidedAt: decidedAt.Format(time.RFC3339Nano),
	}
	if r.Verdict == VerdictRetire {
		removed := make([]string, len(targets))
		for i, k := range targets {
			removed[i] = k.String()
		}
		slices.Sort(removed)
		dr.RetireTargetsCount, dr.RetireTargetsSHA256 = len(removed), hashParts(removed...)
	}
	req, err := json.Marshal(dr)
	if err != nil {
		return Decision{}, fmt.Errorf("decide finding %s: encode the request: %w", f.ID, err)
	}
	sum := sha256.Sum256(req)
	id := hex.EncodeToString(sum[:])
	link, err := e.signer.CreateLink(ctx, "cleaning.decide",
		[]attestation.Artifact{{URI: "crosscodex:cleaning-decision/" + id, Digest: id}},
		[]attestation.Artifact{{URI: "crosscodex:finding/" + f.ID, Digest: f.EvidenceHash}})
	if err != nil {
		return Decision{}, fmt.Errorf("decide finding %s: sign the decision: %w", f.ID, err)
	}
	return Decision{
		ID: id, FindingID: f.ID, Verdict: r.Verdict, DecidedBy: r.Reviewer, Rationale: r.Rationale,
		Request: req, Attestation: link.Raw, DecidedAt: decidedAt,
	}, nil
}
