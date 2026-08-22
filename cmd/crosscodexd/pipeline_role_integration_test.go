//go:build integration

package main

import (
	"context"
	"fmt"
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
	"github.com/complytime-labs/crosscodex/pkg/tenant"
	"github.com/complytime-labs/crosscodex/pkg/tlsconfig"
	"github.com/google/uuid"
)

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
		tenantID := fmt.Sprintf("crosscodexd-split-e2e-%s", uuid.New().String())
		fakeLLM := deterministicLLMClient{}

		configureRole := func(role string) *config.Config {
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

		pipelineCfg := configureRole(config.RolePipeline)
		pipelineCfg.Pipeline.Addr = "127.0.0.1:0"
		pipelineCfg.Health.Addr = "127.0.0.1:0"
		// This test dials the pipeline role over the network, so both sides
		// need matching mTLS: the pipeline listener presents the server cert;
		// the gateway/client below presents the client cert. Both trust one CA.
		pipelineCfg.TLS = testServerTLS()

		workerCfg := configureRole(config.RoleWorker)

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

		gwCfg := baseTestConfig(dsn)
		gwCfg.Role = config.RoleGateway
		gwCfg.Server.Addr = "127.0.0.1:0"
		gwCfg.Pipeline.Endpoint = pipelineAddr
		gwCfg.TLS = testClientTLS()

		gwRT, err := bootstrap(ctx, gwCfg)
		Expect(err).NotTo(HaveOccurred(), "bootstrap gateway role pointed at the standalone pipeline role")
		defer gwRT.close()
		Expect(gwRT.start(ctx)).To(Succeed(), "start gateway role")
		defer func() { _ = gwRT.stop(context.Background()) }()

		Expect(gwRT.pipelineService).To(BeNil(),
			"a standalone gateway role must not build an in-process pipeline.Service anymore")
		Expect(gwRT.gatewayServer).NotTo(BeNil())

		suPool, err := db.NewPool(db.PoolConfig{DSN: dsn, MaxOpenConns: 5, Extensions: []string{"age", "vector"}})
		Expect(err).NotTo(HaveOccurred())
		defer suPool.Close()
		Expect(db.EnsureTenant(ctx, suPool, tenantID, "crosscodexd split e2e")).To(Succeed())

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

	It("rejects a client presenting no certificate, and a valid-cert client with no tenant header", func() {
		dsn := os.Getenv("TEST_DATABASE_DSN")
		if dsn == "" {
			Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
		}

		ctx := context.Background()
		natsURL := startSharedTestNATS(GinkgoT())
		tenantID := fmt.Sprintf("crosscodexd-split-negative-%s", uuid.New().String())

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
		Expect(err.Error()).To(Or(ContainSubstring("certificate"), ContainSubstring("tls")),
			"error should surface as a TLS handshake failure")

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
