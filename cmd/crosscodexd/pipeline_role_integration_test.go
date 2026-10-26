//go:build integration

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	crosscodexv1connect "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/complytime-labs/crosscodex/internal/gateway"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
	"github.com/complytime-labs/crosscodex/pkg/tlsconfig"
	"github.com/google/uuid"
)

// splitRoleConfig builds the config shared by the pipeline and worker roles
// of a split deployment: one external NATS server, the deterministic LLM
// wiring, and the given default tenant.
func splitRoleConfig(dsn, natsURL, tenantID, role string) *config.Config {
	cfg := baseTestConfig(dsn)
	cfg.Role = role
	cfg.NATS = config.NATSConfig{URL: natsURL}
	cfg.LLM = config.LLMConfig{GatewayURL: "http://127.0.0.1:0", DefaultModel: e2eModel, EmbeddingModel: e2eModel}
	cfg.Analysis = e2eAnalysisConfig()
	cfg.Prompt = config.PromptConfig{
		Layers: config.PromptLayerConfig{
			Enabled: true,
			Order:   []config.PromptLayerEntry{{ID: "embedded"}},
		},
	}
	cfg.Pipeline = config.PipelineConfig{MaxConcurrentJobs: 10}
	cfg.Attestation = config.AttestationConfig{
		PrivateKeyPath: testAttestationPrivateKeyPath,
		PublicKeyPath:  testAttestationPublicKeyPath,
		ExpiryDuration: 168 * time.Hour,
	}
	cfg.Tenants.DefaultTenant = tenantID
	cfg.Storage.Objects.BasePath = GinkgoT().TempDir()
	return cfg
}

