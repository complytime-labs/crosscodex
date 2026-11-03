package analysis_test

import (
	"context"
	"errors"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/complytime-labs/crosscodex/internal/analysis"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/natsbus/natsbustest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// fakeDispatcher records Redispatch calls.
type fakeDispatcher struct {
	mu           sync.Mutex
	redispatches []redispatchCall
	redispatchFn func(ctx context.Context, task analyzer.Task, taskType natsbus.TaskType, jobID string, retryCount int) error
}

type redispatchCall struct {
	taskID       string
	collectionID string
	retryCount   int
}

func (f *fakeDispatcher) Dispatch(_ context.Context, _ []analyzer.Task, _ natsbus.TaskType, _, _ string) error {
	return nil
}

func (f *fakeDispatcher) Redispatch(ctx context.Context, task analyzer.Task, taskType natsbus.TaskType, jobID, collectionID string, retryCount int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.redispatches = append(f.redispatches, redispatchCall{taskID: task.TaskID, collectionID: collectionID, retryCount: retryCount})
	if f.redispatchFn != nil {
		return f.redispatchFn(ctx, task, taskType, jobID, retryCount)
	}
	return nil
}

// recordedRedispatches returns a copy of the Redispatch calls so far.
func (f *fakeDispatcher) recordedRedispatches() []redispatchCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]redispatchCall(nil), f.redispatches...)
}

// deliverResult simulates a worker publishing a result message on the result
// subject the collector most recently subscribed to. It delivers nothing when
// the collector has not subscribed yet.
func deliverResult(bus *natsbustest.Fake, taskID string, result proto.Message, errorMsg string) {
	subs := bus.Subscriptions()
	for i := len(subs) - 1; i >= 0; i-- {
		if subs[i].Subject != natsbus.WorkerProgressSubject {
			deliverResultOn(bus, subs[i].Subject, taskID, result, errorMsg)
			return
		}
	}
}

// deliverTick simulates any worker in the pool finishing a task.
func deliverTick(bus *natsbustest.Fake) {
	Expect(bus.Deliver(&natsbus.Message{Subject: natsbus.WorkerProgressSubject})).To(Succeed())
}

// progressSub returns the collector's worker progress subscription.
func progressSub(bus *natsbustest.Fake) *natsbustest.Sub {
	for _, s := range bus.Subscriptions() {
		if s.Subject == natsbus.WorkerProgressSubject {
			return s
		}
	}
	Fail("the collector has no worker progress subscription")
	return nil
}

// deliverResultOn simulates a worker publishing a result message on subject.
func deliverResultOn(bus *natsbustest.Fake, subject, taskID string, result proto.Message, errorMsg string) {
	var data []byte
	var err error
	if result != nil {
		data, err = proto.Marshal(result)
		Expect(err).NotTo(HaveOccurred())
	}

	headers := map[string][]string{
		"X-Task-Id": {taskID},
	}
	if errorMsg != "" {
		headers["X-Error"] = []string{errorMsg}
	}

	_ = bus.Deliver(&natsbus.Message{
		Subject: subject,
		Data:    data,
		Headers: headers,
	})
}

