//go:build integration

package main

import (
	"context"
	"fmt"
	"hash/fnv"
	"math/rand"
	"os"
	"time"

	"connectrpc.com/connect"
	natsserver "github.com/nats-io/nats-server/v2/server"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
	"github.com/google/uuid"
)

// e2eModel is the single embedding/completion model name used throughout
// this suite, mirroring internal/pipeline/pipeline_e2e_integration_test.go's
// e2eModel constant.
const e2eModel = "e2e-test-model"

// e2eEmbeddingDim mirrors the fixed pgvector column width
// (migrations/001_initial_schema.up.sql: "vector vector(2000)"), reproduced
// here rather than imported since it's a test-local constant in the
// internal/pipeline package, not an exported production value.
const e2eEmbeddingDim = 2000

// deterministicLLMClient is a hermetic fake llmclient.Client, copied
// verbatim from internal/pipeline/pipeline_e2e_integration_test.go. Embed
// returns deterministic vectors seeded from each input's fnv hash. Complete
// returns one canned, parseable response per analyzer, routed by the prompt
// name the worker forwards from the task payload.
//
// Rationale for injecting this at the Go level rather than mocking the LLM
// gateway over HTTP: llmclient.CompletionRequest.PromptName (pkg/llmclient/
// types.go) is `json:"-"`, so it is never serialized in the outgoing HTTP
// request -- an HTTP-level mock would have to content-sniff raw prompt text
// to know which analyzer is calling, which is both fragile and unprecedented
// elsewhere in this codebase.
type deterministicLLMClient struct{}

var _ llmclient.Client = deterministicLLMClient{}

func (deterministicLLMClient) Complete(_ context.Context, req *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
	var content string
	switch req.PromptName {
	case "classify":
		// ParseClassification expects "Type|Level".
		content = "Technical|Operational"
	case "requires":
		// requires.ParseResponse keys on "REQUIRES: YES|NO"; a single YES vote
		// with HIGH confidence clears the consensus threshold.
		content = "REQUIRES: YES\nJUSTIFICATION: prerequisite control\nCONFIDENCE: HIGH"
	case "relationship":
		// relationship.Aggregate omits NO_RELATIONSHIP pairs, so the
		// relationship analyzer produces an (empty) result row and populates no
		// vote_summaries rows -- keeping vote_summaries rows == requires pairs so
		// synthesis's persistViability count guard holds.
		content = "RELATIONSHIP: NO_RELATIONSHIP\nCONFIDENCE: HIGH"
	case "artifacts":
		// artifacts.ParseResponse treats "ARTIFACTS: NONE" as the empty
		// sentinel -- a valid, parseable "no artifacts" result.
		content = "ARTIFACTS: NONE"
	default:
		return nil, fmt.Errorf("deterministicLLMClient: unexpected prompt name %q", req.PromptName)
	}

	return &llmclient.CompletionResponse{
		Model: req.Model,
		Choices: []llmclient.CompletionChoice{
			{
				Index:        0,
				Message:      llmclient.ChatMessage{Role: "assistant", Content: content},
				FinishReason: "stop",
			},
		},
	}, nil
}

func (deterministicLLMClient) Embed(_ context.Context, req *llmclient.EmbeddingRequest) (*llmclient.EmbeddingResponse, error) {
	data := make([]llmclient.EmbeddingData, len(req.Input))
	for i, text := range req.Input {
		h := fnv.New64a()
		if _, err := h.Write([]byte(text)); err != nil {
			return nil, fmt.Errorf("deterministicLLMClient: hashing input: %w", err)
		}
		rng := rand.New(rand.NewSource(int64(h.Sum64())))
		vec := make([]float32, e2eEmbeddingDim)
		for j := range vec {
			vec[j] = float32(rng.NormFloat64())
		}
		data[i] = llmclient.EmbeddingData{Index: i, Embedding: vec}
	}
	return &llmclient.EmbeddingResponse{Data: data, Model: req.Model}, nil
}

func (deterministicLLMClient) Health(_ context.Context) error { return nil }
func (deterministicLLMClient) Close() error                   { return nil }

