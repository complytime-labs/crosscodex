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
// semantics as PGVerdictStore. Tests inspect rows with row and set them up
// with set, setHuman and resetProjected; err injects a failure per method
// name. enqueued records the candidates of every Enqueue call.
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
	confidence    float64
	evidence      artifacts.PanelEvidence
	dissent       *artifacts.Verdict
	graphApplied  bool
	attempts      int
	nextAttemptAt time.Time
	leasedUntil   time.Time // zero: not leased
	lastError     string
	decidedAt     time.Time
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

// resetProjected marks every decided row of the tenant unprojected, as the
// documented post-rebuild SQL step does.
func (s *fakeVerdictStore) resetProjected(tenantID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, r := range s.rows {
		if strings.HasPrefix(id, tenantID+"\x00") {
			r.graphApplied = false
		}
	}
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
			determination: artifacts.DeterminationTokenOverlap, nextAttemptAt: now,
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
		return artifacts.Decision{}, fmt.Errorf("decide %s: status %q is not a panel verdict", key, v.Status)
	}
	r, ok := s.rows[fakeRowID(tenantID, key)]
	switch {
	case !ok:
		return artifacts.Decision{}, fmt.Errorf("decide %s: pair is not queued", key)
	case r.determination == artifacts.DeterminationHuman:
		return artifacts.Decision{Outcome: artifacts.DecideHumanOwned}, nil
	case r.status != artifacts.VerdictPending:
		return artifacts.Decision{Outcome: artifacts.DecideStale}, nil
	}
	r.status, r.determination = v.Status, artifacts.DeterminationLLMPanel
	r.confidence, r.evidence, r.decidedAt = v.Confidence, v.Evidence, v.DecidedAt
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
		r.dissent = &v
	}
	return nil
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
		r.status, r.decidedAt = artifacts.VerdictAbandoned, f.At
	}
	return nil
}

func (s *fakeVerdictStore) Unprojected(ctx context.Context, tenantID string, n int) ([]artifacts.Projection, error) {
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
		if strings.HasPrefix(id, tenantID+"\x00") && !r.graphApplied && r.determination != artifacts.DeterminationHuman &&
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
		out = append(out, artifacts.Projection{
			Key: r.key, LowGroupID: r.cand.LowGroupID, HighGroupID: r.cand.HighGroupID,
			CandidateEdgeID: r.cand.CandidateEdgeID, Status: r.status, Confidence: r.confidence,
			Evidence: r.evidence, DecidedAt: r.decidedAt,
		})
	}
	return out, nil
}

func (s *fakeVerdictStore) MarkProjected(ctx context.Context, tenantID, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.err["MarkProjected"]; err != nil {
		return err
	}
	if err := checkTenant(ctx, tenantID); err != nil {
		return err
	}
	if r, ok := s.rows[fakeRowID(tenantID, key)]; ok && r.determination != artifacts.DeterminationHuman &&
		(r.status == artifacts.VerdictConfirmed || r.status == artifacts.VerdictRejected) {
		r.graphApplied = true
	}
	return nil
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
		if !strings.HasPrefix(id, tenantID+"\x00") || r.determination == artifacts.DeterminationHuman {
			continue
		}
		switch r.status {
		case artifacts.VerdictPending:
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
