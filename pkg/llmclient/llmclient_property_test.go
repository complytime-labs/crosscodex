package llmclient_test

import (
	"encoding/json"
	"reflect"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/structpb"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
)

// Suite bootstrap lives in llmclient_bdd_test.go (TestLLMClientBDD).
// This file only registers Describe nodes; Ginkgo collects them automatically.

var _ = Describe("Property Specifications", Ordered, func() {

	Context("ParseModelCatalog", func() {
		It("round-trips any entries and never keeps a field it does not read", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				n := rapid.IntRange(0, 20).Draw(t, "n")
				want := make([]llmclient.ModelDeployment, n)
				entries := make([]map[string]any, n)
				for i := range n {
					want[i] = llmclient.ModelDeployment{
						Alias: rapid.String().Draw(t, "alias"), ID: rapid.String().Draw(t, "id"), Model: rapid.String().Draw(t, "model")}
					entries[i] = map[string]any{
						"model_name":     want[i].Alias,
						"litellm_params": map[string]any{"model": want[i].Model, "api_base": rapid.String().Draw(t, "base")},
						"model_info":     map[string]any{"id": want[i].ID},
					}
				}
				body, err := json.Marshal(map[string]any{"data": entries})
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				got, err := llmclient.ParseModelCatalog(body)
				if err != nil {
					t.Fatalf("parse: %v", err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("got %v, want %v", got, want)
				}
			})
		})

		It("never panics and never returns both entries and an error", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				got, err := llmclient.ParseModelCatalog(rapid.SliceOf(rapid.Byte()).Draw(t, "body"))
				if err != nil && got != nil {
					t.Fatalf("error %v with %d entries", err, len(got))
				}
			})
		})
	})

	Context("shouldRetry — status code classification", func() {
		It("always retries on HTTP 429", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				header := rapid.StringN(0, 20, -1).Draw(t, "retryAfterHeader")
				retry, _ := llmclient.ShouldRetry(429, header)
				Expect(retry).To(BeTrue(),
					"shouldRetry must return true for 429 with header %q", header)
			})
		})

		It("always retries on 500-599", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				code := rapid.IntRange(500, 599).Draw(t, "statusCode")
				retry, _ := llmclient.ShouldRetry(code, "")
				Expect(retry).To(BeTrue(),
					"shouldRetry must return true for status %d", code)
			})
		})

		It("never retries on 200-399", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				code := rapid.IntRange(200, 399).Draw(t, "statusCode")
				retry, _ := llmclient.ShouldRetry(code, "")
				Expect(retry).To(BeFalse(),
					"shouldRetry must return false for status %d", code)
			})
		})
	})

	Context("backoffDuration — bounds", func() {
		It("always returns a non-negative duration capped at 30s", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				attempt := rapid.IntRange(0, 100).Draw(t, "attempt")
				d := llmclient.BackoffDuration(attempt)
				Expect(d).To(BeNumerically(">=", 0),
					"backoffDuration(%d) returned negative: %v", attempt, d)
				Expect(d).To(BeNumerically("<=", 30*time.Second),
					"backoffDuration(%d) exceeded 30s cap: %v", attempt, d)
			})
		})
	})

	Context("ContentHash — determinism", func() {
		It("produces the same 64-char hex output for the same input", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				role := rapid.SampledFrom([]string{"user", "assistant", "system"}).Draw(t, "role")
				content := rapid.StringN(0, 200, -1).Draw(t, "content")
				msgs := []llmclient.ChatMessage{{Role: role, Content: content}}

				hash1 := llmclient.ContentHash(msgs)
				hash2 := llmclient.ContentHash(msgs)

				Expect(hash1).To(Equal(hash2),
					"ContentHash not deterministic for %v", msgs)
				Expect(hash1).To(HaveLen(64),
					"ContentHash should produce 64-char hex, got %d chars", len(hash1))
			})
		})
	})

	Context("parseRetryAfter — bounds", func() {
		It("always returns a non-negative duration", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				header := rapid.StringN(0, 50, -1).Draw(t, "header")
				d := llmclient.ParseRetryAfter(header)
				Expect(d).To(BeNumerically(">=", 0),
					"parseRetryAfter(%q) returned negative: %v", header, d)
			})
		})
	})

	Context("isModelAllowed — allow-list semantics", func() {
		It("allows all models when AllowedModels is empty", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				model := rapid.StringN(1, 50, -1).Draw(t, "model")
				cfg := config.LLMConfig{
					GatewayURL:    "http://localhost:11434",
					Timeout:       5,
					AllowedModels: nil,
				}
				c, err := llmclient.NewClient(cfg)
				Expect(err).NotTo(HaveOccurred())
				defer c.Close()

				Expect(llmclient.ExportIsModelAllowed(c, model)).To(BeTrue(),
					"empty AllowedModels should allow model %q", model)
			})
		})

		It("requires exact match when AllowedModels is non-empty", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				allowed := rapid.SliceOfN(
					rapid.StringMatching(`[a-z0-9-]{3,20}`), 1, 5,
				).Draw(t, "allowedModels")
				// Generate a model that is NOT in the allowed list.
				disallowed := rapid.StringMatching(`[A-Z]{5,10}`).Draw(t, "disallowedModel")

				cfg := config.LLMConfig{
					GatewayURL:    "http://localhost:11434",
					Timeout:       5,
					AllowedModels: allowed,
				}
				c, err := llmclient.NewClient(cfg)
				Expect(err).NotTo(HaveOccurred())
				defer c.Close()

				Expect(llmclient.ExportIsModelAllowed(c, disallowed)).To(BeFalse(),
					"model %q should not be allowed in list %v", disallowed, allowed)

				// Pick a random element from the allowed list and verify it's allowed.
				idx := rapid.IntRange(0, len(allowed)-1).Draw(t, "allowedIdx")
				Expect(llmclient.ExportIsModelAllowed(c, allowed[idx])).To(BeTrue(),
					"model %q should be allowed in list %v", allowed[idx], allowed)
			})
		})
	})

	Context("MessagesFromPayload — round trip", func() {
		It("decodes messages built from valid roles back to the same messages and ContentHash", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				roles := []string{llmclient.RoleSystem, llmclient.RoleUser, llmclient.RoleAssistant}
				messages := rapid.SliceOfN(rapid.Custom(func(t *rapid.T) llmclient.ChatMessage {
					return llmclient.ChatMessage{
						Role:    rapid.SampledFrom(roles).Draw(t, "role"),
						Content: rapid.String().Draw(t, "content"),
					}
				}), 0, 8).Draw(t, "messages")

				items := make([]any, len(messages))
				for i, m := range messages {
					items[i] = map[string]any{"role": m.Role, "content": m.Content, "ignored": "extra"}
				}
				v, err := structpb.NewValue(items)
				if err != nil {
					t.Skip("content is not representable in a structpb string: ", err)
				}

				got, err := llmclient.MessagesFromPayload(v)
				if err != nil {
					t.Fatalf("valid messages rejected: %v", err)
				}
				if len(messages) == 0 {
					messages = []llmclient.ChatMessage{}
				}
				if !reflect.DeepEqual(got, messages) {
					t.Fatalf("decoded %#v, want %#v", got, messages)
				}
				if llmclient.ContentHash(got) != llmclient.ContentHash(messages) {
					t.Fatalf("decoded messages hash differently from the originals")
				}
			})
		})

		It("rejects any role outside the allowlist", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				role := rapid.String().Draw(t, "role")
				if role == llmclient.RoleSystem || role == llmclient.RoleUser || role == llmclient.RoleAssistant {
					t.Skip("allowed role")
				}
				v, err := structpb.NewValue([]any{map[string]any{"role": role, "content": "c"}})
				if err != nil {
					t.Skip("role is not representable in a structpb string: ", err)
				}
				if _, err := llmclient.MessagesFromPayload(v); err == nil {
					t.Fatalf("role %q accepted", role)
				}
			})
		})
	})

	Context("CheckAnswered — an answer is never treated as truncated", func() {
		It("accepts every response with one non-blank answer, whatever the finish reasons", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				reasons := rapid.SampledFrom([]string{"stop", "length", "content_filter", ""})
				n := rapid.IntRange(1, 4).Draw(t, "choices")
				answered := rapid.IntRange(0, n-1).Draw(t, "answered")
				choices := make([]llmclient.CompletionChoice, n)
				for i := range choices {
					content := rapid.SampledFrom([]string{"", " ", "\n"}).Draw(t, "blank")
					if i == answered {
						content = rapid.StringMatching(`[A-Za-z|]{1,20}`).Draw(t, "answer")
					}
					choices[i] = llmclient.CompletionChoice{
						Message:      llmclient.ChatMessage{Role: "assistant", Content: content},
						FinishReason: reasons.Draw(t, "finish"),
					}
				}
				req := &llmclient.CompletionRequest{Model: "m", MaxTokens: rapid.IntRange(1, 4096).Draw(t, "maxTokens")}
				if err := llmclient.CheckAnswered(req, &llmclient.CompletionResponse{Choices: choices}); err != nil {
					t.Fatalf("CheckAnswered refused a response with an answer: %v", err)
				}
			})
		})
	})
})
