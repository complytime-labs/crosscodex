package memdriver_test

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/graphdb/memdriver"
)

var _ = Describe("Property Specifications", Ordered, func() {
	const tenant = "prop-tenant"
	ctx := context.Background()

	newGraph := func() graphdb.GraphDB {
		g := memdriver.New()
		Expect(g.CreateGraph(ctx, tenant)).To(Succeed())
		return g
	}
	// 1970-01-01 .. 2100-01-01 in nanoseconds.
	drawTime := func(t *rapid.T, label string) time.Time {
		return time.Unix(0, rapid.Int64Range(0, 4102444800*int64(time.Second)).Draw(t, label)).UTC()
	}

	Context("CreateNode/GetNode — roundtrip", func() {
		It("reads back every field and the normalized properties", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				g := newGraph()
				i := rapid.Int().Draw(t, "i")
				f := rapid.Float64Range(-1e12, 1e12).Draw(t, "f")
				node := graphdb.Node{
					ID:        rapid.StringMatching(`[a-z0-9-]{1,24}`).Draw(t, "id"),
					Label:     rapid.SampledFrom([]string{"Control", "Artifact", "ArtifactType"}).Draw(t, "label"),
					ValidFrom: drawTime(t, "validFrom"),
					CreatedBy: rapid.StringMatching(`[a-z0-9-]{0,12}`).Draw(t, "createdBy"),
					Properties: map[string]any{
						"s": rapid.String().Draw(t, "s"),
						"i": i,
						"f": f,
						"b": rapid.Bool().Draw(t, "b"),
					},
				}
				Expect(g.CreateNode(ctx, tenant, node)).To(Succeed())

				got, err := g.GetNode(ctx, tenant, node.ID)
				Expect(err).NotTo(HaveOccurred())
				Expect(got.ID).To(Equal(node.ID))
				Expect(got.Label).To(Equal(node.Label))
				Expect(got.ValidFrom.Equal(node.ValidFrom)).To(BeTrue())
				Expect(got.CreatedBy).To(Equal(node.CreatedBy))
				Expect(got.Properties).To(HaveKeyWithValue("s", node.Properties["s"]))
				Expect(got.Properties).To(HaveKeyWithValue("i", float64(i)))
				Expect(got.Properties).To(HaveKeyWithValue("f", f))
				Expect(got.Properties).To(HaveKeyWithValue("b", node.Properties["b"]))
				Expect(got.Properties).To(HaveKeyWithValue("valid_from", graphdb.FormatTime(node.ValidFrom)))
			})
		})
	})

	Context("QueryAsOf — temporal filter", func() {
		It("returns exactly the edges whose single-edge path is ValidAt the instant", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				g := newGraph()
				for _, id := range []string{"a", "b"} {
					Expect(g.CreateNode(ctx, tenant, graphdb.Node{ID: id, Label: "Control", ValidFrom: time.Unix(0, 0).UTC()})).To(Succeed())
				}

				n := rapid.IntRange(1, 8).Draw(t, "n")
				var boundaries []time.Time
				var edges []graphdb.Edge
				for i := range n {
					e := graphdb.Edge{ID: fmt.Sprintf("e%d", i), Label: "MAPS", ValidFrom: drawTime(t, fmt.Sprintf("from%d", i))}
					boundaries = append(boundaries, e.ValidFrom)
					if rapid.Bool().Draw(t, fmt.Sprintf("hasTo%d", i)) {
						to := e.ValidFrom.Add(time.Duration(rapid.Int64Range(1, int64(time.Hour)).Draw(t, fmt.Sprintf("span%d", i))))
						e.ValidTo = &to
						boundaries = append(boundaries, to)
					}
					edges = append(edges, e)
					Expect(g.CreateEdge(ctx, tenant, "a", "b", e)).To(Succeed())
				}

				// Probe boundaries as well as arbitrary instants: off-by-one
				// errors live at valid_from and valid_to exactly.
				at := drawTime(t, "at")
				if rapid.Bool().Draw(t, "probeBoundary") {
					at = rapid.SampledFrom(boundaries).Draw(t, "boundary")
				}

				var want []string
				for _, e := range edges {
					if (graphdb.Path{Edges: []graphdb.Edge{e}}).ValidAt(at) {
						want = append(want, e.ID)
					}
				}
				rels, err := g.QueryAsOf(ctx, tenant, graphdb.RelationshipQuery{}, at)
				Expect(err).NotTo(HaveOccurred())
				var got []string
				for _, r := range rels {
					got = append(got, r.Edge.ID)
				}
				Expect(got).To(ConsistOf(want))
			})
		})
	})

	Context("BulkCreateEdges — atomicity", func() {
		It("persists every edge or none, and a failed batch leaves the stored graph unchanged", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				g := newGraph()
				nodes := []string{"n0", "n1", "n2", "n3"}
				for _, id := range nodes {
					Expect(g.CreateNode(ctx, tenant, graphdb.Node{ID: id, Label: "Control", ValidFrom: time.Unix(0, 0).UTC()})).To(Succeed())
				}
				// A stored edge makes "unchanged" stronger than "empty" and
				// lets a batch collide with an ID already in the graph.
				Expect(g.CreateEdge(ctx, tenant, "n0", "n1", graphdb.Edge{ID: "pre", Label: "MAPS", ValidFrom: time.Unix(1, 0).UTC()})).To(Succeed())

				// snapshot maps each current edge ID to source, label, target
				// and valid_from, and returns the row count so a duplicated row
				// (which the map would hide) is still seen.
				snapshot := func() (map[string][4]string, int) {
					rels, err := g.QueryRelationships(ctx, tenant, graphdb.RelationshipQuery{})
					Expect(err).NotTo(HaveOccurred())
					out := make(map[string][4]string, len(rels))
					for _, r := range rels {
						out[r.Edge.ID] = [4]string{r.Source.ID, r.Edge.Label, r.Target.ID, graphdb.FormatTime(r.Edge.ValidFrom)}
					}
					return out, len(rels)
				}
				before, beforeRows := snapshot()
				endpoints := append(slices.Clone(nodes), "missing")

				size := rapid.IntRange(1, 6).Draw(t, "size")
				batch := make([]graphdb.BulkEdge, size)
				want := maps.Clone(before)
				seen := map[string]bool{"pre": true}
				failAt := -1
				var failWith error
				for i := range batch {
					id := rapid.SampledFrom([]string{"pre", "e0", "e1", "e2", "e3", "e4", "e5"}).Draw(t, fmt.Sprintf("id%d", i))
					src := rapid.SampledFrom(endpoints).Draw(t, fmt.Sprintf("src%d", i))
					tgt := rapid.SampledFrom(endpoints).Draw(t, fmt.Sprintf("tgt%d", i))
					validFrom := time.Unix(rapid.Int64Range(1, 1000).Draw(t, fmt.Sprintf("validFrom%d", i)), 0).UTC()
					batch[i] = graphdb.BulkEdge{SourceID: src, TargetID: tgt, Edge: graphdb.Edge{ID: id, Label: "MAPS", ValidFrom: validFrom}}
					// memdriver resolves both endpoints before checking the ID.
					if failAt < 0 {
						switch {
						case src == "missing" || tgt == "missing":
							failAt, failWith = i, graphdb.ErrNodeNotFound
						case seen[id]:
							failAt, failWith = i, graphdb.ErrEdgeExists
						}
					}
					seen[id] = true
					want[id] = [4]string{src, "MAPS", tgt, graphdb.FormatTime(validFrom)}
				}

				ids, err := g.BulkCreateEdges(ctx, tenant, batch)
				if failAt >= 0 {
					Expect(err).To(MatchError(failWith))
					Expect(err.Error()).To(ContainSubstring(fmt.Sprintf("[%d]", failAt)))
					Expect(ids).To(BeNil())
					after, afterRows := snapshot()
					Expect(afterRows).To(Equal(beforeRows))
					Expect(after).To(Equal(before))
					return
				}
				Expect(err).NotTo(HaveOccurred())
				wantIDs := make([]string, size)
				for i, be := range batch {
					wantIDs[i] = be.Edge.ID
				}
				Expect(ids).To(Equal(wantIDs))
				after, afterRows := snapshot()
				Expect(afterRows).To(Equal(len(want)))
				Expect(after).To(Equal(want))
			})
		})
	})
})
