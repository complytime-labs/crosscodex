//go:build !integration

package main

import (
	"context"
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
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/natsbus/natsbustest"
	"github.com/complytime-labs/crosscodex/pkg/vectordb"
	"github.com/complytime-labs/crosscodex/pkg/vectordb/vectordbtest"
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

	It("caps SimilaritySearch limit at graph.max_similarity_limit", func() {
		ctx := testspecs.SetupTenantContext(tenantID)
		var searched int
		vectors := &vectordbtest.Fake{FindSimilarFunc: func(_ context.Context, _ string, _ vectordb.FindSimilarQuery) ([]vectordb.SimilarityResult, error) {
			searched++
			return nil, nil
		}}
		cfg := &config.Config{}
		cfg.Graph.MaxSimilarityCatalogs = 2
		cfg.Graph.MaxSimilarityLimit = 3
		rt := &runtime{}
		Expect(attachGraph(cfg, &sharedResources{graphDB_: memdriver.New(), vectorDB: vectors}, rt)).To(Succeed())

		_, err := rt.graphService.SimilaritySearch(ctx, connect.NewRequest(&pb.SimilaritySearchRequest{
			TenantContext: &pb.TenantContext{TenantId: tenantID},
			Query:         &pb.SimilaritySearchRequest_QueryEmbedding{QueryEmbedding: &pb.EmbeddingQuery{Embeddings: []float32{0.1, 0.2}}},
			Model:         "m",
			CatalogIds:    []string{"c1"},
			Limit:         4,
		}))
		Expect(err).To(HaveOccurred(), "limit 4 must exceed graph.max_similarity_limit=3; was the option dropped from attachGraph?")
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		Expect(err.Error()).To(ContainSubstring("graph.max_similarity_limit"))
		Expect(searched).To(BeZero())
	})

	It("caps SimilaritySearch catalogs at graph.max_similarity_catalogs", func() {
		ctx := testspecs.SetupTenantContext(tenantID)
		var searched []string
		vectors := &vectordbtest.Fake{FindSimilarFunc: func(_ context.Context, _ string, q vectordb.FindSimilarQuery) ([]vectordb.SimilarityResult, error) {
			searched = append(searched, q.CatalogID)
			return nil, nil
		}}
		cfg := &config.Config{}
		cfg.Graph.MaxSimilarityCatalogs = 2
		rt := &runtime{}
		Expect(attachGraph(cfg, &sharedResources{graphDB_: memdriver.New(), vectorDB: vectors}, rt)).To(Succeed())

		search := func(catalogs ...string) error {
			_, err := rt.graphService.SimilaritySearch(ctx, connect.NewRequest(&pb.SimilaritySearchRequest{
				TenantContext: &pb.TenantContext{TenantId: tenantID},
				Query:         &pb.SimilaritySearchRequest_QueryEmbedding{QueryEmbedding: &pb.EmbeddingQuery{Embeddings: []float32{0.1, 0.2}}},
				Model:         "m",
				CatalogIds:    catalogs,
			}))
			return err
		}

		err := search("c1", "c2", "c3")
		Expect(err).To(HaveOccurred(), "3 catalogs must exceed graph.max_similarity_catalogs=2; was the option dropped from attachGraph?")
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		Expect(err.Error()).To(ContainSubstring("graph.max_similarity_catalogs"))
		Expect(err.Error()).To(ContainSubstring("limit of 2"))
		Expect(searched).To(BeEmpty(), "a rejected request must search no catalog")

		Expect(search("c1", "c2")).To(Succeed(), "a request at the configured limit is accepted")
		Expect(searched).To(Equal([]string{"c1", "c2"}))
	})

	It("consumes MATERIALIZE durably, including catalog parsed events, with nats.materialize's delivery limits", func() {
		bus := &natsbustest.Fake{}
		rt := &runtime{}
		cfg := &config.Config{NATS: config.NATSConfig{Materialize: config.NATSMaterializeConfig{
			MaxDeliver: 7, AckWait: 3 * time.Minute, RedeliverDelay: 4 * time.Second,
		}}}
		Expect(attachGraph(cfg, &sharedResources{graphDB_: memdriver.New(), natsClient: bus}, rt)).To(Succeed())

		ctx := context.Background()
		Expect(rt.graphService.Start(ctx)).To(Succeed())
		DeferCleanup(func() { _ = rt.graphService.Stop(context.Background()) })
		var got []natsbus.ConsumerConfig
		for _, sub := range bus.Subscriptions() {
			got = append(got, sub.Config)
		}
		// Exact set: an unexpected subscription or publish must fail here.
		Expect(got).To(ConsistOf(
			natsbus.ConsumerConfig{
				Stream: natsbus.MaterializeStream, Durable: "graph-materializer",
				FilterSubjects: []string{"crosscodex.pipeline.*.*.stage.completed", "crosscodex.catalog.*.*.parsed"},
				AckWait:        3 * time.Minute, MaxDeliver: 7, RedeliverDelay: 4 * time.Second,
			},
			natsbus.ConsumerConfig{
				Stream: natsbus.MaterializeStream, Durable: "graph-barrier",
				FilterSubjects: []string{"crosscodex.materialize.*.*.barrier"},
				AckWait:        3 * time.Minute, MaxDeliver: -1, RedeliverDelay: 4 * time.Second,
			},
		), "attachGraph must give the graph service a ControlsReader and nats.materialize")
		Expect(bus.Published()).To(BeEmpty())
	})
})
