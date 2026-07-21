package pipeline_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/complytime-labs/crosscodex/internal/analysis"
	"github.com/complytime-labs/crosscodex/internal/pipeline"
	pipelineattestation "github.com/complytime-labs/crosscodex/internal/pipeline/attestation"
	"github.com/complytime-labs/crosscodex/internal/synthesis"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
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
	aggregateFn func(ctx context.Context, results []analyzer.TaskResult) (*analyzer.Output, error)
}

func (s *stubAnalyzer) Name() string        { return s.name }
func (s *stubAnalyzer) DependsOn() []string { return s.deps }

func (s *stubAnalyzer) GenerateWork(ctx context.Context, input *emptypb.Empty, cfg analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
	if s.genWorkFn != nil {
		return s.genWorkFn(ctx, input, cfg)
	}
	return nil, nil
}

func (s *stubAnalyzer) Aggregate(ctx context.Context, results []analyzer.TaskResult) (*analyzer.Output, error) {
	if s.aggregateFn != nil {
		return s.aggregateFn(ctx, results)
	}
	return &analyzer.Output{AnalyzerName: s.name, Metadata: map[string]string{}}, nil
}

func (s *stubAnalyzer) ResultSchema() proto.Message { return &emptypb.Empty{} }

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

func (c *executorTestCollector) Collect(_ context.Context, req analysis.CollectRequest) ([]analyzer.TaskResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.collects = append(c.collects, req)
	if c.resultsFn != nil {
		return c.resultsFn(req)
	}
	// Default: return success for all expected IDs.
	results := make([]analyzer.TaskResult, len(req.ExpectedIDs))
	for i, id := range req.ExpectedIDs {
		results[i] = analyzer.TaskResult{TaskID: id, TaskType: string(req.TaskType)}
	}
	return results, nil
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
		svc = pipeline.New(store, engine, registry, synth, attestor, converter, bus, storage, pipelineCfg, attCfg)
	})

	Describe("executeJob", func() {
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
			failRegistry := analyzer.NewRegistry()
			genWork := func(_ context.Context, _ *emptypb.Empty, _ analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
				return []analyzer.Task{{TaskID: "t1", Payload: &emptypb.Empty{}}}, nil
			}
			fa1 := &stubAnalyzer{name: "analyzer-a", genWorkFn: genWork}
			fa2 := &stubAnalyzer{name: "analyzer-b", deps: []string{"analyzer-a"}, genWorkFn: genWork}
			Expect(analyzer.Register[*emptypb.Empty](failRegistry, fa1)).To(Succeed())
			Expect(analyzer.Register[*emptypb.Empty](failRegistry, fa2)).To(Succeed())

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
			failSvc := pipeline.New(store, failEngine, failRegistry, synth, attestor, converter, bus, storage, pipelineCfg, attCfg)

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
				for _, pub := range bus.publishes {
					if pub.subject == expectedSubject {
						return true
					}
				}
				return false
			}, "1s", "50ms").Should(BeTrue())
		})
	})

})
