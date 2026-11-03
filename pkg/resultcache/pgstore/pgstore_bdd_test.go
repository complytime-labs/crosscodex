package pgstore_test

import (
	"context"
	"errors"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
	"github.com/complytime-labs/crosscodex/pkg/resultcache"
	"github.com/complytime-labs/crosscodex/pkg/resultcache/pgstore"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

var errDriver = errors.New("driver: connection refused")

// pgError carries a SQLSTATE as the driver's errors do.
type pgError struct{ code, msg string }

func (e pgError) Error() string    { return e.msg }
func (e pgError) SQLState() string { return e.code }

func tenantCtx() context.Context {
	ctx, err := tenant.WithTenant(context.Background(), "tenant-a")
	Expect(err).NotTo(HaveOccurred())
	return ctx
}

const attempt1 = "6f1c2b0e-8d4a-4c3e-9b7a-1d2e3f405162"

func validEntry() resultcache.Entry {
	return resultcache.Entry{JobID: "job-1", Analyzer: "classify", Attempt: attempt1, TaskID: "t1", CacheKey: strings.Repeat("a", 64),
		Result: []byte(`{"model":"m"}`), ResponseModel: "m", SourceCatalogID: "cat-1", TargetCatalogID: "cat-2"}
}

// newStore returns a store over a fake connection whose statements succeed.
func newStore(opts ...pgstore.Option) (*pgstore.Store, *dbtest.Conn, *dbtest.Tx) {
	tx := &dbtest.Tx{ExecFunc: func(context.Context, string, ...any) error { return nil }}
	conn := &dbtest.Conn{Tx: tx}
	s, err := pgstore.New(conn, opts...)
	Expect(err).NotTo(HaveOccurred())
	return s, conn, tx
}

var _ = Describe("pgstore.Store", func() {
	var (
		tx   *dbtest.Tx
		conn *dbtest.Conn
		s    *pgstore.Store
	)

	BeforeEach(func() {
		s, conn, tx = newStore()
	})

	It("puts the row and its write entry in one transaction, binding every value", func() {
		e := validEntry()
		Expect(s.Put(tenantCtx(), e)).To(Succeed())
		execs := tx.Execs()
		Expect(execs).To(HaveLen(2))
		Expect(execs[0].Query).To(ContainSubstring("INSERT INTO llm_result_cache"))
		Expect(execs[0].Query).To(ContainSubstring("ON CONFLICT (tenant_id, analyzer, cache_key) DO UPDATE"))
		Expect(execs[0].Args).To(Equal([]any{"tenant-a", "classify", e.CacheKey, `{"model":"m"}`, "m", "job-1", attempt1, "cat-1", "cat-2"}))
		Expect(execs[1].Query).To(ContainSubstring("INSERT INTO job_cache_entries"))
		Expect(execs[1].Query).To(ContainSubstring("ON CONFLICT DO NOTHING"))
		Expect(execs[1].Args).To(Equal([]any{"tenant-a", "job-1", "classify", attempt1, "t1", e.CacheKey, resultcache.ResultDigest(e.Result)}))
		Expect(tx.Commits()).To(Equal(1))
	})

	It("commits nothing when the write entry fails, so no row is left without it", func() {
		calls := 0
		tx.ExecFunc = func(context.Context, string, ...any) error {
			calls++
			if calls == 2 {
				return errDriver
			}
			return nil
		}
		Expect(s.Put(tenantCtx(), validEntry())).To(MatchError(errDriver))
		Expect(tx.Commits()).To(BeZero())
		Expect(tx.Rollbacks()).To(Equal(1))
	})

	// The freeze trigger raises restrict_violation (23001); the store
	// classifies by that code, never by the message.
	DescribeTable("reports a frozen job as ErrJobNotRunning by its SQLSTATE",
		func(call func(*pgstore.Store) error) {
			tx.ExecFunc = func(_ context.Context, query string, _ ...any) error {
				if strings.Contains(query, "job_cache_") {
					return pgError{code: "23001", msg: "any text"}
				}
				return nil
			}
			err := call(s)
			Expect(err).To(MatchError(resultcache.ErrJobNotRunning))
			Expect(err).To(MatchError(ContainSubstring("any text")))
			Expect(tx.Commits()).To(BeZero())
		},
		Entry("Put", func(s *pgstore.Store) error { return s.Put(tenantCtx(), validEntry()) }),
		Entry("RecordStage", func(s *pgstore.Store) error {
			return s.RecordStage(tenantCtx(), resultcache.StageRecord{JobID: "job-2", Analyzer: "classify", Attempt: attempt1,
				Stats: resultcache.Stats{Mode: resultcache.ModeDisabled, Tasks: 1, Dispatched: 1}})
		}),
	)

	// The trigger raises foreign_key_violation (23503) for a job the caller
	// cannot see: that is ErrUnknownJob, never ErrJobNotRunning, whatever
	// the message says.
	It("reports the trigger's invisible job as ErrUnknownJob by its SQLSTATE", func() {
		tx.ExecFunc = func(_ context.Context, query string, _ ...any) error {
			if strings.Contains(query, "job_cache_") {
				return pgError{code: "23503", msg: "cannot add a job_cache_stats row: its status is \"completed\""}
			}
			return nil
		}
		err := s.RecordStage(tenantCtx(), resultcache.StageRecord{JobID: "job-2", Analyzer: "classify", Attempt: attempt1,
			Stats: resultcache.Stats{Mode: resultcache.ModeDisabled, Tasks: 1, Dispatched: 1}})
		Expect(err).To(MatchError(resultcache.ErrUnknownJob))
		Expect(err).NotTo(MatchError(resultcache.ErrJobNotRunning))
	})

	It("refuses an invalid entry before opening a transaction", func() {
		e := validEntry()
		e.CacheKey = "nope"
		Expect(s.Put(tenantCtx(), e)).To(MatchError(resultcache.ErrInvalidEntry))
		Expect(conn.Begins()).To(BeZero())
	})

	It("refuses a manifest for an invalid attempt before opening a transaction", func() {
		_, err := s.Manifest(tenantCtx(), "job-2", "classify", "")
		Expect(err).To(MatchError(resultcache.ErrInvalidEntry))
		Expect(conn.Begins()).To(BeZero())
	})

	It("refuses an invalid stage record before opening a transaction", func() {
		rec := resultcache.StageRecord{JobID: "job-2", Analyzer: "classify", Attempt: attempt1, Stats: resultcache.Stats{Mode: "sometimes"}}
		Expect(s.RecordStage(tenantCtx(), rec)).To(MatchError(resultcache.ErrInvalidEntry))
		Expect(conn.Begins()).To(BeZero())
	})

	DescribeTable("refuses a context without a tenant before opening a transaction",
		func(call func(context.Context) error) {
			Expect(call(context.Background())).To(MatchError(db.ErrTenantRequired))
			Expect(conn.Begins()).To(BeZero())
		},
		Entry("Producers", func(ctx context.Context) error { _, err := s.Producers(ctx, "classify", []string{"k"}); return err }),
		Entry("Put", func(ctx context.Context) error { return s.Put(ctx, validEntry()) }),
		Entry("RecordStage", func(ctx context.Context) error {
			return s.RecordStage(ctx, resultcache.StageRecord{JobID: "j", Analyzer: "a", Attempt: attempt1, Stats: resultcache.Stats{Mode: resultcache.ModeDisabled}})
		}),
		Entry("Manifest", func(ctx context.Context) error { _, err := s.Manifest(ctx, "j", "a", attempt1); return err }),
		Entry("VerifiedSummaries", func(ctx context.Context) error {
			_, err := s.VerifiedSummaries(ctx, "j", map[string]resultcache.SignedAttempt{"a": {Attempt: attempt1}})
			return err
		}),
		Entry("LookupVerifiable", func(ctx context.Context) error {
			_, err := s.LookupVerifiable(ctx, "a", []string{"k"}, nil)
			return err
		}),
		Entry("Checkpoint", func(ctx context.Context) error {
			return s.Checkpoint(ctx, resultcache.Manifest{TenantID: "tenant-a", JobID: "j", Analyzer: "a", Attempt: attempt1},
				func(context.Context, string) error { return nil })
		}),
	)

	It("returns each key's producer, no result bytes, and skips the query for no keys", func() {
		key := strings.Repeat("a", 64)
		tx.QueryFunc = func(_ context.Context, query string, args ...any) (db.Rows, error) {
			Expect(query).To(ContainSubstring("c.cache_key = ANY($3::text[])"))
			Expect(args).To(Equal([]any{"tenant-a", "classify", []string{key}}))
			return dbtest.Rows(nil, []any{key, `{"model":"m"}`, "m", "job-1", attempt1}), nil
		}
		producers, err := s.Producers(tenantCtx(), "classify", []string{key})
		Expect(err).NotTo(HaveOccurred())
		Expect(producers).To(Equal(map[string]string{key: "job-1"}))

		empty, err := s.Producers(tenantCtx(), "classify", nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(empty).To(BeEmpty())
		Expect(conn.Begins()).To(Equal(1))
	})

	Describe("LookupVerifiable", func() {
		key := strings.Repeat("a", 64)
		producer := resultcache.AttemptRef{JobID: "job-1", Analyzer: "classify", Attempt: attempt1}
		record := resultcache.Manifest{TenantID: "tenant-a", JobID: "job-1", Analyzer: "classify", Attempt: attempt1,
			Entries: []resultcache.ManifestEntry{{TaskID: "t1", Role: resultcache.RoleWrite, CacheKey: key,
				ResultDigest: resultcache.ResultDigest([]byte(`{"model":"m"}`))}},
			Stats: []resultcache.Stats{{Mode: resultcache.ModeEnabled, Tasks: 1, Dispatched: 1}}}
		// script answers the cached rows and job-1's attempt.
		script := func() *[]string {
			var queries []string
			tx.QueryFunc = func(_ context.Context, query string, args ...any) (db.Rows, error) {
				queries = append(queries, query)
				switch {
				case strings.Contains(query, "FROM job_cache_entries"):
					Expect(args).To(Equal([]any{"tenant-a", "classify", []string{"job-1"}, []string{attempt1}}))
					return dbtest.Rows(nil, []any{"job-1", attempt1, "t1", "write", key, record.Entries[0].ResultDigest, ""}), nil
				case strings.Contains(query, "FROM job_cache_stats"):
					return dbtest.Rows(nil, []any{"job-1", attempt1, "enabled", "", `{}`, 1, 0, 1}), nil
				default:
					return dbtest.Rows(nil, []any{key, `{"model":"m"}`, "m", "job-1", attempt1}), nil
				}
			}
			return &queries
		}

		It("reads in one REPEATABLE READ transaction, without locking, each repeated attempt once", func() {
			var beginCtx context.Context
			conn.BeginFunc = func(ctx context.Context) (db.Transaction, error) { beginCtx = ctx; return tx, nil }
			queries := script()
			snap, err := s.LookupVerifiable(tenantCtx(), "classify", []string{key},
				[]resultcache.AttemptRef{producer, producer, {JobID: "job-9", Analyzer: "classify", Attempt: attempt1}})
			Expect(err).NotTo(HaveOccurred())
			level, ok := db.IsolationFromContext(beginCtx)
			Expect(ok).To(BeTrue())
			Expect(level).To(Equal(db.IsolationRepeatableRead))
			Expect(*queries).To(HaveLen(3), "the rows, then job-1's attempt once; job-9 stored no requested row")
			for _, q := range *queries {
				Expect(q).NotTo(ContainSubstring("FOR SHARE"))
			}
			Expect(snap.Keys()).To(Equal([]string{key}))
			h, err := snap.Verify(key, map[resultcache.AttemptRef]string{producer: record.Digest()})
			Expect(err).NotTo(HaveOccurred())
			Expect(h.ProducerJobID).To(Equal("job-1"))
			Expect(h.ProducerAttempt).To(Equal(attempt1))
			Expect(tx.Commits()).To(Equal(1))
		})

		It("refuses an attempt for another analyzer before opening a transaction", func() {
			other := producer
			other.Analyzer = "artifacts"
			_, err := s.LookupVerifiable(tenantCtx(), "classify", []string{key}, []resultcache.AttemptRef{other})
			Expect(err).To(MatchError(resultcache.ErrInvalidEntry))
			Expect(conn.Begins()).To(BeZero())
		})

		It("refuses an invalid attempt before opening a transaction", func() {
			_, err := s.LookupVerifiable(tenantCtx(), "classify", []string{key}, []resultcache.AttemptRef{{JobID: "job-1", Analyzer: "classify"}})
			Expect(err).To(MatchError(resultcache.ErrInvalidEntry))
			Expect(conn.Begins()).To(BeZero())
		})

		It("returns a failed read's error and rolls back", func() {
			tx.QueryFunc = func(context.Context, string, ...any) (db.Rows, error) { return dbtest.Rows(errDriver), nil }
			_, err := s.LookupVerifiable(tenantCtx(), "classify", []string{key}, []resultcache.AttemptRef{producer})
			Expect(err).To(MatchError(errDriver))
			Expect(tx.Commits()).To(BeZero())
			Expect(tx.Rollbacks()).To(Equal(1))
		})
	})

	Describe("Checkpoint", func() {
		record := resultcache.Manifest{TenantID: "tenant-a", JobID: "job-1", Analyzer: "classify", Attempt: attempt1}
		// lockAnd answers the lock with job-1 and the attempt's reads with
		// the given entry rows.
		lockAnd := func(entries ...[]any) *[]string {
			var queries []string
			tx.QueryFunc = func(_ context.Context, query string, args ...any) (db.Rows, error) {
				queries = append(queries, query)
				switch {
				case strings.Contains(query, "FOR UPDATE"):
					Expect(args).To(Equal([]any{"tenant-a", "job-1"}))
					return dbtest.Rows(nil, []any{"job-1"}), nil
				case strings.Contains(query, "FROM job_cache_entries"):
					return dbtest.Rows(nil, entries...), nil
				default:
					return dbtest.Rows(nil), nil
				}
			}
			return &queries
		}

		It("locks the job FOR UPDATE, then seals the record's digest while the lock is held", func() {
			queries := lockAnd()
			var sealed string
			Expect(s.Checkpoint(tenantCtx(), record, func(_ context.Context, digest string) error {
				sealed = digest
				Expect(tx.Commits()).To(BeZero(), "seal runs while the lock is held")
				return nil
			})).To(Succeed())
			Expect((*queries)[0]).To(ContainSubstring("FROM jobs WHERE tenant_id = $1 AND job_id = $2 FOR UPDATE"))
			Expect(sealed).To(Equal(record.Digest()))
			Expect(tx.Commits()).To(Equal(1))
		})

		It("refuses rows that differ from the record as corrupt, without sealing", func() {
			lockAnd([]any{"t9", "write", strings.Repeat("e", 64), "sha256:" + strings.Repeat("e", 64), ""})
			err := s.Checkpoint(tenantCtx(), record, func(context.Context, string) error {
				Fail("seal ran")
				return nil
			})
			Expect(err).To(MatchError(resultcache.ErrCorrupt))
			Expect(err).To(MatchError(ContainSubstring("unexpected rows: entry(task=\"t9\"")))
			Expect(tx.Commits()).To(BeZero())
		})

		It("refuses a job it cannot lock with ErrUnknownJob, without sealing", func() {
			tx.QueryFunc = func(context.Context, string, ...any) (db.Rows, error) { return dbtest.Rows(nil), nil }
			err := s.Checkpoint(tenantCtx(), record, func(context.Context, string) error {
				Fail("seal ran")
				return nil
			})
			Expect(err).To(MatchError(resultcache.ErrUnknownJob))
		})

		It("refuses a record of another tenant before opening a transaction", func() {
			other := record
			other.TenantID = "tenant-b"
			Expect(s.Checkpoint(tenantCtx(), other, func(context.Context, string) error { return nil })).To(MatchError(resultcache.ErrInvalidEntry))
			Expect(conn.Begins()).To(BeZero())
		})

		It("returns seal's error and releases the lock without committing", func() {
			lockAnd()
			errSeal := errors.New("sign failed")
			err := s.Checkpoint(tenantCtx(), record, func(context.Context, string) error { return errSeal })
			Expect(err).To(MatchError(errSeal))
			Expect(tx.Commits()).To(BeZero())
			Expect(tx.Rollbacks()).To(Equal(1))
		})
	})

	It("records hit entries in one statement and the stats row, in one transaction", func() {
		rec := resultcache.StageRecord{JobID: "job-2", Analyzer: "classify", Attempt: attempt1,
			Hits: []resultcache.HitEntry{{TaskID: "t1", CacheKey: strings.Repeat("a", 64), ResultDigest: "sha256:" + strings.Repeat("1", 64), ProducerJobID: "job-1"}},
			Stats: resultcache.Stats{Mode: resultcache.ModeEnabled, Tasks: 2, Hits: 1, Dispatched: 1,
				Models: map[string]resultcache.ModelIdentity{"default": {Observed: "qwen"}}}}
		Expect(s.RecordStage(tenantCtx(), rec)).To(Succeed())
		execs := tx.Execs()
		Expect(execs).To(HaveLen(2))
		Expect(execs[0].Query).To(ContainSubstring("unnest($5::text[], $6::text[], $7::text[], $8::text[])"))
		Expect(execs[0].Args).To(Equal([]any{"tenant-a", "job-2", "classify", attempt1,
			[]string{"t1"}, []string{strings.Repeat("a", 64)}, []string{"sha256:" + strings.Repeat("1", 64)}, []string{"job-1"}}))
		Expect(execs[1].Query).To(ContainSubstring("INSERT INTO job_cache_stats"))
		Expect(execs[1].Args).To(Equal([]any{"tenant-a", "job-2", "classify", attempt1, "enabled", "",
			`{"default":{"observed":"qwen","revision":""}}`, 2, 1, 1}))
		Expect(tx.Commits()).To(Equal(1))
	})

	It("records only the stats row when there are no hits, with an empty models object", func() {
		Expect(s.RecordStage(tenantCtx(), resultcache.StageRecord{JobID: "job-2", Analyzer: "classify", Attempt: attempt1,
			Stats: resultcache.Stats{Mode: resultcache.ModeDisabled, Tasks: 1, Dispatched: 1}})).To(Succeed())
		Expect(tx.Execs()).To(HaveLen(1))
		Expect(tx.Execs()[0].Args[6]).To(Equal(`{}`))
	})

	It("commits no hit entries when the stats row fails", func() {
		tx.ExecFunc = func(_ context.Context, query string, _ ...any) error {
			if strings.Contains(query, "job_cache_stats") {
				return errDriver
			}
			return nil
		}
		rec := resultcache.StageRecord{JobID: "job-2", Analyzer: "classify", Attempt: attempt1,
			Hits:  []resultcache.HitEntry{{TaskID: "t1", CacheKey: strings.Repeat("a", 64), ResultDigest: "sha256:" + strings.Repeat("1", 64), ProducerJobID: "job-1"}},
			Stats: resultcache.Stats{Mode: resultcache.ModeEnabled, Tasks: 1, Hits: 1}}
		Expect(s.RecordStage(tenantCtx(), rec)).To(MatchError(errDriver))
		Expect(tx.Commits()).To(BeZero())
		Expect(tx.Rollbacks()).To(Equal(1))
	})

	It("reads a manifest's entries in canonical order and its stats oldest first", func() {
		tx.QueryFunc = func(_ context.Context, query string, args ...any) (db.Rows, error) {
			Expect(args).To(Equal([]any{"tenant-a", "job-2", "classify", attempt1}))
			if strings.Contains(query, "FROM job_cache_entries") {
				return dbtest.Rows(nil,
					[]any{"t2", "write", strings.Repeat("b", 64), "sha256:" + strings.Repeat("2", 64), ""},
					[]any{"t1", "hit", strings.Repeat("a", 64), "sha256:" + strings.Repeat("1", 64), "job-1"},
				), nil
			}
			Expect(query).To(ContainSubstring("ORDER BY seq"))
			return dbtest.Rows(nil,
				[]any{"enabled", "", `{"default":{"observed":"qwen","revision":""}}`, 2, 1, 1},
				[]any{"disabled", "", `{}`, 1, 0, 1},
			), nil
		}
		m, err := s.Manifest(tenantCtx(), "job-2", "classify", attempt1)
		Expect(err).NotTo(HaveOccurred())
		Expect(m.Attempt).To(Equal(attempt1))
		Expect(m.TenantID).To(Equal("tenant-a"))
		Expect(m.Analyzer).To(Equal("classify"))
		Expect(m.Entries).To(Equal([]resultcache.ManifestEntry{
			{TaskID: "t1", Role: resultcache.RoleHit, CacheKey: strings.Repeat("a", 64), ResultDigest: "sha256:" + strings.Repeat("1", 64), ProducerJobID: "job-1"},
			{TaskID: "t2", Role: resultcache.RoleWrite, CacheKey: strings.Repeat("b", 64), ResultDigest: "sha256:" + strings.Repeat("2", 64)},
		}))
		Expect(m.Stats).To(Equal([]resultcache.Stats{
			{Mode: resultcache.ModeEnabled, Tasks: 2, Hits: 1, Dispatched: 1, Models: map[string]resultcache.ModelIdentity{"default": {Observed: "qwen"}}},
			{Mode: resultcache.ModeDisabled, Tasks: 1, Dispatched: 1, Models: map[string]resultcache.ModelIdentity{}},
		}))
	})

	It("refuses a stats row whose models are not a JSON object", func() {
		tx.QueryFunc = func(_ context.Context, query string, _ ...any) (db.Rows, error) {
			if strings.Contains(query, "FROM job_cache_entries") {
				return dbtest.Rows(nil), nil
			}
			return dbtest.Rows(nil, []any{"enabled", "", `[1]`, 1, 0, 1}), nil
		}
		_, err := s.Manifest(tenantCtx(), "job-2", "classify", attempt1)
		Expect(err).To(MatchError(ContainSubstring("decode models")))
	})

	It("summarizes each signed attempt from one REPEATABLE READ read, marking a mismatch unattested", func() {
		var beginCtx context.Context
		conn.BeginFunc = func(ctx context.Context) (db.Transaction, error) { beginCtx = ctx; return tx, nil }
		tx.QueryFunc = func(_ context.Context, query string, args ...any) (db.Rows, error) {
			if strings.Contains(query, "FROM job_cache_stats") && args[2] == "classify" {
				return dbtest.Rows(nil, []any{"enabled", "", `{}`, 2, 2, 0}), nil
			}
			return dbtest.Rows(nil), nil
		}
		classify := resultcache.Manifest{TenantID: "tenant-a", JobID: "job-2", Analyzer: "classify", Attempt: "a-cls",
			Stats: []resultcache.Stats{{Mode: resultcache.ModeEnabled, Tasks: 2, Hits: 2}}}
		sums, err := s.VerifiedSummaries(tenantCtx(), "job-2", map[string]resultcache.SignedAttempt{
			"classify":  {Attempt: "a-cls", Digest: classify.Digest()},
			"artifacts": {Attempt: "a-art", Digest: "sha256:" + strings.Repeat("0", 64)},
		})
		Expect(err).NotTo(HaveOccurred())
		level, ok := db.IsolationFromContext(beginCtx)
		Expect(ok).To(BeTrue())
		Expect(level).To(Equal(db.IsolationRepeatableRead))
		Expect(sums).To(Equal([]resultcache.Summary{
			{Analyzer: "artifacts", Attempt: "a-art"},
			{Analyzer: "classify", Attempt: "a-cls", Attested: true, Stats: resultcache.Stats{Mode: resultcache.ModeEnabled, Tasks: 2, Hits: 2,
				Models: map[string]resultcache.ModelIdentity{}}},
		}))
	})

	It("traces and counts every operation with its tenant", func() {
		tp, err := telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(tp.Shutdown(context.Background())).To(Succeed()) })
		s, _, _ = newStore(pgstore.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()))
		Expect(s.Put(tenantCtx(), validEntry())).To(Succeed())
		span := telemetrytest.FindSpan(tp.GetSpans(), "resultcache.put")
		Expect(span).NotTo(BeNil())
		v, ok := telemetrytest.SpanAttribute(span, "tenant.id")
		Expect(ok).To(BeTrue())
		Expect(v.AsString()).To(Equal("tenant-a"))
		m := telemetrytest.FindMetric(tp.GetMetrics(), "resultcache.store.operations.total")
		Expect(m).NotTo(BeNil())
		n, err := telemetrytest.CounterValue(m)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(BeEquivalentTo(1))
		h := telemetrytest.FindMetric(tp.GetMetrics(), "resultcache.store.duration_ms")
		Expect(h).NotTo(BeNil())
		count, err := telemetrytest.Float64HistogramCount(h)
		Expect(err).NotTo(HaveOccurred())
		Expect(count).To(BeEquivalentTo(1))
	})

	It("records the rows a read returned", func() {
		tp, err := telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(tp.Shutdown(context.Background())).To(Succeed()) })
		var tx2 *dbtest.Tx
		s, _, tx2 = newStore(pgstore.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()))
		key := strings.Repeat("a", 64)
		tx2.QueryFunc = func(context.Context, string, ...any) (db.Rows, error) {
			return dbtest.Rows(nil,
				[]any{key, `{"model":"m"}`, "m", "job-1", attempt1},
				[]any{strings.Repeat("b", 64), `{"model":"m"}`, "m", "job-1", attempt1},
			), nil
		}
		_, err = s.Producers(tenantCtx(), "classify", []string{key, strings.Repeat("b", 64)})
		Expect(err).NotTo(HaveOccurred())
		g := telemetrytest.FindMetric(tp.GetMetrics(), "resultcache.store.lookup.rows")
		Expect(g).NotTo(BeNil())
		n, err := telemetrytest.GaugeValue(g)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(BeEquivalentTo(2))
	})
})
