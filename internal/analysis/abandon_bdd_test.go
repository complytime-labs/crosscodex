package analysis_test

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/complytime-labs/crosscodex/internal/analysis"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/natsbus/natsbustest"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// abandonsOn returns the collections named by every abandon control message
// published on bus, in publish order, and fails the spec on one carrying data.
func abandonsOn(bus *natsbustest.Fake) []natsbus.AbandonedCollection {
	var out []natsbus.AbandonedCollection
	for _, p := range bus.Published() {
		if !server.SubjectMatchesFilter(p.Subject, natsbus.AbandonedSubjectFilter) {
			continue
		}
		ExpectWithOffset(1, p.Data).To(BeEmpty(), "an abandon carries no payload")
		c, err := natsbus.ParseAbandonedSubject(p.Subject)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		out = append(out, c)
	}
	return out
}

// abandonedCount sums analysis.collections.abandoned data points whose
// outcome attribute is outcome.
func abandonedCount(tp *telemetrytest.TestProvider, outcome string) int64 {
	m := telemetrytest.FindMetric(tp.GetMetrics(), "analysis.collections.abandoned")
	if m == nil {
		return 0
	}
	sum, ok := m.Data.(metricdata.Sum[int64])
	ExpectWithOffset(1, ok).To(BeTrue(), "analysis.collections.abandoned is %T, not an Int64Counter", m.Data)
	var total int64
	for _, dp := range sum.DataPoints {
		if v, ok := dp.Attributes.Value(attribute.Key("outcome")); ok && v.AsString() == outcome {
			total += dp.Value
		}
	}
	return total
}

