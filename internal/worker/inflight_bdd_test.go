package worker_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"github.com/complytime-labs/crosscodex/internal/worker"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/llmclient/llmclienttest"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/natsbus/natsbustest"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
)

// gatedLLM is an LLM fake whose completions block until release is closed,
// counting how many are running at once.
type gatedLLM struct {
	*llmclienttest.Fake
	release chan struct{}
	running atomic.Int32
	peak    atomic.Int32
}

func newGatedLLM() *gatedLLM {
	g := &gatedLLM{Fake: newStubLLM(), release: make(chan struct{})}
	answer := g.CompleteFunc
	g.CompleteFunc = func(ctx context.Context, req *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
		n := g.running.Add(1)
		for {
			p := g.peak.Load()
			if n <= p || g.peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-g.release
		g.running.Add(-1)
		return answer(ctx, req)
	}
	return g
}

var _ = Describe("Tasks in flight (worker.max_in_flight)", func() {
	var (
		bus  *natsbustest.Fake
		llm  *gatedLLM
		tp   *telemetrytest.TestProvider
		logs *bytes.Buffer
	)

	BeforeEach(func() {
		bus = &natsbustest.Fake{}
		llm = newGatedLLM()
		logs = &bytes.Buffer{}
		var err error
		tp, err = telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(tp.Shutdown(context.Background())).To(Succeed()) })
	})

	newWorker := func(maxInFlight int) *worker.Worker {
		return worker.New(bus, llm, worker.WorkerConfig{
			MaxAbandonedCollections: 10, MaxInFlight: maxInFlight,
			LLM: config.LLMConfig{DefaultModel: "default-model"},
		},
			worker.WithLogger(slog.New(slog.NewTextHandler(logs, nil))),
			worker.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()),
			worker.WithAbandonTTL(time.Hour))
	}

	// deliver hands task taskID to the worker's work subscription on its own
	// goroutine, as the NATS client's delivery goroutine would, and returns a
	// channel closed once the subscription callback returns.
	deliver := func(taskID string) <-chan struct{} {
		data, err := proto.Marshal(BuildCompletionPayload("test", "gpt-4", 0.0, 256))
		Expect(err).NotTo(HaveOccurred())
		subject, err := natsbus.WorkSubject("tenant-a", natsbus.TaskRelate, "job-1")
		Expect(err).NotTo(HaveOccurred())
		msg := &natsbus.Message{
			Subject: subject, Data: data,
			Headers: map[string][]string{
				"X-Task-Id": {taskID}, "X-Task-Type": {string(natsbus.TaskRelate)}, "X-Job-Id": {"job-1"},
			},
			Metadata: natsbus.MessageMetadata{TenantID: "tenant-a"},
		}
		returned := make(chan struct{})
		go func() {
			defer GinkgoRecover()
			defer close(returned)
			Expect(bus.Deliver(msg)).To(Succeed())
		}()
		return returned
	}

	// results counts the task results the worker published.
	results := func() int {
		n := 0
		for _, p := range bus.Published() {
			if strings.HasPrefix(p.Subject, "crosscodex.results.") {
				n++
			}
		}
		return n
	}

	// releaseOnce lets every gated completion finish. Cleanup calls it too,
	// so a spec that fails before releasing leaves no goroutine blocked.
	var releaseOnce func()
	BeforeEach(func() {
		releaseOnce = sync.OnceFunc(func() { close(llm.release) })
		DeferCleanup(func() { releaseOnce() })
	})

	// start starts a worker with maxInFlight slots and stops it at cleanup,
	// releasing the gated completions first so Stop's wait can end.
	start := func(maxInFlight int) *worker.Worker {
		w := newWorker(maxInFlight)
		Expect(w.Start(context.Background())).To(Succeed())
		DeferCleanup(func() {
			releaseOnce()
			Expect(w.Stop(context.Background())).To(Succeed())
		})
		return w
	}

	DescribeTable("Start refuses a worker without a positive max_in_flight, subscribing to nothing",
		func(maxInFlight int) {
			w := newWorker(maxInFlight)

			err := w.Start(context.Background())

			Expect(err).To(MatchError(worker.ErrNoInFlightLimit))
			Expect(err).To(MatchError(ContainSubstring("worker.max_in_flight")))
			Expect(err).To(MatchError(ContainSubstring("such as the default 1")))
			Expect(bus.Subscriptions()).To(BeEmpty())
		},
		Entry("zero", 0),
		Entry("negative", -1),
	)

	It("runs at most max_in_flight tasks at once and starts the waiting ones as slots free", func() {
		start(3)

		for _, id := range []string{"t1", "t2", "t3", "t4", "t5"} {
			deliver(id)
		}

		Eventually(llm.running.Load).Should(Equal(int32(3)))
		Consistently(llm.running.Load, 200*time.Millisecond).Should(Equal(int32(3)), "tasks beyond max_in_flight wait")

		releaseOnce()

		Eventually(results).Should(Equal(5))
		Expect(llm.CompleteRequests()).To(HaveLen(5))
		Expect(llm.peak.Load()).To(Equal(int32(3)))
	})

	It("runs one task at a time with max_in_flight 1", func() {
		start(1)

		deliver("t1")
		Eventually(llm.running.Load).Should(Equal(int32(1)))
		second := deliver("t2")

		Consistently(second, 200*time.Millisecond).ShouldNot(BeClosed(), "the second delivery waits for a slot")

		releaseOnce()

		Eventually(results).Should(Equal(2))
		Expect(llm.peak.Load()).To(Equal(int32(1)))
	})

	It("waits in Stop for in-flight tasks to finish and publish their results", func() {
		w := newWorker(2)
		Expect(w.Start(context.Background())).To(Succeed())
		deliver("t1")
		deliver("t2")
		Eventually(llm.running.Load).Should(Equal(int32(2)))

		stopped := make(chan error, 1)
		go func() { stopped <- w.Stop(context.Background()) }()
		Consistently(stopped, 200*time.Millisecond).ShouldNot(Receive(), "Stop returns only after the tasks finish")

		releaseOnce()

		Eventually(stopped).Should(Receive(BeNil()))
		Expect(results()).To(Equal(2))
	})

	It("returns from Stop when its context ends, saying how many tasks are still running", func() {
		w := newWorker(2)
		Expect(w.Start(context.Background())).To(Succeed())
		deliver("t1")
		deliver("t2")
		Eventually(llm.running.Load).Should(Equal(int32(2)))

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		err := w.Stop(ctx)

		Expect(err).To(MatchError(context.DeadlineExceeded))
		Expect(err).To(MatchError(ContainSubstring("2 tasks still running")))
		Expect(results()).To(BeZero(), "the running tasks have not published")
		Expect(w.Stop(context.Background())).To(MatchError(worker.ErrNotStarted), "the worker is stopped all the same")
	})

	It("does not start a task still waiting for a slot when Stop is called, counting it as dropped", func() {
		w := newWorker(1)
		Expect(w.Start(context.Background())).To(Succeed())
		deliver("t1")
		Eventually(llm.running.Load).Should(Equal(int32(1)))
		waiting := deliver("t2")
		Consistently(waiting, 100*time.Millisecond).ShouldNot(BeClosed())

		stopped := make(chan error, 1)
		go func() { stopped <- w.Stop(context.Background()) }()
		Eventually(waiting).Should(BeClosed(), "Stop releases the delivery waiting for a slot")

		releaseOnce()

		Eventually(stopped).Should(Receive(BeNil()))
		Expect(llm.CompleteRequests()).To(HaveLen(1), "only the task already running reached the LLM")
		Expect(results()).To(Equal(1))
		Expect(counterBy(tp, "worker.tasks.dropped", map[string]string{"reason": "worker_stopping", "task_type": "relate"})).To(Equal(int64(1)))
		Expect(logs.String()).To(ContainSubstring("task_id=t2"))
	})
})
