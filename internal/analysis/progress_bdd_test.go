package analysis_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/analysis"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/resultcache"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// progressCall is one AddStageProgress call a recordingProgressStore accepted.
type progressCall struct {
	tenantID, jobID, stage string
	delta                  analysis.ProgressDelta
}

// recordingProgressStore is an in-memory analysis.ProgressStore. It rejects
// a ctx without a tenant, as PGStore does, and fails every call with err
// while err is set; neither is recorded. A local fake: ProgressStore is
// internal to this package.
type recordingProgressStore struct {
	mu    sync.Mutex
	calls []progressCall
	err   error
}

func (s *recordingProgressStore) AddStageProgress(ctx context.Context, jobID, stageName string, d analysis.ProgressDelta) error {
	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.calls = append(s.calls, progressCall{tenantID: tenantID, jobID: jobID, stage: stageName, delta: d})
	return nil
}

func (s *recordingProgressStore) failWith(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *recordingProgressStore) recorded() []progressCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]progressCall(nil), s.calls...)
}

// sum folds the recorded deltas of stage the way the store does: Total is
// the last one's, every count is added, LastSuccessAt is the latest.
func (s *recordingProgressStore) sum(stage string) analysis.ProgressDelta {
	var out analysis.ProgressDelta
	for _, c := range s.recorded() {
		if c.stage != stage {
			continue
		}
		out.Total = c.delta.Total
		out.Succeeded += c.delta.Succeeded
		out.Failed += c.delta.Failed
		out.Retried += c.delta.Retried
		out.CacheHits += c.delta.CacheHits
		if c.delta.LastSuccessAt != nil && (out.LastSuccessAt == nil || c.delta.LastSuccessAt.After(*out.LastSuccessAt)) {
			t := *c.delta.LastSuccessAt
			out.LastSuccessAt = &t
		}
	}
	return out
}

// flushOutcomes sums analysis.progress.flushes.total's points whose outcome
// attribute is outcome.
func flushOutcomes(tp *telemetrytest.TestProvider, outcome string) int64 {
	m := telemetrytest.FindMetric(tp.GetMetrics(), "analysis.progress.flushes.total")
	if m == nil {
		return 0
	}
	sum, ok := m.Data.(metricdata.Sum[int64])
	Expect(ok).To(BeTrue(), "analysis.progress.flushes.total is an int64 counter")
	var n int64
	for _, dp := range sum.DataPoints {
		if v, _ := dp.Attributes.Value("outcome"); v.AsString() == outcome {
			n += dp.Value
		}
	}
	return n
}

// progressRig is one stage tracker over a recording store, with its logs
// and metrics captured.
type progressRig struct {
	store     *recordingProgressStore
	logs      *gbytes.Buffer
	telemetry *telemetrytest.TestProvider
	ctx       context.Context
}

func newProgressRig() *progressRig {
	r := &progressRig{store: &recordingProgressStore{}, logs: gbytes.NewBuffer()}
	var err error
	r.telemetry, err = telemetrytest.NewTestProvider()
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() { Expect(r.telemetry.Shutdown(context.Background())).To(Succeed()) })
	r.ctx, err = tenant.WithTenant(context.Background(), "progress-tenant")
	Expect(err).NotTo(HaveOccurred())
	return r
}

func (r *progressRig) tracker(interval time.Duration) *analysis.ExportStageProgress {
	return analysis.ExportNewStageProgress(r.store, "job-p", "classify", interval,
		slog.New(slog.NewTextHandler(r.logs, nil)), r.telemetry.MeterProvider())
}

