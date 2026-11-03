package worker_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/internal/worker"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/llmclient/llmclienttest"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/natsbus/natsbustest"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

func strPtr(s string) *string { return &s }

var _ = Describe("Handler", func() {
	var (
		bus     natsbus.Client
		cleanup func()
		llm     *llmclienttest.Fake
	)

	BeforeEach(func() {
		bus, cleanup = testspecs.SetupTestNATS()
		llm = newStubLLM()
	})

	AfterEach(func() {
		cleanup()
	})

	newWorker := func() *worker.Worker {
		return worker.New(bus, llm, worker.WorkerConfig{
			MaxAbandonedCollections: 10,
			QueueGroup:              "test-workers",
			LLM: config.LLMConfig{
				DefaultModel:   "default-model",
				EmbeddingModel: "default-embed",
			},
		}, worker.WithLogger(testspecs.GinkgoLogger()), worker.WithAbandonTTL(time.Hour))
	}

	Describe("Completion tasks", func() {
		DescribeTable("routes all completion task types",
			func(taskType natsbus.TaskType) {
				ctx := testspecs.SetupTenantContext("tenant-abc")
				w := newWorker()
				Expect(w.Start(ctx)).To(Succeed())
				defer func() { Expect(w.Stop(ctx)).To(Succeed()) }()

				payload := BuildCompletionPayload("test", "gpt-4", 0.0, 256)
				awaitResult := WatchResult(ctx, bus, "tenant-abc", taskType, "job-1", "task-1")
				PublishWorkTask(ctx, bus, "tenant-abc", taskType, "job-1", "task-1", payload)

				result := awaitResult(5 * time.Second)
				Expect(result).NotTo(BeNil())
				Expect(result.Fields["response"].GetStringValue()).NotTo(BeEmpty())
			},
			Entry("classify", natsbus.TaskClassify),
			Entry("relate", natsbus.TaskRelate),
			Entry("requires", natsbus.TaskRequires),
			Entry("artifacts", natsbus.TaskArtifacts),
		)
	})

	Describe("LLM call failure", func() {
		It("publishes error result with X-Error header", func() {
			llm.CompleteFunc = func(_ context.Context, _ *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
				return nil, errors.New("gateway timeout")
			}

			ctx := testspecs.SetupTenantContext("tenant-abc")
			w := newWorker()
			Expect(w.Start(ctx)).To(Succeed())
			defer func() { Expect(w.Stop(ctx)).To(Succeed()) }()

			payload := BuildCompletionPayload("test", "gpt-4", 0.0, 256)
			awaitError := WatchErrorResult(ctx, bus, "tenant-abc", natsbus.TaskClassify, "job-1", "task-1")
			PublishWorkTask(ctx, bus, "tenant-abc", natsbus.TaskClassify, "job-1", "task-1", payload)

			// Wait for the error result
			errorResult := awaitError(5 * time.Second)
			Expect(errorResult).To(Equal("llm_error"))
		})
	})

	Describe("Default model resolution", func() {
		It("uses tenant default model when payload model is empty", func() {
			var receivedModel string
			llm.CompleteFunc = func(_ context.Context, req *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
				receivedModel = req.Model
				return &llmclient.CompletionResponse{
					Model:   req.Model,
					Choices: []llmclient.CompletionChoice{{Message: llmclient.ChatMessage{Content: "ok"}}},
					Usage:   llmclient.TokenUsage{TotalTokens: 10},
				}, nil
			}

			ctx := testspecs.SetupTenantContext("tenant-abc")
			w := worker.New(bus, llm, worker.WorkerConfig{
				MaxAbandonedCollections: 10,
				QueueGroup:              "test-workers",
				LLM: config.LLMConfig{
					DefaultModel:   "global-model",
					EmbeddingModel: "global-embed",
					TenantOverrides: map[string]config.LLMOverride{
						"tenant-abc": {DefaultModel: strPtr("tenant-specific-model")},
					},
				},
			}, worker.WithLogger(testspecs.GinkgoLogger()), worker.WithAbandonTTL(time.Hour))
			Expect(w.Start(ctx)).To(Succeed())
			defer func() { Expect(w.Stop(ctx)).To(Succeed()) }()

			payload := BuildCompletionPayload("test", "", 0.0, 256)
			awaitResult := WatchResult(ctx, bus, "tenant-abc", natsbus.TaskClassify, "job-1", "task-1")
			PublishWorkTask(ctx, bus, "tenant-abc", natsbus.TaskClassify, "job-1", "task-1", payload)

			result := awaitResult(5 * time.Second)
			Expect(result).NotTo(BeNil())
			Expect(receivedModel).To(Equal("tenant-specific-model"))
		})
	})

	Describe("Default embedding model resolution", func() {
		It("uses tenant embedding model when payload model is empty", func() {
			var receivedModel string
			llm.EmbedFunc = func(_ context.Context, req *llmclient.EmbeddingRequest) (*llmclient.EmbeddingResponse, error) {
				receivedModel = req.Model
				return &llmclient.EmbeddingResponse{
					Data:  []llmclient.EmbeddingData{{Embedding: []float32{0.1, 0.2, 0.3}}},
					Model: req.Model,
					Usage: llmclient.EmbeddingUsage{TotalTokens: 5},
				}, nil
			}

			ctx := testspecs.SetupTenantContext("tenant-abc")
			w := worker.New(bus, llm, worker.WorkerConfig{
				MaxAbandonedCollections: 10,
				QueueGroup:              "test-workers",
				LLM: config.LLMConfig{
					DefaultModel:   "global-model",
					EmbeddingModel: "global-embed",
					TenantOverrides: map[string]config.LLMOverride{
						"tenant-abc": {EmbeddingModel: strPtr("tenant-embed-model")},
					},
				},
			}, worker.WithLogger(testspecs.GinkgoLogger()), worker.WithAbandonTTL(time.Hour))
			Expect(w.Start(ctx)).To(Succeed())
			defer func() { Expect(w.Stop(ctx)).To(Succeed()) }()

			payload := BuildEmbeddingPayload("", "test text")
			awaitResult := WatchResult(ctx, bus, "tenant-abc", natsbus.TaskEmbed, "job-1", "task-1")
			PublishWorkTask(ctx, bus, "tenant-abc", natsbus.TaskEmbed, "job-1", "task-1", payload)

			result := awaitResult(5 * time.Second)
			Expect(result).NotTo(BeNil())
			Expect(receivedModel).To(Equal("tenant-embed-model"))
		})
	})

	Describe("Unknown task type", func() {
		It("publishes unsupported_task_type error", func() {
			ctx := testspecs.SetupTenantContext("tenant-abc")
			w := newWorker()
			Expect(w.Start(ctx)).To(Succeed())
			defer func() { Expect(w.Stop(ctx)).To(Succeed()) }()

			payload := BuildCompletionPayload("test", "gpt-4", 0.0, 256)
			awaitError := WatchErrorResult(ctx, bus, "tenant-abc", "unknown_type", "job-1", "task-1")
			PublishWorkTask(ctx, bus, "tenant-abc", "unknown_type", "job-1", "task-1", payload)

			errorResult := awaitError(5 * time.Second)
			Expect(errorResult).To(Equal("unsupported_task_type"))
		})
	})

	Describe("Missing required headers", func() {
		It("discards messages with no task ID and no task type", func() {
			ctx := testspecs.SetupTenantContext("tenant-abc")
			w := newWorker()
			Expect(w.Start(ctx)).To(Succeed())
			defer func() { Expect(w.Stop(ctx)).To(Succeed()) }()

			subject, err := natsbus.WorkSubject("tenant-abc", natsbus.TaskClassify, "job-1")
			Expect(err).NotTo(HaveOccurred())

			payload := BuildCompletionPayload("test", "gpt-4", 0.0, 256)
			data, err := proto.Marshal(payload)
			Expect(err).NotTo(HaveOccurred())

			// Subscribe to result subject to verify no error result arrives
			resultSubject, err := natsbus.ResultSubject("tenant-abc", natsbus.TaskClassify, "job-1")
			Expect(err).NotTo(HaveOccurred())

			var received int32
			sub, err := bus.Subscribe(ctx, resultSubject, func(_ context.Context, _ *natsbus.Message) error {
				atomic.AddInt32(&received, 1)
				return nil
			})
			Expect(err).NotTo(HaveOccurred())
			defer func() { Expect(sub.Unsubscribe()).To(Succeed()) }()

			// Publish with no X-Task-Id and no X-Task-Type headers
			err = bus.PublishWithHeaders(ctx, subject, data, map[string][]string{
				"X-Job-Id": {"job-1"},
			})
			Expect(err).NotTo(HaveOccurred())

			// Assert no error result published (worker discards, doesn't crash)
			Consistently(func() int32 {
				return atomic.LoadInt32(&received)
			}, 500*time.Millisecond, 50*time.Millisecond).Should(Equal(int32(0)),
				"no result should be published for messages missing required headers")
		})
	})

	Describe("Malformed payload", func() {
		It("publishes invalid_payload error for corrupt proto data", func() {
			ctx := testspecs.SetupTenantContext("tenant-abc")
			w := newWorker()
			Expect(w.Start(ctx)).To(Succeed())
			defer func() { Expect(w.Stop(ctx)).To(Succeed()) }()

			subject, err := natsbus.WorkSubject("tenant-abc", natsbus.TaskClassify, "job-1")
			Expect(err).NotTo(HaveOccurred())

			headers := map[string][]string{
				"X-Task-Id":   {"task-bad"},
				"X-Task-Type": {"classify"},
				"X-Job-Id":    {"job-1"},
			}
			awaitError := WatchErrorResult(ctx, bus, "tenant-abc", natsbus.TaskClassify, "job-1", "task-bad")
			// Publish garbage bytes that won't unmarshal as structpb.Struct
			err = bus.PublishWithHeaders(ctx, subject, []byte("not-proto-data"), headers)
			Expect(err).NotTo(HaveOccurred())

			errorResult := awaitError(5 * time.Second)
			Expect(errorResult).To(Equal("invalid_payload"))
		})
	})

	Describe("Embedding LLM failure", func() {
		It("publishes llm_error for embedding failures", func() {
			llm.EmbedFunc = func(_ context.Context, _ *llmclient.EmbeddingRequest) (*llmclient.EmbeddingResponse, error) {
				return nil, errors.New("embedding service unavailable")
			}

			ctx := testspecs.SetupTenantContext("tenant-abc")
			w := newWorker()
			Expect(w.Start(ctx)).To(Succeed())
			defer func() { Expect(w.Stop(ctx)).To(Succeed()) }()

			payload := BuildEmbeddingPayload("text-embedding-3-small", "test text")
			awaitError := WatchErrorResult(ctx, bus, "tenant-abc", natsbus.TaskEmbed, "job-1", "task-1")
			PublishWorkTask(ctx, bus, "tenant-abc", natsbus.TaskEmbed, "job-1", "task-1", payload)

			errorResult := awaitError(5 * time.Second)
			Expect(errorResult).To(Equal("llm_error"))
		})
	})

	Describe("Queue group distribution", func() {
		It("distributes tasks across multiple workers", func() {
			ctx := testspecs.SetupTenantContext("tenant-abc")

			// Subscribe to results before starting workers
			subject, err := natsbus.ResultSubject("tenant-abc", natsbus.TaskClassify, "job-1")
			Expect(err).NotTo(HaveOccurred())

			results := make(map[string]*structpb.Struct, 4)
			var resultsMu sync.Mutex
			sub, err := bus.Subscribe(ctx, subject, func(_ context.Context, msg *natsbus.Message) error {
				if vals := msg.Headers["X-Task-Id"]; len(vals) > 0 {
					taskID := vals[0]
					s := &structpb.Struct{}
					if err := proto.Unmarshal(msg.Data, s); err == nil {
						resultsMu.Lock()
						results[taskID] = s
						resultsMu.Unlock()
					}
				}
				return nil
			})
			Expect(err).NotTo(HaveOccurred())
			defer func() { Expect(sub.Unsubscribe()).To(Succeed()) }()

			w1 := worker.New(bus, llm, worker.WorkerConfig{MaxAbandonedCollections: 10, QueueGroup: "shared-group", LLM: config.LLMConfig{DefaultModel: "m"}}, worker.WithLogger(testspecs.GinkgoLogger()), worker.WithAbandonTTL(time.Hour))
			w2 := worker.New(bus, llm, worker.WorkerConfig{MaxAbandonedCollections: 10, QueueGroup: "shared-group", LLM: config.LLMConfig{DefaultModel: "m"}}, worker.WithLogger(testspecs.GinkgoLogger()), worker.WithAbandonTTL(time.Hour))
			Expect(w1.Start(ctx)).To(Succeed())
			Expect(w2.Start(ctx)).To(Succeed())
			defer func() { Expect(w1.Stop(ctx)).To(Succeed()) }()
			defer func() { Expect(w2.Stop(ctx)).To(Succeed()) }()

			// Publish 4 tasks to verify distribution
			for i := 0; i < 4; i++ {
				payload := BuildCompletionPayload("test", "m", 0.0, 256)
				taskID := fmt.Sprintf("task-%d", i)
				PublishWorkTask(ctx, bus, "tenant-abc", natsbus.TaskClassify, "job-1", taskID, payload)
			}

			// Wait for all results
			Eventually(func() int {
				resultsMu.Lock()
				defer resultsMu.Unlock()
				return len(results)
			}, 2*time.Second, 50*time.Millisecond).Should(Equal(4), "all tasks should complete")
		})
	})
})

