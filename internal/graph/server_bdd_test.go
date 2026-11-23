package graph_test

import (
	"context"
	"crypto/tls"
	"net/http"
	"path/filepath"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/complytime-labs/crosscodex/internal/graph"
	"github.com/complytime-labs/crosscodex/internal/testcerts"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/graphdb/memdriver"
	"github.com/complytime-labs/crosscodex/pkg/tenant/connectutil"
	"github.com/complytime-labs/crosscodex/pkg/tlsconfig"
)

// listFindingsCleaning stands in for the cleaning service the composition
// root mounts; only ListFindings answers, so a success proves the route.
type listFindingsCleaning struct {
	crosscodexv1connect.UnimplementedEdgeCleaningServiceHandler
}

func (listFindingsCleaning) ListFindings(context.Context, *connect.Request[pb.ListFindingsRequest]) (*connect.Response[pb.ListFindingsResponse], error) {
	return connect.NewResponse(&pb.ListFindingsResponse{}), nil
}

// reviewQueueFeedback stands in for the feedback service the composition
// root mounts; only GetReviewQueue answers, so a success proves the route.
type reviewQueueFeedback struct {
	crosscodexv1connect.UnimplementedFeedbackServiceHandler
}

func (reviewQueueFeedback) GetReviewQueue(context.Context, *connect.Request[pb.GetReviewQueueRequest]) (*connect.Response[pb.GetReviewQueueResponse], error) {
	return connect.NewResponse(&pb.GetReviewQueueResponse{}), nil
}

const readOnlyRefusal = "graph writes go through materialization; this listener serves the gateway's read RPCs only"

