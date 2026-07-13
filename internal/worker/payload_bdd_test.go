package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
)

// testMessages are the exact messages used in the "extracts a valid completion
// request from payload" spec. The content_hash field must match
// llmclient.ContentHash over this slice.
var testMessages = []llmclient.ChatMessage{
	{Role: "system", Content: "You are a classifier."},
	{Role: "user", Content: "Classify this."},
}

var _ = Describe("extractCompletionRequest", func() {
	It("extracts a valid completion request from payload", func() {
		payload, err := structpb.NewStruct(map[string]interface{}{
			"messages": []interface{}{
				map[string]interface{}{"role": testMessages[0].Role, "content": testMessages[0].Content},
				map[string]interface{}{"role": testMessages[1].Role, "content": testMessages[1].Content},
			},
			"model":          "gpt-4",
			"temperature":    0.7,
			"max_tokens":     float64(256),
			"prompt_name":    "classify",
			"prompt_version": "1.0",
			"content_hash":   llmclient.ContentHash(testMessages),
		})
		Expect(err).NotTo(HaveOccurred())

		req, err := extractCompletionRequest(payload, "tenant-abc", "job-123")

		Expect(err).NotTo(HaveOccurred())
		Expect(req.Model).To(Equal("gpt-4"))
		Expect(req.Messages).To(HaveLen(2))
		Expect(req.Messages[0].Role).To(Equal("system"))
		Expect(req.Messages[0].Content).To(Equal("You are a classifier."))
		Expect(req.Messages[1].Role).To(Equal("user"))
		Expect(req.Messages[1].Content).To(Equal("Classify this."))
		Expect(*req.Temperature).To(BeNumerically("~", 0.7, 0.001))
		Expect(req.MaxTokens).To(Equal(256))
		Expect(req.TenantID).To(Equal("tenant-abc"))
		Expect(req.JobID).To(Equal("job-123"))
		Expect(req.PromptName).To(Equal("classify"))
		Expect(req.PromptVersion).To(Equal("1.0"))
	})

	It("returns error when messages field is missing", func() {
		payload, _ := structpb.NewStruct(map[string]interface{}{
			"model": "gpt-4",
		})
		_, err := extractCompletionRequest(payload, "tenant-abc", "job-123")
		Expect(err).To(MatchError(ContainSubstring("messages")))
	})

	It("returns error when messages is not a list", func() {
		payload, _ := structpb.NewStruct(map[string]interface{}{
			"messages": "not-a-list",
			"model":    "gpt-4",
		})
		_, err := extractCompletionRequest(payload, "tenant-abc", "job-123")
		Expect(err).To(MatchError(ContainSubstring("messages")))
	})

	It("returns error when a message element is not a struct", func() {
		// structpb.NewStruct cannot store a bare scalar inside a list directly,
		// so we build the Value manually to exercise the msgStruct == nil branch.
		payload, _ := structpb.NewStruct(map[string]interface{}{
			"messages": []interface{}{
				map[string]interface{}{"role": "user", "content": "valid"},
			},
			"model": "gpt-4",
		})
		// Replace the first list element with a scalar (number) value.
		msgList := payload.Fields["messages"].GetListValue()
		msgList.Values[0] = structpb.NewNumberValue(42)

		_, err := extractCompletionRequest(payload, "tenant-abc", "job-123")
		Expect(err).To(MatchError(ContainSubstring("must be a struct")))
		Expect(errors.Is(err, ErrInvalidPayload)).To(BeTrue())
	})

	It("returns error when a message is missing role", func() {
		payload, _ := structpb.NewStruct(map[string]interface{}{
			"messages": []interface{}{
				map[string]interface{}{"content": "missing role"},
			},
			"model": "gpt-4",
		})
		_, err := extractCompletionRequest(payload, "tenant-abc", "job-123")
		Expect(err).To(MatchError(ContainSubstring("role")))
	})

	It("uses empty model when not specified", func() {
		payload, _ := structpb.NewStruct(map[string]interface{}{
			"messages": []interface{}{
				map[string]interface{}{"role": "user", "content": "hello"},
			},
		})
		req, err := extractCompletionRequest(payload, "tenant-abc", "job-123")
		Expect(err).NotTo(HaveOccurred())
		Expect(req.Model).To(BeEmpty())
	})

	It("rejects invalid role", func() {
		payload, err := structpb.NewStruct(map[string]interface{}{
			"messages": []interface{}{
				map[string]interface{}{"role": "INJECTED", "content": "bad"},
			},
		})
		Expect(err).NotTo(HaveOccurred())

		_, err = extractCompletionRequest(payload, "tenant-abc", "job-123")
		Expect(err).To(HaveOccurred())
		Expect(errors.Is(err, ErrInvalidPayload)).To(BeTrue(),
			"invalid role must wrap ErrInvalidPayload")
		Expect(err.Error()).To(ContainSubstring("INJECTED"),
			"error must name the invalid role for actionability")
	})

	It("rejects content_hash mismatch", func() {
		payload, err := structpb.NewStruct(map[string]interface{}{
			"messages": []interface{}{
				map[string]interface{}{"role": "user", "content": "hello"},
			},
			"content_hash": "deliberately-wrong-hash",
		})
		Expect(err).NotTo(HaveOccurred())

		_, err = extractCompletionRequest(payload, "tenant-abc", "job-123")
		Expect(err).To(HaveOccurred())
		Expect(errors.Is(err, ErrInvalidPayload)).To(BeTrue(),
			"content_hash mismatch must wrap ErrInvalidPayload")
		Expect(err.Error()).To(ContainSubstring("mismatch"),
			"error must mention mismatch for actionability")
	})
})

