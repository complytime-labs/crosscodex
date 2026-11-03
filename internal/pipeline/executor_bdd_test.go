package pipeline_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/analysis"
	"github.com/complytime-labs/crosscodex/internal/pipeline"
	pipelineattestation "github.com/complytime-labs/crosscodex/internal/pipeline/attestation"
	"github.com/complytime-labs/crosscodex/internal/synthesis"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/analyzertest"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/natsbus/natsbustest"
	"github.com/complytime-labs/crosscodex/pkg/resultcache"
	storagepkg "github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/storage/storagetest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// fakeSynthesisExecutor records Execute calls.
type fakeSynthesisExecutor struct {
	mu         sync.Mutex
	executions []fakeSynthExec
	result     *synthesis.ExecuteResult
	execErr    error
}

type fakeSynthExec struct {
	jobID           string
	inputs          []synthesis.SynthesisInput
	classifications map[string]synthesis.Classification
}

func (f *fakeSynthesisExecutor) Execute(ctx context.Context, jobID string,
	inputs []synthesis.SynthesisInput, classifications map[string]synthesis.Classification) (*synthesis.ExecuteResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.executions = append(f.executions, fakeSynthExec{
		jobID:           jobID,
		inputs:          inputs,
		classifications: classifications,
	})
	if f.execErr != nil {
		return nil, f.execErr
	}
	if f.result != nil {
		return f.result, nil
	}
	return &synthesis.ExecuteResult{}, nil
}

func (f *fakeSynthesisExecutor) execCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.executions)
}

// fakeAttestation implements attestation.Generator for tests.
// fakeAttestation is a real attestation.Generator over an ephemeral key that
// records layout and link calls and fails the calls a spec scripts.
// linkErrOn limits linkErr to one step; empty fails every link.
type fakeAttestation struct {
	attestation.Generator

	mu          sync.Mutex
	layoutCalls []attestation.LayoutOptions
	linkCalls   []fakeLinkCall
	layoutErr   error
	linkErr     error
	linkErrOn   string
	verifyErr   error
}

type fakeLinkCall struct {
	step      string
	materials []attestation.Artifact
	products  []attestation.Artifact
}

func newFakeAttestation() *fakeAttestation {
	kp, err := attestation.NewEphemeralKeyProvider()
	Expect(err).NotTo(HaveOccurred())
	gen, err := attestation.NewGenerator(kp)
	Expect(err).NotTo(HaveOccurred())
	return &fakeAttestation{Generator: gen}
}

func (f *fakeAttestation) CreateLayout(ctx context.Context, opts attestation.LayoutOptions) (*attestation.SignedLayout, error) {
	f.mu.Lock()
	f.layoutCalls = append(f.layoutCalls, opts)
	err := f.layoutErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return f.Generator.CreateLayout(ctx, opts)
}

func (f *fakeAttestation) CreateLink(ctx context.Context, step string, materials, products []attestation.Artifact, opts ...attestation.LinkOption) (*attestation.SignedLink, error) {
	f.mu.Lock()
	f.linkCalls = append(f.linkCalls, fakeLinkCall{step: step, materials: materials, products: products})
	err := f.linkErr
	if f.linkErrOn != "" && f.linkErrOn != step {
		err = nil
	}
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return f.Generator.CreateLink(ctx, step, materials, products, opts...)
}

func (f *fakeAttestation) VerifyChain(ctx context.Context, layout *attestation.SignedLayout, links []*attestation.SignedLink, subject attestation.Artifact) (*attestation.VerifiedChain, error) {
	f.mu.Lock()
	err := f.verifyErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return f.Generator.VerifyChain(ctx, layout, links, subject)
}

// flakyBundleStorage is the shared storage fake, except that while failures
// is positive each bundle write fails (and decrements it). puts counts
// bundle writes.
type flakyBundleStorage struct {
	*storagetest.Fake
	failures atomic.Int32
	puts     atomic.Int32
}

func (f *flakyBundleStorage) Put(ctx context.Context, key string, data io.Reader) error {
	if strings.HasSuffix(key, "/bundle.json") {
		f.puts.Add(1)
		if f.failures.Load() > 0 {
			f.failures.Add(-1)
			return errors.New("object store unavailable")
		}
	}
	return f.Fake.Put(ctx, key, data)
}

// attestedBy attests every tenant's jobs with gen, retrying a failed
// attestation write twice without delay.
func attestedBy(gen attestation.Generator) pipeline.Attestors {
	return pipeline.Attestors{
		Default:        &pipeline.Attestor{Generator: gen, ExpiresIn: 168 * time.Hour},
		StorageRetries: 2,
	}
}

func (f *fakeAttestation) linkCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.linkCalls)
}

// newStubOf returns an analyzertest.Stub whose unscripted behavior suits
// pipeline specs: GenerateWork yields no tasks and Aggregate yields a bare
// Output naming the analyzer. Metadata is an empty map rather than nil to
// match the Output shape the pre-migration stubs returned (internal/analysis
// engine.go also tolerates nil, so this is shape-preservation, not a
// requirement). Specs assign GenerateWorkFunc / AggregateFunc to override.
func newStubOf[T proto.Message](name string, schema proto.Message, deps ...string) *analyzertest.Stub[T] {
	return &analyzertest.Stub[T]{
		AnalyzerName: name,
		Dependencies: deps,
		Schema:       schema,
		GenerateWorkFunc: func(context.Context, T, analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
			return nil, nil
		},
		AggregateFunc: func(context.Context, []analyzer.TaskResult) (*analyzer.Output, error) {
			return &analyzer.Output{AnalyzerName: name, Metadata: map[string]string{}}, nil
		},
	}
}

// newStub creates an analyzer.Analyzer[*emptypb.Empty] that generates no tasks.
func newStub(name string, deps ...string) *analyzertest.Stub[*emptypb.Empty] {
	return newStubOf[*emptypb.Empty](name, &emptypb.Empty{}, deps...)
}

// newControlStub creates an analyzer.Analyzer[*pb.Control], the generic type
// every production analyzer (classify/embedding/artifacts/requires/
// relationship) is registered as. Needed wherever a test analyzer must
// participate in Controls fan-out (per-control GenerateWork calls) or receive
// the candidate-dependent pass's placeholder *pb.Control Input — both call
// GenerateWorkFromProto with a *pb.Control, which fails the registeredWrapper
// type assertion against newStub's *emptypb.Empty. It generates no tasks until
// a spec assigns GenerateWorkFunc.
func newControlStub(name string, deps ...string) *analyzertest.Stub[*pb.Control] {
	return newStubOf[*pb.Control](name, &pb.Control{}, deps...)
}

// fakeCatalogControlsReader implements pipeline.CatalogControlsReader,
// recording each call's (tenantID, catalogID). It serves byCatalog[catalogID]
// when byCatalog is set, and controls for every catalog otherwise.
type fakeCatalogControlsReader struct {
	mu        sync.Mutex
	calls     []fakeCatalogControlsCall
	controls  []*pb.Control
	byCatalog map[string][]*pb.Control
}

type fakeCatalogControlsCall struct {
	tenantID  string
	catalogID string
}

func (f *fakeCatalogControlsReader) Controls(_ context.Context, tenantID, catalogID string) ([]*pb.Control, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCatalogControlsCall{tenantID: tenantID, catalogID: catalogID})
	if f.byCatalog != nil {
		return f.byCatalog[catalogID], nil
	}
	return f.controls, nil
}

// catalogIDs returns the catalog of every Controls call, in call order.
func (f *fakeCatalogControlsReader) catalogIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]string, len(f.calls))
	for i, c := range f.calls {
		ids[i] = c.catalogID
	}
	return ids
}

var _ pipeline.CatalogControlsReader = (*fakeCatalogControlsReader)(nil)

// catalogLabel is a catalog's current name and version in a
// fakeCatalogNameReader.
type catalogLabel struct{ name, version string }

// fakeCatalogNameReader serves catalogs' current names and versions by
// catalog ID. An absent ID is pipeline.ErrCatalogNotFound and an invalid
// tenant is refused, as the real reader does.
type fakeCatalogNameReader map[string]catalogLabel

func (f fakeCatalogNameReader) CatalogSnapshot(_ context.Context, tenantID, catalogID string) (*pb.CatalogSnapshot, error) {
	if err := tenant.ValidateTenantID(tenantID); err != nil {
		return nil, err
	}
	l, ok := f[catalogID]
	if !ok {
		return nil, fmt.Errorf("catalog %s: %w", catalogID, pipeline.ErrCatalogNotFound)
	}
	return &pb.CatalogSnapshot{CatalogId: catalogID, Name: l.name, Version: l.version}, nil
}

var _ pipeline.CatalogNameReader = fakeCatalogNameReader(nil)

// failingCatalogNameReader fails every CatalogSnapshot call with err, standing in
// for a catalog-name read that fails for a reason other than a missing
// catalog (a database error).
type failingCatalogNameReader struct{ err error }

func (f failingCatalogNameReader) CatalogSnapshot(context.Context, string, string) (*pb.CatalogSnapshot, error) {
	return nil, f.err
}

var _ pipeline.CatalogNameReader = failingCatalogNameReader{}

// executorTestDispatcher records Dispatch calls.
type executorTestDispatcher struct {
	mu          sync.Mutex
	dispatches  []executorDispatchCall
	dispatchErr error
}

type executorDispatchCall struct {
	tasks        []analyzer.Task
	taskType     natsbus.TaskType
	jobID        string
	collectionID string
}

func (d *executorTestDispatcher) Dispatch(_ context.Context, tasks []analyzer.Task, taskType natsbus.TaskType, jobID, collectionID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dispatchErr != nil {
		return d.dispatchErr
	}
	d.dispatches = append(d.dispatches, executorDispatchCall{tasks: tasks, taskType: taskType, jobID: jobID, collectionID: collectionID})
	return nil
}

func (d *executorTestDispatcher) Redispatch(_ context.Context, _ analyzer.Task, _ natsbus.TaskType, _, _ string, _ int) error {
	return nil
}

// executorTestCollector returns configurable results.
type executorTestCollector struct {
	mu        sync.Mutex
	collects  []analysis.CollectRequest
	resultsFn func(req analysis.CollectRequest) ([]analyzer.TaskResult, error)
}

func (c *executorTestCollector) PrepareCollect(ctx context.Context, req analysis.CollectRequest) (*analysis.CollectionHandle, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.collects = append(c.collects, req)
	return &analysis.CollectionHandle{
		Req: req,
	}, nil
}

func (c *executorTestCollector) AwaitResults(ctx context.Context, handle *analysis.CollectionHandle) ([]analyzer.TaskResult, error) {
	req := handle.Req
	if c.resultsFn != nil {
		return c.resultsFn(req)
	}
	// Default: return success for all expected IDs.
	taskResults := make([]analyzer.TaskResult, len(req.ExpectedIDs))
	for i, id := range req.ExpectedIDs {
		taskResults[i] = analyzer.TaskResult{TaskID: id, TaskType: string(req.TaskType)}
	}
	return taskResults, nil
}

// fakeCandidateGen records Generate calls and, via dispatchCountFn, the number
// of analyzer dispatches observed at call time — enough to prove the executor
// runs it between the pre-candidate and candidate-dependent engine passes.
type fakeCandidateGen struct {
	mu                  sync.Mutex
	called              bool
	callCount           int
	dispatchCountAtCall int
	err                 error
	dispatchCountFn     func() int
}

func (f *fakeCandidateGen) Generate(_ context.Context, _, _ string, _ []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called = true
	f.callCount++
	if f.dispatchCountFn != nil {
		f.dispatchCountAtCall = f.dispatchCountFn()
	}
	return f.err
}

func (f *fakeCandidateGen) wasCalled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.called
}

func (f *fakeCandidateGen) dispatchesBeforeCall() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dispatchCountAtCall
}

// registerTwoPassAnalyzers registers a classify -> embedding -> {requires,
// relationship} DAG. requires/relationship are candidate-dependent, so they
// form the post-candidate pass; classify/embedding form the pre-candidate one.
// Each analyzer emits one task so dispatches are countable.
//
// All four are registered as Analyzer[*pb.Control], matching production.
// classify/embedding run against a job whose catalog has a control (see
// twoPassJob) so their per-control GenerateWork actually fires; with no
// controls (a zero-control catalog or no controls reader wired) Controls is
// empty and they correctly produce zero tasks (see "with no catalog controls
// reader wired, leaves per-control analyzers at zero tasks instead of
// erroring" below). requires/relationship always run with Controls empty and
// a placeholder *pb.Control Input, however many controls the catalog has,
// since they read candidate pairs directly instead of fanning out per
// control.
func registerTwoPassAnalyzers(reg *analyzer.Registry) {
	mkControl := func(name string) func(context.Context, *pb.Control, analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
		return func(_ context.Context, _ *pb.Control, _ analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
			return []analyzer.Task{{TaskID: name + "-task", Payload: &emptypb.Empty{}}}, nil
		}
	}
	stub := func(name string, deps ...string) *analyzertest.Stub[*pb.Control] {
		s := newControlStub(name, deps...)
		s.GenerateWorkFunc = mkControl(name)
		return s
	}
	Expect(analyzer.Register[*pb.Control](reg, stub("classify"))).To(Succeed())
	Expect(analyzer.Register[*pb.Control](reg, stub("embedding", "classify"))).To(Succeed())
	Expect(analyzer.Register[*pb.Control](reg, stub("requires", "embedding"))).To(Succeed())
	Expect(analyzer.Register[*pb.Control](reg, stub("relationship", "embedding"))).To(Succeed())
}

// catalogJobConfig is the stored config of a job over catalog cat-1, in the
// shape CreateJob writes (encoding/json of pb.JobConfig).
var catalogJobConfig = []byte(`{"catalog_id":"cat-1"}`)

