package analysis_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/complytime-labs/crosscodex/internal/analysis"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/natsbus/natsbustest"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// noSuccessCount sums analysis.collections.no_success data points whose
// task_type attribute is taskType.
func noSuccessCount(tp *telemetrytest.TestProvider, taskType string) int64 {
	m := telemetrytest.FindMetric(tp.GetMetrics(), "analysis.collections.no_success")
	if m == nil {
		return 0
	}
	sum, ok := m.Data.(metricdata.Sum[int64])
	ExpectWithOffset(1, ok).To(BeTrue(), "analysis.collections.no_success is %T, not an Int64Counter", m.Data)
	var total int64
	for _, dp := range sum.DataPoints {
		if v, ok := dp.Attributes.Value(attribute.Key("task_type")); ok && v.AsString() == taskType {
			total += dp.Value
		}
	}
	return total
}

// starvedOutcome is what AwaitResults returned in a starvation spec.
type starvedOutcome struct {
	results []analyzer.TaskResult
	err     error
}

var _ = Describe("The no-success window (analysis.engine.success_timeout)", func() {
	var (
		bus       *natsbustest.Fake
		disp      *fakeDispatcher
		tp        *telemetrytest.TestProvider
		collector *analysis.NATSCollector
		ctx       context.Context
	)

	BeforeEach(func() {
		bus = &natsbustest.Fake{}
		disp = &fakeDispatcher{}
		var err error
		tp, err = telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(tp.Shutdown(context.Background())).To(Succeed()) })
		collector = analysis.NewNATSCollector(bus, analysis.WithCollectorTelemetry(tp.TracerProvider(), tp.MeterProvider()))
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(func() { cancel() })
		ctx, err = tenant.WithTenant(ctx, "test-tenant")
		Expect(err).NotTo(HaveOccurred())
	})

	// request is a relate collection of n tasks t-0 … t-(n-1).
	request := func(n int, idle, window time.Duration, maxRetries int, backoff time.Duration) analysis.CollectRequest {
		req := analysis.CollectRequest{
			TaskType: natsbus.TaskRelate, JobID: "job-dark", Timeout: idle, StageTimeout: time.Hour,
			SuccessTimeout: window, MaxRetries: maxRetries, Backoff: backoff, Dispatcher: disp,
		}
		for i := range n {
			id := fmt.Sprintf("t-%d", i)
			req.ExpectedIDs = append(req.ExpectedIDs, id)
			req.Tasks = append(req.Tasks, analyzer.Task{TaskID: id, TaskType: "relate", Payload: &structpb.Struct{}})
		}
		return req
	}

	// feed runs step(i) for i = 0, 1, … every 20ms, each followed by a
	// worker progress tick, until the spec ends or n steps ran.
	feed := func(n int, step func(i int)) {
		stop := make(chan struct{})
		stopped := make(chan struct{})
		DeferCleanup(func() { close(stop); <-stopped })
		go func() {
			defer GinkgoRecover()
			defer close(stopped)
			for i := 0; i < n; i++ {
				select {
				case <-stop:
					return
				case <-time.After(20 * time.Millisecond):
				}
				step(i)
				deliverTick(bus)
			}
		}()
	}

	success, err := structpb.NewStruct(map[string]interface{}{"result": "ok"})
	if err != nil {
		panic(err)
	}

	DescribeTable("fails the collection with ErrNoSuccessfulResult when every result is an error for SuccessTimeout, though errors and ticks keep the idle timer alive",
		func(idle, window time.Duration) {
			req := request(200, idle, window, 0, time.Millisecond)
			handle, err := collector.PrepareCollect(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			feed(200, func(i int) { deliverResult(bus, fmt.Sprintf("t-%d", i), nil, "llm request timeout") })

			start := time.Now()
			results, err := collector.AwaitResults(ctx, handle)

			Expect(err).To(MatchError(analysis.ErrNoSuccessfulResult))
			Expect(err).NotTo(MatchError(analysis.ErrTaskTimeout))
			Expect(time.Since(start)).To(BeNumerically(">=", window))
			var nse *analysis.NoSuccessError
			Expect(errors.As(err, &nse)).To(BeTrue())
			Expect(nse.Window).To(Equal(window))
			Expect(nse.Failures).To(BeNumerically(">", 0))
			Expect(results).To(HaveLen(nse.Failures), "every failed result received before the trip is returned")
			Expect(nse.LastSuccess.IsZero()).To(BeTrue(), "no task succeeded since the stage started")
			Expect(nse.LastError).To(MatchError("task_timeout"))
			Expect(err.Error()).To(ContainSubstring("job job-dark"))
			Expect(err.Error()).To(ContainSubstring("analysis.engine.success_timeout"))
			for _, r := range results {
				Expect(r.Error).To(HaveOccurred())
			}
			Expect(abandonsOn(bus)).To(Equal([]natsbus.AbandonedCollection{{
				TenantID: "test-tenant", TaskType: natsbus.TaskRelate, JobID: "job-dark", CollectionID: handle.CollectionID,
			}}))
			Expect(noSuccessCount(tp, string(natsbus.TaskRelate))).To(Equal(int64(1)))
		},
		Entry("with a window shorter than task_timeout", 5*time.Second, 150*time.Millisecond),
		Entry("with a window longer than task_timeout", 100*time.Millisecond, 300*time.Millisecond),
	)

	It("counts errors that will be retried, and redispatches nothing once it fails", func() {
		req := request(1, 5*time.Second, 200*time.Millisecond, 10, 20*time.Millisecond)
		// Every attempt fails again, as against a model server that is down.
		disp.redispatchFn = func(_ context.Context, task analyzer.Task, _ natsbus.TaskType, _ string, _ int) error {
			go deliverResult(bus, task.TaskID, nil, "connection refused")
			return nil
		}
		handle, err := collector.PrepareCollect(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		deliverResult(bus, "t-0", nil, "connection refused")

		_, err = collector.AwaitResults(ctx, handle)

		Expect(err).To(MatchError(analysis.ErrNoSuccessfulResult))
		var nse *analysis.NoSuccessError
		Expect(errors.As(err, &nse)).To(BeTrue())
		Expect(nse.Failures).To(BeNumerically(">=", 2), "the first error and at least one retried one")
		redispatched := len(disp.recordedRedispatches())
		Consistently(func() int { return len(disp.recordedRedispatches()) }, 400*time.Millisecond, 20*time.Millisecond).
			Should(Equal(redispatched), "a retry waiting on its backoff is dropped")
	})

	It("keeps collecting while a success arrives within every SuccessTimeout, though most results are errors", func() {
		req := request(30, 5*time.Second, 250*time.Millisecond, 0, time.Millisecond)
		handle, err := collector.PrepareCollect(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		// 30 results over ~600ms, one in five a success: 100ms apart.
		feed(30, func(i int) {
			if i%5 == 4 {
				deliverResult(bus, fmt.Sprintf("t-%d", i), success, "")
				return
			}
			deliverResult(bus, fmt.Sprintf("t-%d", i), nil, "llm request timeout")
		})

		results, err := collector.AwaitResults(ctx, handle)

		Expect(err).NotTo(HaveOccurred())
		Expect(results).To(HaveLen(30))
		Expect(noSuccessCount(tp, string(natsbus.TaskRelate))).To(BeZero())
	})

	It("does not count worker progress ticks as a success, and names the last success", func() {
		req := request(200, 150*time.Millisecond, 200*time.Millisecond, 0, time.Millisecond)
		handle, err := collector.PrepareCollect(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		deliverResult(bus, "t-0", success, "")
		// Every result after t-0 fails, and each brings a tick.
		feed(199, func(i int) { deliverResult(bus, fmt.Sprintf("t-%d", i+1), nil, "task failed") })

		_, err = collector.AwaitResults(ctx, handle)

		Expect(err).To(MatchError(analysis.ErrNoSuccessfulResult))
		Expect(err).NotTo(MatchError(analysis.ErrTaskTimeout))
		var nse *analysis.NoSuccessError
		Expect(errors.As(err, &nse)).To(BeTrue())
		Expect(nse.LastSuccess.IsZero()).To(BeFalse(), "t-0 succeeded")
		Expect(nse.LastError).To(MatchError("task_failed"))
	})

	Context("after the collection was starved following a failure", func() {
		// Window 200ms, task_timeout 100ms. t-0 fails, then only other
		// stages' ticks arrive for 400ms: the collection is starved.
		var (
			resumed atomic.Bool
			done    chan starvedOutcome
		)

		BeforeEach(func() {
			resumed.Store(false)
			handle, err := collector.PrepareCollect(ctx, request(100, 100*time.Millisecond, 200*time.Millisecond, 0, time.Millisecond))
			Expect(err).NotTo(HaveOccurred())
			deliverResult(bus, "t-0", nil, "task failed")
			next := 1
			feed(100000, func(int) {
				if resumed.Load() && next < 100 {
					deliverResult(bus, fmt.Sprintf("t-%d", next), nil, "task failed")
					next++
				}
			})
			done = make(chan starvedOutcome, 1)
			go func() {
				defer GinkgoRecover()
				results, err := collector.AwaitResults(ctx, handle)
				done <- starvedOutcome{results: results, err: err}
			}()
			Consistently(done, 400*time.Millisecond, 20*time.Millisecond).ShouldNot(Receive(),
				"a stage that stopped receiving its own results is starved, not failing")
		})

		It("does not fail on a single failure once it resumes", func() {
			deliverResult(bus, "t-99", nil, "task failed")

			Consistently(done, 600*time.Millisecond, 20*time.Millisecond).ShouldNot(Receive(),
				"the failure before the starvation does not count toward the new window")
		})

		It("fails only after a full window of resumed failures, counting only those", func() {
			resumedAt := time.Now()
			resumed.Store(true)

			var got starvedOutcome
			Eventually(done, 2*time.Second).Should(Receive(&got))
			Expect(time.Since(resumedAt)).To(BeNumerically(">=", 200*time.Millisecond))
			Expect(got.err).To(MatchError(analysis.ErrNoSuccessfulResult))
			var nse *analysis.NoSuccessError
			Expect(errors.As(got.err, &nse)).To(BeTrue())
			Expect(nse.Failures).To(Equal(len(got.results)-1), "every result failed, and only t-0 came before the starvation")
		})
	})

	It("does not fail a collection that gets no result of its own while the pool works through another's queue", func() {
		req := request(2, 150*time.Millisecond, 100*time.Millisecond, 0, time.Millisecond)
		handle, err := collector.PrepareCollect(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		// Ticks for ~400ms, four windows, then this collection's results.
		feed(22, func(i int) {
			if i == 20 {
				deliverResult(bus, "t-0", success, "")
			}
			if i == 21 {
				deliverResult(bus, "t-1", success, "")
			}
		})

		results, err := collector.AwaitResults(ctx, handle)

		Expect(err).NotTo(HaveOccurred())
		Expect(results).To(HaveLen(2))
	})

	DescribeTable("refuses to prepare a collection without a positive SuccessTimeout, subscribing to nothing",
		func(window time.Duration) {
			handle, err := collector.PrepareCollect(ctx, request(1, time.Second, window, 3, time.Millisecond))
			Expect(err).To(MatchError(analysis.ErrNoSuccessTimeout))
			Expect(err.Error()).To(ContainSubstring("analysis.engine.success_timeout"))
			Expect(handle).To(BeNil())
			Expect(bus.Subscriptions()).To(BeEmpty())
		},
		Entry("zero", time.Duration(0)),
		Entry("negative", -time.Second),
	)
})
