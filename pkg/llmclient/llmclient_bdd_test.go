package llmclient_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	otelcodes "go.opentelemetry.io/otel/codes"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
)

func TestLLMClientBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "LLM Client Suite")
}

var _ = BeforeSuite(func() {
	DeferCleanup(testspecs.RedirectLogsToGinkgo())
})

// gatewayModeCompletionJSON returns a valid OpenAI-style completion response.
func gatewayModeCompletionJSON() []byte {
	return []byte(`{
		"id": "chatcmpl-test",
		"object": "chat.completion",
		"choices": [{
			"index": 0,
			"message": {"role": "assistant", "content": "Hello from gateway mode."},
			"finish_reason": "stop"
		}],
		"usage": {"prompt_tokens": 5, "completion_tokens": 7, "total_tokens": 12}
	}`)
}

// =====================================================
// Level 1: Behavioral Specifications
// =====================================================

var _ = Describe("Credential Resolution", func() {

	Describe("env: scheme", func() {
		It("resolves a credential from an environment variable", func() {
			By("setting the environment variable")
			os.Setenv("TEST_LLM_KEY", "sk-test-secret-123")
			DeferCleanup(func() { os.Unsetenv("TEST_LLM_KEY") })

			By("resolving the credential")
			key, err := llmclient.ResolveCredential("env:TEST_LLM_KEY")
			Expect(err).NotTo(HaveOccurred())
			Expect(key).To(Equal("sk-test-secret-123"))
		})

		It("returns an error for an unset environment variable", func() {
			_, err := llmclient.ResolveCredential("env:NONEXISTENT_LLM_VAR")
			Expect(err).To(HaveOccurred())
			Expect(err).To(MatchError(ContainSubstring("environment variable")))
			Expect(err).To(MatchError(ContainSubstring("NONEXISTENT_LLM_VAR")))
		})
	})

	Describe("file: scheme", func() {
		It("resolves a credential from a file with correct permissions", func() {
			By("creating a temp file with mode 0600")
			dir := GinkgoT().TempDir()
			path := filepath.Join(dir, "api-key")
			err := os.WriteFile(path, []byte("sk-file-secret-456\n"), 0600)
			Expect(err).NotTo(HaveOccurred())

			By("resolving the credential")
			key, err := llmclient.ResolveCredential("file:" + path)
			Expect(err).NotTo(HaveOccurred())
			Expect(key).To(Equal("sk-file-secret-456"))
		})

		It("rejects a file with overly permissive mode", func() {
			dir := GinkgoT().TempDir()
			path := filepath.Join(dir, "api-key-open")
			err := os.WriteFile(path, []byte("sk-loose-789"), 0644)
			Expect(err).NotTo(HaveOccurred())

			_, err = llmclient.ResolveCredential("file:" + path)
			Expect(err).To(HaveOccurred())
			Expect(err).To(MatchError(ContainSubstring("permissions")))
		})

		It("returns an error for a missing file", func() {
			_, err := llmclient.ResolveCredential("file:/nonexistent/path")
			Expect(err).To(HaveOccurred())
			Expect(err).To(MatchError(ContainSubstring("no such file")))
		})
	})

	Describe("vault: scheme", func() {
		It("returns a clear error that vault integration is not wired", func() {
			_, err := llmclient.ResolveCredential("vault:secret/data/llm-key")
			Expect(err).To(HaveOccurred())
			Expect(err).To(MatchError(ContainSubstring("vault")))
			Expect(err).To(MatchError(ContainSubstring("not implemented")))
		})
	})

	Describe("invalid schemes", func() {
		It("returns an error for an unknown scheme", func() {
			_, err := llmclient.ResolveCredential("s3:bucket/key")
			Expect(err).To(HaveOccurred())
			Expect(err).To(MatchError(ContainSubstring("unsupported credential scheme")))
		})

		It("returns an error for an empty ref", func() {
			_, err := llmclient.ResolveCredential("")
			Expect(err).To(HaveOccurred())
			Expect(err).To(MatchError(ContainSubstring("empty")))
		})
	})
})

var _ = Describe("Retry Logic", func() {

	Describe("shouldRetry", func() {
		It("retries on HTTP 429 (rate limit)", func() {
			retry, _ := llmclient.ShouldRetry(429, "")
			Expect(retry).To(BeTrue())
		})

		It("retries on HTTP 500 (server error)", func() {
			retry, _ := llmclient.ShouldRetry(500, "")
			Expect(retry).To(BeTrue())
		})

		It("retries on HTTP 502 (bad gateway)", func() {
			retry, _ := llmclient.ShouldRetry(502, "")
			Expect(retry).To(BeTrue())
		})

		It("retries on HTTP 503 (service unavailable)", func() {
			retry, _ := llmclient.ShouldRetry(503, "")
			Expect(retry).To(BeTrue())
		})

		It("retries on HTTP 504 (gateway timeout)", func() {
			retry, _ := llmclient.ShouldRetry(504, "")
			Expect(retry).To(BeTrue())
		})

		It("does not retry on HTTP 400 (bad request)", func() {
			retry, _ := llmclient.ShouldRetry(400, "")
			Expect(retry).To(BeFalse())
		})

		It("does not retry on HTTP 401 (unauthorized)", func() {
			retry, _ := llmclient.ShouldRetry(401, "")
			Expect(retry).To(BeFalse())
		})

		It("does not retry on HTTP 403 (forbidden)", func() {
			retry, _ := llmclient.ShouldRetry(403, "")
			Expect(retry).To(BeFalse())
		})

		It("does not retry on HTTP 404 (not found)", func() {
			retry, _ := llmclient.ShouldRetry(404, "")
			Expect(retry).To(BeFalse())
		})

		It("does not retry on HTTP 200 (success)", func() {
			retry, _ := llmclient.ShouldRetry(200, "")
			Expect(retry).To(BeFalse())
		})

		It("returns Retry-After delay for 429 with integer header", func() {
			retry, delay := llmclient.ShouldRetry(429, "5")
			Expect(retry).To(BeTrue())
			Expect(delay).To(Equal(5 * time.Second))
		})

		It("caps Retry-After delay at 60 seconds", func() {
			retry, delay := llmclient.ShouldRetry(429, "120")
			Expect(retry).To(BeTrue())
			Expect(delay).To(Equal(60 * time.Second))
		})

		It("returns zero delay for 5xx even with Retry-After header", func() {
			retry, delay := llmclient.ShouldRetry(503, "10")
			Expect(retry).To(BeTrue())
			Expect(delay).To(Equal(time.Duration(0)))
		})

		It("returns zero delay for 429 with unparseable Retry-After", func() {
			retry, delay := llmclient.ShouldRetry(429, "not-a-number")
			Expect(retry).To(BeTrue())
			Expect(delay).To(Equal(time.Duration(0)))
		})
	})

	Describe("parseRetryAfter", func() {
		It("parses integer seconds", func() {
			Expect(llmclient.ParseRetryAfter("30")).To(Equal(30 * time.Second))
		})

		It("returns zero for empty string", func() {
			Expect(llmclient.ParseRetryAfter("")).To(Equal(time.Duration(0)))
		})

		It("returns zero for garbage input", func() {
			Expect(llmclient.ParseRetryAfter("abc")).To(Equal(time.Duration(0)))
		})

		It("returns zero for negative integer", func() {
			Expect(llmclient.ParseRetryAfter("-5")).To(Equal(time.Duration(0)))
		})
	})

	Describe("backoffDuration", func() {
		It("increases exponentially", func() {
			d0 := llmclient.BackoffDuration(0)
			d1 := llmclient.BackoffDuration(1)
			d2 := llmclient.BackoffDuration(2)

			Expect(d1).To(BeNumerically(">", d0))
			Expect(d2).To(BeNumerically(">", d1))
		})

		It("caps at the maximum backoff", func() {
			d10 := llmclient.BackoffDuration(10)
			Expect(d10).To(BeNumerically("<=", 30*time.Second))
		})

		It("adds jitter so consecutive calls differ", func() {
			seen := make(map[time.Duration]bool)
			for i := 0; i < 20; i++ {
				seen[llmclient.BackoffDuration(1)] = true
			}
			Expect(len(seen)).To(BeNumerically(">", 1))
		})
	})
})

