package relationship_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/analyzer/relationship"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/storage"
)

// mockGraphDB captures CreateEdge calls.
type mockGraphDB struct {
	edges []graphdb.Edge
	err   error
}

func (m *mockGraphDB) CreateGraph(_ context.Context, _ string) error                { return nil }
func (m *mockGraphDB) CreateNode(_ context.Context, _ string, _ graphdb.Node) error { return nil }
func (m *mockGraphDB) CreateEdge(_ context.Context, _ string, edge graphdb.Edge) error {
	if m.err != nil {
		return m.err
	}
	m.edges = append(m.edges, edge)
	return nil
}
func (m *mockGraphDB) CreateRequiresEdge(_ context.Context, _ string, _ graphdb.RequiresEdge) error {
	return nil
}
func (m *mockGraphDB) QueryRelationships(_ context.Context, _ string, _ graphdb.RelationshipQuery) ([]graphdb.Relationship, error) {
	return nil, nil
}
func (m *mockGraphDB) Traverse(_ context.Context, _ string, _ graphdb.TraversalQuery) ([]graphdb.Path, error) {
	return nil, nil
}
func (m *mockGraphDB) QueryAsOf(_ context.Context, _ string, _ graphdb.RelationshipQuery, _ time.Time) ([]graphdb.Relationship, error) {
	return nil, nil
}

// mockStorage stores data in memory keyed by path.
type mockStorage struct {
	data map[string][]byte
	err  error
}

func newMockStorage() *mockStorage {
	return &mockStorage{data: make(map[string][]byte)}
}

func (m *mockStorage) Get(_ context.Context, key string) (io.ReadCloser, error) {
	if m.err != nil {
		return nil, m.err
	}
	d, ok := m.data[key]
	if !ok {
		return nil, fmt.Errorf("key not found: %s", key)
	}
	return io.NopCloser(strings.NewReader(string(d))), nil
}

func (m *mockStorage) Put(_ context.Context, key string, data io.Reader) error {
	if m.err != nil {
		return m.err
	}
	b, err := io.ReadAll(data)
	if err != nil {
		return err
	}
	m.data[key] = b
	return nil
}

func (m *mockStorage) Delete(_ context.Context, _ string) error { return nil }
func (m *mockStorage) List(_ context.Context, prefix string) ([]storage.ObjectMetadata, error) {
	if m.err != nil {
		return nil, m.err
	}
	var out []storage.ObjectMetadata
	for k := range m.data {
		if strings.HasPrefix(k, prefix) {
			out = append(out, storage.ObjectMetadata{Key: k})
		}
	}
	return out, nil
}
func (m *mockStorage) Exists(_ context.Context, key string) (bool, error) {
	_, ok := m.data[key]
	return ok, nil
}
func (m *mockStorage) Stat(_ context.Context, key string) (*storage.ObjectMetadata, error) {
	if _, ok := m.data[key]; ok {
		return &storage.ObjectMetadata{Key: key}, nil
	}
	return nil, fmt.Errorf("not found")
}
func (m *mockStorage) Close() error { return nil }

var _ = Describe("GraphMaterializer", func() {
	var (
		graph *mockGraphDB
		store *mockStorage
		mat   *relationship.GraphMaterializer
		ctx   context.Context
	)

	BeforeEach(func() {
		ctx = testspecs.SetupTenantContext("test-tenant")
		graph = &mockGraphDB{}
		store = newMockStorage()
		mat = relationship.NewGraphMaterializer(graph, store, config.RelationshipConfig{})
	})

	Context("Materialize", func() {
		It("reads pair results from storage and creates graph edges", func() {
			pair := relationship.PairResult{
				SourceControlID: "AC-1",
				TargetControlID: "IT-3.2",
				Consensus: relationship.Consensus{
					Relationship:       relationship.RelSupersetOf,
					ConfidenceFraction: 1.0,
					Unanimous:          true,
					ValidVoteCount:     2,
				},
				SimilarityScore: 87.3,
			}
			data, err := json.Marshal(pair)
			Expect(err).NotTo(HaveOccurred())
			store.data["test-tenant/analysis/relationship/job-1/AC-1--IT-3.2.json"] = data

			err = mat.Materialize(ctx, "test-tenant", "job-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(graph.edges).To(HaveLen(1))

			edge := graph.edges[0]
			Expect(edge.Label).To(Equal("SEMANTIC_MATCH"))
			Expect(edge.Source).To(Equal("AC-1"))
			Expect(edge.Target).To(Equal("IT-3.2"))
			Expect(edge.DeterminationType).To(Equal("llm_panel"))
			Expect(edge.DeterminedBy).To(Equal("job-1"))
			Expect(edge.Confidence).To(Equal(1.0))
			Expect(edge.Properties["relationship_type"]).To(Equal("SUPERSET_OF"))
			Expect(edge.Properties["unanimous"]).To(Equal(true))
		})

		It("creates edges for multiple pair results", func() {
			for _, p := range []relationship.PairResult{
				{SourceControlID: "AC-1", TargetControlID: "IT-3.2",
					Consensus: relationship.Consensus{Relationship: relationship.RelSupersetOf, ConfidenceFraction: 1.0}},
				{SourceControlID: "AC-2", TargetControlID: "IT-4.1",
					Consensus: relationship.Consensus{Relationship: relationship.RelEquivalent, ConfidenceFraction: 0.667}},
			} {
				data, _ := json.Marshal(p)
				key := fmt.Sprintf("test-tenant/analysis/relationship/job-2/%s--%s.json", p.SourceControlID, p.TargetControlID)
				store.data[key] = data
			}

			err := mat.Materialize(ctx, "test-tenant", "job-2")
			Expect(err).NotTo(HaveOccurred())
			Expect(graph.edges).To(HaveLen(2))
		})

		It("returns zero edges for empty prefix listing", func() {
			err := mat.Materialize(ctx, "test-tenant", "job-empty")
			Expect(err).NotTo(HaveOccurred())
			Expect(graph.edges).To(BeEmpty())
		})

		It("fails on corrupt JSON", func() {
			store.data["test-tenant/analysis/relationship/job-bad/corrupt.json"] = []byte("{not json")
			err := mat.Materialize(ctx, "test-tenant", "job-bad")
			Expect(err).To(MatchError(ContainSubstring("parsing")))
		})

		It("propagates graph errors", func() {
			pair := relationship.PairResult{
				SourceControlID: "AC-1",
				TargetControlID: "IT-3.2",
				Consensus: relationship.Consensus{
					Relationship: relationship.RelEquivalent,
				},
			}
			data, _ := json.Marshal(pair)
			store.data["test-tenant/analysis/relationship/job-1/AC-1--IT-3.2.json"] = data

			graph.err = fmt.Errorf("graph unavailable")
			err := mat.Materialize(ctx, "test-tenant", "job-1")
			Expect(err).To(MatchError(ContainSubstring("graph unavailable")))
		})

		It("propagates storage errors", func() {
			store.err = fmt.Errorf("storage unavailable")
			err := mat.Materialize(ctx, "test-tenant", "job-1")
			Expect(err).To(MatchError(ContainSubstring("storage unavailable")))
		})

		It("rejects invalid tenant ID", func() {
			err := mat.Materialize(ctx, "INVALID!", "job-1")
			Expect(err).To(MatchError(ContainSubstring("materializer.Materialize")))
		})
	})
})
