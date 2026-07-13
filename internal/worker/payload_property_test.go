package worker

import (
	"errors"
	"math"

	. "github.com/onsi/ginkgo/v2"

	"google.golang.org/protobuf/types/known/structpb"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/llmclient"
)

var _ = Describe("Property Specifications", Ordered, func() {
	Context("extractCompletionRequest — never panics", func() {
		It("handles arbitrary struct payloads without panicking", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				fields := map[string]interface{}{}
				nKeys := rapid.IntRange(0, 10).Draw(t, "nKeys")
				for i := 0; i < nKeys; i++ {
					key := rapid.StringMatching(`[a-z_]{1,20}`).Draw(t, "key")
					fields[key] = rapid.OneOf(
						rapid.Just[any]("string-value"),
						rapid.Just[any](float64(42)),
						rapid.Just[any](true),
						rapid.Just[any](nil),
						rapid.Just[any]([]interface{}{map[string]interface{}{"role": "user"}}),
					).Draw(t, "value")
				}
				payload, err := structpb.NewStruct(fields)
				if err != nil {
					return
				}
				// Must not panic — error return is fine
				_, _ = extractCompletionRequest(payload, "tenant-test", "job-test")
			})
		})
	})

	Context("extractEmbeddingRequest — never panics", func() {
		It("handles arbitrary struct payloads without panicking", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				fields := map[string]interface{}{}
				nKeys := rapid.IntRange(0, 10).Draw(t, "nKeys")
				for i := 0; i < nKeys; i++ {
					key := rapid.StringMatching(`[a-z_]{1,20}`).Draw(t, "key")
					fields[key] = rapid.OneOf(
						rapid.Just[any]("string-value"),
						rapid.Just[any](float64(42)),
						rapid.Just[any](true),
						rapid.Just[any](nil),
					).Draw(t, "value")
				}
				payload, err := structpb.NewStruct(fields)
				if err != nil {
					return
				}
				_, _ = extractEmbeddingRequest(payload, "tenant-test", "job-test")
			})
		})
	})

	Context("completionPayload — roundtrip", func() {
		It("preserves model, max_tokens, and messages through extraction", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				model := rapid.StringMatching(`[a-z0-9-]{1,30}`).Draw(t, "model")
				content := rapid.String().Draw(t, "content")
				temp := rapid.Float64Range(0.0, 2.0).Draw(t, "temperature")
				maxTokens := rapid.IntRange(1, 4096).Draw(t, "maxTokens")

				payload, err := structpb.NewStruct(map[string]interface{}{
					"messages": []interface{}{
						map[string]interface{}{"role": "system", "content": content},
						map[string]interface{}{"role": "user", "content": "test"},
					},
					"model":       model,
					"temperature": temp,
					"max_tokens":  float64(maxTokens),
				})
				if err != nil {
					return
				}

				req, err := extractCompletionRequest(payload, "tenant-abc", "job-1")
				if err != nil {
					t.Fatalf("extraction failed for valid payload: %v", err)
				}
				if req.Model != model {
					t.Fatalf("model mismatch: got %q, want %q", req.Model, model)
				}
				if req.MaxTokens != maxTokens {
					t.Fatalf("max_tokens mismatch: got %d, want %d", req.MaxTokens, maxTokens)
				}
				if len(req.Messages) != 2 {
					t.Fatalf("messages count: got %d, want 2", len(req.Messages))
				}
				if req.Messages[0].Content != content {
					t.Fatalf("system content mismatch")
				}
				// Temperature is the only pointer-based optional field — verify roundtrip.
				if req.Temperature == nil {
					t.Fatalf("temperature missing from extracted request")
				}
				if math.Abs(*req.Temperature-temp) > 0.001 {
					t.Fatalf("temperature roundtrip: got %v, want %v", *req.Temperature, temp)
				}
			})
		})
	})

	Context("extractCompletionRequest — role allowlist fail-closed", func() {
		It("rejects all roles not in the allowlist", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				// Generate strings that are not in {"system", "user", "assistant"}
				role := rapid.StringMatching(`[a-zA-Z]{1,20}`).Draw(t, "role")
				if role == "system" || role == "user" || role == "assistant" {
					t.Skip()
				}

				payload, err := structpb.NewStruct(map[string]interface{}{
					"messages": []interface{}{
						map[string]interface{}{"role": role, "content": "test"},
					},
				})
				if err != nil {
					return
				}

				_, extractErr := extractCompletionRequest(payload, "tenant-test", "job-test")
				if extractErr == nil {
					t.Fatalf("extractCompletionRequest accepted non-allowlisted role %q without error", role)
				}
				if !errors.Is(extractErr, ErrInvalidPayload) {
					t.Fatalf("expected ErrInvalidPayload for role %q, got: %v", role, extractErr)
				}
			})
		})
	})

	Context("buildEmbeddingResult — float32 roundtrip precision", func() {
		It("preserves embedding values within float32 precision after float64 conversion", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				nVecs := rapid.IntRange(1, 3).Draw(t, "nVecs")
				dims := rapid.IntRange(1, 8).Draw(t, "dims")

				data := make([]llmclient.EmbeddingData, nVecs)
				for i := range data {
					emb := make([]float32, dims)
					for j := range emb {
						emb[j] = float32(rapid.Float64Range(-1.0, 1.0).Draw(t, "val"))
					}
					data[i] = llmclient.EmbeddingData{Embedding: emb}
				}

				resp := &llmclient.EmbeddingResponse{
					Data:  data,
					Model: "test-model",
					Usage: llmclient.EmbeddingUsage{TotalTokens: 1},
				}

				result, err := buildEmbeddingResult(resp)
				if err != nil {
					t.Fatalf("buildEmbeddingResult failed: %v", err)
				}

				embList := result.Fields["embeddings"].GetListValue()
				if len(embList.Values) != nVecs {
					t.Fatalf("expected %d vectors, got %d", nVecs, len(embList.Values))
				}

				for i, vecVal := range embList.Values {
					vec := vecVal.GetListValue()
					if len(vec.Values) != dims {
						t.Fatalf("vector %d: expected %d dims, got %d", i, dims, len(vec.Values))
					}
					for j, v := range vec.Values {
						got := float32(v.GetNumberValue())
						want := data[i].Embedding[j]
						// float32→float64→float32 roundtrip must be lossless
						if got != want {
							t.Fatalf("vector[%d][%d]: roundtrip mismatch: got %v, want %v", i, j, got, want)
						}
					}
				}

				// Verify dimensions field
				dimVal := int(result.Fields["dimensions"].GetNumberValue())
				if dimVal != dims {
					t.Fatalf("dimensions: got %d, want %d", dimVal, dims)
				}
			})
		})
	})

	Context("buildCompletionResult — roundtrip", func() {
		It("preserves model and token usage through serialization", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				resp := &llmclient.CompletionResponse{
					Model: rapid.StringMatching(`[a-z0-9-]{1,20}`).Draw(t, "model"),
					Choices: []llmclient.CompletionChoice{
						{Message: llmclient.ChatMessage{Content: rapid.String().Draw(t, "content")}},
					},
					Usage: llmclient.TokenUsage{
						PromptTokens:     rapid.IntRange(0, 10000).Draw(t, "promptTokens"),
						CompletionTokens: rapid.IntRange(0, 10000).Draw(t, "completionTokens"),
						TotalTokens:      rapid.IntRange(0, 20000).Draw(t, "totalTokens"),
					},
				}

				result, err := buildCompletionResult(resp)
				if err != nil {
					t.Fatalf("build failed: %v", err)
				}
				if result.Fields["model"].GetStringValue() != resp.Model {
					t.Fatalf("model mismatch")
				}
				if int(result.Fields["tokens_used"].GetNumberValue()) != resp.Usage.TotalTokens {
					t.Fatalf("tokens mismatch")
				}
			})
		})
	})
})
