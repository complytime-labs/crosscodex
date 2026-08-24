package pipeline_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

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
	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// fakeNATSClient records publishes for test assertions.
type fakeNATSClient struct {
	mu         sync.Mutex
	publishes  []fakePublish
	publishErr error
}

type fakePublish struct {
	subject string
	payload []byte
}

func (f *fakeNATSClient) Publish(_ context.Context, subject string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.publishErr != nil {
		return f.publishErr
	}
	f.publishes = append(f.publishes, fakePublish{subject: subject, payload: data})
	return nil
}

func (f *fakeNATSClient) PublishWithHeaders(_ context.Context, subject string, data []byte, headers map[string][]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.publishErr != nil {
		return f.publishErr
	}
	f.publishes = append(f.publishes, fakePublish{subject: subject, payload: data})
	return nil
}

func (f *fakeNATSClient) Subscribe(_ context.Context, _ string, _ natsbus.MessageHandler) (natsbus.Subscription, error) {
	return nil, errors.New("Subscribe not implemented in fakeNATSClient")
}

func (f *fakeNATSClient) QueueSubscribe(_ context.Context, _, _ string, _ natsbus.MessageHandler) (natsbus.Subscription, error) {
	return nil, errors.New("QueueSubscribe not implemented in fakeNATSClient")
}

func (f *fakeNATSClient) CreateStream(_ context.Context, _ natsbus.StreamConfig) error {
	return errors.New("CreateStream not implemented in fakeNATSClient")
}

func (f *fakeNATSClient) DeleteStream(_ context.Context, _ string) error {
	return errors.New("DeleteStream not implemented in fakeNATSClient")
}

func (f *fakeNATSClient) Close() error {
	return nil
}

func (f *fakeNATSClient) publishCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.publishes)
}

// hasPublish reports whether a message was published to subject. It reads the
// recorded publishes under the lock so callers (e.g. Eventually polls) do not
// race the async executeJob goroutine that appends via Publish.
func (f *fakeNATSClient) hasPublish(subject string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, pub := range f.publishes {
		if pub.subject == subject {
			return true
		}
	}
	return false
}

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
type fakeAttestation struct {
	mu          sync.Mutex
	layoutCalls []attestation.LayoutOptions
	linkCalls   []fakeLinkCall
	layout      *attestation.SignedLayout
	layoutErr   error
	linkErr     error
	verifyErr   error
}

type fakeLinkCall struct {
	step      string
	materials []attestation.Artifact
	products  []attestation.Artifact
}

func (f *fakeAttestation) CreateLayout(ctx context.Context, opts attestation.LayoutOptions) (*attestation.SignedLayout, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.layoutCalls = append(f.layoutCalls, opts)
	if f.layoutErr != nil {
		return nil, f.layoutErr
	}
	if f.layout != nil {
		return f.layout, nil
	}
	return &attestation.SignedLayout{Raw: []byte(`{"_type":"layout"}`)}, nil
}

func (f *fakeAttestation) CreateLink(ctx context.Context, step string, materials, products []attestation.Artifact, opts ...attestation.LinkOption) (*attestation.SignedLink, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.linkCalls = append(f.linkCalls, fakeLinkCall{
		step:      step,
		materials: materials,
		products:  products,
	})
	if f.linkErr != nil {
		return nil, f.linkErr
	}
	linkJSON := fmt.Sprintf(`{"_type":"link","name":%q}`, step)
	return &attestation.SignedLink{
		Raw:       []byte(linkJSON),
		Step:      step,
		Materials: materials,
		Products:  products,
	}, nil
}

func (f *fakeAttestation) VerifyLayout(ctx context.Context, data []byte) (*attestation.VerifiedLayout, error) {
	return nil, errors.New("VerifyLayout not implemented in fakeAttestation")
}

func (f *fakeAttestation) VerifyLink(ctx context.Context, data []byte) (*attestation.VerifiedLink, error) {
	return nil, errors.New("VerifyLink not implemented in fakeAttestation")
}

func (f *fakeAttestation) Verify(ctx context.Context, data []byte) (*attestation.VerifiedLink, error) {
	return nil, errors.New("Verify not implemented in fakeAttestation")
}

