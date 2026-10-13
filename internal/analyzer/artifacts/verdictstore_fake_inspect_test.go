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

// setProjected marks an existing row as written to the graph.
func (s *fakeVerdictStore) setProjected(tenantID, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[fakeRowID(tenantID, key)].graphApplied = true
}

// setUnprojected queues an existing row for projection again, as
// ResetProjection does for every projected row.
func (s *fakeVerdictStore) setUnprojected(tenantID, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[fakeRowID(tenantID, key)].graphApplied = false
}

// setHuman makes an existing row human-owned with the given status and
// queues it for projection, the row effects of DecideHuman. Specs use it for
// states DecideHuman cannot produce, such as a human-owned pending row. It
// leaves decided_at as it is, so a human confirmed or rejected row it makes
// cannot be projected (the graph needs a time); use DecideHuman for those.
func (s *fakeVerdictStore) setHuman(tenantID, key string, status artifacts.VerdictStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.rows[fakeRowID(tenantID, key)]
	r.status, r.determination, r.determinedBy = status, artifacts.DeterminationHuman, "test-reviewer"
	r.graphApplied = false
}