var _ = Describe("NATSCollector", func() {
	var (
		fakeNATS   *natsbustest.Fake
		fakeDisp   *fakeDispatcher
		collector  *analysis.NATSCollector
		ctx        context.Context
		cancelFunc context.CancelFunc
	)

	BeforeEach(func() {
		fakeNATS = &natsbustest.Fake{}
		fakeDisp = &fakeDispatcher{}
		collector = analysis.NewNATSCollector(fakeNATS)
		ctx, cancelFunc = context.WithCancel(context.Background())
		ctx, _ = tenant.WithTenant(ctx, "test-tenant")
	})

	AfterEach(func() {
		cancelFunc()
	})

	Describe("PrepareCollect + AwaitResults", func() {
		It("returns all results when every expected ID arrives", func() {
			tasks := []analyzer.Task{
				{TaskID: "t-1", TaskType: "classify", Payload: &structpb.Struct{}},
				{TaskID: "t-2", TaskType: "classify", Payload: &structpb.Struct{}},
			}
			req := analysis.CollectRequest{
				TaskType:     natsbus.TaskClassify,
				JobID:        "job-1",
				ExpectedIDs:  []string{"t-1", "t-2"},
				Tasks:        tasks,
				Timeout:      1 * time.Second,
				StageTimeout: time.Hour,
				MaxRetries:   3,
				Backoff:      10 * time.Millisecond,
				Dispatcher:   fakeDisp,
			}

			go func() {
				time.Sleep(10 * time.Millisecond)
				payload1, _ := structpb.NewStruct(map[string]interface{}{"result": "1"})
				deliverResult(fakeNATS, "t-1", payload1, "")
				payload2, _ := structpb.NewStruct(map[string]interface{}{"result": "2"})
				deliverResult(fakeNATS, "t-2", payload2, "")
			}()

			handle, err := collector.PrepareCollect(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			results, err := collector.AwaitResults(ctx, handle)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(2))
			Expect(fakeNATS.Subscriptions()[0].Unsubscribed()).To(BeTrue())
		})

		It("calls Redispatch on error result with retries remaining", func() {
			tasks := []analyzer.Task{
				{TaskID: "t-1", TaskType: "classify", Payload: &structpb.Struct{}},
			}
			req := analysis.CollectRequest{
				TaskType:     natsbus.TaskClassify,
				JobID:        "job-1",
				ExpectedIDs:  []string{"t-1"},
				Tasks:        tasks,
				Timeout:      1 * time.Second,
				StageTimeout: time.Hour,
				MaxRetries:   2,
				Backoff:      1 * time.Millisecond,
				Dispatcher:   fakeDisp,
			}

			go func() {
				time.Sleep(10 * time.Millisecond)
				// First attempt: error
				deliverResult(fakeNATS, "t-1", nil, "worker error")
				time.Sleep(20 * time.Millisecond)
				// Second attempt: success
				payload, _ := structpb.NewStruct(map[string]interface{}{"result": "ok"})
				deliverResult(fakeNATS, "t-1", payload, "")
			}()

			handle, err := collector.PrepareCollect(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			results, err := collector.AwaitResults(ctx, handle)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(1))
			Expect(results[0].Error).To(BeNil())

			fakeDisp.mu.Lock()
			redispatches := fakeDisp.redispatches
			fakeDisp.mu.Unlock()

			Expect(redispatches).To(HaveLen(1))
			Expect(redispatches[0].taskID).To(Equal("t-1"))
			Expect(redispatches[0].retryCount).To(Equal(1))
		})

		It("redispatches under the collection's tenant when the NATS handler context carries none", func() {
			// The real client builds the handler context from
			// context.Background(), so the tenant is never in it. The real
			// dispatcher, unlike fakeDispatcher, refuses a tenantless context.
			dispatchBus := &natsbustest.Fake{}
			tasks := []analyzer.Task{
				{TaskID: "t-1", TaskType: "classify", Payload: &structpb.Struct{}},
			}
			req := analysis.CollectRequest{
				TaskType:     natsbus.TaskClassify,
				JobID:        "job-1",
				ExpectedIDs:  []string{"t-1"},
				Tasks:        tasks,
				Timeout:      1 * time.Second,
				StageTimeout: time.Hour,
				MaxRetries:   2,
				Backoff:      1 * time.Millisecond,
				Dispatcher:   analysis.NewNATSDispatcher(dispatchBus),
			}

			go func() {
				time.Sleep(10 * time.Millisecond)
				deliverResult(fakeNATS, "t-1", nil, "worker error")
				time.Sleep(20 * time.Millisecond)
				payload, _ := structpb.NewStruct(map[string]interface{}{"result": "ok"})
				deliverResult(fakeNATS, "t-1", payload, "")
			}()

			handle, err := collector.PrepareCollect(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			results, err := collector.AwaitResults(ctx, handle)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(1))
			Expect(results[0].Error).NotTo(HaveOccurred(), "a retry must not fail the task")

			wantSubject, err := natsbus.WorkSubject("test-tenant", natsbus.TaskClassify, "job-1")
			Expect(err).NotTo(HaveOccurred())
			published := dispatchBus.Published()
			Expect(published).To(HaveLen(1))
			Expect(published[0].Subject).To(Equal(wantSubject))
			Expect(published[0].Headers[analysis.ExportHeaderRetryCount]).To(Equal([]string{"1"}))
		})

		It("records failure when retries exhausted", func() {
			tasks := []analyzer.Task{
				{TaskID: "t-1", TaskType: "classify", Payload: &structpb.Struct{}},
			}
			req := analysis.CollectRequest{
				TaskType:     natsbus.TaskClassify,
				JobID:        "job-1",
				ExpectedIDs:  []string{"t-1"},
				Tasks:        tasks,
				Timeout:      1 * time.Second,
				StageTimeout: time.Hour,
				MaxRetries:   1,
				Backoff:      1 * time.Millisecond,
				Dispatcher:   fakeDisp,
			}

			go func() {
				time.Sleep(10 * time.Millisecond)
				// First attempt: error
				deliverResult(fakeNATS, "t-1", nil, "worker error")
				time.Sleep(20 * time.Millisecond)
				// Second attempt: error (exhausted)
				deliverResult(fakeNATS, "t-1", nil, "worker error")
			}()

			handle, err := collector.PrepareCollect(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			results, err := collector.AwaitResults(ctx, handle)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(1))
			Expect(results[0].Error).NotTo(BeNil())
			Expect(errors.Is(results[0].Error, analysis.ErrRetryExhausted)).To(BeTrue())
		})

		It("returns ErrTaskTimeout when timeout fires before all results", func() {
			tasks := []analyzer.Task{
				{TaskID: "t-1", TaskType: "classify", Payload: &structpb.Struct{}},
				{TaskID: "t-2", TaskType: "classify", Payload: &structpb.Struct{}},
			}
			req := analysis.CollectRequest{
				TaskType:     natsbus.TaskClassify,
				JobID:        "job-1",
				ExpectedIDs:  []string{"t-1", "t-2"},
				Tasks:        tasks,
				Timeout:      50 * time.Millisecond,
				StageTimeout: time.Hour,
				MaxRetries:   3,
				Backoff:      10 * time.Millisecond,
				Dispatcher:   fakeDisp,
			}

			go func() {
				time.Sleep(10 * time.Millisecond)
				payload, _ := structpb.NewStruct(map[string]interface{}{"result": "1"})
				deliverResult(fakeNATS, "t-1", payload, "")
				// t-2 never arrives
			}()

			handle, err := collector.PrepareCollect(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			results, err := collector.AwaitResults(ctx, handle)
			Expect(err).To(HaveOccurred())
			Expect(err).To(MatchError(analysis.ErrTaskTimeout))
			Expect(err.Error()).To(ContainSubstring("no task result or worker progress for 50ms"))
			Expect(err.Error()).To(ContainSubstring("1 of 2 tasks"))
			Expect(err.Error()).To(ContainSubstring("check that LLM workers are running"))
			Expect(err.Error()).To(ContainSubstring("analysis.engine.task_timeout"))
			Expect(results).To(HaveLen(1)) // Partial results returned
			Expect(progressSub(fakeNATS).Unsubscribed()).To(BeTrue())
		})

		It("keeps waiting past Timeout while another collection's tasks keep the shared pool busy", func() {
			// Two concerns share one worker pool: the pool works through
			// "requires" first, so the "relate" collection sees no result of
			// its own for more than twice Timeout, only the ticks every
			// worker publishes after each task.
			busyIDs := []string{"q-1", "q-2", "q-3", "q-4", "q-5", "q-6", "q-7", "q-8", "q-9", "q-10", "q-11"}
			busyTasks := make([]analyzer.Task, len(busyIDs))
			for i, id := range busyIDs {
				busyTasks[i] = analyzer.Task{TaskID: id, TaskType: "requires", Payload: &structpb.Struct{}}
			}
			busyReq := analysis.CollectRequest{
				TaskType: natsbus.TaskRequires, JobID: "job-1", ExpectedIDs: busyIDs, Tasks: busyTasks,
				Timeout: 500 * time.Millisecond, StageTimeout: time.Hour, MaxRetries: 3, Backoff: 10 * time.Millisecond, Dispatcher: fakeDisp,
			}
			slowReq := analysis.CollectRequest{
				TaskType: natsbus.TaskRelate, JobID: "job-1", ExpectedIDs: []string{"r-1"},
				Tasks:   []analyzer.Task{{TaskID: "r-1", TaskType: "relate", Payload: &structpb.Struct{}}},
				Timeout: 500 * time.Millisecond, StageTimeout: time.Hour, MaxRetries: 3, Backoff: 10 * time.Millisecond, Dispatcher: fakeDisp,
			}
			busySubject, err := natsbus.ResultSubject("test-tenant", natsbus.TaskRequires, "job-1")
			Expect(err).NotTo(HaveOccurred())
			slowSubject, err := natsbus.ResultSubject("test-tenant", natsbus.TaskRelate, "job-1")
			Expect(err).NotTo(HaveOccurred())

			busyHandle, err := collector.PrepareCollect(ctx, busyReq)
			Expect(err).NotTo(HaveOccurred())
			slowHandle, err := collector.PrepareCollect(ctx, slowReq)
			Expect(err).NotTo(HaveOccurred())

			payload, _ := structpb.NewStruct(map[string]interface{}{"result": "1"})
			go func() {
				defer GinkgoRecover()
				// One busy result and its tick every 100ms (a fifth of
				// Timeout) for 1.1s, then the slow collection's own result.
				for _, id := range busyIDs {
					time.Sleep(100 * time.Millisecond)
					deliverResultOn(fakeNATS, busySubject, id, payload, "")
					deliverTick(fakeNATS)
				}
				deliverResultOn(fakeNATS, slowSubject, "r-1", payload, "")
				deliverTick(fakeNATS)
			}()

			type outcome struct {
				results []analyzer.TaskResult
				err     error
			}
			busyDone := make(chan outcome, 1)
			go func() {
				r, err := collector.AwaitResults(ctx, busyHandle)
				busyDone <- outcome{r, err}
			}()

			start := time.Now()
			results, err := collector.AwaitResults(ctx, slowHandle)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(1))
			Expect(results[0].TaskID).To(Equal("r-1"))
			Expect(time.Since(start)).To(BeNumerically(">", 2*slowReq.Timeout))

			var busy outcome
			Eventually(busyDone, 5*time.Second).Should(Receive(&busy))
			Expect(busy.err).NotTo(HaveOccurred())
			Expect(busy.results).To(HaveLen(len(busyIDs)))
		})

		It("does not count ticks as results, and times out once they stop", func() {
			req := analysis.CollectRequest{
				TaskType: natsbus.TaskClassify, JobID: "job-1", ExpectedIDs: []string{"t-1"},
				Tasks:   []analyzer.Task{{TaskID: "t-1", TaskType: "classify", Payload: &structpb.Struct{}}},
				Timeout: 300 * time.Millisecond, StageTimeout: time.Hour, MaxRetries: 3, Backoff: 10 * time.Millisecond, Dispatcher: fakeDisp,
			}
			handle, err := collector.PrepareCollect(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			stopTicks := make(chan struct{})
			ticksStopped := make(chan struct{})
			go func() {
				defer GinkgoRecover()
				defer close(ticksStopped)
				for {
					select {
					case <-stopTicks:
						return
					case <-time.After(50 * time.Millisecond):
						deliverTick(fakeNATS)
					}
				}
			}()

			type outcome struct {
				results []analyzer.TaskResult
				err     error
			}
			done := make(chan outcome, 1)
			go func() {
				r, err := collector.AwaitResults(ctx, handle)
				done <- outcome{r, err}
			}()

			// Twice Timeout of ticks alone neither completes the collection
			// nor times it out.
			Consistently(done, 2*req.Timeout, 10*time.Millisecond).ShouldNot(Receive())
			close(stopTicks)
			<-ticksStopped

			var got outcome
			Eventually(done, 5*time.Second).Should(Receive(&got))
			Expect(got.err).To(MatchError(analysis.ErrTaskTimeout))
			Expect(got.err.Error()).To(ContainSubstring("0 of 1 tasks"))
			Expect(got.results).To(BeEmpty())
		})

		It("fails with ErrStageTimeout at StageTimeout while ticks keep arriving and a task never returns", func() {
			req := analysis.CollectRequest{
				TaskType: natsbus.TaskRelate, JobID: "job-lost", ExpectedIDs: []string{"t-1", "t-lost"},
				Tasks: []analyzer.Task{
					{TaskID: "t-1", TaskType: "relate", Payload: &structpb.Struct{}},
					{TaskID: "t-lost", TaskType: "relate", Payload: &structpb.Struct{}},
				},
				Timeout: 200 * time.Millisecond, StageTimeout: 600 * time.Millisecond,
				MaxRetries: 3, Backoff: 10 * time.Millisecond, Dispatcher: fakeDisp,
			}
			handle, err := collector.PrepareCollect(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			// Other work keeps the pool busy: a tick every 20ms, a tenth of
			// the idle Timeout, for as long as the collection waits.
			stopTicks := make(chan struct{})
			ticksStopped := make(chan struct{})
			DeferCleanup(func() { close(stopTicks); <-ticksStopped })
			go func() {
				defer GinkgoRecover()
				defer close(ticksStopped)
				for {
					select {
					case <-stopTicks:
						return
					case <-time.After(20 * time.Millisecond):
						deliverTick(fakeNATS)
					}
				}
			}()
			payload, _ := structpb.NewStruct(map[string]interface{}{"result": "1"})
			deliverResult(fakeNATS, "t-1", payload, "")

			start := time.Now()
			results, err := collector.AwaitResults(ctx, handle)
			Expect(err).To(MatchError(analysis.ErrStageTimeout))
			Expect(err).NotTo(MatchError(analysis.ErrTaskTimeout))
			Expect(err.Error()).To(ContainSubstring("ran longer than pipeline.stage_timeout (600ms), 1 of 2 tasks complete"))
			Expect(err.Error()).To(ContainSubstring("a task may have been lost, so rerun the job, or raise pipeline.stage_timeout"))
			Expect(time.Since(start)).To(BeNumerically(">=", 550*time.Millisecond))
			Expect(results).To(HaveLen(1))
			Expect(results[0].TaskID).To(Equal("t-1"))
			Expect(progressSub(fakeNATS).Unsubscribed()).To(BeTrue())
		})

		DescribeTable("refuses to prepare a collection without a positive StageTimeout, subscribing to nothing",
			func(stageTimeout time.Duration) {
				req := analysis.CollectRequest{
					TaskType: natsbus.TaskClassify, JobID: "job-1", ExpectedIDs: []string{"t-1"},
					Tasks:   []analyzer.Task{{TaskID: "t-1", TaskType: "classify", Payload: &structpb.Struct{}}},
					Timeout: time.Second, StageTimeout: stageTimeout, MaxRetries: 3, Backoff: 10 * time.Millisecond, Dispatcher: fakeDisp,
				}
				handle, err := collector.PrepareCollect(ctx, req)
				Expect(err).To(MatchError(analysis.ErrNoStageTimeout))
				Expect(err.Error()).To(ContainSubstring("pipeline.stage_timeout"))
				Expect(err.Error()).To(ContainSubstring("analysis.WithStageTimeout"))
				Expect(handle).To(BeNil())
				Expect(fakeNATS.Subscriptions()).To(BeEmpty())
			},
			Entry("zero", time.Duration(0)),
			Entry("negative", -time.Second),
		)

		It("removes the worker progress subscription when AwaitResults returns", func() {
			req := analysis.CollectRequest{
				TaskType: natsbus.TaskClassify, JobID: "job-1", ExpectedIDs: []string{"t-1"},
				Tasks:   []analyzer.Task{{TaskID: "t-1", TaskType: "classify", Payload: &structpb.Struct{}}},
				Timeout: 1 * time.Second, StageTimeout: time.Hour, MaxRetries: 3, Backoff: 10 * time.Millisecond, Dispatcher: fakeDisp,
			}
			handle, err := collector.PrepareCollect(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(progressSub(fakeNATS).Unsubscribed()).To(BeFalse())

			payload, _ := structpb.NewStruct(map[string]interface{}{"result": "1"})
			deliverResult(fakeNATS, "t-1", payload, "")
			results, err := collector.AwaitResults(ctx, handle)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(1))
			Expect(progressSub(fakeNATS).Unsubscribed()).To(BeTrue())

			// A tick after the collection returned reaches no handler and
			// cannot block the publisher.
			deliverTick(fakeNATS)
		})

		It("fails PrepareCollect and removes the result subscription when the progress subscription fails", func() {
			fakeNATS.SubscribeWorkerProgressFunc = func(context.Context, func()) (natsbus.Subscription, error) {
				return nil, natsbus.ErrSubscribeFailed
			}
			req := analysis.CollectRequest{
				TaskType: natsbus.TaskClassify, JobID: "job-1", ExpectedIDs: []string{"t-1"},
				Tasks:   []analyzer.Task{{TaskID: "t-1", TaskType: "classify", Payload: &structpb.Struct{}}},
				Timeout: 1 * time.Second, StageTimeout: time.Hour, MaxRetries: 3, Backoff: 10 * time.Millisecond, Dispatcher: fakeDisp,
			}
			_, err := collector.PrepareCollect(ctx, req)
			Expect(err).To(MatchError(natsbus.ErrSubscribeFailed))
			Expect(err.Error()).To(ContainSubstring("worker progress"))

			subs := fakeNATS.Subscriptions()
			Expect(subs).To(HaveLen(1))
			Expect(subs[0].Subject).NotTo(Equal(natsbus.WorkerProgressSubject))
			Expect(subs[0].Unsubscribed()).To(BeTrue())
		})

		It("keeps collecting past Timeout while results keep arriving", func() {
			tasks := []analyzer.Task{
				{TaskID: "t-1", TaskType: "classify", Payload: &structpb.Struct{}},
				{TaskID: "t-2", TaskType: "classify", Payload: &structpb.Struct{}},
				{TaskID: "t-3", TaskType: "classify", Payload: &structpb.Struct{}},
				{TaskID: "t-4", TaskType: "classify", Payload: &structpb.Struct{}},
				{TaskID: "t-5", TaskType: "classify", Payload: &structpb.Struct{}},
				{TaskID: "t-6", TaskType: "classify", Payload: &structpb.Struct{}},
			}
			req := analysis.CollectRequest{
				TaskType:     natsbus.TaskClassify,
				JobID:        "job-1",
				ExpectedIDs:  []string{"t-1", "t-2", "t-3", "t-4", "t-5", "t-6"},
				Tasks:        tasks,
				Timeout:      500 * time.Millisecond,
				StageTimeout: time.Hour,
				MaxRetries:   3,
				Backoff:      10 * time.Millisecond,
				Dispatcher:   fakeDisp,
			}

			handle, err := collector.PrepareCollect(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			// One result every 100ms: 600ms in total, longer than Timeout, but
			// no gap longer than a fifth of Timeout.
			go func() {
				defer GinkgoRecover()
				payload, _ := structpb.NewStruct(map[string]interface{}{"result": "1"})
				for _, id := range []string{"t-1", "t-2", "t-3", "t-4", "t-5", "t-6"} {
					time.Sleep(100 * time.Millisecond)
					deliverResult(fakeNATS, id, payload, "")
				}
			}()

			results, err := collector.AwaitResults(ctx, handle)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(6))
		})

		It("returns immediately on context cancellation", func() {
			tasks := []analyzer.Task{
				{TaskID: "t-1", TaskType: "classify", Payload: &structpb.Struct{}},
				{TaskID: "t-2", TaskType: "classify", Payload: &structpb.Struct{}},
			}
			req := analysis.CollectRequest{
				TaskType:     natsbus.TaskClassify,
				JobID:        "job-1",
				ExpectedIDs:  []string{"t-1", "t-2"},
				Tasks:        tasks,
				Timeout:      10 * time.Second,
				StageTimeout: time.Hour,
				MaxRetries:   3,
				Backoff:      10 * time.Millisecond,
				Dispatcher:   fakeDisp,
			}

			go func() {
				time.Sleep(20 * time.Millisecond)
				cancelFunc()
			}()

			handle, err := collector.PrepareCollect(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			results, err := collector.AwaitResults(ctx, handle)
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, context.Canceled)).To(BeTrue())
			Expect(results).To(HaveLen(0))
			Expect(fakeNATS.Subscriptions()[0].Unsubscribed()).To(BeTrue())
			Expect(progressSub(fakeNATS).Unsubscribed()).To(BeTrue())
		})

		It("releases a handle's subscriptions once, however often release is called", func() {
			req := analysis.CollectRequest{
				TaskType: natsbus.TaskClassify, JobID: "job-1", ExpectedIDs: []string{"t-1"},
				Tasks:   []analyzer.Task{{TaskID: "t-1", TaskType: "classify", Payload: &structpb.Struct{}}},
				Timeout: 1 * time.Second, StageTimeout: time.Hour, MaxRetries: 3, Backoff: 10 * time.Millisecond, Dispatcher: fakeDisp,
			}
			handle, err := collector.PrepareCollect(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			fakeNATS.Subscriptions()[0].UnsubscribeErr = natsbus.ErrConnectionClosed

			Expect(handle.ExportRelease()).To(MatchError(natsbus.ErrConnectionClosed))
			Expect(fakeNATS.Subscriptions()[0].Unsubscribed()).To(BeTrue())
			Expect(progressSub(fakeNATS).Unsubscribed()).To(BeTrue())
			Expect(handle.ExportRelease()).To(Succeed(), "a second release must not unsubscribe again")
		})

		It("ignores results with unknown task IDs", func() {
			tasks := []analyzer.Task{
				{TaskID: "t-1", TaskType: "classify", Payload: &structpb.Struct{}},
			}
			req := analysis.CollectRequest{
				TaskType:     natsbus.TaskClassify,
				JobID:        "job-1",
				ExpectedIDs:  []string{"t-1"},
				Tasks:        tasks,
				Timeout:      1 * time.Second,
				StageTimeout: time.Hour,
				MaxRetries:   3,
				Backoff:      10 * time.Millisecond,
				Dispatcher:   fakeDisp,
			}

			go func() {
				time.Sleep(10 * time.Millisecond)
				// Unknown task ID
				payload, _ := structpb.NewStruct(map[string]interface{}{"result": "unknown"})
				deliverResult(fakeNATS, "t-999", payload, "")
				// Expected task ID
				payload2, _ := structpb.NewStruct(map[string]interface{}{"result": "ok"})
				deliverResult(fakeNATS, "t-1", payload2, "")
			}()

			handle, err := collector.PrepareCollect(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			results, err := collector.AwaitResults(ctx, handle)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(1))
			Expect(results[0].TaskID).To(Equal("t-1"))
		})
	})
})
