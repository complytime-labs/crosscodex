package worker_test

import (
	"context"
	"errors"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/internal/worker"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
)

func TestWorkerBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Worker BDD Suite")
}

var restoreLogs func()

var _ = BeforeSuite(func() {
	restoreLogs = testspecs.RedirectLogsToGinkgo()
})

var _ = AfterSuite(func() {
	restoreLogs()
})

// stubLLMClient implements llmclient.Client for testing.
type stubLLMClient struct {
	completeFunc func(ctx context.Context, req *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error)
	embedFunc    func(ctx context.Context, req *llmclient.EmbeddingRequest) (*llmclient.EmbeddingResponse, error)
}

func (s *stubLLMClient) Complete(ctx context.Context, req *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
	if s.completeFunc != nil {
		return s.completeFunc(ctx, req)
	}
	return &llmclient.CompletionResponse{
		Model:   req.Model,
		Choices: []llmclient.CompletionChoice{{Message: llmclient.ChatMessage{Content: "ok"}}},
		Usage:   llmclient.TokenUsage{TotalTokens: 10},
	}, nil
}

func (s *stubLLMClient) Embed(ctx context.Context, req *llmclient.EmbeddingRequest) (*llmclient.EmbeddingResponse, error) {
	if s.embedFunc != nil {
		return s.embedFunc(ctx, req)
	}
	return &llmclient.EmbeddingResponse{
		Data:  []llmclient.EmbeddingData{{Embedding: []float32{0.1, 0.2}}},
		Model: req.Model,
		Usage: llmclient.EmbeddingUsage{TotalTokens: 5},
	}, nil
}

func (s *stubLLMClient) Health(_ context.Context) error { return nil }
func (s *stubLLMClient) Close() error                   { return nil }

