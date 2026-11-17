//go:build !integration

package cleaning_test

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
	"github.com/complytime-labs/crosscodex/pkg/cleaning"
	"github.com/complytime-labs/crosscodex/pkg/crosswalk"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/graphdb/memdriver"
)

const opsTenant = "tenant-ops"

// opsGraph is a memdriver graph holding controls a1 (catalog cat-a) and b1
// (catalog cat-b) and one analyzer SEMANTIC_MATCH edge "e1" between them with
// every caller-owned field set.
func opsGraph(ctx context.Context) graphdb.GraphDB {
	g := memdriver.New()
	Expect(g.CreateGraph(ctx, opsTenant)).To(Succeed())
	for _, n := range []struct{ id, catalog string }{{"a1", "cat-a"}, {"b1", "cat-b"}} {
		Expect(g.CreateNode(ctx, opsTenant, graphdb.Node{
			ID: n.id, Label: "Control", ValidFrom: t1,
			Properties: map[string]any{"catalog_id": n.catalog, "identifier": n.id},
		})).To(Succeed())
	}
	Expect(g.CreateEdge(ctx, opsTenant, "a1", "b1", graphdb.Edge{
		ID: "e1", Label: results.SemanticMatchEdgeLabel, ValidFrom: t1,
		DeterminedBy: "model-x", DeterminationType: "llm", Confidence: 0.75,
		Properties: map[string]any{"job_id": "j1", "relationship_type": "SUBSET_OF"},
	})).To(Succeed())
	return g
}

