package memdriver_test

import (
	"context"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/graphdb/memdriver"
)

func TestMemdriver(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Memdriver Suite")
}

var _ = Describe("GraphDB contract (memdriver)", testspecs.GraphDBContractBehavior(func() (graphdb.GraphDB, string, string) {
	const tenant, other = "contract-tenant", "contract-other-tenant"
	g := memdriver.New()
	Expect(g.CreateGraph(context.Background(), tenant)).To(Succeed())
	Expect(g.CreateGraph(context.Background(), other)).To(Succeed())
	return g, tenant, other
}))

var _ = Describe("NormalizeValue", func() {
	type demo struct{ X int }

	DescribeTable("converts a property value to the type AGE returns on read-back",
		func(input, want any) {
			Expect(memdriver.NormalizeValue(input)).To(Equal(want))
		},
		Entry("float32 round-trips as its shortest decimal", float32(0.1), 0.1),
		Entry("int becomes float64", 7, float64(7)),
		Entry("int64 max becomes float64", int64(math.MaxInt64), float64(math.MaxInt64)),
		Entry("float64 is unchanged", 3.14, 3.14),
		Entry("bool is unchanged", true, true),
		Entry("string is unchanged", "hello", "hello"),
		Entry("string slice becomes its %v string", []string{"a", "b"}, "[a b]"),
		Entry("struct becomes its %v string", demo{X: 5}, fmt.Sprintf("%v", demo{X: 5})),
	)
})

