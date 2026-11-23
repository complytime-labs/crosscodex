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
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
)

// ReprojectAction is what re-projection does with one finding.
type ReprojectAction string

const (
	// ReprojectRetire supersedes the live edges a retired finding's verdicts
	// have in the graph, and stores them with the finding's edge IDs.
	ReprojectRetire ReprojectAction = "retire"
	// ReprojectRelink replaces a restored finding's edge IDs with its
	// verdicts' live edges.
	ReprojectRelink ReprojectAction = "relink"
	// ReprojectStale marks a finding whose verdicts are gone from the graph
	// stale, with a signed decision naming them.
	ReprojectStale ReprojectAction = "stale"
	// ReprojectInSync changes nothing: the graph already agrees with the
	// finding.
	ReprojectInSync ReprojectAction = "in_sync"
)

// ReprojectItem is one finding re-projection judged.
type ReprojectItem struct {
	FindingID string
	// Status is the finding's status as read: StatusRetired or
	// StatusRestored.
	Status Status
	Action ReprojectAction
	// EdgeIDs are the edge IDs the finding stores after the action, sorted.
	EdgeIDs []string
	// Missing, set only for ReprojectStale, are the finding's targets, sorted:
	// the verdicts the graph no longer holds.
	Missing []VerdictKey
}

// ReprojectReport is what a re-projection did, or with dryRun would do.
type ReprojectReport struct {
	// Items holds one entry per retired or restored finding, in ID order.
	Items []ReprojectItem
	// Counts of Items by action.
	Retired, Relinked, Staled, InSync int
}

// reprojectRequest is the canonical request of re-projection's stale
// decision. Its SHA-256 is the decision ID, and the signed cleaning.reproject
// link names it.
type reprojectRequest struct {
	Tenant       string   `json:"tenant"`
	FindingID    string   `json:"finding_id"`
	Verdict      string   `json:"verdict"`
	Reason       string   `json:"reason"`
	Missing      []string `json:"missing"`
	EvidenceHash string   `json:"evidence_hash"`
	DecidedAt    string   `json:"decided_at"`
}

