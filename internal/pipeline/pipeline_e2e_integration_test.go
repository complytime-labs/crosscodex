//go:build integration

package pipeline

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	crand "crypto/rand"
	"database/sql"
	"fmt"
	"hash/fnv"
	"math/rand"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"connectrpc.com/connect"
	_ "github.com/jackc/pgx/v5/stdlib"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/analysis"
	pipelineattestation "github.com/complytime-labs/crosscodex/internal/pipeline/attestation"
	"github.com/complytime-labs/crosscodex/internal/synthesis"
	"github.com/complytime-labs/crosscodex/internal/worker"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/prompt"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
	"github.com/complytime-labs/crosscodex/pkg/vectordb"
	"github.com/google/uuid"
)

// e2eModel is the single embedding/completion model name used throughout the
// end-to-end test. Every analyzer config, the candidate generator's
// EmbedModel, and the worker's LLM defaults reference it, so the embeddings
// the pipeline writes and the similarity matrix the candidate generator reads
// agree on one model.
const e2eModel = "e2e-test-model"

// deterministicLLMClient is a hermetic fake llmclient.Client for the
// end-to-end test. Embed returns 2000-dimension vectors deterministically
// seeded from each input's fnv hash (identical text always yields the identical
// vector, so two controls with identical prepared text embed to cosine
// similarity 1.0 -> a stable candidate pair). Complete returns one canned,
// parseable response per analyzer, routed by the prompt name the worker
// forwards from the task payload.
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
		// vote_summaries rows — keeping vote_summaries rows == requires pairs so
		// synthesis's persistViability count guard holds.
		content = "RELATIONSHIP: NO_RELATIONSHIP\nCONFIDENCE: HIGH"
	case "artifacts":
		// artifacts.ParseResponse treats "ARTIFACTS: NONE" as the empty
		// sentinel — a valid, parseable "no artifacts" result.
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
		vec := make([]float32, embeddingDim)
		for j := range vec {
			vec[j] = float32(rng.NormFloat64())
		}
		data[i] = llmclient.EmbeddingData{Index: i, Embedding: vec}
	}
	return &llmclient.EmbeddingResponse{Data: data, Model: req.Model}, nil
}

func (deterministicLLMClient) Health(_ context.Context) error { return nil }
func (deterministicLLMClient) Close() error                   { return nil }

// e2eKeyProvider is an ephemeral ECDSA P-256 key provider for the attestation
// generator, mirroring pkg/attestation's test double. Attestation failures are
// non-fatal to job completion, but pipeline.New requires a working generator.
type e2eKeyProvider struct{ key *ecdsa.PrivateKey }

// newE2EKeyProvider builds an e2eKeyProvider. Offset 1 so a key-generation
// failure (vanishingly unlikely) is reported at the caller's line.
func newE2EKeyProvider() *e2eKeyProvider {
	key, err := ecdsa.GenerateKey(elliptic.P256(), crand.Reader)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "generate attestation key")
	return &e2eKeyProvider{key: key}
}

func (p *e2eKeyProvider) SigningKey(_ context.Context) (crypto.Signer, error) {
	return p.key, nil
}

func (p *e2eKeyProvider) VerificationKey(_ context.Context) (crypto.PublicKey, error) {
	return p.key.Public(), nil
}

func (p *e2eKeyProvider) KeyID(_ context.Context) (string, error) { return "e2e-key-id", nil }

// e2eScalar runs a single-integer query through the tenant-scoped connection
// (so RLS applies) inside a transaction, mirroring countCandidates. Offset 1
// so a failure here is reported at the caller's line.
func e2eScalar(conn db.TenantConnection, ctx context.Context, query string, args ...any) int {
	tx, err := conn.Begin(ctx)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "e2eScalar begin")
	defer func() { _ = tx.Rollback() }()
	var n int
	ExpectWithOffset(1, tx.QueryRow(ctx, query, args...).Scan(&n)).To(Succeed(), "e2eScalar scan (%s)", query)
	ExpectWithOffset(1, tx.Commit()).To(Succeed(), "e2eScalar commit")
	return n
}

