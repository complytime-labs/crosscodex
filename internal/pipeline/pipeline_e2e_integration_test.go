//go:build integration

package pipeline

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	crand "crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"connectrpc.com/connect"
	_ "github.com/jackc/pgx/v5/stdlib"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/analysis"
	"github.com/complytime-labs/crosscodex/internal/graph"
	pipelineattestation "github.com/complytime-labs/crosscodex/internal/pipeline/attestation"
	"github.com/complytime-labs/crosscodex/internal/synthesis"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/internal/worker"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/graphdb/memdriver"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/oscal"
	"github.com/complytime-labs/crosscodex/pkg/prompt"
	"github.com/complytime-labs/crosscodex/pkg/resultcache"
	"github.com/complytime-labs/crosscodex/pkg/resultcache/pgstore"
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

// e2eIdenticalStatement and e2eLeafTitle are every seeded leaf control's
// statement and title. The embedding text and the classify prompt hold the
// control's context, title and statement ("[<context>] <title>:
// <statement>"), and the context renders the parent section's title and
// identifier. Leaves under sections with the same title and identifier
// therefore get identical text, so cosine similarity 1.0 makes every pair of
// them a stable candidate and they share one classify cache key. No
// run-unique value may reach the title or the context: the deterministic
// embedder gives different texts a similarity of random sign, which the
// candidate filter drops about half the time.
const (
	e2eIdenticalStatement = "The organization enforces multi-factor authentication for all privileged accounts."
	e2eLeafTitle          = "Privileged Access"
)

// e2eKeyProvider is an ephemeral ECDSA P-256 key provider for the attestation
// generator, mirroring pkg/attestation's test double. The jobs are attested,
// so each completes only once its whole attestation verifies and is stored.
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

// e2eControl is one seeded control row.
type e2eControl struct {
	id, title, statement, class, parentID string
}

// e2eEnv is the durable pipeline under test: a real Postgres (db compose
// profile), an embedded NATS server, real analyzers, a real LLM worker and a
// hermetic fake LLM client. tenantA owns every seeded catalog and job;
// tenantB is an unrelated tenant for the RLS checks.
type e2eEnv struct {
	idb              *integrationDB
	store            *PGStore
	svc              *Service
	tenantA, tenantB string
	ctxA, ctxB       context.Context

	// The result-cache env (e2eOptions.resultCache) only: the store the
	// engine reads and writes, wrapped so a spec can plant a row; the
	// object storage holding the checkpoints; the LLM calls the worker
	// made; and newService, which replaces svc with one attesting with gen
	// over the same database, storage, engine and worker.
	cache      *plantingStore
	storage    storage.Provider
	workerLLM  *countingLLM
	newService func(gen attestation.Generator)

	// The critique env (e2eOptions.critique) only: the graph the graph
	// service materializes tenant A's stage events into.
	graph graphdb.GraphDB
}

// e2eOptions selects what newE2EEnv wires beyond the base pipeline.
type e2eOptions struct {
	// resultCache wires result reuse (#243): a pgstore over the app_user
	// pool for the engine, the pipeline's model probe, and an attestor
	// whose key IDs follow the production scheme, so a checkpoint signed
	// by another key reads as untrusted.
	resultCache bool
	// critique enables the critique stage over a relationship panel that
	// relates every pair, and runs the graph service on the bus over a
	// memdriver graph. The critique panel rejects the verdicts of a pair
	// whose prompt names both critiqueRejectMarkers, and upholds the rest.
	critique bool
	// sharedPanel gives the relationship stage two aliases that answer as
	// one upstream model and wires the panel probe, so the panel diversity
	// check refuses every job.
	sharedPanel bool
}

// critiqueRejectMarkers are the control titles whose pair the critique env's
// critique panel rejects.
var critiqueRejectMarkers = [2]string{"Veto Source Marker", "Veto Target Marker"}

