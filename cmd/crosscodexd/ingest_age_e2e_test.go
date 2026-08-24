//go:build integration

package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	crosscodexv1connect "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/tlsconfig"
)

// This suite contributes to the shared Ginkgo run tree driven by
// TestCrosscodexdIntegrationBDD in bootstrap_integration_test.go.
var _ = Describe("crosscodexd RoleAll: inline OSCAL submission -> relational + AGE materialization", func() {
	It("boots a single all-role process, submits OSCAL over mTLS Connect, and verifies exact graph structure", func() {
		dsn := os.Getenv("TEST_DATABASE_DSN")
		if dsn == "" {
			Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
		}

		ctx := context.Background()

		// Unique per-run tenant so the spec is isolated and idempotent across
		// repeated runs against a persistent DB, mirroring the sibling specs
		// in e2e_integration_test.go and pipeline_role_integration_test.go.
		tenantID := fmt.Sprintf("crosscodexd-all-role-e2e-%s", uuid.New().String())

		// Build config on baseTestConfig + RoleAll overrides, mirroring
		// e2e_integration_test.go's configureRole closure.
		cfg := baseTestConfig(dsn)
		cfg.Database.GraphDSN = provisionGraphUser(ctx, dsn)
		cfg.Role = config.RoleAll
		cfg.NATS = config.NATSConfig{
			URL:      "", // embedded mode
			Embedded: config.NATSEmbeddedConfig{StoreDir: GinkgoT().TempDir()},
		}
		cfg.TLS = testServerTLS()
		cfg.LLM = config.LLMConfig{
			GatewayURL:     "http://127.0.0.1:0", // non-empty dummy
			DefaultModel:   e2eModel,
			EmbeddingModel: e2eModel,
		}
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
		cfg.Server.Addr = "127.0.0.1:0"
		cfg.Health.Addr = "127.0.0.1:0"
		cfg.Tenants.DefaultTenant = tenantID
		cfg.Storage.Objects.BasePath = GinkgoT().TempDir()

		// Bootstrap with the deterministic LLM fake.
		rt, err := bootstrap(ctx, cfg, WithLLMClient(deterministicLLMClient{}))
		Expect(err).NotTo(HaveOccurred(), "bootstrap all role")
		defer rt.close()

		Expect(rt.start(ctx)).To(Succeed(), "start all role")
		defer func() { _ = rt.stop(context.Background()) }()

		// Build an mTLS Connect gateway client.
		clientTLS, err := tlsconfig.BuildTLSConfig(ctx, testClientTLS(), "gateway-client")
		Expect(err).NotTo(HaveOccurred(), "build client TLS config")
		httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: clientTLS}}
		client := crosscodexv1connect.NewGatewayServiceClient(httpClient, "https://"+rt.gatewayServer.Addr())

		// Read the e2e fixture and submit it over the gateway.
		doc, err := os.ReadFile(e2eCatalogFixture)
		Expect(err).NotTo(HaveOccurred(), "read e2e catalog fixture")

		submitResp, err := client.SubmitDocument(ctx, connect.NewRequest(&pb.SubmitDocumentRequest{
			Source:        &pb.SubmitDocumentRequest_Content{Content: doc},
			CatalogFormat: pb.CatalogFormat_CATALOG_FORMAT_OSCAL,
			CatalogName:   "e2e-minimal",
		}))
		Expect(err).NotTo(HaveOccurred(), "SubmitDocument")
		jobID := submitResp.Msg.GetJobId()
		Expect(jobID).NotTo(BeEmpty(), "job id")

		// Poll to a terminal state.
		Eventually(func() pb.JobStatus {
			getResp, err := client.GetJob(ctx, connect.NewRequest(&pb.GetJobRequest{JobId: jobID}))
			if err != nil {
				return pb.JobStatus_JOB_STATUS_UNSPECIFIED
			}
			return getResp.Msg.GetJob().GetStatus()
		}, 90*time.Second, 500*time.Millisecond).Should(
			Or(Equal(pb.JobStatus_JOB_STATUS_COMPLETED), Equal(pb.JobStatus_JOB_STATUS_FAILED)),
		)

		getResp, err := client.GetJob(ctx, connect.NewRequest(&pb.GetJobRequest{JobId: jobID}))
		Expect(err).NotTo(HaveOccurred(), "GetJob")
		Expect(getResp.Msg.GetJob().GetStatus()).To(Equal(pb.JobStatus_JOB_STATUS_COMPLETED),
			fmt.Sprintf("job should complete (error: %s)", getResp.Msg.GetJob().GetError().GetMessage()))

		// --- Relational assertions ---
		// CatalogID is content-hash-derived, so read it first.
		var catalogID string
		Expect(rt.shared.appPool.QueryRow(ctx,
			"SELECT catalog_id FROM catalogs WHERE tenant_id = $1", tenantID).Scan(&catalogID)).
			To(Succeed(), "read catalog_id")

		var controlCount int
		Expect(rt.shared.appPool.QueryRow(ctx,
			"SELECT count(*) FROM controls WHERE catalog_id = $1", catalogID).Scan(&controlCount)).
			To(Succeed(), "count controls")
		Expect(controlCount).To(Equal(3), "controls in relational DB")

		// --- AGE assertions via the typed graphdb client ---
		// Graph materialization is asynchronous relative to the job reaching
		// COMPLETED: the pipeline publishes events that the in-process graph
		// subscriber consumes to write into AGE (CQRS eventual consistency).
		// Poll until the full expected structure has landed. The counts are
		// exact, not lower bounds — the poll only tolerates materialization
		// latency, it does not weaken the assertions.
		g := rt.shared.graphDB_

		Eventually(func(inner Gomega) {
			// Control nodes: ac-2 (section), ac-2.a, ac-2.b (requirements).
			// Node IDs are fmt.Sprintf("%s/%s", catalogID, item.ID).
			for _, id := range []string{"ac-2", "ac-2.a", "ac-2.b"} {
				nodeID := catalogID + "/" + id
				n, err := g.GetNode(ctx, tenantID, nodeID)
				inner.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("GetNode %s", nodeID))
				inner.Expect(n).NotTo(BeNil(), fmt.Sprintf("node %s exists", nodeID))
				inner.Expect(n.Label).To(Equal("Control"), fmt.Sprintf("node %s label", nodeID))
			}

			// PARENT_OF edges: ac-2 -> ac-2.a, ac-2 -> ac-2.b (2 total).
			parentOf, err := g.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{EdgeLabel: "PARENT_OF"})
			inner.Expect(err).NotTo(HaveOccurred(), "query PARENT_OF")
			inner.Expect(parentOf).To(HaveLen(2), "PARENT_OF edges")

			// requires=YES under the deterministic fake -> at least one REQUIRES
			// edge. Waiting for this to land also guarantees the analysis stage
			// (which would emit SEMANTIC_MATCH) has run, making the negative
			// SEMANTIC_MATCH assertion below a true negative rather than a
			// not-yet-materialized read.
			requires, err := g.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{EdgeLabel: "REQUIRES"})
			inner.Expect(err).NotTo(HaveOccurred(), "query REQUIRES")
			inner.Expect(len(requires)).To(BeNumerically(">=", 1), "REQUIRES edges (deterministic fake returns YES)")
		}, 60*time.Second, 500*time.Millisecond).Should(Succeed(), "AGE graph materialization")

		// LLM-derived subgraph under the deterministic fake: artifacts=NONE ->
		// no Artifact nodes; relationship=NO_RELATIONSHIP -> no SEMANTIC_MATCH.
		// Checked after the REQUIRES edge has landed, so the analysis stage has
		// completed and a zero count is authoritative.
		semantic, err := g.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{EdgeLabel: "SEMANTIC_MATCH"})
		Expect(err).NotTo(HaveOccurred(), "query SEMANTIC_MATCH")
		Expect(semantic).To(BeEmpty(), "SEMANTIC_MATCH edges (deterministic fake returns NO_RELATIONSHIP)")
	})
})