var _ = Describe("extractEmbeddingRequest", func() {
	It("extracts a valid embedding request from payload", func() {
		payload, _ := structpb.NewStruct(map[string]interface{}{
			"text":       "compliance requirement text",
			"model":      "text-embedding-3-small",
			"batch_size": float64(100),
		})

		req, err := extractEmbeddingRequest(payload, "tenant-abc", "job-123")

		Expect(err).NotTo(HaveOccurred())
		Expect(req.Model).To(Equal("text-embedding-3-small"))
		Expect(req.Input).To(Equal([]string{"compliance requirement text"}))
		Expect(req.TenantID).To(Equal("tenant-abc"))
		Expect(req.JobID).To(Equal("job-123"))
	})

	It("returns error when text field is missing", func() {
		payload, _ := structpb.NewStruct(map[string]interface{}{
			"model": "text-embedding-3-small",
		})
		_, err := extractEmbeddingRequest(payload, "tenant-abc", "job-123")
		Expect(err).To(MatchError(ContainSubstring("text")))
	})
})

var _ = Describe("buildCompletionResult", func() {
	It("builds a structpb result from completion response", func() {
		resp := &llmclient.CompletionResponse{
			Model: "gpt-4",
			Choices: []llmclient.CompletionChoice{
				{Message: llmclient.ChatMessage{Content: "Type: operational\nLevel: high"}},
			},
			Usage: llmclient.TokenUsage{
				PromptTokens:     80,
				CompletionTokens: 43,
				TotalTokens:      123,
			},
		}

		result, err := buildCompletionResult(resp)

		Expect(err).NotTo(HaveOccurred())
		Expect(result.Fields["response"].GetStringValue()).To(Equal("Type: operational\nLevel: high"))
		Expect(result.Fields["model"].GetStringValue()).To(Equal("gpt-4"))
		Expect(result.Fields["tokens_used"].GetNumberValue()).To(BeNumerically("==", 123))
		Expect(result.Fields["prompt_tokens"].GetNumberValue()).To(BeNumerically("==", 80))
		Expect(result.Fields["completion_tokens"].GetNumberValue()).To(BeNumerically("==", 43))
	})

	It("handles empty choices", func() {
		resp := &llmclient.CompletionResponse{
			Model:   "gpt-4",
			Choices: []llmclient.CompletionChoice{},
			Usage:   llmclient.TokenUsage{TotalTokens: 10},
		}
		result, err := buildCompletionResult(resp)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Fields["response"].GetStringValue()).To(BeEmpty())
	})
})