var _ = Describe("Options", func() {

	Describe("WithTelemetry", func() {
		It("injects tracer and meter into the client", func() {
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = tp.Shutdown(context.Background()) })

			cfg := config.LLMConfig{
				GatewayURL: "http://localhost:11434",
				Timeout:    5,
			}
			client, err := llmclient.NewClient(cfg, llmclient.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()))
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { client.Close() })

			fields := llmclient.ExportTelemetryFields(client)
			Expect(fields.HasTracer).To(BeTrue())
			Expect(fields.HasMeter).To(BeTrue())
			Expect(fields.HasCompletionCounter).To(BeTrue())
			Expect(fields.HasCompletionLatency).To(BeTrue())
			Expect(fields.HasEmbedCounter).To(BeTrue())
			Expect(fields.HasEmbedLatency).To(BeTrue())
			Expect(fields.HasErrorCounter).To(BeTrue())
		})
	})

	Describe("WithAuditEmitter", func() {
		It("injects an audit emitter into the client", func() {
			cfg := config.LLMConfig{
				GatewayURL: "http://localhost:11434",
				Timeout:    5,
			}
			emitter := &mockAuditEmitter{}
			client, err := llmclient.NewClient(cfg, llmclient.WithAuditEmitter(emitter))
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { client.Close() })

			fields := llmclient.ExportTelemetryFields(client)
			Expect(fields.HasAuditEmitter).To(BeTrue())
		})
	})

	Describe("WithHTTPClient", func() {
		It("overrides the default HTTP client", func() {
			cfg := config.LLMConfig{
				GatewayURL: "http://localhost:11434",
				Timeout:    5,
			}
			httpClient := &http.Client{Timeout: 99 * time.Second}
			client, err := llmclient.NewClient(cfg, llmclient.WithHTTPClient(httpClient))
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { client.Close() })
		})
	})
})