// Reproject replays the tenant's retired and restored findings into the
// graph, for use after the graph was rebuilt from the relational record. It
// calls no LLM.
//
// Each target verdict is governed by the latest retire or restore decision
// among the retired and restored findings that target it (decided_at; on a
// tie the lowest decision ID), so the latest change wins whatever the
// findings' ID order, and a finding changes only the edges of verdicts it
// governs. For each finding, in ID order, it reads which stored edge IDs
// still exist and the live edges of its targets:
//
//   - retired, and a governed verdict has a live edge (rebuilt, or a stored
//     edge live again): the edges are superseded at the governing decision's
//     time, or at an edge's valid_from when that is later
//     (RetireRebuiltEdges), and join its edge IDs (ReprojectRetire);
//   - its edge IDs would change: a governed restored verdict's live edges, or
//     a verdict whose stored edges are gone taking its live ones, become its
//     edge IDs (ReprojectRelink);
//   - no target has a live edge, no stored edge ID is in the graph, and the
//     graph holds no edge of any target at all, live or superseded, under any
//     edge ID (PresentVerdicts): the graph was rebuilt from new jobs, so the
//     finding is marked stale with a signed decision naming the missing
//     verdicts (ReprojectStale);
//   - anything else is consistent and changes nothing (ReprojectInSync). This
//     includes a target whose only edges feedback re-projection retired,
//     governed there by a later mapping determination.
//
// A second run therefore reports every remaining finding in sync. With dryRun
// it computes the same report and writes nothing. It is refused while any
// finding is unprojected (resume first), and without a signer unless dryRun,
// before anything is written. Every write is checked against the state token
// read, so a decision recorded meanwhile stops the run with ErrStateChanged
// naming the finding. A failure part-way leaves the findings before it
// re-projected; running it again finishes the rest.
func (e *Engine) Reproject(ctx context.Context, tenantID string, dryRun bool) (_ ReprojectReport, err error) {
	ctx, finish, err := e.begin(ctx, "Reproject", "reproject", tenantID)
	if err != nil {
		return ReprojectReport{}, err
	}
	defer func() { finish(err) }()
	slog.InfoContext(ctx, "cleaning reproject started", "tenant.id", tenantID, "dry_run", dryRun)

	if err := e.refuseUnprojected(ctx, tenantID); err != nil {
		return ReprojectReport{}, err
	}
	if !dryRun && e.signer == nil {
		return ReprojectReport{}, fmt.Errorf("reproject findings: %w", ErrSigningDisabled)
	}
	stored, err := e.store.Findings(ctx, tenantID, Scope{})
	if err != nil {
		return ReprojectReport{}, fmt.Errorf("reproject findings: %w", err)
	}
	var todo []reprojectCandidate
	for _, f := range stored {
		verdict := VerdictRetire
		switch f.Status {
		case StatusRetired:
		case StatusRestored:
			verdict = VerdictRestore
		default:
			continue
		}
		d, err := e.latestDecision(ctx, tenantID, f, verdict)
		if err != nil {
			return ReprojectReport{}, fmt.Errorf("reproject findings: %w", err)
		}
		todo = append(todo, reprojectCandidate{f: f, d: d})
	}
	governor := governors(todo)
	// One read serves the whole run: each verdict has one governor, and only
	// it changes the verdict's edges, so no finding sees an edge another
	// finding of this run changed.
	liveOf, err := e.liveTargetEdges(ctx, tenantID, todo)
	if err != nil {
		return ReprojectReport{}, fmt.Errorf("reproject findings: %w", err)
	}

	var rep ReprojectReport
	for i, c := range todo {
		item, err := e.reprojectOne(ctx, tenantID, c, func(k VerdictKey) bool { return governor[k] == i }, liveOf, dryRun)
		if errors.Is(err, ErrStateChanged) {
			err = fmt.Errorf("finding %s changed during re-projection, so nothing was recorded for it; rerun --reproject to judge its current state: %w",
				c.f.ID, err)
		}
		if err != nil {
			return ReprojectReport{}, fmt.Errorf("reproject finding %s: %d of %d findings were re-projected before it and stand; "+
				"run the re-projection again to finish, it repeats nothing already done: %w", c.f.ID, i, len(todo), err)
		}
		rep.Items = append(rep.Items, item)
		switch item.Action {
		case ReprojectRetire:
			rep.Retired++
		case ReprojectRelink:
			rep.Relinked++
		case ReprojectStale:
			rep.Staled++
		case ReprojectInSync:
			rep.InSync++
		}
	}
	slog.InfoContext(ctx, "cleaning reproject finished", "tenant.id", tenantID, "dry_run", dryRun, "findings", len(rep.Items),
		"retired", rep.Retired, "relinked", rep.Relinked, "staled", rep.Staled, "in_sync", rep.InSync)
	return rep, nil
}

// reprojectCandidate is a retired or restored finding and its latest retire
// or restore decision.
type reprojectCandidate struct {
	f StoredFinding
	d Decision
}

// governors maps each target verdict of cs to the index of the candidate
// whose decision governs it: the latest decided_at, on a tie the lowest
// decision ID.
func governors(cs []reprojectCandidate) map[VerdictKey]int {
	out := map[VerdictKey]int{}
	for i, c := range cs {
		for _, k := range c.f.Targets {
			j, seen := out[k]
			if !seen {
				out[k] = i
				continue
			}
			cur := cs[j].d
			if c.d.DecidedAt.After(cur.DecidedAt) || (c.d.DecidedAt.Equal(cur.DecidedAt) && c.d.ID < cur.ID) {
				out[k] = i
			}
		}
	}
	return out
}