var _ = Describe("memdriver", func() {
	var (
		ctx context.Context
		g   graphdb.GraphDB
		t0  time.Time
	)

	BeforeEach(func() {
		ctx = context.Background()
		g = memdriver.New()
		t0 = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	})

	It("requires CreateGraph before a tenant's graph accepts writes", func() {
		node := graphdb.Node{ID: "a", Label: "Control", ValidFrom: t0}
		err := g.CreateNode(ctx, "tenant-a", node)
		Expect(err).To(MatchError(graphdb.ErrGraphNotFound))
		Expect(err.Error()).To(ContainSubstring("CreateGraph"))

		Expect(g.CreateGraph(ctx, "tenant-a")).To(Succeed())
		Expect(g.CreateNode(ctx, "tenant-a", node)).To(Succeed())
	})

	Context("copy semantics", func() {
		BeforeEach(func() {
			Expect(g.CreateGraph(ctx, "tenant-a")).To(Succeed())
		})

		It("does not alias the caller's input map", func() {
			props := map[string]any{"v": "original"}
			Expect(g.CreateNode(ctx, "tenant-a", graphdb.Node{ID: "a", Label: "Control", ValidFrom: t0, Properties: props})).To(Succeed())
			props["v"] = "mutated"

			got, err := g.GetNode(ctx, "tenant-a", "a")
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Properties).To(HaveKeyWithValue("v", "original"))
		})

		It("does not alias the caller's input map on UpsertNode", func() {
			props := map[string]any{"v": "original"}
			_, err := g.UpsertNode(ctx, "tenant-a", graphdb.Node{ID: "a", Label: "Control", ValidFrom: t0, Properties: props})
			Expect(err).NotTo(HaveOccurred())
			props["v"] = "mutated"

			got, err := g.GetNode(ctx, "tenant-a", "a")
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Properties).To(HaveKeyWithValue("v", "original"))
		})

		It("returns copies that cannot mutate stored state", func() {
			Expect(g.CreateNode(ctx, "tenant-a", graphdb.Node{ID: "a", Label: "Control", ValidFrom: t0})).To(Succeed())
			Expect(g.CreateNode(ctx, "tenant-a", graphdb.Node{ID: "b", Label: "Control", ValidFrom: t0})).To(Succeed())
			Expect(g.CreateRequiresEdge(ctx, "tenant-a", graphdb.RequiresEdge{
				SourceID: "a", TargetID: "b", Models: []string{"m1"}, AnalyzedAt: t0, JobID: "j",
			})).To(Succeed())

			first, err := g.GetEdge(ctx, "tenant-a", "requires_j_a_b")
			Expect(err).NotTo(HaveOccurred())
			first.Properties["job_id"] = "mutated"
			first.Properties["models"].([]any)[0] = "mutated"

			second, err := g.GetEdge(ctx, "tenant-a", "requires_j_a_b")
			Expect(err).NotTo(HaveOccurred())
			Expect(second.Properties).To(HaveKeyWithValue("job_id", "j"))
			Expect(second.Properties).To(HaveKeyWithValue("models", []any{"m1"}))
		})

		It("keeps previously returned values unchanged after SupersedeFact", func() {
			Expect(g.CreateNode(ctx, "tenant-a", graphdb.Node{ID: "a", Label: "Control", ValidFrom: t0})).To(Succeed())
			Expect(g.CreateNode(ctx, "tenant-a", graphdb.Node{ID: "b", Label: "Control", ValidFrom: t0})).To(Succeed())
			Expect(g.CreateEdge(ctx, "tenant-a", "a", "b", graphdb.Edge{ID: "e1", Label: "MAPS", ValidFrom: t0})).To(Succeed())

			before, err := g.GetEdge(ctx, "tenant-a", "e1")
			Expect(err).NotTo(HaveOccurred())

			ok, err := g.SupersedeFact(ctx, "tenant-a", graphdb.SupersedeRequest{
				EdgeID:            "e1",
				SupersededAt:      t0.Add(time.Hour),
				SupersededByJobID: "job-2",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeTrue())

			Expect(before.ValidTo).To(BeNil())
			Expect(before.Properties).NotTo(HaveKey("valid_to"))
			Expect(before.Properties).NotTo(HaveKey("superseded_by"))

			after, err := g.GetEdge(ctx, "tenant-a", "e1")
			Expect(err).NotTo(HaveOccurred())
			Expect(after.ValidTo).NotTo(BeNil())
		})
	})

	Context("ExecuteQuery", func() {
		BeforeEach(func() {
			Expect(g.CreateGraph(ctx, "tenant-a")).To(Succeed())
		})

		It("returns ErrNotSupported with guidance", func() {
			rows, err := g.ExecuteQuery(ctx, "tenant-a", "MATCH (n) RETURN n", nil)
			Expect(rows).To(BeNil())
			Expect(err).To(MatchError(graphdb.ErrNotSupported))
			Expect(err.Error()).To(ContainSubstring("agedriver"))
		})

		It("validates the query before reporting ErrNotSupported", func() {
			_, err := g.ExecuteQuery(ctx, "tenant-a", "", nil)
			Expect(err).To(HaveOccurred())
			Expect(err).NotTo(MatchError(graphdb.ErrNotSupported))
		})
	})

	Context("edges created without an ID", func() {
		var emptyID graphdb.Edge
		driverIDs := func() []string {
			rels, err := g.QueryRelationships(ctx, "tenant-a", graphdb.RelationshipQuery{})
			Expect(err).NotTo(HaveOccurred())
			ids := make([]string, len(rels))
			for i, r := range rels {
				Expect(r.Edge.Properties).To(HaveKeyWithValue("id", ""))
				ids[i] = r.Edge.ID
			}
			return ids
		}

		BeforeEach(func() {
			emptyID = graphdb.Edge{Label: "MAPS", ValidFrom: t0}
			Expect(g.CreateGraph(ctx, "tenant-a")).To(Succeed())
			Expect(g.CreateNode(ctx, "tenant-a", graphdb.Node{ID: "a", Label: "Control", ValidFrom: t0})).To(Succeed())
			Expect(g.CreateNode(ctx, "tenant-a", graphdb.Node{ID: "b", Label: "Control", ValidFrom: t0})).To(Succeed())
		})

		It("get distinct non-empty driver IDs", func() {
			Expect(g.CreateEdge(ctx, "tenant-a", "a", "b", emptyID)).To(Succeed())
			Expect(g.CreateEdge(ctx, "tenant-a", "a", "b", emptyID)).To(Succeed())

			ids := driverIDs()
			Expect(ids).To(HaveLen(2))
			Expect(ids).NotTo(ContainElement(BeEmpty()))
			Expect(ids[0]).NotTo(Equal(ids[1]))
		})

		It("never reuse a driver ID already handed out when a BulkCreateEdges batch fails", func() {
			Expect(g.CreateEdge(ctx, "tenant-a", "a", "b", emptyID)).To(Succeed())
			first := driverIDs()
			Expect(first).To(HaveLen(1))

			// Two empty-ID edges advance the sequence before the missing
			// endpoint fails the batch and rolls it back.
			_, err := g.BulkCreateEdges(ctx, "tenant-a", []graphdb.BulkEdge{
				{SourceID: "a", TargetID: "b", Edge: emptyID},
				{SourceID: "a", TargetID: "b", Edge: emptyID},
				{SourceID: "a", TargetID: "zz", Edge: emptyID},
			})
			Expect(err).To(MatchError(graphdb.ErrNodeNotFound))
			Expect(driverIDs()).To(Equal(first))

			Expect(g.CreateEdge(ctx, "tenant-a", "a", "b", emptyID)).To(Succeed())
			ids := driverIDs()
			Expect(ids).To(HaveLen(2))
			Expect(ids).To(ContainElement(first[0]))
			Expect(ids[0]).NotTo(Equal(ids[1]))
		})
	})

	It("creates a new tenant's graph once when several CreateGraph calls race", func() {
		var wg sync.WaitGroup
		errs := make([]error, 8)
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = g.CreateGraph(ctx, "tenant-new")
			}()
		}
		wg.Wait()
		for _, err := range errs {
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(g.CreateNode(ctx, "tenant-new", graphdb.Node{ID: "a", Label: "Control", ValidFrom: t0})).To(Succeed())
	})

	It("is safe for concurrent writers", func() {
		Expect(g.CreateGraph(ctx, "tenant-a")).To(Succeed())
		var wg sync.WaitGroup
		for i := range 50 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer GinkgoRecover()
				Expect(g.CreateNode(ctx, "tenant-a", graphdb.Node{ID: fmt.Sprintf("n%d", i), Label: "Control", ValidFrom: t0})).To(Succeed())
			}()
		}
		wg.Wait()
		for i := range 50 {
			_, err := g.GetNode(ctx, "tenant-a", fmt.Sprintf("n%d", i))
			Expect(err).NotTo(HaveOccurred())
		}
	})

	It("is safe for concurrent readers and writers", func() {
		Expect(g.CreateGraph(ctx, "tenant-a")).To(Succeed())
		for i := range 10 {
			Expect(g.CreateNode(ctx, "tenant-a", graphdb.Node{ID: fmt.Sprintf("n%d", i), Label: "Control", ValidFrom: t0})).To(Succeed())
		}
		// Seeded ahead of the race so readers always have something to find,
		// and so the supersede writer has a target.
		Expect(g.CreateEdge(ctx, "tenant-a", "n0", "n1", graphdb.Edge{ID: "seed", Label: "MAPS", ValidFrom: t0})).To(Succeed())

		const writers, readers = 20, 20
		var wg sync.WaitGroup

		for i := range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer GinkgoRecover()
				src := fmt.Sprintf("n%d", i%10)
				tgt := fmt.Sprintf("n%d", (i+1)%10)
				Expect(g.CreateEdge(ctx, "tenant-a", src, tgt, graphdb.Edge{ID: fmt.Sprintf("e%d", i), Label: "MAPS", ValidFrom: t0})).To(Succeed())
			}()
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer GinkgoRecover()
			ok, err := g.SupersedeFact(ctx, "tenant-a", graphdb.SupersedeRequest{EdgeID: "seed", SupersededAt: t0.Add(time.Hour)})
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeTrue())
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer GinkgoRecover()
			for i := range 10 {
				created, err := g.UpsertNode(ctx, "tenant-a", graphdb.Node{ID: "n0", Label: "Control", ValidFrom: t0, Properties: map[string]any{"rev": i}})
				Expect(err).NotTo(HaveOccurred())
				Expect(created).To(BeFalse())
			}
		}()

		for range readers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer GinkgoRecover()
				_, err := g.QueryRelationships(ctx, "tenant-a", graphdb.RelationshipQuery{})
				Expect(err).NotTo(HaveOccurred())
				_, err = g.Traverse(ctx, "tenant-a", graphdb.TraversalQuery{StartNode: "n0"})
				Expect(err).NotTo(HaveOccurred())
				// "seed" is created before the goroutines start and is never
				// deleted, so it is always findable regardless of how the
				// supersede write interleaves with this read.
				_, err = g.GetEdge(ctx, "tenant-a", "seed")
				Expect(err).NotTo(HaveOccurred())
			}()
		}

		wg.Wait()

		for i := range writers {
			_, err := g.GetEdge(ctx, "tenant-a", fmt.Sprintf("e%d", i))
			Expect(err).NotTo(HaveOccurred())
		}
		seed, err := g.GetEdge(ctx, "tenant-a", "seed")
		Expect(err).NotTo(HaveOccurred())
		Expect(seed.ValidTo).NotTo(BeNil())
		n0, err := g.GetNode(ctx, "tenant-a", "n0")
		Expect(err).NotTo(HaveOccurred())
		Expect(n0.Properties).To(HaveKeyWithValue("rev", float64(9)))
	})
})
