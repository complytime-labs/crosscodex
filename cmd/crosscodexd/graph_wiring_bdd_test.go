//go:build !integration

package main

import (
	"fmt"
	"time"

	connect "connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/graphdb/memdriver"
)

// attachGraph only constructs values, so it runs here against memdriver to
// prove graph.max_bulk_edges reaches the graph service it builds.
var _ = Describe("attachGraph config wiring", func() {
	const tenantID = "wiring-tenant"

	It("caps BulkCreateEdges at graph.max_bulk_edges", func() {
		ctx := testspecs.SetupTenantContext(tenantID)
		gdb := memdriver.New()
		Expect(gdb.CreateGraph(ctx, tenantID)).To(Succeed())
		for i := range 9 {
			Expect(gdb.CreateNode(ctx, tenantID, graphdb.Node{ID: fmt.Sprintf("n%d", i), Label: "Control", ValidFrom: time.Now()})).To(Succeed())
		}

		cfg := &config.Config{}
		cfg.Graph.MaxBulkEdges = 7
		rt := &runtime{}
		Expect(attachGraph(cfg, &sharedResources{graphDB_: gdb}, rt)).To(Succeed())

		edges := func(n int) []*pb.CreateEdgeRequest {
			out := make([]*pb.CreateEdgeRequest, n)
			for i := range out {
				out[i] = &pb.CreateEdgeRequest{
					SourceNodeId: fmt.Sprintf("n%d", i),
					TargetNodeId: fmt.Sprintf("n%d", i+1),
					Label:        "maps_to",
				}
			}
			return out
		}
		bulk := func(n int) (*connect.Response[pb.BulkCreateEdgesResponse], error) {
			return rt.graphService.BulkCreateEdges(ctx, connect.NewRequest(&pb.BulkCreateEdgesRequest{
				TenantContext: &pb.TenantContext{TenantId: tenantID},
				Edges:         edges(n),
			}))
		}

		_, err := bulk(8)
		Expect(err).To(HaveOccurred(), "8 edges must exceed graph.max_bulk_edges=7; was the option dropped from attachGraph?")
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		Expect(err.Error()).To(ContainSubstring("limit of 7"))
		Expect(err.Error()).To(ContainSubstring("split the edges"))
		rels, err := gdb.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{})
		Expect(err).NotTo(HaveOccurred())
		Expect(rels).To(BeEmpty(), "a rejected request must write no edges")

		resp, err := bulk(7)
		Expect(err).NotTo(HaveOccurred(), "a request at the configured limit is accepted")
		Expect(resp.Msg.GetCreatedCount()).To(Equal(int32(7)))
	})
})