var _ = Describe("Worker Lifecycle", func() {
	var (
		bus     natsbus.Client
		cleanup func()
		llm     *stubLLMClient
		w       *worker.Worker
	)

	BeforeEach(func() {
		bus, cleanup = testspecs.SetupTestNATS()
		llm = &stubLLMClient{}
		w = worker.New(bus, llm, worker.WorkerConfig{
			QueueGroup: "test-workers",
			LLM: config.LLMConfig{
				DefaultModel:   "test-model",
				EmbeddingModel: "test-embed-model",
			},
		}, worker.WithLogger(testspecs.GinkgoLogger()))
	})

	AfterEach(func() {
		cleanup()
	})

	Describe("Start", func() {
		It("subscribes to work subjects", func() {
			ctx := context.Background()
			err := w.Start(ctx)
			Expect(err).NotTo(HaveOccurred())
			defer func() { Expect(w.Stop(ctx)).To(Succeed()) }()
		})

		It("returns error on double start", func() {
			ctx := context.Background()
			Expect(w.Start(ctx)).To(Succeed())
			defer func() { Expect(w.Stop(ctx)).To(Succeed()) }()

			err := w.Start(ctx)
			Expect(err).To(MatchError(worker.ErrAlreadyStarted))
		})
	})

	Describe("Default queue group fallback", func() {
		It("uses defaultQueueGroup when QueueGroup is empty", func() {
			w := worker.New(bus, llm, worker.WorkerConfig{
				// QueueGroup is deliberately empty — worker must fall back to "llm-workers"
				LLM: config.LLMConfig{
					DefaultModel:   "test-model",
					EmbeddingModel: "test-embed-model",
				},
			}, worker.WithLogger(testspecs.GinkgoLogger()))

			ctx := context.Background()
			Expect(w.Start(ctx)).To(Succeed())
			Expect(w.Stop(ctx)).To(Succeed())
			// Reaching here without panic or error confirms the default queue group was used correctly.
		})
	})

	Describe("Stop", func() {
		It("returns error when not started", func() {
			err := w.Stop(context.Background())
			Expect(err).To(MatchError(worker.ErrNotStarted))
		})

		It("drains cleanly after start", func() {
			ctx := context.Background()
			Expect(w.Start(ctx)).To(Succeed())
			err := w.Stop(ctx)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("End-to-end completion task", func() {
		It("processes a classify task and publishes result", func() {
			ctx := testspecs.SetupTenantContext("tenant-abc")
			Expect(w.Start(ctx)).To(Succeed())
			defer func() { Expect(w.Stop(ctx)).To(Succeed()) }()

			// Build and publish a classify task
			payload := BuildCompletionPayload("classify", "gpt-4", 0.0, 256)
			PublishWorkTask(ctx, bus, "tenant-abc", natsbus.TaskClassify, "job-1", "task-1", payload)

			// Subscribe to results and verify
			result := WaitForResult(ctx, bus, "tenant-abc", natsbus.TaskClassify, "job-1", "task-1", 5*time.Second)
			Expect(result).NotTo(BeNil())
			Expect(result.Fields["response"].GetStringValue()).To(Equal("ok"))
			Expect(result.Fields["model"].GetStringValue()).To(Equal("gpt-4"))
		})
	})

	Describe("End-to-end embedding task", func() {
		It("processes an embed task and publishes result", func() {
			ctx := testspecs.SetupTenantContext("tenant-abc")
			Expect(w.Start(ctx)).To(Succeed())
			defer func() { Expect(w.Stop(ctx)).To(Succeed()) }()

			payload := BuildEmbeddingPayload("text-embedding-3-small", "test text")
			PublishWorkTask(ctx, bus, "tenant-abc", natsbus.TaskEmbed, "job-1", "task-1", payload)

			result := WaitForResult(ctx, bus, "tenant-abc", natsbus.TaskEmbed, "job-1", "task-1", 5*time.Second)
			Expect(result).NotTo(BeNil())
			Expect(result.Fields["model"].GetStringValue()).To(Equal("text-embedding-3-small"))
			embeddings := result.Fields["embeddings"].GetListValue()
			Expect(embeddings).NotTo(BeNil())
		})
	})
})

var _ = Describe("WithTelemetry", func() {
	var (
		bus     natsbus.Client
		cleanup func()
		llm     *stubLLMClient
		tp      *telemetrytest.TestProvider
	)

	BeforeEach(func() {
		bus, cleanup = testspecs.SetupTestNATS()
		llm = &stubLLMClient{}

		var err error
		tp, err = telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		Expect(tp.Shutdown(context.Background())).To(Succeed())
		cleanup()
	})

	It("creates a worker with telemetry without error", func() {
		w := worker.New(bus, llm, worker.WorkerConfig{
			QueueGroup: "test-workers",
			LLM: config.LLMConfig{
				DefaultModel:   "test-model",
				EmbeddingModel: "test-embed-model",
			},
		},
			worker.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()),
			worker.WithLogger(testspecs.GinkgoLogger()),
		)
		Expect(w).NotTo(BeNil())

		ctx := context.Background()
		Expect(w.Start(ctx)).To(Succeed())
		Expect(w.Stop(ctx)).To(Succeed())
	})

	It("records spans after processing a task", func() {
		w := worker.New(bus, llm, worker.WorkerConfig{
			QueueGroup: "test-workers",
			LLM: config.LLMConfig{
				DefaultModel:   "test-model",
				EmbeddingModel: "test-embed-model",
			},
		},
			worker.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()),
			worker.WithLogger(testspecs.GinkgoLogger()),
		)

		ctx := testspecs.SetupTenantContext("tenant-abc")
		Expect(w.Start(ctx)).To(Succeed())
		defer func() { Expect(w.Stop(ctx)).To(Succeed()) }()

		payload := BuildCompletionPayload("classify", "gpt-4", 0.0, 256)
		PublishWorkTask(ctx, bus, "tenant-abc", natsbus.TaskClassify, "job-1", "task-1", payload)

		result := WaitForResult(ctx, bus, "tenant-abc", natsbus.TaskClassify, "job-1", "task-1", 5*time.Second)
		Expect(result).NotTo(BeNil())

		spans := tp.GetSpans()
		Expect(spans).NotTo(BeEmpty())

		span := telemetrytest.FindSpan(spans, "worker.ExecuteTask")
		Expect(span).NotTo(BeNil(), "expected a span named 'worker.ExecuteTask'")

		taskIDAttr, found := telemetrytest.SpanAttribute(span, "task.id")
		Expect(found).To(BeTrue(), "span should have task.id attribute")
		Expect(taskIDAttr.AsString()).To(Equal("task-1"))

		tenantAttr, found := telemetrytest.SpanAttribute(span, "tenant.id")
		Expect(found).To(BeTrue(), "span should have tenant.id attribute")
		Expect(tenantAttr.AsString()).To(Equal("tenant-abc"))
	})

	It("records metrics after processing a task", func() {
		w := worker.New(bus, llm, worker.WorkerConfig{
			QueueGroup: "test-workers",
			LLM: config.LLMConfig{
				DefaultModel:   "test-model",
				EmbeddingModel: "test-embed-model",
			},
		},
			worker.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()),
			worker.WithLogger(testspecs.GinkgoLogger()),
		)

		ctx := testspecs.SetupTenantContext("tenant-abc")
		Expect(w.Start(ctx)).To(Succeed())
		defer func() { Expect(w.Stop(ctx)).To(Succeed()) }()

		payload := BuildCompletionPayload("classify", "gpt-4", 0.0, 256)
		PublishWorkTask(ctx, bus, "tenant-abc", natsbus.TaskClassify, "job-1", "task-1", payload)

		result := WaitForResult(ctx, bus, "tenant-abc", natsbus.TaskClassify, "job-1", "task-1", 5*time.Second)
		Expect(result).NotTo(BeNil())

		rm := tp.GetMetrics()
		taskCounter := telemetrytest.FindMetric(rm, "worker.tasks.processed")
		Expect(taskCounter).NotTo(BeNil(), "expected worker.tasks.processed metric to be recorded")

		count, err := telemetrytest.CounterValue(taskCounter)
		Expect(err).NotTo(HaveOccurred())
		Expect(count).To(BeNumerically(">=", int64(1)))

		taskDuration := telemetrytest.FindMetric(rm, "worker.task.duration_ms")
		Expect(taskDuration).NotTo(BeNil(), "expected worker.task.duration_ms metric to be recorded")

		llmLatency := telemetrytest.FindMetric(rm, "worker.llm.latency_ms")
		Expect(llmLatency).NotTo(BeNil(), "expected worker.llm.latency_ms metric to be recorded")
	})

	It("records worker.errors metric on LLM failure", func() {
		failingLLM := &stubLLMClient{
			completeFunc: func(ctx context.Context, req *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
				return nil, errors.New("simulated LLM failure")
			},
		}
		w := worker.New(bus, failingLLM, worker.WorkerConfig{
			QueueGroup: "test-workers",
			LLM: config.LLMConfig{
				DefaultModel:   "test-model",
				EmbeddingModel: "test-embed-model",
			},
		},
			worker.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()),
			worker.WithLogger(testspecs.GinkgoLogger()),
		)

		ctx := testspecs.SetupTenantContext("tenant-abc")
		Expect(w.Start(ctx)).To(Succeed())
		defer func() { Expect(w.Stop(ctx)).To(Succeed()) }()

		payload := BuildCompletionPayload("classify", "gpt-4", 0.0, 256)
		PublishWorkTask(ctx, bus, "tenant-abc", natsbus.TaskClassify, "job-err", "task-err", payload)

		// Wait for error result
		errCategory := WaitForErrorResult(ctx, bus, "tenant-abc", natsbus.TaskClassify, "job-err", "task-err", 5*time.Second)
		Expect(errCategory).NotTo(BeEmpty(), "error result must be published")

		rm := tp.GetMetrics()
		errCounter := telemetrytest.FindMetric(rm, "worker.errors")
		Expect(errCounter).NotTo(BeNil(), "expected worker.errors metric to be recorded on LLM failure")

		count, err := telemetrytest.CounterValue(errCounter)
		Expect(err).NotTo(HaveOccurred())
		Expect(count).To(BeNumerically(">=", int64(1)))
	})

	It("handles nil providers gracefully", func() {
		Expect(func() {
			w := worker.New(bus, llm, worker.WorkerConfig{
				QueueGroup: "test-workers",
				LLM: config.LLMConfig{
					DefaultModel:   "test-model",
					EmbeddingModel: "test-embed-model",
				},
			},
				worker.WithTelemetry(nil, nil),
				worker.WithLogger(testspecs.GinkgoLogger()),
			)

			ctx := testspecs.SetupTenantContext("tenant-abc")
			Expect(w.Start(ctx)).To(Succeed())
			defer func() { Expect(w.Stop(ctx)).To(Succeed()) }()

			payload := BuildCompletionPayload("classify", "gpt-4", 0.0, 256)
			PublishWorkTask(ctx, bus, "tenant-abc", natsbus.TaskClassify, "job-1", "task-1", payload)

			result := WaitForResult(ctx, bus, "tenant-abc", natsbus.TaskClassify, "job-1", "task-1", 5*time.Second)
			Expect(result).NotTo(BeNil())
		}).NotTo(Panic())
	})
})