// This suite contributes to the shared Ginkgo run tree driven by
// TestCrosscodexdIntegrationBDD in bootstrap_integration_test.go -- see
// e2e_integration_test.go's comment on why this file has no testing.T
// entry point of its own.
var _ = Describe("crosscodexd role=pipeline as a standalone, network-addressable process", func() {
	It("completes a job created and polled entirely over the network, with a separate worker role", func() {
		dsn := os.Getenv("TEST_DATABASE_DSN")
		if dsn == "" {
			Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
		}

		ctx := context.Background()
		natsURL := startSharedTestNATS(GinkgoT())
		tenantID := fmt.Sprintf("split-e2e-%s", uuid.New().String())
		fakeLLM := deterministicLLMClient{}

		pipelineCfg := splitRoleConfig(dsn, natsURL, tenantID, config.RolePipeline)
		pipelineCfg.Pipeline.Addr = "127.0.0.1:0"
		pipelineCfg.Health.Addr = "127.0.0.1:0"
		// This test dials the pipeline role over the network, so both sides
		// need matching mTLS: the pipeline listener presents the server cert;
		// the gateway/client below presents the client cert. Both trust one CA.
		pipelineCfg.TLS = testServerTLS()

		workerCfg := splitRoleConfig(dsn, natsURL, tenantID, config.RoleWorker)

		pipelineRT, err := bootstrap(ctx, pipelineCfg, WithLLMClient(fakeLLM))
		Expect(err).NotTo(HaveOccurred(), "bootstrap pipeline role")
		defer pipelineRT.close()
		Expect(pipelineRT.start(ctx)).To(Succeed(), "start pipeline role")
		defer func() { _ = pipelineRT.stop(context.Background()) }()

		Expect(pipelineRT.pipelineServer).NotTo(BeNil(), "pipeline role must own a standalone listener")
		pipelineAddr := pipelineRT.pipelineServer.Addr()

		workerRT, err := bootstrap(ctx, workerCfg, WithLLMClient(fakeLLM))
		Expect(err).NotTo(HaveOccurred(), "bootstrap worker role")
		defer workerRT.close()
		Expect(workerRT.start(ctx)).To(Succeed(), "start worker role")
		defer func() { _ = workerRT.stop(context.Background()) }()

		// The gateway role refuses a default tenant migrate has not
		// provisioned, so provision it before bootstrapping the gateway.
		suPool, err := db.NewPool(db.PoolConfig{DSN: dsn, MaxOpenConns: 5, Extensions: []string{"age", "vector"}})
		Expect(err).NotTo(HaveOccurred())
		defer suPool.Close()
		Expect(db.EnsureTenant(ctx, suPool, tenantID, "crosscodexd split e2e")).To(Succeed())

		gwCfg := baseTestConfig(dsn)
		gwCfg.Role = config.RoleGateway
		gwCfg.Server.Addr = "127.0.0.1:0"
		gwCfg.Pipeline.Endpoint = pipelineAddr
		gwCfg.TLS = testClientTLS()
		gwCfg.NATS = config.NATSConfig{URL: natsURL}
		gwCfg.Tenants.DefaultTenant = tenantID
		gwCfg.Storage.Objects.BasePath = GinkgoT().TempDir()

		gwRT, err := bootstrap(ctx, gwCfg)
		Expect(err).NotTo(HaveOccurred(), "bootstrap gateway role pointed at the standalone pipeline role")
		defer gwRT.close()
		Expect(gwRT.start(ctx)).To(Succeed(), "start gateway role")
		defer func() { _ = gwRT.stop(context.Background()) }()

		Expect(gwRT.pipelineService).To(BeNil(),
			"a standalone gateway role must not build an in-process pipeline.Service anymore")
		Expect(gwRT.gatewayServer).NotTo(BeNil())

		catalogID := "cat-" + uuid.New().String()
		Expect(suPool.Exec(ctx, `
			INSERT INTO catalogs (catalog_id, tenant_id, name, version, source_type, object_path)
			VALUES ($1, $2, 'Split E2E Catalog', 'v1', 'test', 'unused')
		`, catalogID, tenantID)).To(Succeed())

		const identicalStatement = "The organization enforces multi-factor authentication for all privileged accounts."
		seedControls := []struct{ id, title, statement, class, parentID string }{
			{"section-1", "Access Control", "Access control family.", "compliance-section", ""},
			{"ctrl-1", "Control One", identicalStatement, "SC", "section-1"},
			{"ctrl-2", "Control Two", identicalStatement, "SC", "section-1"},
		}
		for _, c := range seedControls {
			Expect(suPool.Exec(ctx, `
				INSERT INTO controls (tenant_id, control_id, catalog_id, identifier, title, statement, class, parent_id)
				VALUES ($1, $2, $3, $2, $4, $5, $6, NULLIF($7, ''))
			`, tenantID, c.id, catalogID, c.title, c.statement, c.class, c.parentID)).To(Succeed())
		}

		// Drive CreateJob/GetJob/CancelJob through the exact production code
		// path this issue adds -- a Connect client dialing the standalone
		// pipeline role over the network -- rather than any in-process
		// shortcut.
		backend, err := gateway.NewConnectPipelineBackend(ctx, pipelineAddr, gwCfg.TLS)
		Expect(err).NotTo(HaveOccurred())

		ctxTenant, err := tenant.WithTenant(ctx, tenantID)
		Expect(err).NotTo(HaveOccurred())

		resp, err := backend.CreateJob(ctxTenant, connect.NewRequest(&pb.CreateJobRequest{
			Config: &pb.JobConfig{Source: &pb.JobConfig_CatalogId{CatalogId: catalogID}},
		}))
		Expect(err).NotTo(HaveOccurred())
		jobID := resp.Msg.JobId

		Eventually(func() pb.JobStatus {
			getResp, err := backend.GetJob(ctxTenant, connect.NewRequest(&pb.GetJobRequest{JobId: jobID}))
			if err != nil {
				return pb.JobStatus_JOB_STATUS_UNSPECIFIED
			}
			return getResp.Msg.GetJob().GetStatus()
		}, 60*time.Second, 500*time.Millisecond).Should(
			Or(Equal(pb.JobStatus_JOB_STATUS_COMPLETED), Equal(pb.JobStatus_JOB_STATUS_FAILED)),
		)

		getResp, err := backend.GetJob(ctxTenant, connect.NewRequest(&pb.GetJobRequest{JobId: jobID}))
		Expect(err).NotTo(HaveOccurred())
		Expect(getResp.Msg.GetJob().GetStatus()).To(Equal(pb.JobStatus_JOB_STATUS_COMPLETED),
			fmt.Sprintf("job should complete via the worker role dispatching analyzer work over NATS, "+
				"with pipeline running as a standalone process (error: %s)", getResp.Msg.GetJob().GetError().GetMessage()))

		// The job already completed above, so CancelJob is expected to
		// report "not running" rather than succeed -- this still proves
		// CancelJob round-trips over the network to a real pipeline process.
		cancelResp, err := backend.CancelJob(ctxTenant, connect.NewRequest(&pb.CancelJobRequest{JobId: jobID}))
		if err == nil {
			Expect(cancelResp.Msg.Cancelled).To(BeFalse())
		} else {
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
		}
	})

	It("imports an OSCAL catalog through a standalone gateway and completes its job with separate pipeline, worker and graph roles", func() {
		dsn := os.Getenv("TEST_DATABASE_DSN")
		if dsn == "" {
			Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
		}

		ctx := context.Background()
		natsURL := startSharedTestNATS(GinkgoT())
		tenantID := fmt.Sprintf("split-import-%s", uuid.New().String())
		fakeLLM := deterministicLLMClient{}

		pipelineCfg := splitRoleConfig(dsn, natsURL, tenantID, config.RolePipeline)
		pipelineCfg.Pipeline.Addr = "127.0.0.1:0"
		pipelineCfg.Health.Addr = "127.0.0.1:0"
		pipelineCfg.TLS = testServerTLS()

		pipelineRT, err := bootstrap(ctx, pipelineCfg, WithLLMClient(fakeLLM))
		Expect(err).NotTo(HaveOccurred(), "bootstrap pipeline role")
		defer pipelineRT.close()
		Expect(pipelineRT.start(ctx)).To(Succeed(), "start pipeline role")
		defer func() { _ = pipelineRT.stop(context.Background()) }()

		workerRT, err := bootstrap(ctx, splitRoleConfig(dsn, natsURL, tenantID, config.RoleWorker), WithLLMClient(fakeLLM))
		Expect(err).NotTo(HaveOccurred(), "bootstrap worker role")
		defer workerRT.close()
		Expect(workerRT.start(ctx)).To(Succeed(), "start worker role")
		defer func() { _ = workerRT.stop(context.Background()) }()

		// The graph role is the only Control-node writer: catalog ingest in
		// the gateway publishes the parsed catalog and stops there.
		graphCfg := baseTestConfig(dsn)
		graphCfg.Role = config.RoleGraph
		graphCfg.NATS = config.NATSConfig{URL: natsURL}
		graphCfg.Health.Addr = "127.0.0.1:0"

		graphRT, err := bootstrap(ctx, graphCfg)
		Expect(err).NotTo(HaveOccurred(), "bootstrap graph role")
		defer graphRT.close()
		Expect(graphRT.start(ctx)).To(Succeed(), "start graph role")
		defer func() { _ = graphRT.stop(context.Background()) }()

		// The gateway role refuses a default tenant migrate has not
		// provisioned, so provision it before bootstrapping the gateway.
		suPool, err := db.NewPool(db.PoolConfig{DSN: dsn, MaxOpenConns: 5, Extensions: []string{"age", "vector"}})
		Expect(err).NotTo(HaveOccurred())
		defer suPool.Close()
		Expect(db.EnsureTenant(ctx, suPool, tenantID, "crosscodexd split import e2e")).To(Succeed())

		// Mirrors deploy/config.yaml: the global identity is the daemon's
		// client cert (used to dial the pipeline role via pipeline-client),
		// and the gateway-server target swaps in the server cert for the
		// listener, inheriting mutual mode and the CA from the global config.
		gwCfg := baseTestConfig(dsn)
		gwCfg.Role = config.RoleGateway
		gwCfg.Server.Addr = "127.0.0.1:0"
		gwCfg.Pipeline.Endpoint = pipelineRT.pipelineServer.Addr()
		gwCfg.TLS = testClientTLS()
		gwCfg.TLS.Targets = map[string]config.TLSOverride{
			"gateway-server": {Cert: testTLSServerCertPath, Key: testTLSServerKeyPath},
		}
		gwCfg.NATS = config.NATSConfig{URL: natsURL}
		gwCfg.Tenants.DefaultTenant = tenantID
		gwCfg.Storage.Objects.BasePath = GinkgoT().TempDir()

		gwRT, err := bootstrap(ctx, gwCfg)
		Expect(err).NotTo(HaveOccurred(), "bootstrap gateway role")
		defer gwRT.close()
		Expect(gwRT.start(ctx)).To(Succeed(), "start gateway role")
		defer func() { _ = gwRT.stop(context.Background()) }()
		Expect(gwRT.shared.graphDB_).To(BeNil(), "the gateway role must hold no graph credentials")

		clientTLS, err := tlsconfig.BuildTLSConfig(ctx, testClientTLS(), "gateway-client")
		Expect(err).NotTo(HaveOccurred(), "build client TLS config")
		client := crosscodexv1connect.NewGatewayServiceClient(
			&http.Client{Transport: &http.Transport{TLSClientConfig: clientTLS}},
			"https://"+gwRT.gatewayServer.Addr(),
		)

		doc, err := os.ReadFile(e2eCatalogFixture)
		Expect(err).NotTo(HaveOccurred(), "read e2e catalog fixture")
		submitResp, err := client.SubmitDocument(ctx, connect.NewRequest(&pb.SubmitDocumentRequest{
			Source:        &pb.SubmitDocumentRequest_Content{Content: doc},
			CatalogFormat: pb.CatalogFormat_CATALOG_FORMAT_OSCAL,
			CatalogName:   "split-import",
		}))
		Expect(err).NotTo(HaveOccurred(), "SubmitDocument through the standalone gateway")
		jobID := submitResp.Msg.GetJobId()
		Expect(jobID).NotTo(BeEmpty(), "job id")
		Expect(submitResp.Msg.GetDocumentId()).NotTo(BeEmpty(), "document id")

		// SubmitDocumentResponse carries no catalog_id; it is content-hash
		// derived, so read it back. Reads enter the tenant for RLS as app_user.
		tenantCtx, err := tenant.WithTenant(ctx, tenantID)
		Expect(err).NotTo(HaveOccurred())
		tx, err := db.NewTenantPool(gwRT.shared.appPool).Begin(tenantCtx)
		Expect(err).NotTo(HaveOccurred(), "begin tenant transaction")
		var catalogID string
		Expect(tx.QueryRow(tenantCtx,
			"SELECT catalog_id FROM catalogs WHERE tenant_id = $1", tenantID).Scan(&catalogID)).
			To(Succeed(), "read catalog_id")
		var controlCount int
		Expect(tx.QueryRow(tenantCtx,
			"SELECT count(*) FROM controls WHERE catalog_id = $1", catalogID).Scan(&controlCount)).
			To(Succeed(), "count controls")
		Expect(tx.Rollback()).To(Succeed())
		Expect(controlCount).To(Equal(3), "controls in relational DB")

		// A transport, auth or TLS error is reported as the failure rather
		// than masked as a status, and a FAILED job stops the poll at once.
		Eventually(func(inner Gomega) {
			getResp, err := client.GetJob(ctx, connect.NewRequest(&pb.GetJobRequest{JobId: jobID}))
			inner.Expect(err).NotTo(HaveOccurred(), "GetJob through the standalone gateway")
			job := getResp.Msg.GetJob()
			if job.GetStatus() == pb.JobStatus_JOB_STATUS_FAILED {
				StopTrying(fmt.Sprintf("job %s failed: %s", jobID, job.GetError().GetMessage())).Now()
			}
			inner.Expect(job.GetStatus()).To(Equal(pb.JobStatus_JOB_STATUS_COMPLETED), "job status")
		}, 60*time.Second, 500*time.Millisecond).Should(Succeed(), "job completes")

		// The graph role projects Control nodes asynchronously from the
		// catalog-parsed event, so poll. Node IDs are "<catalogID>/<item ID>".
		// The tenant is unique to this spec, so the counts are exact.
		g := graphRT.shared.graphDB_
		parentID := catalogID + "/ac-2"
		childA, childB := catalogID+"/ac-2.a", catalogID+"/ac-2.b"
		Eventually(func(inner Gomega) {
			for _, nodeID := range []string{parentID, childA, childB} {
				n, err := g.GetNode(ctx, tenantID, nodeID)
				inner.Expect(err).NotTo(HaveOccurred(), "GetNode %s", nodeID)
				inner.Expect(n).NotTo(BeNil(), "node %s exists", nodeID)
				inner.Expect(n.Label).To(Equal("Control"), "node %s label", nodeID)
			}
			parentOf, err := g.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{EdgeLabel: "PARENT_OF"})
			inner.Expect(err).NotTo(HaveOccurred(), "query PARENT_OF")
			pairs := make([]string, 0, len(parentOf))
			for _, r := range parentOf {
				pairs = append(pairs, r.Source.ID+" -> "+r.Target.ID)
			}
			inner.Expect(pairs).To(ConsistOf(parentID+" -> "+childA, parentID+" -> "+childB), "PARENT_OF edges")
		}, 60*time.Second, 500*time.Millisecond).Should(Succeed(), "graph role projects the imported catalog")
	})

	It("rejects a client presenting no certificate, and a valid-cert client with no tenant header", func() {
		dsn := os.Getenv("TEST_DATABASE_DSN")
		if dsn == "" {
			Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
		}

		ctx := context.Background()
		natsURL := startSharedTestNATS(GinkgoT())
		tenantID := fmt.Sprintf("split-negative-%s", uuid.New().String())

		cfg := baseTestConfig(dsn)
		cfg.Role = config.RolePipeline
		cfg.NATS = config.NATSConfig{URL: natsURL}
		cfg.LLM = config.LLMConfig{GatewayURL: "http://127.0.0.1:0", DefaultModel: e2eModel, EmbeddingModel: e2eModel}
		cfg.Analysis = e2eAnalysisConfig()
		cfg.Prompt = config.PromptConfig{
			Layers: config.PromptLayerConfig{Enabled: true, Order: []config.PromptLayerEntry{{ID: "embedded"}}},
		}
		cfg.Pipeline = config.PipelineConfig{MaxConcurrentJobs: 10, Addr: "127.0.0.1:0"}
		cfg.Attestation = config.AttestationConfig{
			PrivateKeyPath: testAttestationPrivateKeyPath,
			PublicKeyPath:  testAttestationPublicKeyPath,
			ExpiryDuration: 168 * time.Hour,
		}
		cfg.Tenants.DefaultTenant = tenantID
		cfg.Storage.Objects.BasePath = GinkgoT().TempDir()
		cfg.Health.Addr = "127.0.0.1:0"
		// Real mTLS on the listener: attachPipeline now requires it, and both
		// checks below exercise the TLS boundary rather than plaintext.
		cfg.TLS = testServerTLS()

		rt, err := bootstrap(ctx, cfg, WithLLMClient(deterministicLLMClient{}))
		Expect(err).NotTo(HaveOccurred())
		defer rt.close()
		Expect(rt.start(ctx)).To(Succeed())
		defer func() { _ = rt.stop(context.Background()) }()

		addr := rt.pipelineServer.Addr()

		// mTLS enforcement: a client that trusts the CA but presents NO client
		// certificate must be rejected at the TLS handshake, before any
		// Connect-level response can form. This proves the listener actually
		// applies RequireAndVerifyClientCert -- a plaintext or server-only
		// listener would let this request reach the tenant interceptor and
		// return CodeUnauthenticated instead.
		noCertTLS, err := tlsconfig.BuildTLSConfig(ctx, config.TLSConfig{Mode: "server-only", CA: testTLSCAPath}, "")
		Expect(err).NotTo(HaveOccurred())
		noCertClient := crosscodexv1connect.NewPipelineServiceClient(
			&http.Client{Transport: &http.Transport{TLSClientConfig: noCertTLS}},
			"https://"+addr,
		)
		_, err = noCertClient.GetJob(ctx, connect.NewRequest(&pb.GetJobRequest{JobId: "does-not-matter"}))
		Expect(err).To(HaveOccurred(), "listener must reject a client presenting no certificate")
		Expect(connect.CodeOf(err)).NotTo(Equal(connect.CodeUnauthenticated),
			"a no-cert connection must fail at the TLS handshake, not reach the tenant interceptor")
		// The TLS-layer rejection is asserted on a raw connection. With TLS 1.3
		// the client finishes its side of the handshake before the server checks
		// the (missing) client certificate; the server then sends a
		// certificate_required alert and closes. Through the Connect client
		// above, the error text depends on whether that alert or the request
		// body write wins, so its text is not asserted there. A connection that
		// writes nothing always reads the alert. Under TLS 1.2 the same alert
		// fails the handshake itself, so the Dial error is checked the same way.
		rawConn, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", addr, noCertTLS)
		if err == nil {
			defer func() { _ = rawConn.Close() }()
			Expect(rawConn.SetReadDeadline(time.Now().Add(10 * time.Second))).To(Succeed())
			_, err = rawConn.Read(make([]byte, 1))
		}
		var opErr *net.OpError
		Expect(errors.As(err, &opErr)).To(BeTrue(), "expected the server's TLS alert, got %v", err)
		Expect(opErr.Op).To(Equal("remote error"))
		Expect(err).To(MatchError(ContainSubstring("tls: certificate required")),
			"the listener must reject a client presenting no certificate at the TLS layer")

		// Tenant-header enforcement: a client that DOES pass mTLS (valid client
		// cert) but omits the tenant header must still be rejected with
		// CodeUnauthenticated by the server-side tenant interceptor. This
		// deliberately bypasses gateway.NewConnectPipelineBackend, which always
		// attaches the tenant interceptor.
		clientTLS, err := tlsconfig.BuildTLSConfig(ctx, testClientTLS(), "pipeline-client")
		Expect(err).NotTo(HaveOccurred())
		Expect(clientTLS).NotTo(BeNil())
		mtlsClient := crosscodexv1connect.NewPipelineServiceClient(
			&http.Client{Transport: &http.Transport{TLSClientConfig: clientTLS}},
			"https://"+addr,
		)
		_, err = mtlsClient.GetJob(ctx, connect.NewRequest(&pb.GetJobRequest{JobId: "does-not-matter"}))
		Expect(err).To(HaveOccurred())
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated))
	})
})
