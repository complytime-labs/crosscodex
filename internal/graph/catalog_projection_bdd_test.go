package graph_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/complytime-labs/crosscodex/internal/graph"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/graphdb/memdriver"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/natsbus/natsbustest"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

const (
	pipelineSubject = "crosscodex.pipeline.*.*.stage.completed"
	catalogWildcard = "crosscodex.catalog.*.*.parsed"
)

type fakeControlsReader struct {
	controls              []graph.CatalogControl
	containers            []graph.CatalogContainer
	err                   error
	containersErr         error
	calls                 int
	gotTenant, gotCatalog string
}

func (f *fakeControlsReader) ListCatalogControls(ctx context.Context, catalogID string) ([]graph.CatalogControl, error) {
	f.calls++
	f.gotTenant, _ = tenant.FromContext(ctx)
	f.gotCatalog = catalogID
	return f.controls, f.err
}

func (f *fakeControlsReader) ListCatalogContainers(ctx context.Context, catalogID string) ([]graph.CatalogContainer, error) {
	if got, _ := tenant.FromContext(ctx); got != f.gotTenant || catalogID != f.gotCatalog {
		return nil, errors.New("fakeControlsReader: containers listed for a different tenant or catalog than the controls")
	}
	return f.containers, f.containersErr
}

// upsertFailingGraph fails UpsertNode for one node ID and delegates the rest.
type upsertFailingGraph struct {
	graphdb.GraphDB
	failID string
}

func (g upsertFailingGraph) UpsertNode(ctx context.Context, tenantID string, node graphdb.Node) (bool, error) {
	if node.ID == g.failID {
		return false, errors.New("connection reset by peer")
	}
	return g.GraphDB.UpsertNode(ctx, tenantID, node)
}

// edgeFailingGraph fails every CreateEdge with a non-sentinel driver error
// and delegates the rest.
type edgeFailingGraph struct {
	graphdb.GraphDB
}

func (edgeFailingGraph) CreateEdge(context.Context, string, string, string, graphdb.Edge) error {
	return errors.New("connection reset by peer")
}

