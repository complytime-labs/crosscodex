package graph_test

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/graph"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/graphdb/memdriver"
	"github.com/complytime-labs/crosscodex/pkg/natsbus/natsbustest"
)

// failNthEdgeOnce fails the nth CreateEdge call, once, with a driver error and
// passes every other call to the memdriver it embeds, so an event is cut off
// after a partial write.
type failNthEdgeOnce struct {
	graphdb.GraphDB
	n     int
	calls int
}

func (g *failNthEdgeOnce) CreateEdge(ctx context.Context, tenantID, sourceID, targetID string, edge graphdb.Edge) error {
	g.calls++
	if g.calls == g.n {
		return errors.New("connection reset by peer")
	}
	return g.GraphDB.CreateEdge(ctx, tenantID, sourceID, targetID, edge)
}

// Regression: redelivering a stage-completed event must not duplicate edges.
// The subscriber relies on the driver returning ErrEdgeExists for an
// already-materialized edge ID.
var _ = Describe("Subscriber re-materialization", func() {
	const tenantID = "test-tenant"

	var (
		ctx      context.Context
		gdb      graphdb.GraphDB
		resolver *mockResolver
		svc      *graph.Service
	)

	BeforeEach(func() {
		ctx = context.Background()
		gdb = memdriver.New()
		Expect(gdb.CreateGraph(ctx, tenantID)).To(Succeed())
		for _, id := range []string{"ctrl-1", "ctrl-2"} {
			Expect(gdb.CreateNode(ctx, tenantID, graphdb.Node{ID: id, Label: "Control", ValidFrom: time.Now().UTC()})).To(Succeed())
		}
		resolver = &mockResolver{scheme: "pg"}
		registry := graph.NewResolverRegistry()
		registry.Register(resolver)
		svc = graph.New(gdb, &mockVectorDB{}, nil, graph.WithResolverRegistry(registry))
	})

	deliver := func(analyzer string) error {
		return graph.ExportHandleEvent(svc, tenantCtx(tenantID), stageEvent(tenantID, analyzer, 1))
	}

	DescribeTable("yields one edge per label when the same results are materialized twice",
		func(analyzer string, edgeLabels []string, results []map[string]any) {
			data, err := json.Marshal(results)
			Expect(err).NotTo(HaveOccurred())
			resolver.data = data

			Expect(deliver(analyzer)).To(Succeed())
			Expect(deliver(analyzer)).To(Succeed())

			for _, edgeLabel := range edgeLabels {
				rels, err := gdb.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{EdgeLabel: edgeLabel})
				Expect(err).NotTo(HaveOccurred())
				Expect(rels).To(HaveLen(1), "expected exactly one %s relationship", edgeLabel)
			}
		},
		Entry("relationship results", "relationship", []string{"SEMANTIC_MATCH"}, []map[string]any{{
			"source_id": "ctrl-1", "target_id": "ctrl-2", "relationship_type": "EQUIVALENT", "confidence": 0.9,
		}}),
		Entry("requires results", "requires", []string{"REQUIRES"}, []map[string]any{{
			"source_id": "ctrl-1", "target_id": "ctrl-2", "confidence": 0.95, "unanimous": true,
			"valid_votes": 3, "total_votes": 3, "models": []string{"m1"},
		}}),
		Entry("artifacts results", "artifacts", []string{"DEMANDS", "IS_TYPE"}, []map[string]any{{
			"control_id": "ctrl-1",
			"artifacts": []map[string]any{{
				"name": "access-review-log", "type": "LOG", "frequency": "QUARTERLY",
				"owner_role": "SECURITY_ADMIN", "confidence": 0.9,
			}},
		}}),
	)

	It("writes each edge once when an event is redelivered after a partial write", func() {
		Expect(gdb.CreateNode(ctx, tenantID, graphdb.Node{ID: "ctrl-3", Label: "Control", ValidFrom: time.Now().UTC()})).To(Succeed())
		data, err := json.Marshal([]map[string]any{
			{"source_id": "ctrl-1", "target_id": "ctrl-2", "relationship_type": "EQUIVALENT", "confidence": 0.9},
			{"source_id": "ctrl-2", "target_id": "ctrl-3", "relationship_type": "EQUIVALENT", "confidence": 0.9},
			{"source_id": "ctrl-1", "target_id": "ctrl-3", "relationship_type": "EQUIVALENT", "confidence": 0.9},
		})
		Expect(err).NotTo(HaveOccurred())
		resolver.data = data
		bus := &natsbustest.Fake{}
		registry := graph.NewResolverRegistry()
		registry.Register(resolver)
		svc = graph.New(&failNthEdgeOnce{GraphDB: gdb, n: 2}, &mockVectorDB{}, bus, graph.WithResolverRegistry(registry))
		Expect(svc.Start(ctx)).To(Succeed())
		DeferCleanup(func() { _ = svc.Stop(context.Background()) })

		By("cutting the first delivery off after its first edge")
		Expect(bus.Deliver(stageEvent(tenantID, "relationship", 1))).NotTo(Succeed())
		rels, err := gdb.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{EdgeLabel: "SEMANTIC_MATCH"})
		Expect(err).NotTo(HaveOccurred())
		Expect(rels).To(HaveLen(1))

		By("completing the event on redelivery without duplicating the edge already written")
		Expect(bus.Deliver(stageEvent(tenantID, "relationship", 2))).To(Succeed())
		sub := consumerSub(bus, materializerConsumer)
		Expect(sub.Naked()).To(Equal(1))
		Expect(sub.Acked()).To(Equal(1))
		rels, err = gdb.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{EdgeLabel: "SEMANTIC_MATCH"})
		Expect(err).NotTo(HaveOccurred())
		ids := map[string]int{}
		for _, r := range rels {
			ids[r.Edge.ID]++
		}
		Expect(ids).To(HaveLen(3))
		for id, n := range ids {
			Expect(n).To(Equal(1), "edge %s written %d times", id, n)
		}
	})

	// Control IDs are tenant-supplied and may contain "_", so the edge ID
	// must not let ("a_b","c") and ("a","b_c") collide and drop one edge.
	It("keeps SEMANTIC_MATCH edges whose endpoints differ only in where an underscore splits them", func() {
		for _, id := range []string{"a_b", "c", "a", "b_c"} {
			Expect(gdb.CreateNode(ctx, tenantID, graphdb.Node{ID: id, Label: "Control", ValidFrom: time.Now().UTC()})).To(Succeed())
		}
		data, err := json.Marshal([]map[string]any{
			{"source_id": "a_b", "target_id": "c", "relationship_type": "EQUIVALENT", "confidence": 0.9},
			{"source_id": "a", "target_id": "b_c", "relationship_type": "EQUIVALENT", "confidence": 0.9},
		})
		Expect(err).NotTo(HaveOccurred())
		resolver.data = data

		Expect(deliver("relationship")).To(Succeed())

		rels, err := gdb.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{EdgeLabel: "SEMANTIC_MATCH"})
		Expect(err).NotTo(HaveOccurred())
		Expect(rels).To(HaveLen(2), "one SEMANTIC_MATCH edge was silently dropped as a duplicate")
	})
})
