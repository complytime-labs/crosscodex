package graph_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/graph"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

func TestGraphBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Graph BDD Suite")
}

var restoreLogs func()

var _ = BeforeSuite(func() {
	restoreLogs = testspecs.RedirectLogsToGinkgo()
})

var _ = AfterSuite(func() {
	restoreLogs()
})

var _ = Describe("ResolverRegistry", func() {
	var registry *graph.ResolverRegistry

	BeforeEach(func() {
		registry = graph.NewResolverRegistry()
	})

	It("returns ErrResolverNotFound for unknown scheme", func() {
		ref := graph.ResourceRef{URI: "s3://bucket/key"}
		_, err := registry.Resolve(context.Background(), ref)
		Expect(err).To(MatchError(ContainSubstring("no resolver registered")))
	})

	It("dispatches to the correct resolver by scheme", func() {
		mock := &mockResolver{scheme: "mock", data: []byte("result")}
		registry.Register(mock)

		ref := graph.ResourceRef{URI: "mock://test/path"}
		data, err := registry.Resolve(context.Background(), ref)
		Expect(err).NotTo(HaveOccurred())
		Expect(data).To(Equal([]byte("result")))
	})
})

var _ = Describe("SchemeFromURI", func() {
	It("extracts pg scheme", func() {
		Expect(graph.SchemeFromURI("pg://results/job-1/classify")).To(Equal("pg"))
	})

	It("extracts s3 scheme", func() {
		Expect(graph.SchemeFromURI("s3://bucket/key")).To(Equal("s3"))
	})

	It("returns empty for no scheme", func() {
		Expect(graph.SchemeFromURI("no-scheme")).To(BeEmpty())
	})
})

type mockResolver struct {
	scheme string
	data   []byte
	err    error
}

func (m *mockResolver) Resolve(_ context.Context, _ graph.ResourceRef) ([]byte, error) {
	return m.data, m.err
}

func (m *mockResolver) Scheme() string { return m.scheme }

var _ = Describe("PGResolver", func() {
	It("returns error for invalid pg URI", func() {
		resolver := graph.NewPGResolver(nil)
		_, err := resolver.Resolve(context.Background(), graph.ResourceRef{URI: "pg://results/"})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("invalid pg URI"))
	})

	It("returns error for URI missing analyzer segment", func() {
		resolver := graph.NewPGResolver(nil)
		_, err := resolver.Resolve(context.Background(), graph.ResourceRef{URI: "pg://results/job-1"})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("invalid pg URI path"))
	})
})

var _ = Describe("PGResolver tenant scoping", func() {
	It("returns the persisted row when ctx carries the writing tenant", func() {
		ctx, err := tenant.WithTenant(context.Background(), "tenant-a")
		Expect(err).NotTo(HaveOccurred())

		var capturedArgs []any
		tx := &dbtest.Tx{
			QueryRowFunc: func(_ context.Context, _ string, args ...any) db.Row {
				capturedArgs = args
				return dbtest.Row(nil, []byte(`[]`))
			},
		}
		conn := &dbtest.Conn{Tx: tx}
		r := graph.NewPGResolver(conn)

		data, err := r.Resolve(ctx, graph.ResourceRef{URI: "pg://results/job-1/requires"})
		Expect(err).NotTo(HaveOccurred())
		Expect(data).To(Equal([]byte(`[]`)))
		Expect(tx.Commits()).To(Equal(1))
		Expect(capturedArgs).To(HaveLen(3), "query must bind job_id, analyzer_name, and tenant_id")
		Expect(capturedArgs[2]).To(Equal("tenant-a"), "third bound arg must be the tenant_id from context")
	})

	It("surfaces sql.ErrNoRows when ctx carries a different tenant than the row owner", func() {
		ctx, err := tenant.WithTenant(context.Background(), "tenant-b")
		Expect(err).NotTo(HaveOccurred())

		var capturedArgs []any
		tx := &dbtest.Tx{
			QueryRowFunc: func(_ context.Context, _ string, args ...any) db.Row {
				capturedArgs = args
				return dbtest.Row(sql.ErrNoRows)
			},
		}
		conn := &dbtest.Conn{Tx: tx}
		r := graph.NewPGResolver(conn)

		_, err = r.Resolve(ctx, graph.ResourceRef{URI: "pg://results/job-1/requires"})
		Expect(err).To(HaveOccurred())
		Expect(errors.Is(err, sql.ErrNoRows)).To(BeTrue())
		Expect(tx.Commits()).To(Equal(0))
		Expect(tx.Rollbacks()).To(Equal(1))
		Expect(capturedArgs).To(HaveLen(3), "query must bind job_id, analyzer_name, and tenant_id")
		Expect(capturedArgs[2]).To(Equal("tenant-b"), "third bound arg must be the tenant_id from context")
	})

	It("fails loudly when ctx has no tenant at all", func() {
		// Resolve extracts the tenant from ctx before ever touching the
		// connection, so no Begin call happens here — a nil conn confirms it.
		r := graph.NewPGResolver(nil)

		_, err := r.Resolve(context.Background(), graph.ResourceRef{URI: "pg://results/job-1/requires"})
		Expect(errors.Is(err, tenant.ErrNoTenant)).To(BeTrue())
	})

	It("fails loudly when the connection rejects a tenant-bearing ctx", func() {
		ctx, err := tenant.WithTenant(context.Background(), "tenant-a")
		Expect(err).NotTo(HaveOccurred())

		conn := &dbtest.Conn{
			BeginFunc: func(_ context.Context) (db.Transaction, error) {
				return nil, db.ErrTenantRequired
			},
		}
		r := graph.NewPGResolver(conn)

		_, err = r.Resolve(ctx, graph.ResourceRef{URI: "pg://results/job-1/requires"})
		Expect(errors.Is(err, db.ErrTenantRequired)).To(BeTrue())
	})
})

