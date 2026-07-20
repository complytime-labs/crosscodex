package graph_test

import (
	"context"
	"database/sql"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/graph"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
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
		db := (*sql.DB)(nil)
		resolver := graph.NewPGResolver(db)
		_, err := resolver.Resolve(context.Background(), graph.ResourceRef{URI: "pg://results/"})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("invalid pg URI"))
	})

	It("returns error for URI missing analyzer segment", func() {
		db := (*sql.DB)(nil)
		resolver := graph.NewPGResolver(db)
		_, err := resolver.Resolve(context.Background(), graph.ResourceRef{URI: "pg://results/job-1"})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("invalid pg URI path"))
	})
})
