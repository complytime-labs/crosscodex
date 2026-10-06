package graph_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/complytime-labs/crosscodex/internal/graph"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/graphdb/memdriver"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
)

// failingEdgeGraph fails every CreateEdge with a non-sentinel driver error.
type failingEdgeGraph struct {
	graphdb.GraphDB
}

func (failingEdgeGraph) CreateEdge(context.Context, string, string, string, graphdb.Edge) error {
	return errors.New("connection reset by peer")
}

// Regression (#160): the subscriber runs on a core-NATS queue subscription,
// so a handler error is never redelivered. An edge whose endpoint node is
// missing must be skipped and reported, not abort the remaining edges.
var _ = Describe("Subscriber edge with a missing endpoint node", func() {
	const (
		tenantID      = "test-tenant"
		skippedMetric = "graph.materialize.edges_skipped.total"
	)

	var (
		ctx      context.Context
		gdb      graphdb.GraphDB
		resolver *mockResolver
		registry *graph.ResolverRegistry
		tp       *telemetrytest.TestProvider
		logs     *bytes.Buffer
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
		registry = graph.NewResolverRegistry()
		registry.Register(resolver)

		var err error
		tp, err = telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = tp.Shutdown(context.Background()) })

		logs = &bytes.Buffer{}
		svc = graph.New(gdb, &mockVectorDB{}, nil,
			graph.WithResolverRegistry(registry),
			graph.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()),
			graph.WithLogger(slog.New(slog.NewTextHandler(logs, nil))))
	})

	deliver := func(analyzer string, results []map[string]any) error {
		data, err := json.Marshal(results)
		Expect(err).NotTo(HaveOccurred())
		resolver.data = data
		eventData, err := json.Marshal(map[string]string{"analyzer": analyzer, "job_id": "job-1", "stage": "completed"})
		Expect(err).NotTo(HaveOccurred())
		return graph.ExportHandleEvent(svc, ctx, &natsbus.Message{
			Subject: "crosscodex.pipeline." + tenantID + ".job-1.stage.completed",
			Data:    eventData,
		})
	}

	edgeCount := func(label string) int {
		rels, err := gdb.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{EdgeLabel: label})
		Expect(err).NotTo(HaveOccurred())
		return len(rels)
	}

	DescribeTable("skips only the edge whose endpoint is missing and reports it",
		func(analyzer, edgeLabel, sourceID, targetID string, results []map[string]any) {
			Expect(deliver(analyzer, results)).To(Succeed())

			By("writing the edge listed after the skipped one")
			Expect(edgeCount(edgeLabel)).To(Equal(1))

			By("counting the skip by analyzer and edge label")
			m := telemetrytest.FindMetric(tp.GetMetrics(), skippedMetric)
			Expect(m).NotTo(BeNil(), "metric %s not recorded", skippedMetric)
			sum, ok := m.Data.(metricdata.Sum[int64])
			Expect(ok).To(BeTrue())
			Expect(sum.DataPoints).To(HaveLen(1))
			Expect(sum.DataPoints[0].Value).To(Equal(int64(1)))
			attrs := sum.DataPoints[0].Attributes
			Expect(attrs.Len()).To(Equal(2), "metric must stay low-cardinality: %v", attrs.ToSlice())
			v, _ := attrs.Value("analyzer")
			Expect(v.AsString()).To(Equal(analyzer))
			v, _ = attrs.Value("edge.label")
			Expect(v.AsString()).To(Equal(edgeLabel))

			By("recording a span event that names the edge and the missing node")
			span := telemetrytest.FindSpan(tp.GetSpans(), "graph.materialize."+analyzer)
			Expect(span).NotTo(BeNil())
			Expect(span.Events()).To(HaveLen(1))
			ev := span.Events()[0]
			Expect(ev.Name).To(Equal("edge skipped: endpoint node missing"))
			evAttrs := attribute.NewSet(ev.Attributes...)
			v, _ = evAttrs.Value("edge.label")
			Expect(v.AsString()).To(Equal(edgeLabel))
			v, _ = evAttrs.Value("edge.source_id")
			Expect(v.AsString()).To(Equal(sourceID))
			v, _ = evAttrs.Value("edge.target_id")
			Expect(v.AsString()).To(Equal(targetID))
			_, ok = evAttrs.Value("error.message")
			Expect(ok).To(BeTrue())

			By("logging a warning that tells the operator how to recover")
			Expect(logs.String()).To(ContainSubstring("level=WARN"))
			Expect(logs.String()).To(ContainSubstring("tenant=" + tenantID))
			Expect(logs.String()).To(ContainSubstring("edge.label=" + edgeLabel))
			Expect(logs.String()).To(ContainSubstring("source_id=" + sourceID))
			Expect(logs.String()).To(ContainSubstring("target_id=" + targetID))
			Expect(logs.String()).To(ContainSubstring("re-import"))
		},
		Entry("SEMANTIC_MATCH", "relationship", "SEMANTIC_MATCH", "ctrl-missing", "ctrl-2", []map[string]any{
			{"source_id": "ctrl-missing", "target_id": "ctrl-2", "relationship_type": "EQUIVALENT", "confidence": 0.9},
			{"source_id": "ctrl-1", "target_id": "ctrl-2", "relationship_type": "EQUIVALENT", "confidence": 0.9},
		}),
		Entry("REQUIRES", "requires", "REQUIRES", "ctrl-1", "ctrl-missing", []map[string]any{
			{"source_id": "ctrl-1", "target_id": "ctrl-missing", "confidence": 0.95, "unanimous": true, "valid_votes": 3, "total_votes": 3, "models": []string{"m1"}},
			{"source_id": "ctrl-1", "target_id": "ctrl-2", "confidence": 0.95, "unanimous": true, "valid_votes": 3, "total_votes": 3, "models": []string{"m1"}},
		}),
		Entry("DEMANDS", "artifacts", "DEMANDS", "ctrl-missing", "ctrl-missing__art_0", []map[string]any{
			{"control_id": "ctrl-missing", "artifacts": []map[string]any{{"name": "a", "type": "LOG", "frequency": "QUARTERLY", "owner_role": "ADMIN", "confidence": 0.9}}},
			{"control_id": "ctrl-1", "artifacts": []map[string]any{{"name": "b", "type": "LOG", "frequency": "QUARTERLY", "owner_role": "ADMIN", "confidence": 0.9}}},
		}),
	)

	It("records no skip when every endpoint exists", func() {
		Expect(deliver("relationship", []map[string]any{
			{"source_id": "ctrl-1", "target_id": "ctrl-2", "relationship_type": "EQUIVALENT", "confidence": 0.9},
		})).To(Succeed())

		m := telemetrytest.FindMetric(tp.GetMetrics(), skippedMetric)
		if m != nil {
			total, err := telemetrytest.CounterValue(m)
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(BeZero())
		}
		Expect(logs.String()).NotTo(ContainSubstring("level=WARN"))
	})

	It("still fails the event on a driver error other than a missing node", func() {
		svc = graph.New(failingEdgeGraph{GraphDB: gdb}, &mockVectorDB{}, nil,
			graph.WithResolverRegistry(registry),
			graph.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()))

		err := deliver("relationship", []map[string]any{
			{"source_id": "ctrl-1", "target_id": "ctrl-2", "relationship_type": "EQUIVALENT", "confidence": 0.9},
		})
		Expect(err).To(MatchError(ContainSubstring("create SEMANTIC_MATCH edge ctrl-1->ctrl-2")))
		Expect(err).To(MatchError(ContainSubstring("connection reset by peer")))
		Expect(edgeCount("SEMANTIC_MATCH")).To(BeZero())
		Expect(telemetrytest.FindMetric(tp.GetMetrics(), skippedMetric)).To(BeNil())
	})
})