// critiqueLLM is the deterministic LLM, except that relationship prompts get
// an EQUIVALENT verdict and critique prompts naming both
// critiqueRejectMarkers get a REJECT.
type critiqueLLM struct {
	testspecs.DeterministicLLM
}

func (c critiqueLLM) Complete(ctx context.Context, req *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
	resp, err := c.DeterministicLLM.Complete(ctx, req)
	if err != nil {
		return nil, err
	}
	switch req.PromptName {
	case "relationship":
		resp.Choices[0].Message.Content = "RELATIONSHIP: EQUIVALENT\nJUSTIFICATION: the same obligation\nSANITY_CHECK: PASS\nCONFIDENCE: HIGH"
	case "critique":
		var text strings.Builder
		for _, m := range req.Messages {
			text.WriteString(m.Content)
		}
		if strings.Contains(text.String(), critiqueRejectMarkers[0]) && strings.Contains(text.String(), critiqueRejectMarkers[1]) {
			resp.Choices[0].Message.Content = "VERDICT: REJECT\nREASON: the controls govern different activities"
		}
	}
	return resp, nil
}

// sharedModelLLM is the deterministic LLM answering every alias as one
// upstream model, as a gateway routing two aliases to one deployment does.
type sharedModelLLM struct {
	testspecs.DeterministicLLM
}

func (c sharedModelLLM) Complete(ctx context.Context, req *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
	resp, err := c.DeterministicLLM.Complete(ctx, req)
	if err != nil {
		return nil, err
	}
	resp.Model = "deterministic/shared-upstream"
	return resp, nil
}

// countingLLM is the env's fake LLM, counting the calls it answers.
type countingLLM struct {
	llmclient.Client
	calls atomic.Int64
}

func (c *countingLLM) Complete(ctx context.Context, req *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
	c.calls.Add(1)
	return c.Client.Complete(ctx, req)
}

func (c *countingLLM) Embed(ctx context.Context, req *llmclient.EmbeddingRequest) (*llmclient.EmbeddingResponse, error) {
	c.calls.Add(1)
	return c.Client.Embed(ctx, req)
}

// plantingStore is the result cache store, except that while plant is set
// its first Put of analyzer plant.analyzer also puts, through the same
// app_user pool, a write the stage never made into the stage's live
// attempt: the O2 attack of someone holding app_user's credentials.
type plantingStore struct {
	resultcache.Store
	mu    sync.Mutex
	plant *plantSpec
}

// plantSpec is one planned plant: the analyzer to plant into, and the
// entry the plant put once done.
type plantSpec struct {
	analyzer string
	planted  *resultcache.Entry
}

func (p *plantingStore) Put(ctx context.Context, e resultcache.Entry) error {
	if err := p.Store.Put(ctx, e); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.plant == nil || p.plant.planted != nil || e.Analyzer != p.plant.analyzer {
		return nil
	}
	planted := e
	planted.TaskID = "planted-task"
	sum := sha256.Sum256([]byte("planted " + e.CacheKey))
	planted.CacheKey = hex.EncodeToString(sum[:])
	if err := p.Store.Put(ctx, planted); err != nil {
		return fmt.Errorf("plantingStore: plant: %w", err)
	}
	p.plant.planted = &planted
	return nil
}

// planted returns the entry the plant put, or nil.
func (p *plantingStore) planted() *resultcache.Entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.plant == nil {
		return nil
	}
	return p.plant.planted
}

