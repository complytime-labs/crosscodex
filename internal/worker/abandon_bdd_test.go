package worker_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/protobuf/proto"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/internal/worker"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/llmclient/llmclienttest"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/natsbus/natsbustest"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
)

// counterBy sums the data points of Int64Counter name whose attributes
// include every key/value in attrs.
func counterBy(tp *telemetrytest.TestProvider, name string, attrs map[string]string) int64 {
	m := telemetrytest.FindMetric(tp.GetMetrics(), name)
	if m == nil {
		return 0
	}
	sum, ok := m.Data.(metricdata.Sum[int64])
	ExpectWithOffset(1, ok).To(BeTrue(), "%s is %T, not an Int64Counter", name, m.Data)
	var total int64
points:
	for _, dp := range sum.DataPoints {
		for k, want := range attrs {
			if v, ok := dp.Attributes.Value(attribute.Key(k)); !ok || v.AsString() != want {
				continue points
			}
		}
		total += dp.Value
	}
	return total
}

var _ = Describe("Abandoned collections", func() {
	var (
		bus  *natsbustest.Fake
		llm  *llmclienttest.Fake
		tp   *telemetrytest.TestProvider
		logs *bytes.Buffer
		w    *worker.Worker
	)

	target := natsbus.AbandonedCollection{
		TenantID: "tenant-a", TaskType: natsbus.TaskRelate, JobID: "job-1", CollectionID: "6f1c0e2a-3b4d-4e5f-8a9b-0c1d2e3f4a5b",
	}

	BeforeEach(func() {
		bus = &natsbustest.Fake{}
		llm = newStubLLM()
		logs = &bytes.Buffer{}
		var err error
		tp, err = telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(tp.Shutdown(context.Background())).To(Succeed()) })
		w = worker.New(bus, llm, worker.WorkerConfig{MaxAbandonedCollections: 10, LLM: config.LLMConfig{DefaultModel: "default-model"}},
			worker.WithLogger(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))),
			worker.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()),
			worker.WithAbandonTTL(time.Hour))
		Expect(w.Start(context.Background())).To(Succeed())
		DeferCleanup(func() { Expect(w.Stop(context.Background())).To(Succeed()) })
	})

	// abandon delivers the control message the analysis collector publishes
	// for c, with the provenance tenant the real client would extract.
	abandon := func(c natsbus.AbandonedCollection) {
		subject, err := natsbus.AbandonedSubject(c)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		ExpectWithOffset(1, bus.Deliver(&natsbus.Message{
			Subject: subject, Metadata: natsbus.MessageMetadata{TenantID: c.TenantID},
		})).To(Succeed())
	}

	// deliverTask delivers a completion task of collection c (an embed task
	// when c.TaskType is TaskEmbed) through the worker's work subscription.
	deliverTask := func(c natsbus.AbandonedCollection, taskID string, retry int) {
		payload := BuildCompletionPayload("test", "gpt-4", 0.0, 256)
		if c.TaskType == natsbus.TaskEmbed {
			payload = BuildEmbeddingPayload("embed-model", "secret control text")
		}
		data, err := proto.Marshal(payload)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		subject, err := natsbus.WorkSubject(c.TenantID, c.TaskType, c.JobID)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		headers := map[string][]string{
			"X-Task-Id":     {taskID},
			"X-Task-Type":   {string(c.TaskType)},
			"X-Job-Id":      {c.JobID},
			"X-Retry-Count": {strconv.Itoa(retry)},
		}
		if c.CollectionID != "" {
			headers["X-Collection-Id"] = []string{c.CollectionID}
		}
		ExpectWithOffset(1, bus.Deliver(&natsbus.Message{
			Subject: subject, Data: data, Headers: headers,
			Metadata: natsbus.MessageMetadata{TenantID: c.TenantID},
		})).To(Succeed())
	}

	controlSub := func() *natsbustest.Sub {
		for _, s := range bus.Subscriptions() {
			if s.Subject == natsbus.AbandonedSubjectFilter {
				return s
			}
		}
		Fail("the worker has no abandon control subscription")
		return nil
	}

	It("subscribes to every tenant's abandons outside the work queue group, so every worker learns each one", func() {
		Expect(controlSub().Queue).To(BeEmpty())
	})

	It("drops a task of an abandoned collection before any LLM call, with no result, no tick, a count and a debug log", func() {
		abandon(target)

		deliverTask(target, "task-1", 0)

		Expect(llm.CompleteRequests()).To(BeEmpty())
		Expect(bus.Published()).To(BeEmpty(), "no result and no progress tick")
		Expect(counterBy(tp, "worker.tasks.dropped", map[string]string{"reason": "abandoned", "task_type": "relate"})).To(Equal(int64(1)))
		Expect(logs.String()).To(ContainSubstring("level=DEBUG"))
		Expect(logs.String()).To(ContainSubstring("task_id=task-1"))
		Expect(logs.String()).To(ContainSubstring("job_id=job-1"))
		Expect(logs.String()).NotTo(ContainSubstring("Test input."), "task content is never logged")
	})

	It("drops a redispatched task of an abandoned collection too", func() {
		abandon(target)

		deliverTask(target, "task-1", 2)

		Expect(llm.CompleteRequests()).To(BeEmpty())
		Expect(bus.Published()).To(BeEmpty())
	})

	It("drops an embedding task before the embedding call", func() {
		var embeds atomic.Int32
		llm.EmbedFunc = func(context.Context, *llmclient.EmbeddingRequest) (*llmclient.EmbeddingResponse, error) {
			embeds.Add(1)
			return nil, errors.New("must not be called")
		}
		embed := target
		embed.TaskType = natsbus.TaskEmbed
		abandon(embed)

		deliverTask(embed, "task-1", 0)

		Expect(embeds.Load()).To(BeZero())
		Expect(bus.Published()).To(BeEmpty())
		Expect(logs.String()).NotTo(ContainSubstring("secret control text"))
	})

	DescribeTable("runs tasks the abandon does not name",
		func(mutate func(*natsbus.AbandonedCollection)) {
			abandon(target)
			other := target
			mutate(&other)

			deliverTask(other, "task-1", 0)

			Expect(llm.CompleteRequests()).To(HaveLen(1))
			result, err := natsbus.ResultSubject(other.TenantID, other.TaskType, other.JobID)
			Expect(err).NotTo(HaveOccurred())
			Expect(bus.Published()).To(ContainElement(HaveField("Subject", result)))
			Expect(counterBy(tp, "worker.tasks.dropped", nil)).To(BeZero())
		},
		Entry("a resumed run of the same job and task type, which has a new collection ID",
			func(c *natsbus.AbandonedCollection) { c.CollectionID = "9a8b7c6d-5e4f-4a3b-9c2d-1e0f9a8b7c6d" }),
		Entry("another job", func(c *natsbus.AbandonedCollection) { c.JobID = "job-2" }),
		Entry("another tenant", func(c *natsbus.AbandonedCollection) { c.TenantID = "tenant-b" }),
		Entry("another task type of the job", func(c *natsbus.AbandonedCollection) { c.TaskType = natsbus.TaskRequires }),
		Entry("a task dispatched without a collection ID", func(c *natsbus.AbandonedCollection) { c.CollectionID = "" }),
	)

	DescribeTable("drops a malformed control message, logs why, counts it, and abandons nothing",
		func(msg func() *natsbus.Message, wantLog string) {
			Expect(controlSub().Handle(msg())).To(Succeed(), "a bad control message must never fail the subscription")

			Expect(logs.String()).To(ContainSubstring("level=WARN"))
			Expect(logs.String()).To(ContainSubstring("abandon control message dropped"))
			Expect(logs.String()).To(ContainSubstring(wantLog))
			Expect(counterBy(tp, "worker.errors", map[string]string{"error_category": "invalid_control_message"})).To(Equal(int64(1)))
			deliverTask(target, "task-1", 0)
			Expect(llm.CompleteRequests()).To(HaveLen(1), "the target collection's tasks still run")
		},
		Entry("a subject outside the control schema", func() *natsbus.Message {
			return &natsbus.Message{Subject: "crosscodex.control.tenant-a.relate.job-1.abandoned",
				Metadata: natsbus.MessageMetadata{TenantID: "tenant-a"}}
		}, "crosscodex.control.<tenant_id>.<task_type>.<job_id>.<collection_id>.abandoned"),
		Entry("an invalid tenant in the subject", func() *natsbus.Message {
			return &natsbus.Message{Subject: "crosscodex.control.BAD.relate.job-1." + "6f1c0e2a-3b4d-4e5f-8a9b-0c1d2e3f4a5b" + ".abandoned",
				Metadata: natsbus.MessageMetadata{TenantID: "BAD"}}
		}, "tenant validation"),
		Entry("a provenance tenant that differs from the subject's", func() *natsbus.Message {
			subject, _ := natsbus.AbandonedSubject(target)
			return &natsbus.Message{Subject: subject, Metadata: natsbus.MessageMetadata{TenantID: "tenant-b"}}
		}, "publish abandons only for the publisher's own tenant"),
		Entry("no provenance tenant", func() *natsbus.Message {
			subject, _ := natsbus.AbandonedSubject(target)
			return &natsbus.Message{Subject: subject}
		}, "publish abandons only for the publisher's own tenant"),
		Entry("a payload, which an abandon never carries", func() *natsbus.Message {
			subject, _ := natsbus.AbandonedSubject(target)
			return &natsbus.Message{Subject: subject, Data: []byte("task content"),
				Metadata: natsbus.MessageMetadata{TenantID: "tenant-a"}}
		}, "an abandon carries no payload"),
	)

	It("counts a task with missing headers as invalid, not as dropped, even when its collection was abandoned", func() {
		abandon(target)
		data, err := proto.Marshal(BuildCompletionPayload("test", "gpt-4", 0.0, 256))
		Expect(err).NotTo(HaveOccurred())
		subject, err := natsbus.WorkSubject(target.TenantID, target.TaskType, target.JobID)
		Expect(err).NotTo(HaveOccurred())

		Expect(bus.Deliver(&natsbus.Message{
			Subject: subject, Data: data,
			Headers: map[string][]string{
				"X-Task-Type": {string(target.TaskType)}, "X-Job-Id": {target.JobID},
				"X-Collection-Id": {target.CollectionID},
			},
			Metadata: natsbus.MessageMetadata{TenantID: target.TenantID},
		})).To(Succeed())

		Expect(counterBy(tp, "worker.errors", map[string]string{"error_category": "invalid_message"})).To(Equal(int64(1)))
		Expect(counterBy(tp, "worker.tasks.dropped", nil)).To(BeZero())
		Expect(llm.CompleteRequests()).To(BeEmpty())
	})

	It("reports how many collections it holds as abandoned", func() {
		abandon(target)
		second := target
		second.CollectionID = "9a8b7c6d-5e4f-4a3b-9c2d-1e0f9a8b7c6d"
		abandon(second)
		abandon(second) // a repeat does not grow the set

		gauge := telemetrytest.FindMetric(tp.GetMetrics(), "worker.abandoned_collections")
		Expect(gauge).NotTo(BeNil())
		Expect(telemetrytest.GaugeValue(gauge)).To(Equal(int64(2)))
	})
})

