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
			clClient crosscodexv1connect.EdgeCleaningServiceClient

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
			engine := cleaning.NewEngine(store, g, &config.CleaningConfig{MaxRetirePerRun: 1000}, nil, signer)

			graphSrv, err := graph.NewServer(ctx, graph.ServerConfig{
				Addr:              "127.0.0.1:0",
				TLS:               tlsFor("server.pem", "server-key.pem"),
				Service:           graph.New(g, nil, nil),
				Cleaning:          intcleaning.NewService(engine),
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
			gwSrv, err := gateway.NewServer(ctx, gateway.ServerConfig{
				Addr:         "127.0.0.1:0",
				TLS:          tlsFor("server.pem", "server-key.pem"),
				Service:      gateway.NewService(gateway.WithAuthn(reg), gateway.WithGraphBackend(backend), gateway.WithCrosswalkBackend(backend)),
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
	})
})