var _ = Describe("Stage progress", func() {
	var rig *progressRig

	BeforeEach(func() {
		rig = newProgressRig()
	})

	It("flushes observed task events on the ticker while the stage runs", func() {
		p := rig.tracker(5 * time.Millisecond)
		p.Start(rig.ctx, 5, 0, 0)
		DeferCleanup(func() { _ = p.Finish(rig.ctx) })
		p.TaskSucceeded()
		p.TaskSucceeded()
		p.TaskFailed()
		p.TaskRetried()

		Eventually(func() analysis.ProgressDelta {
			d := rig.store.sum("classify")
			d.LastSuccessAt = nil
			return d
		}).Should(Equal(analysis.ProgressDelta{Total: 5, Succeeded: 2, Failed: 1, Retried: 1}))
		calls := rig.store.recorded()
		Expect(calls[0].tenantID).To(Equal("progress-tenant"))
		Expect(calls[0].jobID).To(Equal("job-p"))
	})

	It("writes the remaining events once at Finish, and nothing on a second Finish", func() {
		p := rig.tracker(time.Hour)
		p.Start(rig.ctx, 3, 0, 0)
		p.TaskSucceeded()
		p.TaskFailed()
		Expect(rig.store.recorded()).To(BeEmpty())

		Expect(p.Finish(rig.ctx)).To(Succeed())
		Expect(p.Finish(rig.ctx)).To(Succeed())

		calls := rig.store.recorded()
		Expect(calls).To(HaveLen(1))
		Expect(calls[0].delta.Total).To(Equal(3))
		Expect(calls[0].delta.Succeeded).To(Equal(1))
		Expect(calls[0].delta.Failed).To(Equal(1))
	})

	It("clears what it flushed, so no event is counted twice", func() {
		p := rig.tracker(5 * time.Millisecond)
		p.Start(rig.ctx, 4, 0, 0)
		p.TaskSucceeded()
		Eventually(rig.store.recorded).ShouldNot(BeEmpty())
		p.TaskSucceeded()
		p.TaskFailed()
		Expect(p.Finish(rig.ctx)).To(Succeed())

		sum := rig.store.sum("classify")
		Expect(sum.Succeeded).To(Equal(2))
		Expect(sum.Failed).To(Equal(1))
		for _, c := range rig.store.recorded() {
			Expect(c.delta.Total).To(Equal(4), "every delta carries the stage's total")
		}
	})

	It("writes nothing on a tick with no new events", func() {
		p := rig.tracker(5 * time.Millisecond)
		p.Start(rig.ctx, 2, 0, 0)
		DeferCleanup(func() { _ = p.Finish(rig.ctx) })
		Eventually(rig.store.recorded).Should(HaveLen(1))
		Consistently(rig.store.recorded, 50*time.Millisecond, 5*time.Millisecond).Should(HaveLen(1))
	})

	It("records the time of the latest success", func() {
		p := rig.tracker(time.Hour)
		p.Start(rig.ctx, 2, 0, 0)
		before := time.Now()
		p.TaskSucceeded()
		after := time.Now()
		Expect(p.Finish(rig.ctx)).To(Succeed())

		last := rig.store.sum("classify").LastSuccessAt
		Expect(last).NotTo(BeNil())
		Expect(*last).To(BeTemporally(">=", before))
		Expect(*last).To(BeTemporally("<=", after))
	})

	DescribeTable("counts pre-built results as succeeded without moving the last success",
		func(preBuilt, cacheHits int) {
			p := rig.tracker(time.Hour)
			p.Start(rig.ctx, 3, preBuilt, cacheHits)
			Expect(p.Finish(rig.ctx)).To(Succeed())

			Expect(rig.store.sum("classify")).To(Equal(analysis.ProgressDelta{
				Total: 3, Succeeded: preBuilt, CacheHits: cacheHits}))
		},
		Entry("cache hits", 2, 2),
		Entry("pre-built results that are not cache hits", 1, 0),
		Entry("a mix of both", 3, 1),
	)

	Describe("a failing store", func() {
		storeErr := errors.New("database unavailable")

		It("logs and counts a failed tick, keeps its events, and returns no error until Finish", func() {
			rig.store.failWith(storeErr)
			p := rig.tracker(5 * time.Millisecond)
			p.Start(rig.ctx, 2, 0, 0)
			p.TaskSucceeded()

			Eventually(func() int64 { return flushOutcomes(rig.telemetry, "failed") }).Should(BeNumerically(">=", 1))
			Eventually(rig.logs).Should(gbytes.Say("stage progress"))
			Expect(rig.store.recorded()).To(BeEmpty())

			rig.store.failWith(nil)
			p.TaskFailed()
			Expect(p.Finish(rig.ctx)).To(Succeed())
			sum := rig.store.sum("classify")
			Expect(sum.Succeeded).To(Equal(1), "the event of the failed flush is written by a later one")
			Expect(sum.Failed).To(Equal(1))
			Expect(flushOutcomes(rig.telemetry, "ok")).To(BeNumerically(">=", 1))
		})

		It("returns the final flush's error from Finish, naming the stage and job", func() {
			p := rig.tracker(time.Hour)
			p.Start(rig.ctx, 1, 0, 0)
			p.TaskSucceeded()
			rig.store.failWith(storeErr)

			err := p.Finish(rig.ctx)
			Expect(err).To(MatchError(storeErr))
			Expect(err.Error()).To(ContainSubstring(`"classify"`))
			Expect(err.Error()).To(ContainSubstring(`"job-p"`))
			Expect(rig.store.recorded()).To(BeEmpty())
		})
	})

	It("writes the final counts even when the stage's context was cancelled", func() {
		ctx, cancel := context.WithCancel(rig.ctx)
		p := rig.tracker(time.Hour)
		p.Start(ctx, 1, 0, 0)
		p.TaskFailed()
		cancel()

		Expect(p.Finish(ctx)).To(Succeed())
		Expect(rig.store.sum("classify").Failed).To(Equal(1))
	})
})