var _ = Describe("Handler tenant validation", func() {
	var (
		logs      *bytes.Buffer
		w         *worker.Worker
		bus       *natsbustest.Fake
		llmCalled bool
	)

	BeforeEach(func() {
		logs = &bytes.Buffer{}
		bus = &natsbustest.Fake{}
		llmCalled = false
		stubLLM := &llmclienttest.Fake{
			CompleteFunc: func(_ context.Context, _ *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
				llmCalled = true
				return nil, errors.New("LLM must not be called for a rejected tenant")
			},
			EmbedFunc: func(_ context.Context, _ *llmclient.EmbeddingRequest) (*llmclient.EmbeddingResponse, error) {
				llmCalled = true
				return nil, errors.New("LLM must not be called for a rejected tenant")
			},
		}
		w = worker.New(bus, stubLLM, worker.WorkerConfig{MaxAbandonedCollections: 10},
			worker.WithLogger(slog.New(slog.NewTextHandler(logs, nil))), worker.WithAbandonTTL(time.Hour))
	})

	DescribeTable("discards the task and logs the tenant ID rule",
		func(tenantID, wantMsg string) {
			msg := &natsbus.Message{Metadata: natsbus.MessageMetadata{TenantID: tenantID}}
			// The ctx carries a valid tenant so a regressed publish would be
			// accepted and recorded by the Fake, not rejected unrecorded.
			ctx, err := tenant.WithTenant(context.Background(), "tenant-a")
			Expect(err).NotTo(HaveOccurred())
			Expect(worker.ExportHandleMessage(w, ctx, msg)).To(Succeed())
			Expect(logs.String()).To(ContainSubstring(wantMsg))
			Expect(logs.String()).To(ContainSubstring(natsbus.HeaderTenantID))
			Expect(logs.String()).To(ContainSubstring(tenant.IDRule))
			Expect(llmCalled).To(BeFalse(), "a rejected tenant must never reach the LLM client")
			Expect(bus.Published()).To(BeEmpty(), "a rejected tenant must never publish a result")
			Expect(bus.Subscriptions()).To(BeEmpty(), "handling a message must not subscribe")
		},
		Entry("missing tenant ID", "", "message missing tenant ID"),
		Entry("53-character tenant ID", "a"+strings.Repeat("b", 52), "invalid tenant ID"),
		Entry("uppercase tenant ID", "Tenant-1", "invalid tenant ID"),
	)
})