var _ = Describe("Executor", func() {
	var (
		ctx       context.Context
		store     *fakeStore
		registry  *analyzer.Registry
		engine    *analysis.Engine
		synth     *fakeSynthesisExecutor
		attestor  *fakeAttestation
		converter *pipelineattestation.Converter
		bus       *natsbustest.Fake
		storage   *storagetest.Fake
		svc       *pipeline.Service
		tenantID  string
		jobID     string
	)

	BeforeEach(func() {
		ctx = context.Background()
		tenantID = "test-tenant"
		jobID = "test-job-001"

		ctx, _ = tenant.WithTenant(ctx, tenantID)

		store = newFakeStore()
		registry = analyzer.NewRegistry()
		synth = &fakeSynthesisExecutor{}
		attestor = newFakeAttestation()
		converter = pipelineattestation.NewConverter()
		bus = &natsbustest.Fake{}
		storage = &storagetest.Fake{}

		// Register stub analyzers.
		a1 := newStub("analyzer-a")
		a2 := newStub("analyzer-b", "analyzer-a")

		Expect(analyzer.Register[*emptypb.Empty](registry, a1)).To(Succeed())
		Expect(analyzer.Register[*emptypb.Empty](registry, a2)).To(Succeed())

		// Build engine with fake dispatcher/collector.
		dispatcher := &executorTestDispatcher{}
		collector := &executorTestCollector{}
		taskTypes := map[string]natsbus.TaskType{
			"analyzer-a": "classify",
			"analyzer-b": "relate",
		}
		engineCfg := config.EngineConfig{
			TaskTimeout:  5 * time.Minute,
			MaxRetries:   3,
			RetryBackoff: 1 * time.Second,
		}
		// Results reach the store as in production, where the pipeline
		// signs each analyzer's link over its stored result.
		engine = analysis.New(registry, dispatcher, collector, engineCfg, taskTypes,
			analysis.WithStageReporter(pipeline.NewDBStageReporter(analysis.NewNATSStageReporter(bus), store)))

		// Create service.
		pipelineCfg := config.PipelineConfig{
			MaxConcurrentJobs: 10,
		}
		svc = pipeline.New(store, engine, registry, synth, attestedBy(attestor), converter, bus, storage, pipelineCfg, nil)
	})

	// jobStages are the stages of a job over the BeforeEach analyzers.
	jobStages := []string{"analyzer-a", "analyzer-b", "synthesis", "graph"}
	// newRunningJob is jobID over catalog cat-1, as a resume finds it.
	newRunningJob := func() *pipeline.Job {
		return &pipeline.Job{
			JobID: jobID, TenantID: tenantID, Status: pipeline.JobStatusRunning,
			Config: catalogJobConfig, CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}
	}
	jobStatusOf := func(id string) func() pipeline.JobStatus {
		return func() pipeline.JobStatus {
			j, err := store.GetJob(ctx, id)
			if err != nil {
				return ""
			}
			return j.Status
		}
	}

	Describe("executeJob", func() {
		// A job keeps the stages it was created with. When the daemon's
		// enabled analyzers no longer match them (an analyzer was disabled or
		// enabled between creation and a resume), running would either hang
		// on a stage nothing executes or run an analyzer with no stage row,
		// so the job fails before any analyzer runs, saying what changed.
		DescribeTable("fails a resumed job whose analyzers no longer match the daemon's, running no analyzer and leaving no stage running",
			func(createdWith []string, interrupted string, daemonRuns []string, want ...string) {
				deps := map[string][]string{"embedding": {"classify"}, "requires": {"embedding"}, "relationship": {"embedding"}}
				reg := analyzer.NewRegistry()
				var stubs []*analyzertest.Stub[*pb.Control]
				for _, name := range daemonRuns {
					stub := newControlStub(name, deps[name]...)
					stubs = append(stubs, stub)
					Expect(analyzer.Register[*pb.Control](reg, stub)).To(Succeed())
				}
				eng := analysis.New(reg, &executorTestDispatcher{}, &executorTestCollector{},
					config.EngineConfig{TaskTimeout: time.Minute, MaxRetries: 1, RetryBackoff: time.Millisecond},
					map[string]natsbus.TaskType{})
				svc = pipeline.New(store, eng, reg, synth, pipeline.Attestors{}, converter, bus, storage,
					config.PipelineConfig{MaxConcurrentJobs: 10}, nil,
					pipeline.WithCatalogControlsReader(&fakeCatalogControlsReader{controls: []*pb.Control{{ControlId: "c1", CatalogId: "cat-1"}}}))
				DeferCleanup(func() { Expect(svc.Stop(context.Background())).To(Succeed()) })

				Expect(store.CreateJob(ctx, &pipeline.Job{
					JobID: jobID, TenantID: tenantID, Status: pipeline.JobStatusRunning,
					Config: catalogJobConfig, CreatedAt: time.Now(), UpdatedAt: time.Now(),
				})).To(Succeed())
				Expect(store.CreateStages(ctx, jobID, createdWith)).To(Succeed())
				if interrupted != "" {
					// A shutdown interrupted this stage in the job's last run.
					Expect(store.UpdateStageStatus(ctx, jobID, interrupted, pipeline.StageStatusRunning)).To(Succeed())
				}

				Expect(svc.Start(ctx)).To(Succeed())

				Eventually(func() pipeline.JobStatus {
					j, _ := store.GetJob(ctx, jobID)
					if j == nil {
						return ""
					}
					return j.Status
				}).Should(Equal(pipeline.JobStatusFailed))
				j, err := store.GetJob(ctx, jobID)
				Expect(err).NotTo(HaveOccurred())
				for _, w := range want {
					Expect(j.ErrorMessage).To(ContainSubstring(w))
				}
				// A failed job is never resumed, so the only remedy offered
				// is a new job.
				Expect(j.ErrorMessage).To(ContainSubstring(`The job is marked failed and is not resumed. ` +
					`Start a new job with the analyzers enabled now: crosscodex run start --source cat-1 --target cat-1. `))
				Expect(j.ErrorMessage).NotTo(Or(ContainSubstring("restart"), ContainSubstring("resume it"), ContainSubstring(" back ")))

				stages, err := store.GetStages(ctx, jobID)
				Expect(err).NotTo(HaveOccurred())
				Expect(stages).To(HaveLen(len(createdWith)))
				for _, st := range stages {
					if st.StageName == interrupted {
						Expect(st.Status).To(Equal(pipeline.StageStatusFailed), "interrupted stage %s", st.StageName)
						Expect(st.ErrorMessage).To(ContainSubstring("did not finish: the job failed: job test-job-001 cannot run"))
						continue
					}
					Expect(st.Status).To(Equal(pipeline.StageStatusPending), "stage %s", st.StageName)
				}
				for _, stub := range stubs {
					Expect(stub.Inputs()).To(BeEmpty(), "analyzer %s ran", stub.Name())
				}
			},
			Entry("relationship disabled since the job was created",
				[]string{"classify", "embedding", "candidate_generation", "relationship", "synthesis", "graph"}, "",
				[]string{"classify", "embedding"},
				`job test-job-001 cannot run: analyzer "relationship" ran when the job was created, but analysis.relationship.enabled is now false`,
				`To give the new job the analyzers this one was created with, first set analysis.relationship.enabled to true`),
			Entry("requires enabled since the job was created",
				[]string{"classify", "embedding", "synthesis", "graph"}, "",
				[]string{"classify", "embedding", "requires"},
				`job test-job-001 cannot run: analyzer "requires" did not run when the job was created, but analysis.requires.enabled is now true`,
				`first set analysis.requires.enabled to false`),
			Entry("one analyzer disabled and another enabled",
				[]string{"classify", "embedding", "candidate_generation", "requires", "synthesis", "graph"}, "",
				[]string{"classify", "embedding", "relationship"},
				`analyzer "requires" ran when the job was created, but analysis.requires.enabled is now false`,
				`analyzer "relationship" did not run when the job was created, but analysis.relationship.enabled is now true`,
				`first set analysis.requires.enabled to true and analysis.relationship.enabled to false`),
			Entry("artifacts, defaulting to off, after an upgrade interrupted it",
				[]string{"classify", "embedding", "artifacts", "synthesis", "graph"}, "artifacts",
				[]string{"classify", "embedding"},
				`analyzer "artifacts" ran when the job was created, but analysis.artifacts.enabled is now false`),
		)

		It("recovers from a panic in job execution and fails the job", func() {
			configBytes := catalogJobConfig

			job := &pipeline.Job{
				JobID:     jobID,
				TenantID:  tenantID,
				Status:    pipeline.JobStatusRunning,
				Config:    configBytes,
				CreatedAt: time.Now(),
				UpdatedAt: time.Now(),
			}
			Expect(store.CreateJob(ctx, job)).To(Succeed())
			stageNames := []string{"analyzer-a", "analyzer-b", "synthesis", "graph"}
			Expect(store.CreateStages(ctx, jobID, stageNames)).To(Succeed())

			// GetStages panics inside the executeJob goroutine. Without recover() the
			// panic would crash the whole test binary; with recover() the job is
			// marked failed and the process survives.
			store.getStagesPanics = true

			rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()

			Expect(svc.Start(rctx)).To(Succeed())

			Eventually(func() pipeline.JobStatus {
				j, _ := store.GetJob(rctx, jobID)
				if j == nil {
					return ""
				}
				return j.Status
			}, "1s", "50ms").Should(Equal(pipeline.JobStatusFailed))
		})

		It("runs analysis, synthesis, and graph phases in order", func() {
			// Create job in store.
			configBytes := catalogJobConfig

			job := &pipeline.Job{
				JobID:     jobID,
				TenantID:  tenantID,
				Status:    pipeline.JobStatusPending,
				Config:    configBytes,
				CreatedAt: time.Now(),
				UpdatedAt: time.Now(),
			}
			Expect(store.CreateJob(ctx, job)).To(Succeed())

			// Create stages.
			stageNames := []string{"analyzer-a", "analyzer-b", "synthesis", "graph"}
			Expect(store.CreateStages(ctx, jobID, stageNames)).To(Succeed())

			// Execute.
			ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()

			// Call executeJob via reflection (it's unexported).
			// For testing, we'll create a test job via CreateJob RPC which triggers executeJob.
			// Here, we test the phases indirectly by checking store state.

			// Instead, let's trigger via Start (which resumes jobs).
			job.Status = pipeline.JobStatusRunning
			store.jobs[jobID] = job

			err := svc.Start(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Wait for job to complete.
			Eventually(func() pipeline.JobStatus {
				j, _ := store.GetJob(ctx, jobID)
				if j == nil {
					return ""
				}
				return j.Status
			}, "1s", "50ms").Should(Equal(pipeline.JobStatusCompleted))

			// Verify synthesis was called.
			Expect(synth.execCount()).To(Equal(1))

			// Verify NATS events published (running, stage events, completed).
			Expect(bus.Published()).NotTo(BeEmpty())
			Expect(bus.Subscriptions()).To(BeEmpty(), "job execution only publishes")
		})

		It("publishes its synthesis stage event in the shared stage schema", func() {
			Expect(store.CreateJob(ctx, &pipeline.Job{
				JobID:     jobID,
				TenantID:  tenantID,
				Status:    pipeline.JobStatusRunning,
				Config:    catalogJobConfig,
				CreatedAt: time.Now(),
				UpdatedAt: time.Now(),
			})).To(Succeed())
			Expect(store.CreateStages(ctx, jobID, []string{"analyzer-a", "analyzer-b", "synthesis", "graph"})).To(Succeed())

			ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			Expect(svc.Start(ctx)).To(Succeed())

			Eventually(func() pipeline.JobStatus {
				j, _ := store.GetJob(ctx, jobID)
				if j == nil {
					return ""
				}
				return j.Status
			}, "1s", "50ms").Should(Equal(pipeline.JobStatusCompleted))

			subject, err := natsbus.PipelineStageSubject(tenantID, jobID, natsbus.StageCompleted)
			Expect(err).NotTo(HaveOccurred())
			var event map[string]any
			for _, p := range bus.Published() {
				if p.Subject != subject {
					continue
				}
				var candidate map[string]any
				Expect(json.Unmarshal(p.Data, &candidate)).To(Succeed())
				if candidate["analyzer"] == "synthesis" {
					event = candidate
				}
			}
			Expect(event).NotTo(BeNil(), "no synthesis event published on %s", subject)

			Expect(event).To(HaveKeyWithValue("analyzer", "synthesis"))
			Expect(event).To(HaveKeyWithValue("job_id", jobID))
			Expect(event).To(HaveKeyWithValue("stage", "completed"))
			Expect(event).NotTo(HaveKey("stage_name"))
			ts, ok := event["timestamp"].(string)
			Expect(ok).To(BeTrue(), "timestamp must be an RFC3339 string, got %T", event["timestamp"])
			_, err = time.Parse(time.RFC3339, ts)
			Expect(err).NotTo(HaveOccurred())
		})

		It("skips completed stages on resume", func() {
			// Create job with some stages already completed.
			configBytes := catalogJobConfig

			job := &pipeline.Job{
				JobID:     jobID,
				TenantID:  tenantID,
				Status:    pipeline.JobStatusRunning,
				Config:    configBytes,
				CreatedAt: time.Now(),
				UpdatedAt: time.Now(),
			}
			Expect(store.CreateJob(ctx, job)).To(Succeed())

			// Create stages with analyzer-a already completed.
			stageNames := []string{"analyzer-a", "analyzer-b", "synthesis", "graph"}
			Expect(store.CreateStages(ctx, jobID, stageNames)).To(Succeed())

			// Mark analyzer-a as completed.
			Expect(store.UpdateStageStatus(ctx, jobID, "analyzer-a", pipeline.StageStatusCompleted)).To(Succeed())

			// Start service (resumes job).
			ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()

			err := svc.Start(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Wait for completion.
			Eventually(func() pipeline.JobStatus {
				j, _ := store.GetJob(ctx, jobID)
				if j == nil {
					return ""
				}
				return j.Status
			}, "1s", "50ms").Should(Equal(pipeline.JobStatusCompleted))
		})

		It("marks job as failed when analysis fails", func() {
			// Create a registry with analyzers that generate tasks (so dispatch is called).
			// Registered as Analyzer[*pb.Control] (matching production) and
			// exercised against a job whose catalog has one control, so
			// GenerateWork actually fires and dispatch is reached — with no
			// controls, Controls would be empty and correctly produce zero
			// tasks, never reaching the failing dispatcher.
			failRegistry := analyzer.NewRegistry()
			genWork := func(_ context.Context, _ *pb.Control, _ analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
				return []analyzer.Task{{TaskID: "t1", Payload: &emptypb.Empty{}}}, nil
			}
			fa1 := newControlStub("analyzer-a")
			fa1.GenerateWorkFunc = genWork
			fa2 := newControlStub("analyzer-b", "analyzer-a")
			fa2.GenerateWorkFunc = genWork
			Expect(analyzer.Register[*pb.Control](failRegistry, fa1)).To(Succeed())
			Expect(analyzer.Register[*pb.Control](failRegistry, fa2)).To(Succeed())

			// Create engine with failing dispatcher.
			dispatcher := &executorTestDispatcher{dispatchErr: errors.New("dispatch failure")}
			collector := &executorTestCollector{}
			taskTypes := map[string]natsbus.TaskType{
				"analyzer-a": "classify",
				"analyzer-b": "relate",
			}
			engineCfg := config.EngineConfig{
				TaskTimeout:  5 * time.Minute,
				MaxRetries:   3,
				RetryBackoff: 1 * time.Second,
			}
			failEngine := analysis.New(failRegistry, dispatcher, collector, engineCfg, taskTypes)

			pipelineCfg := config.PipelineConfig{MaxConcurrentJobs: 10}
			catalogReader := &fakeCatalogControlsReader{controls: []*pb.Control{
				{ControlId: "c1", CatalogId: "fail-cat-1"},
			}}
			failSvc := pipeline.New(store, failEngine, failRegistry, synth, pipeline.Attestors{}, converter, bus, storage, pipelineCfg, nil,
				pipeline.WithCatalogControlsReader(catalogReader))

			// Create job.
			configBytes := []byte(`{"catalog_id":"fail-cat-1"}`)

			job := &pipeline.Job{
				JobID:     jobID,
				TenantID:  tenantID,
				Status:    pipeline.JobStatusRunning,
				Config:    configBytes,
				CreatedAt: time.Now(),
				UpdatedAt: time.Now(),
			}
			Expect(store.CreateJob(ctx, job)).To(Succeed())

			stageNames := []string{"analyzer-a", "analyzer-b", "synthesis", "graph"}
			Expect(store.CreateStages(ctx, jobID, stageNames)).To(Succeed())

			// Start service.
			ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()

			err := failSvc.Start(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Wait for failure.
			Eventually(func() pipeline.JobStatus {
				j, _ := store.GetJob(ctx, jobID)
				if j == nil {
					return ""
				}
				return j.Status
			}, "1s", "50ms").Should(Equal(pipeline.JobStatusFailed))
		})

		It("publishes NATS state events for each transition", func() {
			// Create job.
			configBytes := catalogJobConfig

			job := &pipeline.Job{
				JobID:     jobID,
				TenantID:  tenantID,
				Status:    pipeline.JobStatusRunning,
				Config:    configBytes,
				CreatedAt: time.Now(),
				UpdatedAt: time.Now(),
			}
			Expect(store.CreateJob(ctx, job)).To(Succeed())

			stageNames := []string{"analyzer-a", "analyzer-b", "synthesis", "graph"}
			Expect(store.CreateStages(ctx, jobID, stageNames)).To(Succeed())

			// Start service.
			ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()

			err := svc.Start(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Wait for completion.
			Eventually(func() pipeline.JobStatus {
				j, _ := store.GetJob(ctx, jobID)
				if j == nil {
					return ""
				}
				return j.Status
			}, "1s", "50ms").Should(Equal(pipeline.JobStatusCompleted))

			// Verify state events published.
			// Expect at least: running, synthesis.started, synthesis.completed, graph.started, graph.completed, completed.
			Expect(len(bus.Published())).To(BeNumerically(">=", 6))
			Expect(bus.Subscriptions()).To(BeEmpty(), "job execution only publishes")
		})

		It("fails the job before any analyzer runs when its attestation layout cannot be signed", func() {
			attestor.layoutErr = errors.New("signer unavailable")
			Expect(store.CreateJob(ctx, newRunningJob())).To(Succeed())
			Expect(store.CreateStages(ctx, jobID, jobStages)).To(Succeed())

			Expect(svc.Start(ctx)).To(Succeed())
			DeferCleanup(func() { Expect(svc.Stop(context.Background())).To(Succeed()) })

			Eventually(jobStatusOf(jobID)).Should(Equal(pipeline.JobStatusFailed))
			job, err := store.GetJob(ctx, jobID)
			Expect(err).NotTo(HaveOccurred())
			Expect(job.ErrorMessage).To(And(
				ContainSubstring("attestation layout could not be signed"),
				ContainSubstring("signer unavailable"),
				ContainSubstring("No analyzer ran"),
				ContainSubstring("attestation.private_key_path"),
				ContainSubstring("crosscodex run start --source cat-1 --target cat-1")))
			stages, err := store.GetStages(ctx, jobID)
			Expect(err).NotTo(HaveOccurred())
			Expect(stages).To(HaveEach(HaveField("Status", pipeline.StageStatusPending)))
			Expect(synth.executions).To(BeEmpty())
			Expect(storage.Objects()).To(BeEmpty())
		})

		It("creates layout before analysis", func() {
			// Create job.
			configBytes := catalogJobConfig

			job := &pipeline.Job{
				JobID:     jobID,
				TenantID:  tenantID,
				Status:    pipeline.JobStatusRunning,
				Config:    configBytes,
				CreatedAt: time.Now(),
				UpdatedAt: time.Now(),
			}
			Expect(store.CreateJob(ctx, job)).To(Succeed())

			stageNames := []string{"analyzer-a", "analyzer-b", "synthesis", "graph"}
			Expect(store.CreateStages(ctx, jobID, stageNames)).To(Succeed())

			// Start service.
			ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()

			err := svc.Start(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Wait for completion.
			Eventually(func() pipeline.JobStatus {
				j, _ := store.GetJob(ctx, jobID)
				if j == nil {
					return ""
				}
				return j.Status
			}, "1s", "50ms").Should(Equal(pipeline.JobStatusCompleted))

			// Verify layout was created.
			Expect(attestor.layoutCalls).NotTo(BeEmpty())

			// Verify layout stored in object store.
			layoutPath := fmt.Sprintf("attestations/%s/%s/layout.json", tenantID, jobID)
			exists, err := storage.Exists(ctx, layoutPath)
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeTrue())
		})

		It("creates link after each stage completes", func() {
			// Create job.
			configBytes := catalogJobConfig

			job := &pipeline.Job{
				JobID:     jobID,
				TenantID:  tenantID,
				Status:    pipeline.JobStatusRunning,
				Config:    configBytes,
				CreatedAt: time.Now(),
				UpdatedAt: time.Now(),
			}
			Expect(store.CreateJob(ctx, job)).To(Succeed())

			stageNames := []string{"analyzer-a", "analyzer-b", "synthesis", "graph"}
			Expect(store.CreateStages(ctx, jobID, stageNames)).To(Succeed())

			// Start service.
			ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()

			err := svc.Start(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Wait for completion.
			Eventually(func() pipeline.JobStatus {
				j, _ := store.GetJob(ctx, jobID)
				if j == nil {
					return ""
				}
				return j.Status
			}, "1s", "50ms").Should(Equal(pipeline.JobStatusCompleted))

			// Verify links created for each stage.
			// Expect at least 4 links: analyzer-a, analyzer-b, synthesis, graph.
			Expect(attestor.linkCount()).To(BeNumerically(">=", 4))

			// Verify links stored in object store.
			for _, stageName := range []string{"analyzer-a", "analyzer-b", "synthesis", "graph"} {
				linkPath := fmt.Sprintf("attestations/%s/%s/links/%s.json", tenantID, jobID, stageName)
				exists, err := storage.Exists(ctx, linkPath)
				Expect(err).NotTo(HaveOccurred())
				Expect(exists).To(BeTrue())
			}
		})

		It("verifies chain and stores bundle on finalization", func() {
			// Create job.
			configBytes := catalogJobConfig

			job := &pipeline.Job{
				JobID:     jobID,
				TenantID:  tenantID,
				Status:    pipeline.JobStatusRunning,
				Config:    configBytes,
				CreatedAt: time.Now(),
				UpdatedAt: time.Now(),
			}
			Expect(store.CreateJob(ctx, job)).To(Succeed())

			stageNames := []string{"analyzer-a", "analyzer-b", "synthesis", "graph"}
			Expect(store.CreateStages(ctx, jobID, stageNames)).To(Succeed())

			// Start service.
			ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()

			err := svc.Start(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Wait for completion.
			Eventually(func() pipeline.JobStatus {
				j, _ := store.GetJob(ctx, jobID)
				if j == nil {
					return ""
				}
				return j.Status
			}, "1s", "50ms").Should(Equal(pipeline.JobStatusCompleted))

			// Verify bundle stored.
			bundlePath := fmt.Sprintf("attestations/%s/%s/bundle.json", tenantID, jobID)
			exists, err := storage.Exists(ctx, bundlePath)
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeTrue())

			// Verify bundle contents.
			reader, err := storage.Get(ctx, bundlePath)
			Expect(err).NotTo(HaveOccurred())
			defer reader.Close()

			var bundle map[string]interface{}
			err = json.NewDecoder(reader).Decode(&bundle)
			Expect(err).NotTo(HaveOccurred())
			Expect(bundle).To(HaveKey("layout"))
			Expect(bundle).To(HaveKey("links"))
		})

		It("marks job as failed when synthesis execution fails", func() {
			// Wire a synthesis executor that returns an error.
			failSynth := &fakeSynthesisExecutor{execErr: errors.New("synthesis failed")}
			failSvc := pipeline.New(store, engine, registry, failSynth, pipeline.Attestors{}, converter, bus, storage,
				config.PipelineConfig{MaxConcurrentJobs: 10},
				nil,
			)

			// Create job.
			configBytes := catalogJobConfig

			job := &pipeline.Job{
				JobID:     jobID,
				TenantID:  tenantID,
				Status:    pipeline.JobStatusRunning,
				Config:    configBytes,
				CreatedAt: time.Now(),
				UpdatedAt: time.Now(),
			}
			Expect(store.CreateJob(ctx, job)).To(Succeed())

			stageNames := []string{"analyzer-a", "analyzer-b", "synthesis", "graph"}
			Expect(store.CreateStages(ctx, jobID, stageNames)).To(Succeed())

			// Start service.
			ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()

			err := failSvc.Start(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Wait for failure.
			Eventually(func() pipeline.JobStatus {
				j, _ := store.GetJob(ctx, jobID)
				if j == nil {
					return ""
				}
				return j.Status
			}, "1s", "50ms").Should(Equal(pipeline.JobStatusFailed))
		})

		It("fails the job, storing no bundle, when its attestation chain does not verify", func() {
			attestor.verifyErr = fmt.Errorf("chain broken between step %q and step %q: %w", "analyzer-a", "analyzer-b", attestation.ErrChainBroken)
			Expect(store.CreateJob(ctx, newRunningJob())).To(Succeed())
			Expect(store.CreateStages(ctx, jobID, jobStages)).To(Succeed())

			Expect(svc.Start(ctx)).To(Succeed())
			DeferCleanup(func() { Expect(svc.Stop(context.Background())).To(Succeed()) })

			Eventually(jobStatusOf(jobID)).Should(Equal(pipeline.JobStatusFailed))
			job, err := store.GetJob(ctx, jobID)
			Expect(err).NotTo(HaveOccurred())
			Expect(job.ErrorMessage).To(And(
				ContainSubstring("its attestation does not verify"),
				ContainSubstring(`step "analyzer-a" and step "analyzer-b"`),
				ContainSubstring("crosscodex run start --source cat-1 --target cat-1")))
			exists, err := storage.Exists(ctx, fmt.Sprintf("attestations/%s/%s/bundle.json", tenantID, jobID))
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeFalse())
			stages, err := store.GetStages(ctx, jobID)
			Expect(err).NotTo(HaveOccurred())
			Expect(stages).To(HaveEach(HaveField("Status", pipeline.StageStatusCompleted)))
		})

		Describe("attestation", func() {
			bundleKey := func() string { return fmt.Sprintf("attestations/%s/%s/bundle.json", tenantID, jobID) }
			linkKey := func(step string) string {
				return fmt.Sprintf("attestations/%s/%s/links/%s.json", tenantID, jobID, step)
			}
			// serviceWith returns a Service over the BeforeEach analyzers
			// that attests with attestors and stores objects in stor.
			serviceWith := func(attestors pipeline.Attestors, stor storagepkg.Provider) *pipeline.Service {
				s := pipeline.New(store, engine, registry, synth, attestors, converter, bus, stor,
					config.PipelineConfig{MaxConcurrentJobs: 10}, nil)
				DeferCleanup(func() { Expect(s.Stop(context.Background())).To(Succeed()) })
				return s
			}
			runJob := func(s *pipeline.Service) {
				Expect(store.CreateJob(ctx, newRunningJob())).To(Succeed())
				Expect(store.CreateStages(ctx, jobID, jobStages)).To(Succeed())
				Expect(s.Start(ctx)).To(Succeed())
			}
			errorOf := func() string {
				job, err := store.GetJob(ctx, jobID)
				Expect(err).NotTo(HaveOccurred())
				return job.ErrorMessage
			}
			stageStatuses := func() map[string]pipeline.StageStatus {
				stages, err := store.GetStages(ctx, jobID)
				Expect(err).NotTo(HaveOccurred())
				out := make(map[string]pipeline.StageStatus, len(stages))
				for _, st := range stages {
					out[st.StageName] = st.Status
				}
				return out
			}
			allCompleted := map[string]pipeline.StageStatus{
				"analyzer-a": pipeline.StageStatusCompleted, "analyzer-b": pipeline.StageStatusCompleted,
				"synthesis": pipeline.StageStatusCompleted, "graph": pipeline.StageStatusCompleted,
			}

			It("stores a bundle that verifies, with one link per step, each analyzer's signed over its stored result", func() {
				runJob(svc)
				DeferCleanup(func() { Expect(svc.Stop(context.Background())).To(Succeed()) })
				Eventually(jobStatusOf(jobID)).Should(Equal(pipeline.JobStatusCompleted))

				rc, err := storage.Get(ctx, bundleKey())
				Expect(err).NotTo(HaveOccurred())
				data, err := io.ReadAll(rc)
				Expect(err).NotTo(HaveOccurred())
				Expect(rc.Close()).To(Succeed())
				layout, links, err := attestation.ParseBundle(data)
				Expect(err).NotTo(HaveOccurred())
				configSum := sha256.Sum256(catalogJobConfig)
				subject := attestation.Artifact{
					URI:    fmt.Sprintf("job:%s/%s", tenantID, jobID),
					Digest: "sha256:" + hex.EncodeToString(configSum[:]),
				}
				chain, err := attestor.Generator.VerifyChain(ctx, layout, links, subject)
				Expect(err).NotTo(HaveOccurred())
				Expect(chain.Subject).To(Equal(subject))
				Expect(links).To(HaveLen(4))

				stored, err := store.GetCompletedAnalysisResults(ctx, jobID, []string{"analyzer-a"})
				Expect(err).NotTo(HaveOccurred())
				sum := sha256.Sum256(stored["analyzer-a"].ResultData)
				link, err := storage.Get(ctx, linkKey("analyzer-a"))
				Expect(err).NotTo(HaveOccurred())
				raw, err := io.ReadAll(link)
				Expect(err).NotTo(HaveOccurred())
				Expect(link.Close()).To(Succeed())
				verified, err := attestor.Verify(ctx, raw)
				Expect(err).NotTo(HaveOccurred())
				Expect(verified.Products).To(ConsistOf(attestation.Artifact{
					URI: "analyzer-a.output", Digest: "sha256:" + hex.EncodeToString(sum[:]),
				}))
			})

			DescribeTable("runs the jobs of a tenant without attestation unattested, writing no attestation object",
				func(attestors func() pipeline.Attestors) {
					runJob(serviceWith(attestors(), storage))
					Eventually(jobStatusOf(jobID)).Should(Equal(pipeline.JobStatusCompleted))
					Expect(attestor.layoutCalls).To(BeEmpty())
					Expect(storage.Objects()).To(BeEmpty())
					Expect(synth.executions).To(HaveLen(1))
				},
				Entry("when attestation is disabled", func() pipeline.Attestors { return pipeline.Attestors{} }),
				Entry("when the tenant's override disables it", func() pipeline.Attestors {
					a := attestedBy(attestor)
					a.Tenants = map[string]*pipeline.Attestor{tenantID: nil}
					return a
				}),
			)

			It("attests the jobs of a tenant whose override enables attestation the global settings disable", func() {
				runJob(serviceWith(pipeline.Attestors{Tenants: map[string]*pipeline.Attestor{
					tenantID: {Generator: attestor, ExpiresIn: time.Hour},
				}}, storage))
				Eventually(jobStatusOf(jobID)).Should(Equal(pipeline.JobStatusCompleted))
				exists, err := storage.Exists(ctx, bundleKey())
				Expect(err).NotTo(HaveOccurred())
				Expect(exists).To(BeTrue())
			})

			It("fails the synthesis stage and the job when the synthesis link cannot be signed, and runs no later stage", func() {
				attestor.linkErr = errors.New("signer unavailable")
				attestor.linkErrOn = "synthesis"
				runJob(svc)
				DeferCleanup(func() { Expect(svc.Stop(context.Background())).To(Succeed()) })

				Eventually(jobStatusOf(jobID)).Should(Equal(pipeline.JobStatusFailed))
				Expect(errorOf()).To(And(
					ContainSubstring(`the attestation link for step "synthesis" could not be signed`),
					ContainSubstring("signer unavailable"),
					ContainSubstring("crosscodex run retry --from-failure test-job-001"),
					ContainSubstring("crosscodex run start --source cat-1 --target cat-1")))
				Expect(stageStatuses()).To(Equal(map[string]pipeline.StageStatus{
					"analyzer-a": pipeline.StageStatusCompleted, "analyzer-b": pipeline.StageStatusCompleted,
					"synthesis": pipeline.StageStatusFailed, "graph": pipeline.StageStatusPending,
				}))
				Expect(storage.Objects()).NotTo(HaveKey(bundleKey()))
				Expect(storage.Objects()).NotTo(HaveKey(linkKey("synthesis")))
			})

			It("fails the job before synthesis when an analyzer's link cannot be signed", func() {
				attestor.linkErr = errors.New("signer unavailable")
				attestor.linkErrOn = "analyzer-b"
				runJob(svc)
				DeferCleanup(func() { Expect(svc.Stop(context.Background())).To(Succeed()) })

				Eventually(jobStatusOf(jobID)).Should(Equal(pipeline.JobStatusFailed))
				Expect(errorOf()).To(ContainSubstring(`the attestation link for step "analyzer-b" could not be signed`))
				Expect(synth.executions).To(BeEmpty())
				Expect(stageStatuses()).To(HaveKeyWithValue("synthesis", pipeline.StageStatusPending))
				Expect(storage.Objects()).NotTo(HaveKey(bundleKey()))
			})

			It("retries a failed bundle write and completes the job once storage accepts it", func() {
				flaky := &flakyBundleStorage{Fake: storage}
				flaky.failures.Store(2)
				runJob(serviceWith(attestedBy(attestor), flaky))

				Eventually(jobStatusOf(jobID)).Should(Equal(pipeline.JobStatusCompleted))
				Expect(flaky.puts.Load()).To(BeEquivalentTo(3))
				Expect(storage.Objects()).To(HaveKey(bundleKey()))
			})

			// cancelWhileRetryingBundle runs the job against storage that
			// refuses every bundle write and waits an hour between attempts,
			// and cancels the job once it is waiting. It returns the
			// service and the storage.
			cancelWhileRetryingBundle := func() (*pipeline.Service, *flakyBundleStorage) {
				flaky := &flakyBundleStorage{Fake: storage}
				flaky.failures.Store(1000)
				attestors := attestedBy(attestor)
				attestors.StorageRetryDelay = time.Hour
				s := serviceWith(attestors, flaky)
				runJob(s)
				Eventually(flaky.puts.Load).Should(BeEquivalentTo(1))
				_, err := s.CancelJob(ctx, connect.NewRequest(&pb.CancelJobRequest{JobId: jobID}))
				Expect(err).NotTo(HaveOccurred())
				Eventually(jobStatusOf(jobID)).Should(Equal(pipeline.JobStatusCancelled))
				return s, flaky
			}

			It("stops waiting to retry a bundle write when the job is cancelled, and ends it cancelled with no bundle", func() {
				_, flaky := cancelWhileRetryingBundle()
				Consistently(jobStatusOf(jobID), "200ms").Should(Equal(pipeline.JobStatusCancelled))
				Expect(flaky.puts.Load()).To(BeEquivalentTo(1))
				Expect(storage.Objects()).NotTo(HaveKey(bundleKey()))
				Expect(stageStatuses()).To(Equal(allCompleted))
			})

			It("resumes a job cancelled after its last stage when retried from its failure, storing only the bundle", func() {
				s, flaky := cancelWhileRetryingBundle()
				flaky.failures.Store(0)

				// The cancelled run may still be stopping; the retry is
				// refused until it has.
				var resp *connect.Response[pb.RetryJobResponse]
				Eventually(func() error {
					var err error
					resp, err = s.RetryJob(ctx, connect.NewRequest(&pb.RetryJobRequest{JobId: jobID, RetryFromFailure: true}))
					return err
				}).Should(Succeed())
				Expect(resp.Msg.NewJobId).To(Equal(jobID))
				Eventually(jobStatusOf(jobID)).Should(Equal(pipeline.JobStatusCompleted))
				Expect(storage.Objects()).To(HaveKey(bundleKey()))
				Expect(synth.executions).To(HaveLen(1))
			})

			It("fails the job with a retryable error when storage keeps refusing the bundle, and retrying it from its failure stores the bundle without rerunning a stage", func() {
				flaky := &flakyBundleStorage{Fake: storage}
				flaky.failures.Store(1000)
				s := serviceWith(attestedBy(attestor), flaky)
				runJob(s)

				Eventually(jobStatusOf(jobID)).Should(Equal(pipeline.JobStatusFailed))
				Expect(flaky.puts.Load()).To(BeEquivalentTo(3))
				Expect(errorOf()).To(And(
					ContainSubstring("its attestation bundle could not be stored at "+strconv.Quote(bundleKey())+" after 3 attempts"),
					ContainSubstring("object store unavailable"),
					ContainSubstring("storage.objects.base_path"),
					ContainSubstring("crosscodex run retry --from-failure test-job-001"),
					ContainSubstring("crosscodex run start --source cat-1 --target cat-1")))
				Expect(stageStatuses()).To(Equal(allCompleted))
				Expect(storage.Objects()).NotTo(HaveKey(bundleKey()))

				flaky.failures.Store(0)
				// The failed run may not have returned yet; until it has, the
				// retry is refused.
				var resp *connect.Response[pb.RetryJobResponse]
				Eventually(func() error {
					var err error
					resp, err = s.RetryJob(ctx, connect.NewRequest(&pb.RetryJobRequest{JobId: jobID, RetryFromFailure: true}))
					return err
				}).Should(Succeed())
				Expect(resp.Msg.NewJobId).To(Equal(jobID))
				Eventually(jobStatusOf(jobID)).Should(Equal(pipeline.JobStatusCompleted))
				Expect(storage.Objects()).To(HaveKey(bundleKey()))
				Expect(synth.executions).To(HaveLen(1))
				stages, err := store.GetStages(ctx, jobID)
				Expect(err).NotTo(HaveOccurred())
				Expect(stages).To(HaveEach(HaveField("RetryCount", 0)))
			})

			// A resumed job does not run its completed pipeline stages again,
			// so it reads back the links an earlier run stored for them.
			DescribeTable("fails a resumed job whose stored pipeline link cannot be trusted, naming the step and the object, and leaves the objects as they are",
				func(alter func(key string), want string) {
					runJob(svc)
					Eventually(jobStatusOf(jobID)).Should(Equal(pipeline.JobStatusCompleted))
					Expect(svc.Stop(ctx)).To(Succeed())

					// Simulate a stop just before the bundle was stored.
					Expect(storage.Delete(ctx, bundleKey())).To(Succeed())
					Expect(store.UpdateJobStatus(ctx, jobID, pipeline.JobStatusRunning, nil)).To(Succeed())
					alter(linkKey("synthesis"))
					before := maps.Clone(storage.Objects())

					runJob2 := serviceWith(attestedBy(attestor), storage)
					Expect(runJob2.Start(ctx)).To(Succeed())

					Eventually(jobStatusOf(jobID)).Should(Equal(pipeline.JobStatusFailed))
					Expect(errorOf()).To(And(
						ContainSubstring(`step "synthesis"`),
						ContainSubstring(strconv.Quote(linkKey("synthesis"))),
						ContainSubstring(want),
						ContainSubstring("crosscodex run start --source cat-1 --target cat-1")))
					Expect(stageStatuses()).To(Equal(allCompleted))
					// The stored links are verified before anything is signed
					// again, so every object is as it was.
					Expect(storage.Objects()).To(Equal(before))
				},
				Entry("a link whose recorded digest was changed after signing", func(key string) {
					var env map[string]any
					Expect(json.Unmarshal(storage.Objects()[key], &env)).To(Succeed())
					payload, err := base64.StdEncoding.DecodeString(env["payload"].(string))
					Expect(err).NotTo(HaveOccurred())
					Expect(string(payload)).To(ContainSubstring("sha256:"))
					env["payload"] = base64.StdEncoding.EncodeToString(bytes.Replace(payload, []byte("sha256:"), []byte("sha256:0"), 1))
					tampered, err := json.Marshal(env)
					Expect(err).NotTo(HaveOccurred())
					Expect(storage.Put(ctx, key, bytes.NewReader(tampered))).To(Succeed())
				}, "does not verify"),
				Entry("a deleted link", func(key string) {
					Expect(storage.Delete(ctx, key)).To(Succeed())
				}, "is missing"),
			)

			It("fails a resumed job whose stored link was replayed from another job of the tenant, leaving every object as it was", func() {
				// Job "donor" completes first, so its synthesis link is
				// genuine: signed with the same key, for the same step.
				donorID := "donor-job"
				donor := newRunningJob()
				donor.JobID = donorID
				Expect(store.CreateJob(ctx, donor)).To(Succeed())
				Expect(store.CreateStages(ctx, donorID, jobStages)).To(Succeed())
				runJob(svc)
				Eventually(jobStatusOf(donorID)).Should(Equal(pipeline.JobStatusCompleted))
				Eventually(jobStatusOf(jobID)).Should(Equal(pipeline.JobStatusCompleted))
				Expect(svc.Stop(ctx)).To(Succeed())

				Expect(storage.Delete(ctx, bundleKey())).To(Succeed())
				Expect(store.UpdateJobStatus(ctx, jobID, pipeline.JobStatusRunning, nil)).To(Succeed())
				donorLink := storage.Objects()[fmt.Sprintf("attestations/%s/%s/links/synthesis.json", tenantID, donorID)]
				Expect(donorLink).NotTo(BeEmpty())
				Expect(storage.Put(ctx, linkKey("synthesis"), bytes.NewReader(donorLink))).To(Succeed())
				before := maps.Clone(storage.Objects())

				resumed := serviceWith(attestedBy(attestor), storage)
				Expect(resumed.Start(ctx)).To(Succeed())

				Eventually(jobStatusOf(jobID)).Should(Equal(pipeline.JobStatusFailed))
				Expect(errorOf()).To(And(
					ContainSubstring(`step "synthesis"`),
					ContainSubstring("another job"),
					ContainSubstring(fmt.Sprintf("job:%s/%s", tenantID, jobID)),
					ContainSubstring("crosscodex run start --source cat-1 --target cat-1")))
				Expect(storage.Objects()).To(Equal(before))
			})
		})

		It("publishes bundle to audit trail", func() {
			// Create job.
			configBytes := catalogJobConfig

			job := &pipeline.Job{
				JobID:     jobID,
				TenantID:  tenantID,
				Status:    pipeline.JobStatusRunning,
				Config:    configBytes,
				CreatedAt: time.Now(),
				UpdatedAt: time.Now(),
			}
			Expect(store.CreateJob(ctx, job)).To(Succeed())

			stageNames := []string{"analyzer-a", "analyzer-b", "synthesis", "graph"}
			Expect(store.CreateStages(ctx, jobID, stageNames)).To(Succeed())

			// Start service.
			ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()

			err := svc.Start(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Wait for completion.
			Eventually(func() pipeline.JobStatus {
				j, _ := store.GetJob(ctx, jobID)
				if j == nil {
					return ""
				}
				return j.Status
			}, "1s", "50ms").Should(Equal(pipeline.JobStatusCompleted))

			// Verify audit event published.
			expectedSubject, err := natsbus.AuditSubject(tenantID, natsbus.AuditEvents, jobID)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() bool {
				return slices.ContainsFunc(bus.Published(), func(p natsbustest.Publication) bool {
					return p.Subject == expectedSubject
				})
			}, "1s", "50ms").Should(BeTrue())
		})
	})

	Describe("two-pass candidate generation", func() {
		var (
			twoPassDispatcher *executorTestDispatcher
			twoPassEngine     *analysis.Engine
			twoPassRegistry   *analyzer.Registry
			twoPassCatalog    *fakeCatalogControlsReader
		)

		BeforeEach(func() {
			twoPassRegistry = analyzer.NewRegistry()
			registerTwoPassAnalyzers(twoPassRegistry)

			twoPassDispatcher = &executorTestDispatcher{}
			collector := &executorTestCollector{}
			taskTypes := map[string]natsbus.TaskType{
				"classify":     "classify",
				"embedding":    "embedding",
				"requires":     "requires",
				"relationship": "relate",
			}
			twoPassEngine = analysis.New(twoPassRegistry, twoPassDispatcher, collector, config.EngineConfig{
				TaskTimeout:  5 * time.Minute,
				MaxRetries:   3,
				RetryBackoff: time.Millisecond,
			}, taskTypes)

			// classify/embedding are registered as Analyzer[*pb.Control]
			// (matching production), so they need a job whose catalog has at
			// least one control to actually fan out and dispatch. A
			// zero-control catalog would leave Controls empty and correctly
			// produce zero tasks for them (see the dedicated zero-controls
			// test below) -- which would leave these two-pass ordering tests
			// unable to observe classify/embedding's dispatch at all.
			twoPassCatalog = &fakeCatalogControlsReader{controls: []*pb.Control{
				{ControlId: "c1", CatalogId: "two-pass-cat-1"},
			}}
		})

		// twoPassJob creates a running job with the full two-pass stage list.
		twoPassJob := func() {
			configBytes := []byte(`{"catalog_id":"two-pass-cat-1"}`)
			job := &pipeline.Job{
				JobID:     jobID,
				TenantID:  tenantID,
				Status:    pipeline.JobStatusRunning,
				Config:    configBytes,
				CreatedAt: time.Now(),
				UpdatedAt: time.Now(),
			}
			Expect(store.CreateJob(ctx, job)).To(Succeed())
			stageNames := []string{"classify", "embedding", "candidate_generation", "requires", "relationship", "synthesis", "graph"}
			Expect(store.CreateStages(ctx, jobID, stageNames)).To(Succeed())
		}

		stageStatus := func(name string) pipeline.StageStatus {
			stages, err := store.GetStages(ctx, jobID)
			Expect(err).NotTo(HaveOccurred())
			s := findStage(stages, name)
			Expect(s).NotTo(BeNil())
			return s.Status
		}

		// dispatchedTypes reports which analyzer task types have been dispatched.
		// requires -> "requires", relationship -> "relate".
		dispatchedTypes := func() map[natsbus.TaskType]bool {
			twoPassDispatcher.mu.Lock()
			defer twoPassDispatcher.mu.Unlock()
			seen := make(map[natsbus.TaskType]bool)
			for _, d := range twoPassDispatcher.dispatches {
				seen[d.taskType] = true
			}
			return seen
		}

		// dispatchCount reports how many times a given task type was dispatched.
		// Pre-candidate analyzers must dispatch exactly once even though the
		// candidate-dependent pass's DAG subset transitively includes them.
		dispatchCount := func(tt natsbus.TaskType) int {
			twoPassDispatcher.mu.Lock()
			defer twoPassDispatcher.mu.Unlock()
			n := 0
			for _, d := range twoPassDispatcher.dispatches {
				if d.taskType == tt {
					n++
				}
			}
			return n
		}

		// Both pair analyzers disabled: NewProductionRegistry registers
		// neither, so nothing reads candidates.
		It("with no pair analyzer registered, has no candidate_generation stage, generates no candidates and completes with zero synthesis inputs", func() {
			reg := analyzer.NewRegistry()
			for _, name := range []string{"classify", "embedding"} {
				deps := map[string][]string{"embedding": {"classify"}}[name]
				stub := newControlStub(name, deps...)
				stub.GenerateWorkFunc = func(context.Context, *pb.Control, analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
					return []analyzer.Task{{TaskID: name + "-task", Payload: &emptypb.Empty{}}}, nil
				}
				Expect(analyzer.Register[*pb.Control](reg, stub)).To(Succeed())
			}
			eng := analysis.New(reg, twoPassDispatcher, &executorTestCollector{},
				config.EngineConfig{TaskTimeout: 5 * time.Minute, MaxRetries: 3, RetryBackoff: time.Millisecond},
				map[string]natsbus.TaskType{"classify": "classify", "embedding": "embedding"},
				analysis.WithStageReporter(pipeline.NewDBStageReporter(analysis.NewNATSStageReporter(bus), store)))
			gen := &fakeCandidateGen{}
			svc = pipeline.New(store, eng, reg, synth, pipeline.Attestors{}, converter, bus, storage,
				config.PipelineConfig{MaxConcurrentJobs: 10},
				gen,
				pipeline.WithCatalogControlsReader(twoPassCatalog),
				pipeline.WithCatalogNameReader(fakeCatalogNameReader{"two-pass-cat-1": {name: "Two-pass Catalog"}}),
			)
			DeferCleanup(func() { Expect(svc.Stop(context.Background())).To(Succeed()) })

			resp, err := svc.CreateJob(ctx, connect.NewRequest(&pb.CreateJobRequest{
				Config: &pb.JobConfig{CatalogId: "two-pass-cat-1", TargetCatalogId: "two-pass-cat-1"},
			}))
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() pipeline.JobStatus {
				j, _ := store.GetJob(ctx, resp.Msg.JobId)
				if j == nil {
					return ""
				}
				return j.Status
			}).Should(Equal(pipeline.JobStatusCompleted))

			stages, err := store.GetStages(ctx, resp.Msg.JobId)
			Expect(err).NotTo(HaveOccurred())
			Expect(stages).NotTo(ContainElement(HaveField("StageName", "candidate_generation")))
			Expect(stages).To(HaveEach(HaveField("Status", pipeline.StageStatusCompleted)))
			Expect(gen.wasCalled()).To(BeFalse())
			Expect(dispatchedTypes()).To(Equal(map[natsbus.TaskType]bool{"classify": true, "embedding": true}))
			synth.mu.Lock()
			defer synth.mu.Unlock()
			Expect(synth.executions).To(HaveLen(1))
			Expect(synth.executions[0].inputs).To(BeEmpty())
		})

		It("runs candidate generation between the pre- and candidate-dependent passes", func() {
			gen := &fakeCandidateGen{}
			gen.dispatchCountFn = func() int {
				twoPassDispatcher.mu.Lock()
				defer twoPassDispatcher.mu.Unlock()
				return len(twoPassDispatcher.dispatches)
			}

			svc = pipeline.New(store, twoPassEngine, twoPassRegistry, synth, pipeline.Attestors{}, converter, bus, storage,
				config.PipelineConfig{MaxConcurrentJobs: 10},
				gen,
				pipeline.WithCatalogControlsReader(twoPassCatalog),
			)

			twoPassJob()

			runCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			Expect(svc.Start(runCtx)).To(Succeed())

			Eventually(func() pipeline.JobStatus {
				j, _ := store.GetJob(ctx, jobID)
				if j == nil {
					return ""
				}
				return j.Status
			}, "2s", "50ms").Should(Equal(pipeline.JobStatusCompleted))

			Expect(gen.wasCalled()).To(BeTrue())
			// Pre-candidate pass (classify + embedding) dispatched before the call...
			Expect(gen.dispatchesBeforeCall()).To(Equal(2))
			// ...and the candidate-dependent pass (requires + relationship) after it.
			types := dispatchedTypes()
			Expect(types).To(HaveKey(natsbus.TaskType("requires")))
			Expect(types).To(HaveKey(natsbus.TaskType("relate")))
			Expect(stageStatus("candidate_generation")).To(Equal(pipeline.StageStatusCompleted))
		})

		It("does not re-execute pre-candidate analyzers in the candidate-dependent pass", func() {
			gen := &fakeCandidateGen{}

			svc = pipeline.New(store, twoPassEngine, twoPassRegistry, synth, pipeline.Attestors{}, converter, bus, storage,
				config.PipelineConfig{MaxConcurrentJobs: 10},
				gen,
				pipeline.WithCatalogControlsReader(twoPassCatalog),
			)

			twoPassJob()

			runCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			Expect(svc.Start(runCtx)).To(Succeed())

			Eventually(func() pipeline.JobStatus {
				j, _ := store.GetJob(ctx, jobID)
				if j == nil {
					return ""
				}
				return j.Status
			}, "2s", "50ms").Should(Equal(pipeline.JobStatusCompleted))

			// The candidate-dependent pass's DAG subset transitively includes
			// classify and embedding. They already completed in the pre-candidate
			// pass, so they must be dispatched exactly once, not re-run.
			Expect(dispatchCount("classify")).To(Equal(1))
			Expect(dispatchCount("embedding")).To(Equal(1))
			Expect(dispatchCount("requires")).To(Equal(1))
			Expect(dispatchCount("relate")).To(Equal(1))
		})

		It("fails the analysis and skips the candidate-dependent pass when generation errors", func() {
			gen := &fakeCandidateGen{err: errors.New("candidate boom")}

			svc = pipeline.New(store, twoPassEngine, twoPassRegistry, synth, pipeline.Attestors{}, converter, bus, storage,
				config.PipelineConfig{MaxConcurrentJobs: 10},
				gen,
				pipeline.WithCatalogControlsReader(twoPassCatalog),
			)

			twoPassJob()

			runCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			Expect(svc.Start(runCtx)).To(Succeed())

			Eventually(func() pipeline.JobStatus {
				j, _ := store.GetJob(ctx, jobID)
				if j == nil {
					return ""
				}
				return j.Status
			}, "2s", "50ms").Should(Equal(pipeline.JobStatusFailed))

			Expect(gen.wasCalled()).To(BeTrue())
			// Only the pre-candidate pass ran; requires/relationship never dispatched.
			types := dispatchedTypes()
			Expect(types).To(HaveKey(natsbus.TaskType("classify")))
			Expect(types).NotTo(HaveKey(natsbus.TaskType("requires")))
			Expect(types).NotTo(HaveKey(natsbus.TaskType("relate")))

			Expect(stageStatus("candidate_generation")).To(Equal(pipeline.StageStatusFailed))
			stages, err := store.GetStages(ctx, jobID)
			Expect(err).NotTo(HaveOccurred())
			Expect(findStage(stages, "candidate_generation").ErrorMessage).To(ContainSubstring("candidate boom"))
		})

		// Safety mechanism: a stored config without catalog_id (a pre-#212 row,
		// or one written outside CreateJob) must fail, not analyse zero controls.
		It("fails a job whose stored config has no catalog_id and runs no analyzer", func() {
			gen := &fakeCandidateGen{}
			svc = pipeline.New(store, twoPassEngine, twoPassRegistry, synth, pipeline.Attestors{}, converter, bus, storage,
				config.PipelineConfig{MaxConcurrentJobs: 10},
				gen,
				pipeline.WithCatalogControlsReader(twoPassCatalog),
			)

			job := &pipeline.Job{
				JobID:     jobID,
				TenantID:  tenantID,
				Status:    pipeline.JobStatusRunning,
				Config:    []byte(`{}`),
				CreatedAt: time.Now(),
				UpdatedAt: time.Now(),
			}
			Expect(store.CreateJob(ctx, job)).To(Succeed())
			Expect(store.CreateStages(ctx, jobID, []string{"classify", "synthesis"})).To(Succeed())

			runCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			Expect(svc.Start(runCtx)).To(Succeed())

			Eventually(func() pipeline.JobStatus {
				j, _ := store.GetJob(ctx, jobID)
				if j == nil {
					return ""
				}
				return j.Status
			}, "2s", "50ms").Should(Equal(pipeline.JobStatusFailed))

			j, err := store.GetJob(ctx, jobID)
			Expect(err).NotTo(HaveOccurred())
			Expect(j.ErrorMessage).To(ContainSubstring("no usable catalog_id"))
			Expect(j.ErrorMessage).To(ContainSubstring("crosscodex run start --source <source-catalog-id> --target <target-catalog-id>"))
			Expect(stageStatus("classify")).To(Equal(pipeline.StageStatusPending))
			Expect(stageStatus("synthesis")).To(Equal(pipeline.StageStatusPending))
			Expect(dispatchedTypes()).To(BeEmpty())
			Expect(gen.wasCalled()).To(BeFalse())
			twoPassCatalog.mu.Lock()
			defer twoPassCatalog.mu.Unlock()
			Expect(twoPassCatalog.calls).To(BeEmpty())
		})

		It("does not re-execute a pre-candidate analyzer already completed by a prior process (crash-recovery resume)", func() {
			gen := &fakeCandidateGen{}

			svc = pipeline.New(store, twoPassEngine, twoPassRegistry, synth, pipeline.Attestors{}, converter, bus, storage,
				config.PipelineConfig{MaxConcurrentJobs: 10},
				gen,
				pipeline.WithCatalogControlsReader(twoPassCatalog),
			)

			twoPassJob()

			// Simulate a prior process invocation that completed classify and
			// embedding durably (job_stages row + analysis_results row) before
			// crashing. A fresh executeJob (this call to svc.Start) must treat
			// them as already done, not re-dispatch them.
			Expect(store.CompleteAnalysisStage(ctx, jobID, "classify", []byte(`[]`))).To(Succeed())
			Expect(store.CompleteAnalysisStage(ctx, jobID, "embedding", []byte(`[]`))).To(Succeed())

			runCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			Expect(svc.Start(runCtx)).To(Succeed())

			Eventually(func() pipeline.JobStatus {
				j, _ := store.GetJob(ctx, jobID)
				if j == nil {
					return ""
				}
				return j.Status
			}, "2s", "50ms").Should(Equal(pipeline.JobStatusCompleted))

			// classify/embedding must never be dispatched in this invocation —
			// they were already completed by the "prior process".
			Expect(dispatchCount("classify")).To(Equal(0))
			Expect(dispatchCount("embedding")).To(Equal(0))
			// The candidate-dependent pass must still proceed.
			Expect(dispatchCount("requires")).To(Equal(1))
			Expect(dispatchCount("relate")).To(Equal(1))
			Expect(stageStatus("candidate_generation")).To(Equal(pipeline.StageStatusCompleted))
		})

		It("writes vote_summaries from allOutputs on resume even when postPass is empty", func() {
			// Mirrors production: "artifacts" has no dependencies (unlike
			// requires/relationship, which depend on embedding) and can still
			// be running when a prior process invocation crashes after
			// requires and relationship already completed durably but before
			// vote_summaries was written. On resume, requires/relationship are
			// in `completed` (postPass is empty this invocation) while
			// artifacts is not, so incompleteAnalyzers is non-empty and
			// runAnalysis does not take its "all analyzers already completed"
			// shortcut — it must still populate vote_summaries from
			// allOutputs so runSynthesis's later persistViability call finds
			// rows to update.
			reg := analyzer.NewRegistry()
			Expect(analyzer.Register[*pb.Control](reg, newControlStub("classify"))).To(Succeed())
			Expect(analyzer.Register[*pb.Control](reg, newControlStub("embedding", "classify"))).To(Succeed())
			Expect(analyzer.Register[*pb.Control](reg, newControlStub("artifacts"))).To(Succeed())
			requiresStub := newControlStub("requires", "embedding")
			requiresStub.GenerateWorkFunc = func(_ context.Context, _ *pb.Control, _ analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
				return []analyzer.Task{{TaskID: "requires-task", Payload: &emptypb.Empty{}}}, nil
			}
			Expect(analyzer.Register[*pb.Control](reg, requiresStub)).To(Succeed())
			relationshipStub := newControlStub("relationship", "embedding")
			relationshipStub.GenerateWorkFunc = func(_ context.Context, _ *pb.Control, _ analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
				return []analyzer.Task{{TaskID: "relationship-task", Payload: &emptypb.Empty{}}}, nil
			}
			Expect(analyzer.Register[*pb.Control](reg, relationshipStub)).To(Succeed())

			resumeDispatcher := &executorTestDispatcher{}
			collector := &executorTestCollector{}
			eng := analysis.New(reg, resumeDispatcher, collector, config.EngineConfig{
				TaskTimeout:  5 * time.Minute,
				MaxRetries:   3,
				RetryBackoff: time.Millisecond,
			}, map[string]natsbus.TaskType{
				"classify":     "classify",
				"embedding":    "embedding",
				"artifacts":    "artifacts",
				"requires":     "requires",
				"relationship": "relate",
			})

			svc = pipeline.New(store, eng, reg, synth, pipeline.Attestors{}, converter, bus, storage,
				config.PipelineConfig{MaxConcurrentJobs: 10},
				nil,
			)

			configBytes := catalogJobConfig
			job := &pipeline.Job{
				JobID:     jobID,
				TenantID:  tenantID,
				Status:    pipeline.JobStatusRunning,
				Config:    configBytes,
				CreatedAt: time.Now(),
				UpdatedAt: time.Now(),
			}
			Expect(store.CreateJob(ctx, job)).To(Succeed())
			Expect(store.CreateStages(ctx, jobID, []string{
				"classify", "embedding", "artifacts", "candidate_generation", "requires", "relationship", "synthesis", "graph",
			})).To(Succeed())

			// Simulate a prior process invocation that completed classify,
			// embedding, requires, and relationship durably (requires and
			// relationship with real result payloads) before crashing.
			// artifacts did not finish before the crash.
			Expect(store.CompleteAnalysisStage(ctx, jobID, "classify", []byte(`[]`))).To(Succeed())
			Expect(store.CompleteAnalysisStage(ctx, jobID, "embedding", []byte(`[]`))).To(Succeed())
			requiresJSON, err := json.Marshal([]results.RequiresResult{
				{SourceID: "AC-1", TargetID: "AC-2", Confidence: 0.9},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(store.CompleteAnalysisStage(ctx, jobID, "requires", requiresJSON)).To(Succeed())
			relJSON, err := json.Marshal([]results.SemanticMatchResult{
				{SourceID: "AC-1", TargetID: "AC-2", RelationshipType: "supports", Confidence: 0.8,
					Properties: map[string]string{
						results.SemanticMatchContributionTypeKey: "",
						results.SemanticMatchUnanimousKey:        "true",
					}},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(store.CompleteAnalysisStage(ctx, jobID, "relationship", relJSON)).To(Succeed())

			runCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			Expect(svc.Start(runCtx)).To(Succeed())

			Eventually(func() pipeline.JobStatus {
				j, _ := store.GetJob(ctx, jobID)
				if j == nil {
					return ""
				}
				return j.Status
			}, "2s", "50ms").Should(Equal(pipeline.JobStatusCompleted))

			// requires/relationship must never be dispatched this invocation
			// — postPass really was empty (both already completed).
			resumeDispatcher.mu.Lock()
			var dispatchedTypes []natsbus.TaskType
			for _, d := range resumeDispatcher.dispatches {
				dispatchedTypes = append(dispatchedTypes, d.taskType)
			}
			resumeDispatcher.mu.Unlock()
			Expect(dispatchedTypes).NotTo(ContainElement(natsbus.TaskType("requires")))
			Expect(dispatchedTypes).NotTo(ContainElement(natsbus.TaskType("relate")))

			// Yet vote_summaries must still have been written, built from
			// requires/relationship's persisted results loaded into
			// allOutputs via GetCompletedAnalysisResults.
			store.mu.Lock()
			pairs := store.voteSummaries[jobID]
			store.mu.Unlock()
			Expect(pairs).To(HaveLen(2), "one row per contribution: requires and relationship")
			Expect(pairs).To(HaveKey([3]string{"AC-1", "AC-2", "requires"}))
			Expect(pairs[[3]string{"AC-1", "AC-2", "requires"}].Consensus).To(Equal("requires"))
			Expect(pairs).To(HaveKey([3]string{"AC-1", "AC-2", "relationship"}))
			Expect(pairs[[3]string{"AC-1", "AC-2", "relationship"}].Consensus).To(Equal("supports"))
		})
	})

	Describe("controls and analyzer config wiring", func() {
		Describe("across two catalogs", func() {
			// startJob runs a job with config over reader's catalogs, with
			// classify, embedding and artifacts registered (as in
			// production, all per-control) and returns their stubs. Each
			// configure func scripts the stubs before the job starts.
			startJob := func(jobConfig string, reader *fakeCatalogControlsReader, configure ...func([]*analyzertest.Stub[*pb.Control])) []*analyzertest.Stub[*pb.Control] {
				reg := analyzer.NewRegistry()
				stubs := []*analyzertest.Stub[*pb.Control]{
					newControlStub("classify"),
					newControlStub("embedding", "classify"),
					newControlStub("artifacts", "classify"),
				}
				for _, c := range configure {
					c(stubs)
				}
				for _, s := range stubs {
					Expect(analyzer.Register[*pb.Control](reg, s)).To(Succeed())
				}
				eng := analysis.New(reg, &executorTestDispatcher{}, &executorTestCollector{}, config.EngineConfig{
					TaskTimeout:  5 * time.Minute,
					MaxRetries:   3,
					RetryBackoff: time.Millisecond,
				}, map[string]natsbus.TaskType{"classify": "classify", "embedding": "embedding", "artifacts": "artifacts"})
				svc = pipeline.New(store, eng, reg, synth, pipeline.Attestors{}, converter, bus, storage,
					config.PipelineConfig{MaxConcurrentJobs: 10},
					nil,
					pipeline.WithCatalogControlsReader(reader),
					pipeline.WithCatalogNameReader(fakeCatalogNameReader{"cat-src": {name: "CSF 2.0"}, "cat-tgt": {name: "NIST SP 800-53"}}),
				)
				Expect(store.CreateJob(ctx, &pipeline.Job{
					JobID: jobID, TenantID: tenantID, Status: pipeline.JobStatusRunning,
					Config: []byte(jobConfig), CreatedAt: time.Now(), UpdatedAt: time.Now(),
				})).To(Succeed())
				Expect(store.CreateStages(ctx, jobID, []string{"classify", "embedding", "artifacts", "synthesis", "graph"})).To(Succeed())
				runCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
				DeferCleanup(cancel)
				Expect(svc.Start(runCtx)).To(Succeed())
				return stubs
			}
			jobStatus := func() pipeline.JobStatus {
				j, _ := store.GetJob(ctx, jobID)
				if j == nil {
					return ""
				}
				return j.Status
			}
			inputIDs := func(s *analyzertest.Stub[*pb.Control]) []string {
				var ids []string
				for _, in := range s.Inputs() {
					ids = append(ids, in.GetControlId())
				}
				return ids
			}

			It("fans every per-control analyzer out over the controls of both catalogs", func() {
				reader := &fakeCatalogControlsReader{byCatalog: map[string][]*pb.Control{
					"cat-src": {{ControlId: "s1", CatalogId: "cat-src", Statement: "Source one."}},
					"cat-tgt": {{ControlId: "t1", CatalogId: "cat-tgt", Statement: "Target one."}, {ControlId: "t2", CatalogId: "cat-tgt", Statement: "Target two."}},
				}}
				stubs := startJob(`{"catalog_id":"cat-src","target_catalog_id":"cat-tgt"}`, reader)

				Eventually(jobStatus, "2s", "50ms").Should(Equal(pipeline.JobStatusCompleted))
				Expect(reader.catalogIDs()).To(Equal([]string{"cat-src", "cat-tgt"}))
				for _, s := range stubs {
					Expect(inputIDs(s)).To(ConsistOf("s1", "t1", "t2"), s.Name())
				}
			})

			It("runs a job whose stored config predates target_catalog_id within its one catalog", func() {
				reader := &fakeCatalogControlsReader{byCatalog: map[string][]*pb.Control{
					"cat-src": {{ControlId: "s1", CatalogId: "cat-src", Statement: "Source one."}},
				}}
				stubs := startJob(`{"catalog_id":"cat-src"}`, reader)

				Eventually(jobStatus, "2s", "50ms").Should(Equal(pipeline.JobStatusCompleted))
				Expect(reader.catalogIDs()).To(Equal([]string{"cat-src"}))
				Expect(inputIDs(stubs[0])).To(ConsistOf("s1"))
			})

			// A deleted or purged catalog reads as zero controls, so its
			// existence is checked first: it must not be reported as empty.
			It("fails a crosswalk whose target was deleted as a missing catalog, not an empty one, before any analyzer runs", func() {
				reader := &fakeCatalogControlsReader{byCatalog: map[string][]*pb.Control{
					"cat-src": {{ControlId: "s1", CatalogId: "cat-src", Statement: "Source one."}},
				}}
				stubs := startJob(`{"catalog_id":"cat-src","target_catalog_id":"cat-gone"}`, reader)

				Eventually(jobStatus, "2s", "50ms").Should(Equal(pipeline.JobStatusFailed))
				j, err := store.GetJob(ctx, jobID)
				Expect(err).NotTo(HaveOccurred())
				Expect(j.ErrorMessage).To(SatisfyAll(
					ContainSubstring("catalog cat-gone was not found"),
					ContainSubstring(pipeline.ErrCatalogNotFound.Error()),
					ContainSubstring("it may have been deleted or purged. Import the same file again (it gets the same catalog ID)"),
					ContainSubstring("crosscodex run retry --from-failure "+jobID),
					Not(ContainSubstring(pipeline.ErrEmptyTargetCatalog.Error())),
				))
				Expect(j.ErrorClass).To(Equal(pipeline.JobErrorRetryable))
				Expect(reader.catalogIDs()).To(BeEmpty())
				for _, s := range stubs {
					Expect(s.Inputs()).To(BeEmpty(), s.Name())
				}
			})

			// The engine's result-cache failures are classified by their
			// class sentinel, and their tenant message is the job's.
			It("fails a job whose stage stopped on a result-cache error with that error's class and message", func() {
				reader := &fakeCatalogControlsReader{byCatalog: map[string][]*pb.Control{
					"cat-src": {{ControlId: "s1", CatalogId: "cat-src", Statement: "Source one."}},
				}}
				stageErr := &analysis.StageError{Analyzer: "classify", Sentinel: resultcache.ErrCorrupt,
					Cause:   errors.New("dial tcp 10.0.0.7:5432: driver detail"),
					Message: `Stage "classify" stopped: a cached result failed its integrity check.`}
				startJob(`{"catalog_id":"cat-src"}`, reader, func(stubs []*analyzertest.Stub[*pb.Control]) {
					stubs[0].GenerateWorkFunc = func(context.Context, *pb.Control, analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
						return nil, stageErr
					}
				})

				Eventually(jobStatus, "2s", "50ms").Should(Equal(pipeline.JobStatusFailed))
				j, err := store.GetJob(ctx, jobID)
				Expect(err).NotTo(HaveOccurred())
				Expect(j.ErrorClass).To(Equal(pipeline.JobErrorResultCacheCorrupt))
				Expect(j.ErrorMessage).To(SatisfyAll(
					ContainSubstring("analysis failed: 1 analyzers failed: [classify]"),
					ContainSubstring(stageErr.Message),
					Not(ContainSubstring("driver detail"))))
			})

			// Safety mechanism: a crosswalk onto a target with nothing to
			// map onto would bill the source's per-control LLM calls for an
			// empty result.
			It("fails a crosswalk whose target has no control with a statement, with guidance, before any analyzer runs", func() {
				reader := &fakeCatalogControlsReader{byCatalog: map[string][]*pb.Control{
					"cat-src": {{ControlId: "s1", CatalogId: "cat-src", Statement: "Source one."}},
					"cat-tgt": {
						{ControlId: "t-sec", CatalogId: "cat-tgt", Statement: "A family.", Parts: map[string]string{"class": "compliance-section"}},
						{ControlId: "t-blank", CatalogId: "cat-tgt", Statement: "  "},
					},
				}}
				stubs := startJob(`{"catalog_id":"cat-src","target_catalog_id":"cat-tgt"}`, reader)

				Eventually(jobStatus, "2s", "50ms").Should(Equal(pipeline.JobStatusFailed))
				j, err := store.GetJob(ctx, jobID)
				Expect(err).NotTo(HaveOccurred())
				Expect(j.ErrorMessage).To(SatisfyAll(
					ContainSubstring("target catalog cat-tgt has no control with a statement"),
					ContainSubstring(pipeline.ErrEmptyTargetCatalog.Error()),
					ContainSubstring(`crosscodex catalog inspect cat-tgt`),
					ContainSubstring("crosscodex run start --source cat-src --target <catalog-id>"),
				))
				Expect(j.ErrorClass).To(Equal(pipeline.JobErrorEmptyTargetCatalog))
				for _, s := range stubs {
					Expect(s.Inputs()).To(BeEmpty(), s.Name())
				}
				stages, err := store.GetStages(ctx, jobID)
				Expect(err).NotTo(HaveOccurred())
				Expect(stages).To(HaveEach(HaveField("Status", pipeline.StageStatusPending)))
				results, err := store.GetCompletedAnalysisResults(ctx, jobID, []string{"classify", "embedding", "artifacts"})
				Expect(err).NotTo(HaveOccurred())
				Expect(results).To(BeEmpty())
			})
		})

		It("populates Controls and CatalogID for the pre-candidate pass", func() {
			type call struct {
				controlID string
				catalogID string
				ok        bool
			}
			var mu sync.Mutex
			var calls []call

			reg := analyzer.NewRegistry()
			classifyStub := newControlStub("classify")
			classifyStub.GenerateWorkFunc = func(ctx context.Context, input *pb.Control, _ analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
				catalogID, ok := analyzer.CatalogIDFromContext(ctx)
				mu.Lock()
				calls = append(calls, call{controlID: input.GetControlId(), catalogID: catalogID, ok: ok})
				mu.Unlock()
				return []analyzer.Task{{TaskID: input.GetControlId() + "-task", Payload: &emptypb.Empty{}}}, nil
			}
			Expect(analyzer.Register[*pb.Control](reg, classifyStub)).To(Succeed())

			dispatcher := &executorTestDispatcher{}
			collector := &executorTestCollector{}
			eng := analysis.New(reg, dispatcher, collector, config.EngineConfig{
				TaskTimeout:  5 * time.Minute,
				MaxRetries:   3,
				RetryBackoff: time.Millisecond,
			}, map[string]natsbus.TaskType{"classify": "classify"})

			catalogReader := &fakeCatalogControlsReader{controls: []*pb.Control{
				{ControlId: "c1", CatalogId: "cat-1"},
				{ControlId: "c2", CatalogId: "cat-1"},
			}}

			svc = pipeline.New(store, eng, reg, synth, pipeline.Attestors{}, converter, bus, storage,
				config.PipelineConfig{MaxConcurrentJobs: 10},
				nil,
				pipeline.WithCatalogControlsReader(catalogReader),
			)

			job := &pipeline.Job{
				JobID:     jobID,
				TenantID:  tenantID,
				Status:    pipeline.JobStatusRunning,
				Config:    []byte(`{"catalog_id":"cat-1"}`),
				CreatedAt: time.Now(),
				UpdatedAt: time.Now(),
			}
			Expect(store.CreateJob(ctx, job)).To(Succeed())
			Expect(store.CreateStages(ctx, jobID, []string{"classify", "candidate_generation", "synthesis", "graph"})).To(Succeed())

			runCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			Expect(svc.Start(runCtx)).To(Succeed())

			Eventually(func() pipeline.JobStatus {
				j, _ := store.GetJob(ctx, jobID)
				if j == nil {
					return ""
				}
				return j.Status
			}, "2s", "50ms").Should(Equal(pipeline.JobStatusCompleted))

			mu.Lock()
			defer mu.Unlock()
			Expect(calls).To(HaveLen(2))
			seenControls := map[string]bool{}
			for _, c := range calls {
				seenControls[c.controlID] = true
				Expect(c.ok).To(BeTrue())
				Expect(c.catalogID).To(Equal("cat-1"))
			}
			Expect(seenControls).To(HaveKey("c1"))
			Expect(seenControls).To(HaveKey("c2"))

			Expect(catalogReader.calls).To(HaveLen(1))
			Expect(catalogReader.calls[0].tenantID).To(Equal(tenantID))
			Expect(catalogReader.calls[0].catalogID).To(Equal("cat-1"))
		})

		It(`sets AnalyzerConfig[name].Parameters["job_id"] for every analyzer in both passes`, func() {
			var mu sync.Mutex
			cfgByName := make(map[string]string)

			captureControl := func(name string) func(context.Context, *pb.Control, analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
				return func(_ context.Context, _ *pb.Control, cfg analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
					mu.Lock()
					cfgByName[name] = cfg.Parameters["job_id"]
					mu.Unlock()
					return []analyzer.Task{{TaskID: name + "-task", Payload: &emptypb.Empty{}}}, nil
				}
			}

			// classify/embedding are registered as Analyzer[*pb.Control]
			// (matching production), so the job's catalog below must have a
			// non-empty control list for their GenerateWork to fire — with no
			// controls, Controls would be empty and correctly produce zero
			// tasks for them instead.
			reg := analyzer.NewRegistry()
			stub := func(name string, deps ...string) *analyzertest.Stub[*pb.Control] {
				s := newControlStub(name, deps...)
				s.GenerateWorkFunc = captureControl(name)
				return s
			}
			Expect(analyzer.Register[*pb.Control](reg, stub("classify"))).To(Succeed())
			Expect(analyzer.Register[*pb.Control](reg, stub("embedding", "classify"))).To(Succeed())
			Expect(analyzer.Register[*pb.Control](reg, stub("requires", "embedding"))).To(Succeed())
			Expect(analyzer.Register[*pb.Control](reg, stub("relationship", "embedding"))).To(Succeed())

			dispatcher := &executorTestDispatcher{}
			collector := &executorTestCollector{}
			eng := analysis.New(reg, dispatcher, collector, config.EngineConfig{
				TaskTimeout:  5 * time.Minute,
				MaxRetries:   3,
				RetryBackoff: time.Millisecond,
			}, map[string]natsbus.TaskType{
				"classify":     "classify",
				"embedding":    "embedding",
				"requires":     "requires",
				"relationship": "relate",
			})

			catalogReader := &fakeCatalogControlsReader{controls: []*pb.Control{
				{ControlId: "c1", CatalogId: "cfg-cat-1"},
			}}
			svc = pipeline.New(store, eng, reg, synth, pipeline.Attestors{}, converter, bus, storage,
				config.PipelineConfig{MaxConcurrentJobs: 10},
				nil,
				pipeline.WithCatalogControlsReader(catalogReader),
			)

			configBytes := []byte(`{"catalog_id":"cfg-cat-1"}`)
			job := &pipeline.Job{
				JobID:     jobID,
				TenantID:  tenantID,
				Status:    pipeline.JobStatusRunning,
				Config:    configBytes,
				CreatedAt: time.Now(),
				UpdatedAt: time.Now(),
			}
			Expect(store.CreateJob(ctx, job)).To(Succeed())
			Expect(store.CreateStages(ctx, jobID, []string{"classify", "embedding", "candidate_generation", "requires", "relationship", "synthesis", "graph"})).To(Succeed())

			runCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			Expect(svc.Start(runCtx)).To(Succeed())

			Eventually(func() pipeline.JobStatus {
				j, _ := store.GetJob(ctx, jobID)
				if j == nil {
					return ""
				}
				return j.Status
			}, "2s", "50ms").Should(Equal(pipeline.JobStatusCompleted))

			mu.Lock()
			defer mu.Unlock()
			for _, name := range []string{"classify", "embedding", "requires", "relationship"} {
				Expect(cfgByName).To(HaveKeyWithValue(name, jobID))
			}
		})

		Describe("pair analyzer parameters", func() {
			var (
				mu     sync.Mutex
				params map[string]map[string]string
				// pairJobConfig and pairControls are the job startPairJob
				// runs: within catalog cat-1 unless a spec overrides them.
				pairJobConfig string
				pairControls  *fakeCatalogControlsReader
				pairNames     pipeline.CatalogNameReader
			)
			catalogNames := fakeCatalogNameReader{"cat-1": {name: "CSF 2.0", version: "2.0"}, "cat-2": {name: "NIST SP 800-53"}}

			BeforeEach(func() {
				pairNames = catalogNames
				pairJobConfig = `{"catalog_id":"cat-1"}`
				pairControls = &fakeCatalogControlsReader{controls: []*pb.Control{
					{ControlId: "c1", CatalogId: "cat-1", Statement: "Keep an asset inventory.", Parts: map[string]string{"ancestor_title": "Identify"}},
					{ControlId: "c2", CatalogId: "cat-1", Statement: "Review the inventory yearly.", Parts: map[string]string{"ancestor_title": "Protect"}},
				}}
			})

			classified := []byte(`[{"control_id":"c1","type":"Technical","level":"Operational"},{"control_id":"c2","type":"Procedural","level":"Tactical"}]`)

			// startPairJob runs a job whose classify output is classifyResult,
			// with requires and relationship capturing their whole
			// cfg.Parameters map. resumed seeds classify as completed by a
			// prior process run, so its stub must never be called; the stub
			// is returned for that assertion.
			startPairJob := func(classifyResult []byte, resumed bool) *analyzertest.Stub[*pb.Control] {
				params = map[string]map[string]string{}
				perControlTask := func(name string) func(context.Context, *pb.Control, analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
					return func(_ context.Context, c *pb.Control, _ analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
						return []analyzer.Task{{TaskID: name + "-" + c.GetControlId(), Payload: &emptypb.Empty{}}}, nil
					}
				}
				capturePair := func(name string) func(context.Context, *pb.Control, analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
					return func(_ context.Context, _ *pb.Control, cfg analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
						mu.Lock()
						params[name] = maps.Clone(cfg.Parameters)
						mu.Unlock()
						return nil, nil
					}
				}

				reg := analyzer.NewRegistry()
				classify := newControlStub("classify")
				classify.GenerateWorkFunc = perControlTask("classify")
				if resumed {
					classify.GenerateWorkFunc = func(context.Context, *pb.Control, analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
						return nil, errors.New("classify re-ran although a prior run completed it")
					}
				}
				classify.AggregateFunc = func(context.Context, []analyzer.TaskResult) (*analyzer.Output, error) {
					return &analyzer.Output{AnalyzerName: "classify", ResultData: classifyResult}, nil
				}
				requires := newControlStub("requires", "classify")
				requires.GenerateWorkFunc = capturePair("requires")
				relationship := newControlStub("relationship", "classify")
				relationship.GenerateWorkFunc = capturePair("relationship")
				Expect(analyzer.Register[*pb.Control](reg, classify)).To(Succeed())
				Expect(analyzer.Register[*pb.Control](reg, requires)).To(Succeed())
				Expect(analyzer.Register[*pb.Control](reg, relationship)).To(Succeed())

				eng := analysis.New(reg, &executorTestDispatcher{}, &executorTestCollector{}, config.EngineConfig{
					TaskTimeout:  5 * time.Minute,
					MaxRetries:   3,
					RetryBackoff: time.Millisecond,
				}, map[string]natsbus.TaskType{
					"classify":     "classify",
					"requires":     "requires",
					"relationship": "relate",
				})

				svc = pipeline.New(store, eng, reg, synth, pipeline.Attestors{}, converter, bus, storage,
					config.PipelineConfig{MaxConcurrentJobs: 10},
					nil,
					pipeline.WithCatalogControlsReader(pairControls),
					pipeline.WithCatalogNameReader(pairNames),
				)

				job := &pipeline.Job{
					JobID:     jobID,
					TenantID:  tenantID,
					Status:    pipeline.JobStatusRunning,
					Config:    []byte(pairJobConfig),
					CreatedAt: time.Now(),
					UpdatedAt: time.Now(),
				}
				Expect(store.CreateJob(ctx, job)).To(Succeed())
				Expect(store.CreateStages(ctx, jobID, []string{"classify", "candidate_generation", "requires", "relationship", "synthesis", "graph"})).To(Succeed())
				if resumed {
					Expect(store.CompleteAnalysisStage(ctx, jobID, "classify", classifyResult)).To(Succeed())
				}

				runCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
				DeferCleanup(cancel)
				Expect(svc.Start(runCtx)).To(Succeed())
				return classify
			}

			wantPairParams := func() map[string]string {
				return map[string]string{
					"job_id":              jobID,
					"source_text_c1":      "Keep an asset inventory.",
					"target_text_c1":      "Keep an asset inventory.",
					"source_ancestor_c1":  "Identify",
					"target_ancestor_c1":  "Identify",
					"source_type_c1":      "Technical",
					"target_type_c1":      "Technical",
					"source_level_c1":     "Operational",
					"target_level_c1":     "Operational",
					"source_text_c2":      "Review the inventory yearly.",
					"target_text_c2":      "Review the inventory yearly.",
					"source_ancestor_c2":  "Protect",
					"target_ancestor_c2":  "Protect",
					"source_type_c2":      "Procedural",
					"target_type_c2":      "Procedural",
					"source_level_c2":     "Tactical",
					"target_level_c2":     "Tactical",
					"source_framework_c1": "CSF 2.0",
					"target_framework_c1": "CSF 2.0",
					"source_framework_c2": "CSF 2.0",
					"target_framework_c2": "CSF 2.0",
				}
			}

			jobStatus := func() pipeline.JobStatus {
				j, _ := store.GetJob(ctx, jobID)
				if j == nil {
					return ""
				}
				return j.Status
			}

			It("passes every control's text, ancestor and classification under both pair ends", func() {
				startPairJob(classified, false)

				Eventually(jobStatus, "2s", "50ms").Should(Equal(pipeline.JobStatusCompleted))

				mu.Lock()
				defer mu.Unlock()
				for _, name := range []string{"requires", "relationship"} {
					Expect(params[name]).To(Equal(wantPairParams()), name)
				}
			})

			It("gives every control its own catalog's name as its framework in a crosswalk", func() {
				pairJobConfig = `{"catalog_id":"cat-1","target_catalog_id":"cat-2","bidirectional":true}`
				pairControls = &fakeCatalogControlsReader{byCatalog: map[string][]*pb.Control{
					"cat-1": {{ControlId: "c1", CatalogId: "cat-1", Statement: "Keep an asset inventory."}},
					"cat-2": {{ControlId: "t1", CatalogId: "cat-2", Statement: "Maintain a component inventory."}},
				}}
				startPairJob(classified, false)

				Eventually(jobStatus, "2s", "50ms").Should(Equal(pipeline.JobStatusCompleted))

				mu.Lock()
				defer mu.Unlock()
				for _, name := range []string{"requires", "relationship"} {
					Expect(params[name]).To(SatisfyAll(
						HaveKeyWithValue("source_framework_c1", "CSF 2.0"),
						HaveKeyWithValue("target_framework_c1", "CSF 2.0"),
						HaveKeyWithValue("source_framework_t1", "NIST SP 800-53"),
						HaveKeyWithValue("target_framework_t1", "NIST SP 800-53"),
						HaveKeyWithValue("source_text_t1", "Maintain a component inventory."),
					), name)
				}
			})

			// A catalog re-imported under a new name keeps its ID (IDs are
			// content hashes), so a job resumed after the rename must prompt
			// with the names it was created with, byte for byte.
			It("takes each control's framework from the job's creation-time snapshot, not the catalog's current name", func() {
				pairNames = fakeCatalogNameReader{"cat-1": {name: "CSF 2.0 (renamed)"}, "cat-2": {name: "SP 800-53 (renamed)"}}
				pairJobConfig = `{"catalog_id":"cat-1","target_catalog_id":"cat-2","bidirectional":true,` +
					`"source_catalog":{"catalog_id":"cat-1","name":"CSF 2.0","version":"2.0"},` +
					`"target_catalog":{"catalog_id":"cat-2","name":"NIST SP 800-53"}}`
				pairControls = &fakeCatalogControlsReader{byCatalog: map[string][]*pb.Control{
					"cat-1": {{ControlId: "c1", CatalogId: "cat-1", Statement: "Keep an asset inventory."}},
					"cat-2": {{ControlId: "t1", CatalogId: "cat-2", Statement: "Maintain a component inventory."}},
				}}
				startPairJob(classified, false)

				Eventually(jobStatus, "2s", "50ms").Should(Equal(pipeline.JobStatusCompleted))

				mu.Lock()
				defer mu.Unlock()
				for _, name := range []string{"requires", "relationship"} {
					Expect(params[name]).To(SatisfyAll(
						HaveKeyWithValue("source_framework_c1", "CSF 2.0"),
						HaveKeyWithValue("target_framework_c1", "CSF 2.0"),
						HaveKeyWithValue("source_framework_t1", "NIST SP 800-53"),
						HaveKeyWithValue("target_framework_t1", "NIST SP 800-53"),
					), name)
				}
			})

			// A snapshot does not make a deleted catalog's job runnable: the
			// catalog is still read, so it fails before any analyzer runs.
			It("still fails a job with a snapshot whose catalog was deleted, before any analyzer runs", func() {
				pairJobConfig = `{"catalog_id":"cat-1","target_catalog_id":"cat-gone",` +
					`"source_catalog":{"catalog_id":"cat-1","name":"CSF 2.0"},"target_catalog":{"catalog_id":"cat-gone","name":"Gone"}}`
				startPairJob(classified, false)

				Eventually(jobStatus, "2s", "50ms").Should(Equal(pipeline.JobStatusFailed))
				j, err := store.GetJob(ctx, jobID)
				Expect(err).NotTo(HaveOccurred())
				Expect(j.ErrorMessage).To(ContainSubstring("catalog cat-gone was not found"))
				mu.Lock()
				defer mu.Unlock()
				Expect(params).To(BeEmpty())
			})

			// The names are read before any analyzer runs, so a job that
			// cannot name a catalog spends no LLM calls.
			// The new-job command puts the placeholder on whichever side
			// names the missing catalog.
			DescribeTable("fails the job before any analyzer runs when a catalog's name cannot be read",
				func(config, command string) {
					pairJobConfig = config
					pairControls = &fakeCatalogControlsReader{byCatalog: map[string][]*pb.Control{
						"cat-1":    {{ControlId: "c1", CatalogId: "cat-1", Statement: "Keep an asset inventory."}},
						"cat-gone": {{ControlId: "g1", CatalogId: "cat-gone", Statement: "Gone."}},
					}}
					classify := startPairJob(classified, false)

					Eventually(jobStatus, "2s", "50ms").Should(Equal(pipeline.JobStatusFailed))

					j, err := store.GetJob(ctx, jobID)
					Expect(err).NotTo(HaveOccurred())
					Expect(j.ErrorMessage).To(SatisfyAll(
						ContainSubstring("catalog cat-gone was not found"),
						ContainSubstring(pipeline.ErrCatalogNotFound.Error()),
						ContainSubstring("it may have been deleted or purged. Import the same file again (it gets the same catalog ID)"),
						ContainSubstring("crosscodex run retry --from-failure "+jobID),
						HaveSuffix("or start a new job with another catalog: "+command),
					))
					Expect(j.ErrorClass).To(Equal(pipeline.JobErrorRetryable))
					Expect(classify.Inputs()).To(BeEmpty())
					mu.Lock()
					defer mu.Unlock()
					Expect(params).To(BeEmpty())
				},
				Entry("the target is missing", `{"catalog_id":"cat-1","target_catalog_id":"cat-gone","bidirectional":true}`,
					"crosscodex run start --source cat-1 --target <catalog-id> --bidirectional"),
				Entry("the source is missing", `{"catalog_id":"cat-gone","target_catalog_id":"cat-1"}`,
					"crosscodex run start --source <catalog-id> --target cat-1"),
				Entry("the one catalog of a job within it is missing", `{"catalog_id":"cat-gone"}`,
					"crosscodex run start --source <catalog-id> --target <catalog-id>"),
			)

			It("fails the job as retryable, without deleted-catalog guidance, when a catalog's name read fails for another reason", func() {
				pairNames = failingCatalogNameReader{err: errors.New("connection refused")}
				classify := startPairJob(classified, false)

				Eventually(jobStatus, "2s", "50ms").Should(Equal(pipeline.JobStatusFailed))

				j, err := store.GetJob(ctx, jobID)
				Expect(err).NotTo(HaveOccurred())
				Expect(j.ErrorMessage).To(SatisfyAll(
					ContainSubstring("the name of catalog cat-1 could not be read: connection refused"),
					ContainSubstring("crosscodex run retry --from-failure "+jobID),
					Not(ContainSubstring("deleted or purged")),
				))
				Expect(j.ErrorClass).To(Equal(pipeline.JobErrorRetryable))
				Expect(classify.Inputs()).To(BeEmpty())
				mu.Lock()
				defer mu.Unlock()
				Expect(params).To(BeEmpty())
			})

			It("passes classifications persisted by a prior process run on resume", func() {
				classify := startPairJob(classified, true)

				Eventually(jobStatus, "2s", "50ms").Should(Equal(pipeline.JobStatusCompleted))

				Expect(classify.Inputs()).To(BeEmpty())
				mu.Lock()
				defer mu.Unlock()
				for _, name := range []string{"requires", "relationship"} {
					Expect(params[name]).To(SatisfyAll(
						HaveKeyWithValue("source_type_c1", "Technical"),
						HaveKeyWithValue("target_level_c2", "Tactical"),
					), name)
				}
			})

			It("fails the job without dispatching pair work when classify results do not parse", func() {
				startPairJob([]byte("{"), false)

				Eventually(jobStatus, "2s", "50ms").Should(Equal(pipeline.JobStatusFailed))

				j, err := store.GetJob(ctx, jobID)
				Expect(err).NotTo(HaveOccurred())
				Expect(j.ErrorMessage).To(ContainSubstring("classify results"))
				mu.Lock()
				defer mu.Unlock()
				Expect(params).To(BeEmpty())
			})
		})

		It("leaves Controls empty and uses a placeholder Input for the candidate-dependent pass", func() {
			type controlCall struct {
				controlID string
			}
			var mu sync.Mutex
			var classifyCalls, requiresCalls, relationshipCalls []controlCall

			reg := analyzer.NewRegistry()
			classifyStub := newControlStub("classify")
			classifyStub.GenerateWorkFunc = func(_ context.Context, input *pb.Control, _ analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
				mu.Lock()
				classifyCalls = append(classifyCalls, controlCall{controlID: input.GetControlId()})
				mu.Unlock()
				return []analyzer.Task{{TaskID: "classify-" + input.GetControlId(), Payload: &emptypb.Empty{}}}, nil
			}
			Expect(analyzer.Register[*pb.Control](reg, classifyStub)).To(Succeed())
			requiresStub := newControlStub("requires", "classify")
			requiresStub.GenerateWorkFunc = func(_ context.Context, input *pb.Control, _ analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
				mu.Lock()
				requiresCalls = append(requiresCalls, controlCall{controlID: input.GetControlId()})
				mu.Unlock()
				return []analyzer.Task{{TaskID: "requires-task", Payload: &emptypb.Empty{}}}, nil
			}
			Expect(analyzer.Register[*pb.Control](reg, requiresStub)).To(Succeed())
			relationshipStub := newControlStub("relationship", "classify")
			relationshipStub.GenerateWorkFunc = func(_ context.Context, input *pb.Control, _ analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
				mu.Lock()
				relationshipCalls = append(relationshipCalls, controlCall{controlID: input.GetControlId()})
				mu.Unlock()
				return []analyzer.Task{{TaskID: "relationship-task", Payload: &emptypb.Empty{}}}, nil
			}
			Expect(analyzer.Register[*pb.Control](reg, relationshipStub)).To(Succeed())

			dispatcher := &executorTestDispatcher{}
			collector := &executorTestCollector{}
			eng := analysis.New(reg, dispatcher, collector, config.EngineConfig{
				TaskTimeout:  5 * time.Minute,
				MaxRetries:   3,
				RetryBackoff: time.Millisecond,
			}, map[string]natsbus.TaskType{
				"classify":     "classify",
				"requires":     "requires",
				"relationship": "relate",
			})

			catalogReader := &fakeCatalogControlsReader{controls: []*pb.Control{
				{ControlId: "c1", CatalogId: "cat-1"},
				{ControlId: "c2", CatalogId: "cat-1"},
			}}

			svc = pipeline.New(store, eng, reg, synth, pipeline.Attestors{}, converter, bus, storage,
				config.PipelineConfig{MaxConcurrentJobs: 10},
				nil,
				pipeline.WithCatalogControlsReader(catalogReader),
			)

			job := &pipeline.Job{
				JobID:     jobID,
				TenantID:  tenantID,
				Status:    pipeline.JobStatusRunning,
				Config:    []byte(`{"catalog_id":"cat-1"}`),
				CreatedAt: time.Now(),
				UpdatedAt: time.Now(),
			}
			Expect(store.CreateJob(ctx, job)).To(Succeed())
			Expect(store.CreateStages(ctx, jobID, []string{"classify", "candidate_generation", "requires", "relationship", "synthesis", "graph"})).To(Succeed())

			runCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			Expect(svc.Start(runCtx)).To(Succeed())

			Eventually(func() pipeline.JobStatus {
				j, _ := store.GetJob(ctx, jobID)
				if j == nil {
					return ""
				}
				return j.Status
			}, "2s", "50ms").Should(Equal(pipeline.JobStatusCompleted))

			mu.Lock()
			defer mu.Unlock()
			// classify fans out per catalog control, proving Controls did apply
			// to the pre-candidate pass for this job.
			Expect(classifyCalls).To(HaveLen(2))
			// requires/relationship each get exactly one call with a placeholder
			// (empty ControlId) *pb.Control rather than one call per catalog
			// control — proof Controls was left empty for the
			// candidate-dependent pass despite the catalog having controls.
			Expect(requiresCalls).To(HaveLen(1))
			Expect(requiresCalls[0].controlID).To(BeEmpty())
			Expect(relationshipCalls).To(HaveLen(1))
			Expect(relationshipCalls[0].controlID).To(BeEmpty())
		})

		It("with no catalog controls reader wired, leaves per-control analyzers at zero tasks instead of erroring", func() {
			var mu sync.Mutex
			var classifyCalls int
			var requiresCalls int
			var requiresControlID string

			// classify is registered as Analyzer[*pb.Control], matching every
			// real production analyzer. Before the fix, a job with no controls
			// (Controls empty) fell back to calling GenerateWorkFromProto with
			// req.Input == &emptypb.Empty{}, which fails registeredWrapper's
			// *pb.Control type assertion and crashes the whole job (see
			// Finding 1). The fix treats a non-nil, empty Controls as
			// "per-control mode, zero controls": classify's GenerateWork is
			// never invoked, and the analyzer completes with a zero-task
			// output instead.
			reg := analyzer.NewRegistry()
			classifyStub := newControlStub("classify")
			classifyStub.GenerateWorkFunc = func(_ context.Context, _ *pb.Control, _ analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
				mu.Lock()
				classifyCalls++
				mu.Unlock()
				return []analyzer.Task{{TaskID: "classify-task", Payload: &emptypb.Empty{}}}, nil
			}
			Expect(analyzer.Register[*pb.Control](reg, classifyStub)).To(Succeed())
			requiresStub := newControlStub("requires", "classify")
			requiresStub.GenerateWorkFunc = func(_ context.Context, input *pb.Control, _ analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
				mu.Lock()
				requiresCalls++
				requiresControlID = input.GetControlId()
				mu.Unlock()
				return []analyzer.Task{{TaskID: "requires-task", Payload: &emptypb.Empty{}}}, nil
			}
			Expect(analyzer.Register[*pb.Control](reg, requiresStub)).To(Succeed())

			dispatcher := &executorTestDispatcher{}
			collector := &executorTestCollector{}
			// Wire a DB-backed stage reporter (as production does) so that
			// classify's zero-task completion is durably persisted, and this
			// test can assert on the stage's stored status rather than only
			// on overall job completion.
			dbReporter := pipeline.NewDBStageReporter(&fakeNATSReporter{}, store)
			eng := analysis.New(reg, dispatcher, collector, config.EngineConfig{
				TaskTimeout:  5 * time.Minute,
				MaxRetries:   3,
				RetryBackoff: time.Millisecond,
			}, map[string]natsbus.TaskType{
				"classify": "classify",
				"requires": "requires",
			}, analysis.WithStageReporter(dbReporter))

			svc = pipeline.New(store, eng, reg, synth, pipeline.Attestors{}, converter, bus, storage,
				config.PipelineConfig{MaxConcurrentJobs: 10},
				nil,
			)

			job := &pipeline.Job{
				JobID:     jobID,
				TenantID:  tenantID,
				Status:    pipeline.JobStatusRunning,
				Config:    catalogJobConfig,
				CreatedAt: time.Now(),
				UpdatedAt: time.Now(),
			}
			Expect(store.CreateJob(ctx, job)).To(Succeed())
			Expect(store.CreateStages(ctx, jobID, []string{"classify", "candidate_generation", "requires", "synthesis", "graph"})).To(Succeed())

			runCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			Expect(svc.Start(runCtx)).To(Succeed())

			// The job must complete successfully rather than failing on a
			// type-assertion error inside the engine.
			Eventually(func() pipeline.JobStatus {
				j, _ := store.GetJob(ctx, jobID)
				if j == nil {
					return ""
				}
				return j.Status
			}, "2s", "50ms").Should(Equal(pipeline.JobStatusCompleted))

			stages, err := store.GetStages(ctx, jobID)
			Expect(err).NotTo(HaveOccurred())
			classifyStage := findStage(stages, "classify")
			Expect(classifyStage).NotTo(BeNil())
			Expect(classifyStage.Status).To(Equal(pipeline.StageStatusCompleted))

			mu.Lock()
			defer mu.Unlock()
			// classify's GenerateWork is never invoked: Controls is a
			// non-nil, empty slice when no controls reader is wired, so the
			// per-control loop in executeAnalyzer runs zero iterations
			// rather than falling back to the single-Input path (which would
			// type-assert req.Input == &emptypb.Empty{} against *pb.Control
			// and fail).
			Expect(classifyCalls).To(Equal(0))
			// requires (candidate-dependent pass) is unaffected: it always
			// runs with Controls nil and a placeholder *pb.Control Input,
			// however many controls the catalog has.
			Expect(requiresCalls).To(Equal(1))
			Expect(requiresControlID).To(BeEmpty())
		})
	})

	Describe("Start", func() {
		It("bounds concurrent resumed jobs to MaxConcurrentJobs", func() {
			const cap = 2
			const total = 5

			release := make(chan struct{})
			var inFlight, maxInFlight int32
			barrierStore := newFakeStore()
			barrierStore.getJobHook = func() {
				n := atomic.AddInt32(&inFlight, 1)
				for {
					old := atomic.LoadInt32(&maxInFlight)
					if n <= old || atomic.CompareAndSwapInt32(&maxInFlight, old, n) {
						break
					}
				}
				<-release // hold the slot until the test releases it
				atomic.AddInt32(&inFlight, -1)
			}

			cfg := config.PipelineConfig{MaxConcurrentJobs: cap}
			capSvc := pipeline.New(barrierStore, engine, registry, synth, pipeline.Attestors{}, converter, bus, storage, cfg, nil)

			for i := 0; i < total; i++ {
				id := fmt.Sprintf("resume-job-%d", i)
				barrierStore.jobs[id] = &pipeline.Job{
					JobID:     id,
					TenantID:  tenantID,
					Status:    pipeline.JobStatusRunning,
					Config:    catalogJobConfig,
					CreatedAt: time.Now(),
					UpdatedAt: time.Now(),
				}
			}

			Expect(capSvc.Start(ctx)).To(Succeed())

			// At most `cap` jobs may sit at the barrier at once.
			Eventually(func() int32 { return atomic.LoadInt32(&inFlight) }, "1s", "20ms").Should(Equal(int32(cap)))
			Consistently(func() int32 { return atomic.LoadInt32(&maxInFlight) }, "300ms", "20ms").Should(BeNumerically("<=", int32(cap)))

			// Drain: releasing the barrier lets all jobs proceed; the dispatcher then
			// admits the remaining jobs, still capped, until all are done.
			close(release)
			Eventually(func() int32 { return atomic.LoadInt32(&inFlight) }, "2s", "20ms").Should(Equal(int32(0)))
			Expect(atomic.LoadInt32(&maxInFlight)).To(BeNumerically("<=", int32(cap)))

			Expect(capSvc.Stop(context.Background())).To(Succeed())
		})
	})
})