var _ = Describe("Graph retire and restore", func() {
	var (
		ctx       context.Context
		g         graphdb.GraphDB
		at        = time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
		restoreAt = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	)
	BeforeEach(func() {
		ctx = context.Background()
		g = opsGraph(ctx)
	})

	liveEdges := func() []graphdb.Relationship {
		rels, err := g.QueryRelationships(ctx, opsTenant, graphdb.RelationshipQuery{EdgeLabel: results.SemanticMatchEdgeLabel})
		Expect(err).NotTo(HaveOccurred())
		return rels
	}

	Describe("RetireEdges", func() {
		It("supersedes a live edge and reports its ID", func() {
			ids, err := cleaning.RetireEdges(ctx, g, opsTenant, []string{"e1"}, at)
			Expect(err).NotTo(HaveOccurred())
			Expect(ids).To(Equal([]string{"e1"}))
			e, err := g.GetEdge(ctx, opsTenant, "e1")
			Expect(err).NotTo(HaveOccurred())
			Expect(e.ValidTo).To(HaveValue(BeTemporally("==", at)))
			Expect(liveEdges()).To(BeEmpty())
		})

		It("skips an already superseded edge and leaves valid_to unchanged", func() {
			_, err := cleaning.RetireEdges(ctx, g, opsTenant, []string{"e1"}, at)
			Expect(err).NotTo(HaveOccurred())
			ids, err := cleaning.RetireEdges(ctx, g, opsTenant, []string{"e1"}, at.Add(48*time.Hour))
			Expect(err).NotTo(HaveOccurred())
			Expect(ids).To(BeEmpty())
			e, err := g.GetEdge(ctx, opsTenant, "e1")
			Expect(err).NotTo(HaveOccurred())
			Expect(e.ValidTo).To(HaveValue(BeTemporally("==", at)))
		})

		It("refuses a missing edge and names it", func() {
			ids, err := cleaning.RetireEdges(ctx, g, opsTenant, []string{"nope"}, at)
			Expect(err).To(MatchError(graphdb.ErrEdgeNotFound))
			Expect(err).To(MatchError(ContainSubstring("nope")))
			Expect(ids).To(BeEmpty())
		})

		It("refuses a time before valid_from, changing nothing", func() {
			ids, err := cleaning.RetireEdges(ctx, g, opsTenant, []string{"e1"}, t1.Add(-time.Hour))
			Expect(err).To(MatchError(ContainSubstring(`"e1"`)))
			Expect(err).To(MatchError(ContainSubstring("invert")))
			Expect(ids).To(BeEmpty())
			e, err := g.GetEdge(ctx, opsTenant, "e1")
			Expect(err).NotTo(HaveOccurred())
			Expect(e.ValidTo).To(BeNil())
		})

		It("refuses a zero time without touching the edge", func() {
			_, err := cleaning.RetireEdges(ctx, g, opsTenant, []string{"e1"}, time.Time{})
			Expect(err).To(HaveOccurred())
			Expect(liveEdges()).To(HaveLen(1))
		})

		It("refuses a missing tenant graph", func() {
			_, err := cleaning.RetireEdges(ctx, g, "tenant-other", []string{"e1"}, at)
			Expect(err).To(MatchError(graphdb.ErrGraphNotFound))
		})
	})

	Describe("RestoreEdges", func() {
		BeforeEach(func() {
			_, err := cleaning.RetireEdges(ctx, g, opsTenant, []string{"e1"}, at)
			Expect(err).NotTo(HaveOccurred())
		})

		It("recreates the edge with every caller-owned field and a derived ID", func() {
			ids, err := cleaning.RestoreEdges(ctx, g, opsTenant, []string{"e1"}, "dec-1", restoreAt)
			Expect(err).NotTo(HaveOccurred())
			want := graphdb.DerivedID("semantic-match-restored", "e1", "dec-1")
			Expect(ids).To(Equal([]string{want}))

			got, err := g.GetEdge(ctx, opsTenant, want)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.SourceID).To(Equal("a1"))
			Expect(got.TargetID).To(Equal("b1"))
			Expect(got.Label).To(Equal(results.SemanticMatchEdgeLabel))
			Expect(got.Supersedes).To(Equal("e1"))
			Expect(got.DeterminedBy).To(Equal("model-x"))
			Expect(got.DeterminationType).To(Equal("llm"))
			Expect(got.Confidence).To(Equal(0.75))
			Expect(got.ValidTo).To(BeNil())
			Expect(got.Properties).To(HaveKeyWithValue("job_id", "j1"))
			Expect(got.Properties).To(HaveKeyWithValue("relationship_type", "SUBSET_OF"))
			Expect(got.Properties).NotTo(HaveKey("superseded_by"))
		})

		It("appears in QueryRelationships and LoadPair while the retired edge stays queryable by time", func() {
			ids, err := cleaning.RestoreEdges(ctx, g, opsTenant, []string{"e1"}, "dec-1", restoreAt)
			Expect(err).NotTo(HaveOccurred())

			live := liveEdges()
			Expect(live).To(HaveLen(1))
			Expect(live[0].Edge.ID).To(Equal(ids[0]))

			past, err := g.QueryAsOf(ctx, opsTenant, graphdb.RelationshipQuery{EdgeLabel: results.SemanticMatchEdgeLabel}, at.Add(-time.Hour))
			Expect(err).NotTo(HaveOccurred())
			Expect(past).To(HaveLen(1))
			Expect(past[0].Edge.ID).To(Equal("e1"))

			pairs, skipped, err := crosswalk.LoadPair(ctx, g, opsTenant, crosswalk.PairQuery{
				A: "cat-a", B: "cat-b", Jobs: map[string]time.Time{"j1": t1}, Stage: results.StageAnalyzer,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(skipped).To(BeZero())
			Expect(pairs).To(HaveLen(1))
			Expect(pairs[0].Verdicts).To(HaveLen(1))
			v := pairs[0].Verdicts[0]
			Expect(v.Edge.ID).To(Equal(ids[0]))
			Expect(v.Type).To(Equal(pb.RelationshipType_RELATIONSHIP_TYPE_SUBSET_OF))
			Expect(v.Confidence).To(Equal(0.75))
			Expect(v.JobID).To(Equal("j1"))
		})

		It("creates one edge when run twice with the same decision, keeping the first valid_from", func() {
			first, err := cleaning.RestoreEdges(ctx, g, opsTenant, []string{"e1"}, "dec-1", restoreAt)
			Expect(err).NotTo(HaveOccurred())
			second, err := cleaning.RestoreEdges(ctx, g, opsTenant, []string{"e1"}, "dec-1", restoreAt.Add(24*time.Hour))
			Expect(err).NotTo(HaveOccurred())
			Expect(second).To(Equal(first))
			Expect(liveEdges()).To(HaveLen(1))
			got, err := g.GetEdge(ctx, opsTenant, first[0])
			Expect(err).NotTo(HaveOccurred())
			Expect(got.ValidFrom).To(BeTemporally("==", restoreAt))
		})

		It("starts the restored edge at the restore time", func() {
			ids, err := cleaning.RestoreEdges(ctx, g, opsTenant, []string{"e1"}, "dec-1", restoreAt)
			Expect(err).NotTo(HaveOccurred())
			got, err := g.GetEdge(ctx, opsTenant, ids[0])
			Expect(err).NotTo(HaveOccurred())
			Expect(got.ValidFrom).To(BeTemporally("==", restoreAt))
			gap, err := g.QueryAsOf(ctx, opsTenant, graphdb.RelationshipQuery{EdgeLabel: results.SemanticMatchEdgeLabel}, at.Add(time.Hour))
			Expect(err).NotTo(HaveOccurred())
			Expect(gap).To(BeEmpty())
		})

		It("keeps one live successor when restored under a second decision", func() {
			first, err := cleaning.RestoreEdges(ctx, g, opsTenant, []string{"e1"}, "dec-1", restoreAt)
			Expect(err).NotTo(HaveOccurred())
			second, err := cleaning.RestoreEdges(ctx, g, opsTenant, []string{"e1"}, "dec-2", restoreAt.Add(time.Hour))
			Expect(err).NotTo(HaveOccurred())
			Expect(second).To(Equal(first))
			Expect(liveEdges()).To(HaveLen(1))
			_, err = g.GetEdge(ctx, opsTenant, graphdb.DerivedID("semantic-match-restored", "e1", "dec-2"))
			Expect(err).To(MatchError(graphdb.ErrEdgeNotFound))
		})

		It("refuses a zero restore time and a time before the retirement, changing nothing", func() {
			_, err := cleaning.RestoreEdges(ctx, g, opsTenant, []string{"e1"}, "dec-1", time.Time{})
			Expect(err).To(HaveOccurred())
			_, err = cleaning.RestoreEdges(ctx, g, opsTenant, []string{"e1"}, "dec-1", at.Add(-time.Hour))
			Expect(err).To(MatchError(ContainSubstring(`"e1"`)))
			Expect(err).To(MatchError(ContainSubstring("before the edge was retired")))
			Expect(liveEdges()).To(BeEmpty())
		})

		It("refuses a live edge and creates nothing", func() {
			Expect(g.CreateEdge(ctx, opsTenant, "a1", "b1", graphdb.Edge{
				ID: "e2", Label: results.SemanticMatchEdgeLabel, ValidFrom: t1,
			})).To(Succeed())
			ids, err := cleaning.RestoreEdges(ctx, g, opsTenant, []string{"e1", "e2"}, "dec-1", restoreAt)
			Expect(err).To(MatchError(cleaning.ErrEdgeLive))
			Expect(err).To(MatchError(ContainSubstring("e2")))
			Expect(ids).To(BeEmpty())
			_, err = g.GetEdge(ctx, opsTenant, graphdb.DerivedID("semantic-match-restored", "e1", "dec-1"))
			Expect(err).To(MatchError(graphdb.ErrEdgeNotFound))
		})

		It("refuses a missing edge and an empty decision ID", func() {
			_, err := cleaning.RestoreEdges(ctx, g, opsTenant, []string{"nope"}, "dec-1", restoreAt)
			Expect(err).To(MatchError(graphdb.ErrEdgeNotFound))
			_, err = cleaning.RestoreEdges(ctx, g, opsTenant, []string{"e1"}, "", restoreAt)
			Expect(err).To(HaveOccurred())
			Expect(liveEdges()).To(BeEmpty())
		})
	})

	Describe("CheckLive", func() {
		It("accepts live edges", func() {
			Expect(cleaning.CheckLive(ctx, g, opsTenant, []string{"e1"})).To(Succeed())
		})

		It("names a missing edge", func() {
			err := cleaning.CheckLive(ctx, g, opsTenant, []string{"e1", "nope"})
			Expect(err).To(MatchError(cleaning.ErrEdgeNotLive))
			Expect(err).To(MatchError(ContainSubstring("nope")))
		})

		It("names a superseded edge", func() {
			_, err := cleaning.RetireEdges(ctx, g, opsTenant, []string{"e1"}, at)
			Expect(err).NotTo(HaveOccurred())
			err = cleaning.CheckLive(ctx, g, opsTenant, []string{"e1"})
			Expect(err).To(MatchError(cleaning.ErrEdgeNotLive))
			Expect(err).To(MatchError(ContainSubstring("e1")))
		})

		It("does not mistake a driver failure for a missing edge", func() {
			err := cleaning.CheckLive(ctx, g, "tenant-other", []string{"e1"})
			Expect(err).To(MatchError(graphdb.ErrGraphNotFound))
			Expect(err).NotTo(MatchError(cleaning.ErrEdgeNotLive))
		})
	})
})
