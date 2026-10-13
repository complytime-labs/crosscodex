package artifacts_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/complytime-labs/crosscodex/internal/analyzer/artifacts"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// fakeVerdictStore is an in-memory artifacts.VerdictStore with the same
// semantics as PGVerdictStore, including microsecond timestamps. Tests
// inspect rows with row and set them up with set and setHuman; err injects a
// failure per method name. enqueued records the candidates of every Enqueue
// call.
type fakeVerdictStore struct {
	mu       sync.Mutex
	rows     map[string]*fakeVerdictRow // fakeRowID(tenant, key)
	err      map[string]error
	enqueued [][]artifacts.PairCandidate
}

type fakeVerdictRow struct {
	key           string
	cand          artifacts.PairCandidate
	status        artifacts.VerdictStatus
	determination string
	determinedBy  string
	confidence    float64
	evidence      artifacts.PanelEvidence
	dissent       *artifacts.Verdict
	graphApplied  bool
	attempts      int
	nextAttemptAt time.Time
	leasedUntil   time.Time // zero: not leased
	lastError     string
	decidedAt     time.Time
	createdAt     time.Time
	claimedUntil  time.Time // zero: no projection claim
}

var _ artifacts.VerdictStore = (*fakeVerdictStore)(nil)

func newFakeVerdictStore() *fakeVerdictStore {
	return &fakeVerdictStore{rows: map[string]*fakeVerdictRow{}, err: map[string]error{}}
}

func fakeRowID(tenantID, key string) string { return tenantID + "\x00" + key }

// checkTenant refuses a tenantID that differs from ctx's tenant, as
// PGVerdictStore does. Unlike PG it accepts a context without a tenant, so
// unit tests can pass context.Background().
func checkTenant(ctx context.Context, tenantID string) error {
	ctxTenant, err := tenant.FromContext(ctx)
	if err == nil && ctxTenant != tenantID {
		return fmt.Errorf("verdict store: context tenant %q does not match tenant %q", ctxTenant, tenantID)
	}
	return nil
}

func (s *fakeVerdictStore) Enqueue(ctx context.Context, tenantID string, cands []artifacts.PairCandidate, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enqueued = append(s.enqueued, slices.Clone(cands))
	if err := s.err["Enqueue"]; err != nil {
		return err
	}
	if err := checkTenant(ctx, tenantID); err != nil {
		return err
	}
	for _, c := range cands {
		if c.LowGroupID >= c.HighGroupID {
			return fmt.Errorf("enqueue: %q is not below %q", c.LowGroupID, c.HighGroupID)
		}
	}
	for _, c := range cands {
		key := artifacts.PairKey(c.LowGroupID, c.HighGroupID)
		if r, ok := s.rows[fakeRowID(tenantID, key)]; ok {
			if r.status == artifacts.VerdictPending && r.determination != artifacts.DeterminationHuman {
				r.cand.CandidateEdgeID, r.cand.SimilarityScore = c.CandidateEdgeID, c.SimilarityScore
			}
			continue
		}
		s.rows[fakeRowID(tenantID, key)] = &fakeVerdictRow{
			key: key, cand: c, status: artifacts.VerdictPending,
			determination: artifacts.DeterminationTokenOverlap, nextAttemptAt: now, createdAt: now.Truncate(time.Microsecond),
		}
	}
	return nil
}

func (s *fakeVerdictStore) ClosedPairs(ctx context.Context, tenantID string) (map[string]bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.err["ClosedPairs"]; err != nil {
		return nil, err
	}
	if err := checkTenant(ctx, tenantID); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for id, r := range s.rows {
		if strings.HasPrefix(id, tenantID+"\x00") && (r.status == artifacts.VerdictConfirmed || r.status == artifacts.VerdictRejected) {
			out[r.key] = true
		}
	}
	return out, nil
}