// newE2EEnv builds the pipeline and registers its teardown with
// DeferCleanup, so it lives until the calling BeforeAll's container ends.
func newE2EEnv(opts e2eOptions) *e2eEnv {
	ctx := context.Background()
	idb := openIntegrationDB(ctx, 4)
	tenantConn := idb.tenantConn

	// vectordb writes go through a superuser *sql.DB: EmbeddingAnalyzer.Aggregate
	// calls StoreBatch, which writes tenant_id explicitly but does not set
	// app.current_tenant, so an app_user connection would be blocked by the
	// embeddings RLS WITH CHECK. The superuser bypasses RLS, matching how the
	// candidate-generation integration test seeds embeddings.
	vecDB, err := sql.Open("pgx", idb.suDSN)
	Expect(err).NotTo(HaveOccurred(), "open vectordb connection")
	DeferCleanup(vecDB.Close)
	vectors, err := vectordb.NewPgVectorStore(vecDB)
	Expect(err).NotTo(HaveOccurred(), "create pgvector store")

	env := &e2eEnv{
		idb:     idb,
		tenantA: fmt.Sprintf("e2e-a-%s", uuid.New().String()),
		tenantB: fmt.Sprintf("e2e-b-%s", uuid.New().String()),
	}
	Expect(db.EnsureTenant(ctx, idb.suPool, env.tenantA, "e2e-a")).To(Succeed(), "ensure tenant A")
	Expect(db.EnsureTenant(ctx, idb.suPool, env.tenantB, "e2e-b")).To(Succeed(), "ensure tenant B")
	env.ctxA, err = tenant.WithTenant(ctx, env.tenantA)
	Expect(err).NotTo(HaveOccurred(), "WithTenant A")
	env.ctxB, err = tenant.WithTenant(ctx, env.tenantB)
	Expect(err).NotTo(HaveOccurred(), "WithTenant B")

	// Embedded NATS server (in-process JetStream). Shared by the engine's
	// dispatcher/collector, the pipeline's event publishing, and the worker.
	bus, err := natsbus.New(config.NATSConfig{
		URL:      "",
		Embedded: config.NATSEmbeddedConfig{StoreDir: GinkgoT().TempDir()},
	})
	Expect(err).NotTo(HaveOccurred(), "create embedded NATS")
	DeferCleanup(bus.Close)

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
			Enabled: true, Models: []string{e2eModel},
			MaxSourceChars: 10000, MaxTargetChars: 10000, MaxTokens: 512,
			SamplesPerModel: 1, SamplingTemperature: 0, ActionableTypes: []string{"related"},
			ConfidenceWeights: config.RelationshipConfidenceWeights{High: 1.0, Medium: 0.67, Low: 0.33},
		},
		Candidates: config.CandidateConfig{
			Generators:           []config.CandidateGeneratorEntry{{Name: "semantic", Enabled: true, Weight: 1.0}},
			MinEmbeddingCoverage: 0.8,
			MaxPerSource:         10,
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

	fakeLLM := testspecs.DeterministicLLM{EmbeddingDim: embeddingDim}
	var workerLLM llmclient.Client = fakeLLM
	if opts.critique {
		analysisConfig.Critique = config.CritiqueConfig{
			Enabled: true, Models: []string{e2eModel}, SamplesPerModel: 1, MaxTokens: 512,
			ConsensusThreshold: 0.66, MaxErrorRate: 0.5, MaxSourceChars: 10000, MaxTargetChars: 10000,
		}
		workerLLM = critiqueLLM{DeterministicLLM: fakeLLM}
	}
	if opts.sharedPanel {
		analysisConfig.Relationship.Models = []string{"e2e-panel-1", "e2e-panel-2"}
	}

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

	storageProvider, err := storage.NewLocal(GinkgoT().TempDir(), env.tenantA)
	Expect(err).NotTo(HaveOccurred(), "create local storage")
	env.storage = storageProvider

	env.store = NewPGStore(tenantConn, idb.suPool)

	registry, err := NewProductionRegistry(fakeLLM, vectors, storageProvider, prompts, tenantConn, analysisConfig, nil, nil)
	Expect(err).NotTo(HaveOccurred(), "build production registry")

	taskTypes := map[string]natsbus.TaskType{
		"classify":     natsbus.TaskClassify,
		"embedding":    natsbus.TaskEmbed,
		"artifacts":    natsbus.TaskArtifacts,
		"requires":     natsbus.TaskRequires,
		"relationship": natsbus.TaskRelate,
		"critique":     natsbus.TaskCritique,
	}

	dbReporter := NewDBStageReporter(analysis.NewNATSStageReporter(bus), env.store)
	engineOpts := []analysis.Option{analysis.WithStageReporter(dbReporter), analysis.WithStageTimeout(time.Hour)}
	if opts.resultCache {
		cacheStore, err := pgstore.New(tenantConn)
		Expect(err).NotTo(HaveOccurred(), "create result cache store")
		env.cache = &plantingStore{Store: cacheStore}
		engineOpts = append(engineOpts, analysis.WithResultCache(env.cache))
	}
	engine := analysis.NewWithNATS(registry, bus, analysisConfig.Engine, taskTypes, engineOpts...)

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

	var keys attestation.KeyProvider = newE2EKeyProvider()
	if opts.resultCache {
		keys, err = attestation.NewEphemeralKeyProvider()
		Expect(err).NotTo(HaveOccurred(), "create attestation key pair")
	}
	attestor, err := attestation.NewGenerator(keys)
	Expect(err).NotTo(HaveOccurred(), "create attestation generator")
	converter := pipelineattestation.NewConverter()

	pipelineCfg := config.PipelineConfig{MaxConcurrentJobs: 10}
	llmCfg := config.LLMConfig{DefaultModel: e2eModel, EmbeddingModel: e2eModel}

	env.newService = func(gen attestation.Generator) {
		if env.svc != nil {
			stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			Expect(env.svc.Stop(stopCtx)).To(Succeed(), "stop the replaced service")
		}
		svcOpts := []Option{
			WithCatalogControlsReader(NewPGCatalogControlsReader(tenantConn)),
			WithCatalogNameReader(NewPGCatalogNameReader(tenantConn)),
			WithEmbeddingsReader(NewPGEmbeddingsReader(tenantConn)),
			WithEmbeddingConfig(analysisConfig.Embedding),
		}
		if opts.resultCache {
			svcOpts = append(svcOpts, WithResultCache(env.cache, config.ResultCacheConfig{Enabled: true, LookupBatchSize: 1000}, llmCfg, fakeLLM))
		}
		if opts.sharedPanel {
			svcOpts = append(svcOpts, WithPanelProbe(sharedModelLLM{DeterministicLLM: fakeLLM}))
		}
		env.svc = New(
			env.store, engine, registry, synth, Attestors{Default: &Attestor{Generator: gen, ExpiresIn: 168 * time.Hour}},
			converter, bus, storageProvider, pipelineCfg, candGen, svcOpts...,
		)
	}
	env.newService(attestor)

	// Real worker consuming LLM tasks off the shared bus.
	env.workerLLM = &countingLLM{Client: workerLLM}
	w := worker.New(bus, env.workerLLM, config.WorkerConfig{
		MaxAbandonedCollections: 10,
		MaxInFlight:             1,
		LLM:                     llmCfg,
	}, worker.WithAbandonTTL(time.Hour))
	Expect(w.Start(context.Background())).To(Succeed(), "start worker")
	DeferCleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := w.Stop(stopCtx); err != nil {
			GinkgoWriter.Printf("worker stop: %v\n", err)
		}
	})
	// Registered after the worker's cleanup, so (cleanups run LIFO) the
	// service stops before the worker, as the deferred calls did before.
	DeferCleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = env.svc.Stop(stopCtx)
	})

	if opts.critique {
		// The graph service as the daemon runs it: subscribed to the stage
		// events on the bus, resolving results through the PG resolver.
		env.graph = memdriver.New()
		Expect(env.graph.CreateGraph(ctx, env.tenantA)).To(Succeed(), "create tenant A's graph")
		resolvers := graph.NewResolverRegistry()
		resolvers.Register(graph.NewPGResolver(tenantConn))
		graphSvc := graph.New(env.graph, nil, bus, graph.WithResolverRegistry(resolvers))
		Expect(graphSvc.Start(ctx)).To(Succeed(), "start the graph service")
		DeferCleanup(func() {
			stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = graphSvc.Stop(stopCtx)
		})
	}

	return env
}