func (f *fakeAttestation) VerifyChain(ctx context.Context, layout *attestation.SignedLayout, links []*attestation.SignedLink) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.verifyErr != nil {
		return f.verifyErr
	}
	return nil
}

func (f *fakeAttestation) linkCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.linkCalls)
}

// executorFakeStorage implements storage.Provider for executor tests.
type executorFakeStorage struct {
	mu      sync.Mutex
	objects map[string][]byte
	putErr  error
	getErr  error
}

func newExecutorFakeStorage() *executorFakeStorage {
	return &executorFakeStorage{
		objects: make(map[string][]byte),
	}
}

func (f *executorFakeStorage) Put(ctx context.Context, key string, data io.Reader) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putErr != nil {
		return f.putErr
	}
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(data); err != nil {
		return err
	}
	f.objects[key] = buf.Bytes()
	return nil
}

func (f *executorFakeStorage) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	data, exists := f.objects[key]
	if !exists {
		return nil, errors.New("object not found")
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (f *executorFakeStorage) Delete(ctx context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, key)
	return nil
}

func (f *executorFakeStorage) List(ctx context.Context, prefix string) ([]storage.ObjectMetadata, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []storage.ObjectMetadata
	for k := range f.objects {
		result = append(result, storage.ObjectMetadata{Key: k})
	}
	return result, nil
}

func (f *executorFakeStorage) Exists(ctx context.Context, key string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, exists := f.objects[key]
	return exists, nil
}

func (f *executorFakeStorage) Stat(ctx context.Context, key string) (*storage.ObjectMetadata, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, exists := f.objects[key]
	if !exists {
		return nil, errors.New("object not found")
	}
	return &storage.ObjectMetadata{Key: key, Size: int64(len(data))}, nil
}

func (f *executorFakeStorage) Close() error {
	return nil
}

// stubAnalyzer implements analyzer.Analyzer[*emptypb.Empty] for executor tests.
type stubAnalyzer struct {
	name        string
	deps        []string
	genWorkFn   func(ctx context.Context, input *emptypb.Empty, cfg analyzer.AnalyzerConfig) ([]analyzer.Task, error)
	aggregateFn func(ctx context.Context, taskResults []analyzer.TaskResult) (*analyzer.Output, error)
}

func (s *stubAnalyzer) Name() string        { return s.name }
func (s *stubAnalyzer) DependsOn() []string { return s.deps }

func (s *stubAnalyzer) GenerateWork(ctx context.Context, input *emptypb.Empty, cfg analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
	if s.genWorkFn != nil {
		return s.genWorkFn(ctx, input, cfg)
	}
	return nil, nil
}

func (s *stubAnalyzer) Aggregate(ctx context.Context, taskResults []analyzer.TaskResult) (*analyzer.Output, error) {
	if s.aggregateFn != nil {
		return s.aggregateFn(ctx, taskResults)
	}
	return &analyzer.Output{AnalyzerName: s.name, Metadata: map[string]string{}}, nil
}

func (s *stubAnalyzer) ResultSchema() proto.Message { return &emptypb.Empty{} }

// stubControlAnalyzer implements analyzer.Analyzer[*pb.Control], the generic
// type every production analyzer (classify/embedding/artifacts/requires/
// relationship) is registered as. Needed wherever a test analyzer must
// participate in Controls fan-out (per-control GenerateWork calls) or receive
// the candidate-dependent pass's placeholder *pb.Control Input — both call
// GenerateWorkFromProto with a *pb.Control, which fails the registeredWrapper
// type assertion against stubAnalyzer's *emptypb.Empty.
type stubControlAnalyzer struct {
	name      string
	deps      []string
	genWorkFn func(ctx context.Context, input *pb.Control, cfg analyzer.AnalyzerConfig) ([]analyzer.Task, error)
}

func (s *stubControlAnalyzer) Name() string        { return s.name }
func (s *stubControlAnalyzer) DependsOn() []string { return s.deps }

func (s *stubControlAnalyzer) GenerateWork(ctx context.Context, input *pb.Control, cfg analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
	if s.genWorkFn != nil {
		return s.genWorkFn(ctx, input, cfg)
	}
	return nil, nil
}

