package feedback_test

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/complytime-labs/crosscodex/internal/feedback"
	"github.com/complytime-labs/crosscodex/pkg/cleaning"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// Reviewer statuses fakeStore.Reviewers holds, as reviewers.status does.
const (
	reviewerActive  = "active"
	reviewerDeleted = "deleted"
)

// jobSeed is a job a spec seeds: fakeStore.Jobs holds them, and the
// integration suite inserts them into jobs and vote_summaries. Mappings' IDs
// and JobIDs are ignored: stores derive them.
type jobSeed struct {
	ID, Status, CreatedBy string
	Mappings              []feedback.Mapping
}

var _ feedback.Store = (*fakeStore)(nil)

// fakeStore is the in-memory feedback.Store for unit specs. It lives here,
// not in a shared fake package, because the interface is in this package.
// storeContract (contract_test.go) runs against it and, in the integration
// suite, against PGStore, so the two refuse and return the same things. It
// mirrors the database: job reads are tenant-wide whatever user ctx carries,
// as PGStore's are, times are truncated to the microsecond, and no-edge and no-evidence values read back as nil.
//
// The zero value is ready to use. Set Reviewers and Jobs before the first
// call; the methods are then safe for concurrent use. To intercept a few
// methods, embed *fakeStore and override them.
type fakeStore struct {
	// Reviewers maps a tenant to its reviewers' IDs and statuses
	// (reviewerActive or reviewerDeleted).
	Reviewers map[string]map[string]string
	// Jobs maps a tenant to its jobs.
	Jobs map[string][]jobSeed

	mu             sync.Mutex
	votes          map[string][]feedback.Vote            // tenant -> votes in insertion order
	determinations map[string][]feedback.Determination   // tenant -> determinations in insertion order
	reviews        map[string]map[string]feedback.Review // tenant -> mapping ID -> review
}

// checkTenant mirrors PGStore's inTx: ctx must carry a tenant equal to
// tenantID.
func checkTenant(ctx context.Context, tenantID string) error {
	ctxTenant, err := tenant.FromContext(ctx)
	if err != nil {
		return fmt.Errorf("feedback store: %w", err)
	}
	if ctxTenant != tenantID {
		return fmt.Errorf("feedback store: context tenant %q does not match tenant %q", ctxTenant, tenantID)
	}
	return nil
}

// init allocates the maps on first use. Callers hold f.mu.
func (f *fakeStore) init() {
	if f.reviews == nil {
		f.votes = map[string][]feedback.Vote{}
		f.determinations = map[string][]feedback.Determination{}
		f.reviews = map[string]map[string]feedback.Review{}
	}
}

// job returns the tenant's completed job jobID, refused as PGStore's
// requireCompleted refuses it. Like PGStore it ignores ctx's user: job reads
// are tenant-wide. Callers hold f.mu.
func (f *fakeStore) job(op, tenantID, jobID string) (jobSeed, error) {
	for _, j := range f.Jobs[tenantID] {
		if j.ID != jobID {
			continue
		}
		if j.Status != "completed" {
			return jobSeed{}, &feedback.JobNotCompletedError{JobID: jobID, Status: j.Status}
		}
		return j, nil
	}
	return jobSeed{}, fmt.Errorf("%s: job %s: %w", op, jobID, feedback.ErrJobNotFound)
}

// requireMapping mirrors PGStore's requireMapping. Callers hold f.mu.
func (f *fakeStore) requireMapping(op, tenantID, mappingID, jobID, sourceID, targetID string) error {
	j, err := f.job(op, tenantID, jobID)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(j.Mappings, func(m feedback.Mapping) bool { return m.SourceID == sourceID && m.TargetID == targetID }) {
		return fmt.Errorf("%s: %w", op, feedback.MappingNotFound(mappingID, jobID))
	}
	return nil
}