var _ = Describe("graph.Server", func() {
	const tenantID = "acme-corp"

	var (
		ctx     context.Context
		g       graphdb.GraphDB
		certDir string
		edgeID  string
		srv     *graph.Server
	)

	listed := []authn.X509Match{{CN: "client", Organization: "CrossCodex Test"}}

	BeforeEach(func() {
		ctx = context.Background()
		mem := memdriver.New()
		Expect(mem.CreateGraph(ctx, tenantID)).To(Succeed())
		now := time.Now().UTC()
		for _, id := range []string{"ctl-a", "ctl-b"} {
			Expect(mem.CreateNode(ctx, tenantID, graphdb.Node{ID: id, Label: "Control", ValidFrom: now})).To(Succeed())
		}
		edgeID = "edge-a-b"
		Expect(mem.CreateEdge(ctx, tenantID, "ctl-a", "ctl-b",
			graphdb.Edge{ID: edgeID, Label: "SEMANTIC_MATCH", ValidFrom: now})).To(Succeed())
		g = mem

		certDir = GinkgoT().TempDir()
		pki, err := testcerts.Generate()
		Expect(err).NotTo(HaveOccurred())
		Expect(pki.WriteToDir(certDir)).To(Succeed())
	})

	serve := func(identities []authn.X509Match) {
		var err error
		srv, err = graph.NewServer(ctx, graph.ServerConfig{
			Addr: "127.0.0.1:0",
			TLS: config.TLSConfig{
				Mode: "mutual",
				CA:   filepath.Join(certDir, "ca.pem"),
				Cert: filepath.Join(certDir, "server.pem"),
				Key:  filepath.Join(certDir, "server-key.pem"),
			},
			Service:           graph.New(g, nil, nil),
			Cleaning:          listFindingsCleaning{},
			Feedback:          reviewQueueFeedback{},
			ServiceIdentities: identities,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(srv.Start()).To(Succeed())
		DeferCleanup(func() { _ = srv.Shutdown(context.Background()) })
	}

	// mtlsClient presents the generated client certificate
	// (CN=client, O=CrossCodex Test).
	mtlsClient := func() *http.Client {
		clientTLS, err := tlsconfig.BuildTLSConfig(ctx, config.TLSConfig{
			Mode: "mutual",
			CA:   filepath.Join(certDir, "ca.pem"),
			Cert: filepath.Join(certDir, "client.pem"),
			Key:  filepath.Join(certDir, "client-key.pem"),
		}, "")
		Expect(err).NotTo(HaveOccurred())
		return &http.Client{Transport: &http.Transport{TLSClientConfig: clientTLS, ForceAttemptHTTP2: true}}
	}

	baseURL := func() string { return "https://" + srv.Addr() }

	// asTenant sets the headers the gateway sends: the tenant, and an
	// asserted end user with roles when roles is non-empty.
	asTenant := func(h http.Header, roles string) {
		h.Set(connectutil.MetadataKeyTenantID, tenantID)
		if roles != "" {
			h.Set(connectutil.MetadataKeyUserID, "alice@acme.test")
			h.Set(connectutil.MetadataKeyUserRoles, roles)
		}
	}

	tc := &pb.TenantContext{TenantId: tenantID}

	// graphState captures everything a refused write could have changed:
	// both seeded nodes, the node a CreateNode would add, and every
	// SEMANTIC_MATCH edge with its validity interval.
	type graphState struct {
		nodeA, nodeB *graphdb.Node
		newNodeErr   error
		edges        []graphdb.Relationship
	}
	snapshot := func() graphState {
		var s graphState
		var err error
		s.nodeA, err = g.GetNode(ctx, tenantID, "ctl-a")
		Expect(err).NotTo(HaveOccurred())
		s.nodeB, err = g.GetNode(ctx, tenantID, "ctl-b")
		Expect(err).NotTo(HaveOccurred())
		_, s.newNodeErr = g.GetNode(ctx, tenantID, "ctl-new")
		s.edges, err = g.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{EdgeLabel: "SEMANTIC_MATCH"})
		Expect(err).NotTo(HaveOccurred())
		Expect(s.edges).To(HaveLen(1), "the seeded edge must be visible, or the unchanged check is vacuous")
		return s
	}

	Context("with a listed service identity", func() {
		var client crosscodexv1connect.GraphServiceClient

		BeforeEach(func() {
			serve(listed)
			client = crosscodexv1connect.NewGraphServiceClient(mtlsClient(), baseURL())
		})

		DescribeTable("refuses an RPC outside the gateway's reads with fixed copy and leaves the graph unchanged",
			func(call func(crosscodexv1connect.GraphServiceClient) error) {
				before := snapshot()
				Expect(before.newNodeErr).To(MatchError(graphdb.ErrNodeNotFound))

				err := call(client)

				Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnimplemented))
				Expect(err).To(MatchError(ContainSubstring(readOnlyRefusal)))
				Expect(snapshot()).To(Equal(before))
			},
			Entry("CreateNode", func(c crosscodexv1connect.GraphServiceClient) error {
				req := connect.NewRequest(&pb.CreateNodeRequest{TenantContext: tc, Label: "Control",
					Properties: map[string]string{"id": "ctl-new"}})
				asTenant(req.Header(), authn.RoleAdmin)
				_, err := c.CreateNode(ctx, req)
				return err
			}),
			Entry("CreateEdge", func(c crosscodexv1connect.GraphServiceClient) error {
				req := connect.NewRequest(&pb.CreateEdgeRequest{TenantContext: tc,
					SourceNodeId: "ctl-b", TargetNodeId: "ctl-a", Label: "SEMANTIC_MATCH"})
				asTenant(req.Header(), authn.RoleAdmin)
				_, err := c.CreateEdge(ctx, req)
				return err
			}),
			Entry("BulkCreateEdges", func(c crosscodexv1connect.GraphServiceClient) error {
				req := connect.NewRequest(&pb.BulkCreateEdgesRequest{TenantContext: tc,
					Edges: []*pb.CreateEdgeRequest{{TenantContext: tc,
						SourceNodeId: "ctl-b", TargetNodeId: "ctl-a", Label: "SEMANTIC_MATCH"}}})
				asTenant(req.Header(), authn.RoleAdmin)
				_, err := c.BulkCreateEdges(ctx, req)
				return err
			}),
			Entry("SupersedeFact", func(c crosscodexv1connect.GraphServiceClient) error {
				req := connect.NewRequest(&pb.SupersedeFactRequest{TenantContext: tc,
					Target: &pb.SupersedeFactRequest_EdgeId{EdgeId: edgeID}})
				asTenant(req.Header(), authn.RoleAdmin)
				_, err := c.SupersedeFact(ctx, req)
				return err
			}),
			Entry("GetNode", func(c crosscodexv1connect.GraphServiceClient) error {
				req := connect.NewRequest(&pb.GetNodeRequest{TenantContext: tc, NodeId: "ctl-a"})
				asTenant(req.Header(), authn.RoleAdmin)
				_, err := c.GetNode(ctx, req)
				return err
			}),
			Entry("GetEdge", func(c crosscodexv1connect.GraphServiceClient) error {
				req := connect.NewRequest(&pb.GetEdgeRequest{TenantContext: tc, EdgeId: edgeID})
				asTenant(req.Header(), authn.RoleAdmin)
				_, err := c.GetEdge(ctx, req)
				return err
			}),
			Entry("TemporalQuery", func(c crosscodexv1connect.GraphServiceClient) error {
				req := connect.NewRequest(&pb.TemporalQueryRequest{TenantContext: tc, Cypher: "MATCH (n) RETURN n"})
				asTenant(req.Header(), authn.RoleAdmin)
				_, err := c.TemporalQuery(ctx, req)
				return err
			}),
		)

		It("forwards Traverse to the service and returns the seeded nodes", func() {
			req := connect.NewRequest(&pb.TraverseRequest{TenantContext: tc, StartNodeId: "ctl-a", MaxDepth: 1})
			asTenant(req.Header(), "")
			resp, err := client.Traverse(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			ids := make([]string, 0, len(resp.Msg.GetNodes()))
			for _, n := range resp.Msg.GetNodes() {
				ids = append(ids, n.GetNodeId())
			}
			Expect(ids).To(ContainElements("ctl-a", "ctl-b"))
		})

		It("forwards Query to the service, which applies its admin check to the asserted user", func() {
			req := connect.NewRequest(&pb.QueryRequest{TenantContext: tc, Cypher: "MATCH (n) RETURN n"})
			asTenant(req.Header(), authn.RoleReviewer)
			_, err := client.Query(ctx, req)
			Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied))
			Expect(err).To(MatchError(ContainSubstring("admin access required")))
		})

		It("forwards SimilaritySearch to the service", func() {
			req := connect.NewRequest(&pb.SimilaritySearchRequest{TenantContext: tc})
			asTenant(req.Header(), "")
			_, err := client.SimilaritySearch(ctx, req)
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
			Expect(err).To(MatchError(ContainSubstring("vector search not configured")),
				"the service, built without a vector store, answered; the read-only refusal would be Unimplemented")
		})

		It("routes CrosswalkService.GetCrosswalk to the service", func() {
			cw := crosscodexv1connect.NewCrosswalkServiceClient(mtlsClient(), baseURL())
			req := connect.NewRequest(&pb.GetCrosswalkRequest{TenantContext: tc})
			asTenant(req.Header(), "")
			_, err := cw.GetCrosswalk(ctx, req)
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(err).To(MatchError(ContainSubstring("no crosswalk mode set")))
		})

		It("routes EdgeCleaningService to the configured cleaning handler", func() {
			cl := crosscodexv1connect.NewEdgeCleaningServiceClient(mtlsClient(), baseURL())
			req := connect.NewRequest(&pb.ListFindingsRequest{TenantContext: tc})
			asTenant(req.Header(), "")
			_, err := cl.ListFindings(ctx, req)
			Expect(err).NotTo(HaveOccurred())
		})

		It("routes FeedbackService to the configured feedback handler", func() {
			fb := crosscodexv1connect.NewFeedbackServiceClient(mtlsClient(), baseURL())
			req := connect.NewRequest(&pb.GetReviewQueueRequest{TenantContext: tc})
			asTenant(req.Header(), "")
			_, err := fb.GetReviewQueue(ctx, req)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	It("denies a valid client certificate that is not a service identity, leaving the graph unchanged", func() {
		serve([]authn.X509Match{{CN: "someone-else"}})
		before := snapshot()
		client := crosscodexv1connect.NewGraphServiceClient(mtlsClient(), baseURL())
		req := connect.NewRequest(&pb.TraverseRequest{TenantContext: tc, StartNodeId: "ctl-a", MaxDepth: 1})
		asTenant(req.Header(), "")
		_, err := client.Traverse(ctx, req)
		Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied))
		Expect(err).To(MatchError(ContainSubstring("tls.service_identities")))
		Expect(snapshot()).To(Equal(before))
	})

	It("denies FeedbackService to a valid client certificate that is not a service identity", func() {
		serve([]authn.X509Match{{CN: "someone-else"}})
		fb := crosscodexv1connect.NewFeedbackServiceClient(mtlsClient(), baseURL())
		req := connect.NewRequest(&pb.GetReviewQueueRequest{TenantContext: tc})
		asTenant(req.Header(), "")
		_, err := fb.GetReviewQueue(ctx, req)
		Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied))
		Expect(err).To(MatchError(ContainSubstring("tls.service_identities")))
	})

	It("fails the TLS handshake for a client with no certificate", func() {
		serve(listed)
		noCert, err := tlsconfig.BuildTLSConfig(ctx, config.TLSConfig{
			Mode: "server-only",
			CA:   filepath.Join(certDir, "ca.pem"),
		}, "")
		Expect(err).NotTo(HaveOccurred())
		conn, err := tls.Dial("tcp", srv.Addr(), noCert)
		if err == nil {
			// TLS 1.3 reports the server's rejection of a missing client
			// certificate on the first read, not in the handshake.
			defer func() { _ = conn.Close() }()
			_, err = conn.Read(make([]byte, 1))
		}
		Expect(err).To(MatchError(ContainSubstring("certificate required")))
	})
})
