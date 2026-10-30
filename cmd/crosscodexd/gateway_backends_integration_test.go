//go:build integration

package main

import (
	"context"
	"net/http"
	"os"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	crosscodexv1connect "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/tlsconfig"
)

// gatewayClient dials the bootstrapped gateway over mTLS, as the sibling
// ingest_age_e2e_test.go does.
func gatewayClient(ctx context.Context, rt *runtime) crosscodexv1connect.GatewayServiceClient {
	clientTLS, err := tlsconfig.BuildTLSConfig(ctx, testClientTLS(), "gateway-client")
	Expect(err).NotTo(HaveOccurred(), "build client TLS config")
	httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: clientTLS}}
	return crosscodexv1connect.NewGatewayServiceClient(httpClient, "https://"+rt.gatewayServer.Addr())
}

// The standalone gateway role is not covered here. It gets its mappings
// backend from the same buildIngestGateway as role all and attaches no graph
// backend.
//
// An unwired backend answers CodeUnavailable ("... backend not configured").
// A wired one reaches its store, so an unknown job is CodeNotFound.
var _ = Describe("gateway backend wiring", func() {
	const unknownJob = "00000000-0000-0000-0000-000000000000"

	It("role all attaches the mappings and graph backends", func() {
		dsn := os.Getenv("TEST_DATABASE_DSN")
		if dsn == "" {
			Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
		}

		cfg := baseTestConfig(dsn)
		cfg.Role = config.RoleAll
		cfg.NATS = config.NATSConfig{Embedded: config.NATSEmbeddedConfig{StoreDir: GinkgoT().TempDir()}}
		cfg.TLS = testServerTLS()
		cfg.LLM = config.LLMConfig{GatewayURL: reachableLLMGatewayURL(), DefaultModel: "test-model", EmbeddingModel: "test-model"}
		cfg.Server.Addr = "127.0.0.1:0"
		cfg.Tenants.DefaultTenant = "crosscodexd-test"
		cfg.Attestation.PrivateKeyPath = testAttestationPrivateKeyPath
		cfg.Attestation.PublicKeyPath = testAttestationPublicKeyPath
		cfg.Storage.Objects.BasePath = GinkgoT().TempDir()

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		Expect(migrateDatabase(ctx, cfg, GinkgoWriter)).To(Succeed(), "provision the default tenant")

		rt, err := bootstrap(ctx, cfg)
		Expect(err).NotTo(HaveOccurred(), "bootstrap failed")
		defer rt.close()
		Expect(rt.start(ctx)).To(Succeed(), "start all role")
		defer func() { _ = rt.stop(context.Background()) }()

		client := gatewayClient(ctx, rt)

		_, err = client.ListJobMappings(ctx, connect.NewRequest(&pb.ListJobMappingsRequest{JobId: unknownJob}))
		Expect(err).To(HaveOccurred())
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound),
			"ListJobMappings for an unknown job must reach the store; Unavailable means gateway.WithMappingsBackend is not wired: %v", err)

		_, err = client.QueryGraph(ctx, connect.NewRequest(&pb.QueryGraphRequest{Cypher: "MATCH (n) RETURN n"}))
		if err != nil {
			Expect(connect.CodeOf(err)).NotTo(Equal(connect.CodeUnavailable),
				"QueryGraph must reach the graph backend; Unavailable means gateway.WithGraphBackend is not wired in role all: %v", err)
		}
	})
})