func (f *fakeStore) JobMappings(ctx context.Context, tenantID, jobID string) ([]feedback.Mapping, error) {
	if err := checkTenant(ctx, tenantID); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	j, err := f.job("job mappings", tenantID, jobID)
	if err != nil {
		return nil, err
	}
	out := slices.Clone(j.Mappings)
	for i := range out {
		out[i].JobID = jobID
		out[i].ID = feedback.MappingID(jobID, out[i].SourceID, out[i].TargetID)
	}
	slices.SortFunc(out, func(a, b feedback.Mapping) int {
		return cmp.Or(cmp.Compare(a.SourceID, b.SourceID), cmp.Compare(a.TargetID, b.TargetID))
	})
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// cloneVote returns v as the database would hand it back.
func cloneVote(v feedback.Vote) feedback.Vote {
	v.Evidence = maps.Clone(v.Evidence)
	if len(v.Evidence) == 0 {
		v.Evidence = nil
	}
	v.CreatedAt = v.CreatedAt.Truncate(time.Microsecond).UTC()
	return v
}

func (f *fakeStore) RecordVote(ctx context.Context, tenantID string, v feedback.Vote) error {
	if err := v.Validate(); err != nil {
		return err
	}
	if err := checkTenant(ctx, tenantID); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	if err := f.requireMapping("record vote", tenantID, v.MappingID, v.JobID, v.SourceID, v.TargetID); err != nil {
		return err
	}
	if slices.ContainsFunc(f.votes[tenantID], func(s feedback.Vote) bool { return s.ID == v.ID }) {
		return fmt.Errorf("vote %s is already recorded; record a new vote with a new ID; nothing was recorded", v.ID)
	}
	f.votes[tenantID] = append(f.votes[tenantID], cloneVote(v))
	return nil
}

func (f *fakeStore) LatestVotes(ctx context.Context, tenantID, jobID string) ([]feedback.Vote, error) {
	if err := checkTenant(ctx, tenantID); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	type key struct{ mapping, voter string }
	latest := map[key]feedback.Vote{}
	for _, v := range f.votes[tenantID] {
		if v.JobID != jobID {
			continue
		}
		k := key{v.MappingID, v.Voter}
		cur, ok := latest[k]
		if !ok || v.CreatedAt.After(cur.CreatedAt) || (v.CreatedAt.Equal(cur.CreatedAt) && v.ID > cur.ID) {
			latest[k] = v
		}
	}
	var out []feedback.Vote
	for _, v := range latest {
		out = append(out, cloneVote(v))
	}
	slices.SortFunc(out, func(a, b feedback.Vote) int {
		return cmp.Or(cmp.Compare(a.MappingID, b.MappingID), cmp.Compare(a.Voter, b.Voter))
	})
	return out, nil
}

// cloneReview returns r as the database would hand it back.
func cloneReview(r feedback.Review) feedback.Review {
	r.EdgeIDs = slices.Clone(r.EdgeIDs)
	if len(r.EdgeIDs) == 0 {
		r.EdgeIDs = nil
	}
	r.DecidedAt = r.DecidedAt.Truncate(time.Microsecond).UTC()
	return r
}

// cloneDetermination returns d as the database would hand it back.
func cloneDetermination(d feedback.Determination) feedback.Determination {
	d.Request, d.Attestation = slices.Clone(d.Request), slices.Clone(d.Attestation)
	d.DecidedAt = d.DecidedAt.Truncate(time.Microsecond).UTC()
	return d
}

func (f *fakeStore) Review(ctx context.Context, tenantID, mappingID string) (feedback.Review, bool, error) {
	if err := checkTenant(ctx, tenantID); err != nil {
		return feedback.Review{}, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	r, ok := f.reviews[tenantID][mappingID]
	if !ok {
		return feedback.Review{}, false, nil
	}
	return cloneReview(r), true, nil
}

// insertDetermination mirrors PGStore's insertDetermination. Callers hold
// f.mu.
func (f *fakeStore) insertDetermination(tenantID string, d feedback.Determination) error {
	if slices.ContainsFunc(f.determinations[tenantID], func(s feedback.Determination) bool { return s.ID == d.ID }) {
		return fmt.Errorf("determination %s is already recorded: a determination ID is the hash of its request, so this request was already applied; nothing was recorded", d.ID)
	}
	f.determinations[tenantID] = append(f.determinations[tenantID], cloneDetermination(d))
	return nil
}

// setReview stores r for the tenant. Callers hold f.mu.
func (f *fakeStore) setReview(tenantID string, r feedback.Review) {
	if f.reviews[tenantID] == nil {
		f.reviews[tenantID] = map[string]feedback.Review{}
	}
	f.reviews[tenantID][r.MappingID] = cloneReview(r)
}

func (f *fakeStore) Determine(ctx context.Context, tenantID string, d feedback.Determination, expectPrev string, next feedback.Review) error {
	if err := feedback.CheckDetermine(d, next); err != nil {
		return err
	}
	if err := feedback.CheckEdgeIDs("determine mapping", d.MappingID, next.EdgeIDs); err != nil {
		return err
	}
	if err := checkTenant(ctx, tenantID); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	switch status, ok := f.Reviewers[tenantID][d.DecidedBy]; {
	case !ok:
		return fmt.Errorf("determine mapping %s: reviewer %q: %w", d.MappingID, d.DecidedBy, cleaning.ErrReviewerNotRegistered)
	case status == reviewerDeleted:
		return fmt.Errorf("determine mapping %s: %w", d.MappingID, cleaning.ErrReviewerDeleted)
	case status != reviewerActive:
		return fmt.Errorf("determine mapping %s: fakeStore.Reviewers holds status %q for %q; use %q or %q",
			d.MappingID, status, d.DecidedBy, reviewerActive, reviewerDeleted)
	}
	if err := f.requireMapping("determine mapping", tenantID, d.MappingID, d.JobID, d.SourceID, d.TargetID); err != nil {
		return err
	}
	current := f.reviews[tenantID][d.MappingID]
	if current.DeterminationID != expectPrev {
		return feedback.ReviewChanged(d.MappingID, current.DeterminationID)
	}
	if err := f.insertDetermination(tenantID, d); err != nil {
		return fmt.Errorf("determine mapping %s: %w", d.MappingID, err)
	}
	next.GraphApplied = false
	f.setReview(tenantID, next)
	return nil
}

func (f *fakeStore) MarkStale(ctx context.Context, tenantID string, d feedback.Determination, expectPrev string) error {
	if err := feedback.CheckStale(d); err != nil {
		return err
	}
	if err := checkTenant(ctx, tenantID); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	current, ok := f.reviews[tenantID][d.MappingID]
	switch {
	case !ok:
		return fmt.Errorf("mark mapping %s stale: %w", d.MappingID, feedback.ErrReviewNotFound)
	case current.DeterminationID != expectPrev:
		return feedback.ReviewChanged(d.MappingID, current.DeterminationID)
	}
	if err := f.insertDetermination(tenantID, d); err != nil {
		return fmt.Errorf("mark mapping %s stale: %w", d.MappingID, err)
	}
	current.Status, current.DeterminationID, current.DecidedAt, current.GraphApplied = feedback.ReviewStale, d.ID, d.DecidedAt, true
	f.setReview(tenantID, current)
	return nil
}

func (f *fakeStore) MarkReprojected(ctx context.Context, tenantID, mappingID, expectDetermination string, edgeIDs []string) error {
	if err := feedback.CheckEdgeIDs("re-project mapping", mappingID, edgeIDs); err != nil {
		return err
	}
	if err := checkTenant(ctx, tenantID); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	current, ok := f.reviews[tenantID][mappingID]
	switch {
	case !ok:
		return fmt.Errorf("re-project mapping %s: %w", mappingID, feedback.ErrReviewNotFound)
	case current.DeterminationID != expectDetermination:
		return feedback.ReviewChanged(mappingID, current.DeterminationID)
	case !current.GraphApplied:
		return feedback.Unprojected(mappingID)
	}
	current.EdgeIDs = edgeIDs
	f.setReview(tenantID, current)
	return nil
}

// sortedReviews returns copies of the tenant's reviews keep accepts, sorted
// by mapping ID. Callers hold f.mu.
func (f *fakeStore) sortedReviews(tenantID string, keep func(feedback.Review) bool) []feedback.Review {
	var out []feedback.Review
	for _, r := range f.reviews[tenantID] {
		if keep(r) {
			out = append(out, cloneReview(r))
		}
	}
	slices.SortFunc(out, func(a, b feedback.Review) int { return cmp.Compare(a.MappingID, b.MappingID) })
	return out
}

func (f *fakeStore) Unprojected(ctx context.Context, tenantID string) ([]feedback.Review, error) {
	if err := checkTenant(ctx, tenantID); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	return f.sortedReviews(tenantID, func(r feedback.Review) bool { return !r.GraphApplied }), nil
}

func (f *fakeStore) Reviews(ctx context.Context, tenantID string) ([]feedback.Review, error) {
	if err := checkTenant(ctx, tenantID); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	return f.sortedReviews(tenantID, func(feedback.Review) bool { return true }), nil
}

func (f *fakeStore) MarkProjected(ctx context.Context, tenantID, mappingID, expectDetermination string, edgeIDs []string) error {
	if err := feedback.CheckEdgeIDs("mark mapping projected", mappingID, edgeIDs); err != nil {
		return err
	}
	if err := checkTenant(ctx, tenantID); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	current, ok := f.reviews[tenantID][mappingID]
	switch {
	case !ok:
		return fmt.Errorf("mark mapping %s projected: %w", mappingID, feedback.ErrReviewNotFound)
	case current.DeterminationID != expectDetermination:
		return feedback.ReviewChanged(mappingID, current.DeterminationID)
	}
	current.EdgeIDs, current.GraphApplied = edgeIDs, true
	f.setReview(tenantID, current)
	return nil
}

// Determinations returns the tenant's determinations in insertion order:
// the store has no method that reads them, so specs read them here, as an
// auditor reads mapping_determinations directly.
func (f *fakeStore) Determinations(tenantID string) []feedback.Determination {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	out := make([]feedback.Determination, 0, len(f.determinations[tenantID]))
	for _, d := range f.determinations[tenantID] {
		out = append(out, cloneDetermination(d))
	}
	return out
}
