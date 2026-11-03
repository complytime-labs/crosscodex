// Package pgstore is the PostgreSQL resultcache.Store, over the
// llm_result_cache, job_cache_entries and job_cache_stats tables of
// migration 001. Every operation runs in one transaction of a
// db.TenantConnection, as the restricted app_user: RLS scopes it to the
// tenant in ctx, and app_user can only SELECT and INSERT the audit tables.
// Every value is a bound parameter; no SQL text is built from input.
package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/resultcache"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

const (
	lookupSQL = `SELECT c.cache_key, c.result, c.response_model, c.job_id
FROM llm_result_cache c
WHERE c.tenant_id = $1 AND c.analyzer = $2 AND c.cache_key = ANY($3::text[])`

	attemptEntriesSQL = `SELECT e.job_id, e.attempt, e.task_id, e.role, e.cache_key, e.result_digest, e.producer_job_id
FROM job_cache_entries e
JOIN unnest($3::text[], $4::text[]) AS r(job_id, attempt) ON r.job_id = e.job_id AND r.attempt = e.attempt
WHERE e.tenant_id = $1 AND e.analyzer = $2`

	attemptStatsSQL = `SELECT s.job_id, s.attempt, s.mode, s.reason, s.models::text, s.tasks, s.hits, s.dispatched
FROM job_cache_stats s
JOIN unnest($3::text[], $4::text[]) AS r(job_id, attempt) ON r.job_id = s.job_id AND r.attempt = s.attempt
WHERE s.tenant_id = $1 AND s.analyzer = $2
ORDER BY s.seq`

	// lockJobSQL is Checkpoint's lock: FOR UPDATE conflicts with the FOR
	// SHARE require_running_job_for_cache_record takes for every insert.
	lockJobSQL = `SELECT job_id FROM jobs WHERE tenant_id = $1 AND job_id = $2 FOR UPDATE`

	putRowSQL = `INSERT INTO llm_result_cache
  (tenant_id, analyzer, cache_key, result, response_model, job_id, source_catalog_id, target_catalog_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (tenant_id, analyzer, cache_key) DO UPDATE SET
  result = EXCLUDED.result, response_model = EXCLUDED.response_model, job_id = EXCLUDED.job_id,
  source_catalog_id = EXCLUDED.source_catalog_id, target_catalog_id = EXCLUDED.target_catalog_id,
  created_at = now()`

	putWriteSQL = `INSERT INTO job_cache_entries
  (tenant_id, job_id, analyzer, attempt, task_id, cache_key, result_digest, role, producer_job_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, 'write', '')
ON CONFLICT DO NOTHING`

	hitsSQL = `INSERT INTO job_cache_entries
  (tenant_id, job_id, analyzer, attempt, task_id, cache_key, result_digest, role, producer_job_id)
SELECT $1, $2, $3, $4, h.task_id, h.cache_key, h.result_digest, 'hit', h.producer_job_id
FROM unnest($5::text[], $6::text[], $7::text[], $8::text[]) AS h(task_id, cache_key, result_digest, producer_job_id)
ON CONFLICT DO NOTHING`

	statsSQL = `INSERT INTO job_cache_stats (tenant_id, job_id, analyzer, attempt, mode, reason, models, tasks, hits, dispatched)
VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8, $9, $10)`

	manifestEntriesSQL = `SELECT task_id, role, cache_key, result_digest, producer_job_id
FROM job_cache_entries WHERE tenant_id = $1 AND job_id = $2 AND analyzer = $3 AND attempt = $4`

	manifestStatsSQL = `SELECT mode, reason, models::text, tasks, hits, dispatched
FROM job_cache_stats WHERE tenant_id = $1 AND job_id = $2 AND analyzer = $3 AND attempt = $4 ORDER BY seq`
)

// Store is the PostgreSQL resultcache.Store. It is safe for concurrent use.
type Store struct {
	conn       db.TenantConnection
	tracer     trace.Tracer
	ops        metric.Int64Counter
	duration   metric.Float64Histogram
	lookupRows metric.Int64Gauge
}

