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
	"testing"
	"time"

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

func newE2EKeyProvider(t *testing.T) *e2eKeyProvider {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), crand.Reader)
	if err != nil {
		t.Fatalf("generate attestation key: %v", err)
	}
	return &e2eKeyProvider{key: key}
}

func (p *e2eKeyProvider) SigningKey(_ context.Context) (crypto.Signer, error) {
	return p.key, nil
}

func (p *e2eKeyProvider) VerificationKey(_ context.Context) (crypto.PublicKey, error) {
	return p.key.Public(), nil
}

func (p *e2eKeyProvider) KeyID(_ context.Context) (string, error) { return "e2e-key-id", nil }

// TestPipelineE2E exercises the entire durable pipeline against a real
// Postgres (db compose profile), an embedded NATS server, real analyzers, a
// real LLM worker, and a hermetic fake LLM client. It seeds a small catalog,
// runs one job to completion through pipeline.Service.CreateJob, and asserts
// that every persistence surface this plan introduced was populated for the
// owning tenant and is isolated from a second tenant by RLS.
func TestPipelineE2E(t *testing.T) {
	ctx := context.Background()

	suDSN := os.Getenv("TEST_DATABASE_DSN")
	if suDSN == "" {
		t.Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
	}

	// Migrate (idempotent).
	migrator, err := db.NewMigrator(suDSN)
	if err != nil {
		t.Fatalf("create migrator: %v", err)
	}
	if err := migrator.Up(ctx); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	if err := migrator.Close(); err != nil {
		t.Fatalf("close migrator: %v", err)
	}

	// Ensure app_user password (idempotent).
	adminDB, err := sql.Open("pgx", suDSN)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	if _, err := adminDB.ExecContext(ctx, "ALTER ROLE app_user WITH PASSWORD 'apppass'"); err != nil {
		t.Fatalf("set app_user password: %v", err)
	}
	if err := adminDB.Close(); err != nil {
		t.Fatalf("close admin connection: %v", err)
	}

	// Superuser pool for RLS-bypassing seed writes and verification reads.
	suPool, err := db.NewPool(db.PoolConfig{
		DSN:          suDSN,
		MaxOpenConns: 5,
		Extensions:   []string{"age", "vector"},
	})
	if err != nil {
		t.Fatalf("create superuser pool: %v", err)
	}
	defer suPool.Close()

	// app_user pool -> tenant-scoped connection (RLS enforced).
	appDSN, err := candidateITAppUserDSN(suDSN)
	if err != nil {
		t.Fatalf("build app_user DSN: %v", err)
	}
	appPool, err := db.NewPool(db.PoolConfig{DSN: appDSN, MaxOpenConns: 4})
	if err != nil {
		t.Fatalf("create app_user pool: %v", err)
	}
	defer appPool.Close()
	tenantConn := db.NewTenantPool(appPool)

	// vectordb writes go through a superuser *sql.DB: EmbeddingAnalyzer.Aggregate
	// calls StoreBatch, which writes tenant_id explicitly but does not set
	// app.current_tenant, so an app_user connection would be blocked by the
	// embeddings RLS WITH CHECK. The superuser bypasses RLS, matching how the
	// candidate-generation integration test seeds embeddings.
	vecDB, err := sql.Open("pgx", suDSN)
	if err != nil {
		t.Fatalf("open vectordb connection: %v", err)
	}
	defer vecDB.Close()
	vectors, err := vectordb.NewPgVectorStore(vecDB)
	if err != nil {
		t.Fatalf("create pgvector store: %v", err)
	}

	// Two tenants: the owner (A) and an unrelated tenant (B) for the RLS check.
	tenantA := fmt.Sprintf("e2e-a-%s", uuid.New().String())
	tenantB := fmt.Sprintf("e2e-b-%s", uuid.New().String())
	if err := db.EnsureTenant(ctx, suPool, tenantA, "e2e-a"); err != nil {
		t.Fatalf("ensure tenant A: %v", err)
	}
	if err := db.EnsureTenant(ctx, suPool, tenantB, "e2e-b"); err != nil {
		t.Fatalf("ensure tenant B: %v", err)
	}

	catalogID := "cat-" + uuid.New().String()

	// Seed the catalog and its controls under tenant A. section-1 is a
	// compliance-section (classify/embedding auto-skip it, candidate generation
	// excludes it). c1 and c2 are its leaf children with identical statement
	// text, so their prepared embedding text — "[<parent title>] <statement>" —
	// is identical, yielding cosine similarity 1.0 and a stable candidate pair.
	// This also exercises the ancestor_title path (both children inherit the
	// section's title).
	if err := suPool.Exec(ctx, `
		INSERT INTO catalogs (catalog_id, tenant_id, name, version, source_type, object_path)
		VALUES ($1, $2, 'E2E Catalog', 'v1', 'test', 'unused')
	`, catalogID, tenantA); err != nil {
		t.Fatalf("seed catalog: %v", err)
	}

	const identicalStatement = "The organization enforces multi-factor authentication for all privileged accounts."
	controls := []struct {
		id, title, statement, class, parentID string
	}{
		{"section-1", "Access Control", "Access control family.", classifySectionClass, ""},
		{"ctrl-1", "Control One", identicalStatement, "SC", "section-1"},
		{"ctrl-2", "Control Two", identicalStatement, "SC", "section-1"},
	}
	for _, c := range controls {
		if err := suPool.Exec(ctx, `
			INSERT INTO controls (tenant_id, control_id, catalog_id, identifier, title, statement, class, parent_id)
			VALUES ($1, $2, $3, $2, $4, $5, $6, NULLIF($7, ''))
		`, tenantA, c.id, catalogID, c.title, c.statement, c.class, c.parentID); err != nil {
			t.Fatalf("seed control %s: %v", c.id, err)
		}
	}

	// Embedded NATS server (in-process JetStream). Shared by the engine's
	// dispatcher/collector, the pipeline's event publishing, and the worker.
	bus, err := natsbus.New(config.NATSConfig{
		URL:      "",
		Embedded: config.NATSEmbeddedConfig{StoreDir: t.TempDir()},
	})
	if err != nil {
		t.Fatalf("create embedded NATS: %v", err)
	}
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
	if err != nil {
		t.Fatalf("create prompt registry: %v", err)
	}

	storageProvider, err := storage.NewLocal(t.TempDir(), tenantA)
	if err != nil {
		t.Fatalf("create local storage: %v", err)
	}

	store := NewPGStore(tenantConn, suPool)

	registry, err := NewProductionRegistry(fakeLLM, vectors, storageProvider, prompts, tenantConn, analysisConfig)
	if err != nil {
		t.Fatalf("build production registry: %v", err)
	}

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
	candRegistry, err := BuildCandidateRegistry(analysisConfig.Candidates)
	if err != nil {
		t.Fatalf("build candidate registry: %v", err)
	}
	candGen := NewCandidateGenerator(
		NewPGControlsReader(tenantConn),
		NewPGEmbeddingsReader(tenantConn),
		candRegistry,
		NewPGCandidateWriter(tenantConn),
		analysisConfig.Candidates,
		e2eModel,
	)

	synth := synthesis.New(tenantConn, config.SynthesisConfig{}, []string{"requires"})

	attestor, err := attestation.NewGenerator(newE2EKeyProvider(t))
	if err != nil {
		t.Fatalf("create attestation generator: %v", err)
	}
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
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("start worker: %v", err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := w.Stop(stopCtx); err != nil {
			t.Logf("worker stop: %v", err)
		}
	}()

	// Create and launch the job under tenant A's context. CreateJob persists the
	// job + stages and runs executeJob in its own goroutine.
	ctxA, err := tenant.WithTenant(ctx, tenantA)
	if err != nil {
		t.Fatalf("WithTenant A: %v", err)
	}
	req := connect.NewRequest(&pb.CreateJobRequest{
		Config: &pb.JobConfig{Source: &pb.JobConfig_CatalogId{CatalogId: catalogID}},
	})
	resp, err := svc.CreateJob(ctxA, req)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
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
		if err != nil {
			t.Fatalf("GetJob: %v", err)
		}
		finalStatus = job.Status
		if finalStatus == JobStatusCompleted || finalStatus == JobStatusFailed {
			if finalStatus == JobStatusFailed {
				t.Fatalf("job failed: %s", job.ErrorMessage)
			}
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if finalStatus != JobStatusCompleted {
		t.Fatalf("job did not complete within deadline (last status %q)", finalStatus)
	}

	// --- Assertions (owner reads via the tenant-scoped, RLS-enforced conn) ---

	// 1. analysis_results has a row for every enabled analyzer.
	for _, analyzerName := range []string{"classify", "embedding", "artifacts", "requires", "relationship"} {
		n := e2eScalar(t, tenantConn, ctxA,
			"SELECT count(*) FROM analysis_results WHERE job_id = $1 AND analyzer_name = $2",
			jobID, analyzerName)
		if n != 1 {
			t.Errorf("analysis_results for %q: got %d rows, want 1", analyzerName, n)
		}
	}

	// 2. requires_candidates and relationship_candidates are populated.
	if n := countCandidates(t, tenantConn, ctxA, "requires_candidates", jobID); n == 0 {
		t.Errorf("requires_candidates: got 0 rows, want > 0")
	}
	if n := countCandidates(t, tenantConn, ctxA, "relationship_candidates", jobID); n == 0 {
		t.Errorf("relationship_candidates: got 0 rows, want > 0")
	}

	// 3. vote_summaries has rows with non-zero viability (proves synthesis's
	//    persistViability ran against the rows Task 7 created).
	if n := e2eScalar(t, tenantConn, ctxA,
		"SELECT count(*) FROM vote_summaries WHERE job_id = $1 AND viability > 0",
		jobID); n == 0 {
		t.Errorf("vote_summaries with viability > 0: got 0 rows, want > 0")
	}

	// 4. embeddings has 2000-dim vectors for the job's catalog/model.
	if n := e2eScalar(t, tenantConn, ctxA,
		"SELECT count(*) FROM embeddings WHERE catalog_id = $1 AND model = $2",
		catalogID, e2eModel); n != 2 {
		t.Errorf("embeddings for catalog/model: got %d rows, want 2", n)
	}
	if dims := e2eScalar(t, tenantConn, ctxA,
		"SELECT vector_dims(vector) FROM embeddings WHERE catalog_id = $1 AND model = $2 LIMIT 1",
		catalogID, e2eModel); dims != embeddingDim {
		t.Errorf("embedding dimensions: got %d, want %d", dims, embeddingDim)
	}

	// 5. A second tenant sees zero rows across all five surfaces (RLS).
	ctxB, err := tenant.WithTenant(ctx, tenantB)
	if err != nil {
		t.Fatalf("WithTenant B: %v", err)
	}
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
		if n := e2eScalar(t, tenantConn, ctxB, c.query, c.args...); n != 0 {
			t.Errorf("RLS breach: tenant B saw %d rows in %s, want 0", n, c.table)
		}
	}
}

// e2eScalar runs a single-integer query through the tenant-scoped connection
// (so RLS applies) inside a transaction, mirroring countCandidates.
func e2eScalar(t *testing.T, conn db.TenantConnection, ctx context.Context, query string, args ...any) int {
	t.Helper()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("e2eScalar begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	if err := tx.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("e2eScalar scan (%s): %v", query, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("e2eScalar commit: %v", err)
	}
	return n
}