var _ = Describe("Catalog parsed event projection", func() {
	const (
		tenantID  = "test-tenant"
		catalogID = "0123456789abcdef"
		subject   = "crosscodex.catalog." + tenantID + "." + catalogID + ".parsed"
	)

	var (
		ctx    context.Context
		mem    graphdb.GraphDB
		gdb    graphdb.GraphDB
		reader *fakeControlsReader
		bus    *natsbustest.Fake
		failOn string
		tp     *telemetrytest.TestProvider
		logs   *bytes.Buffer
		svc    *graph.Service
	)

	nodeID := func(identifier string) string { return catalogID + "/" + identifier }
	containerID := func(key string) string { return catalogID + "/containers/" + key }
	parentOf := func(parentID, childID string) string {
		return graphdb.DerivedID("parent-of", parentID, childID)
	}

	// Children are listed before their parents to prove every node is written
	// before any edge. ac-1 sits in the nested container g1.1, ac-2 and its
	// part directly in g1.
	fixture := func() []graph.CatalogControl {
		return []graph.CatalogControl{
			{ControlID: nodeID("ac-2_smt"), CatalogID: catalogID, Identifier: "ac-2_smt", Statement: "Manage accounts.",
				ParentID: "ac-2", ContainerKey: "g1", NodeKind: "statement-part"},
			{ControlID: nodeID("ac-1"), CatalogID: catalogID, Identifier: "ac-1", Title: "Policy",
				Statement: "Develop a policy.", Class: "SP800-53", ContainerKey: "g1.1", NodeKind: "control"},
			{ControlID: nodeID("ac-2"), CatalogID: catalogID, Identifier: "ac-2", Title: "Account Management",
				Statement: "Manage accounts.", Class: "SP800-53", ContainerKey: "g1"},
		}
	}

	containerFixture := func() []graph.CatalogContainer {
		return []graph.CatalogContainer{
			{CatalogID: catalogID, Key: "g1.1", Title: "Policies", ParentKey: "g1"},
			{CatalogID: catalogID, Key: "g1", Identifier: "ac", Title: "Access Control"},
		}
	}

	build := func() {
		svc = graph.New(gdb, &mockVectorDB{}, bus,
			graph.WithControlsReader(reader),
			graph.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()),
			graph.WithLogger(slog.New(slog.NewTextHandler(logs, nil))))
		Expect(svc.Start(ctx)).To(Succeed())
		DeferCleanup(func() { _ = svc.Stop(context.Background()) })
	}

	// subFor returns the most recent subscription to subject, or nil.
	subFor := func(subject string) *natsbustest.Sub {
		subs := bus.Subscriptions()
		for i := len(subs) - 1; i >= 0; i-- {
			if subs[i].Subject == subject {
				return subs[i]
			}
		}
		return nil
	}

	// deliverPayload calls the catalog handler directly, not through
	// Fake.Deliver, because the malformed-subject specs feed it subjects the
	// broker would never route to the wildcard subscription.
	deliverPayload := func(subj string, data []byte) error {
		sub := subFor(catalogWildcard)
		Expect(sub).NotTo(BeNil(), "no handler subscribed to %s", catalogWildcard)
		return sub.Handle(&natsbus.Message{Subject: subj, Data: data})
	}

	deliver := func(subj string) error {
		return deliverPayload(subj, []byte(`{"ignored":true}`))
	}

	// nodeCountIn counts the Control nodes present in tenant t's graph among
	// every control ID a spec uses.
	nodeCountIn := func(t string) int {
		n := 0
		for _, id := range []string{"ac-1", "ac-2", "ac-2_smt", "orphan"} {
			node, err := mem.GetNode(ctx, t, nodeID(id))
			if errors.Is(err, graphdb.ErrNodeNotFound) {
				continue
			}
			Expect(err).NotTo(HaveOccurred())
			Expect(node.Label).To(Equal("Control"))
			n++
		}
		return n
	}

	nodeCount := func() int { return nodeCountIn(tenantID) }

	parentEdges := func() []graphdb.Relationship {
		rels, err := mem.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{EdgeLabel: "PARENT_OF"})
		Expect(err).NotTo(HaveOccurred())
		return rels
	}

	eventPoint := func(status string) int64 {
		m := telemetrytest.FindMetric(tp.GetMetrics(), "graph.events.total")
		if m == nil {
			return 0
		}
		sum, ok := m.Data.(metricdata.Sum[int64])
		Expect(ok).To(BeTrue())
		for _, dp := range sum.DataPoints {
			a, _ := dp.Attributes.Value("analyzer")
			s, _ := dp.Attributes.Value("status")
			if a.AsString() == "catalog" && s.AsString() == status {
				return dp.Value
			}
		}
		return 0
	}

	BeforeEach(func() {
		ctx = context.Background()
		mem = memdriver.New()
		Expect(mem.CreateGraph(ctx, tenantID)).To(Succeed())
		gdb = mem
		reader = &fakeControlsReader{controls: fixture(), containers: containerFixture()}
		failOn = ""
		bus = &natsbustest.Fake{QueueSubscribeFunc: func(_ context.Context, subject, _ string, _ natsbus.MessageHandler) (natsbus.Subscription, error) {
			if subject == failOn {
				return nil, errors.New("nats: connection closed")
			}
			// (nil, nil) succeeds with the Sub the Fake recorded for this call.
			return nil, nil
		}}

		var err error
		tp, err = telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = tp.Shutdown(context.Background()) })
		logs = &bytes.Buffer{}
	})

	Describe("subscription", func() {
		It("subscribes to catalog parsed events in the materializer queue group when a reader is set", func() {
			build()
			Expect(subFor(pipelineSubject)).NotTo(BeNil())
			Expect(subFor(catalogWildcard)).NotTo(BeNil())
			Expect(subFor(catalogWildcard).Queue).To(Equal("graph-materializer"))
		})

		It("does not subscribe to catalog events without a reader", func() {
			svc = graph.New(gdb, &mockVectorDB{}, bus)
			Expect(svc.Start(ctx)).To(Succeed())
			Expect(bus.Subscriptions()).To(HaveLen(1))
			Expect(subFor(pipelineSubject)).NotTo(BeNil())
			Expect(svc.Stop(ctx)).To(Succeed())
		})

		It("fails Start, releasing the pipeline subscription, when the catalog subscribe fails", func() {
			failOn = catalogWildcard
			svc = graph.New(gdb, &mockVectorDB{}, bus, graph.WithControlsReader(reader))

			err := svc.Start(ctx)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("catalog events"))
			Expect(subFor(pipelineSubject).Unsubscribed()).To(BeTrue())
			Expect(svc.Stop(ctx)).To(MatchError(graph.ErrNotStarted))

			By("starting cleanly once the bus recovers")
			failOn = ""
			Expect(svc.Start(ctx)).To(Succeed())
			Expect(subFor(catalogWildcard)).NotTo(BeNil())
			Expect(svc.Stop(ctx)).To(Succeed())
		})

		It("drains both subscriptions on Stop", func() {
			build()
			Expect(svc.Stop(ctx)).To(Succeed())
			Expect(subFor(pipelineSubject).Drained()).To(BeTrue())
			Expect(subFor(catalogWildcard).Drained()).To(BeTrue())
			Expect(svc.Stop(ctx)).To(MatchError(graph.ErrNotStarted))
		})

		It("reports a catalog drain failure on Stop", func() {
			build()
			subFor(catalogWildcard).DrainErr = errors.New("drain failed")
			err := svc.Stop(ctx)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("drain failed"))
		})
	})

	Describe("projection", func() {
		It("writes a Control node per row and a PARENT_OF edge per parent link", func() {
			build()
			Expect(deliver(subject)).To(Succeed())

			Expect(reader.gotTenant).To(Equal(tenantID))
			Expect(reader.gotCatalog).To(Equal(catalogID))

			Expect(nodeCount()).To(Equal(3))
			node, err := mem.GetNode(ctx, tenantID, nodeID("ac-1"))
			Expect(err).NotTo(HaveOccurred())
			Expect(node.Label).To(Equal("Control"))
			Expect(node.ValidFrom.IsZero()).To(BeFalse())
			for key, want := range map[string]string{
				"tenant_id":  tenantID,
				"catalog_id": catalogID,
				"identifier": "ac-1",
				"title":      "Policy",
				"statement":  "Develop a policy.",
				"class":      "SP800-53",
				"node_kind":  "control",
			} {
				Expect(node.Properties).To(HaveKeyWithValue(key, want))
			}

			// ac-2 has no node kind: the key is omitted, not set to "".
			unkinded, err := mem.GetNode(ctx, tenantID, nodeID("ac-2"))
			Expect(err).NotTo(HaveOccurred())
			Expect(unkinded.Properties).NotTo(HaveKey("node_kind"))

			By("linking the part to its control")
			edge, err := mem.GetEdge(ctx, tenantID, parentOf(nodeID("ac-2"), nodeID("ac-2_smt")))
			Expect(err).NotTo(HaveOccurred())
			Expect(edge.Label).To(Equal("PARENT_OF"))
			Expect(edge.SourceID).To(Equal(nodeID("ac-2")))
			Expect(edge.TargetID).To(Equal(nodeID("ac-2_smt")))
			Expect(edge.ValidFrom.IsZero()).To(BeFalse(), "edge %s has zero ValidFrom", edge.ID)
		})

		It("writes a Container node per container with its identifier and title", func() {
			build()
			Expect(deliver(subject)).To(Succeed())

			g1, err := mem.GetNode(ctx, tenantID, containerID("g1"))
			Expect(err).NotTo(HaveOccurred())
			Expect(g1.Label).To(Equal("Container"))
			Expect(g1.ValidFrom.IsZero()).To(BeFalse())
			for key, want := range map[string]string{
				"tenant_id":  tenantID,
				"catalog_id": catalogID,
				"identifier": "ac",
				"title":      "Access Control",
				"node_kind":  "container",
			} {
				Expect(g1.Properties).To(HaveKeyWithValue(key, want))
			}

			By("keeping an empty identifier as an empty property")
			nested, err := mem.GetNode(ctx, tenantID, containerID("g1.1"))
			Expect(err).NotTo(HaveOccurred())
			Expect(nested.Properties).To(HaveKeyWithValue("identifier", ""))
			Expect(nested.Properties).To(HaveKeyWithValue("title", "Policies"))
		})

		It("projects published labels and sort keys only where the source gave them", func() {
			reader.controls[1].Label = "AC-2(7)"
			reader.controls[1].SortID = "ac-02.07"
			reader.containers[1].Label = "Access Control Family"
			build()
			Expect(deliver(subject)).To(Succeed())

			labelled, err := mem.GetNode(ctx, tenantID, nodeID("ac-1"))
			Expect(err).NotTo(HaveOccurred())
			Expect(labelled.Properties).To(HaveKeyWithValue("label", "AC-2(7)"))
			Expect(labelled.Properties).To(HaveKeyWithValue("sort_id", "ac-02.07"))

			bare, err := mem.GetNode(ctx, tenantID, nodeID("ac-2"))
			Expect(err).NotTo(HaveOccurred())
			Expect(bare.Properties).NotTo(HaveKey("label"))
			Expect(bare.Properties).NotTo(HaveKey("sort_id"))

			group, err := mem.GetNode(ctx, tenantID, containerID("g1"))
			Expect(err).NotTo(HaveOccurred())
			Expect(group.Properties).To(HaveKeyWithValue("label", "Access Control Family"))
			Expect(group.Properties).NotTo(HaveKey("sort_id"))

			unlabelled, err := mem.GetNode(ctx, tenantID, containerID("g1.1"))
			Expect(err).NotTo(HaveOccurred())
			Expect(unlabelled.Properties).NotTo(HaveKey("label"))
			Expect(unlabelled.Properties).NotTo(HaveKey("sort_id"))
		})

		It("links containers to child containers and to top-level controls only", func() {
			build()
			Expect(deliver(subject)).To(Succeed())

			Expect(parentEdges()).To(HaveLen(4))
			for _, link := range [][2]string{
				{containerID("g1"), containerID("g1.1")},
				{containerID("g1.1"), nodeID("ac-1")},
				{containerID("g1"), nodeID("ac-2")},
				{nodeID("ac-2"), nodeID("ac-2_smt")},
			} {
				edge, err := mem.GetEdge(ctx, tenantID, parentOf(link[0], link[1]))
				Expect(err).NotTo(HaveOccurred())
				Expect(edge.SourceID).To(Equal(link[0]))
				Expect(edge.TargetID).To(Equal(link[1]))
			}
			_, err := mem.GetEdge(ctx, tenantID, parentOf(containerID("g1"), nodeID("ac-2_smt")))
			Expect(err).To(MatchError(graphdb.ErrEdgeNotFound), "a part hangs off its control, not its container")
		})

		It("writes no Container node when the catalog has no controls", func() {
			reader.controls = nil
			build()
			Expect(deliver(subject)).To(HaveOccurred())
			_, err := mem.GetNode(ctx, tenantID, containerID("g1"))
			Expect(err).To(MatchError(graphdb.ErrNodeNotFound))
		})

		It("returns a container listing failure wrapped and leaves the graph unchanged", func() {
			injected := errors.New("connection reset by peer")
			reader.containersErr = injected
			build()

			err := deliver(subject)
			Expect(err).To(MatchError(injected))
			Expect(err.Error()).To(ContainSubstring(catalogID))
			Expect(nodeCount()).To(BeZero())
			_, err = mem.GetNode(ctx, tenantID, containerID("g1"))
			Expect(err).To(MatchError(graphdb.ErrNodeNotFound))
			Expect(eventPoint("error")).To(Equal(int64(1)))
		})

		It("is idempotent when the same event is delivered twice", func() {
			build()
			Expect(deliver(subject)).To(Succeed())
			Expect(deliver(subject)).To(Succeed())

			Expect(nodeCount()).To(Equal(3))
			Expect(parentEdges()).To(HaveLen(4))
			_, err := mem.GetNode(ctx, tenantID, containerID("g1"))
			Expect(err).NotTo(HaveOccurred())
			Expect(logs.String()).NotTo(ContainSubstring("level=WARN"))
		})

		It("refreshes a node in place when the row changes and the catalog is re-imported", func() {
			build()
			Expect(deliver(subject)).To(Succeed())
			reader.controls = fixture()
			reader.controls[1].Title = "Policy and Procedures"
			Expect(deliver(subject)).To(Succeed())

			Expect(nodeCount()).To(Equal(3))
			node, err := mem.GetNode(ctx, tenantID, nodeID("ac-1"))
			Expect(err).NotTo(HaveOccurred())
			Expect(node.Properties["title"]).To(Equal("Policy and Procedures"))
		})

		It("skips and reports a PARENT_OF edge whose parent is not in the catalog", func() {
			reader.controls = append(fixture(), graph.CatalogControl{
				ControlID: nodeID("orphan"), CatalogID: catalogID, Identifier: "orphan", ParentID: "missing",
			})
			build()
			Expect(deliver(subject)).To(Succeed())

			Expect(nodeCount()).To(Equal(4))
			Expect(parentEdges()).To(HaveLen(4))

			m := telemetrytest.FindMetric(tp.GetMetrics(), "graph.materialize.edges_skipped.total")
			Expect(m).NotTo(BeNil())
			sum, ok := m.Data.(metricdata.Sum[int64])
			Expect(ok).To(BeTrue())
			Expect(sum.DataPoints).To(HaveLen(1))
			Expect(sum.DataPoints[0].Value).To(Equal(int64(1)))
			v, _ := sum.DataPoints[0].Attributes.Value("analyzer")
			Expect(v.AsString()).To(Equal("catalog"))
			v, _ = sum.DataPoints[0].Attributes.Value("edge.label")
			Expect(v.AsString()).To(Equal("PARENT_OF"))
		})

		DescribeTable("drops a malformed subject without reading or writing",
			func(subj string) {
				build()
				Expect(deliver(subj)).To(Succeed())
				Expect(reader.calls).To(BeZero())
				Expect(nodeCount()).To(BeZero())
				Expect(eventPoint("error")).To(Equal(int64(1)))
			},
			Entry("too few tokens", "crosscodex.catalog.x"),
			Entry("invalid tenant", "crosscodex.catalog.bad tenant!.cat.parsed"),
			Entry("too many tokens", "crosscodex.catalog.t.c.parsed.x"),
			Entry("wildcard catalog ID", "crosscodex.catalog."+tenantID+".*.parsed"),
			Entry("full wildcard catalog ID", "crosscodex.catalog."+tenantID+".>.parsed"),
			Entry("wrong trailing token", "crosscodex.catalog."+tenantID+"."+catalogID+".deleted"),
			Entry("empty catalog ID", "crosscodex.catalog."+tenantID+"..parsed"),
		)

		It("returns a reader failure wrapped and leaves the graph unchanged", func() {
			injected := errors.New("connection reset by peer")
			reader.err = injected
			build()

			err := deliver(subject)
			Expect(errors.Is(err, injected)).To(BeTrue(), "got %v", err)
			Expect(err.Error()).To(ContainSubstring(catalogID))
			Expect(nodeCount()).To(BeZero())
			Expect(eventPoint("error")).To(Equal(int64(1)))
		})

		It("fails with re-import guidance when the catalog has no controls", func() {
			reader.controls = nil
			build()

			err := deliver(subject)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(catalogID))
			Expect(err.Error()).To(ContainSubstring("re-import the catalog"))
			Expect(nodeCount()).To(BeZero())
		})

		It("writes the other nodes and names the failed one when an upsert fails", func() {
			gdb = upsertFailingGraph{GraphDB: mem, failID: nodeID("ac-1")}
			build()

			err := deliver(subject)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(nodeID("ac-1")))
			Expect(nodeCount()).To(Equal(2))
			_, err = mem.GetNode(ctx, tenantID, nodeID("ac-2"))
			Expect(err).NotTo(HaveOccurred())
			_, err = mem.GetNode(ctx, tenantID, containerID("g1"))
			Expect(err).NotTo(HaveOccurred())
		})

		It("writes the nodes and names both endpoints when an edge write fails", func() {
			gdb = edgeFailingGraph{GraphDB: mem}
			build()

			err := deliver(subject)
			Expect(err).To(HaveOccurred())
			for _, link := range []string{
				containerID("g1") + " -> " + containerID("g1.1"),
				containerID("g1.1") + " -> " + nodeID("ac-1"),
				nodeID("ac-2") + " -> " + nodeID("ac-2_smt"),
			} {
				Expect(err.Error()).To(ContainSubstring(link))
			}
			Expect(err.Error()).To(ContainSubstring("connection reset by peer"))
			Expect(nodeCount()).To(Equal(3))
			Expect(parentEdges()).To(BeEmpty())
		})

		Context("with another tenant's graph present", func() {
			const otherTenant = "tenant-a"

			BeforeEach(func() {
				Expect(mem.CreateGraph(ctx, otherTenant)).To(Succeed())
			})

			DescribeTable("projects only into the subject's tenant, whatever the payload says",
				func(payload string) {
					const subjectTenant = "tenant-b"
					Expect(mem.CreateGraph(ctx, subjectTenant)).To(Succeed())
					build()

					Expect(deliverPayload("crosscodex.catalog."+subjectTenant+"."+catalogID+".parsed", []byte(payload))).To(Succeed())

					Expect(reader.gotTenant).To(Equal(subjectTenant))
					Expect(reader.gotCatalog).To(Equal(catalogID))
					Expect(nodeCountIn(subjectTenant)).To(Equal(3))
					node, err := mem.GetNode(ctx, subjectTenant, containerID("g1"))
					Expect(err).NotTo(HaveOccurred())
					Expect(node.Properties).To(HaveKeyWithValue("tenant_id", subjectTenant))
					Expect(nodeCountIn(otherTenant)).To(BeZero(), "tenant-a's graph must stay unchanged")
					rels, err := mem.QueryRelationships(ctx, otherTenant, graphdb.RelationshipQuery{})
					Expect(err).NotTo(HaveOccurred())
					Expect(rels).To(BeEmpty())
				},
				Entry("empty payload", ``),
				Entry("payload naming another tenant and catalog", `{"tenant_id":"tenant-a","catalog_id":"other"}`),
			)
		})

		It("records a span with tenant and catalog and counts the event", func() {
			build()
			Expect(deliver(subject)).To(Succeed())

			span := telemetrytest.FindSpan(tp.GetSpans(), "graph.materialize.catalog")
			Expect(span).NotTo(BeNil())
			attrs := attribute.NewSet(span.Attributes()...)
			v, _ := attrs.Value("tenant.id")
			Expect(v.AsString()).To(Equal(tenantID))
			v, _ = attrs.Value("catalog.id")
			Expect(v.AsString()).To(Equal(catalogID))
			Expect(eventPoint("ok")).To(Equal(int64(1)))
		})
	})
})
