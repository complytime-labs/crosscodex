package pipeline_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/analysis"
	"github.com/complytime-labs/crosscodex/internal/pipeline"
	pipelineattestation "github.com/complytime-labs/crosscodex/internal/pipeline/attestation"
	"github.com/complytime-labs/crosscodex/internal/synthesis"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/jobconfig"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/natsbus/natsbustest"
	"github.com/complytime-labs/crosscodex/pkg/resultcache"
	"github.com/complytime-labs/crosscodex/pkg/storage/storagetest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

type fakeSynthesis struct{}

func (fakeSynthesis) Execute(ctx context.Context, jobID string, inputs []synthesis.SynthesisInput,
	classifications map[string]synthesis.Classification) (*synthesis.ExecuteResult, error) {
	return &synthesis.ExecuteResult{}, nil
}

// blockingSynthesis blocks Execute until the block channel is closed or ctx is cancelled.
type blockingSynthesis struct {
	block chan struct{}
}

func (b *blockingSynthesis) Execute(ctx context.Context, jobID string, inputs []synthesis.SynthesisInput,
	classifications map[string]synthesis.Classification) (*synthesis.ExecuteResult, error) {
	select {
	case <-b.block:
		return &synthesis.ExecuteResult{}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// recordingSynthesis counts Execute calls, so a spec can assert synthesis
// never ran.
type recordingSynthesis struct {
	calls atomic.Int32
}

func (r *recordingSynthesis) Execute(ctx context.Context, jobID string, inputs []synthesis.SynthesisInput,
	classifications map[string]synthesis.Classification) (*synthesis.ExecuteResult, error) {
	r.calls.Add(1)
	return &synthesis.ExecuteResult{}, nil
}

// stuckClassifyJob is a resumed job whose classify stage waits for a worker
// that never answers, so the job is cancelled or stopped mid-stage. Stage
// transitions go through the production DBStageReporter into the store.
// dispatched lists the collection IDs dispatched so far.
type stuckClassifyJob struct {
	svc        *pipeline.Service
	bus        *natsbustest.Fake
	dispatched func() []string
}

// startStuckClassifyJob stores a running job with stages classify, synthesis
// and graph for tenantID (ctx's tenant), starts a Service over store that
// resumes it, and returns once classify has dispatched its task. The
// service is stopped at cleanup.
func startStuckClassifyJob(ctx context.Context, store pipeline.Store, tenantID, jobID string, opts ...pipeline.Option) stuckClassifyJob {
	GinkgoHelper()
	reg := analyzer.NewRegistry()
	classify := newControlStub("classify")
	classify.GenerateWorkFunc = func(_ context.Context, c *pb.Control, _ analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
		return []analyzer.Task{{TaskID: c.GetControlId() + "-task", Payload: &emptypb.Empty{}}}, nil
	}
	Expect(analyzer.Register[*pb.Control](reg, classify)).To(Succeed())
	bus := &natsbustest.Fake{}
	dispatcher := &executorTestDispatcher{}
	eng := analysis.New(reg, dispatcher, analysis.NewNATSCollector(bus),
		config.EngineConfig{TaskTimeout: time.Hour, RetryBackoff: time.Millisecond},
		map[string]natsbus.TaskType{"classify": natsbus.TaskClassify},
		analysis.WithStageTimeout(time.Hour),
		analysis.WithStageReporter(pipeline.NewDBStageReporter(analysis.NewNATSStageReporter(bus), store)))
	opts = append([]pipeline.Option{pipeline.WithCatalogControlsReader(&fakeCatalogControlsReader{controls: []*pb.Control{
		{ControlId: "c1", CatalogId: "cat-1"},
	}})}, opts...)
	svc := pipeline.New(store, eng, reg, &fakeSynthesisExecutor{}, pipeline.Attestors{},
		pipelineattestation.NewConverter(), bus, &storagetest.Fake{},
		config.PipelineConfig{MaxConcurrentJobs: 10, StageTimeout: time.Hour}, nil, opts...)
	DeferCleanup(func() { Expect(svc.Stop(context.Background())).To(Succeed()) })
	Expect(store.CreateJob(ctx, &pipeline.Job{
		JobID: jobID, TenantID: tenantID, Status: pipeline.JobStatusRunning, CreatedBy: tenantID,
		Config: []byte(`{"catalog_id":"cat-1"}`), CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})).To(Succeed())
	Expect(store.CreateStages(ctx, jobID, []string{"classify", "synthesis", "graph"})).To(Succeed())
	Expect(svc.Start(ctx)).To(Succeed())
	dispatched := func() []string {
		dispatcher.mu.Lock()
		defer dispatcher.mu.Unlock()
		var ids []string
		for _, d := range dispatcher.dispatches {
			ids = append(ids, d.collectionID)
		}
		return ids
	}
	Eventually(dispatched).Should(HaveLen(1))
	return stuckClassifyJob{svc: svc, bus: bus, dispatched: dispatched}
}

// publishedStates returns the statuses published on jobID's pipeline state
// subject, in order.
func publishedStates(bus *natsbustest.Fake, tenantID, jobID string) []string {
	GinkgoHelper()
	subject, err := natsbus.PipelineStateSubject(tenantID, jobID)
	Expect(err).NotTo(HaveOccurred())
	var states []string
	for _, p := range bus.Published() {
		if p.Subject != subject {
			continue
		}
		var msg struct {
			Status string `json:"status"`
		}
		Expect(json.Unmarshal(p.Data, &msg)).To(Succeed())
		states = append(states, msg.Status)
	}
	return states
}

// serviceTestCatalogs are the catalogs the Service specs' jobs name, all of
// them catalogs of the specs' tenant.
var serviceTestCatalogs = fakeCatalogNameReader{
	"test-catalog": {name: "Test Catalog"}, "cat-1": {name: "Catalog One", version: "1.0"}, "cat-2": {name: "Catalog Two", version: "2.0"},
}

// recordedFailureLabel introduces a job's stored failure in a refused retry.
const recordedFailureLabel = "It failed with (as recorded then; a command in it may predate this version): "

// emptyTargetRemedy ends a refused retry of a job whose target catalog has
// no control with a statement, before the new-job command.
const emptyTargetRemedy = "Importing the target again with control statements gives it a new catalog ID, " +
	"so start a new job with that catalog or another target: "

// recordedCallKey marks the context of the call whose catalog lookups a
// recordingCatalogNameReader records.
type recordedCallKey struct{}

// recordingCatalogNameReader serves names and records, as
// "<tenant>/<catalog>", each lookup made under a context marked with
// recordedCallKey. The job a CreateJob starts reads the names again in the
// background under its own context, which stays unrecorded.
type recordingCatalogNameReader struct {
	names fakeCatalogNameReader
	mu    sync.Mutex
	seen  []string
}

func (r *recordingCatalogNameReader) CatalogSnapshot(ctx context.Context, tenantID, catalogID string) (*pb.CatalogSnapshot, error) {
	if ctx.Value(recordedCallKey{}) != nil {
		r.mu.Lock()
		r.seen = append(r.seen, tenantID+"/"+catalogID)
		r.mu.Unlock()
	}
	return r.names.CatalogSnapshot(ctx, tenantID, catalogID)
}

func (r *recordingCatalogNameReader) lookups() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

// leakyCatalogNameReader serves the known catalogs and answers every other
// ID with notFound, standing in for a reader whose NotFound message says
// more than the caller may learn.
type leakyCatalogNameReader struct {
	known    fakeCatalogNameReader
	notFound error
}

func (r leakyCatalogNameReader) CatalogSnapshot(ctx context.Context, tenantID, catalogID string) (*pb.CatalogSnapshot, error) {
	if _, ok := r.known[catalogID]; ok {
		return r.known.CatalogSnapshot(ctx, tenantID, catalogID)
	}
	return nil, r.notFound
}

var _ = Describe("Service", func() {
	var (
		svc      *pipeline.Service
		store    *fakeStore
		registry *analyzer.Registry
		ctx      context.Context
	)

	BeforeEach(func() {
		store = newFakeStore()
		registry = analyzer.NewRegistry()

		cfg := config.PipelineConfig{
			MaxConcurrentJobs: 10,
			StageTimeout:      time.Hour,
		}

		svc = pipeline.New(
			store,
			nil, // engine — not needed for basic RPC tests
			registry,
			&fakeSynthesis{},
			pipeline.Attestors{},
			pipelineattestation.NewConverter(),
			&natsbustest.Fake{},
			&storagetest.Fake{},
			cfg,
			nil,
			pipeline.WithCatalogNameReader(serviceTestCatalogs),
		)

		ctx = context.Background()
		ctx, _ = tenant.WithTenant(ctx, "test-tenant")
	})

	// serviceWithReader is a Service over the specs' store whose catalog
	// reader is r.
	serviceWithReader := func(r pipeline.CatalogNameReader) *pipeline.Service {
		s := pipeline.New(store, nil, registry, &fakeSynthesis{}, pipeline.Attestors{},
			pipelineattestation.NewConverter(), &natsbustest.Fake{}, &storagetest.Fake{},
			config.PipelineConfig{MaxConcurrentJobs: 10, StageTimeout: time.Hour}, nil,
			pipeline.WithCatalogNameReader(r))
		DeferCleanup(func() { Expect(s.Stop(context.Background())).To(Succeed()) })
		return s
	}

	Describe("CreateJob", func() {
		It("creates a job and stages in the store", func() {
			req := &pb.CreateJobRequest{
				Config: &pb.JobConfig{CatalogId: "test-catalog", TargetCatalogId: "test-catalog"},
			}

			resp, err := svc.CreateJob(ctx, connect.NewRequest(req))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.JobId).NotTo(BeEmpty())
			Expect(resp.Msg.Status).To(Equal(pb.JobStatus_JOB_STATUS_PENDING))

			job, err := store.GetJob(ctx, resp.Msg.JobId)
			Expect(err).NotTo(HaveOccurred())
			Expect(job.TenantID).To(Equal("test-tenant"))
			// Job status is not asserted here: CreateJob spawns executeJob
			// asynchronously, which transitions pending -> running immediately.
			// The pending-at-creation guarantee is covered by resp.Msg.Status above.

			stages, err := store.GetStages(ctx, resp.Msg.JobId)
			Expect(err).NotTo(HaveOccurred())
			// Empty registry = no analyzers: only synthesis and graph.
			Expect(stages).To(ConsistOf(HaveField("StageName", "synthesis"), HaveField("StageName", "graph")))
		})

		// The registry holds only the enabled analyzers (NewProductionRegistry),
		// so a job's stages are exactly those, plus candidate_generation when
		// a pair analyzer needs candidates, plus synthesis and graph.
		DescribeTable("creates a stage per registered analyzer, and candidate_generation only before a pair analyzer",
			func(analyzers []string, want []string) {
				deps := map[string][]string{"embedding": {"classify"}, "requires": {"embedding"}, "relationship": {"embedding"}}
				reg := analyzer.NewRegistry()
				for _, name := range analyzers {
					Expect(analyzer.Register[*pb.Control](reg, newControlStub(name, deps[name]...))).To(Succeed())
				}
				eng := analysis.New(reg, &executorTestDispatcher{}, &executorTestCollector{},
					config.EngineConfig{TaskTimeout: time.Minute, MaxRetries: 1, RetryBackoff: time.Millisecond},
					map[string]natsbus.TaskType{})
				stageSvc := pipeline.New(store, eng, reg, &fakeSynthesis{}, pipeline.Attestors{},
					pipelineattestation.NewConverter(), &natsbustest.Fake{}, &storagetest.Fake{},
					config.PipelineConfig{MaxConcurrentJobs: 10, StageTimeout: time.Hour}, nil, pipeline.WithCatalogNameReader(serviceTestCatalogs))
				DeferCleanup(func() { Expect(stageSvc.Stop(context.Background())).To(Succeed()) })

				resp, err := stageSvc.CreateJob(ctx, connect.NewRequest(&pb.CreateJobRequest{
					Config: &pb.JobConfig{CatalogId: "test-catalog", TargetCatalogId: "test-catalog"},
				}))
				Expect(err).NotTo(HaveOccurred())

				stages, err := store.GetStages(ctx, resp.Msg.JobId)
				Expect(err).NotTo(HaveOccurred())
				var names []string
				for _, st := range stages {
					names = append(names, st.StageName)
				}
				Expect(names).To(ConsistOf(want))
				Expect(names[len(names)-2:]).To(Equal([]string{"synthesis", "graph"}))
			},
			Entry("no analyzer", []string{}, []string{"synthesis", "graph"}),
			Entry("classify, embedding and artifacts",
				[]string{"classify", "embedding", "artifacts"},
				[]string{"classify", "embedding", "artifacts", "synthesis", "graph"}),
			Entry("relationship only as pair analyzer",
				[]string{"classify", "embedding", "relationship"},
				[]string{"classify", "embedding", "candidate_generation", "relationship", "synthesis", "graph"}),
			Entry("requires only as pair analyzer",
				[]string{"classify", "embedding", "requires"},
				[]string{"classify", "embedding", "candidate_generation", "requires", "synthesis", "graph"}),
		)

		It("inserts candidate_generation between pre-candidate and candidate-dependent stages", func() {
			// Register a DAG whose topological order places the candidate-dependent
			// analyzers (requires/relationship) after the pre-candidate ones.
			orderingRegistry := analyzer.NewRegistry()
			Expect(analyzer.Register[*emptypb.Empty](orderingRegistry, newStub("classify"))).To(Succeed())
			Expect(analyzer.Register[*emptypb.Empty](orderingRegistry, newStub("embedding", "classify"))).To(Succeed())
			Expect(analyzer.Register[*emptypb.Empty](orderingRegistry, newStub("artifacts", "classify"))).To(Succeed())
			Expect(analyzer.Register[*pb.Control](orderingRegistry, newControlStub("requires", "embedding"))).To(Succeed())
			Expect(analyzer.Register[*pb.Control](orderingRegistry, newControlStub("relationship", "embedding"))).To(Succeed())

			// A real engine avoids a nil-engine panic in the async executeJob;
			// the stub analyzers generate no work, so the job completes quickly.
			dispatcher := &executorTestDispatcher{}
			collector := &executorTestCollector{}
			taskTypes := map[string]natsbus.TaskType{
				"classify":     "classify",
				"embedding":    "embedding",
				"artifacts":    "artifacts",
				"requires":     "requires",
				"relationship": "relate",
			}
			eng := analysis.New(orderingRegistry, dispatcher, collector, config.EngineConfig{
				TaskTimeout:  time.Minute,
				MaxRetries:   1,
				RetryBackoff: time.Millisecond,
			}, taskTypes)

			orderingSvc := pipeline.New(
				store,
				eng,
				orderingRegistry,
				&fakeSynthesis{},
				pipeline.Attestors{},
				pipelineattestation.NewConverter(),
				&natsbustest.Fake{},
				&storagetest.Fake{},
				config.PipelineConfig{MaxConcurrentJobs: 10, StageTimeout: time.Hour},
				nil,
				pipeline.WithCatalogNameReader(serviceTestCatalogs),
			)

			req := &pb.CreateJobRequest{
				Config: &pb.JobConfig{CatalogId: "test-catalog", TargetCatalogId: "test-catalog"},
			}

			resp, err := orderingSvc.CreateJob(ctx, connect.NewRequest(req))
			Expect(err).NotTo(HaveOccurred())

			stages, err := store.GetStages(ctx, resp.Msg.JobId)
			Expect(err).NotTo(HaveOccurred())

			names := make([]string, len(stages))
			for i, stage := range stages {
				names[i] = stage.StageName
			}

			indexOf := func(name string) int {
				for i, n := range names {
					if n == name {
						return i
					}
				}
				return -1
			}

			Expect(indexOf("candidate_generation")).To(BeNumerically(">", indexOf("embedding")))
			Expect(indexOf("candidate_generation")).To(BeNumerically(">", indexOf("artifacts")))
			Expect(indexOf("candidate_generation")).To(BeNumerically("<", indexOf("requires")))
			Expect(indexOf("candidate_generation")).To(BeNumerically("<", indexOf("relationship")))

			// synthesis/graph remain the final two stages.
			Expect(names[len(names)-2]).To(Equal("synthesis"))
			Expect(names[len(names)-1]).To(Equal("graph"))

			// Let the async job finish so it does not race teardown.
			Eventually(func() pipeline.JobStatus {
				j, err := store.GetJob(ctx, resp.Msg.JobId)
				if err != nil {
					return ""
				}
				return j.Status
			}, "2s", "50ms").Should(Equal(pipeline.JobStatusCompleted))
		})

		// The jobs RLS policy refuses a row whose created_by differs from
		// app.current_user, and the gateway shows a non-admin only the jobs
		// it created, so the creator is the caller's identity.
		DescribeTable("records the job's creator",
			func(user, wantCreator string) {
				callCtx := ctx
				if user != "" {
					callCtx = tenant.WithUser(ctx, user)
				}
				resp, err := svc.CreateJob(callCtx, connect.NewRequest(&pb.CreateJobRequest{
					Config: &pb.JobConfig{CatalogId: "test-catalog", TargetCatalogId: "test-catalog"},
				}))
				Expect(err).NotTo(HaveOccurred())

				job, err := store.GetJob(ctx, resp.Msg.JobId)
				Expect(err).NotTo(HaveOccurred())
				Expect(job.CreatedBy).To(Equal(wantCreator))
			},
			Entry("as the calling user when the context carries one", "alice@example.com", "alice@example.com"),
			Entry("as the tenant for a system call without a user", "", "test-tenant"),
		)

		It("returns InvalidArgument when tenant context is missing", func() {
			req := &pb.CreateJobRequest{
				Config: &pb.JobConfig{},
			}

			_, err := svc.CreateJob(context.Background(), connect.NewRequest(req))
			Expect(err).To(HaveOccurred())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		})

		It("returns InvalidArgument when config is nil", func() {
			req := &pb.CreateJobRequest{}

			_, err := svc.CreateJob(ctx, connect.NewRequest(req))
			Expect(err).To(HaveOccurred())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		})

		// Safety mechanism: a job without a catalog would run over zero
		// controls, so no row may be written for it.
		It("rejects a config without catalog_id and writes no job", func() {
			_, err := svc.CreateJob(ctx, connect.NewRequest(&pb.CreateJobRequest{
				Config: &pb.JobConfig{},
			}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(err).To(MatchError(And(ContainSubstring("catalog_id is required"), ContainSubstring("Import the catalog first"))))

			jobs, total, err := store.ListJobs(ctx, "test-tenant", pipeline.JobFilter{})
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(BeZero())
			Expect(jobs).To(BeEmpty())
		})

		// Safety mechanism: CreateJob is the authoritative entry point (the
		// gateway is not its only caller), so it refuses what the gateway
		// refuses, before writing any row.
		DescribeTable("rejects an invalid target or direction with guidance and writes no job",
			func(cfg *pb.JobConfig, want []string) {
				_, err := svc.CreateJob(ctx, connect.NewRequest(&pb.CreateJobRequest{Config: cfg}))
				Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
				for _, w := range want {
					Expect(err.Error()).To(ContainSubstring(w))
				}

				jobs, total, err := store.ListJobs(ctx, "test-tenant", pipeline.JobFilter{})
				Expect(err).NotTo(HaveOccurred())
				Expect(total).To(BeZero())
				Expect(jobs).To(BeEmpty())
			},
			Entry("no target", &pb.JobConfig{CatalogId: "cat-1"},
				[]string{"config.target_catalog_id is required", "set it to config.catalog_id to analyse within one catalog",
					"(crosscodex run start --source cat-1 --target cat-1)"}),
			Entry("bidirectional within one catalog", &pb.JobConfig{CatalogId: "cat-1", TargetCatalogId: "cat-1", Bidirectional: true},
				[]string{"config.bidirectional cannot be set when config.target_catalog_id equals config.catalog_id", "already pairs its controls in both directions"}),
		)

		It("stores the target and direction of a crosswalk job", func() {
			resp, err := svc.CreateJob(ctx, connect.NewRequest(&pb.CreateJobRequest{
				Config: &pb.JobConfig{CatalogId: "cat-1", TargetCatalogId: "cat-2", Bidirectional: true},
			}))
			Expect(err).NotTo(HaveOccurred())
			job, err := store.GetJob(ctx, resp.Msg.JobId)
			Expect(err).NotTo(HaveOccurred())
			cats, ok := jobconfig.ExtractCatalogs(job.Config)
			Expect(ok).To(BeTrue())
			Expect(cats).To(Equal(jobconfig.Catalogs{SourceID: "cat-1", TargetID: "cat-2", Bidirectional: true,
				SourceLabel: jobconfig.Label{Name: "Catalog One", Version: "1.0", Recorded: true},
				TargetLabel: jobconfig.Label{Name: "Catalog Two", Version: "2.0", Recorded: true}}))
		})

		// Safety mechanism, defence in depth: the gateway checks the
		// catalogs, but a direct pipeline RPC caller does not pass through
		// it, so CreateJob confirms both are the job tenant's before
		// writing any row.
		Describe("confirming the job's catalogs are its tenant's", func() {
			expectNoJob := func() {
				jobs, total, err := store.ListJobs(ctx, "test-tenant", pipeline.JobFilter{})
				Expect(err).NotTo(HaveOccurred())
				Expect(total).To(BeZero())
				Expect(jobs).To(BeEmpty())
			}

			It("reads the source, then the target, and creates the job", func() {
				reader := &recordingCatalogNameReader{names: serviceTestCatalogs}
				_, err := serviceWithReader(reader).CreateJob(context.WithValue(ctx, recordedCallKey{}, true), connect.NewRequest(&pb.CreateJobRequest{
					Config: &pb.JobConfig{CatalogId: "cat-1", TargetCatalogId: "cat-2"},
				}))
				Expect(err).NotTo(HaveOccurred())
				Expect(reader.lookups()).To(Equal([]string{"test-tenant/cat-1", "test-tenant/cat-2"}))
			})

			It("reads a within-catalog job's one catalog once", func() {
				reader := &recordingCatalogNameReader{names: serviceTestCatalogs}
				_, err := serviceWithReader(reader).CreateJob(context.WithValue(ctx, recordedCallKey{}, true), connect.NewRequest(&pb.CreateJobRequest{
					Config: &pb.JobConfig{CatalogId: "cat-1", TargetCatalogId: "cat-1"},
				}))
				Expect(err).NotTo(HaveOccurred())
				Expect(reader.lookups()).To(Equal([]string{"test-tenant/cat-1"}))
			})

			// The snapshot is part of the stored config, which the job's
			// attestation subject digests (contentDigest(job.Config)), so the
			// labels a job reports are signed with it.
			DescribeTable("snapshots the catalogs' names and versions into the stored config, replacing whatever the caller sent",
				func(cfg *pb.JobConfig, wantSource, wantTarget *pb.CatalogSnapshot) {
					sent := proto.Clone(cfg)
					resp, err := serviceWithReader(serviceTestCatalogs).CreateJob(ctx, connect.NewRequest(&pb.CreateJobRequest{Config: cfg}))
					Expect(err).NotTo(HaveOccurred())

					job, err := store.GetJob(ctx, resp.Msg.JobId)
					Expect(err).NotTo(HaveOccurred())
					var stored pb.JobConfig
					Expect(json.Unmarshal(job.Config, &stored)).To(Succeed())
					Expect(proto.Equal(stored.GetSourceCatalog(), wantSource)).To(BeTrue(), "source snapshot %v", stored.GetSourceCatalog())
					Expect(proto.Equal(stored.GetTargetCatalog(), wantTarget)).To(BeTrue(), "target snapshot %v", stored.GetTargetCatalog())
					Expect(proto.Equal(cfg, sent)).To(BeTrue(), "CreateJob changed the caller's request message")
				},
				Entry("a crosswalk without a snapshot",
					&pb.JobConfig{CatalogId: "cat-1", TargetCatalogId: "cat-2"},
					&pb.CatalogSnapshot{CatalogId: "cat-1", Name: "Catalog One", Version: "1.0"},
					&pb.CatalogSnapshot{CatalogId: "cat-2", Name: "Catalog Two", Version: "2.0"}),
				Entry("a crosswalk with a forged snapshot",
					&pb.JobConfig{CatalogId: "cat-1", TargetCatalogId: "cat-2",
						SourceCatalog: &pb.CatalogSnapshot{CatalogId: "cat-1", Name: "Forged", Version: "9"},
						TargetCatalog: &pb.CatalogSnapshot{CatalogId: "cat-other", Name: "Forged too"}},
					&pb.CatalogSnapshot{CatalogId: "cat-1", Name: "Catalog One", Version: "1.0"},
					&pb.CatalogSnapshot{CatalogId: "cat-2", Name: "Catalog Two", Version: "2.0"}),
				Entry("within one catalog, both sides from one read",
					&pb.JobConfig{CatalogId: "cat-1", TargetCatalogId: "cat-1",
						TargetCatalog: &pb.CatalogSnapshot{CatalogId: "cat-1", Name: "Forged"}},
					&pb.CatalogSnapshot{CatalogId: "cat-1", Name: "Catalog One", Version: "1.0"},
					&pb.CatalogSnapshot{CatalogId: "cat-1", Name: "Catalog One", Version: "1.0"}),
			)

			// No existence oracle. The reader here leaks, wrapping
			// ErrCatalogNotFound in a message naming another tenant, so equal
			// bytes prove CreateJob, not the reader, withholds it.
			DescribeTable("answers a missing catalog and another tenant's catalog with byte-identical NotFound errors, and writes no job",
				func(cfg *pb.JobConfig, field string) {
					start := func(notFound error) *connect.Error {
						reader := leakyCatalogNameReader{known: serviceTestCatalogs, notFound: notFound}
						_, err := serviceWithReader(reader).CreateJob(ctx, connect.NewRequest(&pb.CreateJobRequest{Config: cfg}))
						var connectErr *connect.Error
						Expect(errors.As(err, &connectErr)).To(BeTrue(), "want a *connect.Error, got %v", err)
						return connectErr
					}
					othersErr := start(fmt.Errorf("catalog cat-b belongs to tenant other-tenant: %w", pipeline.ErrCatalogNotFound))
					missingErr := start(fmt.Errorf("catalog cat-b: %w", pipeline.ErrCatalogNotFound))

					Expect(othersErr.Code()).To(Equal(connect.CodeNotFound))
					Expect(missingErr.Code()).To(Equal(connect.CodeNotFound))
					Expect([]byte(othersErr.Error())).To(Equal([]byte(missingErr.Error())))
					Expect(othersErr.Error()).To(SatisfyAll(
						ContainSubstring(fmt.Sprintf(`config.%s "cat-b" is not a catalog of your tenant`, field)),
						ContainSubstring("crosscodex catalog list"),
						Not(ContainSubstring("other-tenant")),
					))
					expectNoJob()
				},
				Entry("the source", &pb.JobConfig{CatalogId: "cat-b", TargetCatalogId: "cat-1"}, "catalog_id"),
				Entry("the target", &pb.JobConfig{CatalogId: "cat-1", TargetCatalogId: "cat-b"}, "target_catalog_id"),
			)

			It("answers a failed catalog read with Unavailable, withholds the cause, and writes no job", func() {
				reader := failingCatalogNameReader{err: errors.New("dial tcp 10.0.0.5:5432: connect: connection refused")}
				_, err := serviceWithReader(reader).CreateJob(ctx, connect.NewRequest(&pb.CreateJobRequest{
					Config: &pb.JobConfig{CatalogId: "cat-1", TargetCatalogId: "cat-2"},
				}))
				Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnavailable))
				Expect(err.Error()).To(SatisfyAll(
					ContainSubstring(`cannot create the job: whether config.catalog_id "cat-1"`),
					ContainSubstring("Retry the request"),
					Not(ContainSubstring("10.0.0.5")),
				))
				expectNoJob()
			})

			It("refuses every job when no catalog reader is wired, and writes no job", func() {
				_, err := serviceWithReader(nil).CreateJob(ctx, connect.NewRequest(&pb.CreateJobRequest{
					Config: &pb.JobConfig{CatalogId: "cat-1", TargetCatalogId: "cat-1"},
				}))
				Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnavailable))
				Expect(err.Error()).To(ContainSubstring("no catalog reader is configured"))
				expectNoJob()
			})
		})
	})

	Describe("GetJob", func() {
		It("counts completed, failed and cancelled stages apart", func() {
			now := time.Now()
			Expect(store.CreateJob(ctx, &pipeline.Job{
				JobID: "job-tally", TenantID: "test-tenant", Status: pipeline.JobStatusCancelled,
				Config: []byte(`{}`), CreatedBy: "test-tenant", CreatedAt: now, UpdatedAt: now,
			})).To(Succeed())
			Expect(store.CreateStages(ctx, "job-tally", []string{"classify", "embedding", "synthesis", "graph"})).To(Succeed())
			Expect(store.UpdateStageStatus(ctx, "job-tally", "classify", pipeline.StageStatusCompleted)).To(Succeed())
			Expect(store.UpdateStageError(ctx, "job-tally", "embedding", errors.New("worker lost"))).To(Succeed())
			Expect(store.UpdateStageStatus(ctx, "job-tally", "synthesis", pipeline.StageStatusCancelled)).To(Succeed())

			resp, err := svc.GetJob(ctx, connect.NewRequest(&pb.GetJobRequest{JobId: "job-tally"}))
			Expect(err).NotTo(HaveOccurred())
			p := resp.Msg.GetJob().GetProgress()
			Expect([]int32{p.GetTotalSteps(), p.GetCompletedSteps(), p.GetFailedSteps(), p.GetCancelledSteps()}).
				To(Equal([]int32{4, 1, 1, 1}))
		})

		It("returns the job with stages", func() {
			now := time.Now()
			job := &pipeline.Job{
				JobID:     "job-1",
				TenantID:  "test-tenant",
				Status:    pipeline.JobStatusRunning,
				Config:    []byte(`{}`),
				CreatedBy: "test-tenant",
				CreatedAt: now,
				UpdatedAt: now,
			}
			Expect(store.CreateJob(ctx, job)).To(Succeed())
			Expect(store.CreateStages(ctx, "job-1", []string{"stage-1", "stage-2"})).To(Succeed())

			req := &pb.GetJobRequest{JobId: "job-1"}

			resp, err := svc.GetJob(ctx, connect.NewRequest(req))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.Job.JobId).To(Equal("job-1"))
			Expect(resp.Msg.Job.Status).To(Equal(pb.JobStatus_JOB_STATUS_RUNNING))
			Expect(resp.Msg.Job.Stages).To(HaveLen(2))
		})

		It("returns NotFound for missing job", func() {
			req := &pb.GetJobRequest{JobId: "missing"}

			_, err := svc.GetJob(ctx, connect.NewRequest(req))
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("ListJobs", func() {
		BeforeEach(func() {
			now := time.Now()
			for i := 1; i <= 5; i++ {
				job := &pipeline.Job{
					JobID:     "job-" + string(rune('0'+i)),
					TenantID:  "test-tenant",
					Status:    pipeline.JobStatusCompleted,
					Config:    []byte(`{}`),
					CreatedBy: "test-tenant",
					CreatedAt: now,
					UpdatedAt: now,
				}
				Expect(store.CreateJob(ctx, job)).To(Succeed())
			}
		})

		It("returns all jobs for the tenant", func() {
			req := &pb.ListJobsRequest{}

			resp, err := svc.ListJobs(ctx, connect.NewRequest(req))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.Jobs).To(HaveLen(5))
		})

		It("filters by status", func() {
			now := time.Now()
			failedJob := &pipeline.Job{
				JobID:     "job-failed",
				TenantID:  "test-tenant",
				Status:    pipeline.JobStatusFailed,
				Config:    []byte(`{}`),
				CreatedBy: "test-tenant",
				CreatedAt: now,
				UpdatedAt: now,
			}
			Expect(store.CreateJob(ctx, failedJob)).To(Succeed())

			req := &pb.ListJobsRequest{
				Status: pb.JobStatus_JOB_STATUS_FAILED,
			}

			resp, err := svc.ListJobs(ctx, connect.NewRequest(req))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.Jobs).To(HaveLen(1))
			Expect(resp.Msg.Jobs[0].JobId).To(Equal("job-failed"))
		})
	})

	Describe("CancelJob", func() {
		It("returns NotFound if job is not running", func() {
			req := &pb.CancelJobRequest{JobId: "not-running"}

			_, err := svc.CancelJob(ctx, connect.NewRequest(req))
			Expect(err).To(HaveOccurred())
		})

		It("cancels a running job", func() {
			blocker := &blockingSynthesis{block: make(chan struct{})}
			defer close(blocker.block)

			blockingSvc := pipeline.New(
				store,
				nil,
				registry,
				blocker,
				pipeline.Attestors{},
				pipelineattestation.NewConverter(),
				&natsbustest.Fake{},
				&storagetest.Fake{},
				config.PipelineConfig{
					MaxConcurrentJobs: 10,
					StageTimeout:      time.Hour,
				},
				nil,
				pipeline.WithCatalogNameReader(serviceTestCatalogs),
			)

			createReq := &pb.CreateJobRequest{
				Config: &pb.JobConfig{CatalogId: "test-catalog", TargetCatalogId: "test-catalog"},
			}

			createResp, err := blockingSvc.CreateJob(ctx, connect.NewRequest(createReq))
			Expect(err).NotTo(HaveOccurred())

			jobID := createResp.Msg.JobId

			// Wait until the job appears in the store as running.
			Eventually(func() pipeline.JobStatus {
				j, err := store.GetJob(ctx, jobID)
				if err != nil {
					return ""
				}
				return j.Status
			}, "2s", "50ms").Should(Equal(pipeline.JobStatusRunning))

			cancelResp, err := blockingSvc.CancelJob(ctx, connect.NewRequest(&pb.CancelJobRequest{JobId: jobID}))
			Expect(err).NotTo(HaveOccurred())
			Expect(cancelResp.Msg.Cancelled).To(BeTrue())
		})

		jobStatus := func(jobID string) func() pipeline.JobStatus {
			return func() pipeline.JobStatus {
				j, err := store.GetJob(ctx, jobID)
				if err != nil {
					return ""
				}
				return j.Status
			}
		}
		stageStatuses := func(jobID string) map[string]pipeline.StageStatus {
			stages, err := store.GetStages(ctx, jobID)
			Expect(err).NotTo(HaveOccurred())
			out := make(map[string]pipeline.StageStatus, len(stages))
			for _, st := range stages {
				out[st.StageName] = st.Status
			}
			return out
		}
		// A cancelled run keeps writing (the job, then its running stages)
		// after CancelJob returns. holdCancelledStage parks it right after
		// its first cancelled-stage write, until release is closed, and
		// closes parked once it is there. Only the first such write parks.
		holdCancelledStage := func() (chan struct{}, func()) {
			parked, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			store.afterUpdateStageStatus = func(_, _ string, status pipeline.StageStatus) {
				if status != pipeline.StageStatusCancelled {
					return
				}
				once.Do(func() {
					close(parked)
					<-release
				})
			}
			var closeOnce sync.Once
			return parked, func() { closeOnce.Do(func() { close(release) }) }
		}

		It("refuses a retry while the cancelled run is still stopping, then retries it once stopped, and the old run never touches the new one", func() {
			const jobID = "job-cancel-retry"
			parked, release := holdCancelledStage()
			job := startStuckClassifyJob(ctx, store, "test-tenant", jobID)
			// Registered after the service's Stop, so it runs first: a
			// failed spec must not leave the run parked while Stop waits.
			DeferCleanup(release)

			_, err := job.svc.CancelJob(ctx, connect.NewRequest(&pb.CancelJobRequest{JobId: jobID}))
			Expect(err).NotTo(HaveOccurred())
			Eventually(parked).Should(BeClosed())
			Expect(jobStatus(jobID)()).To(Equal(pipeline.JobStatusCancelled))

			for _, fromFailure := range []bool{true, false} {
				_, err = job.svc.RetryJob(ctx, connect.NewRequest(&pb.RetryJobRequest{JobId: jobID, RetryFromFailure: fromFailure}))
				Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
				Expect(err).To(MatchError(And(
					ContainSubstring("job "+jobID+" is still stopping"),
					ContainSubstring("crosscodex run status "+jobID))))
			}

			release()
			Eventually(func() error {
				_, err := job.svc.RetryJob(ctx, connect.NewRequest(&pb.RetryJobRequest{JobId: jobID, RetryFromFailure: true}))
				return err
			}).Should(Succeed())

			// The new run reaches classify again and keeps running: nothing
			// from the old run cancels it or ends its stage.
			Eventually(job.dispatched).Should(HaveLen(2))
			Consistently(jobStatus(jobID), "200ms").Should(Equal(pipeline.JobStatusRunning))
			Expect(stageStatuses(jobID)).To(HaveKeyWithValue("classify", pipeline.StageStatusRunning))

			// And it is still the service's to cancel, which records it.
			_, err = job.svc.CancelJob(ctx, connect.NewRequest(&pb.CancelJobRequest{JobId: jobID}))
			Expect(err).NotTo(HaveOccurred())
			Eventually(jobStatus(jobID)).Should(Equal(pipeline.JobStatusCancelled))
		})

		It("lets exactly one of several concurrent retries of a failed job start it", func() {
			// job.svc runs a stuck classify run of another job; the failed
			// job retried here is not running anywhere.
			job := startStuckClassifyJob(ctx, store, "test-tenant", "job-busy")
			const jobID = "job-concurrent-retry"
			Expect(store.CreateJob(ctx, &pipeline.Job{
				JobID: jobID, TenantID: "test-tenant", Status: pipeline.JobStatusRunning, CreatedBy: "test-tenant",
				Config: []byte(`{"catalog_id":"cat-1"}`), CreatedAt: time.Now(), UpdatedAt: time.Now(),
			})).To(Succeed())
			Expect(store.CreateStages(ctx, jobID, []string{"classify", "synthesis", "graph"})).To(Succeed())
			Expect(store.UpdateStageError(ctx, jobID, "classify", errors.New("worker lost"))).To(Succeed())
			Expect(store.UpdateJobStatus(ctx, jobID, pipeline.JobStatusFailed, errors.New("worker lost"))).To(Succeed())

			const callers = 8
			var wg sync.WaitGroup
			errs := make([]error, callers)
			start := make(chan struct{})
			for i := range callers {
				wg.Add(1)
				go func() {
					defer GinkgoRecover()
					defer wg.Done()
					<-start
					_, errs[i] = job.svc.RetryJob(ctx, connect.NewRequest(&pb.RetryJobRequest{JobId: jobID, RetryFromFailure: true}))
				}()
			}
			close(start)
			wg.Wait()

			var won int
			for _, err := range errs {
				if err == nil {
					won++
					continue
				}
				Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
			}
			Expect(won).To(Equal(1))
			Eventually(job.dispatched).Should(HaveLen(2))
			Consistently(job.dispatched, "200ms").Should(HaveLen(2))
		})

		It("abandons the running stage's collection so workers drop its queued LLM tasks", func() {
			const jobID = "job-cancel-abandon"
			job := startStuckClassifyJob(ctx, store, "test-tenant", jobID)

			_, err := job.svc.CancelJob(ctx, connect.NewRequest(&pb.CancelJobRequest{JobId: jobID}))
			Expect(err).NotTo(HaveOccurred())

			Eventually(jobStatus(jobID)).Should(Equal(pipeline.JobStatusCancelled))
			var abandons []natsbus.AbandonedCollection
			for _, p := range job.bus.Published() {
				if c, err := natsbus.ParseAbandonedSubject(p.Subject); err == nil {
					abandons = append(abandons, c)
				}
			}
			Expect(abandons).To(Equal([]natsbus.AbandonedCollection{{
				TenantID: "test-tenant", TaskType: natsbus.TaskClassify, JobID: jobID, CollectionID: job.dispatched()[0],
			}}))
		})

		// The job's own context is cancelled by the time these writes run;
		// fakeStore refuses a cancelled context as PGStore does, so they
		// land only if they are made on a context that outlives the job.
		It("records the cancellation: the job and its running stage end cancelled and the state is published", func() {
			const jobID = "job-cancel-recorded"
			job := startStuckClassifyJob(ctx, store, "test-tenant", jobID)
			Expect(stageStatuses(jobID)).To(HaveKeyWithValue("classify", pipeline.StageStatusRunning))

			_, err := job.svc.CancelJob(ctx, connect.NewRequest(&pb.CancelJobRequest{JobId: jobID}))
			Expect(err).NotTo(HaveOccurred())

			Eventually(jobStatus(jobID)).Should(Equal(pipeline.JobStatusCancelled))
			Eventually(func() []string { return publishedStates(job.bus, "test-tenant", jobID) }).Should(ContainElement("cancelled"))
			Expect(stageStatuses(jobID)).To(Equal(map[string]pipeline.StageStatus{
				"classify":  pipeline.StageStatusCancelled,
				"synthesis": pipeline.StageStatusPending,
				"graph":     pipeline.StageStatusPending,
			}))

			resp, err := job.svc.GetJob(ctx, connect.NewRequest(&pb.GetJobRequest{JobId: jobID}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetJob().GetStatus()).To(Equal(pb.JobStatus_JOB_STATUS_CANCELLED))
			Expect(resp.Msg.GetJob().GetStages()).To(ContainElement(And(
				HaveField("StageName", "classify"),
				HaveField("Status", pb.JobStatus_JOB_STATUS_CANCELLED),
			)))
		})

		It("leaves a job interrupted by Stop running, so the next start resumes it", func() {
			const jobID = "job-stop-resumable"
			job := startStuckClassifyJob(ctx, store, "test-tenant", jobID)

			stopCtx, stopCancel := context.WithTimeout(ctx, 5*time.Second)
			defer stopCancel()
			Expect(job.svc.Stop(stopCtx)).To(Succeed())

			Expect(jobStatus(jobID)()).To(Equal(pipeline.JobStatusRunning))
			Expect(stageStatuses(jobID)).To(HaveKeyWithValue("classify", pipeline.StageStatusRunning))
			Expect(publishedStates(job.bus, "test-tenant", jobID)).NotTo(ContainElement("cancelled"))
			resumable, err := store.GetResumableJobs(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(resumable).To(ContainElement(HaveField("JobID", jobID)))
		})

		It("logs once, saying what to do, when the cancellation cannot be recorded", func() {
			const jobID = "job-cancel-unrecorded"
			store.updateStatusErr = errors.New("connection refused")
			store.updateStatusErrOn = pipeline.JobStatusCancelled
			logs := gbytes.NewBuffer()
			job := startStuckClassifyJob(ctx, store, "test-tenant", jobID, pipeline.WithLogger(slog.New(slog.NewTextHandler(logs, nil))))

			_, err := job.svc.CancelJob(ctx, connect.NewRequest(&pb.CancelJobRequest{JobId: jobID}))
			Expect(err).NotTo(HaveOccurred())

			Eventually(logs).Should(gbytes.Say(`could not record that job ` + jobID + ` was cancelled`))
			Expect(job.svc.Stop(context.Background())).To(Succeed())
			text := string(logs.Contents())
			Expect(strings.Count(text, "could not record that job")).To(Equal(1))
			Expect(text).To(And(
				ContainSubstring("connection refused"),
				ContainSubstring("resumes it on its next start"),
				ContainSubstring("crosscodex run cancel --yes "+jobID),
			))
			Expect(jobStatus(jobID)()).To(Equal(pipeline.JobStatusRunning))
		})
	})

	// A job is written completed only once its attestation bundle is
	// stored; completed is immutable, so a bundle lost to a late cancel or
	// shutdown could never be produced afterwards.
	Describe("terminal transitions", func() {
		const bundleKey = "attestations/test-tenant/%s/bundle.json"

		// attestedService runs one classify analyzer (no tasks) over a real
		// attestation generator, so links and bundles are real, against
		// objects. When holdBundle is true, storing a bundle blocks until
		// the job's context is done and then fails with its error, as an
		// object store does when a request is cancelled.
		type attestedService struct {
			svc        *pipeline.Service
			bus        *natsbustest.Fake
			bundlePuts chan struct{}
		}
		newAttestedService := func(objects *storagetest.Fake, attestor attestation.Generator, holdBundle bool) attestedService {
			reg := analyzer.NewRegistry()
			Expect(analyzer.Register[*pb.Control](reg, newControlStub("classify"))).To(Succeed())
			bus := &natsbustest.Fake{}
			eng := analysis.New(reg, &executorTestDispatcher{}, &executorTestCollector{},
				config.EngineConfig{TaskTimeout: time.Minute, MaxRetries: 1, RetryBackoff: time.Millisecond},
				map[string]natsbus.TaskType{"classify": natsbus.TaskClassify},
				analysis.WithStageReporter(pipeline.NewDBStageReporter(analysis.NewNATSStageReporter(bus), store)))
			bundlePuts := make(chan struct{}, 10)
			stor := &storagetest.Fake{
				GetFunc: objects.Get,
				PutFunc: func(ctx context.Context, key string, data io.Reader) error {
					if strings.HasSuffix(key, "/bundle.json") {
						bundlePuts <- struct{}{}
						if holdBundle {
							<-ctx.Done()
							return ctx.Err()
						}
					}
					return objects.Put(ctx, key, data)
				},
			}
			svc := pipeline.New(store, eng, reg, &fakeSynthesisExecutor{},
				pipeline.Attestors{Default: &pipeline.Attestor{Generator: attestor, ExpiresIn: time.Hour}},
				pipelineattestation.NewConverter(), bus, stor,
				config.PipelineConfig{MaxConcurrentJobs: 10, StageTimeout: time.Hour}, nil,
				pipeline.WithCatalogControlsReader(&fakeCatalogControlsReader{controls: []*pb.Control{{ControlId: "c1", CatalogId: "cat-1"}}}),
				pipeline.WithCatalogNameReader(serviceTestCatalogs))
			DeferCleanup(func() { Expect(svc.Stop(context.Background())).To(Succeed()) })
			return attestedService{svc: svc, bus: bus, bundlePuts: bundlePuts}
		}
		newAttestor := func() attestation.Generator {
			kp, err := attestation.NewEphemeralKeyProvider()
			Expect(err).NotTo(HaveOccurred())
			gen, err := attestation.NewGenerator(kp)
			Expect(err).NotTo(HaveOccurred())
			return gen
		}
		createJob := func(svc *pipeline.Service) string {
			resp, err := svc.CreateJob(ctx, connect.NewRequest(&pb.CreateJobRequest{Config: &pb.JobConfig{CatalogId: "cat-1", TargetCatalogId: "cat-1"}}))
			Expect(err).NotTo(HaveOccurred())
			return resp.Msg.JobId
		}
		jobStatus := func(jobID string) func() pipeline.JobStatus {
			return func() pipeline.JobStatus {
				j, err := store.GetJob(ctx, jobID)
				if err != nil {
					return ""
				}
				return j.Status
			}
		}
		noStageRunning := HaveEach(HaveField("Status", Not(Equal(pipeline.StageStatusRunning))))

		It("cancels a job cancelled while its bundle is stored, instead of completing it without one", func() {
			objects := &storagetest.Fake{}
			as := newAttestedService(objects, newAttestor(), true)
			jobID := createJob(as.svc)
			Eventually(as.bundlePuts).Should(Receive())

			_, err := as.svc.CancelJob(ctx, connect.NewRequest(&pb.CancelJobRequest{JobId: jobID}))
			Expect(err).NotTo(HaveOccurred())

			Eventually(jobStatus(jobID)).Should(Equal(pipeline.JobStatusCancelled))
			Consistently(jobStatus(jobID), "200ms").Should(Equal(pipeline.JobStatusCancelled))
			exists, err := objects.Exists(ctx, fmt.Sprintf(bundleKey, jobID))
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeFalse())
			stages, err := store.GetStages(ctx, jobID)
			Expect(err).NotTo(HaveOccurred())
			Expect(stages).To(noStageRunning)
		})

		It("leaves a job stopped while its bundle is stored running, and the next start completes it with the full bundle", func() {
			objects := &storagetest.Fake{}
			attestor := newAttestor()
			first := newAttestedService(objects, attestor, true)
			jobID := createJob(first.svc)
			Eventually(first.bundlePuts).Should(Receive())

			Expect(first.svc.Stop(ctx)).To(Succeed())
			Expect(jobStatus(jobID)()).To(Equal(pipeline.JobStatusRunning))
			exists, err := objects.Exists(ctx, fmt.Sprintf(bundleKey, jobID))
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeFalse())

			// Every stage completed in the first run; the resume must still
			// finalize, from the links that run stored.
			stages, err := store.GetStages(ctx, jobID)
			Expect(err).NotTo(HaveOccurred())
			Expect(stages).To(HaveEach(HaveField("Status", pipeline.StageStatusCompleted)))

			second := newAttestedService(objects, attestor, false)
			Expect(second.svc.Start(ctx)).To(Succeed())
			Eventually(jobStatus(jobID)).Should(Equal(pipeline.JobStatusCompleted))

			rc, err := objects.Get(ctx, fmt.Sprintf(bundleKey, jobID))
			Expect(err).NotTo(HaveOccurred())
			defer rc.Close()
			var bundle struct {
				Layout json.RawMessage   `json:"layout"`
				Links  []json.RawMessage `json:"links"`
			}
			Expect(json.NewDecoder(rc).Decode(&bundle)).To(Succeed())
			Expect(bundle.Layout).NotTo(BeEmpty())
			var steps []string
			for _, raw := range bundle.Links {
				link, err := attestor.Verify(ctx, raw)
				Expect(err).NotTo(HaveOccurred())
				steps = append(steps, link.Step)
			}
			Expect(steps).To(ConsistOf("classify", "synthesis", "graph"))
		})

		It("cancels a job cancelled while its graph stage runs, leaving no stage running", func() {
			objects := &storagetest.Fake{}
			as := newAttestedService(objects, newAttestor(), false)
			var jobID atomic.Value
			store.afterUpdateStageStatus = func(id, stage string, status pipeline.StageStatus) {
				if stage == "graph" && status == pipeline.StageStatusRunning {
					_, err := as.svc.CancelJob(ctx, connect.NewRequest(&pb.CancelJobRequest{JobId: id}))
					Expect(err).NotTo(HaveOccurred())
					jobID.Store(id)
				}
			}
			id := createJob(as.svc)

			Eventually(jobStatus(id)).Should(Equal(pipeline.JobStatusCancelled))
			Expect(jobID.Load()).To(Equal(id))
			stages, err := store.GetStages(ctx, id)
			Expect(err).NotTo(HaveOccurred())
			Expect(stages).To(noStageRunning)
			Expect(stages).To(ContainElement(And(HaveField("StageName", "graph"), HaveField("Status", pipeline.StageStatusCancelled))))
		})

		// CancelJob and completion race for the job's terminal state; exactly
		// one wins, and CancelJob tells the caller which.
		It("refuses to cancel a job that is already being recorded as completed, and the job completes", func() {
			objects := &storagetest.Fake{}
			as := newAttestedService(objects, newAttestor(), false)
			cancelErr := make(chan error, 1)
			store.beforeUpdateJobStatus = func(id string, status pipeline.JobStatus) {
				if status == pipeline.JobStatusCompleted {
					_, err := as.svc.CancelJob(ctx, connect.NewRequest(&pb.CancelJobRequest{JobId: id}))
					cancelErr <- err
				}
			}
			jobID := createJob(as.svc)

			var err error
			Eventually(cancelErr).Should(Receive(&err))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
			Expect(err).To(MatchError(And(
				ContainSubstring("cannot cancel job "+jobID),
				ContainSubstring("already completing"),
				ContainSubstring("crosscodex run status "+jobID),
			)))
			Eventually(jobStatus(jobID)).Should(Equal(pipeline.JobStatusCompleted))
			Expect(publishedStates(as.bus, "test-tenant", jobID)).NotTo(ContainElement("cancelled"))
		})
	})

	Describe("synthesis over a corrupt relationship result", func() {
		It("fails the job without running synthesis", func() {
			synth := &recordingSynthesis{}
			corruptSvc := pipeline.New(
				store,
				nil,
				registry,
				synth,
				pipeline.Attestors{},
				pipelineattestation.NewConverter(),
				&natsbustest.Fake{},
				&storagetest.Fake{},
				config.PipelineConfig{
					MaxConcurrentJobs: 10,
					StageTimeout:      time.Hour,
				},
				nil,
			)
			DeferCleanup(func() { Expect(corruptSvc.Stop(context.Background())).To(Succeed()) })

			now := time.Now()
			configBytes, err := json.Marshal(&pb.JobConfig{CatalogId: "test-catalog"})
			Expect(err).NotTo(HaveOccurred())
			Expect(store.CreateJob(ctx, &pipeline.Job{
				JobID: "job-corrupt-rel", TenantID: "test-tenant", Status: pipeline.JobStatusFailed,
				Config: configBytes, CreatedBy: "test-tenant", CreatedAt: now, UpdatedAt: now,
			})).To(Succeed())
			Expect(store.CreateStages(ctx, "job-corrupt-rel",
				[]string{"candidate_generation", "relationship", "synthesis", "graph"})).To(Succeed())
			Expect(store.UpdateStageStatus(ctx, "job-corrupt-rel", "candidate_generation", pipeline.StageStatusCompleted)).To(Succeed())
			// A relationship result whose contribution type is not one the
			// analyzer writes.
			Expect(store.CompleteAnalysisStage(ctx, "job-corrupt-rel", "relationship", []byte(
				`[{"source_id":"AC-1","target_id":"AC-2","relationship_type":"CONTRIBUTES_TO","confidence":0.9,`+
					`"properties":{"contribution_type":"BOGUS","unanimous":"true"}}]`), "")).To(Succeed())
			Expect(store.UpdateStageStatus(ctx, "job-corrupt-rel", "synthesis", pipeline.StageStatusFailed)).To(Succeed())

			_, err = corruptSvc.RetryJob(ctx, connect.NewRequest(&pb.RetryJobRequest{
				JobId:            "job-corrupt-rel",
				RetryFromFailure: true,
			}))
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() pipeline.JobStatus {
				j, err := store.GetJob(ctx, "job-corrupt-rel")
				if err != nil {
					return ""
				}
				return j.Status
			}, "2s", "20ms").Should(Equal(pipeline.JobStatusFailed))
			Expect(synth.calls.Load()).To(BeZero(), "no synthesis inputs, so no viability is written")
		})
	})

	Describe("RetryJob", func() {
		// A cancel stops a job a person may want to finish later: its
		// completed stages and their results are sound, so an explicit
		// retry resumes it from the stage the cancel interrupted, exactly as
		// from a failed one, instead of re-running (and re-billing) it all.
		DescribeTable("resets the stage the job stopped at and re-runs the job from it when retry_from_failure is true",
			func(stoppedJob pipeline.JobStatus, stoppedStage pipeline.StageStatus) {
				// blockingSynthesis parks the re-run inside the synthesis stage, so
				// "running" is a state this spec can observe, not a window the
				// background executeJob can close at any moment.
				blocker := &blockingSynthesis{block: make(chan struct{})}
				released := false
				release := func() {
					if !released {
						released = true
						close(blocker.block)
					}
				}

				retrySvc := pipeline.New(
					store,
					nil,
					registry,
					blocker,
					pipeline.Attestors{},
					pipelineattestation.NewConverter(),
					&natsbustest.Fake{},
					&storagetest.Fake{},
					config.PipelineConfig{
						MaxConcurrentJobs: 10,
						StageTimeout:      time.Hour,
					},
					nil,
				)
				DeferCleanup(func() {
					release()
					Expect(retrySvc.Stop(context.Background())).To(Succeed())
				})

				now := time.Now()
				configBytes, err := json.Marshal(&pb.JobConfig{CatalogId: "test-catalog"})
				Expect(err).NotTo(HaveOccurred())
				job := &pipeline.Job{
					JobID:     "job-retry",
					TenantID:  "test-tenant",
					Status:    stoppedJob,
					Config:    configBytes,
					CreatedBy: "test-tenant",
					CreatedAt: now,
					UpdatedAt: now,
				}
				Expect(store.CreateJob(ctx, job)).To(Succeed())
				// Stages of a job created before candidate_generation was left
				// out without pair analyzers. The job stopped in synthesis, so
				// the stage before it had completed.
				Expect(store.CreateStages(ctx, "job-retry", []string{"candidate_generation", "synthesis", "graph"})).To(Succeed())
				Expect(store.UpdateStageStatus(ctx, "job-retry", "candidate_generation", pipeline.StageStatusCompleted)).To(Succeed())
				Expect(store.UpdateStageStatus(ctx, "job-retry", "synthesis", stoppedStage)).To(Succeed())

				resp, err := retrySvc.RetryJob(ctx, connect.NewRequest(&pb.RetryJobRequest{
					JobId:            "job-retry",
					RetryFromFailure: true,
				}))
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.Msg.NewJobId).To(Equal("job-retry"))

				stageStatus := func(name string) func() pipeline.StageStatus {
					return func() pipeline.StageStatus {
						stages, err := store.GetStages(ctx, "job-retry")
						if err != nil {
							return ""
						}
						for _, s := range stages {
							if s.StageName == name {
								return s.Status
							}
						}
						return ""
					}
				}
				jobStatus := func() pipeline.JobStatus {
					j, err := store.GetJob(ctx, "job-retry")
					if err != nil {
						return ""
					}
					return j.Status
				}

				// The re-run reached the failed stage and is parked there.
				Eventually(stageStatus("synthesis"), "2s", "20ms").Should(Equal(pipeline.StageStatusRunning))
				Expect(stageStatus("candidate_generation")()).To(Equal(pipeline.StageStatusCompleted),
					"stages before the failed one are not re-run")
				Expect(stageStatus("graph")()).To(Equal(pipeline.StageStatusPending),
					"stages after the failed one are reset to pending")
				Expect(jobStatus()).To(Equal(pipeline.JobStatusRunning))

				release()
				Eventually(jobStatus, "2s", "20ms").Should(Equal(pipeline.JobStatusCompleted))
				Expect(stageStatus("synthesis")()).To(Equal(pipeline.StageStatusCompleted))
				Expect(stageStatus("graph")()).To(Equal(pipeline.StageStatusCompleted))
			},
			Entry("a failed job, from its failed stage", pipeline.JobStatusFailed, pipeline.StageStatusFailed),
			Entry("a cancelled job, from its cancelled stage", pipeline.JobStatusCancelled, pipeline.StageStatusCancelled),
		)

		// A failure whose cause a retry cannot change is refused before
		// anything is reset, so the job keeps its stages and its error.
		DescribeTable("refuses to retry from its failure a job whose failure a retry cannot fix, leaving it unchanged",
			func(cause error, want, remedy string) {
				now := time.Now()
				configBytes, _ := json.Marshal(&pb.JobConfig{CatalogId: "test-catalog"})
				Expect(store.CreateJob(ctx, &pipeline.Job{
					JobID: "job-permanent", TenantID: "test-tenant", Status: pipeline.JobStatusRunning,
					Config: configBytes, CreatedBy: "test-tenant", CreatedAt: now, UpdatedAt: now,
				})).To(Succeed())
				Expect(store.CreateStages(ctx, "job-permanent", []string{"classify", "synthesis", "graph"})).To(Succeed())
				Expect(store.UpdateStageError(ctx, "job-permanent", "synthesis", cause)).To(Succeed())
				failure := fmt.Errorf("job job-permanent: %w. Start a new job: crosscodex run start --source test-catalog --target test-catalog", cause)
				Expect(store.UpdateJobStatus(ctx, "job-permanent", pipeline.JobStatusFailed, failure)).To(Succeed())

				_, err := svc.RetryJob(ctx, connect.NewRequest(&pb.RetryJobRequest{JobId: "job-permanent", RetryFromFailure: true}))
				Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
				Expect(err).To(MatchError(And(
					ContainSubstring("cannot retry job job-permanent from its failure: "+want),
					ContainSubstring(recordedFailureLabel+failure.Error()),
					HaveSuffix(remedy))))

				j, err := store.GetJob(ctx, "job-permanent")
				Expect(err).NotTo(HaveOccurred())
				Expect(j.Status).To(Equal(pipeline.JobStatusFailed))
				Expect(j.ErrorMessage).To(Equal(failure.Error()))
				stages, err := store.GetStages(ctx, "job-permanent")
				Expect(err).NotTo(HaveOccurred())
				Expect(stages).To(HaveEach(HaveField("RetryCount", 0)))
				Expect(stages).To(ContainElement(And(HaveField("StageName", "synthesis"), HaveField("Status", pipeline.StageStatusFailed))))
			},
			Entry("its stored attestation does not verify", pipeline.ErrAttestationInvalid, "its attestation does not verify",
				"Start a new job: crosscodex run start --source test-catalog --target test-catalog"),
			Entry("its analyzers changed", pipeline.ErrAnalyzersChanged, "the enabled analyzers changed",
				"Start a new job: crosscodex run start --source test-catalog --target test-catalog"),
			Entry("its config names no catalog", pipeline.ErrMissingCatalogID, "its config names no catalog",
				"Start a new job: crosscodex run start --source test-catalog --target test-catalog"),
			Entry("its target catalog has nothing to map onto", pipeline.ErrEmptyTargetCatalog,
				"its target catalog has no control with a statement, and a retry reads the same catalog",
				emptyTargetRemedy+"crosscodex run start --source test-catalog --target <catalog-id>"),
			Entry("a cached result failed its integrity check", resultcache.ErrCorrupt,
				"a cached result failed its integrity check, or a stage run's cache record did not match what the stage wrote, "+
					"and a retry reads the same cache",
				"Start a new job with --no-cache, which recomputes and replaces the cached results: "+
					"crosscodex run start --source test-catalog --target test-catalog --no-cache"),
		)

		It("accepts a retry from the failure of a job whose catalog was not found", func() {
			now := time.Now()
			configBytes, _ := json.Marshal(&pb.JobConfig{CatalogId: "cat-a", TargetCatalogId: "cat-b"})
			Expect(store.CreateJob(ctx, &pipeline.Job{
				JobID: "job-catalog-gone", TenantID: "test-tenant", Status: pipeline.JobStatusRunning,
				Config: configBytes, CreatedBy: "test-tenant", CreatedAt: now, UpdatedAt: now,
			})).To(Succeed())
			Expect(store.CreateStages(ctx, "job-catalog-gone", []string{"classify", "synthesis", "graph"})).To(Succeed())
			Expect(store.UpdateStageError(ctx, "job-catalog-gone", "classify", pipeline.ErrCatalogNotFound)).To(Succeed())
			failure := fmt.Errorf("job job-catalog-gone cannot run: catalog cat-b was not found: %w", pipeline.ErrCatalogNotFound)
			Expect(store.UpdateJobStatus(ctx, "job-catalog-gone", pipeline.JobStatusFailed, failure)).To(Succeed())

			resp, err := svc.RetryJob(ctx, connect.NewRequest(&pb.RetryJobRequest{JobId: "job-catalog-gone", RetryFromFailure: true}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.NewJobId).To(Equal("job-catalog-gone"))
		})

		// The stored failure is the job's record and is quoted as it was
		// written; a job that failed before cross-catalog analysis recorded
		// a command without --target, which the CLI now refuses. The
		// refusal therefore ends with a command built from the job's config.
		DescribeTable("quotes the stored failure as recorded and ends with the new-job command built from the job's config",
			func(config, recorded, remedy string) {
				now := time.Now()
				Expect(store.CreateJob(ctx, &pipeline.Job{
					JobID: "job-recorded", TenantID: "test-tenant", Status: pipeline.JobStatusRunning,
					Config: []byte(config), CreatedBy: "test-tenant", CreatedAt: now, UpdatedAt: now,
				})).To(Succeed())
				Expect(store.CreateStages(ctx, "job-recorded", []string{"classify", "synthesis", "graph"})).To(Succeed())
				Expect(store.UpdateStageError(ctx, "job-recorded", "classify", pipeline.ErrAnalyzersChanged)).To(Succeed())
				failure := fmt.Errorf("job job-recorded cannot run: %w. %s", pipeline.ErrAnalyzersChanged, recorded)
				Expect(store.UpdateJobStatus(ctx, "job-recorded", pipeline.JobStatusFailed, failure)).To(Succeed())

				_, err := svc.RetryJob(ctx, connect.NewRequest(&pb.RetryJobRequest{JobId: "job-recorded", RetryFromFailure: true}))
				Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
				Expect(err).To(MatchError(And(
					ContainSubstring("cannot retry job job-recorded from its failure: the enabled analyzers changed"),
					ContainSubstring(recordedFailureLabel+failure.Error()),
					HaveSuffix(remedy))))

				j, err := store.GetJob(ctx, "job-recorded")
				Expect(err).NotTo(HaveOccurred())
				Expect(j.ErrorMessage).To(Equal(failure.Error()))
			},
			Entry("a failure recorded before --target existed, within one catalog",
				`{"catalog_id":"cat-a"}`,
				"Start a new job with the analyzers enabled now: crosscodex run start --source cat-a. "+
					"To give the new job the analyzers this one was created with, first set analysis.classify.enabled=true",
				"Start a new job: crosscodex run start --source cat-a --target cat-a"),
			Entry("a failure recorded with the current command, across catalogs in both directions",
				`{"catalog_id":"cat-a","target_catalog_id":"cat-b","bidirectional":true}`,
				"Start a new job with the analyzers enabled now: crosscodex run start --source cat-a --target cat-b --bidirectional",
				"Start a new job: crosscodex run start --source cat-a --target cat-b --bidirectional"),
			Entry("a failure of a job started with --no-cache, whose new-job command keeps it",
				`{"catalog_id":"cat-a","target_catalog_id":"cat-a","no_cache":true}`,
				"Start a new job with the analyzers enabled now: crosscodex run start --source cat-a --target cat-a --no-cache",
				"Start a new job: crosscodex run start --source cat-a --target cat-a --no-cache"),
		)

		It("refuses to retry as a new job a job whose cached result failed its integrity check, writing no job", func() {
			now := time.Now()
			configBytes, _ := json.Marshal(&pb.JobConfig{CatalogId: "cat-1", TargetCatalogId: "cat-2"})
			Expect(store.CreateJob(ctx, &pipeline.Job{
				JobID: "job-corrupt", TenantID: "test-tenant", Status: pipeline.JobStatusRunning,
				Config: configBytes, CreatedBy: "test-tenant", CreatedAt: now, UpdatedAt: now,
			})).To(Succeed())
			Expect(store.CreateStages(ctx, "job-corrupt", []string{"classify", "synthesis", "graph"})).To(Succeed())
			failure := fmt.Errorf("stage classify: %w", resultcache.ErrCorrupt)
			Expect(store.UpdateStageError(ctx, "job-corrupt", "classify", failure)).To(Succeed())
			Expect(store.UpdateJobStatus(ctx, "job-corrupt", pipeline.JobStatusFailed, failure)).To(Succeed())

			_, err := svc.RetryJob(ctx, connect.NewRequest(&pb.RetryJobRequest{JobId: "job-corrupt"}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
			Expect(err).To(MatchError(And(
				ContainSubstring("cannot retry job job-corrupt as a new job: a cached result it would reuse failed its integrity check, "+
					"or a stage run's cache record did not match what the stage wrote, and a new job with the same config reads the same cache"),
				HaveSuffix("crosscodex run start --source cat-1 --target cat-2 --no-cache"))))

			jobs, total, err := store.ListJobs(ctx, "test-tenant", pipeline.JobFilter{})
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(BeEquivalentTo(1), "only the old job exists")
			Expect(jobs).To(ConsistOf(HaveField("JobID", "job-corrupt")))
			j, err := store.GetJob(ctx, "job-corrupt")
			Expect(err).NotTo(HaveOccurred())
			Expect(j.Status).To(Equal(pipeline.JobStatusFailed))
			Expect(j.ErrorClass).To(Equal(pipeline.JobErrorResultCacheCorrupt))
		})

		// A job started with --no-cache looks nothing up, so a new job with
		// its config is the remedy itself; resuming it is not.
		It("accepts a new-job retry of a --no-cache job that failed its cache integrity check, and still refuses resuming it", func() {
			now := time.Now()
			configBytes, _ := json.Marshal(&pb.JobConfig{CatalogId: "cat-1", TargetCatalogId: "cat-2", NoCache: true})
			Expect(store.CreateJob(ctx, &pipeline.Job{
				JobID: "job-corrupt-nocache", TenantID: "test-tenant", Status: pipeline.JobStatusRunning,
				Config: configBytes, CreatedBy: "test-tenant", CreatedAt: now, UpdatedAt: now,
			})).To(Succeed())
			Expect(store.CreateStages(ctx, "job-corrupt-nocache", []string{"classify", "synthesis", "graph"})).To(Succeed())
			failure := fmt.Errorf("stage classify: %w", resultcache.ErrCorrupt)
			Expect(store.UpdateStageError(ctx, "job-corrupt-nocache", "classify", failure)).To(Succeed())
			Expect(store.UpdateJobStatus(ctx, "job-corrupt-nocache", pipeline.JobStatusFailed, failure)).To(Succeed())

			_, err := svc.RetryJob(ctx, connect.NewRequest(&pb.RetryJobRequest{JobId: "job-corrupt-nocache", RetryFromFailure: true}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
			Expect(err).To(MatchError(ContainSubstring("cannot retry job job-corrupt-nocache from its failure")))

			resp, err := svc.RetryJob(ctx, connect.NewRequest(&pb.RetryJobRequest{JobId: "job-corrupt-nocache"}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.NewJobId).NotTo(BeEmpty())
			Expect(resp.Msg.NewJobId).NotTo(Equal("job-corrupt-nocache"))
			newJob, err := store.GetJob(ctx, resp.Msg.NewJobId)
			Expect(err).NotTo(HaveOccurred())
			cats, ok := jobconfig.ExtractCatalogs(newJob.Config)
			Expect(ok).To(BeTrue())
			Expect(cats.NoCache).To(BeTrue(), "the new job keeps --no-cache")
		})

		It("accepts a retry from the failure of a job whose result cache could not be reached", func() {
			now := time.Now()
			configBytes, _ := json.Marshal(&pb.JobConfig{CatalogId: "cat-1", TargetCatalogId: "cat-1"})
			Expect(store.CreateJob(ctx, &pipeline.Job{JobID: "job-cache-down", TenantID: "test-tenant", Status: pipeline.JobStatusRunning,
				Config: configBytes, CreatedBy: "test-tenant", CreatedAt: now, UpdatedAt: now})).To(Succeed())
			Expect(store.CreateStages(ctx, "job-cache-down", []string{"classify", "synthesis", "graph"})).To(Succeed())
			Expect(store.UpdateStageError(ctx, "job-cache-down", "classify", resultcache.ErrUnavailable)).To(Succeed())
			Expect(store.UpdateJobStatus(ctx, "job-cache-down", pipeline.JobStatusFailed, resultcache.ErrUnavailable)).To(Succeed())

			resp, err := svc.RetryJob(ctx, connect.NewRequest(&pb.RetryJobRequest{JobId: "job-cache-down", RetryFromFailure: true}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.NewJobId).To(Equal("job-cache-down"))
		})

		DescribeTable("accepts both retries of a job whose model probe failed",
			func(fromFailure bool) {
				now := time.Now()
				configBytes, err := json.Marshal(&pb.JobConfig{CatalogId: "cat-1", TargetCatalogId: "cat-1"})
				Expect(err).NotTo(HaveOccurred())
				Expect(store.CreateJob(ctx, &pipeline.Job{JobID: "job-llm-down", TenantID: "test-tenant", Status: pipeline.JobStatusRunning,
					Config: configBytes, CreatedBy: "test-tenant", CreatedAt: now, UpdatedAt: now})).To(Succeed())
				Expect(store.CreateStages(ctx, "job-llm-down", []string{"classify", "synthesis", "graph"})).To(Succeed())
				failure := &analysis.StageError{Analyzer: "classify", Sentinel: pipeline.ErrLLMUnavailable, Message: "probe failed"}
				Expect(store.UpdateStageError(ctx, "job-llm-down", "classify", failure)).To(Succeed())
				Expect(store.UpdateJobStatus(ctx, "job-llm-down", pipeline.JobStatusFailed, failure)).To(Succeed())
				j, err := store.GetJob(ctx, "job-llm-down")
				Expect(err).NotTo(HaveOccurred())
				Expect(j.ErrorClass).To(Equal(pipeline.JobErrorLLMUnavailable))

				resp, err := svc.RetryJob(ctx, connect.NewRequest(&pb.RetryJobRequest{JobId: "job-llm-down", RetryFromFailure: fromFailure}))
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.Msg.NewJobId).NotTo(BeEmpty())
			},
			Entry("from its failure", true),
			Entry("as a new job", false),
		)

		// Re-importing a target with statements gives it a new catalog ID
		// (IDs are content hashes), so the new job must name a target
		// rather than reuse this job's.
		DescribeTable("refuses to retry a job whose target has nothing to map onto, pointing at a new job with another target, and writes no job",
			func(fromFailure bool, want string) {
				now := time.Now()
				configBytes, _ := json.Marshal(&pb.JobConfig{CatalogId: "cat-1", TargetCatalogId: "cat-2", Bidirectional: true})
				Expect(store.CreateJob(ctx, &pipeline.Job{
					JobID: "job-empty-target", TenantID: "test-tenant", Status: pipeline.JobStatusRunning,
					Config: configBytes, CreatedBy: "test-tenant", CreatedAt: now, UpdatedAt: now,
				})).To(Succeed())
				Expect(store.CreateStages(ctx, "job-empty-target", []string{"classify", "synthesis", "graph"})).To(Succeed())
				Expect(store.UpdateStageError(ctx, "job-empty-target", "classify", pipeline.ErrEmptyTargetCatalog)).To(Succeed())
				failure := fmt.Errorf("job job-empty-target cannot run: %w", pipeline.ErrEmptyTargetCatalog)
				Expect(store.UpdateJobStatus(ctx, "job-empty-target", pipeline.JobStatusFailed, failure)).To(Succeed())

				_, err := svc.RetryJob(ctx, connect.NewRequest(&pb.RetryJobRequest{JobId: "job-empty-target", RetryFromFailure: fromFailure}))
				Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
				Expect(err).To(MatchError(And(
					ContainSubstring(want),
					HaveSuffix(emptyTargetRemedy+"crosscodex run start --source cat-1 --target <catalog-id> --bidirectional"),
					Not(ContainSubstring("--target cat-2")))))

				jobs, total, err := store.ListJobs(ctx, "test-tenant", pipeline.JobFilter{})
				Expect(err).NotTo(HaveOccurred())
				Expect(total).To(BeEquivalentTo(1), "only the old job exists")
				Expect(jobs).To(ConsistOf(HaveField("JobID", "job-empty-target")))
				j, err := store.GetJob(ctx, "job-empty-target")
				Expect(err).NotTo(HaveOccurred())
				Expect(j.Status).To(Equal(pipeline.JobStatusFailed))
				Expect(j.ErrorClass).To(Equal(pipeline.JobErrorEmptyTargetCatalog))
				stages, err := store.GetStages(ctx, "job-empty-target")
				Expect(err).NotTo(HaveOccurred())
				Expect(stages).To(HaveEach(HaveField("RetryCount", 0)))
			},
			Entry("from its failure", true,
				"cannot retry job job-empty-target from its failure: its target catalog has no control with a statement, and a retry reads the same catalog"),
			// A new job with the same config reads the same empty catalog.
			Entry("as a new job", false,
				"cannot retry job job-empty-target as a new job: its target catalog has no control with a statement, and a new job with the same config reads the same catalog"),
		)

		DescribeTable("classifies a job's failure by its cause, never its text",
			func(err error, want pipeline.JobErrorClass) {
				Expect(pipeline.ClassifyJobError(err)).To(Equal(want))
			},
			Entry("no error", nil, pipeline.JobErrorRetryable),
			Entry("a storage failure", fmt.Errorf("x: %w", pipeline.ErrAttestationStorage), pipeline.JobErrorRetryable),
			Entry("an invalid attestation", fmt.Errorf("x: %w", pipeline.ErrAttestationInvalid), pipeline.JobErrorAttestationInvalid),
			Entry("changed analyzers", fmt.Errorf("x: %w", pipeline.ErrAnalyzersChanged), pipeline.JobErrorAnalyzersChanged),
			Entry("a missing catalog", fmt.Errorf("x: %w", pipeline.ErrMissingCatalogID), pipeline.JobErrorMissingCatalog),
			Entry("an empty target catalog", fmt.Errorf("x: %w", pipeline.ErrEmptyTargetCatalog), pipeline.JobErrorEmptyTargetCatalog),
			// Re-importing the same file restores the catalog's ID, so a
			// retry can succeed.
			Entry("a deleted catalog", fmt.Errorf("x: %w", pipeline.ErrCatalogNotFound), pipeline.JobErrorRetryable),
			Entry("an unwrapped error with an invalid attestation's text", errors.New(pipeline.ErrAttestationInvalid.Error()), pipeline.JobErrorRetryable),
			Entry("an unreadable result cache", fmt.Errorf("x: %w", resultcache.ErrUnavailable), pipeline.JobErrorResultCacheUnavailable),
			Entry("a corrupt cached result", fmt.Errorf("x: %w", resultcache.ErrCorrupt), pipeline.JobErrorResultCacheCorrupt),
			Entry("a model probe that failed", fmt.Errorf("x: %w", pipeline.ErrLLMUnavailable), pipeline.JobErrorLLMUnavailable),
			Entry("a model probe stage error, by its class alone",
				&analysis.StageError{Sentinel: pipeline.ErrLLMUnavailable, Cause: resultcache.ErrCorrupt}, pipeline.JobErrorLLMUnavailable),
			Entry("an unreadable cache and a corrupt one in two analyzers, by the class a retry cannot fix",
				errors.Join(resultcache.ErrUnavailable, resultcache.ErrCorrupt), pipeline.JobErrorResultCacheCorrupt),
			Entry("an unreadable-cache stage error whose cause wraps a corrupt result, by its class alone",
				&analysis.StageError{Sentinel: resultcache.ErrUnavailable, Cause: resultcache.ErrCorrupt}, pipeline.JobErrorResultCacheUnavailable),
		)

		It("returns FailedPrecondition, saying what to do, when no stage has failed or been cancelled", func() {
			now := time.Now()
			configBytes, _ := json.Marshal(&pb.JobConfig{CatalogId: "cat-a", TargetCatalogId: "cat-b", Bidirectional: true})
			job := &pipeline.Job{
				JobID:     "job-retry-nofail",
				TenantID:  "test-tenant",
				Status:    pipeline.JobStatusCompleted,
				Config:    configBytes,
				CreatedBy: "test-tenant",
				CreatedAt: now,
				UpdatedAt: now,
			}
			Expect(store.CreateJob(ctx, job)).To(Succeed())
			Expect(store.CreateStages(ctx, "job-retry-nofail", []string{"stage-1", "stage-2"})).To(Succeed())
			// Leave all stages in pending (default) status — none failed.

			req := &pb.RetryJobRequest{
				JobId:            "job-retry-nofail",
				RetryFromFailure: true,
			}

			_, err := svc.RetryJob(ctx, connect.NewRequest(req))
			Expect(err).To(HaveOccurred())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
			Expect(err).To(MatchError(And(
				ContainSubstring("cannot retry job job-retry-nofail from a failure: no stage failed or was cancelled"),
				ContainSubstring("crosscodex run start --source cat-a --target cat-b --bidirectional"),
			)))
			stages, err := store.GetStages(ctx, "job-retry-nofail")
			Expect(err).NotTo(HaveOccurred())
			Expect(stages).To(HaveEach(And(HaveField("Status", pipeline.StageStatusPending), HaveField("RetryCount", 0))))
			j, err := store.GetJob(ctx, "job-retry-nofail")
			Expect(err).NotTo(HaveOccurred())
			Expect(j.Status).To(Equal(pipeline.JobStatusCompleted))
		})

		It("creates a new job when retry_from_failure is false", func() {
			now := time.Now()
			configBytes, _ := json.Marshal(&pb.JobConfig{CatalogId: "test-catalog"})
			job := &pipeline.Job{
				JobID:     "job-retry-new",
				TenantID:  "test-tenant",
				Status:    pipeline.JobStatusFailed,
				Config:    configBytes,
				CreatedBy: "test-tenant",
				CreatedAt: now,
				UpdatedAt: now,
			}
			Expect(store.CreateJob(ctx, job)).To(Succeed())

			req := &pb.RetryJobRequest{
				JobId:            "job-retry-new",
				RetryFromFailure: false,
			}

			resp, err := svc.RetryJob(ctx, connect.NewRequest(req))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.NewJobId).NotTo(Equal("job-retry-new"))
			Expect(resp.Msg.NewJobId).NotTo(BeEmpty())

			// A distinct new job exists in the store. Its status is not asserted:
			// RetryJob spawns executeJob asynchronously, which transitions the new
			// job away from pending immediately after creation.
			_, err = store.GetJob(ctx, resp.Msg.NewJobId)
			Expect(err).NotTo(HaveOccurred())
		})

		// A config stored before target_catalog_id ran within its one
		// catalog; the new job keeps that, stated explicitly, so every job
		// row created from now on names its target.
		It("stores the source as the target of a new job retried from a config without a target", func() {
			now := time.Now()
			configBytes, err := json.Marshal(&pb.JobConfig{CatalogId: "test-catalog"})
			Expect(err).NotTo(HaveOccurred())
			Expect(store.CreateJob(ctx, &pipeline.Job{
				JobID: "job-retry-legacy", TenantID: "test-tenant", Status: pipeline.JobStatusFailed,
				Config: configBytes, CreatedBy: "test-tenant", CreatedAt: now, UpdatedAt: now,
			})).To(Succeed())

			resp, err := svc.RetryJob(ctx, connect.NewRequest(&pb.RetryJobRequest{JobId: "job-retry-legacy"}))
			Expect(err).NotTo(HaveOccurred())

			newJob, err := store.GetJob(ctx, resp.Msg.NewJobId)
			Expect(err).NotTo(HaveOccurred())
			var stored pb.JobConfig
			Expect(json.Unmarshal(newJob.Config, &stored)).To(Succeed())
			Expect(stored.GetCatalogId()).To(Equal("test-catalog"))
			Expect(stored.GetTargetCatalogId()).To(Equal("test-catalog"))
			Expect(stored.GetBidirectional()).To(BeFalse())

			old, err := store.GetJob(ctx, "job-retry-legacy")
			Expect(err).NotTo(HaveOccurred())
			Expect(old.Config).To(Equal(configBytes))
		})

		// The new job is created now, so it records the catalogs' labels
		// now: a catalog renamed by a re-import since the old job shows its
		// current name on the new job, and the old job keeps its own.
		It("takes a fresh snapshot of the catalogs for a new job", func() {
			now := time.Now()
			configBytes, err := json.Marshal(&pb.JobConfig{CatalogId: "cat-1", TargetCatalogId: "cat-2",
				SourceCatalog: &pb.CatalogSnapshot{CatalogId: "cat-1", Name: "Old One", Version: "0.9"},
				TargetCatalog: &pb.CatalogSnapshot{CatalogId: "cat-2", Name: "Old Two"}})
			Expect(err).NotTo(HaveOccurred())
			Expect(store.CreateJob(ctx, &pipeline.Job{
				JobID: "job-retry-snapshot", TenantID: "test-tenant", Status: pipeline.JobStatusFailed,
				Config: configBytes, CreatedBy: "test-tenant", CreatedAt: now, UpdatedAt: now,
			})).To(Succeed())

			resp, err := svc.RetryJob(ctx, connect.NewRequest(&pb.RetryJobRequest{JobId: "job-retry-snapshot"}))
			Expect(err).NotTo(HaveOccurred())

			newJob, err := store.GetJob(ctx, resp.Msg.NewJobId)
			Expect(err).NotTo(HaveOccurred())
			var stored pb.JobConfig
			Expect(json.Unmarshal(newJob.Config, &stored)).To(Succeed())
			Expect(proto.Equal(stored.GetSourceCatalog(), &pb.CatalogSnapshot{CatalogId: "cat-1", Name: "Catalog One", Version: "1.0"})).To(BeTrue(), "%v", stored.GetSourceCatalog())
			Expect(proto.Equal(stored.GetTargetCatalog(), &pb.CatalogSnapshot{CatalogId: "cat-2", Name: "Catalog Two", Version: "2.0"})).To(BeTrue(), "%v", stored.GetTargetCatalog())

			old, err := store.GetJob(ctx, "job-retry-snapshot")
			Expect(err).NotTo(HaveOccurred())
			Expect(old.Config).To(Equal(configBytes))
		})

		// Safety mechanism: the new job is confirmed to be over the
		// tenant's catalogs as CreateJob confirms one.
		It("refuses a new job over a catalog the tenant no longer has, and writes no job", func() {
			now := time.Now()
			configBytes, err := json.Marshal(&pb.JobConfig{CatalogId: "cat-1", TargetCatalogId: "cat-deleted"})
			Expect(err).NotTo(HaveOccurred())
			Expect(store.CreateJob(ctx, &pipeline.Job{
				JobID: "job-retry-gone", TenantID: "test-tenant", Status: pipeline.JobStatusFailed,
				Config: configBytes, CreatedBy: "test-tenant", CreatedAt: now, UpdatedAt: now,
			})).To(Succeed())

			_, err = svc.RetryJob(ctx, connect.NewRequest(&pb.RetryJobRequest{JobId: "job-retry-gone"}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
			Expect(err.Error()).To(SatisfyAll(
				ContainSubstring(`cannot retry job job-retry-gone as a new job: config.target_catalog_id "cat-deleted" is not a catalog of your tenant`),
				ContainSubstring("Import the catalog first"),
			))
			_, total, err := store.ListJobs(ctx, "test-tenant", pipeline.JobFilter{})
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(BeEquivalentTo(1), "only the old job exists")
			old, err := store.GetJob(ctx, "job-retry-gone")
			Expect(err).NotTo(HaveOccurred())
			Expect(old.Config).To(Equal(configBytes))
		})

		DescribeTable("names the retry, not a job creation, when the new job's catalogs cannot be checked, and writes no job",
			func(reader pipeline.CatalogNameReader, code connect.Code, want string) {
				now := time.Now()
				configBytes, err := json.Marshal(&pb.JobConfig{CatalogId: "cat-1", TargetCatalogId: "cat-2"})
				Expect(err).NotTo(HaveOccurred())
				Expect(store.CreateJob(ctx, &pipeline.Job{
					JobID: "job-retry-op", TenantID: "test-tenant", Status: pipeline.JobStatusFailed,
					Config: configBytes, CreatedBy: "test-tenant", CreatedAt: now, UpdatedAt: now,
				})).To(Succeed())

				_, err = serviceWithReader(reader).RetryJob(ctx, connect.NewRequest(&pb.RetryJobRequest{JobId: "job-retry-op"}))
				Expect(connect.CodeOf(err)).To(Equal(code))
				Expect(err.Error()).To(SatisfyAll(
					ContainSubstring("cannot retry job job-retry-op as a new job: "+want),
					Not(ContainSubstring("create")),
				))
				_, total, err := store.ListJobs(ctx, "test-tenant", pipeline.JobFilter{})
				Expect(err).NotTo(HaveOccurred())
				Expect(total).To(BeEquivalentTo(1), "only the old job exists")
			},
			Entry("the catalog store cannot be read", failingCatalogNameReader{err: errors.New("connection refused")},
				connect.CodeUnavailable, `whether config.catalog_id "cat-1" is a catalog of your tenant could not be checked`),
			Entry("no catalog reader is wired", nil, connect.CodeUnavailable, "no catalog reader is configured"),
		)

		// A resume runs such a config without the flag (within one catalog,
		// controls are paired in both directions already), so a new job
		// from it does the same rather than being refused.
		It("retries a job stored as bidirectional within one catalog as a new job without the flag", func() {
			now := time.Now()
			configBytes := []byte(`{"catalog_id":"cat-1","target_catalog_id":"cat-1","bidirectional":true}`)
			Expect(store.CreateJob(ctx, &pipeline.Job{
				JobID: "job-retry-within-bidi", TenantID: "test-tenant", Status: pipeline.JobStatusFailed,
				Config: configBytes, CreatedBy: "test-tenant", CreatedAt: now, UpdatedAt: now,
			})).To(Succeed())

			resp, err := svc.RetryJob(ctx, connect.NewRequest(&pb.RetryJobRequest{JobId: "job-retry-within-bidi"}))
			Expect(err).NotTo(HaveOccurred())

			newJob, err := store.GetJob(ctx, resp.Msg.NewJobId)
			Expect(err).NotTo(HaveOccurred())
			var stored pb.JobConfig
			Expect(json.Unmarshal(newJob.Config, &stored)).To(Succeed())
			Expect(stored.GetCatalogId()).To(Equal("cat-1"))
			Expect(stored.GetTargetCatalogId()).To(Equal("cat-1"))
			Expect(stored.GetBidirectional()).To(BeFalse())
			old, err := store.GetJob(ctx, "job-retry-within-bidi")
			Expect(err).NotTo(HaveOccurred())
			Expect(old.Config).To(Equal(configBytes))
		})

		// Safety mechanism: a stored config CreateJob would refuse (written
		// before validation or outside CreateJob) must not become a new job.
		DescribeTable("refuses to retry a job whose stored config is invalid as a new job, and writes no job",
			func(configBytes []byte, want []string) {
				now := time.Now()
				Expect(store.CreateJob(ctx, &pipeline.Job{
					JobID: "job-retry-invalid", TenantID: "test-tenant", Status: pipeline.JobStatusFailed,
					Config: configBytes, CreatedBy: "test-tenant", CreatedAt: now, UpdatedAt: now,
				})).To(Succeed())

				_, err := svc.RetryJob(ctx, connect.NewRequest(&pb.RetryJobRequest{JobId: "job-retry-invalid"}))
				Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
				Expect(err.Error()).To(ContainSubstring("cannot retry job job-retry-invalid as a new job"))
				for _, w := range want {
					Expect(err.Error()).To(ContainSubstring(w))
				}

				jobs, total, err := store.ListJobs(ctx, "test-tenant", pipeline.JobFilter{})
				Expect(err).NotTo(HaveOccurred())
				Expect(total).To(Equal(int64(1)))
				Expect(jobs).To(ConsistOf(HaveField("JobID", "job-retry-invalid")))
				old, err := store.GetJob(ctx, "job-retry-invalid")
				Expect(err).NotTo(HaveOccurred())
				Expect(old.Config).To(Equal(configBytes))
				Expect(old.Status).To(Equal(pipeline.JobStatusFailed))
			},
			Entry("no catalog_id",
				[]byte(`{"target_catalog_id":"cat-1"}`),
				[]string{"config.catalog_id is required", "crosscodex run start --source <source-catalog-id> --target <target-catalog-id>"}),
			Entry("unreadable config",
				[]byte(`{"catalog_id":`),
				[]string{"could not be read", "crosscodex run start --source <source-catalog-id> --target <target-catalog-id>"}),
		)

		// The new job belongs to whoever retries: the jobs RLS WITH CHECK
		// demands created_by = app.current_user, the retrying caller.
		DescribeTable("records the retrying caller as the new job's creator",
			func(user, wantCreator string) {
				now := time.Now()
				configBytes, err := json.Marshal(&pb.JobConfig{CatalogId: "test-catalog"})
				Expect(err).NotTo(HaveOccurred())
				Expect(store.CreateJob(ctx, &pipeline.Job{
					JobID:     "job-retry-creator",
					TenantID:  "test-tenant",
					Status:    pipeline.JobStatusFailed,
					Config:    configBytes,
					CreatedBy: "original-creator",
					CreatedAt: now,
					UpdatedAt: now,
				})).To(Succeed())

				callCtx := ctx
				if user != "" {
					callCtx = tenant.WithUser(ctx, user)
				}
				resp, err := svc.RetryJob(callCtx, connect.NewRequest(&pb.RetryJobRequest{
					JobId:            "job-retry-creator",
					RetryFromFailure: false,
				}))
				Expect(err).NotTo(HaveOccurred())

				newJob, err := store.GetJob(ctx, resp.Msg.NewJobId)
				Expect(err).NotTo(HaveOccurred())
				Expect(newJob.CreatedBy).To(Equal(wantCreator))
			},
			Entry("as the calling user when the context carries one", "bob@example.com", "bob@example.com"),
			Entry("as the tenant for a system call without a user", "", "test-tenant"),
		)
	})

	Describe("Stop", func() {
		It("cancels running jobs and returns nil", func() {
			blocker := &blockingSynthesis{block: make(chan struct{})}
			defer close(blocker.block)

			stoppableSvc := pipeline.New(
				store,
				nil,
				registry,
				blocker,
				pipeline.Attestors{},
				pipelineattestation.NewConverter(),
				&natsbustest.Fake{},
				&storagetest.Fake{},
				config.PipelineConfig{
					MaxConcurrentJobs: 10,
					StageTimeout:      time.Hour,
				},
				nil,
				pipeline.WithCatalogNameReader(serviceTestCatalogs),
			)

			createReq := &pb.CreateJobRequest{
				Config: &pb.JobConfig{CatalogId: "test-catalog", TargetCatalogId: "test-catalog"},
			}

			// Create a job so it starts running (blocked in synthesis).
			_, err := stoppableSvc.CreateJob(ctx, connect.NewRequest(createReq))
			Expect(err).NotTo(HaveOccurred())

			// Wait for the job to reach running status.
			Eventually(func() bool {
				jobs, _, _ := store.ListJobs(ctx, "test-tenant", pipeline.JobFilter{Status: pipeline.JobStatusRunning})
				return len(jobs) > 0
			}, "2s", "50ms").Should(BeTrue())

			// Stop with a 2-second timeout context.
			stopCtx, stopCancel := context.WithTimeout(ctx, 2*time.Second)
			defer stopCancel()

			err = stoppableSvc.Stop(stopCtx)
			Expect(err).NotTo(HaveOccurred())

			// After Stop, the concurrency slot should be free.
			// Verify by creating another job (would fail with ResourceExhausted
			// if the slot were still occupied).
			unblocked := &blockingSynthesis{block: make(chan struct{})}
			close(unblocked.block) // let it complete immediately

			postStopSvc := pipeline.New(
				store,
				nil,
				registry,
				unblocked,
				pipeline.Attestors{},
				pipelineattestation.NewConverter(),
				&natsbustest.Fake{},
				&storagetest.Fake{},
				config.PipelineConfig{
					MaxConcurrentJobs: 1,
					StageTimeout:      time.Hour,
				},
				nil,
				pipeline.WithCatalogNameReader(serviceTestCatalogs),
			)

			_, err = postStopSvc.CreateJob(ctx, connect.NewRequest(createReq))
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Concurrency limit", func() {
		It("returns ResourceExhausted when MaxConcurrentJobs is reached", func() {
			blocker := &blockingSynthesis{block: make(chan struct{})}
			defer close(blocker.block)

			limitedSvc := pipeline.New(
				store,
				nil,
				registry,
				blocker,
				pipeline.Attestors{},
				pipelineattestation.NewConverter(),
				&natsbustest.Fake{},
				&storagetest.Fake{},
				config.PipelineConfig{
					MaxConcurrentJobs: 1,
					StageTimeout:      time.Hour,
				},
				nil,
				pipeline.WithCatalogNameReader(serviceTestCatalogs),
			)

			createReq := &pb.CreateJobRequest{
				Config: &pb.JobConfig{CatalogId: "test-catalog", TargetCatalogId: "test-catalog"},
			}

			// First job should succeed.
			_, err := limitedSvc.CreateJob(ctx, connect.NewRequest(createReq))
			Expect(err).NotTo(HaveOccurred())

			// Second job should be rejected — the first is still running (blocked in synthesis).
			_, err = limitedSvc.CreateJob(ctx, connect.NewRequest(createReq))
			Expect(err).To(HaveOccurred())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeResourceExhausted))
		})
	})
})
