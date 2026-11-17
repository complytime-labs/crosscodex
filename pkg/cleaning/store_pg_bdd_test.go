//go:build !integration

package cleaning_test

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/complytime-labs/crosscodex/pkg/cleaning"
	"github.com/complytime-labs/crosscodex/pkg/cleaning/cleaningtest"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// findingRow returns s as the PG store's finding SELECT yields it.
func findingRow(s cleaning.StoredFinding, evidence, concerns string) []any {
	return []any{s.ID, string(s.Signal), s.SourceCatalog, s.TargetCatalog, []byte(evidence), s.EvidenceHash,
		[]byte(concerns), []byte(`null`), []byte(`["e1"]`), string(s.Status), s.GraphApplied, time.Unix(0, 0)}
}

// Specs for what only the Postgres store does, over a scripted connection:
// its error mapping and messages, its parsing of stored rows, and its
// telemetry. Behavior shared with the Fake is in cleaningtest's contract,
// which store_integration_bdd_test.go runs against Postgres.
var _ = Describe("PGFindingStore", func() {
	const tenantID = "tenant-a"
	var (
		ctx   context.Context
		tx    *dbtest.Tx
		store *cleaning.PGFindingStore
		f     cleaning.StoredFinding
	)

	BeforeEach(func() {
		var err error
		ctx, err = tenant.WithTenant(context.Background(), tenantID)
		Expect(err).NotTo(HaveOccurred())
		tx = &dbtest.Tx{}
		store = cleaning.NewPGFindingStore(&dbtest.Conn{Tx: tx})
		f = cleaningtest.ContractFinding(1, cleaning.SignalLowAgreement, cleaning.StatusPending, "cat-x", "cat-y")
	})

	// inserts returns the recorded statements that write a decision.
	inserts := func() []dbtest.Statement {
		var out []dbtest.Statement
		for _, s := range tx.Execs() {
			if strings.Contains(s.Query, "INSERT INTO edge_finding_decisions") {
				out = append(out, s)
			}
		}
		return out
	}

	Describe("Decide", func() {
		var d cleaning.Decision

		BeforeEach(func() {
			d = cleaning.Decision{ID: "d-1", FindingID: f.ID, Verdict: cleaning.VerdictDismiss, DecidedBy: "alice",
				Rationale: "r", Request: []byte("{}"), Attestation: []byte("sig")}
		})

		DescribeTable("maps the reviewer check's SQLSTATE to a sentinel with the command that fixes it",
			func(code string, want error, command string) {
				tx.ExecFunc = func(_ context.Context, query string, _ ...any) error {
					if strings.Contains(query, "require_active_reviewer") {
						return &pgconn.PgError{Code: code}
					}
					return nil
				}
				err := store.Decide(ctx, tenantID, d, cleaning.StateToken(f), f)
				Expect(err).To(MatchError(want))
				Expect(err).To(MatchError(ContainSubstring("nothing was recorded")))
				Expect(err).To(MatchError(ContainSubstring(command)))
				Expect(inserts()).To(BeEmpty())
				Expect(tx.Rollbacks()).To(Equal(1))
				Expect(tx.Commits()).To(BeZero())
			},
			Entry("P0002 is ErrReviewerNotRegistered", "P0002", cleaning.ErrReviewerNotRegistered, "crosscodexd admin reviewer add"),
			Entry("55000 is ErrReviewerDeleted", "55000", cleaning.ErrReviewerDeleted, "crosscodexd admin reviewer restore"),
		)

		It("refuses a stale state token with a message to reload, and writes nothing", func() {
			tx.ExecFunc = func(context.Context, string, ...any) error { return nil }
			tx.QueryRowFunc = func(context.Context, string, ...any) db.Row {
				changed := f
				changed.Status = cleaning.StatusRetired
				return dbtest.Row(nil, findingRow(changed, `{"verdicts":null}`, `[]`)...)
			}
			err := store.Decide(ctx, tenantID, d, cleaning.StateToken(f), f)
			Expect(err).To(MatchError(cleaning.ErrStateChanged))
			Expect(err).To(MatchError(ContainSubstring("Reload the finding")))
			Expect(inserts()).To(BeEmpty())
			Expect(tx.Rollbacks()).To(Equal(1))
		})
	})

	Describe("MarkProjected at a lowered edge_ids bound", func() {
		BeforeEach(func() { DeferCleanup(cleaning.SetStoredListLimit(20)) })

		It("refuses edge IDs over the bound with ErrFindingTooLarge before any statement", func() {
			err := store.MarkProjected(ctx, tenantID, f.ID, []string{"edge-0001", "edge-0002"})
			Expect(err).To(MatchError(cleaning.ErrFindingTooLarge))
			Expect(err).To(MatchError(ContainSubstring("the edge_ids column would hold 25 bytes of JSON, over the 20-byte limit")))
			Expect(err).To(MatchError(ContainSubstring("nothing was written")))
			Expect(tx.Execs()).To(BeEmpty())
			Expect(tx.Commits()).To(BeZero())
		})

		It("does the same on the Fake and leaves the finding unchanged", func() {
			fake := &cleaningtest.Fake{Seed: map[string][]cleaning.StoredFinding{tenantID: {f}}}
			err := fake.MarkProjected(ctx, tenantID, f.ID, []string{"edge-0001", "edge-0002"})
			Expect(err).To(MatchError(cleaning.ErrFindingTooLarge))
			Expect(err).To(MatchError(ContainSubstring("nothing was written")))
			got, _, err := fake.GetFinding(ctx, tenantID, f.ID)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.EdgeIDs).To(Equal(f.EdgeIDs))
		})
	})

	Describe("reading stored findings", func() {
		DescribeTable("fails closed on a row that does not decode, naming the finding",
			func(evidence, concerns string, want error) {
				tx.QueryRowFunc = func(context.Context, string, ...any) db.Row {
					return dbtest.Row(nil, findingRow(f, evidence, concerns)...)
				}
				_, _, err := store.GetFinding(ctx, tenantID, f.ID)
				Expect(err).To(MatchError(want))
				Expect(err).To(MatchError(ContainSubstring(f.ID)))
			},
			Entry("evidence with an unknown field", `{"verdicts":null,"x":1}`, `[]`, cleaning.ErrInvalidEvidence),
			Entry("a concern with an empty part", `{"verdicts":null}`, `[{"job_id":"","source_id":"s","target_id":"t"}]`,
				cleaning.ErrInvalidVerdictKey),
			Entry("a concern with an unknown field", `{"verdicts":null}`, `[{"job_id":"j","source_id":"s","target_id":"t","x":1}]`,
				cleaning.ErrInvalidVerdictKey),
		)
	})

	Describe("CompletedJobs", func() {
		completed := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
		const config = `{"catalog_id":"cat-x","target_catalog_id":"cat-y"}`

		// script makes the jobs query return jobs and the stages query stages.
		script := func(jobs, stages [][]any) {
			tx.QueryFunc = func(_ context.Context, query string, _ ...any) (db.Rows, error) {
				if strings.Contains(query, "FROM jobs") && !strings.Contains(query, "stage_record") {
					return dbtest.Rows(nil, jobs...), nil
				}
				return dbtest.Rows(nil, stages...), nil
			}
		}

		It("returns each job's catalogs, latest stage completion and parsed stage records", func() {
			script([][]any{{"job-1", []byte(config), true, completed}},
				[][]any{{"job-1", "analyze", true, `{"prompt_name":"p","prompt_version":"1","models":{"a":"r"}}`},
					{"job-1", "synthesize", false, ""}}) // NULL stage_record
			jobs, err := store.CompletedJobs(ctx, tenantID)
			Expect(err).NotTo(HaveOccurred())
			Expect(jobs).To(HaveLen(1))
			Expect(jobs[0].ID).To(Equal("job-1"))
			Expect(jobs[0].CompletedAt).To(Equal(completed))
			Expect(jobs[0].Catalogs.SourceID).To(Equal("cat-x"))
			Expect(jobs[0].Catalogs.TargetID).To(Equal("cat-y"))
			Expect(jobs[0].Stages).To(Equal(map[string]*cleaning.StageRecord{
				"analyze":    {PromptName: "p", PromptVersion: "1", Models: map[string]string{"a": "r"}},
				"synthesize": nil,
			}))
		})

		It("reads every user's jobs: it drops the per-user row filter from ctx", func() {
			var beginUser string
			store = cleaning.NewPGFindingStore(&dbtest.Conn{BeginFunc: func(ctx context.Context) (db.Transaction, error) {
				beginUser = tenant.UserFromContext(ctx)
				return tx, nil
			}})
			script(nil, nil)
			_, err := store.CompletedJobs(tenant.WithUser(ctx, "alice"), tenantID)
			Expect(err).NotTo(HaveOccurred())
			Expect(beginUser).To(BeEmpty())
		})

		DescribeTable("fails closed, naming the job, on what it cannot read",
			func(jobs, stages [][]any, contains []string, want error) {
				script(jobs, stages)
				_, err := store.CompletedJobs(ctx, tenantID)
				Expect(err).To(HaveOccurred())
				for _, s := range contains {
					Expect(err).To(MatchError(ContainSubstring(s)))
				}
				if want != nil {
					Expect(err).To(MatchError(want))
				}
			},
			Entry("a stage_record that does not parse",
				[][]any{{"job-1", []byte(config), true, completed}}, [][]any{{"job-1", "analyze", true, `{"x":1}`}},
				[]string{"job-1", "analyze"}, cleaning.ErrInvalidStageRecord),
			Entry("a non-NULL empty stage_record",
				[][]any{{"job-1", []byte(config), true, completed}}, [][]any{{"job-1", "analyze", true, ""}},
				[]string{"job-1", "analyze", "empty"}, cleaning.ErrInvalidStageRecord),
			Entry("a config without a catalog",
				[][]any{{"job-1", []byte(`{}`), true, completed}}, nil, []string{"job-1", "catalog"}, nil),
			Entry("a NULL config",
				[][]any{{"job-1", nil, true, completed}}, nil, []string{"job-1", "catalog"}, nil),
			Entry("no stage completion time",
				[][]any{{"job-1", []byte(config), false, time.Unix(0, 0).UTC()}}, nil, []string{"job-1", "completion time"}, nil),
		)
	})

	Describe("telemetry", func() {
		var tp *telemetrytest.TestProvider

		BeforeEach(func() {
			var err error
			tp, err = telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { Expect(tp.Shutdown(context.Background())).To(Succeed()) })
			// A nil connection: every call below is refused before using it.
			store = cleaning.NewPGFindingStore(nil, cleaning.WithFindingStoreTelemetry(tp.TracerProvider(), tp.MeterProvider()))
		})

		counter := func(key, value string) int64 {
			m := telemetrytest.FindMetric(tp.GetMetrics(), "cleaning.findingstore.operations.total")
			Expect(m).NotTo(BeNil())
			sum, ok := m.Data.(metricdata.Sum[int64])
			Expect(ok).To(BeTrue())
			var total int64
			for _, dp := range sum.DataPoints {
				if v, ok := dp.Attributes.Value(attribute.Key(key)); ok && v.AsString() == value {
					total += dp.Value
				}
			}
			return total
		}

		DescribeTable("traces, counts and times every method, including a refused call",
			func(op string, call func(ctx context.Context) error) {
				ctxB, err := tenant.WithTenant(context.Background(), "tenant-b")
				Expect(err).NotTo(HaveOccurred())
				Expect(call(ctxB)).To(MatchError(ContainSubstring("does not match")))
				span := telemetrytest.FindSpan(tp.GetSpans(), "cleaning.findingstore."+op)
				Expect(span).NotTo(BeNil())
				Expect(span.Status().Code).To(Equal(codes.Error))
				v, ok := telemetrytest.SpanAttribute(span, "tenant.id")
				Expect(ok).To(BeTrue())
				Expect(v.AsString()).To(Equal(tenantID))
				Expect(counter("operation", op)).To(Equal(int64(1)))
				Expect(counter("status", "error")).To(Equal(int64(1)))
				m := telemetrytest.FindMetric(tp.GetMetrics(), "cleaning.findingstore.operation.duration_ms")
				Expect(m).NotTo(BeNil())
				Expect(telemetrytest.HistogramCount(m)).To(Equal(uint64(1)))
			},
			Entry("CompletedJobs", "CompletedJobs", func(c context.Context) error { _, err := store.CompletedJobs(c, tenantID); return err }),
			Entry("Findings", "Findings", func(c context.Context) error {
				_, err := store.Findings(c, tenantID, cleaning.Scope{})
				return err
			}),
			Entry("ListFindings", "ListFindings", func(c context.Context) error {
				_, _, err := store.ListFindings(c, tenantID, cleaning.FindingFilter{PageSize: 1})
				return err
			}),
			Entry("GetFinding", "GetFinding", func(c context.Context) error { _, _, err := store.GetFinding(c, tenantID, "x"); return err }),
			Entry("ApplyRun", "ApplyRun", func(c context.Context) error { return store.ApplyRun(c, tenantID, cleaning.RunWrite{}) }),
			Entry("Decide", "Decide", func(c context.Context) error {
				d := cleaning.Decision{ID: "d", FindingID: f.ID, Verdict: cleaning.VerdictDismiss, DecidedBy: "alice",
					Request: []byte("{}"), Attestation: []byte("sig")}
				return store.Decide(c, tenantID, d, "", f)
			}),
			Entry("Unprojected", "Unprojected", func(c context.Context) error { _, err := store.Unprojected(c, tenantID); return err }),
			Entry("MarkProjected", "MarkProjected", func(c context.Context) error { return store.MarkProjected(c, tenantID, "x", nil) }),
			Entry("PendingCount", "PendingCount", func(c context.Context) error { _, err := store.PendingCount(c, tenantID); return err }),
		)
	})
})
