package graph_test

import (
	"context"
	"encoding/json"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/graph"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
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
			return graph.ExportHandleEvent(svc, context.Background(), &natsbus.Message{
				Subject: "crosscodex.pipeline.test-tenant.job-1.stage.completed",
				Data:    eventData,
			})
		}

		relationships := func(db graphdb.GraphDB, label string) []graphdb.Relationship {
			rels, err := db.QueryRelationships(context.Background(), "test-tenant", graphdb.RelationshipQuery{EdgeLabel: label})
			Expect(err).NotTo(HaveOccurred())
			return rels
		}

		It("skips events with unknown analyzers", func() {
			event := map[string]string{
				"analyzer": "unknown-analyzer",
				"job_id":   "job-1",
				"stage":    "completed",
			}
			data, _ := json.Marshal(event)
			msg := &natsbus.Message{
				Subject: "crosscodex.pipeline.test-tenant.job-1.stage.completed",
				Data:    data,
			}

			err := graph.ExportHandleEvent(svc, context.Background(), msg)
			Expect(err).NotTo(HaveOccurred())
		})

		It("rejects events with malformed subjects", func() {
			msg := &natsbus.Message{
				Subject: "bad.subject",
				Data:    []byte(`{}`),
			}

			err := graph.ExportHandleEvent(svc, context.Background(), msg)
			Expect(err).NotTo(HaveOccurred()) // nil return = don't redeliver
		})

		It("rejects events with invalid tenant IDs", func() {
			event := map[string]string{
				"analyzer": "relationship",
				"job_id":   "job-1",
				"stage":    "completed",
			}
			data, _ := json.Marshal(event)
			msg := &natsbus.Message{
				Subject: "crosscodex.pipeline.invalid!tenant.job-1.stage.completed",
				Data:    data,
			}

			err := graph.ExportHandleEvent(svc, context.Background(), msg)
			Expect(err).NotTo(HaveOccurred()) // nil return = don't redeliver
		})

		It("rejects events with malformed JSON", func() {
			msg := &natsbus.Message{
				Subject: "crosscodex.pipeline.test-tenant.job-1.stage.completed",
				Data:    []byte(`{bad json`),
			}

			err := graph.ExportHandleEvent(svc, context.Background(), msg)
			Expect(err).NotTo(HaveOccurred()) // nil return = don't redeliver
		})

		It("rejects events missing required fields", func() {
			event := map[string]string{
				"stage": "completed",
			}
			data, _ := json.Marshal(event)
			msg := &natsbus.Message{
				Subject: "crosscodex.pipeline.test-tenant.job-1.stage.completed",
				Data:    data,
			}

			err := graph.ExportHandleEvent(svc, context.Background(), msg)
			Expect(err).NotTo(HaveOccurred()) // nil return = don't redeliver
		})

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

			err := graph.ExportHandleEvent(svc, context.Background(), msg)
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

			err := graph.ExportHandleEvent(svc, context.Background(), msg)
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

			err := graph.ExportHandleEvent(svc, context.Background(), msg)
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

			err := graph.ExportHandleEvent(svc, context.Background(), msg)
			Expect(err).NotTo(HaveOccurred())
		})

		It("skips embed materialization gracefully", func() {
			resolver.data = []byte(`[]`)
			event := map[string]string{
				"analyzer": "embed",
				"job_id":   "job-1",
				"stage":    "completed",
			}
			eventData, _ := json.Marshal(event)
			msg := &natsbus.Message{
				Subject: "crosscodex.pipeline.test-tenant.job-1.stage.completed",
				Data:    eventData,
			}

			err := graph.ExportHandleEvent(svc, context.Background(), msg)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

var _ = Describe("Subscriber Lifecycle", func() {
	var (
		svc     *graph.Service
		mockBus *mockNATSClient
	)

	BeforeEach(func() {
		mockBus = &mockNATSClient{}
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
		mockBus.queueSubscribeFunc = func(_ context.Context, _, _ string, _ natsbus.MessageHandler) (natsbus.Subscription, error) {
			return &mockSubscription{drainFunc: func() error {
				return errors.New("drain failed")
			}}, nil
		}
		Expect(svc.Start(context.Background())).To(Succeed())
		err := svc.Stop(context.Background())
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("drain"))
	})
})
