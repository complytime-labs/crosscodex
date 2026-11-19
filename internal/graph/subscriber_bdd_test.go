package graph_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/complytime-labs/crosscodex/internal/graph"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/natsbus/natsbustest"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
)

var _ = Describe("Subscriber Event Handling", func() {
	var (
		svc       *graph.Service
		mockGraph *mockGraphDB
		resolver  *mockResolver
	)

	BeforeEach(func() {
		mockGraph = &mockGraphDB{}
		mockVectors := &mockVectorDB{}
		resolver = &mockResolver{scheme: "pg", data: []byte(`[]`)}
		registry := graph.NewResolverRegistry()
		registry.Register(resolver)
		svc = graph.New(mockGraph, mockVectors, nil,
			graph.WithResolverRegistry(registry),
		)
	})

	Describe("handleEvent", func() {
		// memService swaps svc for one backed by a memdriver graph holding
		// Control nodes ctrl-1 and ctrl-2, and returns that graph.
		memService := func() graphdb.GraphDB {
			db := newMemGraph(context.Background(), "test-tenant", "ctrl-1", "ctrl-2")
			registry := graph.NewResolverRegistry()
			registry.Register(resolver)
			svc = graph.New(db, &mockVectorDB{}, nil, graph.WithResolverRegistry(registry))
			return db
		}

		deliver := func(analyzer string, results any) error {
			data, err := json.Marshal(results)
			Expect(err).NotTo(HaveOccurred())
			resolver.data = data
			eventData, err := json.Marshal(map[string]string{"analyzer": analyzer, "job_id": "job-1", "stage": "completed"})
			Expect(err).NotTo(HaveOccurred())
			return graph.ExportHandleEvent(svc, tenantCtx("test-tenant"), &natsbus.Message{
				Subject: "crosscodex.pipeline.test-tenant.job-1.stage.completed",
				Data:    eventData,
			})
		}

		relationships := func(db graphdb.GraphDB, label string) []graphdb.Relationship {
			rels, err := db.QueryRelationships(context.Background(), "test-tenant", graphdb.RelationshipQuery{EdgeLabel: label})
			Expect(err).NotTo(HaveOccurred())
			return rels
		}

		DescribeTable("does not resolve results for a stage it has no handler for",
			func(analyzer string) {
				resolver.err = errors.New("resolver must not be called")
				data, err := json.Marshal(map[string]string{
					"analyzer": analyzer, "job_id": "job-1", "stage": "completed",
					"timestamp": "2026-10-30T22:38:22Z",
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(graph.ExportHandleEvent(svc, tenantCtx("test-tenant"), &natsbus.Message{
					Subject: "crosscodex.pipeline.test-tenant.job-1.stage.completed",
					Data:    data,
				})).To(Succeed())
			},
			Entry("graph", "graph"),
			Entry("an unknown analyzer", "unknown-analyzer"),
		)

		It("counts a stage event with no graph handler as skipped", func() {
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = tp.Shutdown(context.Background()) })
			registry := graph.NewResolverRegistry()
			registry.Register(resolver)
			svc = graph.New(mockGraph, &mockVectorDB{}, nil,
				graph.WithResolverRegistry(registry),
				graph.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()))

			Expect(deliver("graph", []any{})).To(Succeed())

			m := telemetrytest.FindMetric(tp.GetMetrics(), "graph.events.total")
			Expect(m).NotTo(BeNil(), "graph.events.total not recorded")
			sum, ok := m.Data.(metricdata.Sum[int64])
			Expect(ok).To(BeTrue())
			Expect(sum.DataPoints).To(HaveLen(1))
			Expect(sum.DataPoints[0].Value).To(Equal(int64(1)))
			status, _ := sum.DataPoints[0].Attributes.Value("status")
			Expect(status.AsString()).To(Equal("skipped"))
			analyzer, _ := sum.DataPoints[0].Attributes.Value("analyzer")
			Expect(analyzer.AsString()).To(Equal("graph"))
		})

		It("acks an event from an analyzer it has no handler for", func() {
			err := graph.ExportHandleEvent(svc, tenantCtx("test-tenant"), &natsbus.Message{
				Subject: "crosscodex.pipeline.test-tenant.job-1.stage.completed",
				Data:    []byte(`{"analyzer":"unknown-analyzer","job_id":"job-1","stage":"completed"}`),
			})
			Expect(err).NotTo(HaveOccurred())
		})

		DescribeTable("terms, so the message is not redelivered, an event it can never process",
			func(subject string, data string) {
				resolver.err = errors.New("resolver must not be called")
				err := graph.ExportHandleEvent(svc, tenantCtx("test-tenant"), &natsbus.Message{Subject: subject, Data: []byte(data)})
				Expect(err).To(MatchError(natsbus.ErrPermanent))
			},
			Entry("a malformed subject",
				"bad.subject", `{}`),
			Entry("an invalid tenant ID in the subject",
				"crosscodex.pipeline.invalid!tenant.job-1.stage.completed",
				`{"analyzer":"relationship","job_id":"job-1","stage":"completed"}`),
			Entry("malformed JSON",
				"crosscodex.pipeline.test-tenant.job-1.stage.completed", `{bad json`),
			Entry("missing required fields",
				"crosscodex.pipeline.test-tenant.job-1.stage.completed", `{"stage":"completed"}`),
			Entry("a subject with an extra token",
				"crosscodex.pipeline.test-tenant.job-1.stage.completed.x",
				`{"analyzer":"relationship","job_id":"job-1","stage":"completed"}`),
			Entry("a payload naming another job than the subject",
				"crosscodex.pipeline.test-tenant.job-1.stage.completed",
				`{"analyzer":"relationship","job_id":"job-2","stage":"completed"}`),
		)

		It("returns error when resolver fails", func() {
			resolver.err = errors.New("resolution failed")
			event := map[string]string{
				"analyzer": "relationship",
				"job_id":   "job-1",
				"stage":    "completed",
			}
			data, _ := json.Marshal(event)
			msg := &natsbus.Message{
				Subject: "crosscodex.pipeline.test-tenant.job-1.stage.completed",
				Data:    data,
			}

			err := graph.ExportHandleEvent(svc, tenantCtx("test-tenant"), msg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("resolve"))
		})

		It("materializes relationship edges", func() {
			db := memService()
			Expect(deliver("relationship", []map[string]any{{
				"source_id":         "ctrl-1",
				"target_id":         "ctrl-2",
				"relationship_type": "implements",
				"confidence":        0.9,
				"properties":        map[string]string{"method": "llm"},
			}})).To(Succeed())

			rels := relationships(db, "SEMANTIC_MATCH")
			Expect(rels).To(HaveLen(1))
			Expect(rels[0].Source.ID).To(Equal("ctrl-1"))
			Expect(rels[0].Target.ID).To(Equal("ctrl-2"))
			Expect(rels[0].Edge.Properties).To(HaveKeyWithValue("relationship_type", "implements"))
			Expect(rels[0].Edge.Properties).To(HaveKeyWithValue("stage", "analyzer"))
			Expect(rels[0].Edge.Confidence).To(BeNumerically("~", 0.9, 0.01))
		})

		It("returns error when CreateEdge fails with non-idempotent error", func() {
			relationshipData := []map[string]any{
				{
					"source_id":         "ctrl-1",
					"target_id":         "ctrl-2",
					"relationship_type": "implements",
					"confidence":        0.9,
				},
			}
			data, _ := json.Marshal(relationshipData)
			resolver.data = data

			mockGraph.createEdgeFunc = func(_ context.Context, _, _, _ string, _ graphdb.Edge) error {
				return errors.New("connection refused")
			}

			event := map[string]string{
				"analyzer": "relationship",
				"job_id":   "job-1",
				"stage":    "completed",
			}
			eventData, _ := json.Marshal(event)
			msg := &natsbus.Message{
				Subject: "crosscodex.pipeline.test-tenant.job-1.stage.completed",
				Data:    eventData,
			}

			err := graph.ExportHandleEvent(svc, tenantCtx("test-tenant"), msg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("materialize"))
		})

		It("returns error when resolver returns invalid JSON for relationship", func() {
			resolver.data = []byte(`{not valid json}`)

			event := map[string]string{
				"analyzer": "relationship",
				"job_id":   "job-1",
				"stage":    "completed",
			}
			eventData, _ := json.Marshal(event)
			msg := &natsbus.Message{
				Subject: "crosscodex.pipeline.test-tenant.job-1.stage.completed",
				Data:    eventData,
			}

			err := graph.ExportHandleEvent(svc, tenantCtx("test-tenant"), msg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("unmarshal"))
		})

		It("materializes requires edges", func() {
			db := memService()
			Expect(deliver("requires", []map[string]any{{
				"source_id":   "ctrl-1",
				"target_id":   "ctrl-2",
				"confidence":  0.95,
				"unanimous":   true,
				"valid_votes": 3,
				"total_votes": 3,
				"models":      []string{"claude-3-5-sonnet-20241022", "gpt-4o"},
			}})).To(Succeed())

			rels := relationships(db, "REQUIRES")
			Expect(rels).To(HaveLen(1))
			Expect(rels[0].Source.ID).To(Equal("ctrl-1"))
			Expect(rels[0].Target.ID).To(Equal("ctrl-2"))
			Expect(rels[0].Edge.Properties).To(HaveKeyWithValue("unanimous", true))
		})

		It("materializes artifact nodes and edges", func() {
			db := memService()
			Expect(deliver("artifacts", []map[string]any{{
				"control_id": "ctrl-1",
				"artifacts": []map[string]any{{
					"name":       "Security Log",
					"type":       "log",
					"frequency":  "daily",
					"owner_role": "security-team",
					"confidence": 0.85,
				}},
			}})).To(Succeed())

			demands := relationships(db, "DEMANDS")
			Expect(demands).To(HaveLen(1))
			Expect(demands[0].Source.ID).To(Equal("ctrl-1"))
			Expect(demands[0].Target.ID).To(ContainSubstring("ctrl-1__art_0"))
			Expect(demands[0].Target.Label).To(Equal("Artifact"))
			Expect(demands[0].Target.Properties).To(HaveKeyWithValue("name", "Security Log"))

			isType := relationships(db, "IS_TYPE")
			Expect(isType).To(HaveLen(1))
			Expect(isType[0].Source.ID).To(Equal(demands[0].Target.ID))
			Expect(isType[0].Target.Label).To(Equal("ArtifactType"))
			Expect(isType[0].Target.Properties).To(HaveKeyWithValue("name", "log"))
		})

		It("skips classify materialization gracefully", func() {
			resolver.data = []byte(`[]`)
			event := map[string]string{
				"analyzer": "classify",
				"job_id":   "job-1",
				"stage":    "completed",
			}
			eventData, _ := json.Marshal(event)
			msg := &natsbus.Message{
				Subject: "crosscodex.pipeline.test-tenant.job-1.stage.completed",
				Data:    eventData,
			}

			err := graph.ExportHandleEvent(svc, tenantCtx("test-tenant"), msg)
			Expect(err).NotTo(HaveOccurred())
		})

		// Regression (#216): the dispatch case must match the embedding
		// analyzer's Name(), or its events fall through to "no handler".
		It("routes embedding analyzer events to the embed handler", func() {
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = tp.Shutdown(context.Background()) })
			registry := graph.NewResolverRegistry()
			registry.Register(resolver)
			svc = graph.New(mockGraph, &mockVectorDB{}, nil,
				graph.WithResolverRegistry(registry),
				graph.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()))

			Expect(deliver("embedding", []any{})).To(Succeed())

			span := telemetrytest.FindSpan(tp.GetSpans(), "graph.materialize.embedding")
			Expect(span).NotTo(BeNil())
			Expect(span.Status().Code).To(Equal(codes.Ok))

			// Only a matched handler records duration; the default branch
			// returns before recordMaterialize.
			m := telemetrytest.FindMetric(tp.GetMetrics(), "graph.materialize.duration_ms")
			Expect(m).NotTo(BeNil(), "materialize duration not recorded")
			hist, ok := m.Data.(metricdata.Histogram[float64])
			Expect(ok).To(BeTrue())
			Expect(hist.DataPoints).To(HaveLen(1))
			v, _ := hist.DataPoints[0].Attributes.Value("analyzer")
			Expect(v.AsString()).To(Equal("embedding"))
		})
	})
})