func (s *fakeVerdictStore) Lease(ctx context.Context, tenantID string, n int, leaseFor time.Duration, now time.Time) ([]artifacts.LeasedPair, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.err["Lease"]; err != nil {
		return nil, err
	}
	if err := checkTenant(ctx, tenantID); err != nil {
		return nil, err
	}
	if n <= 0 || leaseFor <= 0 {
		return nil, errors.New("lease: n and leaseFor must be positive")
	}
	var due []*fakeVerdictRow
	for id, r := range s.rows {
		if strings.HasPrefix(id, tenantID+"\x00") && r.status == artifacts.VerdictPending &&
			r.determination != artifacts.DeterminationHuman && !r.nextAttemptAt.After(now) &&
			(r.leasedUntil.IsZero() || r.leasedUntil.Before(now)) {
			due = append(due, r)
		}
	}
	slices.SortFunc(due, func(a, b *fakeVerdictRow) int {
		if c := a.nextAttemptAt.Compare(b.nextAttemptAt); c != 0 {
			return c
		}
		return strings.Compare(a.key, b.key)
	})
	var out []artifacts.LeasedPair
	for _, r := range due[:min(n, len(due))] {
		r.leasedUntil = now.Add(leaseFor)
		out = append(out, artifacts.LeasedPair{PairCandidate: r.cand, Key: r.key, Attempts: r.attempts, LeasedUntil: r.leasedUntil})
	}
	return out, nil
}

func (s *fakeVerdictStore) Decide(ctx context.Context, tenantID, key string, v artifacts.Verdict) (artifacts.Decision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.err["Decide"]; err != nil {
		return artifacts.Decision{}, err
	}
	if err := checkTenant(ctx, tenantID); err != nil {
		return artifacts.Decision{}, err
	}
	switch v.Status {
	case artifacts.VerdictConfirmed, artifacts.VerdictRejected, artifacts.VerdictUndecided:
	default:
		return artifacts.Decision{}, fmt.Errorf("decide pair %s: status %q is not a panel verdict", artifacts.PairRef(key), v.Status)
	}
	r, ok := s.rows[fakeRowID(tenantID, key)]
	switch {
	case !ok:
		return artifacts.Decision{}, fmt.Errorf("decide pair %s: pair is not queued", artifacts.PairRef(key))
	case r.determination == artifacts.DeterminationHuman:
		return artifacts.Decision{Outcome: artifacts.DecideHumanOwned}, nil
	case r.status != artifacts.VerdictPending:
		return artifacts.Decision{Outcome: artifacts.DecideStale}, nil
	}
	r.status, r.determination, r.determinedBy = v.Status, artifacts.DeterminationLLMPanel, "artifact-adjudicator"
	r.confidence, r.evidence, r.decidedAt = v.Confidence, v.Evidence, v.DecidedAt.Truncate(time.Microsecond)
	r.leasedUntil, r.graphApplied, r.lastError = time.Time{}, false, ""
	return artifacts.Decision{Outcome: artifacts.DecideApplied, CandidateEdgeID: r.cand.CandidateEdgeID}, nil
}

func (s *fakeVerdictStore) RecordDissent(ctx context.Context, tenantID, key string, v artifacts.Verdict) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.err["RecordDissent"]; err != nil {
		return err
	}
	if err := checkTenant(ctx, tenantID); err != nil {
		return err
	}
	if r, ok := s.rows[fakeRowID(tenantID, key)]; ok && r.determination == artifacts.DeterminationHuman {
		v.DecidedAt = v.DecidedAt.Truncate(time.Microsecond)
		r.dissent = &v
	}
	return nil
}