var _ = Describe("Handler worker progress", func() {
	var (
		logs *bytes.Buffer
		bus  *natsbustest.Fake
		llm  *llmclienttest.Fake
		tp   *telemetrytest.TestProvider
		w    *worker.Worker
	)

	resultSubject := "crosscodex.results.tenant-a.classify.job-1"

	BeforeEach(func() {
		logs = &bytes.Buffer{}
		bus = &natsbustest.Fake{}
		llm = newStubLLM()
		var err error
		tp, err = telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(tp.Shutdown(context.Background())).To(Succeed()) })
		w = worker.New(bus, llm, worker.WorkerConfig{MaxAbandonedCollections: 10, LLM: config.LLMConfig{DefaultModel: "default-model"}},
			worker.WithLogger(slog.New(slog.NewTextHandler(logs, nil))),
			worker.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()), worker.WithAbandonTTL(time.Hour))
	})

	classifyTask := func() *natsbus.Message {
		data, err := proto.Marshal(BuildCompletionPayload("test", "gpt-4", 0.0, 256))
		Expect(err).NotTo(HaveOccurred())
		return &natsbus.Message{
			Subject: "crosscodex.work.tenant-a.classify.job-1",
			Data:    data,
			Headers: map[string][]string{
				"X-Task-Id":   {"task-1"},
				"X-Task-Type": {string(natsbus.TaskClassify)},
				"X-Job-Id":    {"job-1"},
			},
			Metadata: natsbus.MessageMetadata{TenantID: "tenant-a"},
		}
	}

	tick := natsbustest.Publication{Subject: natsbus.WorkerProgressSubject}

	It("publishes a bare progress tick after the success result", func() {
		Expect(worker.ExportHandleMessage(w, context.Background(), classifyTask())).To(Succeed())

		published := bus.Published()
		Expect(published).To(HaveLen(2))
		Expect(published[0].Subject).To(Equal(resultSubject))
		Expect(published[0].Headers).NotTo(HaveKey("X-Error"))
		Expect(published[1]).To(Equal(tick))
	})

	It("publishes a bare progress tick after the error result", func() {
		llm.CompleteFunc = func(context.Context, *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
			return nil, errors.New("gateway timeout")
		}
		Expect(worker.ExportHandleMessage(w, context.Background(), classifyTask())).To(Succeed())

		published := bus.Published()
		Expect(published).To(HaveLen(2))
		Expect(published[0].Subject).To(Equal(resultSubject))
		Expect(published[0].Headers).To(HaveKeyWithValue("X-Error", []string{"llm_error"}))
		Expect(published[1]).To(Equal(tick))
	})

	It("publishes no tick when the result could not be published", func() {
		bus.PublishWithHeadersFunc = func(context.Context, string, []byte, map[string][]string) error {
			return natsbus.ErrPublishFailed
		}
		Expect(worker.ExportHandleMessage(w, context.Background(), classifyTask())).To(Succeed())

		published := bus.Published()
		Expect(published).To(HaveLen(1))
		Expect(published[0].Subject).To(Equal(resultSubject))
	})

	It("keeps the result and the task's success when the tick fails, and logs and counts the failure", func() {
		bus.PublishWorkerProgressErr = natsbus.ErrPublishFailed

		Expect(worker.ExportHandleMessage(w, context.Background(), classifyTask())).To(Succeed())

		published := bus.Published()
		Expect(published).To(HaveLen(2))
		Expect(published[0].Subject).To(Equal(resultSubject))
		Expect(published[0].Headers).NotTo(HaveKey("X-Error"))

		Expect(logs.String()).To(ContainSubstring("level=WARN"))
		Expect(logs.String()).To(ContainSubstring("failed to publish worker progress"))
		Expect(logs.String()).To(ContainSubstring("analysis.engine.task_timeout"))
		Expect(strings.Count(logs.String(), "failed to publish worker progress")).To(Equal(1))

		errs := telemetrytest.FindMetric(tp.GetMetrics(), "worker.errors")
		Expect(errs).NotTo(BeNil())
		Expect(errs.Data).To(BeAssignableToTypeOf(metricdata.Sum[int64]{}))
		var progressErrors int64
		for _, dp := range errs.Data.(metricdata.Sum[int64]).DataPoints {
			if v, ok := dp.Attributes.Value("error_category"); ok && v.AsString() == "progress_publish_error" {
				progressErrors += dp.Value
			}
		}
		Expect(progressErrors).To(Equal(int64(1)))
	})
})
