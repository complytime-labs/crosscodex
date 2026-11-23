//go:build !integration

package gateway_test

import (
	"context"
	"net/http"
	"path/filepath"
	"sync"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	crosscodexv1connect "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	intcleaning "github.com/complytime-labs/crosscodex/internal/cleaning"
	"github.com/complytime-labs/crosscodex/internal/gateway"
	"github.com/complytime-labs/crosscodex/internal/graph"
	"github.com/complytime-labs/crosscodex/internal/testcerts"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/cleaning"
	"github.com/complytime-labs/crosscodex/pkg/cleaning/cleaningtest"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/graphdb/memdriver"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
	"github.com/complytime-labs/crosscodex/pkg/tlsconfig"
)

// queryingGraph answers ExecuteQuery, which memdriver does not support, with
// one scalar row and records the tenant each query ran in; every other method
// is memdriver's.
type queryingGraph struct {
	graphdb.GraphDB
	mu      sync.Mutex
	tenants []string
}

func (q *queryingGraph) ExecuteQuery(_ context.Context, tenantID, _ string, _ map[string]string) ([]graphdb.QueryRow, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.tenants = append(q.tenants, tenantID)
	return []graphdb.QueryRow{{Values: []graphdb.QueryValue{{Type: graphdb.QueryValueScalar, ScalarVal: "pong"}}}}, nil
}

func (q *queryingGraph) queriedTenants() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]string(nil), q.tenants...)
}

// feedbackCall is what recordingFeedback saw of one forwarded RPC.
type feedbackCall struct {
	method    string
	subject   string
	roles     []string
	tenant    string
	msgTenant string
	mappingID string
	probe     string
}

// recordingFeedback stands in for the graph role's feedback service: it
// records the identity, tenant and headers each SubmitVote,
// RecordDetermination and GetReviewQueue reached it with, and answers with
// fixed IDs.
type recordingFeedback struct {
	crosscodexv1connect.UnimplementedFeedbackServiceHandler
	mu    sync.Mutex
	calls []feedbackCall
}

func (r *recordingFeedback) record(ctx context.Context, method string, header http.Header, tc *pb.TenantContext, mappingID string) {
	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		tenantID = "no tenant: " + err.Error()
	}
	call := feedbackCall{
		method:    method,
		tenant:    tenantID,
		msgTenant: tc.GetTenantId(),
		mappingID: mappingID,
		probe:     header.Get(callerProbeHeader),
	}
	if id := authn.IdentityFromContext(ctx); id != nil {
		call.subject, call.roles = id.Subject, id.Roles
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, call)
}

func (r *recordingFeedback) SubmitVote(ctx context.Context, req *connect.Request[pb.SubmitVoteRequest]) (*connect.Response[pb.SubmitVoteResponse], error) {
	r.record(ctx, "SubmitVote", req.Header(), req.Msg.GetTenantContext(), req.Msg.GetMappingId())
	return connect.NewResponse(&pb.SubmitVoteResponse{VoteId: "vote-1"}), nil
}

func (r *recordingFeedback) RecordDetermination(ctx context.Context, req *connect.Request[pb.RecordDeterminationRequest]) (*connect.Response[pb.RecordDeterminationResponse], error) {
	r.record(ctx, "RecordDetermination", req.Header(), req.Msg.GetTenantContext(), req.Msg.GetMappingId())
	return connect.NewResponse(&pb.RecordDeterminationResponse{DeterminationId: "det-1"}), nil
}

func (r *recordingFeedback) GetReviewQueue(ctx context.Context, req *connect.Request[pb.GetReviewQueueRequest]) (*connect.Response[pb.GetReviewQueueResponse], error) {
	r.record(ctx, "GetReviewQueue", req.Header(), req.Msg.GetTenantContext(), "")
	return connect.NewResponse(&pb.GetReviewQueueResponse{Items: []*pb.ReviewItem{{}}}), nil
}

func (r *recordingFeedback) recorded() []feedbackCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]feedbackCall(nil), r.calls...)
}

// callerProbeHeader is a header the gateway caller sets; reaching the graph
// role with it would mean the gateway forwarded the caller's headers.
const callerProbeHeader = "X-Caller-Probe"