func (s *fakeVerdictStore) DecideHuman(ctx context.Context, tenantID, ref string, h artifacts.HumanVerdict) (artifacts.HumanDecision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.err["DecideHuman"]; err != nil {
		return artifacts.HumanDecision{}, err
	}
	if err := checkTenant(ctx, tenantID); err != nil {
		return artifacts.HumanDecision{}, err
	}
	if err := artifacts.ExportValidateHumanVerdict(ref, h); err != nil {
		return artifacts.HumanDecision{}, err
	}
	var matches []*fakeVerdictRow
	for id, r := range s.rows {
		if strings.HasPrefix(id, tenantID+"\x00") && artifacts.PairRef(r.key) == ref {
			matches = append(matches, r)
		}
	}
	switch len(matches) {
	case 0:
		return artifacts.HumanDecision{}, fmt.Errorf("decide human %s: %w", ref, artifacts.ErrPairNotFound)
	case 1:
	default:
		return artifacts.HumanDecision{}, fmt.Errorf("decide human %s: %w", ref, artifacts.ErrPairRefAmbiguous)
	}
	r := matches[0]
	d := artifacts.HumanDecision{Outcome: artifacts.HumanApplied, Key: r.key}
	if r.claimedUntil.After(h.DecidedAt) {
		d.ClaimedUntil = r.claimedUntil
	}
	if r.determination == artifacts.DeterminationHuman && r.status == h.Status {
		if !r.disputed() {
			d.Outcome = artifacts.HumanUnchanged
			return d, nil
		}
		d.Outcome = artifacts.HumanReaffirmed
	}
	if r.determination == artifacts.DeterminationLLMPanel {
		r.dissent = &artifacts.Verdict{Status: r.status, Confidence: r.confidence, Evidence: r.evidence, DecidedAt: r.decidedAt}
	}
	r.status, r.determination, r.determinedBy = h.Status, artifacts.DeterminationHuman, h.Reviewer
	r.confidence, r.evidence, r.decidedAt = 1, artifacts.PanelEvidence{}, h.DecidedAt.Truncate(time.Microsecond)
	r.graphApplied, r.leasedUntil, r.lastError = false, time.Time{}, ""
	return d, nil
}

func (s *fakeVerdictStore) ReviewQueue(ctx context.Context, tenantID string, n int) ([]artifacts.ReviewItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.err["ReviewQueue"]; err != nil {
		return nil, err
	}
	if err := checkTenant(ctx, tenantID); err != nil {
		return nil, err
	}
	if n <= 0 {
		return nil, fmt.Errorf("review queue: n (%d) must be positive", n)
	}
	var rows []*fakeVerdictRow
	for id, r := range s.rows {
		if !strings.HasPrefix(id, tenantID+"\x00") {
			continue
		}
		if r.status == artifacts.VerdictUndecided || r.status == artifacts.VerdictAbandoned || r.disputed() {
			rows = append(rows, r)
		}
	}
	slices.SortFunc(rows, func(a, b *fakeVerdictRow) int {
		if c := a.createdAt.Compare(b.createdAt); c != 0 {
			return c
		}
		return strings.Compare(a.key, b.key)
	})
	var out []artifacts.ReviewItem
	for _, r := range rows[:min(n, len(rows))] {
		item := artifacts.ReviewItem{Ref: artifacts.PairRef(r.key), Key: r.key, Status: r.status,
			DeterminationType: r.determination, SimilarityScore: r.cand.SimilarityScore, Context: r.cand.Context}
		if r.dissent != nil {
			item.DissentStatus = r.dissent.Status
		}
		out = append(out, item)
	}
	return out, nil
}

// SearchPairs folds case with strings.ToLower; PGVerdictStore's ILIKE folds
// by the database locale. They agree on ASCII, which is what the contract
// specs search.
func (s *fakeVerdictStore) SearchPairs(ctx context.Context, tenantID, name string, n int) ([]artifacts.ReviewItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.err["SearchPairs"]; err != nil {
		return nil, err
	}
	if err := checkTenant(ctx, tenantID); err != nil {
		return nil, err
	}
	if n <= 0 {
		return nil, fmt.Errorf("search pairs: n (%d) must be positive", n)
	}
	if err := artifacts.ValidateSearchName(name); err != nil {
		return nil, err
	}
	needle := strings.ToLower(name)
	var rows []*fakeVerdictRow
	for id, r := range s.rows {
		if strings.HasPrefix(id, tenantID+"\x00") && (strings.Contains(strings.ToLower(r.cand.Context.LowName), needle) ||
			strings.Contains(strings.ToLower(r.cand.Context.HighName), needle)) {
			rows = append(rows, r)
		}
	}
	slices.SortFunc(rows, func(a, b *fakeVerdictRow) int {
		if c := a.createdAt.Compare(b.createdAt); c != 0 {
			return c
		}
		return strings.Compare(a.key, b.key)
	})
	var out []artifacts.ReviewItem
	for _, r := range rows[:min(n, len(rows))] {
		item := artifacts.ReviewItem{Ref: artifacts.PairRef(r.key), Key: r.key, Status: r.status,
			DeterminationType: r.determination, SimilarityScore: r.cand.SimilarityScore, Context: r.cand.Context}
		if r.dissent != nil {
			item.DissentStatus = r.dissent.Status
		}
		out = append(out, item)
	}
	return out, nil
}