// startSharedTestNATS starts a real, standalone NATS server (JetStream
// enabled) on a random localhost port, mirroring pkg/natsbus's own embedded
// startup options exactly. Unlike letting each bootstrap() call start its
// own embedded server (config.NATSConfig{URL: ""}), which would give the
// gateway and worker runtimes two disconnected buses on two different random
// ports, this starts exactly one server up front and both runtimes are
// configured to dial it as an external client (config.NATSConfig{URL: url}).
// That is what makes this test "two processes talking over one real NATS
// server" rather than two processes each humming to themselves.
func startSharedTestNATS(t GinkgoTInterface) string {
	t.Helper()

	opts := &natsserver.Options{
		Host:               "127.0.0.1",
		Port:               -1, // random available port
		NoLog:              true,
		NoSigs:             true,
		JetStream:          true,
		StoreDir:           t.TempDir(),
		JetStreamMaxMemory: -1,
		JetStreamMaxStore:  -1,
	}
	ns, err := natsserver.NewServer(opts)
	Expect(err).NotTo(HaveOccurred(), "create shared test NATS server")

	ns.Start()
	Expect(ns.ReadyForConnections(10*time.Second)).To(BeTrue(), "shared test NATS server did not become ready")

	DeferCleanup(func() {
		ns.Shutdown()
		ns.WaitForShutdown()
	})

	return ns.ClientURL()
}

// e2eAnalysisConfig returns an AnalysisConfig with every analyzer enabled,
// mirroring internal/pipeline/pipeline_e2e_integration_test.go's
// analysisConfig. ActionableTypes is left empty: it's the configured NIST IR
// 8477 relationship-type set (see pkg/config/validate.go's
// validRelationshipTypes), which can never legally contain "requires" --
// requires-derived rows are counted as actionable unconditionally by
// synthesis.NewAssessor (see internal/synthesis.ConsensusRequires), not via
// this list.
func e2eAnalysisConfig() config.AnalysisConfig {
	return config.AnalysisConfig{
		Engine: config.EngineConfig{
			TaskTimeout:  2 * time.Minute,
			MaxRetries:   2,
			RetryBackoff: time.Second,
		},
		Classification: config.ClassificationConfig{
			Enabled: true, Model: e2eModel, MaxTextLength: 10000, Temperature: 0, MaxTokens: 512,
		},
		Embedding: config.EmbeddingConfig{
			Enabled: true, Models: []string{e2eModel}, MaxChars: 10000, BatchSize: 1,
		},
		Relationship: config.RelationshipConfig{
			Enabled: true, Models: []string{e2eModel}, TopK: 10,
			MaxSourceChars: 10000, MaxTargetChars: 10000, MaxTokens: 512,
			SamplesPerModel: 1, SamplingTemperature: 0,
		},
		Candidates: config.CandidateConfig{
			Generators:           []config.CandidateGeneratorEntry{{Name: "semantic", Enabled: true, Weight: 1.0}},
			MinEmbeddingCoverage: 0.8,
			EmbedModel:           e2eModel,
		},
		Requires: config.RequiresConfig{
			Enabled: true, Models: []string{e2eModel}, SamplesPerModel: 1,
			ConsensusThreshold: 0.5, MaxErrorRate: 0.5,
			MaxSourceChars: 10000, MaxTargetChars: 10000, MaxTokens: 512, SamplingTemperature: 0,
		},
		Artifacts: config.ArtifactsConfig{
			Enabled: true, Models: []string{e2eModel}, SamplesPerModel: 1,
			MaxTokens: 512, MaxTextChars: 10000, FuzzyThreshold: 0.6, SamplingTemperature: 0,
		},
	}
}