// progressStub returns a stub analyzer generating tasks and aggregating an
// output with empty metadata.
func progressStub(name string, tasks []analyzer.Task) *analyzer.Registry {
	stub := newStubWithTasks(name, tasks)
	stub.AggregateFunc = func(context.Context, []analyzer.TaskResult) (*analyzer.Output, error) {
		return &analyzer.Output{AnalyzerName: name, Metadata: map[string]string{}}, nil
	}
	registry := analyzer.NewRegistry()
	Expect(analyzer.Register[*emptypb.Empty](registry, stub)).To(Succeed())
	return registry
}

var _ = Describe("Engine stage progress", func() {
	var (
		rig       *progressRig
		collector *engineTestCollector
	)

	BeforeEach(func() {
		rig = newProgressRig()
		// Reports two successes, a retry and one failure through the
		// collection's observer, as NATSCollector does.
		collector = &engineTestCollector{resultsFn: func(req analysis.CollectRequest) ([]analyzer.TaskResult, error) {
			out := make([]analyzer.TaskResult, len(req.ExpectedIDs))
			for i, id := range req.ExpectedIDs {
				out[i] = analyzer.TaskResult{TaskID: id, TaskType: string(req.TaskType)}
				if req.Progress == nil {
					continue
				}
				if i == len(req.ExpectedIDs)-1 {
					req.Progress.TaskRetried()
					req.Progress.TaskFailed()
					out[i].Error = errors.New("task failed")
					continue
				}
				req.Progress.TaskSucceeded()
			}
			return out, nil
		}}
	})

	execute := func(engine *analysis.Engine) *analysis.ExecutionResult {
		res, err := engine.Execute(rig.ctx, analysis.ExecutionRequest{JobID: "job-e", Input: &emptypb.Empty{}})
		Expect(err).NotTo(HaveOccurred())
		return res
	}

	It("writes each stage's total and task outcomes under the job and stage", func() {
		engine := analysis.New(progressStub("classify", makeTasks("classify", 3)), &engineTestDispatcher{}, collector,
			validCfg(), allTaskTypes(), analysis.WithProgressStore(rig.store, time.Hour))

		res := execute(engine)
		Expect(res.Completed).To(ConsistOf("classify"))
		sum := rig.store.sum("classify")
		Expect(sum.LastSuccessAt).NotTo(BeNil())
		sum.LastSuccessAt = nil
		Expect(sum).To(Equal(analysis.ProgressDelta{Total: 3, Succeeded: 2, Failed: 1, Retried: 1}))
		Expect(rig.store.recorded()[0].jobID).To(Equal("job-e"))
		Expect(res.Outputs["classify"].Metadata).NotTo(HaveKey("progress_stale"))
	})

	It("counts pre-built results in the total and as succeeded", func() {
		payload, err := structpb.NewStruct(map[string]any{"work": "yes"})
		Expect(err).NotTo(HaveOccurred())
		tasks := []analyzer.Task{
			{TaskID: "normal-1", TaskType: "classify", Payload: payload},
			{TaskID: "normal-2", TaskType: "classify", Payload: payload},
			{TaskID: "skip-1", TaskType: "classify", Payload: &pb.AnalysisResult{ResultId: "pre-built"}, PreBuiltResult: true},
		}
		engine := analysis.New(progressStub("classify", tasks), &engineTestDispatcher{}, collector,
			validCfg(), allTaskTypes(), analysis.WithProgressStore(rig.store, time.Hour))

		execute(engine)
		sum := rig.store.sum("classify")
		Expect(sum.Total).To(Equal(3))
		Expect(sum.Succeeded).To(Equal(2), "one dispatched success and the pre-built result")
		Expect(sum.Failed).To(Equal(1))
		Expect(sum.CacheHits).To(BeZero())
	})

	It("writes no progress row for a stage that dispatched nothing", func() {
		engine := analysis.New(progressStub("classify", nil), &engineTestDispatcher{}, collector,
			validCfg(), allTaskTypes(), analysis.WithProgressStore(rig.store, time.Hour))

		res := execute(engine)
		Expect(res.Completed).To(ConsistOf("classify"))
		Expect(rig.store.recorded()).To(BeEmpty(), "a stage with no tasks has no progress, not a 0/0 row")
	})

	It("passes no observer to the collector, and writes nothing, without a progress store", func() {
		engine := analysis.New(progressStub("classify", makeTasks("classify", 2)), &engineTestDispatcher{}, collector,
			validCfg(), allTaskTypes())

		res := execute(engine)
		Expect(res.Completed).To(ConsistOf("classify"))
		Expect(collector.collects).To(HaveLen(1))
		Expect(collector.collects[0].Progress).To(BeNil())
	})

	It("falls back to the default flush interval when given none", func() {
		engine := analysis.New(progressStub("classify", makeTasks("classify", 2)), &engineTestDispatcher{}, collector,
			validCfg(), allTaskTypes(), analysis.WithProgressStore(rig.store, 0))

		res := execute(engine)
		Expect(res.Completed).To(ConsistOf("classify"))
		Expect(rig.store.sum("classify").Total).To(Equal(2))
	})

	It("completes the stage and marks its output progress_stale when the final flush fails", func() {
		rig.store.failWith(errors.New("database unavailable"))
		engine := analysis.New(progressStub("classify", makeTasks("classify", 2)), &engineTestDispatcher{}, collector,
			validCfg(), allTaskTypes(), analysis.WithProgressStore(rig.store, time.Hour),
			analysis.WithLogger(slog.New(slog.NewTextHandler(rig.logs, nil))))

		res := execute(engine)
		Expect(res.Completed).To(ConsistOf("classify"))
		Expect(res.Outputs["classify"].Metadata).To(HaveKeyWithValue("progress_stale", "true"))
		Expect(rig.logs).To(gbytes.Say("stage progress"))
		Expect(rig.store.recorded()).To(BeEmpty())
	})

	It("keeps a failed stage's own error when its final flush fails too, and logs the flush failure", func() {
		rig.store.failWith(errors.New("database unavailable"))
		collector.resultsFn = func(analysis.CollectRequest) ([]analyzer.TaskResult, error) {
			return nil, errors.New("collect broke")
		}
		engine := analysis.New(progressStub("classify", makeTasks("classify", 2)), &engineTestDispatcher{}, collector,
			validCfg(), allTaskTypes(), analysis.WithProgressStore(rig.store, time.Hour),
			analysis.WithLogger(slog.New(slog.NewTextHandler(rig.logs, nil))))

		res := execute(engine)
		Expect(res.Failed).To(ConsistOf("classify"))
		Expect(res.Errors["classify"]).To(MatchError(ContainSubstring("collect broke")))
		Expect(res.Errors["classify"].Error()).NotTo(ContainSubstring("database unavailable"))
		Expect(rig.logs).To(gbytes.Say("stage progress"))
	})

	DescribeTable("counts the stage's progress writes by outcome, whichever option comes first",
		func(telemetryFirst bool, storeErr error, outcome string) {
			rig.store.failWith(storeErr)
			opts := []analysis.Option{analysis.WithProgressStore(rig.store, time.Hour)}
			withTelemetry := analysis.WithTelemetry(nil, rig.telemetry.MeterProvider())
			if telemetryFirst {
				opts = append([]analysis.Option{withTelemetry}, opts...)
			} else {
				opts = append(opts, withTelemetry)
			}
			engine := analysis.New(progressStub("classify", makeTasks("classify", 2)), &engineTestDispatcher{}, collector,
				validCfg(), allTaskTypes(), opts...)

			execute(engine)
			Expect(flushOutcomes(rig.telemetry, outcome)).To(BeEquivalentTo(1))
			m := telemetrytest.FindMetric(rig.telemetry.GetMetrics(), "analysis.progress.flush.duration_ms")
			Expect(m).NotTo(BeNil())
			n, err := telemetrytest.Float64HistogramCount(m)
			Expect(err).NotTo(HaveOccurred())
			Expect(n).To(BeEquivalentTo(1))
		},
		Entry("a write that succeeds, telemetry first", true, nil, "ok"),
		Entry("a write that succeeds, telemetry last", false, nil, "ok"),
		Entry("a write that fails, telemetry first", true, errors.New("database unavailable"), "failed"),
		Entry("a write that fails, telemetry last", false, errors.New("database unavailable"), "failed"),
	)

	It("counts results served from the result cache as cache hits that do not move the last success", func() {
		cr := newCacheRig("a", "b", "c")
		cr.produce("job-1")
		cr.engine = analysis.New(analyzerRegistry(cr), cr.dispatcher, cr.collector, validCfg(), allTaskTypes(),
			append(cr.options(), analysis.WithProgressStore(rig.store, time.Hour))...)

		res := cr.run("job-2", cr.settings(resultcache.ModeEnabled))
		Expect(res.Completed).To(ConsistOf(cacheAnalyzer))
		Expect(rig.store.sum(cacheAnalyzer)).To(Equal(analysis.ProgressDelta{Total: 3, Succeeded: 3, CacheHits: 3}))
	})
})