func (s *fakeVerdictStore) RecordFailure(ctx context.Context, tenantID, key string, f artifacts.Failure) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.err["RecordFailure"]; err != nil {
		return err
	}
	if err := checkTenant(ctx, tenantID); err != nil {
		return err
	}
	r, ok := s.rows[fakeRowID(tenantID, key)]
	if !ok || r.status != artifacts.VerdictPending || r.determination == artifacts.DeterminationHuman ||
		r.leasedUntil.IsZero() || !r.leasedUntil.Equal(f.LeasedUntil) {
		return nil
	}
	r.attempts++
	r.lastError, r.leasedUntil, r.nextAttemptAt = f.Err, time.Time{}, f.NextAttemptAt
	if f.Abandon {
		r.status, r.decidedAt = artifacts.VerdictAbandoned, f.At.Truncate(time.Microsecond)
	}
	return nil
}

func (s *fakeVerdictStore) Unprojected(ctx context.Context, tenantID string, n int, now time.Time) ([]artifacts.Projection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.err["Unprojected"]; err != nil {
		return nil, err
	}
	if err := checkTenant(ctx, tenantID); err != nil {
		return nil, err
	}
	var rows []*fakeVerdictRow
	for id, r := range s.rows {
		if strings.HasPrefix(id, tenantID+"\x00") && !r.graphApplied && !r.claimedUntil.After(now) &&
			(r.status == artifacts.VerdictConfirmed || r.status == artifacts.VerdictRejected) {
			rows = append(rows, r)
		}
	}
	slices.SortFunc(rows, func(a, b *fakeVerdictRow) int {
		if c := a.decidedAt.Compare(b.decidedAt); c != 0 {
			return c
		}
		return strings.Compare(a.key, b.key)
	})
	var out []artifacts.Projection
	for _, r := range rows[:min(n, len(rows))] {
		p := artifacts.Projection{
			Key: r.key, LowGroupID: r.cand.LowGroupID, HighGroupID: r.cand.HighGroupID,
			CandidateEdgeID: r.cand.CandidateEdgeID, Status: r.status, Confidence: r.confidence,
			DecidedAt: r.decidedAt, DeterminationType: r.determination, DeterminedBy: r.determinedBy,
		}
		if r.determination == artifacts.DeterminationLLMPanel {
			p.Evidence = r.evidence
		}
		out = append(out, p)
	}
	return out, nil
}

// disputed reports whether r is a human verdict a panel contradicted
// afterwards: a confirmed or rejected dissent, different from the human
// status, decided after the human verdict.
func (r *fakeVerdictRow) disputed() bool {
	return r.determination == artifacts.DeterminationHuman && r.dissent != nil &&
		(r.dissent.Status == artifacts.VerdictConfirmed || r.dissent.Status == artifacts.VerdictRejected) &&
		r.dissent.Status != r.status && r.dissent.DecidedAt.After(r.decidedAt)
}

// holds reports whether r holds the verdict p describes.
func (r *fakeVerdictRow) holds(p artifacts.Projection) bool {
	return r.determination == p.DeterminationType && r.status == p.Status &&
		r.decidedAt.Truncate(time.Microsecond).Equal(p.DecidedAt.Truncate(time.Microsecond))
}

