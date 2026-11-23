//go:build !integration

package cleaning_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
	"github.com/complytime-labs/crosscodex/pkg/cleaning"
	"github.com/complytime-labs/crosscodex/pkg/cleaning/cleaningtest"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/jobconfig"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// failQueries fails every QueryRelationships as a graph outage would.
type failQueries struct{ graphdb.GraphDB }

func (failQueries) QueryRelationships(context.Context, string, graphdb.RelationshipQuery) ([]graphdb.Relationship, error) {
	return nil, errInjected
}

var _ = Describe("Engine.VerdictEdges", func() {
	var (
		ctx   context.Context
		g     graphdb.GraphDB
		store *cleaningtest.Fake
	)

	// j2 completed after j1 on the same catalogs; its a1->b1 verdict has a
	// NO_RELATIONSHIP analyzer edge and a non-viable synthesized edge.
	j2 := cleaning.Job{ID: "j2", CompletedAt: t2, Catalogs: jobconfig.Catalogs{SourceID: "cat-a", TargetID: "cat-b"}}

	verdictEdges := func(gdb graphdb.GraphDB, keys ...cleaning.VerdictKey) (map[cleaning.VerdictKey][]string, error) {
		return cleaning.NewEngine(store, gdb, engineConfig(), nil, nil).VerdictEdges(ctx, engTenant, keys)
	}

	BeforeEach(func() {
		var err error
		ctx, err = tenant.WithTenant(context.Background(), engTenant)
		Expect(err).NotTo(HaveOccurred())
		g = engineGraph(ctx)
		for _, e := range []struct{ id, stage string }{{"e-j2-a1-b1", ""}, {"s-j2-a1-b1", results.StageSynthesized}} {
			props := map[string]any{"job_id": "j2", "relationship_type": "NO_RELATIONSHIP"}
			if e.stage != "" {
				props[results.StageProperty], props["is_viable"] = e.stage, false
			}
			Expect(g.CreateEdge(ctx, engTenant, "a1", "b1", graphdb.Edge{
				ID: e.id, Label: results.SemanticMatchEdgeLabel, ValidFrom: t2, Properties: props,
			})).To(Succeed())
		}
		store = engineStore()
		store.Jobs[engTenant] = append(store.Jobs[engTenant], j2)
	})

	It("returns each verdict's live edges of both stages, sorted, non-viable and NO_RELATIONSHIP included", func() {
		got, err := verdictEdges(g, key("j1", a1, b2), key("j2", a1, b1), key("j1", a1, b1))
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(map[cleaning.VerdictKey][]string{
			key("j1", a1, b2): {"e-j1-a1-b2", "s-j1-a1-b2"},
			key("j2", a1, b1): {"e-j2-a1-b1", "s-j2-a1-b1"},
			key("j1", a1, b1): {"e-j1-a1-b1"},
		}))
	})

	It("leaves out superseded edges and maps a verdict without live edges to nil", func() {
		_, err := cleaning.RetireEdges(ctx, g, engTenant, []string{"e-j1-a1-b2", "e-j2-a1-b1", "s-j2-a1-b1"}, engNow)
		Expect(err).NotTo(HaveOccurred())
		got, err := verdictEdges(g, key("j1", a1, b2), key("j2", a1, b1))
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(HaveLen(2))
		Expect(got).To(HaveKeyWithValue(key("j1", a1, b2), []string{"s-j1-a1-b2"}))
		Expect(got).To(HaveKeyWithValue(key("j2", a1, b1), BeNil()))
	})

	It("maps a key with no edge in the graph to nil", func() {
		got, err := verdictEdges(g, key("j2", a2, b2))
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(HaveKeyWithValue(key("j2", a2, b2), BeNil()))
	})

	It("reads a catalog pair once however many jobs and keys are on it", func() {
		one := &countQueries{GraphDB: g}
		_, err := verdictEdges(one, key("j1", a1, b1))
		Expect(err).NotTo(HaveOccurred())
		many := &countQueries{GraphDB: g}
		_, err = verdictEdges(many, key("j1", a1, b1), key("j1", a2, b2), key("j2", a1, b1))
		Expect(err).NotTo(HaveOccurred())
		Expect(one.calls).To(BeNumerically(">", 0))
		Expect(many.calls).To(Equal(one.calls))
	})

	DescribeTable("refuses a key whose job is not a completed job of the tenant, before reading the graph",
		func(job string) {
			store.Jobs["tenant-other"] = []cleaning.Job{{ID: "j-other", CompletedAt: t1, Catalogs: engJob.Catalogs}}
			counted := &countQueries{GraphDB: g}
			got, err := verdictEdges(counted, key("j1", a1, b1), key(job, a1, b1))
			Expect(err).To(MatchError(cleaning.ErrVerdictJobNotFound))
			Expect(err).To(MatchError(ContainSubstring(job)))
			Expect(err).To(MatchError(ContainSubstring("completed jobs")))
			Expect(err).NotTo(MatchError(ContainSubstring("tenant-other")))
			Expect(got).To(BeNil())
			Expect(counted.calls).To(BeZero())
		},
		Entry("an unknown job", "j-unknown"),
		Entry("another tenant's job", "j-other"),
	)

	It("returns a graph outage", func() {
		got, err := verdictEdges(failQueries{GraphDB: g}, key("j1", a1, b1))
		Expect(err).To(MatchError(errInjected))
		Expect(got).To(BeNil())
	})

	It("refuses a tenant other than the authenticated one without reading", func() {
		counted := &countQueries{GraphDB: g}
		other, err := tenant.WithTenant(context.Background(), "tenant-other")
		Expect(err).NotTo(HaveOccurred())
		got, err := cleaning.NewEngine(store, counted, engineConfig(), nil, nil).VerdictEdges(other, engTenant, []cleaning.VerdictKey{key("j1", a1, b1)})
		Expect(err).To(MatchError(cleaning.ErrTenantMismatch))
		Expect(got).To(BeNil())
		Expect(counted.calls).To(BeZero())
	})

	It("records a span with the tenant and operation", func() {
		tp, err := telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(tp.Shutdown(context.Background())).To(Succeed()) })
		eng := cleaning.NewEngine(store, g, engineConfig(), nil, nil, cleaning.WithEngineTelemetry(tp.TracerProvider(), tp.MeterProvider()))
		_, err = eng.VerdictEdges(ctx, engTenant, []cleaning.VerdictKey{key("j1", a1, b1)})
		Expect(err).NotTo(HaveOccurred())
		span := telemetrytest.FindSpan(tp.GetSpans(), "cleaning.VerdictEdges")
		Expect(span).NotTo(BeNil())
		Expect(spanString(span, "tenant.id")).To(Equal(engTenant))
		Expect(spanString(span, "cleaning.operation")).To(Equal("verdict_edges"))
	})
})