var _ = Describe("Abandoning incomplete collections", func() {
	var (
		bus       *natsbustest.Fake
		disp      *fakeDispatcher
		tp        *telemetrytest.TestProvider
		collector *analysis.NATSCollector
		ctx       context.Context
		cancel    context.CancelFunc
	)

	BeforeEach(func() {
		bus = &natsbustest.Fake{}
		disp = &fakeDispatcher{}
		var err error
		tp, err = telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(tp.Shutdown(context.Background())).To(Succeed()) })
		collector = analysis.NewNATSCollector(bus, analysis.WithCollectorTelemetry(tp.TracerProvider(), tp.MeterProvider()))
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(func() { cancel() })
		ctx, err = tenant.WithTenant(ctx, "test-tenant")
		Expect(err).NotTo(HaveOccurred())
	})

	request := func(timeout, stageTimeout time.Duration) analysis.CollectRequest {
		return analysis.CollectRequest{
			TaskType:    natsbus.TaskClassify,
			JobID:       "job-1",
			ExpectedIDs: []string{"t-1", "t-2"},
			Tasks: []analyzer.Task{
				{TaskID: "t-1", TaskType: "classify", Payload: &structpb.Struct{}},
				{TaskID: "t-2", TaskType: "classify", Payload: &structpb.Struct{}},
			},
			Timeout:        timeout,
			StageTimeout:   stageTimeout,
			SuccessTimeout: time.Hour,
			MaxRetries:     1,
			Backoff:        10 * time.Millisecond,
			Dispatcher:     disp,
		}
	}

	It("gives every collection its own collection ID, so a resumed job's tasks never match an old abandon", func() {
		first, err := collector.PrepareCollect(ctx, request(time.Second, time.Hour))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(first.ExportRelease)
		second, err := collector.PrepareCollect(ctx, request(time.Second, time.Hour))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(second.ExportRelease)

		Expect(first.CollectionID).NotTo(BeEmpty())
		Expect(second.CollectionID).NotTo(Equal(first.CollectionID))
	})

	DescribeTable("publishes exactly one abandon naming the collection when AwaitResults ends incomplete",
		func(timeout, stageTimeout time.Duration, cancelJob bool, wantErr error) {
			handle, err := collector.PrepareCollect(ctx, request(timeout, stageTimeout))
			Expect(err).NotTo(HaveOccurred())
			wantResults := 0
			if cancelJob {
				// Cancelled before AwaitResults runs, so it returns at once with
				// nothing collected; a result delivered first would race the
				// cancellation in AwaitResults' select.
				cancel()
			} else {
				payload, _ := structpb.NewStruct(map[string]interface{}{"result": "1"})
				deliverResult(bus, "t-1", payload, "") // t-2 never arrives
				wantResults = 1
			}

			results, err := collector.AwaitResults(ctx, handle)

			Expect(err).To(MatchError(wantErr))
			Expect(results).To(HaveLen(wantResults))
			Expect(abandonsOn(bus)).To(Equal([]natsbus.AbandonedCollection{{
				TenantID: "test-tenant", TaskType: natsbus.TaskClassify, JobID: "job-1", CollectionID: handle.CollectionID,
			}}))
			Expect(abandonedCount(tp, "published")).To(Equal(int64(1)))
		},
		Entry("on the idle timeout (analysis.engine.task_timeout)", 30*time.Millisecond, time.Hour, false, analysis.ErrTaskTimeout),
		Entry("on the stage cap (pipeline.stage_timeout)", time.Hour, 30*time.Millisecond, false, analysis.ErrStageTimeout),
		Entry("on context cancellation, which is how CancelJob reaches a running stage", time.Hour, time.Hour, true, context.Canceled),
	)

	It("publishes no abandon when every expected result arrives", func() {
		handle, err := collector.PrepareCollect(ctx, request(time.Second, time.Hour))
		Expect(err).NotTo(HaveOccurred())
		payload, _ := structpb.NewStruct(map[string]interface{}{"result": "1"})
		deliverResult(bus, "t-1", payload, "")
		deliverResult(bus, "t-2", payload, "")

		results, err := collector.AwaitResults(ctx, handle)

		Expect(err).NotTo(HaveOccurred())
		Expect(results).To(HaveLen(2))
		Expect(abandonsOn(bus)).To(BeEmpty())
		Expect(abandonedCount(tp, "published")).To(BeZero())
	})

	It("keeps the stage's own error and counts the failure when the abandon cannot be published", func() {
		bus.PublishFunc = func(context.Context, string, []byte) error {
			return errors.New("nats: connection closed")
		}
		handle, err := collector.PrepareCollect(ctx, request(30*time.Millisecond, time.Hour))
		Expect(err).NotTo(HaveOccurred())

		_, err = collector.AwaitResults(ctx, handle)

		Expect(err).To(MatchError(analysis.ErrTaskTimeout))
		Expect(err).NotTo(MatchError(ContainSubstring("nats: connection closed")))
		Expect(abandonedCount(tp, "publish_failed")).To(Equal(int64(1)))
		Expect(abandonedCount(tp, "published")).To(BeZero())
	})

	It("publishes a cancelled collection's abandon on a context that is not cancelled but expires", func() {
		type publishCtx struct {
			err         error
			hasDeadline bool
			remaining   time.Duration
		}
		published := make(chan publishCtx, 1)
		bus.PublishFunc = func(pctx context.Context, subject string, _ []byte) error {
			if _, err := natsbus.ParseAbandonedSubject(subject); err == nil {
				deadline, ok := pctx.Deadline()
				published <- publishCtx{err: pctx.Err(), hasDeadline: ok, remaining: time.Until(deadline)}
			}
			return nil
		}
		handle, err := collector.PrepareCollect(ctx, request(time.Hour, time.Hour))
		Expect(err).NotTo(HaveOccurred())
		done := make(chan error, 1)
		go func() {
			defer GinkgoRecover()
			_, err := collector.AwaitResults(ctx, handle)
			done <- err
		}()

		cancel()

		var got publishCtx
		Eventually(published).Should(Receive(&got))
		Expect(got.err).NotTo(HaveOccurred())
		Expect(got.hasDeadline).To(BeTrue(), "the abandon publish must be bounded")
		Expect(got.remaining).To(BeNumerically("<=", 30*time.Second))
		Eventually(done).Should(Receive(MatchError(context.Canceled)))
	})

	It("redispatches a failed task under its collection's ID", func() {
		handle, err := collector.PrepareCollect(ctx, request(time.Second, time.Hour))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(handle.ExportRelease)

		deliverResult(bus, "t-1", nil, "worker error")

		Eventually(func() []string {
			var ids []string
			for _, r := range disp.recordedRedispatches() {
				ids = append(ids, r.collectionID)
			}
			return ids
		}).Should(Equal([]string{handle.CollectionID}))
	})

	It("cancels retries still waiting on their backoff once the collection is abandoned", func() {
		req := request(30*time.Millisecond, time.Hour)
		req.Backoff = 300 * time.Millisecond
		handle, err := collector.PrepareCollect(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		deliverResult(bus, "t-1", nil, "worker error")

		_, err = collector.AwaitResults(ctx, handle)
		Expect(err).To(MatchError(analysis.ErrTaskTimeout))

		Consistently(disp.recordedRedispatches, 600*time.Millisecond, 50*time.Millisecond).Should(BeEmpty())
	})

	It("never redispatches after the abandon is published, even when a retry's backoff ends during the publish", func() {
		// The abandon publish blocks until a redispatch happens or 500ms
		// pass, so a retry timer that is still live while the abandon goes
		// out is sure to fire inside that window.
		var mu sync.Mutex
		var events []string
		record := func(e string) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, e)
		}
		redispatched := make(chan struct{}, 1)
		disp.redispatchFn = func(context.Context, analyzer.Task, natsbus.TaskType, string, int) error {
			record("redispatch")
			select {
			case redispatched <- struct{}{}:
			default:
			}
			return nil
		}
		bus.PublishFunc = func(_ context.Context, subject string, _ []byte) error {
			if server.SubjectMatchesFilter(subject, natsbus.AbandonedSubjectFilter) {
				record("abandon")
				select {
				case <-redispatched:
				case <-time.After(500 * time.Millisecond):
				}
			}
			return nil
		}
		req := request(50*time.Millisecond, time.Hour)
		req.Backoff = 80 * time.Millisecond // fires after the idle timeout: 60-100ms
		handle, err := collector.PrepareCollect(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		deliverResult(bus, "t-1", nil, "worker error")

		_, err = collector.AwaitResults(ctx, handle)
		Expect(err).To(MatchError(analysis.ErrTaskTimeout))

		mu.Lock()
		defer mu.Unlock()
		Expect(events).To(HaveExactElements("abandon"), "a redispatch after the abandon is a task the workers may run for nobody")
	})

	Describe("through the engine", func() {
		var registry *analyzer.Registry

		BeforeEach(func() {
			registry = analyzer.NewRegistry()
		})

		It("abandons the collection when Dispatch fails part way", func() {
			Expect(analyzer.Register[*emptypb.Empty](registry, newStubWithTasks("classify", makeTasks("classify", 2)))).To(Succeed())
			failing := &engineTestDispatcher{dispatchErr: errors.New("NATS connection lost")}
			engine := analysis.New(registry, failing, collector, validCfg(), allTaskTypes(),
				analysis.WithStageTimeout(time.Hour))

			result, err := engine.Execute(ctx, analysis.ExecutionRequest{JobID: "job-dispatch", Input: &emptypb.Empty{}})

			Expect(err).NotTo(HaveOccurred())
			Expect(result.Errors["classify"]).To(MatchError(ContainSubstring("dispatch failed")))
			abandons := abandonsOn(bus)
			Expect(abandons).To(HaveLen(1))
			Expect(abandons[0]).To(abandonOfAnyCollection(natsbus.AbandonedCollection{
				TenantID: "test-tenant", TaskType: natsbus.TaskClassify, JobID: "job-dispatch",
			}))
		})

		It("abandons every in-flight collection of a cancelled job, one per task type, under the IDs it dispatched", func() {
			Expect(analyzer.Register[*emptypb.Empty](registry, newStubWithTasks("classify", makeTasks("classify", 2)))).To(Succeed())
			Expect(analyzer.Register[*emptypb.Empty](registry, newStubWithTasks("artifacts", makeTasks("artifacts", 2)))).To(Succeed())
			recorder := &engineTestDispatcher{}
			engine := analysis.New(registry, recorder, collector, validCfg(), allTaskTypes(),
				analysis.WithStageTimeout(time.Hour))

			done := make(chan error, 1)
			go func() {
				_, err := engine.Execute(ctx, analysis.ExecutionRequest{JobID: "job-cancel", Input: &emptypb.Empty{}})
				done <- err
			}()
			Eventually(recorder.dispatchCount).Should(Equal(2))
			cancel()
			Eventually(done).Should(Receive(MatchError(context.Canceled)))

			recorder.mu.Lock()
			dispatched := map[natsbus.TaskType]string{}
			for _, d := range recorder.dispatches {
				dispatched[d.taskType] = d.collectionID
			}
			recorder.mu.Unlock()
			Expect(abandonsOn(bus)).To(ConsistOf(
				natsbus.AbandonedCollection{TenantID: "test-tenant", TaskType: natsbus.TaskClassify, JobID: "job-cancel", CollectionID: dispatched[natsbus.TaskClassify]},
				natsbus.AbandonedCollection{TenantID: "test-tenant", TaskType: natsbus.TaskArtifacts, JobID: "job-cancel", CollectionID: dispatched[natsbus.TaskArtifacts]},
			))
		})
	})
})

// abandonOfAnyCollection matches an AbandonedCollection on the tenant, task
// type and job of want with any non-empty CollectionID, for specs that cannot
// know the ID.
func abandonOfAnyCollection(want natsbus.AbandonedCollection) OmegaMatcher {
	return And(
		HaveField("TenantID", want.TenantID),
		HaveField("TaskType", want.TaskType),
		HaveField("JobID", want.JobID),
		HaveField("CollectionID", Not(BeEmpty())),
	)
}