// liveTargetEdges returns the live edges of every target of cs whose job is
// a completed job of the tenant. VerdictEdges is called only with those:
// retention may have purged an old finding's job, which then has no verdicts
// left to read, and VerdictEdges would refuse it with ErrVerdictJobNotFound.
// Such a target is left out of the map, so it has no live edge and the
// finding is judged by its stored edge IDs alone.
func (e *Engine) liveTargetEdges(ctx context.Context, tenantID string, cs []reprojectCandidate) (map[VerdictKey][]string, error) {
	jobs, err := e.store.CompletedJobs(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("load completed jobs: %w", err)
	}
	completed := make(map[string]bool, len(jobs))
	for _, j := range jobs {
		completed[j.ID] = true
	}
	seen := map[VerdictKey]bool{}
	var keys []VerdictKey
	for _, c := range cs {
		for _, k := range c.f.Targets {
			if completed[k.JobID] && !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	if len(keys) == 0 {
		return map[VerdictKey][]string{}, nil
	}
	return e.VerdictEdges(ctx, tenantID, keys)
}

// storedEdge is one of a finding's stored edges still in the graph: its
// verdict, read from the edge's job_id and endpoints as a cleaning pass reads
// it, and whether it is live.
type storedEdge struct {
	id   string
	key  VerdictKey
	live bool
}

// storedEdges returns those of ids the graph still holds, in input order. A
// missing edge (graphdb.ErrEdgeNotFound) is left out; any other failure is
// returned, so an outage is not taken for a rebuilt graph.
func storedEdges(ctx context.Context, g graphdb.GraphDB, tenantID string, ids []string) ([]storedEdge, error) {
	edges, err := lookupEdges(ctx, g, tenantID, ids)
	if err != nil {
		return nil, err
	}
	var out []storedEdge
	for _, le := range edges {
		ed := le.edge
		job, _ := ed.Properties["job_id"].(string)
		out = append(out, storedEdge{id: le.id, key: VerdictKey{JobID: job, SourceID: ed.SourceID, TargetID: ed.TargetID}, live: ed.ValidTo == nil})
	}
	return out, nil
}

// reprojectOne judges one retired or restored finding and, unless dryRun,
// applies the change; governs reports whether the finding's decision
// governs a verdict. See Reproject.
func (e *Engine) reprojectOne(ctx context.Context, tenantID string, c reprojectCandidate, governs func(VerdictKey) bool,
	liveOf map[VerdictKey][]string, dryRun bool) (ReprojectItem, error) {
	f := c.f
	existing, err := storedEdges(ctx, e.g, tenantID, f.EdgeIDs)
	if err != nil {
		return ReprojectItem{}, err
	}
	targets := slices.Compact(sortedKeys(f.Targets))
	existingOf := map[VerdictKey][]storedEdge{}
	var next, retire []string
	for _, se := range existing {
		if _, ok := slices.BinarySearchFunc(targets, se.key, compareKeys); ok {
			existingOf[se.key] = append(existingOf[se.key], se)
			continue
		}
		// An edge the finding stores under none of its targets is its own.
		next = append(next, se.id)
		if f.Status == StatusRetired && se.live {
			retire = append(retire, se.id)
		}
	}
	anyLive := false
	for _, k := range targets {
		live := liveOf[k]
		anyLive = anyLive || len(live) > 0
		var kept []string
		for _, se := range existingOf[k] {
			kept = append(kept, se.id)
			if governs(k) && f.Status == StatusRetired && se.live {
				retire = append(retire, se.id)
			}
		}
		switch {
		case governs(k) && f.Status == StatusRetired:
			next = append(append(next, kept...), live...)
			retire = append(retire, live...)
		case governs(k) && len(live) > 0:
			next = append(next, live...)
		case len(kept) > 0:
			next = append(next, kept...)
		default:
			// The verdict's stored edges are gone: name its edges now.
			next = append(next, live...)
		}
	}
	next, retire = sortedUnique(next), sortedUnique(retire)
	stored := sortedUnique(f.EdgeIDs)

	item := ReprojectItem{FindingID: f.ID, Status: f.Status, Action: ReprojectInSync, EdgeIDs: stored}
	gone := !anyLive && len(existing) == 0
	if gone {
		present, err := PresentVerdicts(ctx, e.g, tenantID, targets)
		if err != nil {
			return ReprojectItem{}, err
		}
		if slices.ContainsFunc(targets, func(k VerdictKey) bool { return present[k] }) {
			// The verdict is still in the graph, under edge IDs the finding
			// never stored, and has no live edge it can read: feedback
			// re-projection retired them, or retention purged the job. It
			// is not gone, and nothing here is the finding's to change.
			return item, nil
		}
	}
	switch {
	case gone:
		item.Action, item.Missing = ReprojectStale, targets
		if !dryRun {
			if err := e.markStale(ctx, tenantID, f, item.Missing); err != nil {
				return ReprojectItem{}, err
			}
		}
	case len(retire) > 0:
		item.Action, item.EdgeIDs = ReprojectRetire, next
		if !dryRun {
			// Recorded before the retire: should the retire fail, the stored
			// edge IDs name the still-live edges, so a rerun finds and retires
			// them. The other order would leave edge IDs the graph does not
			// hold, and a rerun would find nothing and mark the finding stale.
			if err := e.store.MarkReprojected(ctx, tenantID, f.ID, StateToken(f), next); err != nil {
				return ReprojectItem{}, err
			}
			if _, _, err := RetireRebuiltEdges(ctx, e.g, tenantID, retire, c.d.DecidedAt); err != nil {
				return ReprojectItem{}, err
			}
		}
	case !slices.Equal(next, stored):
		item.Action, item.EdgeIDs = ReprojectRelink, next
		if !dryRun {
			if err := e.store.MarkReprojected(ctx, tenantID, f.ID, StateToken(f), next); err != nil {
				return ReprojectItem{}, err
			}
		}
	}
	return item, nil
}

// markStale signs and records re-projection's stale decision on f, whose
// verdicts missing the graph no longer holds.
func (e *Engine) markStale(ctx context.Context, tenantID string, f StoredFinding, missing []VerdictKey) error {
	keys := make([]string, len(missing))
	for i, k := range missing {
		keys[i] = k.String()
	}
	reason := fmt.Sprintf("the graph no longer holds verdicts %s: it was rebuilt from new jobs. "+
		"Decide again on the findings a new cleaning pass raises", strings.Join(keys, ", "))
	decidedAt := e.stamp()
	req, err := json.Marshal(reprojectRequest{
		Tenant: tenantID, FindingID: f.ID, Verdict: VerdictStale, Reason: reason, Missing: keys,
		EvidenceHash: f.EvidenceHash, DecidedAt: decidedAt.Format(time.RFC3339Nano),
	})
	if err != nil {
		return fmt.Errorf("encode the stale decision: %w", err)
	}
	sum := sha256.Sum256(req)
	id := hex.EncodeToString(sum[:])
	link, err := e.signer.CreateLink(ctx, "cleaning.reproject",
		[]attestation.Artifact{{URI: "crosscodex:cleaning-decision/" + id, Digest: id}},
		[]attestation.Artifact{{URI: "crosscodex:finding/" + f.ID, Digest: f.EvidenceHash}})
	if err != nil {
		return fmt.Errorf("sign the stale decision: %w", err)
	}
	d := Decision{
		ID: id, FindingID: f.ID, Verdict: VerdictStale, DecidedBy: DecidedByReproject, Rationale: reason,
		Request: req, Attestation: link.Raw, DecidedAt: decidedAt,
	}
	if err := e.store.MarkStale(ctx, tenantID, d, StateToken(f)); err != nil {
		return err
	}
	slog.InfoContext(ctx, "cleaning finding marked stale", "tenant.id", tenantID, "finding", f.ID,
		"decision", d.ID, "missing", len(missing))
	return nil
}
