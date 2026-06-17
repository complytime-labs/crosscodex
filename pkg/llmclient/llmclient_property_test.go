package llmclient_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
)

// Suite bootstrap lives in llmclient_bdd_test.go (TestLLMClientBDD).
// This file only registers Describe nodes; Ginkgo collects them automatically.

var _ = Describe("Property Specifications", Ordered, func() {

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
})
