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
		tx := &fakeTransaction{
			queryRowFunc: func(_ context.Context, _ string, args ...any) db.Row {
				capturedArgs = args
				return &fakeRow{scanFunc: func(dest ...any) error {
					*dest[0].(*[]byte) = []byte(`[]`)
					return nil
				}}
			},
		}
		conn := &fakeTenantConnection{
			beginFunc: func(_ context.Context) (db.Transaction, error) {
				return tx, nil
			},
		}
		r := graph.NewPGResolver(conn)

		data, err := r.Resolve(ctx, graph.ResourceRef{URI: "pg://results/job-1/requires"})
		Expect(err).NotTo(HaveOccurred())
		Expect(data).To(Equal([]byte(`[]`)))
		Expect(tx.committed).To(BeTrue())
		Expect(capturedArgs).To(HaveLen(3), "query must bind job_id, analyzer_name, and tenant_id")
		Expect(capturedArgs[2]).To(Equal("tenant-a"), "third bound arg must be the tenant_id from context")
	})

	It("surfaces sql.ErrNoRows when ctx carries a different tenant than the row owner", func() {
		ctx, err := tenant.WithTenant(context.Background(), "tenant-b")
		Expect(err).NotTo(HaveOccurred())

		var capturedArgs []any
		tx := &fakeTransaction{
			queryRowFunc: func(_ context.Context, _ string, args ...any) db.Row {
				capturedArgs = args
				return &fakeRow{scanFunc: func(_ ...any) error {
					return sql.ErrNoRows
				}}
			},
		}
		conn := &fakeTenantConnection{
			beginFunc: func(_ context.Context) (db.Transaction, error) {
				return tx, nil
			},
		}
		r := graph.NewPGResolver(conn)

		_, err = r.Resolve(ctx, graph.ResourceRef{URI: "pg://results/job-1/requires"})
		Expect(err).To(HaveOccurred())
		Expect(errors.Is(err, sql.ErrNoRows)).To(BeTrue())
		Expect(tx.committed).To(BeFalse())
		Expect(tx.rolledBack).To(BeTrue())
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

		conn := &fakeTenantConnection{
			beginFunc: func(_ context.Context) (db.Transaction, error) {
				return nil, db.ErrTenantRequired
			},
		}
		r := graph.NewPGResolver(conn)

		_, err = r.Resolve(ctx, graph.ResourceRef{URI: "pg://results/job-1/requires"})
		Expect(errors.Is(err, db.ErrTenantRequired)).To(BeTrue())
	})
})

// fakeTenantConnection is a package-local minimal test double for
// db.TenantConnection. internal/graph cannot import internal/pipeline's
// test-only mocks, so this mirrors the shape used there.
type fakeTenantConnection struct {
	beginFunc func(ctx context.Context) (db.Transaction, error)
}

func (f *fakeTenantConnection) Begin(ctx context.Context) (db.Transaction, error) {
	if f.beginFunc != nil {
		return f.beginFunc(ctx)
	}
	return nil, errors.New("not implemented")
}

func (f *fakeTenantConnection) Query(_ context.Context, _ string, _ ...any) (db.Rows, error) {
	return nil, errors.New("Query not allowed on TenantConnection outside transaction")
}

func (f *fakeTenantConnection) QueryRow(_ context.Context, _ string, _ ...any) db.Row {
	panic("QueryRow not allowed on TenantConnection outside transaction")
}

func (f *fakeTenantConnection) Exec(_ context.Context, _ string, _ ...any) error {
	return errors.New("Exec not allowed on TenantConnection outside transaction")
}

func (f *fakeTenantConnection) Close() error { return nil }

type fakeTransaction struct {
	queryRowFunc func(ctx context.Context, query string, args ...any) db.Row
	commitFunc   func() error
	rollbackFunc func() error
	committed    bool
	rolledBack   bool
}

func (f *fakeTransaction) Commit() error {
	f.committed = true
	if f.commitFunc != nil {
		return f.commitFunc()
	}
	return nil
}

func (f *fakeTransaction) Rollback() error {
	if f.committed {
		// Mirrors real driver behavior: Rollback after a successful
		// Commit is a no-op, not a second rollback.
		return nil
	}
	f.rolledBack = true
	if f.rollbackFunc != nil {
		return f.rollbackFunc()
	}
	return nil
}

func (f *fakeTransaction) Query(_ context.Context, _ string, _ ...any) (db.Rows, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeTransaction) QueryRow(ctx context.Context, query string, args ...any) db.Row {
	if f.queryRowFunc != nil {
		return f.queryRowFunc(ctx, query, args...)
	}
	panic("not implemented")
}

func (f *fakeTransaction) Exec(_ context.Context, _ string, _ ...any) error {
	return errors.New("not implemented")
}

// fakeRow is a minimal test double for db.Row.
type fakeRow struct {
	scanFunc func(dest ...any) error
}

func (r *fakeRow) Scan(dest ...any) error {
	return r.scanFunc(dest...)
}
