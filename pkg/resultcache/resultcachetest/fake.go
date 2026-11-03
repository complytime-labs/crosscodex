package resultcachetest

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/resultcache"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// ErrUnknownParent is wrapped by the error a Fake returns for a write that
// names a job or catalog not added for the ctx tenant.
var ErrUnknownParent = errors.New("resultcachetest: job or catalog not added for this tenant")

// jobRunning is the only job status the cache tables accept records for.
const jobRunning = "running"

var _ resultcache.Store = (*Fake)(nil)

type rowKey struct{ tenant, analyzer, key string }

type row struct {
	result                              []byte
	model, job, attempt, source, target string
}

type entryKey struct {
	tenant, job, analyzer, attempt string
	entry                          resultcache.ManifestEntry
}

type statsRow struct {
	tenant, job, analyzer, attempt string
	stats                          resultcache.Stats
}

// Fake fakes resultcache.Store; see the package doc.
type Fake struct {
	ProducersFunc   func(ctx context.Context, analyzer string, keys []string) (map[string]string, error)
	PutFunc         func(ctx context.Context, e resultcache.Entry) error
	RecordStageFunc func(ctx context.Context, rec resultcache.StageRecord) error
	ManifestFunc    func(ctx context.Context, jobID, analyzer, attempt string) (resultcache.Manifest, error)
	// LookupVerifiableFunc, CheckpointFunc and VerifiedSummariesFunc script
	// those methods.
	LookupVerifiableFunc  func(ctx context.Context, analyzer string, keys []string, attempts []resultcache.AttemptRef) (resultcache.Snapshot, error)
	CheckpointFunc        func(ctx context.Context, expected resultcache.Manifest, seal func(ctx context.Context, digest string) error) error
	VerifiedSummariesFunc func(ctx context.Context, jobID string, signed map[string]resultcache.SignedAttempt) ([]resultcache.Summary, error)

	mu       sync.Mutex
	jobLocks map[[2]string]*sync.RWMutex // (tenant, job) row locks: Checkpoint's FOR UPDATE, inserts' FOR SHARE
	jobs     map[[2]string]string        // (tenant, job) -> status
	catalogs map[[2]string]bool
	rows     map[rowKey]row
	entries  map[entryKey]bool
	stats    []statsRow
}

// init allocates the maps; callers hold f.mu.
func (f *Fake) init() {
	if f.rows == nil {
		f.jobs = map[[2]string]string{}
		f.jobLocks = map[[2]string]*sync.RWMutex{}
		f.catalogs = map[[2]string]bool{}
		f.rows = map[rowKey]row{}
		f.entries = map[entryKey]bool{}
	}
}

// AddJob makes jobID a running job of tenantID, as a jobs row would.
func (f *Fake) AddJob(tenantID, jobID string) {
	f.SetJobStatus(tenantID, jobID, jobRunning)
}

// SetJobStatus makes jobID a job of tenantID with status ("running",
// "completed", "failed" or "cancelled"), as the pipeline's status updates
// would. Put and RecordStage refuse a job whose status is not "running".
func (f *Fake) SetJobStatus(tenantID, jobID, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	f.jobs[[2]string{tenantID, jobID}] = status
}

// jobLock returns the lock standing in for (tenantID, jobID)'s jobs row.
func (f *Fake) jobLock(tenantID, jobID string) *sync.RWMutex {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	k := [2]string{tenantID, jobID}
	if f.jobLocks[k] == nil {
		f.jobLocks[k] = &sync.RWMutex{}
	}
	return f.jobLocks[k]
}

// checkRunning returns the error the freeze trigger raises for a row of
// table whose job is known but not running, or nil. Callers hold f.mu.
func (f *Fake) checkRunning(tenantID, jobID, table string) error {
	if status := f.jobs[[2]string{tenantID, jobID}]; status != jobRunning {
		return fmt.Errorf("%w: cannot add a %s row for job %s: its status is %q. "+
			"Result-cache records are written only while the job runs; to recompute, start a new job",
			resultcache.ErrJobNotRunning, table, jobID, status)
	}
	return nil
}

// AddCatalog makes catalogID a catalog of tenantID, as a catalogs row would.
func (f *Fake) AddCatalog(tenantID, catalogID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	f.catalogs[[2]string{tenantID, catalogID}] = true
}

func tenantOf(ctx context.Context) (string, error) {
	id, err := tenant.FromContext(ctx)
	if err != nil {
		return "", fmt.Errorf("resultcachetest: %w: %w", db.ErrTenantRequired, err)
	}
	return id, nil
}

func cloneStats(s resultcache.Stats) resultcache.Stats {
	s.Models = maps.Clone(s.Models)
	return s
}