var _ resultcache.Store = (*Store)(nil)

// Option configures a Store.
type Option func(*Store) error

// WithTelemetry instruments the store: a span per operation with tenant,
// operation and analyzer attributes, resultcache.store.operations.total
// (by operation and outcome), resultcache.store.duration_ms and
// resultcache.store.lookup.rows (rows the latest lookup returned). There is
// no config field: the caller passes the providers it instruments the rest
// of the daemon with. A nil provider leaves that half uninstrumented.
func WithTelemetry(tp trace.TracerProvider, mp metric.MeterProvider) Option {
	return func(s *Store) error {
		if tp != nil {
			s.tracer = tp.Tracer("crosscodex/pkg/resultcache/pgstore")
		}
		if mp == nil {
			return nil
		}
		meter := mp.Meter("crosscodex")
		var err error
		if s.ops, err = meter.Int64Counter("resultcache.store.operations.total",
			metric.WithDescription("Result cache store operations, by operation and outcome")); err != nil {
			return fmt.Errorf("create resultcache.store.operations.total: %w", err)
		}
		if s.duration, err = meter.Float64Histogram("resultcache.store.duration_ms",
			metric.WithDescription("Result cache store operation duration"), metric.WithUnit("ms")); err != nil {
			return fmt.Errorf("create resultcache.store.duration_ms: %w", err)
		}
		if s.lookupRows, err = meter.Int64Gauge("resultcache.store.lookup.rows",
			metric.WithDescription("Rows the latest result cache lookup returned")); err != nil {
			return fmt.Errorf("create resultcache.store.lookup.rows: %w", err)
		}
		return nil
	}
}

// New returns a Store over conn, which must be a tenant pool of the
// restricted app_user (db.NewTenantPool).
func New(conn db.TenantConnection, opts ...Option) (*Store, error) {
	s := &Store{conn: conn}
	for _, opt := range opts {
		if err := opt(s); err != nil {
			return nil, fmt.Errorf("resultcache pgstore: %w", err)
		}
	}
	return s, nil
}

// start opens the span for one operation and reads the tenant; the
// returned finish records the operation's outcome and ends the span. With
// no tenant in ctx it finishes the operation itself and returns an error
// wrapping db.ErrTenantRequired.
func (s *Store) start(ctx context.Context, op string, attrs ...attribute.KeyValue) (context.Context, string, func(error), error) {
	begun := time.Now()
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "resultcache."+op)
	finish := func(err error) {
		outcome := "ok"
		if err != nil {
			outcome = "error"
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		} else {
			span.SetStatus(codes.Ok, "")
		}
		set := metric.WithAttributes(attribute.String("operation", op), attribute.String("outcome", outcome))
		if s.ops != nil {
			s.ops.Add(ctx, 1, set)
		}
		if s.duration != nil {
			s.duration.Record(ctx, float64(time.Since(begun))/float64(time.Millisecond), set)
		}
		span.End()
	}
	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		err = fmt.Errorf("resultcache %s: %w: %w", op, db.ErrTenantRequired, err)
		finish(err)
		return ctx, "", nil, err
	}
	span.SetAttributes(append(attrs, attribute.String("tenant.id", tenantID), attribute.String("operation", op))...)
	return ctx, tenantID, finish, nil
}