var _ = Describe("Abandon wiring", func() {
	It("refuses to start without an abandon TTL, and subscribes to nothing", func() {
		bus := &natsbustest.Fake{}
		w := worker.New(bus, newStubLLM(), worker.WorkerConfig{MaxAbandonedCollections: 10})

		err := w.Start(context.Background())

		Expect(err).To(MatchError(worker.ErrNoAbandonTTL))
		Expect(err).To(MatchError(ContainSubstring("worker.WithAbandonTTL")))
		Expect(err).To(MatchError(ContainSubstring("pipeline.stage_timeout")))
		Expect(bus.Subscriptions()).To(BeEmpty())
	})

	DescribeTable("refuses to start without a positive worker.max_abandoned_collections, and subscribes to nothing",
		func(limit int) {
			bus := &natsbustest.Fake{}
			w := worker.New(bus, newStubLLM(), worker.WorkerConfig{MaxAbandonedCollections: limit}, worker.WithAbandonTTL(time.Hour))

			err := w.Start(context.Background())

			Expect(err).To(MatchError(worker.ErrNoAbandonedCollectionsLimit))
			Expect(err).To(MatchError(ContainSubstring("worker.max_abandoned_collections")))
			Expect(bus.Subscriptions()).To(BeEmpty())
		},
		Entry("unset", 0),
		Entry("negative", -1),
	)

	It("remembers at most worker.max_abandoned_collections, forgetting the oldest", func() {
		bus := &natsbustest.Fake{}
		llm := newStubLLM()
		w := worker.New(bus, llm, worker.WorkerConfig{MaxAbandonedCollections: 1, LLM: config.LLMConfig{DefaultModel: "m"}},
			worker.WithAbandonTTL(time.Hour))
		Expect(w.Start(context.Background())).To(Succeed())
		DeferCleanup(func() { Expect(w.Stop(context.Background())).To(Succeed()) })
		older := natsbus.AbandonedCollection{TenantID: "tenant-a", TaskType: natsbus.TaskRelate, JobID: "job-1",
			CollectionID: "6f1c0e2a-3b4d-4e5f-8a9b-0c1d2e3f4a5b"}
		newer := older
		newer.CollectionID = "9a8b7c6d-5e4f-4a3b-9c2d-1e0f9a8b7c6d"
		for _, c := range []natsbus.AbandonedCollection{older, newer} {
			subject, err := natsbus.AbandonedSubject(c)
			Expect(err).NotTo(HaveOccurred())
			Expect(bus.Deliver(&natsbus.Message{Subject: subject, Metadata: natsbus.MessageMetadata{TenantID: c.TenantID}})).To(Succeed())
		}
		deliver := func(c natsbus.AbandonedCollection, taskID string) {
			data, err := proto.Marshal(BuildCompletionPayload("test", "gpt-4", 0.0, 256))
			Expect(err).NotTo(HaveOccurred())
			subject, err := natsbus.WorkSubject(c.TenantID, c.TaskType, c.JobID)
			Expect(err).NotTo(HaveOccurred())
			Expect(bus.Deliver(&natsbus.Message{Subject: subject, Data: data,
				Headers: map[string][]string{"X-Task-Id": {taskID}, "X-Task-Type": {string(c.TaskType)},
					"X-Job-Id": {c.JobID}, "X-Collection-Id": {c.CollectionID}},
				Metadata: natsbus.MessageMetadata{TenantID: c.TenantID}})).To(Succeed())
		}

		deliver(newer, "newer-task")
		deliver(older, "older-task")

		Expect(llm.CompleteRequests()).To(HaveLen(1), "only the evicted, older collection's task runs")
	})

	It("removes the control subscription when the work subscription fails", func() {
		bus := &natsbustest.Fake{
			QueueSubscribeFunc: func(context.Context, string, string, natsbus.MessageHandler) (natsbus.Subscription, error) {
				return nil, natsbus.ErrSubscribeFailed
			},
		}
		w := worker.New(bus, newStubLLM(), worker.WorkerConfig{MaxAbandonedCollections: 10}, worker.WithAbandonTTL(time.Hour))

		Expect(w.Start(context.Background())).To(MatchError(natsbus.ErrSubscribeFailed))

		subs := bus.Subscriptions()
		Expect(subs).To(HaveLen(1))
		Expect(subs[0].Subject).To(Equal(natsbus.AbandonedSubjectFilter))
		Expect(subs[0].Unsubscribed()).To(BeTrue())
		Expect(w.Stop(context.Background())).To(MatchError(worker.ErrNotStarted))
	})

	It("drains both subscriptions on Stop", func() {
		bus := &natsbustest.Fake{}
		w := worker.New(bus, newStubLLM(), worker.WorkerConfig{MaxAbandonedCollections: 10}, worker.WithAbandonTTL(time.Hour))
		Expect(w.Start(context.Background())).To(Succeed())

		Expect(w.Stop(context.Background())).To(Succeed())

		Expect(bus.Subscriptions()).To(HaveLen(2))
		for _, s := range bus.Subscriptions() {
			Expect(s.Drained()).To(BeTrue(), "subscription to %s left running", s.Subject)
		}
	})
})