// This suite contributes to the shared Ginkgo run tree driven by
// TestCrosscodexdIntegrationBDD in bootstrap_integration_test.go. Ginkgo
// registers all Describe/It nodes package-wide regardless of which _test.go
// file declares them; RunSpecs may only be invoked once per test binary
// ("It looks like you are calling RunSpecs more than once" if a second
// `func TestXxx(t *testing.T) { RunSpecs(...) }` is added here), so this
// file deliberately has no testing.T entry point of its own.
var _ = Describe("crosscodexd pipeline -> worker -> analysis, role=gateway + role=worker over real NATS and Postgres", func() {
	It("completes a job created through the gateway role using a separately-bootstrapped worker role", func() {
		dsn := os.Getenv("TEST_DATABASE_DSN")
		if dsn == "" {
			Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
		}

		ctx := context.Background()
		natsURL := startSharedTestNATS(GinkgoT())
		tenantID := fmt.Sprintf("crosscodexd-e2e-%s", uuid.New().String())
		fakeLLM := deterministicLLMClient{}

		configureRole := func(role string) *config.Config {
			cfg := baseTestConfig(dsn)
			cfg.Role = role
			// External mode: both runtimes dial the one shared server started
			// above instead of each starting their own embedded instance.
			cfg.NATS = config.NATSConfig{URL: natsURL}
			// GatewayURL only needs to be non-empty so buildSharedResources'
			// llmclient.NewClient(cfg.LLM) call succeeds; the real client it
			// constructs is immediately replaced by WithLLMClient(fakeLLM)
			// below, so nothing ever dials this address.
			cfg.LLM = config.LLMConfig{GatewayURL: "http://127.0.0.1:0", DefaultModel: e2eModel, EmbeddingModel: e2eModel}
			cfg.Analysis = e2eAnalysisConfig()
			// Embedded-only prompt layer: built-in defaults provide classify,
			// artifacts, requires, and relationship prompts. Restricting the
			// stack to "embedded" keeps the test hermetic (no user/project
			// XDG overlay).
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
			return cfg
		}

		gwCfg := configureRole(config.RoleGateway)
		workerCfg := configureRole(config.RoleWorker)

		gwRT, err := bootstrap(ctx, gwCfg, WithLLMClient(fakeLLM))
		Expect(err).NotTo(HaveOccurred(), "bootstrap gateway role")
		defer gwRT.close()
		Expect(gwRT.start(ctx)).To(Succeed(), "start gateway role")
		defer func() { _ = gwRT.stop(context.Background()) }()

		workerRT, err := bootstrap(ctx, workerCfg, WithLLMClient(fakeLLM))
		Expect(err).NotTo(HaveOccurred(), "bootstrap worker role")
		defer workerRT.close()
		Expect(workerRT.start(ctx)).To(Succeed(), "start worker role")
		defer func() { _ = workerRT.stop(context.Background()) }()

		suPool, err := db.NewPool(db.PoolConfig{DSN: dsn, MaxOpenConns: 5, Extensions: []string{"age", "vector"}})
		Expect(err).NotTo(HaveOccurred())
		defer suPool.Close()
		Expect(db.EnsureTenant(ctx, suPool, tenantID, "crosscodexd e2e")).To(Succeed())

		// Seed a catalog with a section and two leaf controls sharing
		// identical statement text, mirroring
		// internal/pipeline/pipeline_e2e_integration_test.go: identical text
		// yields identical prepared embeddings (cosine similarity 1.0), which
		// is what actually produces a candidate pair for the requires and
		// relationship analyzers to run against. A single-control catalog
		// would leave that path completely untested (0 candidates).
		catalogID := "cat-" + uuid.New().String()
		Expect(suPool.Exec(ctx, `
			INSERT INTO catalogs (catalog_id, tenant_id, name, version, source_type, object_path)
			VALUES ($1, $2, 'E2E Catalog', 'v1', 'test', 'unused')
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

		ctxTenant, err := tenant.WithTenant(ctx, tenantID)
		Expect(err).NotTo(HaveOccurred())

		resp, err := gwRT.pipelineService.CreateJob(ctxTenant, connect.NewRequest(&pb.CreateJobRequest{
			Config: &pb.JobConfig{Source: &pb.JobConfig_CatalogId{CatalogId: catalogID}},
		}))
		Expect(err).NotTo(HaveOccurred())
		jobID := resp.Msg.JobId

		Eventually(func() pb.JobStatus {
			getResp, err := gwRT.pipelineService.GetJob(ctxTenant, connect.NewRequest(&pb.GetJobRequest{JobId: jobID}))
			if err != nil {
				return pb.JobStatus_JOB_STATUS_UNSPECIFIED
			}
			return getResp.Msg.GetJob().GetStatus()
		}, 60*time.Second, 500*time.Millisecond).Should(
			Or(Equal(pb.JobStatus_JOB_STATUS_COMPLETED), Equal(pb.JobStatus_JOB_STATUS_FAILED)),
		)

		getResp, err := gwRT.pipelineService.GetJob(ctxTenant, connect.NewRequest(&pb.GetJobRequest{JobId: jobID}))
		Expect(err).NotTo(HaveOccurred())
		Expect(getResp.Msg.GetJob().GetStatus()).To(Equal(pb.JobStatus_JOB_STATUS_COMPLETED),
			fmt.Sprintf("job should complete via the worker role dispatching analyzer work over NATS (error: %s)", getResp.Msg.GetJob().GetError().GetMessage()))
	})
})