// TestPipelineE2E exercises the entire durable pipeline against a real
// Postgres (db compose profile), an embedded NATS server, real analyzers, a
// real LLM worker, and a hermetic fake LLM client. It seeds a small catalog,
// runs one job to completion through pipeline.Service.CreateJob, and asserts
// that every persistence surface this plan introduced was populated for the
// owning tenant and is isolated from a second tenant by RLS.
var _ = Describe("Pipeline end-to-end", func() {
	It("runs a job to completion and populates every durable surface, isolated per tenant", func() {
		ctx := context.Background()

		suDSN := os.Getenv("TEST_DATABASE_DSN")
		if suDSN == "" {
			Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
		}

		// Migrate (idempotent).
		migrator, err := db.NewMigrator(suDSN)
		Expect(err).NotTo(HaveOccurred(), "create migrator")
		Expect(migrator.Up(ctx)).To(Succeed(), "run migrations")
		Expect(migrator.Close()).To(Succeed(), "close migrator")

		// Ensure app_user password (idempotent).
		adminDB, err := sql.Open("pgx", suDSN)
		Expect(err).NotTo(HaveOccurred(), "open admin connection")
		_, err = adminDB.ExecContext(ctx, "ALTER ROLE app_user WITH PASSWORD 'apppass'")
		Expect(err).NotTo(HaveOccurred(), "set app_user password")
		Expect(adminDB.Close()).To(Succeed(), "close admin connection")

		// Superuser pool for RLS-bypassing seed writes and verification reads.
		suPool, err := db.NewPool(db.PoolConfig{
			DSN:          suDSN,
			MaxOpenConns: 5,
			Extensions:   []string{"age", "vector"},
		})
		Expect(err).NotTo(HaveOccurred(), "create superuser pool")
		defer suPool.Close()

		// app_user pool -> tenant-scoped connection (RLS enforced).
		appDSN, err := candidateITAppUserDSN(suDSN)
		Expect(err).NotTo(HaveOccurred(), "build app_user DSN")
		appPool, err := db.NewPool(db.PoolConfig{DSN: appDSN, MaxOpenConns: 4})
		Expect(err).NotTo(HaveOccurred(), "create app_user pool")
		defer appPool.Close()
		tenantConn := db.NewTenantPool(appPool)

		// vectordb writes go through a superuser *sql.DB: EmbeddingAnalyzer.Aggregate
		// calls StoreBatch, which writes tenant_id explicitly but does not set
		// app.current_tenant, so an app_user connection would be blocked by the
		// embeddings RLS WITH CHECK. The superuser bypasses RLS, matching how the
		// candidate-generation integration test seeds embeddings.
		vecDB, err := sql.Open("pgx", suDSN)
		Expect(err).NotTo(HaveOccurred(), "open vectordb connection")
		defer vecDB.Close()
		vectors, err := vectordb.NewPgVectorStore(vecDB)
		Expect(err).NotTo(HaveOccurred(), "create pgvector store")

		// Two tenants: the owner (A) and an unrelated tenant (B) for the RLS check.
		tenantA := fmt.Sprintf("e2e-a-%s", uuid.New().String())
		tenantB := fmt.Sprintf("e2e-b-%s", uuid.New().String())
		Expect(db.EnsureTenant(ctx, suPool, tenantA, "e2e-a")).To(Succeed(), "ensure tenant A")
		Expect(db.EnsureTenant(ctx, suPool, tenantB, "e2e-b")).To(Succeed(), "ensure tenant B")

		catalogID := "cat-" + uuid.New().String()

		// Seed the catalog and its controls under tenant A. section-1 is a
		// compliance-section (classify/embedding auto-skip it, candidate generation
		// excludes it). c1 and c2 are its leaf children with identical statement
		// text, so their prepared embedding text — "[<parent title>] <statement>" —
		// is identical, yielding cosine similarity 1.0 and a stable candidate pair.
		// This also exercises the ancestor_title path (both children inherit the
		// section's title).
		Expect(suPool.Exec(ctx, `
			INSERT INTO catalogs (catalog_id, tenant_id, name, version, source_type, object_path)
			VALUES ($1, $2, 'E2E Catalog', 'v1', 'test', 'unused')
		`, catalogID, tenantA)).To(Succeed(), "seed catalog")

		const identicalStatement = "The organization enforces multi-factor authentication for all privileged accounts."
		controls := []struct {
			id, title, statement, class, parentID string
		}{
			{"section-1", "Access Control", "Access control family.", classifySectionClass, ""},
			{"ctrl-1", "Control One", identicalStatement, "SC", "section-1"},
			{"ctrl-2", "Control Two", identicalStatement, "SC", "section-1"},
		}
		for _, c := range controls {
			Expect(suPool.Exec(ctx, `
				INSERT INTO controls (tenant_id, control_id, catalog_id, identifier, title, statement, class, parent_id)
				VALUES ($1, $2, $3, $2, $4, $5, $6, NULLIF($7, ''))
			`, tenantA, c.id, catalogID, c.title, c.statement, c.class, c.parentID)).To(Succeed(), "seed control %s", c.id)
		}

		// Embedded NATS server (in-process JetStream). Shared by the engine's
		// dispatcher/collector, the pipeline's event publishing, and the worker.
		bus, err := natsbus.New(config.NATSConfig{
			URL:      "",
			Embedded: config.NATSEmbeddedConfig{StoreDir: GinkgoT().TempDir()},
		})
		Expect(err).NotTo(HaveOccurred(), "create embedded NATS")
		defer bus.Close()

		// Analyzer configuration: every analyzer enabled, one model, generous
		// limits so seeded text is never truncated to empty.
		analysisConfig := config.AnalysisConfig{
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
				SamplesPerModel: 1, SamplingTemperature: 0, ActionableTypes: []string{"related"},
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

		fakeLLM := deterministicLLMClient{}

		// Embedded-only prompt layer: the built-in defaults provide classify,
		// artifacts, requires, and relationship prompts. Restricting the stack to
		// "embedded" keeps the test hermetic (no user/project XDG overlay).
		prompts, err := prompt.NewRegistry(config.PromptConfig{
			Layers: config.PromptLayerConfig{
				Enabled: true,
				Order:   []config.PromptLayerEntry{{ID: "embedded"}},
			},
		})
		Expect(err).NotTo(HaveOccurred(), "create prompt registry")

		storageProvider, err := storage.NewLocal(GinkgoT().TempDir(), tenantA)
		Expect(err).NotTo(HaveOccurred(), "create local storage")

		store := NewPGStore(tenantConn, suPool)

		registry, err := NewProductionRegistry(fakeLLM, vectors, storageProvider, prompts, tenantConn, analysisConfig, nil, nil)
		Expect(err).NotTo(HaveOccurred(), "build production registry")

		taskTypes := map[string]natsbus.TaskType{
			"classify":     natsbus.TaskClassify,
			"embedding":    natsbus.TaskEmbed,
			"artifacts":    natsbus.TaskArtifacts,
			"requires":     natsbus.TaskRequires,
			"relationship": natsbus.TaskRelate,
		}

		dbReporter := NewDBStageReporter(analysis.NewNATSStageReporter(bus), store)
		engine := analysis.NewWithNATS(registry, bus, analysisConfig.Engine, taskTypes, analysis.WithStageReporter(dbReporter))

		// Candidate generator over the real Postgres adapters.
		candRegistry, err := BuildCandidateRegistry(analysisConfig.Candidates, nil, nil)
		Expect(err).NotTo(HaveOccurred(), "build candidate registry")
		candGen := NewCandidateGenerator(
			NewPGControlsReader(tenantConn),
			NewPGEmbeddingsReader(tenantConn),
			candRegistry,
			NewPGCandidateWriter(tenantConn),
			analysisConfig.Candidates,
			e2eModel,
		)

		// actionableTypes is the configured NIST IR 8477 relationship-type set;
		// requires-derived rows are always actionable regardless of this list
		// (see internal/synthesis.ConsensusRequires), so it's left empty here.
		synth := synthesis.New(tenantConn, config.SynthesisConfig{}, []string{})

		attestor, err := attestation.NewGenerator(newE2EKeyProvider())
		Expect(err).NotTo(HaveOccurred(), "create attestation generator")
		converter := pipelineattestation.NewConverter()

		pipelineCfg := config.PipelineConfig{MaxConcurrentJobs: 10}
		attCfg := config.AttestationConfig{ExpiryDuration: 168 * time.Hour}

		svc := New(
			store, engine, registry, synth, attestor, converter, bus, storageProvider,
			pipelineCfg, attCfg, candGen,
			WithCatalogControlsReader(NewPGCatalogControlsReader(tenantConn)),
			WithEmbeddingsReader(NewPGEmbeddingsReader(tenantConn)),
			WithEmbeddingConfig(analysisConfig.Embedding),
		)

		// Real worker consuming LLM tasks off the shared bus.
		w := worker.New(bus, fakeLLM, config.WorkerConfig{
			LLM: config.LLMConfig{DefaultModel: e2eModel, EmbeddingModel: e2eModel},
		})
		Expect(w.Start(context.Background())).To(Succeed(), "start worker")
		defer func() {
			stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := w.Stop(stopCtx); err != nil {
				GinkgoWriter.Printf("worker stop: %v\n", err)
			}
		}()

		// Create and launch the job under tenant A's context. CreateJob persists the
		// job + stages and runs executeJob in its own goroutine.
		ctxA, err := tenant.WithTenant(ctx, tenantA)
		Expect(err).NotTo(HaveOccurred(), "WithTenant A")
		req := connect.NewRequest(&pb.CreateJobRequest{
			Config: &pb.JobConfig{Source: &pb.JobConfig_CatalogId{CatalogId: catalogID}},
		})
		resp, err := svc.CreateJob(ctxA, req)
		Expect(err).NotTo(HaveOccurred(), "CreateJob")
		jobID := resp.Msg.JobId

		defer func() {
			stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = svc.Stop(stopCtx)
		}()

		// Poll to completion.
		deadline := time.Now().Add(90 * time.Second)
		var finalStatus JobStatus
		for time.Now().Before(deadline) {
			job, err := store.GetJob(ctxA, jobID)
			Expect(err).NotTo(HaveOccurred(), "GetJob")
			finalStatus = job.Status
			if finalStatus == JobStatusCompleted || finalStatus == JobStatusFailed {
				Expect(finalStatus).NotTo(Equal(JobStatusFailed), "job failed: %s", job.ErrorMessage)
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
		Expect(finalStatus).To(Equal(JobStatusCompleted), "job did not complete within deadline")

		// --- Assertions (owner reads via the tenant-scoped, RLS-enforced conn) ---

		// 1. analysis_results has a row for every enabled analyzer.
		for _, analyzerName := range []string{"classify", "embedding", "artifacts", "requires", "relationship"} {
			n := e2eScalar(tenantConn, ctxA,
				"SELECT count(*) FROM analysis_results WHERE job_id = $1 AND analyzer_name = $2",
				jobID, analyzerName)
			Expect(n).To(Equal(1), "analysis_results for %q: want 1 row", analyzerName)
		}

		// 2. requires_candidates and relationship_candidates are populated.
		Expect(countCandidates(tenantConn, ctxA, "requires_candidates", jobID)).NotTo(BeZero(), "requires_candidates: want > 0")
		Expect(countCandidates(tenantConn, ctxA, "relationship_candidates", jobID)).NotTo(BeZero(), "relationship_candidates: want > 0")

		// 3. vote_summaries has rows with non-zero viability (proves synthesis's
		//    persistViability ran against the rows Task 7 created).
		Expect(e2eScalar(tenantConn, ctxA,
			"SELECT count(*) FROM vote_summaries WHERE job_id = $1 AND viability > 0",
			jobID)).NotTo(BeZero(), "vote_summaries with viability > 0: want > 0")

		// 4. embeddings has 2000-dim vectors for the job's catalog/model.
		Expect(e2eScalar(tenantConn, ctxA,
			"SELECT count(*) FROM embeddings WHERE catalog_id = $1 AND model = $2",
			catalogID, e2eModel)).To(Equal(2), "embeddings for catalog/model")
		Expect(e2eScalar(tenantConn, ctxA,
			"SELECT vector_dims(vector) FROM embeddings WHERE catalog_id = $1 AND model = $2 LIMIT 1",
			catalogID, e2eModel)).To(Equal(embeddingDim), "embedding dimensions")

		// 5. A second tenant sees zero rows across all five surfaces (RLS).
		ctxB, err := tenant.WithTenant(ctx, tenantB)
		Expect(err).NotTo(HaveOccurred(), "WithTenant B")
		isolationChecks := []struct {
			table string
			query string
			args  []any
		}{
			{"analysis_results", "SELECT count(*) FROM analysis_results WHERE job_id = $1", []any{jobID}},
			{"requires_candidates", "SELECT count(*) FROM requires_candidates WHERE job_id = $1", []any{jobID}},
			{"relationship_candidates", "SELECT count(*) FROM relationship_candidates WHERE job_id = $1", []any{jobID}},
			{"vote_summaries", "SELECT count(*) FROM vote_summaries WHERE job_id = $1", []any{jobID}},
			{"embeddings", "SELECT count(*) FROM embeddings WHERE catalog_id = $1", []any{catalogID}},
		}
		for _, c := range isolationChecks {
			Expect(e2eScalar(tenantConn, ctxB, c.query, c.args...)).To(BeZero(), "RLS breach: tenant B saw rows in %s", c.table)
		}
	})
})
