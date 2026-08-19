package analysis_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/analysis"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

func TestAnalysisBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Analysis Engine BDD Suite")
}

var restoreLogs func()

var _ = BeforeSuite(func() {
	restoreLogs = testspecs.RedirectLogsToGinkgo()
})

var _ = AfterSuite(func() {
	restoreLogs()
})

// --- Engine test fakes ---

// stubAnalyzer implements analyzer.Analyzer[*emptypb.Empty] for engine tests.
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

// newStub creates a stub analyzer with the given name and dependencies.
func newStub(name string, deps ...string) *stubAnalyzer {
	return &stubAnalyzer{name: name, deps: deps}
}

// newStubWithTasks creates a stub analyzer that generates the specified tasks.
func newStubWithTasks(name string, tasks []analyzer.Task, deps ...string) *stubAnalyzer {
	return &stubAnalyzer{
		name: name,
		deps: deps,
		genWorkFn: func(_ context.Context, _ *emptypb.Empty, _ analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
			return tasks, nil
		},
	}
}

// controlMockAnalyzer implements analyzer.Analyzer[*pb.Control] for
// per-control fan-out tests. It records every control it was called with
// and every catalog ID its Aggregate observed via context.
type controlMockAnalyzer struct {
	name        string
	genWorkFn   func(ctx context.Context, input *pb.Control, cfg analyzer.AnalyzerConfig) ([]analyzer.Task, error)
	aggregateFn func(ctx context.Context, results []analyzer.TaskResult) (*analyzer.Output, error)

	mu           sync.Mutex
	genWorkCalls []*pb.Control
}

func (m *controlMockAnalyzer) Name() string        { return m.name }
func (m *controlMockAnalyzer) DependsOn() []string { return nil }

func (m *controlMockAnalyzer) GenerateWork(ctx context.Context, input *pb.Control, cfg analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
	m.mu.Lock()
	m.genWorkCalls = append(m.genWorkCalls, input)
	m.mu.Unlock()
	if m.genWorkFn != nil {
		return m.genWorkFn(ctx, input, cfg)
	}
	return nil, nil
}

func (m *controlMockAnalyzer) Aggregate(ctx context.Context, results []analyzer.TaskResult) (*analyzer.Output, error) {
	if m.aggregateFn != nil {
		return m.aggregateFn(ctx, results)
	}
	return &analyzer.Output{AnalyzerName: m.name, Metadata: map[string]string{}}, nil
}

func (m *controlMockAnalyzer) ResultSchema() proto.Message { return &pb.Control{} }

func (m *controlMockAnalyzer) calls() []*pb.Control {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*pb.Control, len(m.genWorkCalls))
	copy(out, m.genWorkCalls)
	return out
}

// engineTestDispatcher records Dispatch calls for engine test assertions.
type engineTestDispatcher struct {
	mu          sync.Mutex
	dispatches  []engineDispatchCall
	dispatchErr error
}

type engineDispatchCall struct {
	tasks    []analyzer.Task
	taskType natsbus.TaskType
	jobID    string
}

func (d *engineTestDispatcher) Dispatch(_ context.Context, tasks []analyzer.Task, taskType natsbus.TaskType, jobID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dispatchErr != nil {
		return d.dispatchErr
	}
	d.dispatches = append(d.dispatches, engineDispatchCall{tasks: tasks, taskType: taskType, jobID: jobID})
	return nil
}

func (d *engineTestDispatcher) Redispatch(_ context.Context, _ analyzer.Task, _ natsbus.TaskType, _ string, _ int) error {
	return nil
}

func (d *engineTestDispatcher) dispatchCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.dispatches)
}

// engineTestCollector returns configurable results for engine tests.
type engineTestCollector struct {
	mu        sync.Mutex
	collects  []analysis.CollectRequest
	resultsFn func(req analysis.CollectRequest) ([]analyzer.TaskResult, error)
}

func (c *engineTestCollector) Collect(_ context.Context, req analysis.CollectRequest) ([]analyzer.TaskResult, error) {
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

func (c *engineTestCollector) collectCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.collects)
}

// engineTestReporter is a StageReporter whose ReportStageCompleted can be
// forced to fail, simulating a durable-persistence failure. Started/Failed
// reports always succeed.
type engineTestReporter struct {
	completedErr error
}

func (r *engineTestReporter) ReportStageStarted(context.Context, string, string) error { return nil }

func (r *engineTestReporter) ReportStageCompleted(context.Context, string, string, *analyzer.Output) error {
	return r.completedErr
}

func (r *engineTestReporter) ReportStageFailed(context.Context, string, string, error) error {
	return nil
}

// validCfg returns a valid EngineConfig for tests.
func validCfg() config.EngineConfig {
	return config.EngineConfig{
		TaskTimeout:  5 * time.Minute,
		MaxRetries:   3,
		RetryBackoff: time.Second,
	}
}

// allTaskTypes returns task type mappings for all standard analyzer names.
func allTaskTypes() map[string]natsbus.TaskType {
	return map[string]natsbus.TaskType{
		"classify":     natsbus.TaskClassify,
		"embedding":    natsbus.TaskEmbed,
		"relationship": natsbus.TaskRelate,
		"requires":     natsbus.TaskRequires,
		"artifacts":    natsbus.TaskArtifacts,
	}
}