// semanticMatches returns tenant A's SEMANTIC_MATCH edges of jobID written
// at stage, by "<source>><target>".
func (env *e2eEnv) semanticMatches(g Gomega, jobID, stage string) map[string]graphdb.Edge {
	rels, err := env.graph.QueryRelationships(env.ctxA, env.tenantA, graphdb.RelationshipQuery{
		EdgeLabel:  results.SemanticMatchEdgeLabel,
		Properties: map[string]any{"job_id": jobID, results.StageProperty: stage},
	})
	g.Expect(err).NotTo(HaveOccurred())
	out := map[string]graphdb.Edge{}
	for _, r := range rels {
		out[r.Source.ID+">"+r.Target.ID] = r.Edge
	}
	return out
}

// seedCatalog writes catalogID, named name, and its controls under tenant A
// (superuser: RLS bypassed, tenant_id explicit). Each control's identifier is
// its ID without the "<catalogID>/" prefix, as import stores it, so contexts
// carry no catalog ID. Offset 1 so a failure is reported at the caller's
// line.
func (env *e2eEnv) seedCatalog(catalogID, name string, controls []e2eControl) {
	ctx := context.Background()
	ExpectWithOffset(1, env.idb.suPool.Exec(ctx, `
		INSERT INTO catalogs (catalog_id, tenant_id, name, version, source_type, object_path)
		VALUES ($1, $2, $3, 'v1', 'test', 'unused')
	`, catalogID, env.tenantA, name)).To(Succeed(), "seed catalog %s", catalogID)
	for _, c := range controls {
		ExpectWithOffset(1, env.idb.suPool.Exec(ctx, `
			INSERT INTO controls (tenant_id, control_id, catalog_id, identifier, title, statement, class, parent_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''))
		`, env.tenantA, c.id, catalogID, strings.TrimPrefix(c.id, catalogID+"/"), c.title, c.statement, c.class, c.parentID)).
			To(Succeed(), "seed control %s", c.id)
	}
}