// Producers returns the job that stored each of keys' rows, unless
// ProducersFunc is set.
func (f *Fake) Producers(ctx context.Context, analyzerName string, keys []string) (map[string]string, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	if f.ProducersFunc != nil {
		return f.ProducersFunc(ctx, analyzerName, keys)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	out := make(map[string]string, len(keys))
	for key, h := range f.hits(tenantID, analyzerName, keys) {
		out[key] = h.ProducerJobID
	}
	return out, nil
}

// hits returns the tenant's rows for keys; callers hold f.mu.
func (f *Fake) hits(tenantID, analyzerName string, keys []string) map[string]resultcache.Hit {
	hits := make(map[string]resultcache.Hit, len(keys))
	for _, key := range keys {
		if r, ok := f.rows[rowKey{tenantID, analyzerName, key}]; ok {
			hits[key] = resultcache.Hit{CacheKey: key, Result: slices.Clone(r.result), ResponseModel: r.model,
				ProducerJobID: r.job, ProducerAttempt: r.attempt}
		}
	}
	return hits
}

// LookupVerifiable reads the rows for keys and the requested attempts of
// their producers under one hold of the fake's lock, which is its snapshot,
// unless LookupVerifiableFunc is set. Like pgstore it takes no job lock, so
// it does not wait on a Checkpoint in progress; that is harmless because a
// checkpoint writes no cache rows, and the snapshot is consistent either
// way.
func (f *Fake) LookupVerifiable(ctx context.Context, analyzerName string, keys []string, attempts []resultcache.AttemptRef) (resultcache.Snapshot, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return resultcache.Snapshot{}, err
	}
	if f.LookupVerifiableFunc != nil {
		return f.LookupVerifiableFunc(ctx, analyzerName, keys, attempts)
	}
	if err := resultcache.ValidAttemptRefs(analyzerName, attempts); err != nil {
		return resultcache.Snapshot{}, fmt.Errorf("resultcachetest: lookup verifiable: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	hits := f.hits(tenantID, analyzerName, keys)
	producers := map[string]bool{}
	for _, h := range hits {
		producers[h.ProducerJobID] = true
	}
	manifests := map[resultcache.AttemptRef]resultcache.Manifest{}
	for _, r := range attempts {
		if producers[r.JobID] {
			manifests[r] = f.manifest(tenantID, r.JobID, analyzerName, r.Attempt)
		}
	}
	return resultcache.NewSnapshot(analyzerName, hits, manifests), nil
}

// Put stores e's row and its write entry together, unless PutFunc is set.
func (f *Fake) Put(ctx context.Context, e resultcache.Entry) error {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return err
	}
	if f.PutFunc != nil {
		return f.PutFunc(ctx, e)
	}
	if err := e.Validate(); err != nil {
		return fmt.Errorf("resultcachetest: put: %w", err)
	}
	l := f.jobLock(tenantID, e.JobID)
	l.RLock()
	defer l.RUnlock()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	if f.jobs[[2]string{tenantID, e.JobID}] == "" || !f.catalogs[[2]string{tenantID, e.SourceCatalogID}] ||
		!f.catalogs[[2]string{tenantID, e.TargetCatalogID}] {
		return fmt.Errorf("resultcachetest: put %s for job %s: %w", e.CacheKey, e.JobID, ErrUnknownParent)
	}
	if err := f.checkRunning(tenantID, e.JobID, "job_cache_entries"); err != nil {
		return err
	}
	f.rows[rowKey{tenantID, e.Analyzer, e.CacheKey}] = row{result: slices.Clone(e.Result), model: e.ResponseModel,
		job: e.JobID, attempt: e.Attempt, source: e.SourceCatalogID, target: e.TargetCatalogID}
	f.entries[entryKey{tenantID, e.JobID, e.Analyzer, e.Attempt, resultcache.ManifestEntry{TaskID: e.TaskID, Role: resultcache.RoleWrite,
		CacheKey: e.CacheKey, ResultDigest: resultcache.ResultDigest(e.Result)}}] = true
	return nil
}

// RecordStage adds rec's hit entries and appends its stats together,
// unless RecordStageFunc is set.
func (f *Fake) RecordStage(ctx context.Context, rec resultcache.StageRecord) error {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return err
	}
	if f.RecordStageFunc != nil {
		return f.RecordStageFunc(ctx, rec)
	}
	if err := rec.Validate(); err != nil {
		return fmt.Errorf("resultcachetest: record stage: %w", err)
	}
	l := f.jobLock(tenantID, rec.JobID)
	l.RLock()
	defer l.RUnlock()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	if f.jobs[[2]string{tenantID, rec.JobID}] == "" {
		return fmt.Errorf("resultcachetest: record stage for job %s: %w", rec.JobID, resultcache.ErrUnknownJob)
	}
	// pgstore inserts the hit entries first, so their table names the refusal.
	table := "job_cache_stats"
	if len(rec.Hits) > 0 {
		table = "job_cache_entries"
	}
	if err := f.checkRunning(tenantID, rec.JobID, table); err != nil {
		return err
	}
	for _, h := range rec.Hits {
		f.entries[entryKey{tenantID, rec.JobID, rec.Analyzer, rec.Attempt, resultcache.ManifestEntry{TaskID: h.TaskID, Role: resultcache.RoleHit,
			CacheKey: h.CacheKey, ResultDigest: h.ResultDigest, ProducerJobID: h.ProducerJobID}}] = true
	}
	f.stats = append(f.stats, statsRow{tenant: tenantID, job: rec.JobID, analyzer: rec.Analyzer, attempt: rec.Attempt, stats: cloneStats(rec.Stats)})
	return nil
}