func (s *fakeVerdictStore) ClaimProjection(ctx context.Context, tenantID string, p artifacts.Projection, until, now time.Time) (artifacts.ClaimOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.err["ClaimProjection"]; err != nil {
		return 0, err
	}
	if err := checkTenant(ctx, tenantID); err != nil {
		return 0, err
	}
	if !until.After(now) {
		return 0, fmt.Errorf("claim projection pair %s: until (%s) must be after now (%s)", artifacts.PairRef(p.Key), until, now)
	}
	r, ok := s.rows[fakeRowID(tenantID, p.Key)]
	switch {
	case !ok:
		return 0, fmt.Errorf("claim projection pair %s: pair is not queued", artifacts.PairRef(p.Key))
	case !r.holds(p):
		return artifacts.ClaimReplaced, nil
	case r.graphApplied || r.claimedUntil.Truncate(time.Microsecond).After(now):
		return artifacts.ClaimBusy, nil
	}
	r.claimedUntil = until.Truncate(time.Microsecond)
	return artifacts.ClaimAcquired, nil
}

func (s *fakeVerdictStore) MarkProjected(ctx context.Context, tenantID string, p artifacts.Projection, claimedUntil time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.err["MarkProjected"]; err != nil {
		return false, err
	}
	if err := checkTenant(ctx, tenantID); err != nil {
		return false, err
	}
	r, ok := s.rows[fakeRowID(tenantID, p.Key)]
	if !ok || r.claimedUntil.IsZero() || !r.claimedUntil.Equal(claimedUntil.Truncate(time.Microsecond)) {
		return false, nil
	}
	r.graphApplied, r.claimedUntil = r.holds(p), time.Time{}
	return r.graphApplied, nil
}

func (s *fakeVerdictStore) ReleaseClaim(ctx context.Context, tenantID, key string, claimedUntil time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.err["ReleaseClaim"]; err != nil {
		return err
	}
	if err := checkTenant(ctx, tenantID); err != nil {
		return err
	}
	if r, ok := s.rows[fakeRowID(tenantID, key)]; ok && !r.claimedUntil.IsZero() &&
		r.claimedUntil.Equal(claimedUntil.Truncate(time.Microsecond)) {
		r.claimedUntil = time.Time{}
	}
	return nil
}

func (s *fakeVerdictStore) ResetProjection(ctx context.Context, tenantID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.err["ResetProjection"]; err != nil {
		return 0, err
	}
	if err := checkTenant(ctx, tenantID); err != nil {
		return 0, err
	}
	n := 0
	for id, r := range s.rows {
		if strings.HasPrefix(id, tenantID+"\x00") && r.graphApplied &&
			(r.determination == artifacts.DeterminationLLMPanel || r.determination == artifacts.DeterminationHuman) &&
			(r.status == artifacts.VerdictConfirmed || r.status == artifacts.VerdictRejected) {
			r.graphApplied = false
			n++
		}
	}
	return n, nil
}

func (s *fakeVerdictStore) Counts(ctx context.Context, tenantID string, now time.Time) (artifacts.QueueCounts, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.err["Counts"]; err != nil {
		return artifacts.QueueCounts{}, err
	}
	if err := checkTenant(ctx, tenantID); err != nil {
		return artifacts.QueueCounts{}, err
	}
	var c artifacts.QueueCounts
	for id, r := range s.rows {
		if !strings.HasPrefix(id, tenantID+"\x00") {
			continue
		}
		switch r.status {
		case artifacts.VerdictPending:
			if r.determination == artifacts.DeterminationHuman {
				continue
			}
			c.Pending++
			if !r.nextAttemptAt.After(now) && (r.leasedUntil.IsZero() || r.leasedUntil.Before(now)) {
				c.Due++
			}
		case artifacts.VerdictConfirmed, artifacts.VerdictRejected:
			if !r.graphApplied {
				c.Unprojected++
			}
		}
	}
	return c, nil
}
