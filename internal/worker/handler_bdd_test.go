package worker_test

import (
	"context"
	"errors"
	"fmt"
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
				PublishWorkTask(ctx, bus, "tenant-abc", taskType, "job-1", "task-1", payload)

				result := WaitForResult(ctx, bus, "tenant-abc", taskType, "job-1", "task-1", 5*time.Second)
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
			PublishWorkTask(ctx, bus, "tenant-abc", natsbus.TaskClassify, "job-1", "task-1", payload)

			// Wait for the error result
			errorResult := WaitForErrorResult(ctx, bus, "tenant-abc", natsbus.TaskClassify, "job-1", "task-1", 5*time.Second)
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
			PublishWorkTask(ctx, bus, "tenant-abc", natsbus.TaskClassify, "job-1", "task-1", payload)

			result := WaitForResult(ctx, bus, "tenant-abc", natsbus.TaskClassify, "job-1", "task-1", 5*time.Second)
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
			PublishWorkTask(ctx, bus, "tenant-abc", natsbus.TaskEmbed, "job-1", "task-1", payload)

			result := WaitForResult(ctx, bus, "tenant-abc", natsbus.TaskEmbed, "job-1", "task-1", 5*time.Second)
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
			PublishWorkTask(ctx, bus, "tenant-abc", "unknown_type", "job-1", "task-1", payload)

			errorResult := WaitForErrorResult(ctx, bus, "tenant-abc", "unknown_type", "job-1", "task-1", 5*time.Second)
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
			// Publish garbage bytes that won't unmarshal as structpb.Struct
			err = bus.PublishWithHeaders(ctx, subject, []byte("not-proto-data"), headers)
			Expect(err).NotTo(HaveOccurred())

			errorResult := WaitForErrorResult(ctx, bus, "tenant-abc", natsbus.TaskClassify, "job-1", "task-bad", 5*time.Second)
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
			PublishWorkTask(ctx, bus, "tenant-abc", natsbus.TaskEmbed, "job-1", "task-1", payload)

			errorResult := WaitForErrorResult(ctx, bus, "tenant-abc", natsbus.TaskEmbed, "job-1", "task-1", 5*time.Second)
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