// Manifest returns the entries and stats attempt of jobID recorded for
// analyzer, unless ManifestFunc is set.
func (f *Fake) Manifest(ctx context.Context, jobID, analyzerName, attempt string) (resultcache.Manifest, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return resultcache.Manifest{}, err
	}
	if f.ManifestFunc != nil {
		return f.ManifestFunc(ctx, jobID, analyzerName, attempt)
	}
	if err := resultcache.ValidAttempt(attempt); err != nil {
		return resultcache.Manifest{}, fmt.Errorf("resultcachetest: manifest: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	return f.manifest(tenantID, jobID, analyzerName, attempt), nil
}

// manifest returns one attempt's manifest; callers hold f.mu.
func (f *Fake) manifest(tenantID, jobID, analyzerName, attempt string) resultcache.Manifest {
	m := resultcache.Manifest{TenantID: tenantID, JobID: jobID, Analyzer: analyzerName, Attempt: attempt}
	for ek := range f.entries {
		if ek.tenant == tenantID && ek.job == jobID && ek.analyzer == analyzerName && ek.attempt == attempt {
			m.Entries = append(m.Entries, ek.entry)
		}
	}
	slices.SortFunc(m.Entries, resultcache.CompareEntries)
	for _, s := range f.stats {
		if s.tenant == tenantID && s.job == jobID && s.analyzer == analyzerName && s.attempt == attempt {
			m.Stats = append(m.Stats, cloneStats(s.stats))
		}
	}
	return m
}

// Checkpoint holds the job's lock exclusively, so Put and RecordStage for
// the job wait, while it compares the attempt's rows with expected and, if
// they match, runs seal with expected's digest; unless CheckpointFunc is
// set. seal must not call the fake for the same job.
func (f *Fake) Checkpoint(ctx context.Context, expected resultcache.Manifest, seal func(ctx context.Context, digest string) error) error {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return err
	}
	if f.CheckpointFunc != nil {
		return f.CheckpointFunc(ctx, expected, seal)
	}
	if err := resultcache.ValidCheckpoint(tenantID, expected); err != nil {
		return fmt.Errorf("resultcachetest: checkpoint: %w", err)
	}
	l := f.jobLock(tenantID, expected.JobID)
	l.Lock()
	defer l.Unlock()
	f.mu.Lock()
	if f.jobs[[2]string{tenantID, expected.JobID}] == "" {
		f.mu.Unlock()
		return fmt.Errorf("resultcachetest: checkpoint job %s: %w", expected.JobID, resultcache.ErrUnknownJob)
	}
	stored := f.manifest(tenantID, expected.JobID, expected.Analyzer, expected.Attempt)
	f.mu.Unlock()
	if err := resultcache.MatchRecord(expected, stored); err != nil {
		return fmt.Errorf("resultcachetest: checkpoint: %w", err)
	}
	if err := seal(ctx, expected.Digest()); err != nil {
		return fmt.Errorf("resultcachetest: checkpoint job %s attempt %s: %w", expected.JobID, expected.Attempt, err)
	}
	return nil
}

// VerifiedSummaries summarizes each signed attempt of jobID from its rows
// (resultcache.SummarizeSigned), unless VerifiedSummariesFunc is set.
func (f *Fake) VerifiedSummaries(ctx context.Context, jobID string, signed map[string]resultcache.SignedAttempt) ([]resultcache.Summary, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	if f.VerifiedSummariesFunc != nil {
		return f.VerifiedSummariesFunc(ctx, jobID, signed)
	}
	for a, sa := range signed {
		if err := resultcache.ValidAttempt(sa.Attempt); err != nil {
			return nil, fmt.Errorf("resultcachetest: verified summaries: analyzer %s: %w", a, err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	manifests := make(map[string]resultcache.Manifest, len(signed))
	for a, sa := range signed {
		manifests[a] = f.manifest(tenantID, jobID, a, sa.Attempt)
	}
	return resultcache.SummarizeSigned(manifests, signed), nil
}