// seedAlikeCatalogs seeds a source and a target catalog under tenant A and
// returns their IDs. Every leaf in both has the same context, title and
// statement, so every pair, within a catalog too, is maximally similar: only
// the job's catalogs and direction decide which pairs are generated.
func (env *e2eEnv) seedAlikeCatalogs() (src, tgt string) {
	src, tgt = "cat-src-"+uuid.New().String(), "cat-tgt-"+uuid.New().String()
	for _, cat := range []struct{ id, name string }{{src, "E2E Source Framework"}, {tgt, "E2E Target Framework"}} {
		env.seedCatalog(cat.id, cat.name, []e2eControl{
			{cat.id + "/section", "Access Control", "Access control family.", oscal.ClassSection, ""},
			{cat.id + "/c1", e2eLeafTitle, e2eIdenticalStatement, "SC", "section"},
			{cat.id + "/c2", e2eLeafTitle, e2eIdenticalStatement, "SC", "section"},
		})
	}
	return src, tgt
}

// runJob creates a job with cfg under tenant A's context and waits for it to
// complete, failing the spec if it fails or does not finish in time. It
// returns the job's ID. Offset 1 so a failure is reported at the caller's line.
func (env *e2eEnv) runJob(cfg *pb.JobConfig) string {
	resp, err := env.svc.CreateJob(env.ctxA, connect.NewRequest(&pb.CreateJobRequest{Config: cfg}))
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "CreateJob")
	jobID := resp.Msg.JobId
	EventuallyWithOffset(1, func(g Gomega) {
		job, err := env.store.GetJob(env.ctxA, jobID)
		g.Expect(err).NotTo(HaveOccurred(), "GetJob")
		g.Expect(job.Status).NotTo(Equal(JobStatusFailed), "job failed: %s", job.ErrorMessage)
		g.Expect(job.Status).To(Equal(JobStatusCompleted))
	}, 90*time.Second, 250*time.Millisecond).Should(Succeed(), "job %s did not complete", jobID)
	return jobID
}

