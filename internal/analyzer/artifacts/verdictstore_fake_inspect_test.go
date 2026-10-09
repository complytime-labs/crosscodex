//go:build !integration

package artifacts_test

import (
	"slices"
	"strings"

	"github.com/complytime-labs/crosscodex/internal/analyzer/artifacts"
)

// row returns a copy of the stored row, or nil.
func (s *fakeVerdictStore) row(tenantID, key string) *fakeVerdictRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[fakeRowID(tenantID, key)]
	if !ok {
		return nil
	}
	c := *r
	return &c
}

// keys returns the tenant's pair keys, sorted.
func (s *fakeVerdictStore) keys(tenantID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for id, r := range s.rows {
		if strings.HasPrefix(id, tenantID+"\x00") {
			out = append(out, r.key)
		}
	}
	slices.Sort(out)
	return out
}

// set stores a row for cand with the given status and determination,
// simulating a verdict written by a human or an earlier run.
func (s *fakeVerdictStore) set(tenantID string, cand artifacts.PairCandidate, status artifacts.VerdictStatus, determination string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := artifacts.PairKey(cand.LowGroupID, cand.HighGroupID)
	r, ok := s.rows[fakeRowID(tenantID, key)]
	if !ok {
		r = &fakeVerdictRow{key: key, cand: cand}
		s.rows[fakeRowID(tenantID, key)] = r
	}
	r.status, r.determination = status, determination
}

// setHuman records a human verdict on an existing row, as the human verdict
// API (a follow-up issue) will.
func (s *fakeVerdictStore) setHuman(tenantID, key string, status artifacts.VerdictStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.rows[fakeRowID(tenantID, key)]
	r.status, r.determination = status, artifacts.DeterminationHuman
}