var _ = Describe("Client", func() {

	// completionJSON builds a valid OpenAI chat completion response body.
	completionJSON := func() []byte {
		resp := map[string]any{
			"id":    "chatcmpl-test-1",
			"model": "gpt-4",
			"choices": []map[string]any{
				{
					"index":         0,
					"message":       map[string]string{"role": "assistant", "content": "Hello from GPT-4"},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]int{
				"prompt_tokens":     10,
				"completion_tokens": 5,
				"total_tokens":      15,
			},
		}
		data, _ := json.Marshal(resp)
		return data
	}

	// embeddingJSON builds a valid OpenAI embedding response body.
	embeddingJSON := func() []byte {
		resp := map[string]any{
			"data": []map[string]any{
				{"index": 0, "embedding": []float64{0.1, 0.2, 0.3}},
			},
			"model": "text-embedding-ada-002",
			"usage": map[string]int{
				"prompt_tokens": 8,
				"total_tokens":  8,
			},
		}
		data, _ := json.Marshal(resp)
		return data
	}

	Describe("Complete", func() {
		It("sends a well-formed request and parses the response", func() {
			var receivedMethod, receivedPath, receivedContentType, receivedAuth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				receivedMethod = r.Method
				receivedPath = r.URL.Path
				receivedContentType = r.Header.Get("Content-Type")
				receivedAuth = r.Header.Get("Authorization")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(completionJSON())
			}))
			DeferCleanup(srv.Close)

			os.Setenv("TEST_COMP_KEY", "sk-test-key")
			DeferCleanup(func() { os.Unsetenv("TEST_COMP_KEY") })

			cfg := config.LLMConfig{
				GatewayURL: srv.URL,
				APIKeyRef:  "env:TEST_COMP_KEY",
				Timeout:    5,
			}
			client, err := llmclient.NewClient(cfg)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { client.Close() })

			ctx := testspecs.SetupTenantContext("test-tenant")
			resp, err := client.Complete(ctx, &llmclient.CompletionRequest{
				Model:    "gpt-4",
				Messages: []llmclient.ChatMessage{{Role: "user", Content: "Hello"}},
				TenantID: "test-tenant",
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying the HTTP request")
			Expect(receivedMethod).To(Equal(http.MethodPost))
			Expect(receivedPath).To(Equal("/v1/chat/completions"))
			Expect(receivedContentType).To(Equal("application/json"))
			Expect(receivedAuth).To(Equal("Bearer sk-test-key"))

			By("verifying the response")
			Expect(resp.Choices).To(HaveLen(1))
			Expect(resp.Choices[0].Message.Content).To(Equal("Hello from GPT-4"))
			Expect(resp.Usage.TotalTokens).To(Equal(15))
		})

		It("rejects a request with empty TenantID", func() {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(completionJSON())
			}))
			DeferCleanup(srv.Close)

			cfg := config.LLMConfig{GatewayURL: srv.URL, Timeout: 5}
			client, err := llmclient.NewClient(cfg)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { client.Close() })

			_, err = client.Complete(context.Background(), &llmclient.CompletionRequest{
				Model:    "gpt-4",
				Messages: []llmclient.ChatMessage{{Role: "user", Content: "Hi"}},
				TenantID: "",
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("tenant"))
		})

		It("rejects a model not in the allowed list", func() {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(completionJSON())
			}))
			DeferCleanup(srv.Close)

			cfg := config.LLMConfig{
				GatewayURL:    srv.URL,
				Timeout:       5,
				AllowedModels: []string{"gpt-4", "gpt-3.5-turbo"},
			}
			client, err := llmclient.NewClient(cfg)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { client.Close() })

			ctx := testspecs.SetupTenantContext("test-tenant")
			_, err = client.Complete(ctx, &llmclient.CompletionRequest{
				Model:    "claude-3",
				Messages: []llmclient.ChatMessage{{Role: "user", Content: "Hi"}},
				TenantID: "test-tenant",
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not in allowed list"))
		})

		It("retries on HTTP 429 and succeeds on second attempt", func() {
			var attempts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				n := attempts.Add(1)
				if n == 1 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusTooManyRequests)
					fmt.Fprintf(w, `{"error":{"message":"rate limited","type":"rate_limit"}}`)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(completionJSON())
			}))
			DeferCleanup(srv.Close)

			cfg := config.LLMConfig{
				GatewayURL: srv.URL,
				Timeout:    10,
				MaxRetries: 3,
			}
			client, err := llmclient.NewClient(cfg)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { client.Close() })

			ctx := testspecs.SetupTenantContext("test-tenant")
			resp, err := client.Complete(ctx, &llmclient.CompletionRequest{
				Model:    "gpt-4",
				Messages: []llmclient.ChatMessage{{Role: "user", Content: "Hi"}},
				TenantID: "test-tenant",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(attempts.Load()).To(BeEquivalentTo(2))
			Expect(resp.Choices).To(HaveLen(1))
		})

		It("does not retry on HTTP 401", func() {
			var attempts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				attempts.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprintf(w, `{"error":{"message":"invalid key","type":"auth_error"}}`)
			}))
			DeferCleanup(srv.Close)

			cfg := config.LLMConfig{
				GatewayURL: srv.URL,
				Timeout:    5,
				MaxRetries: 3,
			}
			client, err := llmclient.NewClient(cfg)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { client.Close() })

			ctx := testspecs.SetupTenantContext("test-tenant")
			_, err = client.Complete(ctx, &llmclient.CompletionRequest{
				Model:    "gpt-4",
				Messages: []llmclient.ChatMessage{{Role: "user", Content: "Hi"}},
				TenantID: "test-tenant",
			})
			Expect(err).To(HaveOccurred())
			Expect(attempts.Load()).To(BeEquivalentTo(1))
		})

		It("rejects a request with empty messages", func() {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(completionJSON())
			}))
			DeferCleanup(srv.Close)

			cfg := config.LLMConfig{GatewayURL: srv.URL, Timeout: 5}
			client, err := llmclient.NewClient(cfg)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { client.Close() })

			ctx := testspecs.SetupTenantContext("test-tenant")
			_, err = client.Complete(ctx, &llmclient.CompletionRequest{
				Model:    "gpt-4",
				Messages: []llmclient.ChatMessage{},
				TenantID: "test-tenant",
			})
			Expect(err).To(HaveOccurred())
			Expect(err).To(MatchError(ContainSubstring("at least one message")))
			Expect(err.Error()).To(ContainSubstring("invalid LLM request"))
		})

		It("rejects a request with invalid TenantID format", func() {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(completionJSON())
			}))
			DeferCleanup(srv.Close)

			cfg := config.LLMConfig{GatewayURL: srv.URL, Timeout: 5}
			client, err := llmclient.NewClient(cfg)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { client.Close() })

			ctx := testspecs.SetupTenantContext("test-tenant")
			_, err = client.Complete(ctx, &llmclient.CompletionRequest{
				Model:    "gpt-4",
				Messages: []llmclient.ChatMessage{{Role: "user", Content: "Hi"}},
				TenantID: "UPPER_CASE!",
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("invalid LLM request"))
		})

		It("returns an error for malformed JSON response", func() {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("not json at all"))
			}))
			DeferCleanup(srv.Close)

			cfg := config.LLMConfig{GatewayURL: srv.URL, Timeout: 5}
			client, err := llmclient.NewClient(cfg)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { client.Close() })

			ctx := testspecs.SetupTenantContext("test-tenant")
			_, err = client.Complete(ctx, &llmclient.CompletionRequest{
				Model:    "gpt-4",
				Messages: []llmclient.ChatMessage{{Role: "user", Content: "Hi"}},
				TenantID: "test-tenant",
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("decoding response"))
		})

		It("fails after exhausting all retries on persistent 429", func() {
			var attempts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				attempts.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				fmt.Fprintf(w, `{"error":{"message":"rate limited","type":"rate_limit"}}`)
			}))
			DeferCleanup(srv.Close)

			cfg := config.LLMConfig{
				GatewayURL: srv.URL,
				Timeout:    30,
				MaxRetries: 2,
			}
			client, err := llmclient.NewClient(cfg)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { client.Close() })

			ctx := testspecs.SetupTenantContext("test-tenant")
			_, err = client.Complete(ctx, &llmclient.CompletionRequest{
				Model:    "gpt-4",
				Messages: []llmclient.ChatMessage{{Role: "user", Content: "retry me"}},
				TenantID: "test-tenant",
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed after"))

			By("verifying the correct number of attempts (initial + 2 retries = 3)")
			Expect(attempts.Load()).To(BeEquivalentTo(3))
		})

		It("returns a gateway error when the server is unreachable", func() {
			cfg := config.LLMConfig{
				GatewayURL: "http://127.0.0.1:1",
				Timeout:    1,
			}
			client, err := llmclient.NewClient(cfg)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { client.Close() })

			ctx := testspecs.SetupTenantContext("test-tenant")
			_, err = client.Complete(ctx, &llmclient.CompletionRequest{
				Model:    "gpt-4",
				Messages: []llmclient.ChatMessage{{Role: "user", Content: "Hi"}},
				TenantID: "test-tenant",
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("gateway"))
		})
	})

	Describe("Embed", func() {
		It("sends a well-formed request and parses the response", func() {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				Expect(r.URL.Path).To(Equal("/v1/embeddings"))
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(embeddingJSON())
			}))
			DeferCleanup(srv.Close)

			cfg := config.LLMConfig{GatewayURL: srv.URL, Timeout: 5}
			client, err := llmclient.NewClient(cfg)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { client.Close() })

			ctx := testspecs.SetupTenantContext("test-tenant")
			resp, err := client.Embed(ctx, &llmclient.EmbeddingRequest{
				Model:    "text-embedding-ada-002",
				Input:    []string{"hello world"},
				TenantID: "test-tenant",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Data).To(HaveLen(1))
			Expect(resp.Data[0].Embedding).To(HaveLen(3))
		})

		It("rejects model not in allowed list", func() {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(embeddingJSON())
			}))
			DeferCleanup(srv.Close)

			cfg := config.LLMConfig{
				GatewayURL:    srv.URL,
				Timeout:       5,
				AllowedModels: []string{"allowed-model"},
			}
			client, err := llmclient.NewClient(cfg)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { client.Close() })

			ctx := testspecs.SetupTenantContext("test-tenant")
			_, err = client.Embed(ctx, &llmclient.EmbeddingRequest{
				Model:    "disallowed-model",
				Input:    []string{"hello"},
				TenantID: "test-tenant",
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not in allowed list"))
		})

		It("rejects a request with empty input", func() {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(embeddingJSON())
			}))
			DeferCleanup(srv.Close)

			cfg := config.LLMConfig{GatewayURL: srv.URL, Timeout: 5}
			client, err := llmclient.NewClient(cfg)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { client.Close() })

			ctx := testspecs.SetupTenantContext("test-tenant")
			_, err = client.Embed(ctx, &llmclient.EmbeddingRequest{
				Model:    "text-embedding-ada-002",
				Input:    nil,
				TenantID: "test-tenant",
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("input"))
		})
	})

	Describe("Health", func() {
		It("returns nil when the gateway is reachable", func() {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				Expect(r.URL.Path).To(Equal("/v1/models"))
				w.WriteHeader(http.StatusOK)
				fmt.Fprintf(w, `{"data":[]}`)
			}))
			DeferCleanup(srv.Close)

			cfg := config.LLMConfig{GatewayURL: srv.URL, Timeout: 5}
			client, err := llmclient.NewClient(cfg)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { client.Close() })

			Expect(client.Health(context.Background())).To(Succeed())
		})

		It("returns an error when the gateway is unreachable", func() {
			cfg := config.LLMConfig{GatewayURL: "http://127.0.0.1:1", Timeout: 1}
			client, err := llmclient.NewClient(cfg)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { client.Close() })

			err = client.Health(context.Background())
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("Constructor", func() {
		It("requires a gateway URL", func() {
			_, err := llmclient.NewClient(config.LLMConfig{})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("gateway URL"))
		})

		It("resolves an API key from environment", func() {
			os.Setenv("TEST_CTOR_KEY", "sk-ctor-test")
			DeferCleanup(func() { os.Unsetenv("TEST_CTOR_KEY") })

			cfg := config.LLMConfig{
				GatewayURL: "http://localhost:11434",
				APIKeyRef:  "env:TEST_CTOR_KEY",
				Timeout:    5,
			}
			client, err := llmclient.NewClient(cfg)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { client.Close() })
		})

		It("fails on an unresolvable credential", func() {
			cfg := config.LLMConfig{
				GatewayURL: "http://localhost:11434",
				APIKeyRef:  "env:NONEXISTENT_KEY_XYZ",
				Timeout:    5,
			}
			_, err := llmclient.NewClient(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("credential"))
		})
	})
})