var _ = Describe("Subscriber Lifecycle", func() {
	var (
		svc     *graph.Service
		mockBus *natsbustest.Fake
	)

	BeforeEach(func() {
		mockBus = &natsbustest.Fake{}
		svc = graph.New(&mockGraphDB{}, &mockVectorDB{}, mockBus)
	})

	It("starts successfully", func() {
		err := svc.Start(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(svc.Stop(context.Background())).To(Succeed())
	})

	It("returns ErrAlreadyStarted on double start", func() {
		Expect(svc.Start(context.Background())).To(Succeed())
		err := svc.Start(context.Background())
		Expect(err).To(MatchError(graph.ErrAlreadyStarted))
		Expect(svc.Stop(context.Background())).To(Succeed())
	})

	It("returns ErrNotStarted when stopping before start", func() {
		err := svc.Stop(context.Background())
		Expect(err).To(MatchError(graph.ErrNotStarted))
	})

	It("propagates drain errors on stop", func() {
		Expect(svc.Start(context.Background())).To(Succeed())
		consumerSub(mockBus, materializerConsumer).DrainErr = errors.New("drain failed")
		err := svc.Stop(context.Background())
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("drain"))
	})
})

var _ = Describe("Durable materialization consumer", func() {
	const (
		tenantID   = "test-tenant"
		maxDeliver = 3
	)

	var (
		bus      *natsbustest.Fake
		mem      graphdb.GraphDB
		resolver *mockResolver
		tp       *telemetrytest.TestProvider
		logs     *bytes.Buffer
		svc      *graph.Service
	)

	build := func(g graphdb.GraphDB) {
		registry := graph.NewResolverRegistry()
		registry.Register(resolver)
		svc = graph.New(g, &mockVectorDB{}, bus,
			graph.WithResolverRegistry(registry),
			graph.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()),
			graph.WithLogger(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))),
			graph.WithMaterializeConfig(config.NATSMaterializeConfig{MaxDeliver: maxDeliver, AckWait: time.Minute, RedeliverDelay: 2 * time.Second}))
		Expect(svc.Start(context.Background())).To(Succeed())
		DeferCleanup(func() { _ = svc.Stop(context.Background()) })
	}

	materializer := func() *natsbustest.Sub {
		sub := consumerSub(bus, materializerConsumer)
		Expect(sub).NotTo(BeNil(), "no %s consumer", materializerConsumer)
		return sub
	}

	semanticMatches := func(t string) []graphdb.Relationship {
		rels, err := mem.QueryRelationships(context.Background(), t, graphdb.RelationshipQuery{EdgeLabel: "SEMANTIC_MATCH"})
		Expect(err).NotTo(HaveOccurred())
		return rels
	}

	BeforeEach(func() {
		mem = newMemGraph(context.Background(), tenantID, "ctrl-1", "ctrl-2")
		resolver = &mockResolver{scheme: "pg", data: []byte(
			`[{"source_id":"ctrl-1","target_id":"ctrl-2","relationship_type":"EQUIVALENT","confidence":0.9}]`)}
		bus = &natsbustest.Fake{}
		var err error
		tp, err = telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = tp.Shutdown(context.Background()) })
		logs = &bytes.Buffer{}
	})

	It("consumes stage events and barriers from MATERIALIZE with the configured delivery limits", func() {
		build(mem)

		Expect(materializer().Config).To(Equal(natsbus.ConsumerConfig{
			Stream:         natsbus.MaterializeStream,
			Durable:        materializerConsumer,
			FilterSubjects: []string{"crosscodex.pipeline.*.*.stage.completed"},
			AckWait:        time.Minute,
			MaxDeliver:     maxDeliver,
			RedeliverDelay: 2 * time.Second,
		}))
		barrier := consumerSub(bus, barrierConsumer)
		Expect(barrier).NotTo(BeNil())
		Expect(barrier.Config).To(Equal(natsbus.ConsumerConfig{
			Stream:         natsbus.MaterializeStream,
			Durable:        barrierConsumer,
			FilterSubjects: []string{"crosscodex.materialize.*.*.barrier"},
			AckWait:        time.Minute,
			MaxDeliver:     -1,
			RedeliverDelay: 2 * time.Second,
		}))
		Expect(bus.Subscriptions()).To(HaveLen(2))
	})

	It("acks an event once its graph writes succeed", func() {
		build(mem)

		Expect(bus.Deliver(stageEvent(tenantID, "relationship", 1))).To(Succeed())

		Expect(materializer().Acked()).To(Equal(1))
		Expect(semanticMatches(tenantID)).To(HaveLen(1))
		Expect(bus.Published()).To(BeEmpty())
		Expect(eventCount(tp, "relationship", "ok")).To(Equal(int64(1)))
	})

	It("naks an event whose graph write failed, for redelivery", func() {
		build(failingEdgeGraph{GraphDB: mem})

		err := bus.Deliver(stageEvent(tenantID, "relationship", 1))

		Expect(err).To(HaveOccurred())
		Expect(err).NotTo(MatchError(natsbus.ErrPermanent))
		Expect(err.Error()).To(ContainSubstring("delivery 1 of 3"))
		Expect(err.Error()).To(ContainSubstring("connection reset by peer"))
		Expect(materializer().Naked()).To(Equal(1))
		Expect(bus.Published()).To(BeEmpty(), "only the last delivery reports a failure")
		Expect(semanticMatches(tenantID)).To(BeEmpty())
	})

	It("publishes a failed outcome and terms the event on its last delivery", func() {
		build(failingEdgeGraph{GraphDB: mem})

		err := bus.Deliver(stageEvent(tenantID, "relationship", maxDeliver))

		Expect(err).To(MatchError(natsbus.ErrPermanent))
		Expect(materializer().Termed()).To(Equal(1))
		pubs := bus.Published()
		Expect(pubs).To(HaveLen(1))
		Expect(pubs[0].Subject).To(Equal("crosscodex.audit." + tenantID + ".events.job-1.graph.failed"))
		Expect(pubs[0].Durable).To(BeTrue())
		var outcome natsbus.GraphOutcome
		Expect(json.Unmarshal(pubs[0].Data, &outcome)).To(Succeed())
		Expect(outcome.JobID).To(Equal("job-1"))
		Expect(outcome.Outcome).To(Equal(natsbus.GraphOutcomeFailed))
		Expect(outcome.Analyzer).To(Equal("relationship"))
		Expect(outcome.Error).To(ContainSubstring("graph role logs"))
		Expect(outcome.Error).NotTo(ContainSubstring("connection reset"), "the outcome must not carry driver detail")
		Expect(eventCount(tp, "relationship", "failed")).To(Equal(int64(1)))
	})

	It("naks the last delivery, keeping the event, when its failed outcome is not stored", func() {
		bus.PublishDurableFunc = func(context.Context, string, []byte, map[string][]string) error {
			return fmt.Errorf("publish: %w: %w", natsbus.ErrPublishFailed, natsbus.ErrNotStored)
		}
		build(failingEdgeGraph{GraphDB: mem})

		err := bus.Deliver(stageEvent(tenantID, "relationship", maxDeliver))

		Expect(err).To(MatchError(natsbus.ErrNotStored))
		Expect(err).NotTo(MatchError(natsbus.ErrPermanent))
		Expect(materializer().Naked()).To(Equal(1))
		Expect(materializer().Termed()).To(BeZero())
	})

	It("ignores an event with no analyzer without counting an error", func() {
		build(mem)
		msg := stageEvent(tenantID, "", 1)

		Expect(bus.Deliver(msg)).To(MatchError(natsbus.ErrPermanent))

		Expect(materializer().Termed()).To(Equal(1))
		Expect(eventCount(tp, "unknown", "skipped")).To(Equal(int64(1)))
		Expect(eventCount(tp, "unknown", "error")).To(BeZero())
		Expect(logs.String()).To(ContainSubstring("level=DEBUG"))
		Expect(logs.String()).NotTo(ContainSubstring("level=ERROR"))
		Expect(semanticMatches(tenantID)).To(BeEmpty())
	})

	It("terms an event whose subject names another tenant than its provenance, writing nothing", func() {
		const otherTenant = "tenant-b"
		Expect(mem.CreateGraph(context.Background(), otherTenant)).To(Succeed())
		for _, id := range []string{"ctrl-1", "ctrl-2"} {
			Expect(mem.CreateNode(context.Background(), otherTenant, graphdb.Node{ID: id, Label: "Control", ValidFrom: time.Now().UTC()})).To(Succeed())
		}
		build(mem)
		msg := stageEvent(otherTenant, "relationship", 1)
		msg.Metadata.TenantID = tenantID
		msg.Data = []byte(`{"analyzer":"relationship","job_id":"job-1","stage":"completed","metadata":{"marker":"payload-secret"}}`)

		err := bus.Deliver(msg)

		Expect(err).To(MatchError(natsbus.ErrPermanent))
		Expect(err.Error()).To(ContainSubstring("tenant"))
		Expect(materializer().Termed()).To(Equal(1))
		Expect(semanticMatches(otherTenant)).To(BeEmpty())
		Expect(semanticMatches(tenantID)).To(BeEmpty())
		Expect(eventCount(tp, "unknown", "rejected")).To(Equal(int64(1)))
		Expect(logs.String()).To(ContainSubstring("level=WARN"))
		Expect(logs.String()).NotTo(ContainSubstring("payload-secret"))
		Expect(err.Error()).NotTo(ContainSubstring("payload-secret"))
	})
})