// pairsBetween counts the job's rows in table whose source control belongs
// to fromCatalog and whose target control belongs to toCatalog, read through
// the tenant-scoped connection. table is one of the fixed names below.
func (env *e2eEnv) pairsBetween(table, jobID, fromCatalog, toCatalog string) int {
	switch table {
	case "vote_summaries", "relationship_candidates", "requires_candidates":
	default:
		Fail(fmt.Sprintf("unsupported table %q", table), 1)
	}
	return e2eScalar(env.idb.tenantConn, env.ctxA, `
		SELECT count(*) FROM `+table+` p
		JOIN controls s ON s.tenant_id = p.tenant_id AND s.control_id = p.source_id
		JOIN controls t ON t.tenant_id = p.tenant_id AND t.control_id = p.target_id
		WHERE p.job_id = $1 AND s.catalog_id = $2 AND t.catalog_id = $3
	`, jobID, fromCatalog, toCatalog)
}

// TestPipelineE2E exercises the entire durable pipeline. Each spec seeds its
// own catalogs, runs one job to completion through pipeline.Service.CreateJob,
// and asserts on the persistence surfaces for the owning tenant, isolated
// from a second tenant by RLS.
var _ = Describe("Pipeline end-to-end", Ordered, func() {
	var env *e2eEnv

	BeforeAll(func() {
		env = newE2EEnv(e2eOptions{})
	})

	It("runs a job to completion and populates every durable surface, isolated per tenant", func() {
		catalogID := "cat-" + uuid.New().String()

		// section-1 is a compliance-section (classify/embedding auto-skip it,
		// candidate generation excludes it). ctrl-1 and ctrl-2 are its leaf
		// children with identical titles and statements, which also
		// exercises the context path (both take the section's title as
		// context).
		env.seedCatalog(catalogID, "E2E Catalog", []e2eControl{
			{catalogID + "/section-1", "Access Control", "Access control family.", oscal.ClassSection, ""},
			{catalogID + "/ctrl-1", e2eLeafTitle, e2eIdenticalStatement, "SC", "section-1"},
			{catalogID + "/ctrl-2", e2eLeafTitle, e2eIdenticalStatement, "SC", "section-1"},
		})

		jobID := env.runJob(&pb.JobConfig{CatalogId: catalogID, TargetCatalogId: catalogID})
		tenantConn, ctxA := env.idb.tenantConn, env.ctxA

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
			Expect(e2eScalar(tenantConn, env.ctxB, c.query, c.args...)).To(BeZero(), "RLS breach: tenant B saw rows in %s", c.table)
		}
	})

	It("maps a source catalog onto a target catalog with no pair inside either catalog", func() {
		src, tgt := env.seedAlikeCatalogs()

		jobID := env.runJob(&pb.JobConfig{CatalogId: src, TargetCatalogId: tgt})
		tenantConn, ctxA := env.idb.tenantConn, env.ctxA

		// The per-control analyzers covered both catalogs: each leaf is
		// classified, and embedded under its own catalog.
		for _, cat := range []string{src, tgt} {
			Expect(e2eScalar(tenantConn, ctxA,
				"SELECT count(*) FROM embeddings WHERE catalog_id = $1 AND model = $2", cat, e2eModel)).To(Equal(2), "embeddings of %s", cat)
		}
		var classifyBlob []byte
		tx, err := tenantConn.Begin(ctxA)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = tx.Rollback() }()
		Expect(tx.QueryRow(ctxA, "SELECT result_data FROM analysis_results WHERE job_id = $1 AND analyzer_name = 'classify'", jobID).Scan(&classifyBlob)).To(Succeed())
		Expect(tx.Commit()).To(Succeed())
		var classified []results.ClassifyResult
		Expect(json.Unmarshal(classifyBlob, &classified)).To(Succeed())
		classifiedIDs := make([]string, len(classified))
		for i, c := range classified {
			classifiedIDs[i] = c.ControlID
		}
		Expect(classifiedIDs).To(ContainElements(src+"/c1", src+"/c2", tgt+"/c1", tgt+"/c2"))

		// Mappings (the rows the graph service writes as SEMANTIC_MATCH and
		// REQUIRES edges) and both candidate tables run source to target only.
		for _, table := range []string{"vote_summaries", "relationship_candidates", "requires_candidates"} {
			Expect(env.pairsBetween(table, jobID, src, tgt)).NotTo(BeZero(), "%s: want source-to-target pairs", table)
			Expect(env.pairsBetween(table, jobID, src, src)).To(BeZero(), "%s: a pair within the source catalog", table)
			Expect(env.pairsBetween(table, jobID, tgt, tgt)).To(BeZero(), "%s: a pair within the target catalog", table)
			Expect(env.pairsBetween(table, jobID, tgt, src)).To(BeZero(), "%s: a target-to-source pair without bidirectional", table)
		}
		Expect(e2eScalar(tenantConn, ctxA,
			"SELECT count(*) FROM vote_summaries WHERE job_id = $1 AND analyzer_name IN ('relationship', 'requires')", jobID)).NotTo(BeZero(),
			"vote_summaries: want relationship or requires rows")

		// RLS: tenant B sees none of the crosswalk.
		Expect(e2eScalar(tenantConn, env.ctxB, "SELECT count(*) FROM vote_summaries WHERE job_id = $1", jobID)).To(BeZero(), "RLS breach: tenant B saw the crosswalk")
	})

	It("maps both catalogs onto each other with bidirectional, still with no pair inside either catalog", func() {
		src, tgt := env.seedAlikeCatalogs()

		jobID := env.runJob(&pb.JobConfig{CatalogId: src, TargetCatalogId: tgt, Bidirectional: true})

		for _, table := range []string{"vote_summaries", "relationship_candidates", "requires_candidates"} {
			Expect(env.pairsBetween(table, jobID, src, tgt)).NotTo(BeZero(), "%s: want source-to-target pairs", table)
			Expect(env.pairsBetween(table, jobID, tgt, src)).NotTo(BeZero(), "%s: want target-to-source pairs", table)
			Expect(env.pairsBetween(table, jobID, src, src)).To(BeZero(), "%s: a pair within the source catalog", table)
			Expect(env.pairsBetween(table, jobID, tgt, tgt)).To(BeZero(), "%s: a pair within the target catalog", table)
		}
	})
})

