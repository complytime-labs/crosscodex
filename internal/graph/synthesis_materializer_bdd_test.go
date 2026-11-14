package graph_test

import (
	"context"
	"encoding/json"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/graph"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
)

// bulkRecordingGraph records the size of each BulkCreateEdges call and can
// fail it with a driver error that is neither a missing node nor a duplicate.
type bulkRecordingGraph struct {
	graphdb.GraphDB
	bulkSizes []int
	bulkErr   error
}

func (g *bulkRecordingGraph) BulkCreateEdges(ctx context.Context, tenant string, edges []graphdb.BulkEdge) ([]string, error) {
	g.bulkSizes = append(g.bulkSizes, len(edges))
	if g.bulkErr != nil {
		return nil, &graphdb.BulkEdgeError{Index: 0, Err: g.bulkErr}
	}
	return g.GraphDB.BulkCreateEdges(ctx, tenant, edges)
}

var _ = Describe("Synthesis materialization", func() {
	const tenantID = "test-tenant"

	var (
		ctx      context.Context
		gdb      *bulkRecordingGraph
		resolver *mockResolver
		svc      *graph.Service
	)

	mapping := func(src, tgt, rel string) map[string]any {
		return map[string]any{"source_id": src, "target_id": tgt, "relationship_type": rel,
			"confidence": 0.8, "viability": 0.6, "is_viable": true}
	}

	newService := func(opts ...graph.Option) {
		registry := graph.NewResolverRegistry()
		registry.Register(resolver)
		svc = graph.New(gdb, &mockVectorDB{}, nil, append(opts, graph.WithResolverRegistry(registry))...)
	}

	deliver := func(stage string, rows any) error {
		data, err := json.Marshal(rows)
		Expect(err).NotTo(HaveOccurred())
		resolver.data = data
		eventData, err := json.Marshal(map[string]string{"analyzer": stage, "job_id": "job-1", "stage": "completed"})
		Expect(err).NotTo(HaveOccurred())
		return graph.ExportHandleEvent(svc, ctx, &natsbus.Message{
			Subject: "crosscodex.pipeline." + tenantID + ".job-1.stage.completed",
			Data:    eventData,
		})
	}

	edges := func() []graphdb.Relationship {
		rels, err := gdb.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{EdgeLabel: results.SemanticMatchEdgeLabel})
		Expect(err).NotTo(HaveOccurred())
		return rels
	}

	BeforeEach(func() {
		ctx = context.Background()
		gdb = &bulkRecordingGraph{GraphDB: newMemGraph(ctx, tenantID, "ctrl-1", "ctrl-2", "ctrl-3")}
		resolver = &mockResolver{scheme: "pg"}
		newService()
	})

	It("writes one synthesized SEMANTIC_MATCH edge per mapping with its stage, viability and confidence", func() {
		Expect(deliver("synthesis", []map[string]any{mapping("ctrl-1", "ctrl-2", "EQUIVALENT")})).To(Succeed())

		rels := edges()
		Expect(rels).To(HaveLen(1))
		Expect(rels[0].Source.ID).To(Equal("ctrl-1"))
		Expect(rels[0].Target.ID).To(Equal("ctrl-2"))
		Expect(rels[0].Edge.ID).To(Equal(graphdb.DerivedID(results.SemanticMatchSynthesizedIDKind, "job-1", "ctrl-1", "ctrl-2")))
		Expect(rels[0].Edge.Confidence).To(BeNumerically("~", 0.8, 0.001))
		Expect(rels[0].Edge.Properties).To(HaveKeyWithValue("relationship_type", "EQUIVALENT"))
		Expect(rels[0].Edge.Properties).To(HaveKeyWithValue("job_id", "job-1"))
		Expect(rels[0].Edge.Properties).To(HaveKeyWithValue("stage", "synthesized"))
		Expect(rels[0].Edge.Properties).To(HaveKeyWithValue("viability", BeNumerically("~", 0.6, 0.001)))
		Expect(rels[0].Edge.Properties).To(HaveKeyWithValue("is_viable", true))
	})

	It("puts a critiqued mapping's vote counts and note on its synthesized edge, and none on an uncritiqued one", func() {
		critiqued := mapping("ctrl-1", "ctrl-2", "EQUIVALENT")
		critiqued["critique"] = results.CritiqueResult{Kind: results.RelationshipAnalyzer, SourceControlID: "ctrl-1", TargetControlID: "ctrl-2",
			Upheld: 2, Rejected: 1, Outcome: results.CritiqueSplit, Reasons: []string{"panel-1 s0 REJECT: too broad"}}
		Expect(deliver("synthesis", []map[string]any{critiqued, mapping("ctrl-2", "ctrl-3", "EQUIVALENT")})).To(Succeed())

		props := map[string]map[string]any{}
		for _, r := range edges() {
			props[r.Source.ID] = r.Edge.Properties
		}
		Expect(props["ctrl-1"]).To(HaveKeyWithValue(results.CritiqueUpheldProperty, BeNumerically("==", 2)))
		Expect(props["ctrl-1"]).To(HaveKeyWithValue(results.CritiqueRejectedProperty, BeNumerically("==", 1)))
		Expect(props["ctrl-1"]).To(HaveKeyWithValue(results.CritiqueNoteProperty, "critique split: 1 of 3 rejected — panel-1 s0: too broad"))
		Expect(props["ctrl-2"]).NotTo(HaveKey(results.CritiqueUpheldProperty))
		Expect(props["ctrl-2"]).NotTo(HaveKey(results.CritiqueNoteProperty))
	})

	It("keeps a pair's analyzer edge and synthesized edge apart", func() {
		Expect(deliver("relationship", []map[string]any{mapping("ctrl-1", "ctrl-2", "EQUIVALENT")})).To(Succeed())
		Expect(deliver("synthesis", []map[string]any{mapping("ctrl-1", "ctrl-2", "SUBSET_OF")})).To(Succeed())

		stages := map[any]any{}
		for _, r := range edges() {
			stages[r.Edge.Properties["stage"]] = r.Edge.Properties["relationship_type"]
		}
		Expect(stages).To(Equal(map[any]any{"analyzer": "EQUIVALENT", "synthesized": "SUBSET_OF"}))
	})

	It("writes nothing for a job with no mappings", func() {
		Expect(deliver("synthesis", []map[string]any{})).To(Succeed())
		Expect(edges()).To(BeEmpty())
	})

	It("is idempotent when the same mappings are materialized twice", func() {
		rows := []map[string]any{mapping("ctrl-1", "ctrl-2", "EQUIVALENT"), mapping("ctrl-2", "ctrl-3", "SUBSET_OF")}
		Expect(deliver("synthesis", rows)).To(Succeed())
		Expect(deliver("synthesis", rows)).To(Succeed())
		Expect(edges()).To(HaveLen(2))
	})

	It("writes the new edges when a redelivery carries one already materialized", func() {
		Expect(deliver("synthesis", []map[string]any{mapping("ctrl-1", "ctrl-2", "EQUIVALENT")})).To(Succeed())
		Expect(deliver("synthesis", []map[string]any{
			mapping("ctrl-1", "ctrl-2", "EQUIVALENT"), mapping("ctrl-2", "ctrl-3", "SUBSET_OF"),
		})).To(Succeed())
		Expect(edges()).To(HaveLen(2))
	})

	It("skips a mapping whose endpoint is missing and still writes the rest", func() {
		Expect(deliver("synthesis", []map[string]any{
			mapping("ctrl-missing", "ctrl-2", "EQUIVALENT"), mapping("ctrl-1", "ctrl-2", "EQUIVALENT"),
		})).To(Succeed())

		rels := edges()
		Expect(rels).To(HaveLen(1))
		Expect(rels[0].Source.ID).To(Equal("ctrl-1"))
	})

	It("sends mappings in chunks of at most the configured bulk size", func() {
		newService(graph.WithMaxBulkEdges(2))
		Expect(deliver("synthesis", []map[string]any{
			mapping("ctrl-1", "ctrl-2", "EQUIVALENT"), mapping("ctrl-2", "ctrl-3", "EQUIVALENT"), mapping("ctrl-1", "ctrl-3", "EQUIVALENT"),
		})).To(Succeed())

		Expect(gdb.bulkSizes).To(Equal([]int{2, 1}))
		Expect(edges()).To(HaveLen(3))
	})

	It("fails the event, writing nothing, on a driver error other than a duplicate or missing node", func() {
		gdb.bulkErr = errors.New("connection reset by peer")
		err := deliver("synthesis", []map[string]any{mapping("ctrl-1", "ctrl-2", "EQUIVALENT")})

		Expect(err).To(MatchError(ContainSubstring("bulk create synthesized SEMANTIC_MATCH edges")))
		Expect(err).To(MatchError(ContainSubstring("connection reset by peer")))
		Expect(edges()).To(BeEmpty())
	})

	It("fails the event on malformed synthesis data", func() {
		resolver.data = []byte(`{not json`)
		eventData, err := json.Marshal(map[string]string{"analyzer": "synthesis", "job_id": "job-1", "stage": "completed"})
		Expect(err).NotTo(HaveOccurred())
		err = graph.ExportHandleEvent(svc, ctx, &natsbus.Message{
			Subject: "crosscodex.pipeline." + tenantID + ".job-1.stage.completed", Data: eventData,
		})
		Expect(err).To(MatchError(ContainSubstring("unmarshal synthesis results")))
		Expect(edges()).To(BeEmpty())
	})
})
