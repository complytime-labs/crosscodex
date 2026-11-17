package crosswalk_test

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
	"github.com/complytime-labs/crosscodex/pkg/crosswalk"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/graphdb/memdriver"
)

var _ = Describe("LoadPair", func() {
	const tenant = "tenant-load"
	var (
		ctx  context.Context
		mem  graphdb.GraphDB
		t0   = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		t1   = t0.Add(time.Hour)
		jobs = map[string]time.Time{"j1": t0, "j2": t1}
	)

	control := func(id, catalog, identifier string) {
		props := map[string]any{"catalog_id": catalog}
		if identifier != "" {
			props["identifier"] = identifier
		}
		Expect(mem.CreateNode(ctx, tenant, graphdb.Node{ID: id, Label: "Control", Properties: props, ValidFrom: t0})).To(Succeed())
	}
	edge := func(id, src, tgt string, props map[string]any) {
		Expect(mem.CreateEdge(ctx, tenant, src, tgt, graphdb.Edge{
			ID: id, Label: results.SemanticMatchEdgeLabel, Properties: props, ValidFrom: t0, Confidence: 0.8,
		})).To(Succeed())
	}
	query := func() crosswalk.PairQuery {
		return crosswalk.PairQuery{A: "catA", B: "catB", Jobs: jobs, Stage: results.StageAnalyzer}
	}

	BeforeEach(func() {
		ctx = context.Background()
		mem = memdriver.New()
		Expect(mem.CreateGraph(ctx, tenant)).To(Succeed())
		control("a1", "catA", "A-1")
		control("a2", "catA", "A-2")
		control("b1", "catB", "B-1")
		control("b2", "catB", "B-2")
	})

	It("reads a stored a-to-b edge as a direct verdict", func() {
		edge("e1", "a1", "b1", map[string]any{"job_id": "j1", "relationship_type": "SUBSET_OF"})
		pairs, skipped, err := crosswalk.LoadPair(ctx, mem, tenant, query())
		Expect(err).NotTo(HaveOccurred())
		Expect(skipped).To(BeZero())
		Expect(pairs).To(HaveLen(1))
		Expect(pairs[0].A.ID).To(Equal("a1"))
		Expect(pairs[0].B.ID).To(Equal("b1"))
		Expect(pairs[0].Verdicts).To(HaveLen(1))
		v := pairs[0].Verdicts[0]
		Expect(v.Reversed).To(BeFalse())
		Expect(v.JobID).To(Equal("j1"))
		Expect(v.CompletedAt).To(Equal(t0))
		Expect(v.Type).To(Equal(pb.RelationshipType_RELATIONSHIP_TYPE_SUBSET_OF))
		Expect(v.Confidence).To(Equal(0.8))
		Expect(v.SourceID).To(Equal("a1"))
		Expect(v.TargetID).To(Equal("b1"))
	})

	It("reads a b-to-a edge as reversed with A and B kept in query order", func() {
		edge("e1", "b1", "a1", map[string]any{"job_id": "j1", "relationship_type": "SUBSET_OF"})
		pairs, _, err := crosswalk.LoadPair(ctx, mem, tenant, query())
		Expect(err).NotTo(HaveOccurred())
		Expect(pairs).To(HaveLen(1))
		Expect(pairs[0].A.ID).To(Equal("a1"))
		Expect(pairs[0].B.ID).To(Equal("b1"))
		v := pairs[0].Verdicts[0]
		Expect(v.Reversed).To(BeTrue())
		Expect(v.SourceID).To(Equal("b1"))
		Expect(v.TargetID).To(Equal("a1"))
	})

	It("ignores an edge of a job that is not listed", func() {
		edge("e1", "a1", "b1", map[string]any{"job_id": "j9", "relationship_type": "SUBSET_OF"})
		pairs, skipped, err := crosswalk.LoadPair(ctx, mem, tenant, query())
		Expect(err).NotTo(HaveOccurred())
		Expect(pairs).To(BeEmpty())
		Expect(skipped).To(BeZero())
	})

	It("counts an edge with an unreadable endpoint as skipped", func() {
		control("b3", "catB", "")
		edge("e1", "a1", "b3", map[string]any{"job_id": "j1", "relationship_type": "SUBSET_OF"})
		pairs, skipped, err := crosswalk.LoadPair(ctx, mem, tenant, query())
		Expect(err).NotTo(HaveOccurred())
		Expect(pairs).To(BeEmpty())
		Expect(skipped).To(Equal(int64(1)))
	})

	It("ignores edges of the other stage", func() {
		edge("e1", "a1", "b1", map[string]any{"job_id": "j1", "relationship_type": "SUBSET_OF", results.StageProperty: results.StageSynthesized, "is_viable": true})
		pairs, _, err := crosswalk.LoadPair(ctx, mem, tenant, query())
		Expect(err).NotTo(HaveOccurred())
		Expect(pairs).To(BeEmpty())
	})

	Describe("viability of synthesized edges", func() {
		BeforeEach(func() {
			edge("e1", "a1", "b1", map[string]any{"job_id": "j1", "relationship_type": "PARTIAL", results.StageProperty: results.StageSynthesized, "is_viable": false})
		})
		It("excludes a non-viable edge by default", func() {
			q := query()
			q.Stage = results.StageSynthesized
			pairs, _, err := crosswalk.LoadPair(ctx, mem, tenant, q)
			Expect(err).NotTo(HaveOccurred())
			Expect(pairs).To(BeEmpty())
		})
		It("includes it when IncludeNonViable is set", func() {
			q := query()
			q.Stage = results.StageSynthesized
			q.IncludeNonViable = true
			pairs, _, err := crosswalk.LoadPair(ctx, mem, tenant, q)
			Expect(err).NotTo(HaveOccurred())
			Expect(pairs).To(HaveLen(1))
		})
	})

	Describe("NO_RELATIONSHIP edges", func() {
		BeforeEach(func() {
			edge("e1", "a1", "b1", map[string]any{"job_id": "j1", "relationship_type": "NO_RELATIONSHIP"})
		})
		It("drops them by default", func() {
			pairs, _, err := crosswalk.LoadPair(ctx, mem, tenant, query())
			Expect(err).NotTo(HaveOccurred())
			Expect(pairs).To(BeEmpty())
		})
		It("keeps them when IncludeNoRelationship is set", func() {
			q := query()
			q.IncludeNoRelationship = true
			pairs, _, err := crosswalk.LoadPair(ctx, mem, tenant, q)
			Expect(err).NotTo(HaveOccurred())
			Expect(pairs).To(HaveLen(1))
			Expect(pairs[0].Verdicts[0].Type).To(Equal(pb.RelationshipType_RELATIONSHIP_TYPE_NO_RELATIONSHIP))
		})
	})

	It("collects distinct critique notes of a pair", func() {
		note := results.CritiqueNoteProperty
		edge("e1", "a1", "b1", map[string]any{"job_id": "j1", "relationship_type": "SUBSET_OF", note: "split"})
		edge("e2", "b1", "a1", map[string]any{"job_id": "j1", "relationship_type": "SUPERSET_OF", note: "split"})
		pairs, _, err := crosswalk.LoadPair(ctx, mem, tenant, query())
		Expect(err).NotTo(HaveOccurred())
		Expect(pairs[0].CritiqueNotes).To(Equal([]string{"split"}))
		Expect(pairs[0].Verdicts).To(HaveLen(2))
	})

	It("reads edges once, in stored direction, when A equals B", func() {
		edge("e1", "a1", "a2", map[string]any{"job_id": "j1", "relationship_type": "SUBSET_OF"})
		q := query()
		q.B = "catA"
		pairs, _, err := crosswalk.LoadPair(ctx, mem, tenant, q)
		Expect(err).NotTo(HaveOccurred())
		Expect(pairs).To(HaveLen(1))
		Expect(pairs[0].Verdicts).To(HaveLen(1))
		Expect(pairs[0].Verdicts[0].Reversed).To(BeFalse())
	})

	It("orders pairs by (A, B) and verdicts by (job, source), whatever the insert order", func() {
		edge("e4", "b2", "a1", map[string]any{"job_id": "j2", "relationship_type": "SUBSET_OF"})
		edge("e3", "a2", "b1", map[string]any{"job_id": "j1", "relationship_type": "SUBSET_OF"})
		edge("e2", "b1", "a1", map[string]any{"job_id": "j1", "relationship_type": "SUBSET_OF"})
		edge("e1", "a1", "b1", map[string]any{"job_id": "j1", "relationship_type": "SUBSET_OF"})
		pairs, _, err := crosswalk.LoadPair(ctx, mem, tenant, query())
		Expect(err).NotTo(HaveOccurred())
		var order []string
		for _, p := range pairs {
			order = append(order, p.A.ID+"-"+p.B.ID)
		}
		Expect(order).To(Equal([]string{"a1-b1", "a1-b2", "a2-b1"}))
		Expect(pairs[0].Verdicts[0].SourceID).To(Equal("a1"))
		Expect(pairs[0].Verdicts[1].SourceID).To(Equal("b1"))
	})

	It("fails with context when the graph cannot be queried", func() {
		_, _, err := crosswalk.LoadPair(ctx, mem, "tenant-without-graph", query())
		Expect(err).To(MatchError(ContainSubstring("query SEMANTIC_MATCH edges")))
	})
})

