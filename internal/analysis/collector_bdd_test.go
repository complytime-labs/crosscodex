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
	taskID     string
	retryCount int
}

func (f *fakeDispatcher) Dispatch(_ context.Context, _ []analyzer.Task, _ natsbus.TaskType, _ string) error {
	return nil
}

func (f *fakeDispatcher) Redispatch(ctx context.Context, task analyzer.Task, taskType natsbus.TaskType, jobID string, retryCount int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.redispatches = append(f.redispatches, redispatchCall{taskID: task.TaskID, retryCount: retryCount})
	if f.redispatchFn != nil {
		return f.redispatchFn(ctx, task, taskType, jobID, retryCount)
	}
	return nil
}

// deliverResult simulates a worker publishing a result message on the subject
// the collector most recently subscribed to. It delivers nothing when the
// collector has not subscribed yet.
func deliverResult(bus *natsbustest.Fake, taskID string, result proto.Message, errorMsg string) {
	subs := bus.Subscriptions()
	if len(subs) == 0 {
		return
	}

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
		Subject: subs[len(subs)-1].Subject,
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
				TaskType:    natsbus.TaskClassify,
				JobID:       "job-1",
				ExpectedIDs: []string{"t-1", "t-2"},
				Tasks:       tasks,
				Timeout:     1 * time.Second,
				MaxRetries:  3,
				Backoff:     10 * time.Millisecond,
				Dispatcher:  fakeDisp,
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
				TaskType:    natsbus.TaskClassify,
				JobID:       "job-1",
				ExpectedIDs: []string{"t-1"},
				Tasks:       tasks,
				Timeout:     1 * time.Second,
				MaxRetries:  2,
				Backoff:     1 * time.Millisecond,
				Dispatcher:  fakeDisp,
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
				TaskType:    natsbus.TaskClassify,
				JobID:       "job-1",
				ExpectedIDs: []string{"t-1"},
				Tasks:       tasks,
				Timeout:     1 * time.Second,
				MaxRetries:  2,
				Backoff:     1 * time.Millisecond,
				Dispatcher:  analysis.NewNATSDispatcher(dispatchBus),
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
				TaskType:    natsbus.TaskClassify,
				JobID:       "job-1",
				ExpectedIDs: []string{"t-1"},
				Tasks:       tasks,
				Timeout:     1 * time.Second,
				MaxRetries:  1,
				Backoff:     1 * time.Millisecond,
				Dispatcher:  fakeDisp,
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
				TaskType:    natsbus.TaskClassify,
				JobID:       "job-1",
				ExpectedIDs: []string{"t-1", "t-2"},
				Tasks:       tasks,
				Timeout:     50 * time.Millisecond,
				MaxRetries:  3,
				Backoff:     10 * time.Millisecond,
				Dispatcher:  fakeDisp,
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
			Expect(errors.Is(err, analysis.ErrTaskTimeout)).To(BeTrue())
			Expect(results).To(HaveLen(1)) // Partial results returned
		})

		It("returns immediately on context cancellation", func() {
			tasks := []analyzer.Task{
				{TaskID: "t-1", TaskType: "classify", Payload: &structpb.Struct{}},
				{TaskID: "t-2", TaskType: "classify", Payload: &structpb.Struct{}},
			}
			req := analysis.CollectRequest{
				TaskType:    natsbus.TaskClassify,
				JobID:       "job-1",
				ExpectedIDs: []string{"t-1", "t-2"},
				Tasks:       tasks,
				Timeout:     10 * time.Second,
				MaxRetries:  3,
				Backoff:     10 * time.Millisecond,
				Dispatcher:  fakeDisp,
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
		})

		It("ignores results with unknown task IDs", func() {
			tasks := []analyzer.Task{
				{TaskID: "t-1", TaskType: "classify", Payload: &structpb.Struct{}},
			}
			req := analysis.CollectRequest{
				TaskType:    natsbus.TaskClassify,
				JobID:       "job-1",
				ExpectedIDs: []string{"t-1"},
				Tasks:       tasks,
				Timeout:     1 * time.Second,
				MaxRetries:  3,
				Backoff:     10 * time.Millisecond,
				Dispatcher:  fakeDisp,
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