// makeTasks creates N tasks with sequential IDs for the given analyzer.
func makeTasks(analyzerName string, count int) []analyzer.Task {
	payload, _ := structpb.NewStruct(map[string]interface{}{"source": analyzerName})
	tasks := make([]analyzer.Task, count)
	for i := range count {
		tasks[i] = analyzer.Task{
			TaskID:   fmt.Sprintf("%s-task-%d", analyzerName, i),
			TaskType: analyzerName,
			Payload:  payload,
		}
	}
	return tasks
}

var _ = Describe("Engine", func() {

	var (
		registry   *analyzer.Registry
		dispatcher *engineTestDispatcher
		collector  *engineTestCollector
		ctx        context.Context
	)

	BeforeEach(func() {
		registry = analyzer.NewRegistry()
		dispatcher = &engineTestDispatcher{}
		collector = &engineTestCollector{}
		ctx, _ = tenant.WithTenant(context.Background(), "test-tenant")
	})

	// =================================================================
	// HAPPY PATH
	// =================================================================

	Describe("Happy path", func() {

		It("executes a single analyzer with no dependencies", func() {
			tasks := makeTasks("classify", 2)
			stub := newStubWithTasks("classify", tasks)
			Expect(analyzer.Register[*emptypb.Empty](registry, stub)).To(Succeed())

			engine := analysis.New(registry, dispatcher, collector,
				validCfg(), allTaskTypes())

			result, err := engine.Execute(ctx, analysis.ExecutionRequest{
				JobID: "job-001",
				Input: &emptypb.Empty{},
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result.Completed).To(ConsistOf("classify"))
			Expect(result.Failed).To(BeEmpty())
			Expect(result.Skipped).To(BeEmpty())
			Expect(result.Outputs).To(HaveKey("classify"))
			Expect(dispatcher.dispatchCount()).To(Equal(1))
			Expect(collector.collectCount()).To(Equal(1))
		})

		It("executes a multi-level DAG in level order", func() {
			// A (level 0) → B (level 1) → C (level 2)
			Expect(analyzer.Register[*emptypb.Empty](registry,
				newStubWithTasks("aaa", makeTasks("aaa", 1)))).To(Succeed())
			Expect(analyzer.Register[*emptypb.Empty](registry,
				newStubWithTasks("bbb", makeTasks("bbb", 1), "aaa"))).To(Succeed())
			Expect(analyzer.Register[*emptypb.Empty](registry,
				newStubWithTasks("ccc", makeTasks("ccc", 1), "bbb"))).To(Succeed())

			taskTypes := map[string]natsbus.TaskType{
				"aaa": natsbus.TaskClassify,
				"bbb": natsbus.TaskEmbed,
				"ccc": natsbus.TaskRelate,
			}

			engine := analysis.New(registry, dispatcher, collector,
				validCfg(), taskTypes)

			result, err := engine.Execute(ctx, analysis.ExecutionRequest{
				JobID: "job-dag",
				Input: &emptypb.Empty{},
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result.Completed).To(ConsistOf("aaa", "bbb", "ccc"))
			Expect(result.Failed).To(BeEmpty())
			Expect(result.Skipped).To(BeEmpty())
			Expect(dispatcher.dispatchCount()).To(Equal(3))
		})

		It("executes within-level analyzers in parallel", func() {
			// Two independent analyzers at level 0.
			var callOrder atomic.Int64

			stubA := &stubAnalyzer{
				name: "artifacts",
				genWorkFn: func(_ context.Context, _ *emptypb.Empty, _ analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
					callOrder.Add(1)
					return makeTasks("artifacts", 1), nil
				},
			}
			stubB := &stubAnalyzer{
				name: "classify",
				genWorkFn: func(_ context.Context, _ *emptypb.Empty, _ analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
					callOrder.Add(1)
					return makeTasks("classify", 1), nil
				},
			}

			Expect(analyzer.Register[*emptypb.Empty](registry, stubA)).To(Succeed())
			Expect(analyzer.Register[*emptypb.Empty](registry, stubB)).To(Succeed())

			engine := analysis.New(registry, dispatcher, collector,
				validCfg(), allTaskTypes())

			result, err := engine.Execute(ctx, analysis.ExecutionRequest{
				JobID: "job-parallel",
				Input: &emptypb.Empty{},
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result.Completed).To(ConsistOf("artifacts", "classify"))
			Expect(callOrder.Load()).To(BeNumerically("==", 2))
		})

		It("runs a subset of analyzers when AnalyzerNames is specified", func() {
			Expect(analyzer.Register[*emptypb.Empty](registry,
				newStubWithTasks("classify", makeTasks("classify", 1)))).To(Succeed())
			Expect(analyzer.Register[*emptypb.Empty](registry,
				newStubWithTasks("embedding", makeTasks("embedding", 1), "classify"))).To(Succeed())
			Expect(analyzer.Register[*emptypb.Empty](registry,
				newStubWithTasks("artifacts", makeTasks("artifacts", 1)))).To(Succeed())

			engine := analysis.New(registry, dispatcher, collector,
				validCfg(), allTaskTypes())

			result, err := engine.Execute(ctx, analysis.ExecutionRequest{
				JobID:         "job-subset",
				AnalyzerNames: []string{"embedding"},
				Input:         &emptypb.Empty{},
			})

			Expect(err).NotTo(HaveOccurred())
			// embedding depends on classify, so both should run.
			Expect(result.Completed).To(ConsistOf("classify", "embedding"))
			// artifacts should not appear at all.
			Expect(result.Completed).NotTo(ContainElement("artifacts"))
			Expect(result.Skipped).NotTo(ContainElement("artifacts"))
		})
	})

	// =================================================================
	// ERROR HANDLING
	// =================================================================

	Describe("Error handling", func() {

		It("records partial results when some tasks fail", func() {
			tasks := makeTasks("classify", 3)
			stub := newStubWithTasks("classify", tasks)
			Expect(analyzer.Register[*emptypb.Empty](registry, stub)).To(Succeed())

			// Collector returns one success and two failures.
			collector.resultsFn = func(req analysis.CollectRequest) ([]analyzer.TaskResult, error) {
				return []analyzer.TaskResult{
					{TaskID: req.ExpectedIDs[0], TaskType: "classify"},
					{TaskID: req.ExpectedIDs[1], TaskType: "classify", Error: fmt.Errorf("task_failed")},
					{TaskID: req.ExpectedIDs[2], TaskType: "classify", Error: fmt.Errorf("task_failed")},
				}, nil
			}

			engine := analysis.New(registry, dispatcher, collector,
				validCfg(), allTaskTypes())

			result, err := engine.Execute(ctx, analysis.ExecutionRequest{
				JobID: "job-partial",
				Input: &emptypb.Empty{},
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result.Completed).To(ConsistOf("classify"))
			Expect(result.Failed).To(BeEmpty())
			Expect(result.Outputs["classify"].Metadata["incomplete"]).To(Equal("true"))
			Expect(result.Outputs["classify"].Metadata["failed_tasks"]).To(Equal("2"))
		})

		It("marks analyzer as failed when all tasks fail", func() {
			tasks := makeTasks("classify", 2)
			stub := newStubWithTasks("classify", tasks)
			Expect(analyzer.Register[*emptypb.Empty](registry, stub)).To(Succeed())

			collector.resultsFn = func(req analysis.CollectRequest) ([]analyzer.TaskResult, error) {
				return []analyzer.TaskResult{
					{TaskID: req.ExpectedIDs[0], TaskType: "classify", Error: fmt.Errorf("task_failed")},
					{TaskID: req.ExpectedIDs[1], TaskType: "classify", Error: fmt.Errorf("task_failed")},
				}, nil
			}

			engine := analysis.New(registry, dispatcher, collector,
				validCfg(), allTaskTypes())

			result, err := engine.Execute(ctx, analysis.ExecutionRequest{
				JobID: "job-all-fail",
				Input: &emptypb.Empty{},
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result.Failed).To(ConsistOf("classify"))
			Expect(result.Completed).To(BeEmpty())
			Expect(result.Errors).To(HaveKey("classify"))
			Expect(result.Errors["classify"]).To(MatchError(ContainSubstring("all 2 tasks failed")))
		})

		It("marks analyzer as failed and skips dependents on GenerateWork error", func() {
			failingStub := &stubAnalyzer{
				name: "classify",
				genWorkFn: func(_ context.Context, _ *emptypb.Empty, _ analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
					return nil, fmt.Errorf("model unavailable")
				},
			}
			Expect(analyzer.Register[*emptypb.Empty](registry, failingStub)).To(Succeed())
			Expect(analyzer.Register[*emptypb.Empty](registry,
				newStubWithTasks("embedding", makeTasks("embedding", 1), "classify"))).To(Succeed())

			engine := analysis.New(registry, dispatcher, collector,
				validCfg(), allTaskTypes())

			result, err := engine.Execute(ctx, analysis.ExecutionRequest{
				JobID: "job-genwork-err",
				Input: &emptypb.Empty{},
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result.Failed).To(ConsistOf("classify"))
			Expect(result.Skipped).To(ConsistOf("embedding"))
			Expect(result.Errors["classify"]).To(MatchError(ContainSubstring("GenerateWork failed")))
			Expect(result.Errors["classify"]).To(MatchError(ContainSubstring("model unavailable")))
			Expect(dispatcher.dispatchCount()).To(Equal(0))
		})

		It("marks analyzer as failed on Dispatch error", func() {
			tasks := makeTasks("classify", 1)
			stub := newStubWithTasks("classify", tasks)
			Expect(analyzer.Register[*emptypb.Empty](registry, stub)).To(Succeed())

			dispatcher.dispatchErr = fmt.Errorf("NATS connection lost")

			engine := analysis.New(registry, dispatcher, collector,
				validCfg(), allTaskTypes())

			result, err := engine.Execute(ctx, analysis.ExecutionRequest{
				JobID: "job-dispatch-err",
				Input: &emptypb.Empty{},
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result.Failed).To(ConsistOf("classify"))
			Expect(result.Errors["classify"]).To(MatchError(ContainSubstring("dispatch failed")))
			Expect(result.Errors["classify"]).To(MatchError(ContainSubstring("NATS connection lost")))
		})

		It("marks analyzer as failed and skips dependents on Aggregate error", func() {
			tasks := makeTasks("classify", 1)
			failingStub := &stubAnalyzer{
				name: "classify",
				genWorkFn: func(_ context.Context, _ *emptypb.Empty, _ analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
					return tasks, nil
				},
				aggregateFn: func(_ context.Context, _ []analyzer.TaskResult) (*analyzer.Output, error) {
					return nil, fmt.Errorf("aggregation logic error")
				},
			}
			Expect(analyzer.Register[*emptypb.Empty](registry, failingStub)).To(Succeed())
			Expect(analyzer.Register[*emptypb.Empty](registry,
				newStubWithTasks("embedding", makeTasks("embedding", 1), "classify"))).To(Succeed())

			engine := analysis.New(registry, dispatcher, collector,
				validCfg(), allTaskTypes())

			result, err := engine.Execute(ctx, analysis.ExecutionRequest{
				JobID: "job-aggregate-err",
				Input: &emptypb.Empty{},
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result.Failed).To(ConsistOf("classify"))
			Expect(result.Skipped).To(ConsistOf("embedding"))
			Expect(result.Errors["classify"]).To(MatchError(ContainSubstring("Aggregate failed")))
			Expect(result.Errors["classify"]).To(MatchError(ContainSubstring("aggregation logic error")))
		})

		It("returns ErrUnknownTaskType before any dispatch when mapping is missing", func() {
			stub := newStubWithTasks("classify", makeTasks("classify", 1))
			Expect(analyzer.Register[*emptypb.Empty](registry, stub)).To(Succeed())

			// Empty task types map — no mapping for "classify".
			engine := analysis.New(registry, dispatcher, collector,
				validCfg(), map[string]natsbus.TaskType{})

			result, err := engine.Execute(ctx, analysis.ExecutionRequest{
				JobID: "job-no-tasktype",
				Input: &emptypb.Empty{},
			})

			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, analysis.ErrUnknownTaskType)).To(BeTrue())
			Expect(err.Error()).To(ContainSubstring("classify"))
			Expect(result).To(BeNil())
			Expect(dispatcher.dispatchCount()).To(Equal(0))
		})
	})

	// =================================================================
	// EDGE CASES
	// =================================================================

	Describe("Edge cases", func() {

		It("returns empty result when registry is empty", func() {
			engine := analysis.New(registry, dispatcher, collector,
				validCfg(), allTaskTypes())

			result, err := engine.Execute(ctx, analysis.ExecutionRequest{
				JobID: "job-empty",
				Input: &emptypb.Empty{},
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result.Completed).To(BeEmpty())
			Expect(result.Failed).To(BeEmpty())
			Expect(result.Skipped).To(BeEmpty())
			Expect(dispatcher.dispatchCount()).To(Equal(0))
		})

		It("handles zero tasks from GenerateWork by skipping dispatch", func() {
			// Stub that returns zero tasks (default genWorkFn returns nil).
			stub := newStub("classify")
			Expect(analyzer.Register[*emptypb.Empty](registry, stub)).To(Succeed())

			engine := analysis.New(registry, dispatcher, collector,
				validCfg(), allTaskTypes())

			result, err := engine.Execute(ctx, analysis.ExecutionRequest{
				JobID: "job-zero-tasks",
				Input: &emptypb.Empty{},
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result.Completed).To(ConsistOf("classify"))
			Expect(result.Outputs["classify"].Metadata["tasks"]).To(Equal("0"))
			Expect(dispatcher.dispatchCount()).To(Equal(0))
			Expect(collector.collectCount()).To(Equal(0))
		})

		It("returns partial result and context.Canceled on cancellation", func() {
			// Level 0: classify (completes immediately)
			// Level 1: embedding (blocks until context is cancelled)
			Expect(analyzer.Register[*emptypb.Empty](registry,
				newStubWithTasks("classify", makeTasks("classify", 1)))).To(Succeed())
			Expect(analyzer.Register[*emptypb.Empty](registry,
				newStubWithTasks("embedding", makeTasks("embedding", 1), "classify"))).To(Succeed())
			Expect(analyzer.Register[*emptypb.Empty](registry,
				newStubWithTasks("relationship", makeTasks("relationship", 1), "embedding"))).To(Succeed())

			taskTypes := map[string]natsbus.TaskType{
				"classify":     natsbus.TaskClassify,
				"embedding":    natsbus.TaskEmbed,
				"relationship": natsbus.TaskRelate,
			}

			cancelCtx, cancel := context.WithCancel(ctx)

			// Collector blocks on embedding until context is cancelled.
			collector.resultsFn = func(req analysis.CollectRequest) ([]analyzer.TaskResult, error) {
				if req.TaskType == natsbus.TaskEmbed {
					cancel() // Cancel context when embedding collect starts.
					return nil, context.Canceled
				}
				results := make([]analyzer.TaskResult, len(req.ExpectedIDs))
				for i, id := range req.ExpectedIDs {
					results[i] = analyzer.TaskResult{TaskID: id, TaskType: string(req.TaskType)}
				}
				return results, nil
			}

			engine := analysis.New(registry, dispatcher, collector,
				validCfg(), taskTypes)

			result, err := engine.Execute(cancelCtx, analysis.ExecutionRequest{
				JobID: "job-cancel",
				Input: &emptypb.Empty{},
			})

			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, context.Canceled)).To(BeTrue())
			Expect(result).NotTo(BeNil())
			Expect(result.Completed).To(ContainElement("classify"))
			// relationship should be in Skipped (subsequent level, never started).
			Expect(result.Skipped).To(ContainElement("relationship"))
		})
	})

	// =================================================================
	// NEGATIVE PATH
	// =================================================================

	Describe("Negative path", func() {

		DescribeTable("rejects invalid ExecutionRequests with actionable errors",
			func(makeCtx func() context.Context, req analysis.ExecutionRequest, sentinel error, substr string) {
				engine := analysis.New(registry, dispatcher, collector,
					validCfg(), allTaskTypes())

				result, err := engine.Execute(makeCtx(), req)

				// 1. Error returned.
				Expect(err).To(HaveOccurred())
				// 2. Error is actionable.
				Expect(errors.Is(err, sentinel)).To(BeTrue())
				Expect(err.Error()).To(ContainSubstring(substr))
				// 3. Result state is consistent.
				Expect(result).To(BeNil())
				Expect(dispatcher.dispatchCount()).To(Equal(0))
			},
			Entry("missing tenant context",
				context.Background,
				analysis.ExecutionRequest{JobID: "job-1", Input: &emptypb.Empty{}},
				analysis.ErrNoTenant, "tenant"),
			Entry("empty job ID",
				func() context.Context { return ctx },
				analysis.ExecutionRequest{JobID: "", Input: &emptypb.Empty{}},
				analysis.ErrEmptyJobID, "job ID"),
			Entry("nil input",
				func() context.Context { return ctx },
				analysis.ExecutionRequest{JobID: "job-1", Input: nil},
				analysis.ErrNilInput, "input"),
		)

		It("returns actionable error with analyzer context for GenerateWork failure", func() {
			failing := &stubAnalyzer{
				name: "classify",
				genWorkFn: func(_ context.Context, _ *emptypb.Empty, _ analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
					return nil, fmt.Errorf("model not found")
				},
			}
			Expect(analyzer.Register[*emptypb.Empty](registry, failing)).To(Succeed())

			engine := analysis.New(registry, dispatcher, collector,
				validCfg(), allTaskTypes())

			result, err := engine.Execute(ctx, analysis.ExecutionRequest{
				JobID: "job-genwork-neg",
				Input: &emptypb.Empty{},
			})

			// 1. No top-level error (analyzer failure is recorded in result).
			Expect(err).NotTo(HaveOccurred())

			// 2. Error is actionable and contains context.
			Expect(result.Failed).To(ConsistOf("classify"))
			Expect(result.Errors["classify"]).To(MatchError(ContainSubstring("classify")))
			Expect(result.Errors["classify"]).To(MatchError(ContainSubstring("GenerateWork failed")))
			Expect(result.Errors["classify"]).To(MatchError(ContainSubstring("model not found")))
			Expect(errors.Is(result.Errors["classify"], analysis.ErrAnalyzerFailed)).To(BeTrue())

			// 3. State is consistent.
			Expect(result.Completed).To(BeEmpty())
		})

		It("returns actionable error for all-tasks-failed case", func() {
			tasks := makeTasks("classify", 3)
			stub := newStubWithTasks("classify", tasks)
			Expect(analyzer.Register[*emptypb.Empty](registry, stub)).To(Succeed())

			collector.resultsFn = func(req analysis.CollectRequest) ([]analyzer.TaskResult, error) {
				results := make([]analyzer.TaskResult, len(req.ExpectedIDs))
				for i, id := range req.ExpectedIDs {
					results[i] = analyzer.TaskResult{
						TaskID:   id,
						TaskType: "classify",
						Error:    fmt.Errorf("task_failed"),
					}
				}
				return results, nil
			}

			engine := analysis.New(registry, dispatcher, collector,
				validCfg(), allTaskTypes())

			result, err := engine.Execute(ctx, analysis.ExecutionRequest{
				JobID: "job-all-fail-neg",
				Input: &emptypb.Empty{},
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result.Failed).To(ConsistOf("classify"))
			Expect(result.Errors["classify"]).To(MatchError(ContainSubstring("all 3 tasks failed")))
			Expect(errors.Is(result.Errors["classify"], analysis.ErrAnalyzerFailed)).To(BeTrue())
			Expect(result.Completed).To(BeEmpty())
		})
	})

	// =================================================================
	// DEPENDENCY GATING
	// =================================================================

	Describe("Dependency gating", func() {

		It("skips analyzer when a dependency failed", func() {
			failing := &stubAnalyzer{
				name: "classify",
				genWorkFn: func(_ context.Context, _ *emptypb.Empty, _ analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
					return nil, fmt.Errorf("forced failure")
				},
			}
			Expect(analyzer.Register[*emptypb.Empty](registry, failing)).To(Succeed())
			Expect(analyzer.Register[*emptypb.Empty](registry,
				newStubWithTasks("embedding", makeTasks("embedding", 1), "classify"))).To(Succeed())
			Expect(analyzer.Register[*emptypb.Empty](registry,
				newStubWithTasks("relationship", makeTasks("relationship", 1), "embedding"))).To(Succeed())

			engine := analysis.New(registry, dispatcher, collector,
				validCfg(), allTaskTypes())

			result, err := engine.Execute(ctx, analysis.ExecutionRequest{
				JobID: "job-dep-gate",
				Input: &emptypb.Empty{},
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result.Failed).To(ConsistOf("classify"))
			Expect(result.Skipped).To(ConsistOf("embedding", "relationship"))
			Expect(result.Completed).To(BeEmpty())
		})
	})

	// =================================================================
	// TWO-PASS COMPLETEDOUTPUTS PRE-SEEDING
	// =================================================================

	Describe("Two-pass CompletedOutputs pre-seeding", func() {

		It("does not re-dispatch a pre-seeded analyzer and lets its dependent proceed", func() {
			Expect(analyzer.Register[*emptypb.Empty](registry,
				newStubWithTasks("embedding", makeTasks("embedding", 1)))).To(Succeed())
			Expect(analyzer.Register[*emptypb.Empty](registry,
				newStubWithTasks("relationship", makeTasks("relationship", 1), "embedding"))).To(Succeed())

			engine := analysis.New(registry, dispatcher, collector,
				validCfg(), allTaskTypes())

			preSeeded := &analyzer.Output{AnalyzerName: "embedding", ResultData: []byte(`[]`)}

			result, err := engine.Execute(ctx, analysis.ExecutionRequest{
				JobID:            "job-preseed",
				AnalyzerNames:    []string{"relationship"},
				Input:            &emptypb.Empty{},
				CompletedOutputs: map[string]*analyzer.Output{"embedding": preSeeded},
			})

			Expect(err).NotTo(HaveOccurred())
			// embedding must not be dispatched/collected again -- only
			// relationship should appear in the dispatch log.
			Expect(dispatcher.dispatches).To(HaveLen(1))
			Expect(dispatcher.dispatches[0].taskType).To(Equal(natsbus.TaskRelate))
			// relationship (which depends on embedding) must proceed, not be skipped.
			Expect(result.Completed).To(ContainElement("relationship"))
			Expect(result.Skipped).To(BeEmpty())
			Expect(result.Outputs["embedding"]).To(Equal(preSeeded))
		})
	})

	// =================================================================
	// ANALYZER CONFIG FORWARDING
	// =================================================================

	Describe("Analyzer config forwarding", func() {

		It("passes per-analyzer config to GenerateWorkFromProto", func() {
			var receivedCfg analyzer.AnalyzerConfig

			configStub := &stubAnalyzer{
				name: "classify",
				genWorkFn: func(_ context.Context, _ *emptypb.Empty, cfg analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
					receivedCfg = cfg
					return makeTasks("classify", 1), nil
				},
			}
			Expect(analyzer.Register[*emptypb.Empty](registry, configStub)).To(Succeed())

			engine := analysis.New(registry, dispatcher, collector,
				validCfg(), allTaskTypes())

			_, err := engine.Execute(ctx, analysis.ExecutionRequest{
				JobID: "job-config",
				Input: &emptypb.Empty{},
				AnalyzerConfig: map[string]analyzer.AnalyzerConfig{
					"classify": {Parameters: map[string]string{"model": "qwen3:8b"}},
				},
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(receivedCfg.Parameters["model"]).To(Equal("qwen3:8b"))
		})
	})

	// =================================================================
	// PERSISTENCE-FAILURE PROPAGATION
	// =================================================================
	//
	// ReportStageCompleted returns an error when a non-empty result fails to
	// durably persist. An analyzer must NOT be marked completed in that case,
	// or resume/candidate-generation logic would proceed against data that was
	// never committed. Both executeAnalyzer completion paths (normal success
	// and the zero-tasks early return) must fail the analyzer instead.

	Describe("Persistence-failure propagation", func() {

		It("fails the analyzer when ReportStageCompleted errors on the normal-success path", func() {
			Expect(analyzer.Register[*emptypb.Empty](registry,
				newStubWithTasks("classify", makeTasks("classify", 1)))).To(Succeed())

			reporter := &engineTestReporter{completedErr: errors.New("db persist failed")}
			engine := analysis.New(registry, dispatcher, collector,
				validCfg(), allTaskTypes(), analysis.WithStageReporter(reporter))

			result, err := engine.Execute(ctx, analysis.ExecutionRequest{
				JobID: "job-persist-fail",
				Input: &emptypb.Empty{},
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result.Failed).To(ConsistOf("classify"))
			Expect(result.Completed).NotTo(ContainElement("classify"))
			Expect(result.Outputs).NotTo(HaveKey("classify"))
			Expect(result.Errors).To(HaveKey("classify"))
		})

		It("fails the analyzer when ReportStageCompleted errors on the zero-tasks path", func() {
			// GenerateWork returns no tasks, exercising the zero-tasks early return.
			Expect(analyzer.Register[*emptypb.Empty](registry, newStub("classify"))).To(Succeed())

			reporter := &engineTestReporter{completedErr: errors.New("db persist failed")}
			engine := analysis.New(registry, dispatcher, collector,
				validCfg(), allTaskTypes(), analysis.WithStageReporter(reporter))

			result, err := engine.Execute(ctx, analysis.ExecutionRequest{
				JobID: "job-persist-fail-zero",
				Input: &emptypb.Empty{},
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result.Failed).To(ConsistOf("classify"))
			Expect(result.Completed).NotTo(ContainElement("classify"))
			Expect(result.Outputs).NotTo(HaveKey("classify"))
			Expect(result.Errors).To(HaveKey("classify"))
		})

		It("completes the analyzer normally when ReportStageCompleted succeeds", func() {
			Expect(analyzer.Register[*emptypb.Empty](registry,
				newStubWithTasks("classify", makeTasks("classify", 1)))).To(Succeed())

			reporter := &engineTestReporter{completedErr: nil}
			engine := analysis.New(registry, dispatcher, collector,
				validCfg(), allTaskTypes(), analysis.WithStageReporter(reporter))

			result, err := engine.Execute(ctx, analysis.ExecutionRequest{
				JobID: "job-persist-ok",
				Input: &emptypb.Empty{},
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result.Completed).To(ConsistOf("classify"))
			Expect(result.Failed).To(BeEmpty())
			Expect(result.Outputs).To(HaveKey("classify"))
		})
	})

	// =================================================================
	// PER-CONTROL FAN-OUT
	// =================================================================
	//
	// Every real analyzer is registered as Analyzer[*pb.Control]. When
	// ExecutionRequest.Controls is set, executeAnalyzer must call
	// GenerateWork once per control and concatenate the resulting task
	// slices into a single Dispatch/Collect/Aggregate cycle.

	Describe("Per-control fan-out", func() {

		It("calls GenerateWork once per control and concatenates tasks into a single dispatch/collect", func() {
			mock := &controlMockAnalyzer{
				name: "classify",
				genWorkFn: func(_ context.Context, input *pb.Control, _ analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
					payload, _ := structpb.NewStruct(map[string]interface{}{"control": input.GetControlId()})
					return []analyzer.Task{{
						TaskID:   "mock-" + input.GetControlId(),
						TaskType: "classify",
						Payload:  payload,
					}}, nil
				},
			}
			Expect(analyzer.Register[*pb.Control](registry, mock)).To(Succeed())

			engine := analysis.New(registry, dispatcher, collector,
				validCfg(), allTaskTypes())

			result, err := engine.Execute(ctx, analysis.ExecutionRequest{
				JobID: "job-fanout",
				Controls: []*pb.Control{
					{ControlId: "AC-1"},
					{ControlId: "AC-2"},
				},
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result.Completed).To(ConsistOf("classify"))
			Expect(result.Failed).To(BeEmpty())

			calls := mock.calls()
			Expect(calls).To(HaveLen(2))
			Expect([]string{calls[0].GetControlId(), calls[1].GetControlId()}).To(ConsistOf("AC-1", "AC-2"))

			// Dispatch and Collect are each called exactly once, over the
			// combined two-task set -- not once per control.
			Expect(dispatcher.dispatchCount()).To(Equal(1))
			Expect(dispatcher.dispatches[0].tasks).To(HaveLen(2))
			Expect(collector.collectCount()).To(Equal(1))
		})

		It("fails the analyzer without dispatching when a per-control GenerateWork call errors", func() {
			mock := &controlMockAnalyzer{
				name: "classify",
				genWorkFn: func(_ context.Context, input *pb.Control, _ analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
					if input.GetControlId() == "AC-2" {
						return nil, fmt.Errorf("model unavailable for AC-2")
					}
					return []analyzer.Task{{TaskID: "mock-" + input.GetControlId(), TaskType: "classify"}}, nil
				},
			}
			Expect(analyzer.Register[*pb.Control](registry, mock)).To(Succeed())

			engine := analysis.New(registry, dispatcher, collector,
				validCfg(), allTaskTypes())

			result, err := engine.Execute(ctx, analysis.ExecutionRequest{
				JobID: "job-fanout-err",
				Controls: []*pb.Control{
					{ControlId: "AC-1"},
					{ControlId: "AC-2"},
				},
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result.Failed).To(ConsistOf("classify"))
			Expect(result.Errors["classify"]).To(MatchError(ContainSubstring("model unavailable for AC-2")))
			Expect(dispatcher.dispatchCount()).To(Equal(0))
		})

		It("makes CatalogID available to Aggregate via context", func() {
			var observedCatalogID string
			var observedOK bool

			mock := &controlMockAnalyzer{
				name: "classify",
				genWorkFn: func(_ context.Context, input *pb.Control, _ analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
					return []analyzer.Task{{TaskID: "mock-" + input.GetControlId(), TaskType: "classify"}}, nil
				},
				aggregateFn: func(ctx context.Context, results []analyzer.TaskResult) (*analyzer.Output, error) {
					observedCatalogID, observedOK = analyzer.CatalogIDFromContext(ctx)
					return &analyzer.Output{AnalyzerName: "classify", Metadata: map[string]string{}}, nil
				},
			}
			Expect(analyzer.Register[*pb.Control](registry, mock)).To(Succeed())

			engine := analysis.New(registry, dispatcher, collector,
				validCfg(), allTaskTypes())

			result, err := engine.Execute(ctx, analysis.ExecutionRequest{
				JobID:     "job-fanout-catalog",
				Controls:  []*pb.Control{{ControlId: "AC-1"}},
				CatalogID: "cat-42",
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result.Completed).To(ConsistOf("classify"))
			Expect(observedOK).To(BeTrue())
			Expect(observedCatalogID).To(Equal("cat-42"))
		})
	})

	// =================================================================
	// PRE-BUILT RESULTS (SECTION AUTO-SKIP)
	// =================================================================
	//
	// A task with PreBuiltResult=true already carries its final result in
	// Payload (e.g. section auto-skip, which never runs on a worker). The
	// engine must not dispatch it over NATS; it must convert Payload into a
	// TaskResult and feed it to Aggregate alongside collected results.

	Describe("Pre-built results", func() {

		It("does not dispatch pre-built tasks and merges them into Aggregate's input", func() {
			normalPayload, _ := structpb.NewStruct(map[string]interface{}{"work": "yes"})
			preBuilt := &pb.AnalysisResult{
				ResultId:   "pre-built",
				Attributes: map[string]string{"k": "v"},
			}

			var aggregated []analyzer.TaskResult
			stub := &stubAnalyzer{
				name: "classify",
				genWorkFn: func(_ context.Context, _ *emptypb.Empty, _ analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
					return []analyzer.Task{
						{TaskID: "normal-1", TaskType: "classify", Payload: normalPayload},
						{TaskID: "skip-1", TaskType: "classify", Payload: preBuilt, PreBuiltResult: true},
					}, nil
				},
				aggregateFn: func(_ context.Context, results []analyzer.TaskResult) (*analyzer.Output, error) {
					aggregated = results
					return &analyzer.Output{AnalyzerName: "classify", Metadata: map[string]string{}}, nil
				},
			}
			Expect(analyzer.Register[*emptypb.Empty](registry, stub)).To(Succeed())

			engine := analysis.New(registry, dispatcher, collector,
				validCfg(), allTaskTypes())

			result, err := engine.Execute(ctx, analysis.ExecutionRequest{
				JobID: "job-prebuilt-mixed",
				Input: &emptypb.Empty{},
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result.Completed).To(ConsistOf("classify"))

			// Only the normal task is dispatched/collected.
			Expect(dispatcher.dispatchCount()).To(Equal(1))
			Expect(dispatcher.dispatches[0].tasks).To(HaveLen(1))
			Expect(dispatcher.dispatches[0].tasks[0].TaskID).To(Equal("normal-1"))
			Expect(collector.collectCount()).To(Equal(1))
			Expect(collector.collects[0].ExpectedIDs).To(ConsistOf("normal-1"))

			// Aggregate sees both results: the collected one and the pre-built one.
			Expect(aggregated).To(HaveLen(2))
			var preBuiltResult *analyzer.TaskResult
			for i := range aggregated {
				if aggregated[i].TaskID == "skip-1" {
					preBuiltResult = &aggregated[i]
				}
			}
			Expect(preBuiltResult).NotTo(BeNil())
			Expect(preBuiltResult.TaskType).To(Equal("classify"))
			Expect(preBuiltResult.Error).NotTo(HaveOccurred())
			gotResult, ok := preBuiltResult.Result.(*pb.AnalysisResult)
			Expect(ok).To(BeTrue())
			Expect(gotResult.GetResultId()).To(Equal("pre-built"))
			Expect(gotResult.GetAttributes()).To(HaveKeyWithValue("k", "v"))
		})

		It("never dispatches or collects when all tasks are pre-built", func() {
			preBuilt := &pb.AnalysisResult{ResultId: "only-prebuilt"}

			var aggregated []analyzer.TaskResult
			stub := &stubAnalyzer{
				name: "classify",
				genWorkFn: func(_ context.Context, _ *emptypb.Empty, _ analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
					return []analyzer.Task{
						{TaskID: "skip-only", TaskType: "classify", Payload: preBuilt, PreBuiltResult: true},
					}, nil
				},
				aggregateFn: func(_ context.Context, results []analyzer.TaskResult) (*analyzer.Output, error) {
					aggregated = results
					return &analyzer.Output{AnalyzerName: "classify", Metadata: map[string]string{}}, nil
				},
			}
			Expect(analyzer.Register[*emptypb.Empty](registry, stub)).To(Succeed())

			engine := analysis.New(registry, dispatcher, collector,
				validCfg(), allTaskTypes())

			result, err := engine.Execute(ctx, analysis.ExecutionRequest{
				JobID: "job-prebuilt-only",
				Input: &emptypb.Empty{},
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result.Completed).To(ConsistOf("classify"))
			Expect(dispatcher.dispatchCount()).To(Equal(0))
			Expect(collector.collectCount()).To(Equal(0))

			Expect(aggregated).To(HaveLen(1))
			Expect(aggregated[0].TaskID).To(Equal("skip-only"))
			Expect(aggregated[0].Error).NotTo(HaveOccurred())
			gotResult, ok := aggregated[0].Result.(*pb.AnalysisResult)
			Expect(ok).To(BeTrue())
			Expect(gotResult.GetResultId()).To(Equal("only-prebuilt"))
		})
	})
})