var _ = DescribeTable("ParseRelationshipType",
	func(name string, want pb.RelationshipType) {
		Expect(crosswalk.ParseRelationshipType(name)).To(Equal(want))
	},
	Entry("bare name", "SUBSET_OF", pb.RelationshipType_RELATIONSHIP_TYPE_SUBSET_OF),
	Entry("proto enum name", "RELATIONSHIP_TYPE_SUBSET_OF", pb.RelationshipType_RELATIONSHIP_TYPE_SUBSET_OF),
	Entry("unknown name is unspecified", "BOGUS", pb.RelationshipType_RELATIONSHIP_TYPE_UNSPECIFIED),
)

var _ = DescribeTable("EdgeStage",
	func(props map[string]any, want string) {
		Expect(crosswalk.EdgeStage(graphdb.Edge{Properties: props})).To(Equal(want))
	},
	Entry("missing property is an analyzer edge", nil, results.StageAnalyzer),
	Entry("stored stage is returned", map[string]any{results.StageProperty: results.StageSynthesized}, results.StageSynthesized),
)

var _ = Describe("EquivalenceGroups", func() {
	e := func(sn, sc, tn, tc string) crosswalk.EquivalenceEdge {
		return crosswalk.EquivalenceEdge{SourceNode: sn, SourceCatalog: sc, TargetNode: tn, TargetCatalog: tc}
	}

	It("groups a source equivalent to two targets in one catalog", func() {
		groups := crosswalk.EquivalenceGroups([]crosswalk.EquivalenceEdge{e("s1", "x", "t1", "y"), e("s1", "x", "t2", "y")})
		Expect(groups).To(Equal([]crosswalk.EquivalenceGroup{{Node: "s1", NodeIsSource: true, OtherCatalog: "y", Members: []int{0, 1}}}))
	})

	It("groups a target equivalent to two sources in one catalog", func() {
		groups := crosswalk.EquivalenceGroups([]crosswalk.EquivalenceEdge{e("s1", "x", "t1", "y"), e("s2", "x", "t1", "y")})
		Expect(groups).To(Equal([]crosswalk.EquivalenceGroup{{Node: "t1", NodeIsSource: false, OtherCatalog: "x", Members: []int{0, 1}}}))
	})

	It("makes no group of a single edge", func() {
		Expect(crosswalk.EquivalenceGroups([]crosswalk.EquivalenceEdge{e("s1", "x", "t1", "y")})).To(BeEmpty())
	})

	It("keeps targets in different catalogs apart", func() {
		Expect(crosswalk.EquivalenceGroups([]crosswalk.EquivalenceEdge{e("s1", "x", "t1", "y"), e("s1", "x", "t2", "z")})).To(BeEmpty())
	})

	It("orders groups deterministically: source groups first, by node then catalog", func() {
		edges := []crosswalk.EquivalenceEdge{
			e("s2", "x", "t1", "y"), e("s2", "x", "t2", "y"),
			e("s1", "x", "t3", "y"), e("s1", "x", "t4", "y"),
		}
		groups := crosswalk.EquivalenceGroups(edges)
		Expect(groups).To(HaveLen(2))
		Expect(groups[0].Node).To(Equal("s1"))
		Expect(groups[1].Node).To(Equal("s2"))
	})
})