func (s *stubControlAnalyzer) Aggregate(ctx context.Context, taskResults []analyzer.TaskResult) (*analyzer.Output, error) {
	return &analyzer.Output{AnalyzerName: s.name, Metadata: map[string]string{}}, nil
}

func (s *stubControlAnalyzer) ResultSchema() proto.Message { return &pb.Control{} }

// fakeCatalogControlsReader implements pipeline.CatalogControlsReader,
// recording each call's (tenantID, catalogID) and returning a fixed control
// set for executor tests.
type fakeCatalogControlsReader struct {
	mu       sync.Mutex
	calls    []fakeCatalogControlsCall
	controls []*pb.Control
}

type fakeCatalogControlsCall struct {
	tenantID  string
	catalogID string
}

func (f *fakeCatalogControlsReader) Controls(_ context.Context, tenantID, catalogID string) ([]*pb.Control, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCatalogControlsCall{tenantID: tenantID, catalogID: catalogID})
	return f.controls, nil
}

var _ pipeline.CatalogControlsReader = (*fakeCatalogControlsReader)(nil)

// executorTestDispatcher records Dispatch calls.
type executorTestDispatcher struct {
	mu          sync.Mutex
	dispatches  []executorDispatchCall
	dispatchErr error
}

type executorDispatchCall struct {
	tasks    []analyzer.Task
	taskType natsbus.TaskType
	jobID    string
}

func (d *executorTestDispatcher) Dispatch(_ context.Context, tasks []analyzer.Task, taskType natsbus.TaskType, jobID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dispatchErr != nil {
		return d.dispatchErr
	}
	d.dispatches = append(d.dispatches, executorDispatchCall{tasks: tasks, taskType: taskType, jobID: jobID})
	return nil
}

