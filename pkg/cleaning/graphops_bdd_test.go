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

	Describe("RetireRebuiltEdges", func() {
		// addEdge adds a live a1->b1 edge starting at from.
		addEdge := func(id string, from time.Time) {
			Expect(g.CreateEdge(ctx, opsTenant, "a1", "b1", graphdb.Edge{
				ID: id, Label: results.SemanticMatchEdgeLabel, ValidFrom: from,
			})).To(Succeed())
		}
		validTo := func(id string) *time.Time {
			e, err := g.GetEdge(ctx, opsTenant, id)
			Expect(err).NotTo(HaveOccurred())
			return e.ValidTo
		}

		It("retires at the decision time when every edge was valid before it", func() {
			ids, used, err := cleaning.RetireRebuiltEdges(ctx, g, opsTenant, []string{"e1"}, at)
			Expect(err).NotTo(HaveOccurred())
			Expect(ids).To(Equal([]string{"e1"}))
			Expect(used).To(BeTemporally("==", at))
			Expect(validTo("e1")).To(HaveValue(BeTemporally("==", at)))
		})

		It("retires every edge at the latest valid_from when an edge was rebuilt after the decision", func() {
			addEdge("e2", restoreAt)
			ids, used, err := cleaning.RetireRebuiltEdges(ctx, g, opsTenant, []string{"e1", "e2"}, at)
			Expect(err).NotTo(HaveOccurred())
			Expect(ids).To(Equal([]string{"e1", "e2"}))
			Expect(used).To(BeTemporally("==", restoreAt))
			Expect(validTo("e1")).To(HaveValue(BeTemporally("==", restoreAt)))
			Expect(validTo("e2")).To(HaveValue(BeTemporally("==", restoreAt)))
		})

		It("skips an already superseded edge and does not let its valid_from move the time", func() {
			addEdge("e2", restoreAt)
			_, err := cleaning.RetireEdges(ctx, g, opsTenant, []string{"e2"}, restoreAt.Add(time.Hour))
			Expect(err).NotTo(HaveOccurred())
			ids, used, err := cleaning.RetireRebuiltEdges(ctx, g, opsTenant, []string{"e1", "e2"}, at)
			Expect(err).NotTo(HaveOccurred())
			Expect(ids).To(Equal([]string{"e1"}))
			Expect(used).To(BeTemporally("==", at))
			Expect(validTo("e2")).To(HaveValue(BeTemporally("==", restoreAt.Add(time.Hour))))
		})

		It("changes nothing when run a second time", func() {
			_, _, err := cleaning.RetireRebuiltEdges(ctx, g, opsTenant, []string{"e1"}, at)
			Expect(err).NotTo(HaveOccurred())
			ids, _, err := cleaning.RetireRebuiltEdges(ctx, g, opsTenant, []string{"e1"}, at.Add(time.Hour))
			Expect(err).NotTo(HaveOccurred())
			Expect(ids).To(BeEmpty())
			Expect(validTo("e1")).To(HaveValue(BeTemporally("==", at)))
		})

		It("refuses a missing edge before retiring any, naming it", func() {
			ids, _, err := cleaning.RetireRebuiltEdges(ctx, g, opsTenant, []string{"e1", "nope"}, at)
			Expect(err).To(MatchError(graphdb.ErrEdgeNotFound))
			Expect(err).To(MatchError(ContainSubstring(`"nope"`)))
			Expect(ids).To(BeEmpty())
			Expect(validTo("e1")).To(BeNil())
		})

		It("refuses a zero decision time, changing nothing", func() {
			_, _, err := cleaning.RetireRebuiltEdges(ctx, g, opsTenant, []string{"e1"}, time.Time{})
			Expect(err).To(MatchError(ContainSubstring("decision time is required")))
			Expect(validTo("e1")).To(BeNil())
		})
	})

	Describe("ExistingEdges", func() {
		It("returns the live and superseded edges that exist, in input order", func() {
			Expect(g.CreateEdge(ctx, opsTenant, "a1", "b1", graphdb.Edge{
				ID: "e2", Label: results.SemanticMatchEdgeLabel, ValidFrom: t1,
			})).To(Succeed())
			_, err := cleaning.RetireEdges(ctx, g, opsTenant, []string{"e2"}, at)
			Expect(err).NotTo(HaveOccurred())
			ids, err := cleaning.ExistingEdges(ctx, g, opsTenant, []string{"e2", "nope", "e1"})
			Expect(err).NotTo(HaveOccurred())
			Expect(ids).To(Equal([]string{"e2", "e1"}))
		})

		It("returns none when no edge exists", func() {
			ids, err := cleaning.ExistingEdges(ctx, g, opsTenant, []string{"nope"})
			Expect(err).NotTo(HaveOccurred())
			Expect(ids).To(BeEmpty())
		})

		It("returns a driver failure instead of reporting the edge absent", func() {
			ids, err := cleaning.ExistingEdges(ctx, failingGetEdge{GraphDB: g}, opsTenant, []string{"e1"})
			Expect(err).To(MatchError(errInjected))
			Expect(err).To(MatchError(ContainSubstring(`"e1"`)))
			Expect(ids).To(BeEmpty())
		})
	})

	Describe("PresentVerdicts", func() {
		j1a1b1 := cleaning.VerdictKey{JobID: "j1", SourceID: "a1", TargetID: "b1"}
		addEdge := func(src, tgt string, e graphdb.Edge) {
			Expect(g.CreateEdge(ctx, opsTenant, src, tgt, e)).To(Succeed())
		}
		match := func(id, job string) graphdb.Edge {
			return graphdb.Edge{ID: id, Label: results.SemanticMatchEdgeLabel, ValidFrom: t1, Properties: map[string]any{"job_id": job}}
		}

		DescribeTable("reports whether the graph holds a SEMANTIC_MATCH edge of the verdict, live or superseded",
			func(key cleaning.VerdictKey, prepare func(), want bool) {
				prepare()
				got, err := cleaning.PresentVerdicts(ctx, g, opsTenant, []cleaning.VerdictKey{key})
				Expect(err).NotTo(HaveOccurred())
				Expect(got).To(Equal(map[cleaning.VerdictKey]bool{key: want}))
			},
			Entry("a live edge", j1a1b1, func() {}, true),
			Entry("only a superseded edge, of another edge ID kind", j1a1b1, func() {
				_, err := cleaning.RetireEdges(ctx, g, opsTenant, []string{"e1"}, at)
				Expect(err).NotTo(HaveOccurred())
				addEdge("a1", "b1", match("semantic-match-rebuild-x", "j1"))
				_, err = cleaning.RetireEdges(ctx, g, opsTenant, []string{"semantic-match-rebuild-x"}, at)
				Expect(err).NotTo(HaveOccurred())
			}, true),
			Entry("no edge of the verdict's job on the pair", cleaning.VerdictKey{JobID: "j2", SourceID: "a1", TargetID: "b1"}, func() {}, false),
			Entry("an edge of the job from the source to another target", cleaning.VerdictKey{JobID: "j1", SourceID: "a1", TargetID: "c1"}, func() {}, false),
			Entry("an edge of the job in the other direction", cleaning.VerdictKey{JobID: "j1", SourceID: "b1", TargetID: "a1"}, func() {}, false),
			Entry("an edge of the verdict under another label", cleaning.VerdictKey{JobID: "j3", SourceID: "a1", TargetID: "b1"}, func() {
				addEdge("a1", "b1", graphdb.Edge{ID: "other", Label: "RELATED", ValidFrom: t1, Properties: map[string]any{"job_id": "j3"}})
			}, false),
			Entry("a source node the graph does not hold", cleaning.VerdictKey{JobID: "j1", SourceID: "nope", TargetID: "b1"}, func() {}, false),
		)

		It("answers every key, reading each source once", func() {
			addEdge("a1", "b1", match("e-j2", "j2"))
			keys := []cleaning.VerdictKey{j1a1b1, {JobID: "j2", SourceID: "a1", TargetID: "b1"}, {JobID: "j9", SourceID: "a1", TargetID: "b1"}}
			counting := &countingTraverse{GraphDB: g}
			got, err := cleaning.PresentVerdicts(ctx, counting, opsTenant, keys)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(map[cleaning.VerdictKey]bool{keys[0]: true, keys[1]: true, keys[2]: false}))
			Expect(counting.calls).To(Equal(1))
		})

		It("reads nothing for no keys", func() {
			counting := &countingTraverse{GraphDB: g}
			got, err := cleaning.PresentVerdicts(ctx, counting, opsTenant, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(BeEmpty())
			Expect(counting.calls).To(BeZero())
		})

		It("returns a driver failure instead of reporting the verdict gone", func() {
			got, err := cleaning.PresentVerdicts(ctx, failingTraverse{GraphDB: g}, opsTenant, []cleaning.VerdictKey{j1a1b1})
			Expect(err).To(MatchError(errInjected))
			Expect(err).To(MatchError(ContainSubstring("a1")))
			Expect(got).To(BeNil())
		})
	})
})

// failingGetEdge fails every GetEdge as a graph outage would.
type failingGetEdge struct{ graphdb.GraphDB }

func (failingGetEdge) GetEdge(context.Context, string, string) (*graphdb.EdgeWithEndpoints, error) {
	return nil, errInjected
}

// failingTraverse fails every Traverse as a graph outage would.
type failingTraverse struct{ graphdb.GraphDB }

func (failingTraverse) Traverse(context.Context, string, graphdb.TraversalQuery) ([]graphdb.Path, error) {
	return nil, errInjected
}

// countingTraverse counts Traverse calls.
type countingTraverse struct {
	graphdb.GraphDB
	calls int
}

func (c *countingTraverse) Traverse(ctx context.Context, tenantID string, q graphdb.TraversalQuery) ([]graphdb.Path, error) {
	c.calls++
	return c.GraphDB.Traverse(ctx, tenantID, q)
}