// Producers implements resultcache.Store.
func (s *Store) Producers(ctx context.Context, analyzerName string, keys []string) (producers map[string]string, err error) {
	ctx, tenantID, finish, err := s.start(ctx, "producers", attribute.String("analyzer.name", analyzerName), attribute.Int("keys", len(keys)))
	if err != nil {
		return nil, err
	}
	defer func() { finish(err) }()
	producers = make(map[string]string, len(keys))
	if len(keys) == 0 {
		return producers, nil
	}
	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("resultcache producers: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	hits := map[string]resultcache.Hit{}
	if err := readHits(ctx, tx, tenantID, analyzerName, keys, hits); err != nil {
		return nil, fmt.Errorf("resultcache producers: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("resultcache producers: commit: %w", err)
	}
	for key, h := range hits {
		producers[key] = h.ProducerJobID
	}
	if s.lookupRows != nil {
		s.lookupRows.Record(ctx, int64(len(producers)))
	}
	return producers, nil
}

// readHits adds the cached rows of analyzer under keys to hits.
func readHits(ctx context.Context, tx db.Transaction, tenantID, analyzerName string, keys []string, hits map[string]resultcache.Hit) error {
	rows, err := tx.Query(ctx, lookupSQL, tenantID, analyzerName, keys)
	if err != nil {
		return fmt.Errorf("query cached rows: %w", err)
	}
	for rows.Next() {
		var key, result, model, producer string
		if err := rows.Scan(&key, &result, &model, &producer); err != nil {
			return errors.Join(fmt.Errorf("scan cached row: %w", err), rows.Close())
		}
		hits[key] = resultcache.Hit{CacheKey: key, Result: []byte(result), ResponseModel: model, ProducerJobID: producer}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("read cached rows: %w", err)
	}
	return nil
}

// LookupVerifiable implements resultcache.Store. Its transaction is
// REPEATABLE READ, set at BEGIN (db.WithIsolation) because the tenant scope
// runs a statement first, which fixes the one snapshot every later read
// uses. That alone makes the result fail closed: a row planted in a signed
// attempt before the snapshot is in the manifest read here, which then
// mismatches its signature, and one committed later is invisible. No lock
// is taken, so concurrent updates of the producers' jobs never fail it.
func (s *Store) LookupVerifiable(ctx context.Context, analyzerName string, keys []string, attempts []resultcache.AttemptRef) (snap resultcache.Snapshot, err error) {
	ctx, tenantID, finish, err := s.start(ctx, "lookup_verifiable", attribute.String("analyzer.name", analyzerName),
		attribute.Int("keys", len(keys)), attribute.Int("attempts", len(attempts)))
	if err != nil {
		return snap, err
	}
	defer func() { finish(err) }()
	if err := resultcache.ValidAttemptRefs(analyzerName, attempts); err != nil {
		return snap, fmt.Errorf("resultcache lookup verifiable: %w", err)
	}
	hits := map[string]resultcache.Hit{}
	manifests := map[resultcache.AttemptRef]resultcache.Manifest{}
	if len(keys) == 0 {
		return resultcache.NewSnapshot(analyzerName, hits, manifests), nil
	}
	tx, err := s.conn.Begin(db.WithIsolation(ctx, db.IsolationRepeatableRead))
	if err != nil {
		return snap, fmt.Errorf("resultcache lookup verifiable: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := readHits(ctx, tx, tenantID, analyzerName, keys, hits); err != nil {
		return snap, fmt.Errorf("resultcache lookup verifiable: %w", err)
	}
	producers := map[string]bool{}
	for _, h := range hits {
		producers[h.ProducerJobID] = true
	}
	var jobs, tries []string
	for _, r := range attempts {
		if _, dup := manifests[r]; dup || !producers[r.JobID] {
			continue
		}
		manifests[r] = resultcache.Manifest{TenantID: tenantID, JobID: r.JobID, Analyzer: analyzerName, Attempt: r.Attempt}
		jobs, tries = append(jobs, r.JobID), append(tries, r.Attempt)
	}
	if len(jobs) > 0 {
		if err := readAttempts(ctx, tx, tenantID, analyzerName, jobs, tries, manifests); err != nil {
			return snap, fmt.Errorf("resultcache lookup verifiable: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return snap, fmt.Errorf("resultcache lookup verifiable: commit: %w", err)
	}
	if s.lookupRows != nil {
		s.lookupRows.Record(ctx, int64(len(hits)))
	}
	return resultcache.NewSnapshot(analyzerName, hits, manifests), nil
}

// readAttempts fills the manifests of the (jobs[i], attempts[i]) pairs,
// which manifests already holds empty.
func readAttempts(ctx context.Context, tx db.Transaction, tenantID, analyzerName string, jobs, attempts []string,
	manifests map[resultcache.AttemptRef]resultcache.Manifest) error {
	rows, err := tx.Query(ctx, attemptEntriesSQL, tenantID, analyzerName, jobs, attempts)
	if err != nil {
		return fmt.Errorf("query entries: %w", err)
	}
	for rows.Next() {
		r := resultcache.AttemptRef{Analyzer: analyzerName}
		var e resultcache.ManifestEntry
		if err := rows.Scan(&r.JobID, &r.Attempt, &e.TaskID, &e.Role, &e.CacheKey, &e.ResultDigest, &e.ProducerJobID); err != nil {
			return errors.Join(fmt.Errorf("scan entry: %w", err), rows.Close())
		}
		m := manifests[r]
		m.Entries = append(m.Entries, e)
		manifests[r] = m
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("read entries: %w", err)
	}
	rows, err = tx.Query(ctx, attemptStatsSQL, tenantID, analyzerName, jobs, attempts)
	if err != nil {
		return fmt.Errorf("query stats: %w", err)
	}
	for rows.Next() {
		r := resultcache.AttemptRef{Analyzer: analyzerName}
		st, err := scanStats(rows, &r.JobID, &r.Attempt)
		if err != nil {
			return errors.Join(fmt.Errorf("scan stats: %w", err), rows.Close())
		}
		m := manifests[r]
		m.Stats = append(m.Stats, st)
		manifests[r] = m
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("read stats: %w", err)
	}
	for r, m := range manifests {
		slices.SortFunc(m.Entries, resultcache.CompareEntries)
		manifests[r] = m
	}
	return nil
}

// Put implements resultcache.Store.
func (s *Store) Put(ctx context.Context, e resultcache.Entry) (err error) {
	ctx, tenantID, finish, err := s.start(ctx, "put", attribute.String("analyzer.name", e.Analyzer), attribute.String("job.id", e.JobID))
	if err != nil {
		return err
	}
	defer func() { finish(err) }()
	if err := e.Validate(); err != nil {
		return fmt.Errorf("resultcache put: %w", err)
	}
	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("resultcache put: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := tx.Exec(ctx, putRowSQL, tenantID, e.Analyzer, e.CacheKey, string(e.Result), e.ResponseModel,
		e.JobID, e.SourceCatalogID, e.TargetCatalogID); err != nil {
		return fmt.Errorf("resultcache put: store the result: %w", err)
	}
	if err := tx.Exec(ctx, putWriteSQL, tenantID, e.JobID, e.Analyzer, e.Attempt, e.TaskID, e.CacheKey, resultcache.ResultDigest(e.Result)); err != nil {
		return fmt.Errorf("resultcache put: record the write entry: %w", classify(err))
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("resultcache put: commit: %w", err)
	}
	return nil
}

// RecordStage implements resultcache.Store.
func (s *Store) RecordStage(ctx context.Context, rec resultcache.StageRecord) (err error) {
	ctx, tenantID, finish, err := s.start(ctx, "record_stage", attribute.String("analyzer.name", rec.Analyzer), attribute.String("job.id", rec.JobID))
	if err != nil {
		return err
	}
	defer func() { finish(err) }()
	if err := rec.Validate(); err != nil {
		return fmt.Errorf("resultcache record stage: %w", err)
	}
	models := rec.Stats.Models
	if models == nil {
		models = map[string]resultcache.ModelIdentity{}
	}
	modelsJSON, err := json.Marshal(models)
	if err != nil {
		return fmt.Errorf("resultcache record stage: encode models: %w", err)
	}
	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("resultcache record stage: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if len(rec.Hits) > 0 {
		tasks := make([]string, len(rec.Hits))
		keys := make([]string, len(rec.Hits))
		digests := make([]string, len(rec.Hits))
		producers := make([]string, len(rec.Hits))
		for i, h := range rec.Hits {
			tasks[i], keys[i], digests[i], producers[i] = h.TaskID, h.CacheKey, h.ResultDigest, h.ProducerJobID
		}
		if err := tx.Exec(ctx, hitsSQL, tenantID, rec.JobID, rec.Analyzer, rec.Attempt, tasks, keys, digests, producers); err != nil {
			return fmt.Errorf("resultcache record stage: record hit entries: %w", classify(err))
		}
	}
	if err := tx.Exec(ctx, statsSQL, tenantID, rec.JobID, rec.Analyzer, rec.Attempt, string(rec.Stats.Mode), rec.Stats.Reason,
		string(modelsJSON), rec.Stats.Tasks, rec.Stats.Hits, rec.Stats.Dispatched); err != nil {
		return fmt.Errorf("resultcache record stage: record stats: %w", classify(err))
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("resultcache record stage: commit: %w", err)
	}
	return nil
}

// The SQLSTATEs require_running_job_for_cache_record raises: for a job
// that is not running, and for a job the caller cannot see.
const (
	sqlstateRestrictViolation   = "23001"
	sqlstateForeignKeyViolation = "23503"
)

// classify marks a refusal of the freeze trigger by SQLSTATE:
// resultcache.ErrJobNotRunning or resultcache.ErrUnknownJob. It is applied
// only to inserts into the audit tables, whose only foreign key violation
// is the trigger's (it runs before the foreign key check); other errors
// pass unchanged.
func classify(err error) error {
	switch {
	case db.IsPgErrorCode(err, sqlstateRestrictViolation):
		return fmt.Errorf("%w: %w", resultcache.ErrJobNotRunning, err)
	case db.IsPgErrorCode(err, sqlstateForeignKeyViolation):
		return fmt.Errorf("%w: %w", resultcache.ErrUnknownJob, err)
	}
	return err
}

// scanStats reads one stats row: the lead columns (if any), then mode,
// reason, models::text, tasks, hits, dispatched.
func scanStats(rows db.Rows, lead ...any) (resultcache.Stats, error) {
	var st resultcache.Stats
	var mode, models string
	if err := rows.Scan(append(lead, &mode, &st.Reason, &models, &st.Tasks, &st.Hits, &st.Dispatched)...); err != nil {
		return st, err
	}
	st.Mode = resultcache.Mode(mode)
	if err := json.Unmarshal([]byte(models), &st.Models); err != nil {
		return st, fmt.Errorf("decode models: %w", err)
	}
	return st, nil
}

// Manifest implements resultcache.Store.
func (s *Store) Manifest(ctx context.Context, jobID, analyzerName, attempt string) (m resultcache.Manifest, err error) {
	ctx, tenantID, finish, err := s.start(ctx, "manifest", attribute.String("analyzer.name", analyzerName), attribute.String("job.id", jobID))
	if err != nil {
		return m, err
	}
	defer func() { finish(err) }()
	if err := resultcache.ValidAttempt(attempt); err != nil {
		return m, fmt.Errorf("resultcache manifest: %w", err)
	}
	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return m, fmt.Errorf("resultcache manifest: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if m, err = readManifest(ctx, tx, tenantID, jobID, analyzerName, attempt); err != nil {
		return resultcache.Manifest{}, fmt.Errorf("resultcache manifest: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return resultcache.Manifest{}, fmt.Errorf("resultcache manifest: commit: %w", err)
	}
	return m, nil
}

// readManifest reads one attempt's manifest in tx.
func readManifest(ctx context.Context, tx db.Transaction, tenantID, jobID, analyzerName, attempt string) (resultcache.Manifest, error) {
	m := resultcache.Manifest{TenantID: tenantID, JobID: jobID, Analyzer: analyzerName, Attempt: attempt}
	rows, err := tx.Query(ctx, manifestEntriesSQL, tenantID, jobID, analyzerName, attempt)
	if err != nil {
		return m, fmt.Errorf("query entries: %w", err)
	}
	for rows.Next() {
		var e resultcache.ManifestEntry
		if err := rows.Scan(&e.TaskID, &e.Role, &e.CacheKey, &e.ResultDigest, &e.ProducerJobID); err != nil {
			return m, errors.Join(fmt.Errorf("scan entry: %w", err), rows.Close())
		}
		m.Entries = append(m.Entries, e)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return m, fmt.Errorf("read entries: %w", err)
	}
	slices.SortFunc(m.Entries, resultcache.CompareEntries)

	rows, err = tx.Query(ctx, manifestStatsSQL, tenantID, jobID, analyzerName, attempt)
	if err != nil {
		return m, fmt.Errorf("query stats: %w", err)
	}
	for rows.Next() {
		st, err := scanStats(rows)
		if err != nil {
			return m, errors.Join(fmt.Errorf("scan stats: %w", err), rows.Close())
		}
		m.Stats = append(m.Stats, st)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return m, fmt.Errorf("read stats: %w", err)
	}
	return m, nil
}

// Checkpoint implements resultcache.Store. Its transaction is READ
// COMMITTED: the attempt's rows are read by a statement after the lock is
// held, so it sees every insert that committed before the lock, and no
// insert into the job can commit until the transaction ends.
func (s *Store) Checkpoint(ctx context.Context, expected resultcache.Manifest, seal func(ctx context.Context, digest string) error) (err error) {
	ctx, tenantID, finish, err := s.start(ctx, "checkpoint", attribute.String("analyzer.name", expected.Analyzer),
		attribute.String("job.id", expected.JobID))
	if err != nil {
		return err
	}
	defer func() { finish(err) }()
	if err := resultcache.ValidCheckpoint(tenantID, expected); err != nil {
		return fmt.Errorf("resultcache checkpoint: %w", err)
	}
	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("resultcache checkpoint: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.Query(ctx, lockJobSQL, tenantID, expected.JobID)
	if err != nil {
		return fmt.Errorf("resultcache checkpoint: lock job %s: %w", expected.JobID, err)
	}
	found := rows.Next()
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("resultcache checkpoint: lock job %s: %w", expected.JobID, err)
	}
	if !found {
		return fmt.Errorf("resultcache checkpoint: job %s: %w", expected.JobID, resultcache.ErrUnknownJob)
	}
	stored, err := readManifest(ctx, tx, tenantID, expected.JobID, expected.Analyzer, expected.Attempt)
	if err != nil {
		return fmt.Errorf("resultcache checkpoint: %w", err)
	}
	if err := resultcache.MatchRecord(expected, stored); err != nil {
		return fmt.Errorf("resultcache checkpoint: %w", err)
	}
	if err := seal(ctx, expected.Digest()); err != nil {
		return fmt.Errorf("resultcache checkpoint job %s attempt %s: %w", expected.JobID, expected.Attempt, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("resultcache checkpoint: commit: %w", err)
	}
	return nil
}

// VerifiedSummaries implements resultcache.Store. The signed attempts are
// read in one REPEATABLE READ snapshot.
func (s *Store) VerifiedSummaries(ctx context.Context, jobID string, signed map[string]resultcache.SignedAttempt) (out []resultcache.Summary, err error) {
	ctx, tenantID, finish, err := s.start(ctx, "verified_summaries", attribute.String("job.id", jobID))
	if err != nil {
		return nil, err
	}
	defer func() { finish(err) }()
	analyzers := slices.Sorted(maps.Keys(signed))
	for _, a := range analyzers {
		if err := resultcache.ValidAttempt(signed[a].Attempt); err != nil {
			return nil, fmt.Errorf("resultcache verified summaries: analyzer %s: %w", a, err)
		}
	}
	if len(analyzers) == 0 {
		return nil, nil
	}
	tx, err := s.conn.Begin(db.WithIsolation(ctx, db.IsolationRepeatableRead))
	if err != nil {
		return nil, fmt.Errorf("resultcache verified summaries: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	manifests := make(map[string]resultcache.Manifest, len(analyzers))
	for _, a := range analyzers {
		m, err := readManifest(ctx, tx, tenantID, jobID, a, signed[a].Attempt)
		if err != nil {
			return nil, fmt.Errorf("resultcache verified summaries: analyzer %s: %w", a, err)
		}
		manifests[a] = m
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("resultcache verified summaries: commit: %w", err)
	}
	return resultcache.SummarizeSigned(manifests, signed), nil
}