var _ = Describe("Abandoned collections over a real NATS connection", func() {
	It("drops the abandoned collection's tasks and runs a resumed run's tasks of the same job", func() {
		bus, cleanup := testspecs.SetupTestNATS()
		DeferCleanup(cleanup)
		tp, err := telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(tp.Shutdown(context.Background())).To(Succeed()) })
		llm := newStubLLM()
		w := worker.New(bus, llm, worker.WorkerConfig{QueueGroup: "test-workers", MaxAbandonedCollections: 10, LLM: config.LLMConfig{DefaultModel: "m"}},
			worker.WithLogger(testspecs.GinkgoLogger()),
			worker.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()),
			worker.WithAbandonTTL(time.Hour))
		ctx := testspecs.SetupTenantContext("tenant-abc")
		Expect(w.Start(ctx)).To(Succeed())
		DeferCleanup(func() { Expect(w.Stop(ctx)).To(Succeed()) })

		abandoned := natsbus.AbandonedCollection{TenantID: "tenant-abc", TaskType: natsbus.TaskRelate, JobID: "job-1", CollectionID: "6f1c0e2a-3b4d-4e5f-8a9b-0c1d2e3f4a5b"}
		subject, err := natsbus.AbandonedSubject(abandoned)
		Expect(err).NotTo(HaveOccurred())
		Expect(bus.Publish(ctx, subject, nil)).To(Succeed())
		// The control and work subscriptions deliver independently, so wait
		// until the worker has recorded the abandon.
		Eventually(func() int64 {
			m := telemetrytest.FindMetric(tp.GetMetrics(), "worker.abandoned_collections")
			if m == nil {
				return 0
			}
			v, _ := telemetrytest.GaugeValue(m)
			return v
		}).Should(Equal(int64(1)))

		publish := func(collectionID, taskID string) {
			data, err := proto.Marshal(BuildCompletionPayload("test", "gpt-4", 0.0, 256))
			Expect(err).NotTo(HaveOccurred())
			work, err := natsbus.WorkSubject("tenant-abc", natsbus.TaskRelate, "job-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(bus.PublishWithHeaders(ctx, work, data, map[string][]string{
				"X-Task-Id": {taskID}, "X-Task-Type": {string(natsbus.TaskRelate)}, "X-Job-Id": {"job-1"},
				"X-Retry-Count": {"0"}, "X-Collection-Id": {collectionID},
			})).To(Succeed())
		}
		awaitDropped := WatchResult(ctx, bus, "tenant-abc", natsbus.TaskRelate, "job-1", "old-task")
		publish("6f1c0e2a-3b4d-4e5f-8a9b-0c1d2e3f4a5b", "old-task")
		awaitResumed := WatchResult(ctx, bus, "tenant-abc", natsbus.TaskRelate, "job-1", "new-task")
		publish("9a8b7c6d-5e4f-4a3b-9c2d-1e0f9a8b7c6d", "new-task")

		Expect(awaitResumed(5*time.Second)).NotTo(BeNil(), "the resumed run's task must run")
		Expect(awaitDropped(300*time.Millisecond)).To(BeNil(), "the abandoned run's task must not")
		Expect(llm.CompleteRequests()).To(HaveLen(1))
	})
})