var _ = Describe("NewConnectGraphBackend", func() {
	var certDir string

	BeforeEach(func() {
		certDir = GinkgoT().TempDir()
		pki, err := testcerts.Generate()
		Expect(err).NotTo(HaveOccurred())
		Expect(pki.WriteToDir(certDir)).To(Succeed())
	})

	DescribeTable("fails closed with a message naming the setting to fix",
		func(endpoint string, tlsCfg func() config.TLSConfig, wantKey string) {
			backend, err := gateway.NewConnectGraphBackend(context.Background(), endpoint, tlsCfg())
			Expect(err).To(MatchError(ContainSubstring(wantKey)))
			Expect(backend).To(BeNil())
		},
		Entry("an empty endpoint names graph.endpoint", "",
			func() config.TLSConfig { return config.TLSConfig{} }, "graph.endpoint"),
		Entry("TLS without a client certificate names the graph-client target", "graph.internal:8443",
			func() config.TLSConfig {
				return config.TLSConfig{Mode: "server-only", CA: filepath.Join(certDir, "ca.pem")}
			}, "tls.targets.graph-client.mode"),
		Entry("TLS without a CA names the graph-client target", "graph.internal:8443",
			func() config.TLSConfig {
				return config.TLSConfig{Mode: "mutual",
					Cert: filepath.Join(certDir, "client.pem"), Key: filepath.Join(certDir, "client-key.pem")}
			}, "graph-client"),
	)

	Describe("against a graph role", func() {
		const tenantID = "acme"

		var (
			ctx      context.Context
			g        *queryingGraph
			store    *cleaningtest.Fake
			finding  cleaning.StoredFinding
			caller   *authn.Identity
			httpC    *http.Client
			gwAddr   string
			gwClient crosscodexv1connect.GatewayServiceClient
			gwSvc    *gateway.Service
			clClient crosscodexv1connect.EdgeCleaningServiceClient
			fb       *recordingFeedback

			decidedMu sync.Mutex
			decidedBy []string
		)

		tlsFor := func(cert, key string) config.TLSConfig {
			return config.TLSConfig{
				Mode: "mutual",
				CA:   filepath.Join(certDir, "ca.pem"),
				Cert: filepath.Join(certDir, cert),
				Key:  filepath.Join(certDir, key),
			}
		}

		BeforeEach(func() {
			ctx = context.Background()
			tenantCtx, err := tenant.WithTenant(ctx, tenantID)
			Expect(err).NotTo(HaveOccurred())
			mem := memdriver.New()
			Expect(mem.CreateGraph(tenantCtx, tenantID)).To(Succeed())
			g = &queryingGraph{GraphDB: mem}

			finding = cleaningtest.ContractFinding(1, cleaning.SignalEquivalenceFanout, cleaning.StatusPending, "cat-a", "cat-b")
			decidedBy = nil
			store = &cleaningtest.Fake{
				Seed:      map[string][]cleaning.StoredFinding{tenantID: {finding}},
				Reviewers: map[string]map[string]string{tenantID: {"alice": cleaningtest.ReviewerActive}},
				DecideFunc: func(_ context.Context, _ string, d cleaning.Decision, _ string, _ cleaning.StoredFinding) error {
					decidedMu.Lock()
					defer decidedMu.Unlock()
					decidedBy = append(decidedBy, d.DecidedBy)
					return nil
				},
			}
			kp, err := attestation.NewEphemeralKeyProvider()
			Expect(err).NotTo(HaveOccurred())
			signer, err := attestation.NewGenerator(kp)
			Expect(err).NotTo(HaveOccurred())
			fb = &recordingFeedback{}
			engine := cleaning.NewEngine(store, g, &config.CleaningConfig{MaxRetirePerRun: 1000}, nil, signer)

			graphSrv, err := graph.NewServer(ctx, graph.ServerConfig{
				Addr:              "127.0.0.1:0",
				TLS:               tlsFor("server.pem", "server-key.pem"),
				Service:           graph.New(g, nil, nil),
				Cleaning:          intcleaning.NewService(engine),
				Feedback:          fb,
				ServiceIdentities: []authn.X509Match{{CN: "client", Organization: "CrossCodex Test"}},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(graphSrv.Start()).To(Succeed())
			DeferCleanup(func() { _ = graphSrv.Shutdown(context.Background()) })

			backend, err := gateway.NewConnectGraphBackend(ctx, graphSrv.Addr(), tlsFor("client.pem", "client-key.pem"))
			Expect(err).NotTo(HaveOccurred())

			// The authenticator returns caller, so a spec sets the gateway
			// caller's subject and roles before it calls.
			caller = &authn.Identity{Subject: "alice", TenantID: tenantID, Method: authn.AuthMethodMTLS}
			reg, err := authn.NewRegistry(nil, []authn.Authenticator{fixedIdentity{id: caller}})
			Expect(err).NotTo(HaveOccurred())
			gwSvc = gateway.NewService(gateway.WithAuthn(reg), gateway.WithGraphBackend(backend),
				gateway.WithCrosswalkBackend(backend), gateway.WithFeedbackBackend(backend.Feedback()))
			gwSrv, err := gateway.NewServer(ctx, gateway.ServerConfig{
				Addr:         "127.0.0.1:0",
				TLS:          tlsFor("server.pem", "server-key.pem"),
				Service:      gwSvc,
				Cleaning:     backend.Cleaning(),
				TenantStatus: &fakeTenantStatus{active: true},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(gwSrv.Start()).To(Succeed())
			DeferCleanup(func() { _ = gwSrv.Shutdown(context.Background()) })

			clientTLS, err := tlsconfig.BuildTLSConfig(ctx, tlsFor("client.pem", "client-key.pem"), "")
			Expect(err).NotTo(HaveOccurred())
			httpC = &http.Client{Transport: &http.Transport{TLSClientConfig: clientTLS, ForceAttemptHTTP2: true}}
			gwAddr = "https://" + gwSrv.Addr()
			gwClient = crosscodexv1connect.NewGatewayServiceClient(httpC, gwAddr)
			clClient = crosscodexv1connect.NewEdgeCleaningServiceClient(httpC, gwAddr)
		})

		tc := &pb.TenantContext{TenantId: tenantID}

		It("answers QueryGraph through the graph role in the caller's tenant", func() {
			caller.Roles = []string{authn.RoleAdmin}
			resp, err := gwClient.QueryGraph(ctx, connect.NewRequest(&pb.QueryGraphRequest{Cypher: "RETURN 'pong'"}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetResponse().GetRows()).To(HaveLen(1))
			Expect(resp.Msg.GetResponse().GetRows()[0].GetValues()[0].GetScalar()).To(Equal("pong"))
			Expect(g.queriedTenants()).To(ConsistOf(tenantID))
		})

		It("returns the graph role's PermissionDenied for a reviewer's RunCleaning verbatim, and runs nothing", func() {
			caller.Roles = []string{authn.RoleReviewer}
			_, err := clClient.RunCleaning(ctx, connect.NewRequest(&pb.RunCleaningRequest{
				TenantContext: tc, Mode: pb.CleaningMode_CLEANING_MODE_APPLY, ExpectToken: "t",
			}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied))
			Expect(err).To(MatchError(ContainSubstring("running a cleaning pass needs the admin role")))
			Expect(store.Runs(tenantID)).To(BeEmpty())
			Expect(decidedBy).To(BeEmpty())
		})

		It("records a DecideFinding under the gateway caller's subject", func() {
			caller.Subject = "alice"
			caller.Roles = []string{authn.RoleReviewer}
			resp, err := clClient.DecideFinding(ctx, connect.NewRequest(&pb.DecideFindingRequest{
				TenantContext: tc, FindingId: finding.ID, Verdict: cleaning.VerdictDismiss,
				Rationale: "not a problem", ExpectStateToken: cleaning.StateToken(finding),
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetDecisionId()).NotTo(BeEmpty())
			decidedMu.Lock()
			defer decidedMu.Unlock()
			Expect(decidedBy).To(Equal([]string{"alice"}))
		})

		// The gateway forwards the request message, not the caller's HTTP
		// headers: a caller's Content-Encoding would otherwise describe a body
		// the gateway re-encodes, and the graph role would fail to read it.
		It("forwards a gzip-compressed caller's cleaning request", func() {
			caller.Roles = []string{authn.RoleReviewer}
			gzipClient := crosscodexv1connect.NewEdgeCleaningServiceClient(httpC, gwAddr, connect.WithSendGzip())
			resp, err := gzipClient.ListFindings(ctx, connect.NewRequest(&pb.ListFindingsRequest{TenantContext: tc}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetFindings()).To(HaveLen(1))
		})

		It("forwards RecordDetermination as the gateway caller in the caller's tenant, without the caller's headers", func() {
			caller.Subject = "alice"
			caller.Roles = []string{authn.RoleReviewer}
			req := connect.NewRequest(&pb.RecordDeterminationRequest{
				JobId: "job-1", MappingId: "map-1", DeterminationRationale: "checked",
			})
			req.Header().Set(callerProbeHeader, "from-caller")
			resp, err := gwClient.RecordDetermination(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetDeterminationId()).To(Equal("det-1"))
			Expect(fb.recorded()).To(Equal([]feedbackCall{{
				method: "RecordDetermination", subject: "alice", roles: []string{authn.RoleReviewer},
				tenant: tenantID, msgTenant: tenantID, mappingID: "map-1",
			}}))
		})

		It("forwards SubmitVote as the gateway caller in the caller's tenant, without the caller's headers", func() {
			caller.Roles = []string{authn.RoleReviewer}
			req := connect.NewRequest(&pb.SubmitVoteRequest{
				JobId: "job-1", MappingId: "map-2", VoteType: pb.VoteType_VOTE_TYPE_AGREE,
			})
			req.Header().Set(callerProbeHeader, "from-caller")
			resp, err := gwClient.SubmitVote(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetVoteId()).To(Equal("vote-1"))
			Expect(fb.recorded()).To(Equal([]feedbackCall{{
				method: "SubmitVote", subject: "alice", roles: []string{authn.RoleReviewer},
				tenant: tenantID, msgTenant: tenantID, mappingID: "map-2",
			}}))
		})

		It("forwards GetReviewQueue as the gateway caller in the caller's tenant, without the caller's headers", func() {
			caller.Roles = []string{authn.RoleReviewer}
			req := connect.NewRequest(&pb.GetReviewQueueRequest{JobId: "job-1"})
			req.Header().Set(callerProbeHeader, "from-caller")
			resp, err := gwClient.GetReviewQueue(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetItems()).To(HaveLen(1))
			Expect(fb.recorded()).To(Equal([]feedbackCall{{
				method: "GetReviewQueue", subject: "alice", roles: []string{authn.RoleReviewer},
				tenant: tenantID, msgTenant: tenantID,
			}}))
		})

		// Over the wire the auth interceptor refuses a foreign tenant_context
		// first, so the handler is called directly, with the identity and
		// tenant the interceptor sets for alice, to prove the handler itself
		// replaces it.
		It("replaces a foreign tenant_context with the caller's before forwarding", func() {
			identity := &authn.Identity{Subject: "alice", TenantID: tenantID, Roles: []string{authn.RoleReviewer}, Method: authn.AuthMethodMTLS}
			callCtx, err := tenant.WithTenant(gateway.ExportContextWithIdentity(ctx, identity), tenantID)
			Expect(err).NotTo(HaveOccurred())
			callCtx = tenant.WithUser(callCtx, identity.Subject)
			_, err = gwSvc.RecordDetermination(callCtx, connect.NewRequest(&pb.RecordDeterminationRequest{
				TenantContext: &pb.TenantContext{TenantId: "other-tenant"},
				JobId:         "job-1", MappingId: "map-1", DeterminationRationale: "checked",
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(fb.recorded()).To(Equal([]feedbackCall{{
				method: "RecordDetermination", subject: "alice", roles: []string{authn.RoleReviewer},
				tenant: tenantID, msgTenant: tenantID, mappingID: "map-1",
			}}))
		})
	})
})