// =====================================================
// Level 2: Telemetry and Audit Integration
// =====================================================

var _ = Describe("Telemetry Integration", func() {
	var server *httptest.Server

	AfterEach(func() {
		if server != nil {
			server.Close()
		}
	})

	It("creates spans with correct attributes on Complete", func() {
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{
				"id": "chatcmpl-tel",
				"model": "gpt-4",
				"choices": [{"index": 0, "message": {"role": "assistant", "content": "traced"}, "finish_reason": "stop"}],
				"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
			}`)
		}))

		tp, err := telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = tp.Shutdown(context.Background()) })

		cfg := config.LLMConfig{GatewayURL: server.URL, Timeout: 5}
		client, err := llmclient.NewClient(cfg, llmclient.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { client.Close() })

		ctx := testspecs.SetupTenantContext("test-tenant")
		_, err = client.Complete(ctx, &llmclient.CompletionRequest{
			Model:    "gpt-4",
			Messages: []llmclient.ChatMessage{{Role: "user", Content: "hello"}},
			TenantID: "test-tenant",
		})
		Expect(err).NotTo(HaveOccurred())

		By("verifying the span name and attributes")
		spans := tp.GetSpans()
		span := telemetrytest.FindSpan(spans, "llmclient.Complete")
		Expect(span).NotTo(BeNil())

		tenantAttr, ok := telemetrytest.SpanAttribute(span, "tenant.id")
		Expect(ok).To(BeTrue())
		Expect(tenantAttr.AsString()).To(Equal("test-tenant"))

		modelAttr, ok := telemetrytest.SpanAttribute(span, "llm.model")
		Expect(ok).To(BeTrue())
		Expect(modelAttr.AsString()).To(Equal("gpt-4"))

		tokensAttr, ok := telemetrytest.SpanAttribute(span, "llm.tokens.total")
		Expect(ok).To(BeTrue())
		Expect(tokensAttr.AsInt64()).To(Equal(int64(15)))
	})

	It("includes prompt name and version as span attributes when set", func() {
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{
				"id": "chatcmpl-pspan",
				"model": "gpt-4",
				"choices": [{"index": 0, "message": {"role": "assistant", "content": "span-prov"}, "finish_reason": "stop"}],
				"usage": {"prompt_tokens": 4, "completion_tokens": 2, "total_tokens": 6}
			}`)
		}))

		tp, err := telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = tp.Shutdown(context.Background()) })

		cfg := config.LLMConfig{GatewayURL: server.URL, Timeout: 5}
		client, err := llmclient.NewClient(cfg, llmclient.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { client.Close() })

		ctx := testspecs.SetupTenantContext("test-tenant")
		_, err = client.Complete(ctx, &llmclient.CompletionRequest{
			Model:         "gpt-4",
			Messages:      []llmclient.ChatMessage{{Role: "user", Content: "prompt span test"}},
			TenantID:      "test-tenant",
			PromptName:    "structured-extract",
			PromptVersion: "2.0.0",
		})
		Expect(err).NotTo(HaveOccurred())

		spans := tp.GetSpans()
		span := telemetrytest.FindSpan(spans, "llmclient.Complete")
		Expect(span).NotTo(BeNil())

		nameAttr, ok := telemetrytest.SpanAttribute(span, "llm.prompt.name")
		Expect(ok).To(BeTrue())
		Expect(nameAttr.AsString()).To(Equal("structured-extract"))

		versionAttr, ok := telemetrytest.SpanAttribute(span, "llm.prompt.version")
		Expect(ok).To(BeTrue())
		Expect(versionAttr.AsString()).To(Equal("2.0.0"))
	})

	It("omits prompt span attributes when not set on request", func() {
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{
				"id": "chatcmpl-nospan",
				"model": "gpt-4",
				"choices": [{"index": 0, "message": {"role": "assistant", "content": "no span prov"}, "finish_reason": "stop"}],
				"usage": {"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}
			}`)
		}))

		tp, err := telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = tp.Shutdown(context.Background()) })

		cfg := config.LLMConfig{GatewayURL: server.URL, Timeout: 5}
		client, err := llmclient.NewClient(cfg, llmclient.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { client.Close() })

		ctx := testspecs.SetupTenantContext("test-tenant")
		_, err = client.Complete(ctx, &llmclient.CompletionRequest{
			Model:    "gpt-4",
			Messages: []llmclient.ChatMessage{{Role: "user", Content: "no prompt metadata"}},
			TenantID: "test-tenant",
		})
		Expect(err).NotTo(HaveOccurred())

		spans := tp.GetSpans()
		span := telemetrytest.FindSpan(spans, "llmclient.Complete")
		Expect(span).NotTo(BeNil())

		_, hasName := telemetrytest.SpanAttribute(span, "llm.prompt.name")
		Expect(hasName).To(BeFalse())

		_, hasVersion := telemetrytest.SpanAttribute(span, "llm.prompt.version")
		Expect(hasVersion).To(BeFalse())
	})

	It("creates spans with correct attributes on Embed", func() {
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{
				"data": [{"index": 0, "embedding": [0.1, 0.2, 0.3]}],
				"model": "text-embedding-ada-002",
				"usage": {"prompt_tokens": 3, "total_tokens": 3}
			}`)
		}))

		tp, err := telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = tp.Shutdown(context.Background()) })

		cfg := config.LLMConfig{GatewayURL: server.URL, Timeout: 5}
		client, err := llmclient.NewClient(cfg, llmclient.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { client.Close() })

		ctx := testspecs.SetupTenantContext("test-tenant")
		_, err = client.Embed(ctx, &llmclient.EmbeddingRequest{
			Model:    "text-embedding-ada-002",
			Input:    []string{"hello"},
			TenantID: "test-tenant",
		})
		Expect(err).NotTo(HaveOccurred())

		By("verifying the span name and attributes")
		spans := tp.GetSpans()
		span := telemetrytest.FindSpan(spans, "llmclient.Embed")
		Expect(span).NotTo(BeNil())

		inputCountAttr, ok := telemetrytest.SpanAttribute(span, "llm.input_count")
		Expect(ok).To(BeTrue())
		Expect(inputCountAttr.AsInt64()).To(Equal(int64(1)))
	})

	It("records error spans on failure", func() {
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"message":"bad request","type":"invalid_request_error"}}`)
		}))

		tp, err := telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = tp.Shutdown(context.Background()) })

		cfg := config.LLMConfig{GatewayURL: server.URL, Timeout: 5}
		client, err := llmclient.NewClient(cfg, llmclient.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { client.Close() })

		ctx := testspecs.SetupTenantContext("test-tenant")
		_, err = client.Complete(ctx, &llmclient.CompletionRequest{
			Model:    "gpt-4",
			Messages: []llmclient.ChatMessage{{Role: "user", Content: "fail"}},
			TenantID: "test-tenant",
		})
		Expect(err).To(HaveOccurred())

		By("verifying the span has error status")
		spans := tp.GetSpans()
		span := telemetrytest.FindSpan(spans, "llmclient.Complete")
		Expect(span).NotTo(BeNil())
		Expect(span.Status().Code).To(Equal(otelcodes.Error))
	})

	It("records metrics on successful completion", func() {
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{
				"id": "chatcmpl-met",
				"model": "gpt-4",
				"choices": [{"index": 0, "message": {"role": "assistant", "content": "metriced"}, "finish_reason": "stop"}],
				"usage": {"prompt_tokens": 5, "completion_tokens": 1, "total_tokens": 6}
			}`)
		}))

		tp, err := telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = tp.Shutdown(context.Background()) })

		cfg := config.LLMConfig{GatewayURL: server.URL, Timeout: 5}
		client, err := llmclient.NewClient(cfg, llmclient.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { client.Close() })

		ctx := testspecs.SetupTenantContext("test-tenant")
		_, err = client.Complete(ctx, &llmclient.CompletionRequest{
			Model:    "gpt-4",
			Messages: []llmclient.ChatMessage{{Role: "user", Content: "count me"}},
			TenantID: "test-tenant",
		})
		Expect(err).NotTo(HaveOccurred())

		By("verifying counter and histogram metrics")
		metrics := tp.GetMetrics()

		counter := telemetrytest.FindMetric(metrics, "llmclient.completions.total")
		Expect(counter).NotTo(BeNil())
		counterVal, err := telemetrytest.CounterValue(counter)
		Expect(err).NotTo(HaveOccurred())
		Expect(counterVal).To(Equal(int64(1)))

		histogram := telemetrytest.FindMetric(metrics, "llmclient.completion.duration_ms")
		Expect(histogram).NotTo(BeNil())
	})
})