var _ = Describe("buildEmbeddingResult", func() {
	It("builds a structpb result from embedding response", func() {
		resp := &llmclient.EmbeddingResponse{
			Data: []llmclient.EmbeddingData{
				{Embedding: []float32{0.1, 0.2, 0.3}},
			},
			Model: "text-embedding-3-small",
			Usage: llmclient.EmbeddingUsage{TotalTokens: 42},
		}

		result, err := buildEmbeddingResult(resp)

		Expect(err).NotTo(HaveOccurred())
		Expect(result.Fields["model"].GetStringValue()).To(Equal("text-embedding-3-small"))
		Expect(result.Fields["tokens_used"].GetNumberValue()).To(BeNumerically("==", 42))
		Expect(result.Fields["dimensions"].GetNumberValue()).To(BeNumerically("==", 3),
			"dimensions must equal the embedding vector length")
		embList := result.Fields["embeddings"].GetListValue()
		Expect(embList).NotTo(BeNil())
		Expect(embList.Values).To(HaveLen(1))
		vec := embList.Values[0].GetListValue()
		Expect(vec.Values).To(HaveLen(3))
		Expect(vec.Values[0].GetNumberValue()).To(BeNumerically("~", 0.1, 0.001))
	})

	It("handles empty data slice", func() {
		resp := &llmclient.EmbeddingResponse{
			Data:  []llmclient.EmbeddingData{},
			Model: "text-embedding-3-small",
			Usage: llmclient.EmbeddingUsage{TotalTokens: 0},
		}
		result, err := buildEmbeddingResult(resp)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Fields["dimensions"].GetNumberValue()).To(BeNumerically("==", 0),
			"dimensions must be 0 when no embeddings are returned")
		embList := result.Fields["embeddings"].GetListValue()
		Expect(embList).NotTo(BeNil())
		Expect(embList.Values).To(BeEmpty())
	})
})

var _ = Describe("errorCategory", func() {
	DescribeTable("maps errors to categories",
		func(err error, expected string) {
			Expect(errorCategory(err)).To(Equal(expected))
		},
		Entry("invalid payload", ErrInvalidPayload, "invalid_payload"),
		Entry("wrapped invalid payload", fmt.Errorf("wrap: %w", ErrInvalidPayload), "invalid_payload"),
		Entry("LLM call error", ErrLLMCall, "llm_error"),
		Entry("wrapped LLM error", fmt.Errorf("wrap: %w", ErrLLMCall), "llm_error"),
		Entry("tenant config error", ErrTenantConfig, "tenant_config_error"),
		Entry("wrapped tenant config error", fmt.Errorf("wrap: %w", ErrTenantConfig), "tenant_config_error"),
		Entry("unknown error", errors.New("something else"), "unknown"),
		Entry("nil-wrapped unknown", fmt.Errorf("wrap: %w", errors.New("other")), "unknown"),
	)
})

var _ = Describe("handleMessage tenant validation", func() {
	It("rejects empty tenant ID", func() {
		w := &Worker{logger: slog.Default()}
		msg := &natsbus.Message{
			Headers:  map[string][]string{},
			Metadata: natsbus.MessageMetadata{TenantID: ""},
		}
		err := w.handleMessage(context.Background(), msg)
		Expect(err).NotTo(HaveOccurred())
	})

	It("rejects malformed tenant ID", func() {
		w := &Worker{logger: slog.Default()}
		msg := &natsbus.Message{
			Headers: map[string][]string{
				"X-Task-Id":   {"task-1"},
				"X-Task-Type": {"classify"},
				"X-Job-Id":    {"job-1"},
			},
			Metadata: natsbus.MessageMetadata{TenantID: "INVALID!!tenant"},
		}
		err := w.handleMessage(context.Background(), msg)
		Expect(err).NotTo(HaveOccurred())
	})
})
