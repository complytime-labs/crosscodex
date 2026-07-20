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
			relationshipData := []map[string]any{
				{
					"source_id":         "ctrl-1",
					"target_id":         "ctrl-2",
					"relationship_type": "implements",
					"confidence":        0.9,
					"properties":        map[string]string{"method": "llm"},
				},
			}
			data, _ := json.Marshal(relationshipData)
			resolver.data = data

			var createdEdges []graphdb.Edge
			mockGraph.createEdgeFunc = func(_ context.Context, _, sourceID, targetID string, edge graphdb.Edge) error {
				createdEdges = append(createdEdges, edge)
				Expect(sourceID).To(Equal("ctrl-1"))
				Expect(targetID).To(Equal("ctrl-2"))
				return nil
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
			Expect(err).NotTo(HaveOccurred())
			Expect(createdEdges).To(HaveLen(1))
			Expect(createdEdges[0].Label).To(Equal("SEMANTIC_MATCH"))
			Expect(createdEdges[0].Properties["relationship_type"]).To(Equal("implements"))
			Expect(createdEdges[0].Confidence).To(BeNumerically("~", 0.9, 0.01))
		})

		It("treats ErrNodeExists as success for idempotency", func() {
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
				return graphdb.ErrNodeExists
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
			Expect(err).NotTo(HaveOccurred())
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
			requiresData := []map[string]any{
				{
					"source_id":   "ctrl-1",
					"target_id":   "ctrl-2",
					"confidence":  0.95,
					"unanimous":   true,
					"valid_votes": 3,
					"total_votes": 3,
					"models":      []string{"claude-3-5-sonnet-20241022", "gpt-4o"},
				},
			}
			data, _ := json.Marshal(requiresData)
			resolver.data = data

			var createdRequires []graphdb.RequiresEdge
			mockGraph.createRequiresEdgeFunc = func(_ context.Context, _ string, reqEdge graphdb.RequiresEdge) error {
				createdRequires = append(createdRequires, reqEdge)
				return nil
			}

			event := map[string]string{
				"analyzer": "requires",
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
			Expect(createdRequires).To(HaveLen(1))
			Expect(createdRequires[0].SourceID).To(Equal("ctrl-1"))
			Expect(createdRequires[0].TargetID).To(Equal("ctrl-2"))
			Expect(createdRequires[0].Unanimous).To(BeTrue())
		})

		It("materializes artifact nodes and edges", func() {
			artifactData := []map[string]any{
				{
					"control_id": "ctrl-1",
					"artifacts": []map[string]any{
						{
							"name":       "Security Log",
							"type":       "log",
							"frequency":  "daily",
							"owner_role": "security-team",
							"confidence": 0.85,
						},
					},
				},
			}
			data, _ := json.Marshal(artifactData)
			resolver.data = data

			var createdNodes []graphdb.Node
			var createdEdges []graphdb.Edge
			type capturedEdge struct {
				sourceID string
				targetID string
				edge     graphdb.Edge
			}
			var capturedEdgeDetails []capturedEdge
			mockGraph.createNodeFunc = func(_ context.Context, _ string, node graphdb.Node) error {
				createdNodes = append(createdNodes, node)
				return nil
			}
			mockGraph.createEdgeFunc = func(_ context.Context, _, sourceID, targetID string, edge graphdb.Edge) error {
				capturedEdgeDetails = append(capturedEdgeDetails, capturedEdge{sourceID: sourceID, targetID: targetID, edge: edge})
				createdEdges = append(createdEdges, edge)
				return nil
			}

			event := map[string]string{
				"analyzer": "artifacts",
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
			// Artifact + ArtifactType nodes
			Expect(createdNodes).To(HaveLen(2))
			// DEMANDS + IS_TYPE edges
			Expect(createdEdges).To(HaveLen(2))

			// Verify artifact node
			artifactNode := createdNodes[0]
			Expect(artifactNode.Label).To(Equal("Artifact"))
			Expect(artifactNode.Properties["name"]).To(Equal("Security Log"))

			// Verify type node
			typeNode := createdNodes[1]
			Expect(typeNode.Label).To(Equal("ArtifactType"))
			Expect(typeNode.Properties["name"]).To(Equal("log"))

			// Verify edges
			Expect(createdEdges[0].Label).To(Equal("DEMANDS"))
			Expect(createdEdges[1].Label).To(Equal("IS_TYPE"))

			// Verify edge endpoints
			Expect(capturedEdgeDetails[0].sourceID).To(Equal("ctrl-1"))
			Expect(capturedEdgeDetails[1].sourceID).To(ContainSubstring("ctrl-1__art_0"))
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