var _ = Describe("Audit Emission", func() {
	var server *httptest.Server

	AfterEach(func() {
		if server != nil {
			server.Close()
		}
	})

	It("emits an audit event on successful completion", func() {
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{
				"id": "chatcmpl-aud",
				"model": "gpt-4",
				"choices": [{"index": 0, "message": {"role": "assistant", "content": "audited"}, "finish_reason": "stop"}],
				"usage": {"prompt_tokens": 4, "completion_tokens": 2, "total_tokens": 6}
			}`)
		}))

		emitter := &mockAuditEmitter{}
		cfg := config.LLMConfig{GatewayURL: server.URL, Timeout: 5}
		client, err := llmclient.NewClient(cfg, llmclient.WithAuditEmitter(emitter))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { client.Close() })

		ctx := testspecs.SetupTenantContext("test-tenant")
		_, err = client.Complete(ctx, &llmclient.CompletionRequest{
			Model:    "gpt-4",
			Messages: []llmclient.ChatMessage{{Role: "user", Content: "audit me"}},
			TenantID: "test-tenant",
			JobID:    "job-123",
		})
		Expect(err).NotTo(HaveOccurred())

		By("verifying the audit event fields")
		Expect(emitter.events).To(HaveLen(1))
		evt := emitter.events[0]
		Expect(evt.TenantID).To(Equal("test-tenant"))
		Expect(evt.JobID).To(Equal("job-123"))
		Expect(evt.Model).To(Equal("gpt-4"))
		Expect(evt.Operation).To(Equal("complete"))
		Expect(evt.Success).To(BeTrue())
		Expect(evt.TokensUsed).To(Equal(6))
		Expect(evt.PromptHash).NotTo(BeEmpty())
		Expect(evt.DurationMS).To(BeNumerically(">=", 0))
	})

	It("emits an audit event on failed completion", func() {
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"message":"invalid model","type":"invalid_request_error"}}`)
		}))

		emitter := &mockAuditEmitter{}
		cfg := config.LLMConfig{GatewayURL: server.URL, Timeout: 5}
		client, err := llmclient.NewClient(cfg, llmclient.WithAuditEmitter(emitter))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { client.Close() })

		ctx := testspecs.SetupTenantContext("test-tenant")
		_, err = client.Complete(ctx, &llmclient.CompletionRequest{
			Model:    "gpt-4",
			Messages: []llmclient.ChatMessage{{Role: "user", Content: "fail audit"}},
			TenantID: "test-tenant",
		})
		Expect(err).To(HaveOccurred())

		By("verifying the failure audit event")
		Expect(emitter.events).To(HaveLen(1))
		evt := emitter.events[0]
		Expect(evt.Success).To(BeFalse())
		Expect(evt.ErrorMessage).NotTo(BeEmpty())
	})

	It("emits an audit event on successful embedding", func() {
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{
				"data": [{"index": 0, "embedding": [0.1, 0.2]}],
				"model": "text-embedding-ada-002",
				"usage": {"prompt_tokens": 3, "total_tokens": 3}
			}`)
		}))

		emitter := &mockAuditEmitter{}
		cfg := config.LLMConfig{GatewayURL: server.URL, Timeout: 5}
		client, err := llmclient.NewClient(cfg, llmclient.WithAuditEmitter(emitter))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { client.Close() })

		ctx := testspecs.SetupTenantContext("test-tenant")
		_, err = client.Embed(ctx, &llmclient.EmbeddingRequest{
			Model:    "text-embedding-ada-002",
			Input:    []string{"embed me"},
			TenantID: "test-tenant",
		})
		Expect(err).NotTo(HaveOccurred())

		By("verifying the embedding audit event")
		Expect(emitter.events).To(HaveLen(1))
		evt := emitter.events[0]
		Expect(evt.Operation).To(Equal("embed"))
		Expect(evt.Success).To(BeTrue())
		Expect(evt.TokensUsed).To(Equal(3))
	})

	It("includes prompt name and version in audit event when set on request", func() {
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{
				"id": "chatcmpl-prov",
				"model": "gpt-4",
				"choices": [{"index": 0, "message": {"role": "assistant", "content": "provenance"}, "finish_reason": "stop"}],
				"usage": {"prompt_tokens": 4, "completion_tokens": 2, "total_tokens": 6}
			}`)
		}))

		emitter := &mockAuditEmitter{}
		cfg := config.LLMConfig{GatewayURL: server.URL, Timeout: 5}
		client, err := llmclient.NewClient(cfg, llmclient.WithAuditEmitter(emitter))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { client.Close() })

		ctx := testspecs.SetupTenantContext("test-tenant")
		_, err = client.Complete(ctx, &llmclient.CompletionRequest{
			Model:         "gpt-4",
			Messages:      []llmclient.ChatMessage{{Role: "user", Content: "provenance test"}},
			TenantID:      "test-tenant",
			PromptName:    "section-detect",
			PromptVersion: "1.0.0",
		})
		Expect(err).NotTo(HaveOccurred())

		By("verifying prompt metadata in audit event")
		Expect(emitter.events).To(HaveLen(1))
		evt := emitter.events[0]
		Expect(evt.PromptName).To(Equal("section-detect"))
		Expect(evt.PromptVersion).To(Equal("1.0.0"))
		Expect(evt.PromptHash).NotTo(BeEmpty())
	})

	It("leaves prompt fields empty in audit event when not set on request", func() {
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{
				"id": "chatcmpl-noprov",
				"model": "gpt-4",
				"choices": [{"index": 0, "message": {"role": "assistant", "content": "no prov"}, "finish_reason": "stop"}],
				"usage": {"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}
			}`)
		}))

		emitter := &mockAuditEmitter{}
		cfg := config.LLMConfig{GatewayURL: server.URL, Timeout: 5}
		client, err := llmclient.NewClient(cfg, llmclient.WithAuditEmitter(emitter))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { client.Close() })

		ctx := testspecs.SetupTenantContext("test-tenant")
		_, err = client.Complete(ctx, &llmclient.CompletionRequest{
			Model:    "gpt-4",
			Messages: []llmclient.ChatMessage{{Role: "user", Content: "no prompt metadata"}},
			TenantID: "test-tenant",
		})
		Expect(err).NotTo(HaveOccurred())

		By("verifying prompt fields are empty")
		Expect(emitter.events).To(HaveLen(1))
		evt := emitter.events[0]
		Expect(evt.PromptName).To(BeEmpty())
		Expect(evt.PromptVersion).To(BeEmpty())
	})

	It("produces empty prompt fields in audit event for embedding operations", func() {
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{
				"data": [{"index": 0, "embedding": [0.1, 0.2]}],
				"model": "text-embedding-ada-002",
				"usage": {"prompt_tokens": 3, "total_tokens": 3}
			}`)
		}))

		emitter := &mockAuditEmitter{}
		cfg := config.LLMConfig{GatewayURL: server.URL, Timeout: 5}
		client, err := llmclient.NewClient(cfg, llmclient.WithAuditEmitter(emitter))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { client.Close() })

		ctx := testspecs.SetupTenantContext("test-tenant")
		_, err = client.Embed(ctx, &llmclient.EmbeddingRequest{
			Model:    "text-embedding-ada-002",
			Input:    []string{"embed no prompt"},
			TenantID: "test-tenant",
		})
		Expect(err).NotTo(HaveOccurred())

		By("verifying embed audit has empty prompt fields")
		Expect(emitter.events).To(HaveLen(1))
		evt := emitter.events[0]
		Expect(evt.PromptName).To(BeEmpty())
		Expect(evt.PromptVersion).To(BeEmpty())
	})

	It("does not fail the primary operation when audit emission fails", func() {
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{
				"id": "chatcmpl-aud-fail",
				"model": "gpt-4",
				"choices": [{"index": 0, "message": {"role": "assistant", "content": "still works"}, "finish_reason": "stop"}],
				"usage": {"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}
			}`)
		}))

		emitter := &failingAuditEmitter{}
		cfg := config.LLMConfig{GatewayURL: server.URL, Timeout: 5}
		client, err := llmclient.NewClient(cfg, llmclient.WithAuditEmitter(emitter))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { client.Close() })

		ctx := testspecs.SetupTenantContext("test-tenant")
		resp, err := client.Complete(ctx, &llmclient.CompletionRequest{
			Model:    "gpt-4",
			Messages: []llmclient.ChatMessage{{Role: "user", Content: "audit fails"}},
			TenantID: "test-tenant",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Choices).To(HaveLen(1))
		Expect(resp.Choices[0].Message.Content).To(Equal("still works"))
	})
})

// =====================================================
// Level 3: Edge Cases and Negative Paths
// =====================================================

var _ = Describe("Edge Cases", func() {
	var server *httptest.Server

	AfterEach(func() {
		if server != nil {
			server.Close()
		}
	})

	It("uses DefaultModel from config when request model is empty", func() {
		var receivedBody map[string]any
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&receivedBody)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{
				"id": "chatcmpl-def",
				"model": "gpt-3.5-turbo",
				"choices": [{"index": 0, "message": {"role": "assistant", "content": "default"}, "finish_reason": "stop"}],
				"usage": {"prompt_tokens": 2, "completion_tokens": 1, "total_tokens": 3}
			}`)
		}))

		cfg := config.LLMConfig{
			GatewayURL:   server.URL,
			Timeout:      5,
			DefaultModel: "gpt-3.5-turbo",
		}
		client, err := llmclient.NewClient(cfg)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { client.Close() })

		ctx := testspecs.SetupTenantContext("test-tenant")
		_, err = client.Complete(ctx, &llmclient.CompletionRequest{
			Model:    "", // empty — should fall back to config default
			Messages: []llmclient.ChatMessage{{Role: "user", Content: "hi"}},
			TenantID: "test-tenant",
		})
		Expect(err).NotTo(HaveOccurred())

		By("verifying the server received the default model")
		Expect(receivedBody).To(HaveKeyWithValue("model", "gpt-3.5-turbo"))
	})

	It("uses EmbeddingModel from config when request model is empty", func() {
		var receivedBody map[string]any
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&receivedBody)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{
				"data": [{"index": 0, "embedding": [0.5]}],
				"model": "text-embedding-3-small",
				"usage": {"prompt_tokens": 1, "total_tokens": 1}
			}`)
		}))

		cfg := config.LLMConfig{
			GatewayURL:     server.URL,
			Timeout:        5,
			EmbeddingModel: "text-embedding-3-small",
		}
		client, err := llmclient.NewClient(cfg)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { client.Close() })

		ctx := testspecs.SetupTenantContext("test-tenant")
		_, err = client.Embed(ctx, &llmclient.EmbeddingRequest{
			Model:    "", // empty — should fall back to config default
			Input:    []string{"embed"},
			TenantID: "test-tenant",
		})
		Expect(err).NotTo(HaveOccurred())

		By("verifying the server received the embedding model")
		Expect(receivedBody).To(HaveKeyWithValue("model", "text-embedding-3-small"))
	})

	It("respects context cancellation during retry wait", func() {
		var attempts atomic.Int32
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			attempts.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"message":"rate limited","type":"rate_limit"}}`)
		}))

		cfg := config.LLMConfig{
			GatewayURL: server.URL,
			Timeout:    10,
			MaxRetries: 5,
		}
		client, err := llmclient.NewClient(cfg)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { client.Close() })

		ctx := testspecs.SetupTenantContext("test-tenant")
		ctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		DeferCleanup(cancel)

		_, err = client.Complete(ctx, &llmclient.CompletionRequest{
			Model:    "gpt-4",
			Messages: []llmclient.ChatMessage{{Role: "user", Content: "cancel me"}},
			TenantID: "test-tenant",
		})
		Expect(err).To(HaveOccurred())

		By("verifying fewer attempts than max retries were made")
		Expect(attempts.Load()).To(BeNumerically("<", 6)) // less than MaxRetries+1
	})

	It("produces deterministic hashes for identical messages", func() {
		msgs := []llmclient.ChatMessage{
			{Role: "user", Content: "hello world"},
		}
		hash1 := llmclient.ContentHash(msgs)
		hash2 := llmclient.ContentHash(msgs)
		Expect(hash1).To(Equal(hash2))
		Expect(hash1).NotTo(BeEmpty())
	})

	It("produces different hashes for different messages", func() {
		msgs1 := []llmclient.ChatMessage{{Role: "user", Content: "alpha"}}
		msgs2 := []llmclient.ChatMessage{{Role: "user", Content: "beta"}}
		hash1 := llmclient.ContentHash(msgs1)
		hash2 := llmclient.ContentHash(msgs2)
		Expect(hash1).NotTo(Equal(hash2))
	})

	It("includes code when present in APIError formatting", func() {
		apiErr := &llmclient.APIError{
			StatusCode: 400,
			Code:       "invalid_model",
			Message:    "model not found",
			Type:       "invalid_request_error",
		}
		Expect(apiErr.Error()).To(ContainSubstring("400"))
		Expect(apiErr.Error()).To(ContainSubstring("invalid_model"))
		Expect(apiErr.Error()).To(ContainSubstring("model not found"))
	})

	It("formats without code when absent in APIError", func() {
		apiErr := &llmclient.APIError{
			StatusCode: 500,
			Message:    "internal error",
		}
		errStr := apiErr.Error()
		Expect(errStr).To(ContainSubstring("500"))
		Expect(errStr).To(ContainSubstring("internal error"))
		Expect(errStr).NotTo(ContainSubstring("code="))
	})

	It("returns error for nil CompletionRequest", func() {
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

		cfg := config.LLMConfig{GatewayURL: server.URL, Timeout: 5}
		client, err := llmclient.NewClient(cfg)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { client.Close() })

		ctx := testspecs.SetupTenantContext("test-tenant")
		_, err = client.Complete(ctx, nil)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("nil"))
	})

	It("returns error for nil EmbeddingRequest", func() {
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

		cfg := config.LLMConfig{GatewayURL: server.URL, Timeout: 5}
		client, err := llmclient.NewClient(cfg)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { client.Close() })

		ctx := testspecs.SetupTenantContext("test-tenant")
		_, err = client.Embed(ctx, nil)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("nil"))
	})
})

