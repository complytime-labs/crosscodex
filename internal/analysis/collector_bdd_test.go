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

// fakeSubscription implements natsbus.Subscription.
type fakeSubscription struct {
	unsubscribed bool
	drained      bool
}

func (f *fakeSubscription) Unsubscribe() error {
	f.unsubscribed = true
	return nil
}

func (f *fakeSubscription) Drain() error {
	f.drained = true
	return nil
}

// fakeCollectorNATSClient extends fakeNATSClient with Subscribe support.
type fakeCollectorNATSClient struct {
	*fakeNATSClient
	mu      sync.Mutex
	handler natsbus.MessageHandler
	sub     *fakeSubscription
}

func (f *fakeCollectorNATSClient) Subscribe(_ context.Context, _ string, handler natsbus.MessageHandler) (natsbus.Subscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handler = handler
	f.sub = &fakeSubscription{}
	return f.sub, nil
}

// deliverResult simulates a worker publishing a result message.
func (f *fakeCollectorNATSClient) deliverResult(ctx context.Context, taskID string, result proto.Message, errorMsg string) {
	f.mu.Lock()
	handler := f.handler
	f.mu.Unlock()

	if handler == nil {
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

	msg := &natsbus.Message{
		Subject: "fake.subject",
		Data:    data,
		Headers: headers,
	}

	_ = handler(ctx, msg)
}

var _ = Describe("NATSCollector", func() {
	var (
		fakeNATS   *fakeCollectorNATSClient
		fakeDisp   *fakeDispatcher
		collector  *analysis.NATSCollector
		ctx        context.Context
		cancelFunc context.CancelFunc
	)

	BeforeEach(func() {
		fakeNATS = &fakeCollectorNATSClient{
			fakeNATSClient: &fakeNATSClient{},
		}
		fakeDisp = &fakeDispatcher{}
		collector = analysis.NewNATSCollector(fakeNATS)
		ctx, cancelFunc = context.WithCancel(context.Background())
		ctx, _ = tenant.WithTenant(ctx, "test-tenant")
	})

	AfterEach(func() {
		cancelFunc()
	})

	Describe("Collect", func() {
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
				fakeNATS.deliverResult(ctx, "t-1", payload1, "")
				payload2, _ := structpb.NewStruct(map[string]interface{}{"result": "2"})
				fakeNATS.deliverResult(ctx, "t-2", payload2, "")
			}()

			results, err := collector.Collect(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(2))
			Expect(fakeNATS.sub.unsubscribed).To(BeTrue())
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
				fakeNATS.deliverResult(ctx, "t-1", nil, "worker error")
				time.Sleep(20 * time.Millisecond)
				// Second attempt: success
				payload, _ := structpb.NewStruct(map[string]interface{}{"result": "ok"})
				fakeNATS.deliverResult(ctx, "t-1", payload, "")
			}()

			results, err := collector.Collect(ctx, req)
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
				fakeNATS.deliverResult(ctx, "t-1", nil, "worker error")
				time.Sleep(20 * time.Millisecond)
				// Second attempt: error (exhausted)
				fakeNATS.deliverResult(ctx, "t-1", nil, "worker error")
			}()

			results, err := collector.Collect(ctx, req)
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
				fakeNATS.deliverResult(ctx, "t-1", payload, "")
				// t-2 never arrives
			}()

			results, err := collector.Collect(ctx, req)
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

			results, err := collector.Collect(ctx, req)
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
				fakeNATS.deliverResult(ctx, "t-999", payload, "")
				// Expected task ID
				payload2, _ := structpb.NewStruct(map[string]interface{}{"result": "ok"})
				fakeNATS.deliverResult(ctx, "t-1", payload2, "")
			}()

			results, err := collector.Collect(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(1))
			Expect(results[0].TaskID).To(Equal("t-1"))
		})
	})
})