var _ = Describe("PGResolver synthesis stage", func() {
	var ctx context.Context

	BeforeEach(func() {
		var err error
		ctx, err = tenant.WithTenant(context.Background(), "tenant-a")
		Expect(err).NotTo(HaveOccurred())
	})

	It("serves the job's relationship vote summaries as synthesized mappings, bound to the ctx tenant", func() {
		var capturedArgs []any
		tx := &dbtest.Tx{
			QueryFunc: func(_ context.Context, _ string, args ...any) (db.Rows, error) {
				capturedArgs = args
				return dbtest.Rows(nil,
					[]any{"c1", "c2", "EQUIVALENT", 0.9, 0.7},
					[]any{"c2", "c3", "SUBSET_OF", 0.8, 0.0}), nil
			},
		}
		r := graph.NewPGResolver(&dbtest.Conn{Tx: tx})

		data, err := r.Resolve(ctx, graph.ResourceRef{URI: "pg://results/job-1/synthesis"})
		Expect(err).NotTo(HaveOccurred())
		Expect(data).To(MatchJSON(`[
			{"source_id":"c1","target_id":"c2","relationship_type":"EQUIVALENT","confidence":0.9,"viability":0.7,"is_viable":true},
			{"source_id":"c2","target_id":"c3","relationship_type":"SUBSET_OF","confidence":0.8,"viability":0,"is_viable":false}]`))
		Expect(capturedArgs).To(Equal([]any{"tenant-a", "job-1", "relationship"}))
		Expect(tx.Commits()).To(Equal(1))
	})

	It("serves an empty array, not null, when the job has no vote summaries", func() {
		tx := &dbtest.Tx{
			QueryFunc: func(context.Context, string, ...any) (db.Rows, error) { return dbtest.Rows(nil), nil },
		}
		data, err := graph.NewPGResolver(&dbtest.Conn{Tx: tx}).Resolve(ctx, graph.ResourceRef{URI: "pg://results/job-1/synthesis"})
		Expect(err).NotTo(HaveOccurred())
		Expect(data).To(MatchJSON(`[]`))
	})

	DescribeTable("rolls back and names the job when reading fails",
		func(queryFunc func(context.Context, string, ...any) (db.Rows, error), wantMsg string) {
			tx := &dbtest.Tx{QueryFunc: queryFunc}
			_, err := graph.NewPGResolver(&dbtest.Conn{Tx: tx}).Resolve(ctx, graph.ResourceRef{URI: "pg://results/job-1/synthesis"})
			Expect(err).To(MatchError(ContainSubstring("pg resolve job job-1 analyzer synthesis")))
			Expect(err).To(MatchError(ContainSubstring(wantMsg)))
			Expect(tx.Commits()).To(BeZero())
			Expect(tx.Rollbacks()).To(Equal(1))
		},
		Entry("the query fails",
			func(context.Context, string, ...any) (db.Rows, error) { return nil, errors.New("connection lost") },
			"connection lost"),
		Entry("iterating rows fails",
			func(context.Context, string, ...any) (db.Rows, error) {
				return dbtest.Rows(errors.New("cursor broke")), nil
			},
			"cursor broke"),
	)
})