var _ = Describe("Pipeline end-to-end with the critique stage", Ordered, func() {
	var env *e2eEnv

	BeforeAll(func() {
		env = newE2EEnv(e2eOptions{critique: true})
	})

	It("writes no SEMANTIC_MATCH edge for the pair the critique vetoed and writes its siblings' with their votes", func() {
		src, tgt := "cat-src-"+uuid.New().String(), "cat-tgt-"+uuid.New().String()
		env.seedCatalog(src, "E2E Critique Source", []e2eControl{
			{src + "/section", "Access Control", "Access control family.", oscal.ClassSection, ""},
			{src + "/c1", "Account management", "Manage system accounts.", "SC", "section"},
			{src + "/c2", critiqueRejectMarkers[0], "Review audit records.", "SC", "section"},
		})
		env.seedCatalog(tgt, "E2E Critique Target", []e2eControl{
			{tgt + "/section", "Access Control", "Access control family.", oscal.ClassSection, ""},
			{tgt + "/c1", "Account management", "Manage system accounts.", "SC", "section"},
			{tgt + "/c2", critiqueRejectMarkers[1], "Generate audit records.", "SC", "section"},
		})
		// The graph holds the controls' nodes, as the catalog projection
		// writes them on import, so edges between them can be written.
		for _, cat := range []string{src, tgt} {
			for _, c := range []string{"/c1", "/c2"} {
				Expect(env.graph.CreateNode(env.ctxA, env.tenantA, graphdb.Node{ID: cat + c, Label: "Control", ValidFrom: time.Now().UTC()})).To(Succeed())
			}
		}
		vetoed := src + "/c2>" + tgt + "/c2"
		siblings := []string{src + "/c1>" + tgt + "/c1", src + "/c1>" + tgt + "/c2", src + "/c2>" + tgt + "/c1"}

		jobID := env.runJob(&pb.JobConfig{CatalogId: src, TargetCatalogId: tgt})

		stages, err := env.store.GetStages(env.ctxA, jobID)
		Expect(err).NotTo(HaveOccurred())
		Expect(stages).To(ContainElement(SatisfyAll(HaveField("StageName", results.CritiqueAnalyzer), HaveField("Status", StageStatusCompleted))))

		// The synthesis event is the job's last: once its edges are in, every
		// earlier event has been materialized.
		Eventually(func(g Gomega) {
			g.Expect(env.semanticMatches(g, jobID, results.StageSynthesized)).To(HaveLen(len(siblings)))
		}, 30*time.Second, 250*time.Millisecond).Should(Succeed())

		analyzerEdges := env.semanticMatches(Default, jobID, results.StageAnalyzer)
		Expect(analyzerEdges).NotTo(HaveKey(vetoed))
		Expect(env.semanticMatches(Default, jobID, results.StageSynthesized)).NotTo(HaveKey(vetoed))
		for _, pair := range siblings {
			Expect(analyzerEdges).To(HaveKey(pair))
			// Written by the critique event, not the deferred relationship one.
			Expect(analyzerEdges[pair].Properties).To(HaveKeyWithValue(results.CritiqueUpheldProperty, BeNumerically("==", 1)), pair)
		}
		Expect(e2eScalar(env.idb.tenantConn, env.ctxA,
			"SELECT count(*) FROM vote_summaries WHERE job_id = $1 AND source_id = $2 AND target_id = $3", jobID, src+"/c2", tgt+"/c2")).
			To(BeZero(), "synthesis received the vetoed pair")
	})
})

var _ = Describe("Pipeline end-to-end with a shared-model panel", Ordered, func() {
	var env *e2eEnv

	BeforeAll(func() {
		env = newE2EEnv(e2eOptions{sharedPanel: true})
	})

	It("fails the job at the panel diversity check before any analysis call", func() {
		src, tgt := env.seedAlikeCatalogs()
		resp, err := env.svc.CreateJob(env.ctxA, connect.NewRequest(&pb.CreateJobRequest{Config: &pb.JobConfig{CatalogId: src, TargetCatalogId: tgt}}))
		Expect(err).NotTo(HaveOccurred())
		var job *Job
		Eventually(func(g Gomega) {
			job, err = env.store.GetJob(env.ctxA, resp.Msg.JobId)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(job.Status).To(Equal(JobStatusFailed))
		}, 90*time.Second, 250*time.Millisecond).Should(Succeed())
		Expect(job.ErrorMessage).To(ContainSubstring("panel aliases e2e-panel-1 and e2e-panel-2 both answered as deterministic/shared-upstream"))
		Expect(job.ErrorMessage).To(ContainSubstring("analysis.relationship.allow_shared_panel_models"))
		Expect(env.workerLLM.calls.Load()).To(BeZero(), "no analysis call reached the worker")
	})
})
