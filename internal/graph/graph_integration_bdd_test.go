//go:build integration

package graph_test

// Suite bootstrap lives in resolver_bdd_test.go — do NOT add RunSpecs here.

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/graph"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
)

var _ = Describe("Graph Service Integration", Ordered, func() {
	const (
		tenantID    = "graph-int-test"
		otherTenant = "graph-int-other"
	)

	var (
		svc     *graph.Service
		graphDB graphdb.GraphDB
		ctx     context.Context
		cleanup func()
	)

	BeforeAll(func() {
		db, dbCleanup := testspecs.SetupTestDatabase()
		cleanup = dbCleanup

		var err error
		graphDB, err = graphdb.New(db)
		Expect(err).NotTo(HaveOccurred(), "failed to create GraphDB")

		// Create tenant graphs via superuser (no RLS).
		bgCtx := context.Background()
		Expect(graphDB.CreateGraph(bgCtx, tenantID)).To(Succeed(), "failed to create tenant graph")
		Expect(graphDB.CreateGraph(bgCtx, otherTenant)).To(Succeed(), "failed to create other tenant graph")

		// Create service (no vectorDB or bus needed for basic node/edge/traverse tests).
		svc = graph.New(graphDB, nil, nil)

		// Create tenant context for main test tenant.
		ctx = testspecs.SetupTenantContext(tenantID)
	})

	AfterAll(func() {
		if cleanup != nil {
			cleanup()
		}
	})

	Describe("CreateNode and GetNode", func() {
		It("creates and retrieves a node", func() {
			createResp, err := svc.CreateNode(ctx, connect.NewRequest(&pb.CreateNodeRequest{
				TenantContext: &pb.TenantContext{TenantId: tenantID},
				Label:         "Control",
				Properties:    map[string]string{"text": "Access Control", "source": "NIST"},
				Temporal: &pb.TemporalAttributes{
					ValidFrom: timestamppb.Now(),
				},
			}))
			Expect(err).NotTo(HaveOccurred(), "CreateNode failed")
			Expect(createResp.Msg.GetNodeId()).NotTo(BeEmpty(), "returned node_id is empty")

			getResp, err := svc.GetNode(ctx, connect.NewRequest(&pb.GetNodeRequest{
				TenantContext: &pb.TenantContext{TenantId: tenantID},
				NodeId:        createResp.Msg.GetNodeId(),
			}))
			Expect(err).NotTo(HaveOccurred(), "GetNode failed")
			Expect(getResp.Msg.GetNode()).NotTo(BeNil())
			Expect(getResp.Msg.GetNode().GetLabel()).To(Equal("Control"))
			Expect(getResp.Msg.GetNode().GetProperties()).To(HaveKeyWithValue("text", "Access Control"))
		})
	})

	Describe("CreateEdge and Traverse", func() {
		It("traverses graph relationships via Traverse", func() {
			// Create two nodes.
			n1, err := svc.CreateNode(ctx, connect.NewRequest(&pb.CreateNodeRequest{
				TenantContext: &pb.TenantContext{TenantId: tenantID},
				Label:         "Control",
				Properties:    map[string]string{"text": "Source Control"},
				Temporal:      &pb.TemporalAttributes{ValidFrom: timestamppb.Now()},
			}))
			Expect(err).NotTo(HaveOccurred())

			n2, err := svc.CreateNode(ctx, connect.NewRequest(&pb.CreateNodeRequest{
				TenantContext: &pb.TenantContext{TenantId: tenantID},
				Label:         "Control",
				Properties:    map[string]string{"text": "Target Control"},
				Temporal:      &pb.TemporalAttributes{ValidFrom: timestamppb.Now()},
			}))
			Expect(err).NotTo(HaveOccurred())

			// Create an edge.
			edgeResp, err := svc.CreateEdge(ctx, connect.NewRequest(&pb.CreateEdgeRequest{
				TenantContext: &pb.TenantContext{TenantId: tenantID},
				SourceNodeId:  n1.Msg.GetNodeId(),
				TargetNodeId:  n2.Msg.GetNodeId(),
				Label:         "SEMANTIC_MATCH",
				Properties:    map[string]string{"confidence": "0.95"},
				Temporal:      &pb.TemporalAttributes{ValidFrom: timestamppb.Now()},
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(edgeResp.Msg.GetEdgeId()).NotTo(BeEmpty())

			// Traverse outbound from n1.
			traverseResp, err := svc.Traverse(ctx, connect.NewRequest(&pb.TraverseRequest{
				TenantContext: &pb.TenantContext{TenantId: tenantID},
				StartNodeId:   n1.Msg.GetNodeId(),
				Direction:     pb.TraversalDirection_TRAVERSAL_DIRECTION_OUTBOUND,
				MaxDepth:      1,
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(traverseResp.Msg.GetNodes()).NotTo(BeEmpty(), "Traverse returned no nodes")

			// Verify the target node appears in results.
			found := false
			for _, node := range traverseResp.Msg.GetNodes() {
				if node.GetNodeId() == n2.Msg.GetNodeId() {
					found = true
					break
				}
			}
			Expect(found).To(BeTrue(), "target node not found in Traverse results")
		})
	})

	Describe("BulkCreateEdges", func() {
		It("creates multiple edges in one call", func() {
			// Create three nodes.
			n1, err := svc.CreateNode(ctx, connect.NewRequest(&pb.CreateNodeRequest{
				TenantContext: &pb.TenantContext{TenantId: tenantID},
				Label:         "Control",
				Properties:    map[string]string{"text": "Bulk Source"},
				Temporal:      &pb.TemporalAttributes{ValidFrom: timestamppb.Now()},
			}))
			Expect(err).NotTo(HaveOccurred())

			n2, err := svc.CreateNode(ctx, connect.NewRequest(&pb.CreateNodeRequest{
				TenantContext: &pb.TenantContext{TenantId: tenantID},
				Label:         "Control",
				Properties:    map[string]string{"text": "Bulk Target 1"},
				Temporal:      &pb.TemporalAttributes{ValidFrom: timestamppb.Now()},
			}))
			Expect(err).NotTo(HaveOccurred())

			n3, err := svc.CreateNode(ctx, connect.NewRequest(&pb.CreateNodeRequest{
				TenantContext: &pb.TenantContext{TenantId: tenantID},
				Label:         "Control",
				Properties:    map[string]string{"text": "Bulk Target 2"},
				Temporal:      &pb.TemporalAttributes{ValidFrom: timestamppb.Now()},
			}))
			Expect(err).NotTo(HaveOccurred())

			// BulkCreateEdges from n1 to n2 and n3.
			bulkResp, err := svc.BulkCreateEdges(ctx, connect.NewRequest(&pb.BulkCreateEdgesRequest{
				TenantContext: &pb.TenantContext{TenantId: tenantID},
				Edges: []*pb.CreateEdgeRequest{
					{
						TenantContext: &pb.TenantContext{TenantId: tenantID},
						SourceNodeId:  n1.Msg.GetNodeId(),
						TargetNodeId:  n2.Msg.GetNodeId(),
						Label:         "SEMANTIC_MATCH",
						Properties:    map[string]string{"confidence": "0.90"},
						Temporal:      &pb.TemporalAttributes{ValidFrom: timestamppb.Now()},
					},
					{
						TenantContext: &pb.TenantContext{TenantId: tenantID},
						SourceNodeId:  n1.Msg.GetNodeId(),
						TargetNodeId:  n3.Msg.GetNodeId(),
						Label:         "SEMANTIC_MATCH",
						Properties:    map[string]string{"confidence": "0.85"},
						Temporal:      &pb.TemporalAttributes{ValidFrom: timestamppb.Now()},
					},
				},
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(bulkResp.Msg.GetCreatedCount()).To(Equal(int32(2)))
			Expect(bulkResp.Msg.GetEdgeIds()).To(HaveLen(2))
		})
	})

	Describe("SupersedeFact", func() {
		It("supersedes a node and verifies temporal state", func() {
			createResp, err := svc.CreateNode(ctx, connect.NewRequest(&pb.CreateNodeRequest{
				TenantContext: &pb.TenantContext{TenantId: tenantID},
				Label:         "Control",
				Properties:    map[string]string{"text": "Temporal Test Node"},
				Temporal:      &pb.TemporalAttributes{ValidFrom: timestamppb.Now()},
			}))
			Expect(err).NotTo(HaveOccurred())

			supersedeResp, err := svc.SupersedeFact(ctx, connect.NewRequest(&pb.SupersedeFactRequest{
				TenantContext:     &pb.TenantContext{TenantId: tenantID},
				Target:            &pb.SupersedeFactRequest_NodeId{NodeId: createResp.Msg.GetNodeId()},
				SupersededAt:      timestamppb.Now(),
				SupersededByJobId: "job-supersede-test",
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(supersedeResp.Msg.GetUpdated()).To(BeTrue(), "SupersedeFact did not mark node as updated")

			// Verify node is marked superseded (valid_to is set).
			getResp, err := svc.GetNode(ctx, connect.NewRequest(&pb.GetNodeRequest{
				TenantContext: &pb.TenantContext{TenantId: tenantID},
				NodeId:        createResp.Msg.GetNodeId(),
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(getResp.Msg.GetNode().GetTemporal().GetValidTo()).NotTo(BeNil(), "valid_to not set after SupersedeFact")
		})

		It("supersedes an edge and verifies temporal state", func() {
			// Create two nodes.
			n1, err := svc.CreateNode(ctx, connect.NewRequest(&pb.CreateNodeRequest{
				TenantContext: &pb.TenantContext{TenantId: tenantID},
				Label:         "Control",
				Properties:    map[string]string{"text": "Edge Supersede Source"},
				Temporal:      &pb.TemporalAttributes{ValidFrom: timestamppb.Now()},
			}))
			Expect(err).NotTo(HaveOccurred())

			n2, err := svc.CreateNode(ctx, connect.NewRequest(&pb.CreateNodeRequest{
				TenantContext: &pb.TenantContext{TenantId: tenantID},
				Label:         "Control",
				Properties:    map[string]string{"text": "Edge Supersede Target"},
				Temporal:      &pb.TemporalAttributes{ValidFrom: timestamppb.Now()},
			}))
			Expect(err).NotTo(HaveOccurred())

			// Create an edge.
			edgeResp, err := svc.CreateEdge(ctx, connect.NewRequest(&pb.CreateEdgeRequest{
				TenantContext: &pb.TenantContext{TenantId: tenantID},
				SourceNodeId:  n1.Msg.GetNodeId(),
				TargetNodeId:  n2.Msg.GetNodeId(),
				Label:         "SEMANTIC_MATCH",
				Temporal:      &pb.TemporalAttributes{ValidFrom: timestamppb.Now()},
			}))
			Expect(err).NotTo(HaveOccurred())

			// Supersede the edge.
			supersedeResp, err := svc.SupersedeFact(ctx, connect.NewRequest(&pb.SupersedeFactRequest{
				TenantContext:     &pb.TenantContext{TenantId: tenantID},
				Target:            &pb.SupersedeFactRequest_EdgeId{EdgeId: edgeResp.Msg.GetEdgeId()},
				SupersededAt:      timestamppb.Now(),
				SupersededByJobId: "job-edge-supersede",
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(supersedeResp.Msg.GetUpdated()).To(BeTrue())
		})
	})

	Describe("Tenant Isolation", func() {
		It("enforces tenant isolation on GetNode", func() {
			// Create a node in tenantID.
			createResp, err := svc.CreateNode(ctx, connect.NewRequest(&pb.CreateNodeRequest{
				TenantContext: &pb.TenantContext{TenantId: tenantID},
				Label:         "Control",
				Properties:    map[string]string{"text": "Isolated Node"},
				Temporal:      &pb.TemporalAttributes{ValidFrom: timestamppb.Now()},
			}))
			Expect(err).NotTo(HaveOccurred())

			// Attempt to access from another tenant context.
			otherCtx := testspecs.SetupTenantContext(otherTenant)
			_, err = svc.GetNode(otherCtx, connect.NewRequest(&pb.GetNodeRequest{
				TenantContext: &pb.TenantContext{TenantId: tenantID},
				NodeId:        createResp.Msg.GetNodeId(),
			}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied), "expected PermissionDenied for cross-tenant access")
		})

		It("enforces tenant isolation on Traverse", func() {
			// Create nodes in tenantID.
			n1, err := svc.CreateNode(ctx, connect.NewRequest(&pb.CreateNodeRequest{
				TenantContext: &pb.TenantContext{TenantId: tenantID},
				Label:         "Control",
				Properties:    map[string]string{"text": "Isolated Traverse Source"},
				Temporal:      &pb.TemporalAttributes{ValidFrom: timestamppb.Now()},
			}))
			Expect(err).NotTo(HaveOccurred())

			// Attempt to traverse from another tenant context.
			otherCtx := testspecs.SetupTenantContext(otherTenant)
			_, err = svc.Traverse(otherCtx, connect.NewRequest(&pb.TraverseRequest{
				TenantContext: &pb.TenantContext{TenantId: tenantID},
				StartNodeId:   n1.Msg.GetNodeId(),
				Direction:     pb.TraversalDirection_TRAVERSAL_DIRECTION_OUTBOUND,
				MaxDepth:      1,
			}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied), "expected PermissionDenied for cross-tenant Traverse")
		})
	})
})