func (d *executorTestDispatcher) Redispatch(_ context.Context, _ analyzer.Task, _ natsbus.TaskType, _ string, _ int) error {
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
// classify/embedding run against a catalog-backed job (see twoPassJob) so
// their per-control GenerateWork actually fires; a document-backed job would
// leave Controls empty and, correctly, produce zero tasks for them (see
// "on a document-backed job (no catalog_id) leaves Controls empty for both
// passes" below). requires/relationship always run with Controls empty and a
// placeholder *pb.Control Input regardless of whether the job is
// catalog-backed, since they read candidate pairs directly instead of
// fanning out per control.
func registerTwoPassAnalyzers(reg *analyzer.Registry) {
	mkControl := func(name string) func(context.Context, *pb.Control, analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
		return func(_ context.Context, _ *pb.Control, _ analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
			return []analyzer.Task{{TaskID: name + "-task", Payload: &emptypb.Empty{}}}, nil
		}
	}
	Expect(analyzer.Register[*pb.Control](reg, &stubControlAnalyzer{name: "classify", genWorkFn: mkControl("classify")})).To(Succeed())
	Expect(analyzer.Register[*pb.Control](reg, &stubControlAnalyzer{name: "embedding", deps: []string{"classify"}, genWorkFn: mkControl("embedding")})).To(Succeed())
	Expect(analyzer.Register[*pb.Control](reg, &stubControlAnalyzer{name: "requires", deps: []string{"embedding"}, genWorkFn: mkControl("requires")})).To(Succeed())
	Expect(analyzer.Register[*pb.Control](reg, &stubControlAnalyzer{name: "relationship", deps: []string{"embedding"}, genWorkFn: mkControl("relationship")})).To(Succeed())
}

var _ = Describe("Executor", func() {
	var (
		ctx       context.Context
		store     *fakeStore
		registry  *analyzer.Registry
		engine    *analysis.Engine
		synth     *fakeSynthesisExecutor
		attestor  *fakeAttestation
		converter *pipelineattestation.Converter
		bus       *fakeNATSClient
		storage   *executorFakeStorage
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
		attestor = &fakeAttestation{}
		converter = pipelineattestation.NewConverter()
		bus = &fakeNATSClient{}
		storage = newExecutorFakeStorage()

		// Register stub analyzers.
		a1 := &stubAnalyzer{name: "analyzer-a"}
		a2 := &stubAnalyzer{name: "analyzer-b", deps: []string{"analyzer-a"}}

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
		engine = analysis.New(registry, dispatcher, collector, engineCfg, taskTypes)

		// Create service.
		pipelineCfg := config.PipelineConfig{
			MaxConcurrentJobs: 10,
		}
		attCfg := config.AttestationConfig{
			ExpiryDuration: 168 * time.Hour,
		}
		svc = pipeline.New(store, engine, registry, synth, attestor, converter, bus, storage, pipelineCfg, attCfg, nil)
	})

	Describe("executeJob", func() {
		It("recovers from a panic in job execution and fails the job", func() {
			jobConfig := map[string]interface{}{"test": "config"}
			configBytes, err := json.Marshal(jobConfig)
			Expect(err).NotTo(HaveOccurred())

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
			jobConfig := map[string]interface{}{"test": "config"}
			configBytes, err := json.Marshal(jobConfig)
			Expect(err).NotTo(HaveOccurred())

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

			err = svc.Start(ctx)
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
			Expect(bus.publishCount()).To(BeNumerically(">", 0))
		})

		It("skips completed stages on resume", func() {
			// Create job with some stages already completed.
			jobConfig := map[string]interface{}{"test": "config"}
			configBytes, err := json.Marshal(jobConfig)
			Expect(err).NotTo(HaveOccurred())

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

			err = svc.Start(ctx)
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
			// exercised against a catalog-backed job with one control, so
			// GenerateWork actually fires and dispatch is reached — a
			// document-backed job would leave Controls empty and correctly
			// produce zero tasks, never reaching the failing dispatcher.
			failRegistry := analyzer.NewRegistry()
			genWork := func(_ context.Context, _ *pb.Control, _ analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
				return []analyzer.Task{{TaskID: "t1", Payload: &emptypb.Empty{}}}, nil
			}
			fa1 := &stubControlAnalyzer{name: "analyzer-a", genWorkFn: genWork}
			fa2 := &stubControlAnalyzer{name: "analyzer-b", deps: []string{"analyzer-a"}, genWorkFn: genWork}
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
			attCfg := config.AttestationConfig{ExpiryDuration: 168 * time.Hour}
			catalogReader := &fakeCatalogControlsReader{controls: []*pb.Control{
				{ControlId: "c1", CatalogId: "fail-cat-1"},
			}}
			failSvc := pipeline.New(store, failEngine, failRegistry, synth, attestor, converter, bus, storage, pipelineCfg, attCfg, nil,
				pipeline.WithCatalogControlsReader(catalogReader))

			// Create job.
			jobConfig := map[string]interface{}{"Source": map[string]interface{}{"CatalogId": "fail-cat-1"}}
			configBytes, err := json.Marshal(jobConfig)
			Expect(err).NotTo(HaveOccurred())

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

			err = failSvc.Start(ctx)
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
			jobConfig := map[string]interface{}{"test": "config"}
			configBytes, err := json.Marshal(jobConfig)
			Expect(err).NotTo(HaveOccurred())

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

			err = svc.Start(ctx)
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
			Expect(bus.publishCount()).To(BeNumerically(">=", 6))
		})

		It("completes job even when attestation finalization fails", func() {
			// Create attestor that fails.
			attestor.layoutErr = errors.New("attestation generation failed")

			// Create job.
			jobConfig := map[string]interface{}{"test": "config"}
			configBytes, err := json.Marshal(jobConfig)
			Expect(err).NotTo(HaveOccurred())

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

			err = svc.Start(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Wait for completion (job should still complete despite attestation failure).
			Eventually(func() pipeline.JobStatus {
				j, _ := store.GetJob(ctx, jobID)
				if j == nil {
					return ""
				}
				return j.Status
			}, "1s", "50ms").Should(Equal(pipeline.JobStatusCompleted))
		})

		It("creates layout before analysis", func() {
			// Create job.
			jobConfig := map[string]interface{}{"test": "config"}
			configBytes, err := json.Marshal(jobConfig)
			Expect(err).NotTo(HaveOccurred())

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

			err = svc.Start(ctx)
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
			jobConfig := map[string]interface{}{"test": "config"}
			configBytes, err := json.Marshal(jobConfig)
			Expect(err).NotTo(HaveOccurred())

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

			err = svc.Start(ctx)
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
			jobConfig := map[string]interface{}{"test": "config"}
			configBytes, err := json.Marshal(jobConfig)
			Expect(err).NotTo(HaveOccurred())

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

			err = svc.Start(ctx)
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
			failSvc := pipeline.New(store, engine, registry, failSynth, attestor, converter, bus, storage,
				config.PipelineConfig{MaxConcurrentJobs: 10},
				config.AttestationConfig{ExpiryDuration: 168 * time.Hour},
				nil,
			)

			// Create job.
			jobConfig := map[string]interface{}{"test": "config"}
			configBytes, err := json.Marshal(jobConfig)
			Expect(err).NotTo(HaveOccurred())

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

			err = failSvc.Start(ctx)
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

		It("completes job even when VerifyChain fails", func() {
			// Set VerifyChain to fail. Layout creation still succeeds,
			// so finalizeAttestation reaches VerifyChain and returns an error.
			// executeJob treats that as non-fatal.
			attestor.verifyErr = errors.New("chain verification failed")

			// Create job.
			jobConfig := map[string]interface{}{"test": "config"}
			configBytes, err := json.Marshal(jobConfig)
			Expect(err).NotTo(HaveOccurred())

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

			err = svc.Start(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Job should still complete despite chain verification failure.
			Eventually(func() pipeline.JobStatus {
				j, _ := store.GetJob(ctx, jobID)
				if j == nil {
					return ""
				}
				return j.Status
			}, "1s", "50ms").Should(Equal(pipeline.JobStatusCompleted))

			// Verify no attestation bundle was stored (chain verification
			// failed before bundle creation).
			bundlePath := fmt.Sprintf("attestations/%s/%s/bundle.json", tenantID, jobID)
			exists, err := storage.Exists(ctx, bundlePath)
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeFalse())
		})

		It("publishes bundle to audit trail", func() {
			// Create job.
			jobConfig := map[string]interface{}{"test": "config"}
			configBytes, err := json.Marshal(jobConfig)
			Expect(err).NotTo(HaveOccurred())

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

			err = svc.Start(ctx)
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
				return bus.hasPublish(expectedSubject)
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
			// (matching production), so they need a catalog-backed job with at
			// least one control to actually fan out and dispatch. A
			// document-backed job would leave Controls empty and correctly
			// produce zero tasks for them (see the dedicated document-backed
			// test below) -- which would leave these two-pass ordering tests
			// unable to observe classify/embedding's dispatch at all.
			twoPassCatalog = &fakeCatalogControlsReader{controls: []*pb.Control{
				{ControlId: "c1", CatalogId: "two-pass-cat-1"},
			}}
		})

		// twoPassJob creates a running, catalog-backed job with the full
		// two-pass stage list.
		twoPassJob := func() {
			configBytes, err := json.Marshal(map[string]interface{}{
				"Source": map[string]interface{}{"CatalogId": "two-pass-cat-1"},
			})
			Expect(err).NotTo(HaveOccurred())
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

		It("runs candidate generation between the pre- and candidate-dependent passes", func() {
			gen := &fakeCandidateGen{}
			gen.dispatchCountFn = func() int {
				twoPassDispatcher.mu.Lock()
				defer twoPassDispatcher.mu.Unlock()
				return len(twoPassDispatcher.dispatches)
			}

			svc = pipeline.New(store, twoPassEngine, twoPassRegistry, synth, attestor, converter, bus, storage,
				config.PipelineConfig{MaxConcurrentJobs: 10},
				config.AttestationConfig{ExpiryDuration: 168 * time.Hour},
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

			svc = pipeline.New(store, twoPassEngine, twoPassRegistry, synth, attestor, converter, bus, storage,
				config.PipelineConfig{MaxConcurrentJobs: 10},
				config.AttestationConfig{ExpiryDuration: 168 * time.Hour},
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

			svc = pipeline.New(store, twoPassEngine, twoPassRegistry, synth, attestor, converter, bus, storage,
				config.PipelineConfig{MaxConcurrentJobs: 10},
				config.AttestationConfig{ExpiryDuration: 168 * time.Hour},
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

		It("treats ErrNotACatalogJob as a no-op and still runs the candidate-dependent pass", func() {
			gen := &fakeCandidateGen{err: pipeline.ErrNotACatalogJob}

			svc = pipeline.New(store, twoPassEngine, twoPassRegistry, synth, attestor, converter, bus, storage,
				config.PipelineConfig{MaxConcurrentJobs: 10},
				config.AttestationConfig{ExpiryDuration: 168 * time.Hour},
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
			// Candidate-dependent pass still ran despite zero candidates.
			types := dispatchedTypes()
			Expect(types).To(HaveKey(natsbus.TaskType("requires")))
			Expect(types).To(HaveKey(natsbus.TaskType("relate")))
			Expect(stageStatus("candidate_generation")).To(Equal(pipeline.StageStatusCompleted))
		})

		It("does not re-execute a pre-candidate analyzer already completed by a prior process (crash-recovery resume)", func() {
			gen := &fakeCandidateGen{}

			svc = pipeline.New(store, twoPassEngine, twoPassRegistry, synth, attestor, converter, bus, storage,
				config.PipelineConfig{MaxConcurrentJobs: 10},
				config.AttestationConfig{ExpiryDuration: 168 * time.Hour},
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
			Expect(analyzer.Register[*pb.Control](reg, &stubControlAnalyzer{name: "classify"})).To(Succeed())
			Expect(analyzer.Register[*pb.Control](reg, &stubControlAnalyzer{name: "embedding", deps: []string{"classify"}})).To(Succeed())
			Expect(analyzer.Register[*pb.Control](reg, &stubControlAnalyzer{name: "artifacts"})).To(Succeed())
			Expect(analyzer.Register[*pb.Control](reg, &stubControlAnalyzer{
				name: "requires",
				deps: []string{"embedding"},
				genWorkFn: func(_ context.Context, _ *pb.Control, _ analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
					return []analyzer.Task{{TaskID: "requires-task", Payload: &emptypb.Empty{}}}, nil
				},
			})).To(Succeed())
			Expect(analyzer.Register[*pb.Control](reg, &stubControlAnalyzer{
				name: "relationship",
				deps: []string{"embedding"},
				genWorkFn: func(_ context.Context, _ *pb.Control, _ analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
					return []analyzer.Task{{TaskID: "relationship-task", Payload: &emptypb.Empty{}}}, nil
				},
			})).To(Succeed())

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

			svc = pipeline.New(store, eng, reg, synth, attestor, converter, bus, storage,
				config.PipelineConfig{MaxConcurrentJobs: 10},
				config.AttestationConfig{ExpiryDuration: 168 * time.Hour},
				nil,
			)

			configBytes, err := json.Marshal(map[string]interface{}{"test": "config"})
			Expect(err).NotTo(HaveOccurred())
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
				{SourceID: "AC-1", TargetID: "AC-2", RelationshipType: "supports", Confidence: 0.8},
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
			Expect(pairs).To(HaveKey([2]string{"AC-1", "AC-2"}))
			Expect(pairs[[2]string{"AC-1", "AC-2"}].Consensus).To(Equal("supports"))
		})
	})

	Describe("controls and analyzer config wiring", func() {
		It("populates Controls and CatalogID for the pre-candidate pass on a catalog-backed job", func() {
			type call struct {
				controlID string
				catalogID string
				ok        bool
			}
			var mu sync.Mutex
			var calls []call

			reg := analyzer.NewRegistry()
			Expect(analyzer.Register[*pb.Control](reg, &stubControlAnalyzer{
				name: "classify",
				genWorkFn: func(ctx context.Context, input *pb.Control, _ analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
					catalogID, ok := analyzer.CatalogIDFromContext(ctx)
					mu.Lock()
					calls = append(calls, call{controlID: input.GetControlId(), catalogID: catalogID, ok: ok})
					mu.Unlock()
					return []analyzer.Task{{TaskID: input.GetControlId() + "-task", Payload: &emptypb.Empty{}}}, nil
				},
			})).To(Succeed())

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

			svc = pipeline.New(store, eng, reg, synth, attestor, converter, bus, storage,
				config.PipelineConfig{MaxConcurrentJobs: 10},
				config.AttestationConfig{ExpiryDuration: 168 * time.Hour},
				nil,
				pipeline.WithCatalogControlsReader(catalogReader),
			)

			job := &pipeline.Job{
				JobID:     jobID,
				TenantID:  tenantID,
				Status:    pipeline.JobStatusRunning,
				Config:    []byte(`{"Source":{"CatalogId":"cat-1"}}`),
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
			// (matching production), so the job below must be catalog-backed
			// with a non-empty control list for their GenerateWork to fire —
			// a document-backed job would leave Controls empty and correctly
			// produce zero tasks for them instead.
			reg := analyzer.NewRegistry()
			Expect(analyzer.Register[*pb.Control](reg, &stubControlAnalyzer{name: "classify", genWorkFn: captureControl("classify")})).To(Succeed())
			Expect(analyzer.Register[*pb.Control](reg, &stubControlAnalyzer{name: "embedding", deps: []string{"classify"}, genWorkFn: captureControl("embedding")})).To(Succeed())
			Expect(analyzer.Register[*pb.Control](reg, &stubControlAnalyzer{name: "requires", deps: []string{"embedding"}, genWorkFn: captureControl("requires")})).To(Succeed())
			Expect(analyzer.Register[*pb.Control](reg, &stubControlAnalyzer{name: "relationship", deps: []string{"embedding"}, genWorkFn: captureControl("relationship")})).To(Succeed())

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
			svc = pipeline.New(store, eng, reg, synth, attestor, converter, bus, storage,
				config.PipelineConfig{MaxConcurrentJobs: 10},
				config.AttestationConfig{ExpiryDuration: 168 * time.Hour},
				nil,
				pipeline.WithCatalogControlsReader(catalogReader),
			)

			configBytes, err := json.Marshal(map[string]interface{}{
				"Source": map[string]interface{}{"CatalogId": "cfg-cat-1"},
			})
			Expect(err).NotTo(HaveOccurred())
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

		It("leaves Controls empty and uses a placeholder Input for the candidate-dependent pass", func() {
			type controlCall struct {
				controlID string
			}
			var mu sync.Mutex
			var classifyCalls, requiresCalls, relationshipCalls []controlCall

			reg := analyzer.NewRegistry()
			Expect(analyzer.Register[*pb.Control](reg, &stubControlAnalyzer{
				name: "classify",
				genWorkFn: func(_ context.Context, input *pb.Control, _ analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
					mu.Lock()
					classifyCalls = append(classifyCalls, controlCall{controlID: input.GetControlId()})
					mu.Unlock()
					return []analyzer.Task{{TaskID: "classify-" + input.GetControlId(), Payload: &emptypb.Empty{}}}, nil
				},
			})).To(Succeed())
			Expect(analyzer.Register[*pb.Control](reg, &stubControlAnalyzer{
				name: "requires",
				deps: []string{"classify"},
				genWorkFn: func(_ context.Context, input *pb.Control, _ analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
					mu.Lock()
					requiresCalls = append(requiresCalls, controlCall{controlID: input.GetControlId()})
					mu.Unlock()
					return []analyzer.Task{{TaskID: "requires-task", Payload: &emptypb.Empty{}}}, nil
				},
			})).To(Succeed())
			Expect(analyzer.Register[*pb.Control](reg, &stubControlAnalyzer{
				name: "relationship",
				deps: []string{"classify"},
				genWorkFn: func(_ context.Context, input *pb.Control, _ analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
					mu.Lock()
					relationshipCalls = append(relationshipCalls, controlCall{controlID: input.GetControlId()})
					mu.Unlock()
					return []analyzer.Task{{TaskID: "relationship-task", Payload: &emptypb.Empty{}}}, nil
				},
			})).To(Succeed())

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

			svc = pipeline.New(store, eng, reg, synth, attestor, converter, bus, storage,
				config.PipelineConfig{MaxConcurrentJobs: 10},
				config.AttestationConfig{ExpiryDuration: 168 * time.Hour},
				nil,
				pipeline.WithCatalogControlsReader(catalogReader),
			)

			job := &pipeline.Job{
				JobID:     jobID,
				TenantID:  tenantID,
				Status:    pipeline.JobStatusRunning,
				Config:    []byte(`{"Source":{"CatalogId":"cat-1"}}`),
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
			// to the pre-candidate pass for this catalog-backed job.
			Expect(classifyCalls).To(HaveLen(2))
			// requires/relationship each get exactly one call with a placeholder
			// (empty ControlId) *pb.Control rather than one call per catalog
			// control — proof Controls was left empty for the
			// candidate-dependent pass despite the job being catalog-backed.
			Expect(requiresCalls).To(HaveLen(1))
			Expect(requiresCalls[0].controlID).To(BeEmpty())
			Expect(relationshipCalls).To(HaveLen(1))
			Expect(relationshipCalls[0].controlID).To(BeEmpty())
		})

		It("on a document-backed job (no catalog_id), leaves per-control analyzers at zero tasks instead of erroring", func() {
			var mu sync.Mutex
			var classifyCalls int
			var requiresCalls int
			var requiresControlID string

			// classify is registered as Analyzer[*pb.Control], matching every
			// real production analyzer. Before the fix, a document-backed job
			// (Controls empty) fell back to calling GenerateWorkFromProto with
			// req.Input == &emptypb.Empty{}, which fails registeredWrapper's
			// *pb.Control type assertion and crashes the whole job (see
			// Finding 1). The fix treats a non-nil, empty Controls as
			// "per-control mode, zero controls": classify's GenerateWork is
			// never invoked, and the analyzer completes with a zero-task
			// output instead.
			reg := analyzer.NewRegistry()
			Expect(analyzer.Register[*pb.Control](reg, &stubControlAnalyzer{
				name: "classify",
				genWorkFn: func(_ context.Context, _ *pb.Control, _ analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
					mu.Lock()
					classifyCalls++
					mu.Unlock()
					return []analyzer.Task{{TaskID: "classify-task", Payload: &emptypb.Empty{}}}, nil
				},
			})).To(Succeed())
			Expect(analyzer.Register[*pb.Control](reg, &stubControlAnalyzer{
				name: "requires",
				deps: []string{"classify"},
				genWorkFn: func(_ context.Context, input *pb.Control, _ analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
					mu.Lock()
					requiresCalls++
					requiresControlID = input.GetControlId()
					mu.Unlock()
					return []analyzer.Task{{TaskID: "requires-task", Payload: &emptypb.Empty{}}}, nil
				},
			})).To(Succeed())

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

			svc = pipeline.New(store, eng, reg, synth, attestor, converter, bus, storage,
				config.PipelineConfig{MaxConcurrentJobs: 10},
				config.AttestationConfig{ExpiryDuration: 168 * time.Hour},
				nil,
			)

			job := &pipeline.Job{
				JobID:     jobID,
				TenantID:  tenantID,
				Status:    pipeline.JobStatusRunning,
				Config:    []byte(`{"Source":{"DocumentUri":"file:///doc.pdf"}}`),
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
			// non-nil, empty slice for this document-backed job, so the
			// per-control loop in executeAnalyzer runs zero iterations
			// rather than falling back to the single-Input path (which would
			// type-assert req.Input == &emptypb.Empty{} against *pb.Control
			// and fail).
			Expect(classifyCalls).To(Equal(0))
			// requires (candidate-dependent pass) is unaffected: it always
			// runs with Controls nil and a placeholder *pb.Control Input,
			// regardless of whether the job is catalog-backed.
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
			capSvc := pipeline.New(barrierStore, engine, registry, synth, attestor, converter, bus, storage, cfg, config.AttestationConfig{ExpiryDuration: 168 * time.Hour}, nil)

			for i := 0; i < total; i++ {
				id := fmt.Sprintf("resume-job-%d", i)
				barrierStore.jobs[id] = &pipeline.Job{
					JobID:     id,
					TenantID:  tenantID,
					Status:    pipeline.JobStatusRunning,
					Config:    []byte(`{"test":"config"}`),
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