// =====================================================
// Level 4: Gateway Mode
// =====================================================

var _ = Describe("Gateway Mode", func() {

	It("makes exactly one attempt when gateway_mode is true", func() {
		var attempts atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			attempts.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprintf(w, `{"error":{"message":"rate limited","type":"rate_limit"}}`)
		}))
		DeferCleanup(srv.Close)

		cfg := config.LLMConfig{
			GatewayURL:  srv.URL,
			GatewayMode: true,
			Timeout:     10,
			MaxRetries:  3,
		}
		client, err := llmclient.NewClient(cfg)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { client.Close() })

		ctx := testspecs.SetupTenantContext("test-tenant")
		_, err = client.Complete(ctx, &llmclient.CompletionRequest{
			Model:    "gpt-4",
			Messages: []llmclient.ChatMessage{{Role: "user", Content: "Hi"}},
			TenantID: "test-tenant",
		})
		Expect(err).To(HaveOccurred())
		Expect(attempts.Load()).To(BeEquivalentTo(1))
	})

	It("returns success on first attempt when gateway_mode is true", func() {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(gatewayModeCompletionJSON())
		}))
		DeferCleanup(srv.Close)

		cfg := config.LLMConfig{
			GatewayURL:  srv.URL,
			GatewayMode: true,
			Timeout:     10,
			MaxRetries:  0,
		}
		client, err := llmclient.NewClient(cfg)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { client.Close() })

		ctx := testspecs.SetupTenantContext("test-tenant")
		resp, err := client.Complete(ctx, &llmclient.CompletionRequest{
			Model:    "gpt-4",
			Messages: []llmclient.ChatMessage{{Role: "user", Content: "Hi"}},
			TenantID: "test-tenant",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Choices).To(HaveLen(1))
		Expect(resp.Choices[0].Message.Content).To(Equal("Hello from gateway mode."))
	})

	It("surfaces 5xx immediately without retry when gateway_mode is true", func() {
		var attempts atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			attempts.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, `{"error":{"message":"service unavailable","type":"server_error"}}`)
		}))
		DeferCleanup(srv.Close)

		cfg := config.LLMConfig{
			GatewayURL:  srv.URL,
			GatewayMode: true,
			Timeout:     10,
			MaxRetries:  3,
		}
		client, err := llmclient.NewClient(cfg)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { client.Close() })

		ctx := testspecs.SetupTenantContext("test-tenant")
		_, err = client.Complete(ctx, &llmclient.CompletionRequest{
			Model:    "gpt-4",
			Messages: []llmclient.ChatMessage{{Role: "user", Content: "Hi"}},
			TenantID: "test-tenant",
		})
		Expect(err).To(HaveOccurred())
		Expect(attempts.Load()).To(BeEquivalentTo(1))
	})

	It("preserves telemetry instrumentation in gateway mode", func() {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(gatewayModeCompletionJSON())
		}))
		DeferCleanup(srv.Close)

		tp, err := telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = tp.Shutdown(context.Background()) })

		cfg := config.LLMConfig{
			GatewayURL:  srv.URL,
			GatewayMode: true,
			Timeout:     10,
			MaxRetries:  0,
		}
		client, err := llmclient.NewClient(cfg, llmclient.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { client.Close() })

		ctx := testspecs.SetupTenantContext("test-tenant")
		_, err = client.Complete(ctx, &llmclient.CompletionRequest{
			Model:    "gpt-4",
			Messages: []llmclient.ChatMessage{{Role: "user", Content: "Hi"}},
			TenantID: "test-tenant",
		})
		Expect(err).NotTo(HaveOccurred())

		spans := tp.GetSpans()
		span := telemetrytest.FindSpan(spans, "llmclient.Complete")
		Expect(span).NotTo(BeNil())
	})

	It("preserves audit emission in gateway mode", func() {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(gatewayModeCompletionJSON())
		}))
		DeferCleanup(srv.Close)

		emitter := &mockAuditEmitter{}
		cfg := config.LLMConfig{
			GatewayURL:  srv.URL,
			GatewayMode: true,
			Timeout:     10,
			MaxRetries:  0,
		}
		client, err := llmclient.NewClient(cfg, llmclient.WithAuditEmitter(emitter))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { client.Close() })

		ctx := testspecs.SetupTenantContext("test-tenant")
		_, err = client.Complete(ctx, &llmclient.CompletionRequest{
			Model:    "gpt-4",
			Messages: []llmclient.ChatMessage{{Role: "user", Content: "Hi"}},
			TenantID: "test-tenant",
			JobID:    "gw-job-1",
		})
		Expect(err).NotTo(HaveOccurred())

		Expect(emitter.events).To(HaveLen(1))
		evt := emitter.events[0]
		Expect(evt.TenantID).To(Equal("test-tenant"))
		Expect(evt.JobID).To(Equal("gw-job-1"))
		Expect(evt.Model).To(Equal("gpt-4"))
		Expect(evt.Operation).To(Equal("complete"))
		Expect(evt.Success).To(BeTrue())
	})

	It("does not log warnings when gateway_mode=true with normalized max_retries", func() {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(gatewayModeCompletionJSON())
		}))
		DeferCleanup(srv.Close)

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
		origDefault := slog.Default()
		slog.SetDefault(logger)
		DeferCleanup(func() { slog.SetDefault(origDefault) })

		cfg := config.LLMConfig{
			GatewayURL:  srv.URL,
			GatewayMode: true,
			Timeout:     10,
			MaxRetries:  0,
		}
		c, err := llmclient.NewClient(cfg)
		Expect(err).NotTo(HaveOccurred())
		Expect(c).NotTo(BeNil())

		Expect(buf.String()).To(BeEmpty())
	})

	It("does not retry gateway_mode=false (baseline)", func() {
		var attempts atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			n := attempts.Add(1)
			if n == 1 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				fmt.Fprintf(w, `{"error":{"message":"rate limited","type":"rate_limit"}}`)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(gatewayModeCompletionJSON())
		}))
		DeferCleanup(srv.Close)

		cfg := config.LLMConfig{
			GatewayURL:  srv.URL,
			GatewayMode: false,
			Timeout:     10,
			MaxRetries:  3,
		}
		client, err := llmclient.NewClient(cfg)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { client.Close() })

		ctx := testspecs.SetupTenantContext("test-tenant")
		resp, err := client.Complete(ctx, &llmclient.CompletionRequest{
			Model:    "gpt-4",
			Messages: []llmclient.ChatMessage{{Role: "user", Content: "Hi"}},
			TenantID: "test-tenant",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(attempts.Load()).To(BeEquivalentTo(2))
		Expect(resp.Choices).To(HaveLen(1))
	})
})

// mockAuditEmitter captures audit events for testing. Safe for concurrent use.
type mockAuditEmitter struct {
	mu     sync.Mutex
	events []*llmclient.AuditEvent
}

func (m *mockAuditEmitter) EmitLLMAudit(_ context.Context, event *llmclient.AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, event)
	return nil
}

// failingAuditEmitter always returns an error, for testing best-effort emission.
type failingAuditEmitter struct{}

func (f *failingAuditEmitter) EmitLLMAudit(_ context.Context, _ *llmclient.AuditEvent) error {
	return fmt.Errorf("simulated audit failure")
}
