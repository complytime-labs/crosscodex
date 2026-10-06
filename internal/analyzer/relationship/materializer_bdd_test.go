package relationship_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/analyzer/relationship"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/graphdb/memdriver"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// failingCreateEdge wraps a memdriver graph and fails CreateEdge for edges
// leaving one source node.
type failingCreateEdge struct {
	graphdb.GraphDB
	sourceID string
	err      error
}

func (f failingCreateEdge) CreateEdge(ctx context.Context, tenantID, sourceID, targetID string, edge graphdb.Edge) error {
	if sourceID == f.sourceID {
		return f.err
	}
	return f.GraphDB.CreateEdge(ctx, tenantID, sourceID, targetID, edge)
}

// newControlGraph returns a memdriver graph for tenant holding a Control node
// for each ID, as catalog ingest would have created them.
func newControlGraph(ctx context.Context, tenantID string, controlIDs ...string) graphdb.GraphDB {
	g := memdriver.New()
	Expect(g.CreateGraph(ctx, tenantID)).To(Succeed())
	for _, id := range controlIDs {
		Expect(g.CreateNode(ctx, tenantID, graphdb.Node{ID: id, Label: "Control", ValidFrom: time.Now()})).To(Succeed())
	}
	return g
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

// List returns matching keys sorted, as the local and S3 providers do, so
// specs that depend on file order are deterministic.
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
	slices.SortFunc(out, func(a, b storage.ObjectMetadata) int { return strings.Compare(a.Key, b.Key) })
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
	const tenantID = "test-tenant"

	var (
		graph graphdb.GraphDB
		store *mockStorage
		mat   *relationship.GraphMaterializer
		ctx   context.Context
	)

	matches := func() []graphdb.Relationship {
		rels, err := graph.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{EdgeLabel: "SEMANTIC_MATCH"})
		Expect(err).NotTo(HaveOccurred())
		return rels
	}

	storePairs := func(jobID string, pairs ...relationship.PairResult) {
		for _, p := range pairs {
			data, err := json.Marshal(p)
			Expect(err).NotTo(HaveOccurred())
			key := fmt.Sprintf("%s/analysis/relationship/%s/%s--%s.json", tenantID, jobID, p.SourceControlID, p.TargetControlID)
			store.data[key] = data
		}
	}

	BeforeEach(func() {
		ctx = testspecs.SetupTenantContext(tenantID)
		graph = newControlGraph(ctx, tenantID, "AC-1", "IT-3.2", "AC-2", "IT-4.1")
		store = newMockStorage()
		mat = relationship.NewGraphMaterializer(graph, store, config.RelationshipConfig{})
	})

	Context("Materialize", func() {
		It("reads pair results from storage and creates graph edges", func() {
			storePairs("job-1", relationship.PairResult{
				SourceControlID: "AC-1",
				TargetControlID: "IT-3.2",
				Consensus: relationship.Consensus{
					Relationship:       relationship.RelSupersetOf,
					ContributionType:   relationship.ContribIntegralTo,
					ConfidenceFraction: 1.0,
					Unanimous:          true,
					ValidVoteCount:     2,
				},
				SimilarityScore: 87.3,
			})

			Expect(mat.Materialize(ctx, tenantID, "job-1")).To(Succeed())

			rels := matches()
			Expect(rels).To(HaveLen(1))
			r := rels[0]
			Expect(r.Source.ID).To(Equal("AC-1"))
			Expect(r.Target.ID).To(Equal("IT-3.2"))
			// Not internal/graph's "semantic-match" kind: the two writers attach
			// different payloads, so a shared ID would let the first silently win.
			Expect(r.Edge.ID).To(Equal(graphdb.DerivedID("semantic-match-rebuild", "job-1", "AC-1", "IT-3.2")))
			Expect(r.Edge.Label).To(Equal("SEMANTIC_MATCH"))
			Expect(r.Edge.DeterminationType).To(Equal("llm_panel"))
			Expect(r.Edge.DeterminedBy).To(Equal("job-1"))
			Expect(r.Edge.Confidence).To(Equal(1.0))
			Expect(r.Edge.Properties).To(HaveKeyWithValue("relationship_type", "SUPERSET_OF"))
			Expect(r.Edge.Properties).To(HaveKeyWithValue("unanimous", true))
			Expect(r.Edge.Properties).To(HaveKeyWithValue("contribution_type", "INTEGRAL_TO"))
			// memdriver stores ints as float64, as AGE reads numbers back.
			Expect(r.Edge.Properties).To(HaveKeyWithValue("valid_vote_count", float64(2)))
			// SimilarityScore is a float32; the materializer widens it to float64.
			Expect(r.Edge.Properties).To(HaveKeyWithValue("similarity_score", float64(float32(87.3))))
		})

		It("creates edges for multiple pair results", func() {
			storePairs("job-2",
				relationship.PairResult{SourceControlID: "AC-1", TargetControlID: "IT-3.2",
					Consensus: relationship.Consensus{Relationship: relationship.RelSupersetOf, ConfidenceFraction: 1.0}},
				relationship.PairResult{SourceControlID: "AC-2", TargetControlID: "IT-4.1",
					Consensus: relationship.Consensus{Relationship: relationship.RelEquivalent, ConfidenceFraction: 0.667}},
			)

			Expect(mat.Materialize(ctx, tenantID, "job-2")).To(Succeed())

			rels := matches()
			Expect(rels).To(HaveLen(2))
			pairs := map[string]bool{}
			for _, r := range rels {
				pairs[r.Source.ID+"--"+r.Target.ID] = true
			}
			Expect(pairs).To(Equal(map[string]bool{"AC-1--IT-3.2": true, "AC-2--IT-4.1": true}))
		})

		It("re-materializing the same job into the same graph adds no edges", func() {
			storePairs("job-1", relationship.PairResult{
				SourceControlID: "AC-1",
				TargetControlID: "IT-3.2",
				Consensus: relationship.Consensus{
					Relationship:       relationship.RelSupersetOf,
					ConfidenceFraction: 1.0,
				},
			})

			Expect(mat.Materialize(ctx, tenantID, "job-1")).To(Succeed())
			Expect(matches()).To(HaveLen(1))

			Expect(mat.Materialize(ctx, tenantID, "job-1")).To(Succeed())
			second := matches()
			Expect(second).To(HaveLen(1))
			Expect(second[0].Edge.ID).To(Equal(graphdb.DerivedID("semantic-match-rebuild", "job-1", "AC-1", "IT-3.2")))
		})

		It("counts only the edges it creates across runs", func() {
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { Expect(tp.Shutdown(context.Background())).To(Succeed()) })
			instrumented := relationship.NewGraphMaterializer(graph, store, config.RelationshipConfig{},
				relationship.WithMaterializerTelemetry(tp.TracerProvider(), tp.MeterProvider()))
			storePairs("job-1", relationship.PairResult{SourceControlID: "AC-1", TargetControlID: "IT-3.2",
				Consensus: relationship.Consensus{Relationship: relationship.RelSupersetOf, ConfidenceFraction: 1.0}})

			Expect(instrumented.Materialize(ctx, tenantID, "job-1")).To(Succeed())
			Expect(instrumented.Materialize(ctx, tenantID, "job-1")).To(Succeed())

			counter := telemetrytest.FindMetric(tp.GetMetrics(), "relationship.edges.materialized")
			Expect(counter).NotTo(BeNil())
			Expect(telemetrytest.CounterValue(counter)).To(Equal(int64(1)))
			Expect(matches()).To(HaveLen(1))
		})

		It("returns zero edges for empty prefix listing", func() {
			Expect(mat.Materialize(ctx, tenantID, "job-empty")).To(Succeed())
			Expect(matches()).To(BeEmpty())
		})

		It("fails on corrupt JSON", func() {
			store.data["test-tenant/analysis/relationship/job-bad/corrupt.json"] = []byte("{not json")
			err := mat.Materialize(ctx, tenantID, "job-bad")
			Expect(err).To(MatchError(ContainSubstring("parsing")))
			Expect(matches()).To(BeEmpty())
		})

		// Pairs are written one at a time, so a failure keeps the pairs written
		// before it; re-running the job completes it because edge IDs are derived.
		It("propagates graph errors, keeping the pairs written before the failure, and a re-run completes the job", func() {
			storePairs("job-1",
				relationship.PairResult{SourceControlID: "AC-1", TargetControlID: "IT-3.2",
					Consensus: relationship.Consensus{Relationship: relationship.RelEquivalent}},
				relationship.PairResult{SourceControlID: "AC-2", TargetControlID: "IT-4.1",
					Consensus: relationship.Consensus{Relationship: relationship.RelEquivalent}},
			)
			injected := errors.New("graph unavailable")
			failing := failingCreateEdge{GraphDB: graph, sourceID: "AC-2", err: injected}

			err := relationship.NewGraphMaterializer(failing, store, config.RelationshipConfig{}).Materialize(ctx, tenantID, "job-1")
			Expect(err).To(MatchError(injected))
			Expect(err.Error()).To(ContainSubstring("creating edge AC-2--IT-4.1"))

			rels := matches()
			Expect(rels).To(HaveLen(1))
			Expect(rels[0].Source.ID).To(Equal("AC-1"))
			Expect(rels[0].Target.ID).To(Equal("IT-3.2"))
			firstEdge := rels[0].Edge

			Expect(mat.Materialize(ctx, tenantID, "job-1")).To(Succeed())

			byPair := map[string]graphdb.Edge{}
			for _, r := range matches() {
				byPair[r.Source.ID+"--"+r.Target.ID] = r.Edge
			}
			Expect(byPair).To(HaveLen(2))
			Expect(byPair).To(HaveKeyWithValue("AC-1--IT-3.2", firstEdge), "the re-run must keep, not rewrite, the edge written before the failure")
			Expect(byPair).To(HaveKey("AC-2--IT-4.1"))
			Expect(byPair["AC-2--IT-4.1"].ID).To(Equal(graphdb.DerivedID("semantic-match-rebuild", "job-1", "AC-2", "IT-4.1")))
		})

		It("propagates storage errors", func() {
			store.err = fmt.Errorf("storage unavailable")
			err := mat.Materialize(ctx, tenantID, "job-1")
			Expect(err).To(MatchError(ContainSubstring("storage unavailable")))
			Expect(matches()).To(BeEmpty())
		})

		DescribeTable("rejects an invalid tenant ID before touching storage or the graph",
			func(badTenant, wantText string) {
				storePairs("job-1", relationship.PairResult{SourceControlID: "AC-1", TargetControlID: "IT-3.2",
					Consensus: relationship.Consensus{Relationship: relationship.RelSupersetOf, ConfidenceFraction: 1.0}})
				err := mat.Materialize(ctx, badTenant, "job-1")
				Expect(err).To(MatchError(tenant.ErrInvalidTenant))
				Expect(err.Error()).To(HavePrefix("materializer.Materialize: tenant ID"),
					"the materializer must reject the tenant itself, not leave it to the driver")
				Expect(err.Error()).To(ContainSubstring(wantText))
				Expect(matches()).To(BeEmpty())
			},
			Entry("empty", "", "tenant ID must not be empty"),
			Entry("uppercase", "INVALID", tenant.IDRule),
			Entry("53 characters", "t"+strings.Repeat("a", 52), tenant.IDRule),
		)
	})
})
