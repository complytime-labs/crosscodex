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
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/internal/worker"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

func strPtr(s string) *string { return &s }

var _ = Describe("Handler", func() {
	var (
		bus     natsbus.Client
		cleanup func()
		llm     *stubLLMClient
	)

	BeforeEach(func() {
		bus, cleanup = testspecs.SetupTestNATS()
		llm = &stubLLMClient{}
	})

	AfterEach(func() {
		cleanup()
	})

	newWorker := func() *worker.Worker {
		return worker.New(bus, llm, worker.WorkerConfig{
			QueueGroup: "test-workers",
			LLM: config.LLMConfig{
				DefaultModel:   "default-model",
				EmbeddingModel: "default-embed",
			},
		}, worker.WithLogger(testspecs.GinkgoLogger()))
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
			llm.completeFunc = func(_ context.Context, _ *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
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
			llm.completeFunc = func(_ context.Context, req *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
				receivedModel = req.Model
				return &llmclient.CompletionResponse{
					Model:   req.Model,
					Choices: []llmclient.CompletionChoice{{Message: llmclient.ChatMessage{Content: "ok"}}},
					Usage:   llmclient.TokenUsage{TotalTokens: 10},
				}, nil
			}

			ctx := testspecs.SetupTenantContext("tenant-abc")
			w := worker.New(bus, llm, worker.WorkerConfig{
				QueueGroup: "test-workers",
				LLM: config.LLMConfig{
					DefaultModel:   "global-model",
					EmbeddingModel: "global-embed",
					TenantOverrides: map[string]config.LLMOverride{
						"tenant-abc": {DefaultModel: strPtr("tenant-specific-model")},
					},
				},
			}, worker.WithLogger(testspecs.GinkgoLogger()))
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
			llm.embedFunc = func(_ context.Context, req *llmclient.EmbeddingRequest) (*llmclient.EmbeddingResponse, error) {
				receivedModel = req.Model
				return &llmclient.EmbeddingResponse{
					Data:  []llmclient.EmbeddingData{{Embedding: []float32{0.1, 0.2, 0.3}}},
					Model: req.Model,
					Usage: llmclient.EmbeddingUsage{TotalTokens: 5},
				}, nil
			}

			ctx := testspecs.SetupTenantContext("tenant-abc")
			w := worker.New(bus, llm, worker.WorkerConfig{
				QueueGroup: "test-workers",
				LLM: config.LLMConfig{
					DefaultModel:   "global-model",
					EmbeddingModel: "global-embed",
					TenantOverrides: map[string]config.LLMOverride{
						"tenant-abc": {EmbeddingModel: strPtr("tenant-embed-model")},
					},
				},
			}, worker.WithLogger(testspecs.GinkgoLogger()))
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
			llm.embedFunc = func(_ context.Context, _ *llmclient.EmbeddingRequest) (*llmclient.EmbeddingResponse, error) {
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

			w1 := worker.New(bus, llm, worker.WorkerConfig{QueueGroup: "shared-group", LLM: config.LLMConfig{DefaultModel: "m"}}, worker.WithLogger(testspecs.GinkgoLogger()))
			w2 := worker.New(bus, llm, worker.WorkerConfig{QueueGroup: "shared-group", LLM: config.LLMConfig{DefaultModel: "m"}}, worker.WithLogger(testspecs.GinkgoLogger()))
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

// publishSpyBus embeds natsbus.Client (nil; the tenant-validation handler
// path returns before touching the bus, so no other method is ever called)
// to record whether a rejected tenant's message ever reached a publish call.
type publishSpyBus struct {
	natsbus.Client
	published bool
}

func (b *publishSpyBus) Publish(_ context.Context, _ string, _ []byte) error {
	b.published = true
	return nil
}

func (b *publishSpyBus) PublishWithHeaders(_ context.Context, _ string, _ []byte, _ map[string][]string) error {
	b.published = true
	return nil
}

var _ = Describe("Handler tenant validation", func() {
	var (
		logs      *bytes.Buffer
		w         *worker.Worker
		bus       *publishSpyBus
		llmCalled bool
	)

	BeforeEach(func() {
		logs = &bytes.Buffer{}
		bus = &publishSpyBus{}
		llmCalled = false
		stubLLM := &stubLLMClient{
			completeFunc: func(_ context.Context, _ *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
				llmCalled = true
				return nil, errors.New("LLM must not be called for a rejected tenant")
			},
			embedFunc: func(_ context.Context, _ *llmclient.EmbeddingRequest) (*llmclient.EmbeddingResponse, error) {
				llmCalled = true
				return nil, errors.New("LLM must not be called for a rejected tenant")
			},
		}
		w = worker.New(bus, stubLLM, worker.WorkerConfig{},
			worker.WithLogger(slog.New(slog.NewTextHandler(logs, nil))))
	})

	DescribeTable("discards the task and logs the tenant ID rule",
		func(tenantID, wantMsg string) {
			msg := &natsbus.Message{Metadata: natsbus.MessageMetadata{TenantID: tenantID}}
			Expect(worker.ExportHandleMessage(w, context.Background(), msg)).To(Succeed())
			Expect(logs.String()).To(ContainSubstring(wantMsg))
			Expect(logs.String()).To(ContainSubstring(natsbus.HeaderTenantID))
			Expect(logs.String()).To(ContainSubstring(tenant.IDRule))
			Expect(llmCalled).To(BeFalse(), "a rejected tenant must never reach the LLM client")
			Expect(bus.published).To(BeFalse(), "a rejected tenant must never publish a result")
		},
		Entry("missing tenant ID", "", "message missing tenant ID"),
		Entry("53-character tenant ID", "a"+strings.Repeat("b", 52), "invalid tenant ID"),
		Entry("uppercase tenant ID", "Tenant-1", "invalid tenant ID"),
	)
})
