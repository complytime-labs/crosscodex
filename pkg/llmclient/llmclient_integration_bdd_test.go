//go:build integration_llm

package llmclient_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
)

// Suite bootstrap lives in llmclient_bdd_test.go — do NOT add RunSpecs or BeforeSuite here.

const (
	// Default models — override via TEST_LLM_CHAT_MODEL / TEST_LLM_EMBED_MODEL.
	integrationDefaultChatModel  = "llama3.1:8b"
	integrationDefaultEmbedModel = "granite-embedding:30m"

	// Timeout for pulling a model that is not present.
	integrationModelPullTimeout = 10 * time.Minute
)

var (
	integrationOllamaHost string
	integrationChatModel  string // Model name for tests (may be a LiteLLM alias in full-stack mode).
	integrationEmbedModel string // Embed model name for tests (may be a LiteLLM alias in full-stack mode).

	// Actual Ollama model names for direct-connect tests.
	// In full-stack mode these differ from chatModel/embedModel (which are aliases).
	integrationOllamaChatModel  string
	integrationOllamaEmbedModel string

	// Full-stack mode (LiteLLM + Ollama + Jaeger).
	integrationLitellmURL     string
	integrationOtlpEndpoint   string
	integrationJaegerQueryURL string
)

// probeOllamaIntegration checks that Ollama responds at the given host.
func probeOllamaIntegration(ctx context.Context, host string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, host+"/api/tags", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return nil
}

// ensureModelIntegration checks if a model is available and pulls it if not.
func ensureModelIntegration(host, model string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, host+"/api/tags", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var tags struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return fmt.Errorf("decoding model list: %w", err)
	}

	for _, m := range tags.Models {
		if m.Name == model {
			return nil
		}
	}

	fmt.Fprintf(os.Stderr, "Model %q not found on Ollama, pulling (this may take several minutes)...\n", model)
	return pullModelIntegration(host, model)
}

// pullModelIntegration pulls a model from the Ollama registry.
func pullModelIntegration(host, model string) error {
	ctx, cancel := context.WithTimeout(context.Background(), integrationModelPullTimeout)
	defer cancel()

	payload, _ := json.Marshal(map[string]any{
		"name":   model,
		"stream": false,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+"/api/pull", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("pull request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("pull returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	// Drain the response body (Ollama streams JSON lines even with stream:false).
	_, _ = io.ReadAll(resp.Body)

	fmt.Fprintf(os.Stderr, "Model %q pulled successfully.\n", model)
	return nil
}

// newIntegrationTestClient creates a configured llmclient.Client pointing at the Ollama instance.
// Uses actual Ollama model names (not LiteLLM aliases) for direct-connect tests.
func newIntegrationTestClient(opts ...llmclient.Option) llmclient.Client {
	cfg := config.LLMConfig{
		GatewayURL:     integrationOllamaHost,
		DefaultModel:   integrationOllamaChatModel,
		EmbeddingModel: integrationOllamaEmbedModel,
		Timeout:        120,
		MaxRetries:     1,
	}
	c, err := llmclient.NewClient(cfg, opts...)
	Expect(err).NotTo(HaveOccurred(), "NewClient")
	DeferCleanup(func() { _ = c.Close() })
	return c
}

// newIntegrationGatewayClient creates a llmclient.Client pointing at the LiteLLM proxy
// with gateway_mode enabled. For full-stack integration tests only.
func newIntegrationGatewayClient(opts ...llmclient.Option) llmclient.Client {
	if integrationLitellmURL == "" {
		Skip("TEST_LITELLM_URL not set; skipping gateway mode test")
	}
	cfg := config.LLMConfig{
		GatewayURL:     integrationLitellmURL,
		GatewayMode:    true,
		DefaultModel:   integrationChatModel,
		EmbeddingModel: integrationEmbedModel,
		Timeout:        120,
		MaxRetries:     0,
	}
	c, err := llmclient.NewClient(cfg, opts...)
	Expect(err).NotTo(HaveOccurred(), "NewClient (gateway)")
	DeferCleanup(func() { _ = c.Close() })
	return c
}

// integrationCaptureEmitter records emitted audit events for test assertions. Safe for concurrent use.
type integrationCaptureEmitter struct {
	mu     sync.Mutex
	events []*llmclient.AuditEvent
}

func (e *integrationCaptureEmitter) EmitLLMAudit(_ context.Context, event *llmclient.AuditEvent) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, event)
	return nil
}

var _ = Describe("LLM Client Integration", Ordered, func() {
	BeforeAll(func() {
		integrationLitellmURL = os.Getenv("TEST_LITELLM_URL")
		integrationOtlpEndpoint = os.Getenv("TEST_OTLP_GRPC_ENDPOINT")
		integrationJaegerQueryURL = os.Getenv("TEST_JAEGER_QUERY_URL")

		if integrationLitellmURL != "" {
			// Full-stack mode: LiteLLM + Ollama + Jaeger.
			integrationOllamaHost = os.Getenv("TEST_OLLAMA_HOST")
			if integrationOllamaHost == "" {
				integrationOllamaHost = "http://localhost:11434"
			}
		} else {
			integrationOllamaHost = os.Getenv("OLLAMA_HOST")
			if integrationOllamaHost == "" {
				Skip("Neither TEST_LITELLM_URL nor OLLAMA_HOST set — skipping llmclient integration tests. " +
					"Set OLLAMA_HOST for direct-connect tests, or TEST_LITELLM_URL for full-stack tests.")
			}
		}

		// Normalize scheme.
		if !strings.HasPrefix(integrationOllamaHost, "http://") && !strings.HasPrefix(integrationOllamaHost, "https://") {
			integrationOllamaHost = "http://" + integrationOllamaHost
		}

		integrationChatModel = os.Getenv("TEST_LLM_CHAT_MODEL")
		if integrationChatModel == "" {
			integrationChatModel = integrationDefaultChatModel
		}
		integrationEmbedModel = os.Getenv("TEST_LLM_EMBED_MODEL")
		if integrationEmbedModel == "" {
			integrationEmbedModel = integrationDefaultEmbedModel
		}

		// In full-stack mode, chatModel/embedModel are LiteLLM aliases (e.g. "small").
		// Direct-connect tests talk to Ollama and need actual model names.
		if integrationLitellmURL != "" {
			integrationOllamaChatModel = "llama3.2:1b"
			integrationOllamaEmbedModel = "granite-embedding:30m"
		} else {
			integrationOllamaChatModel = integrationChatModel
			integrationOllamaEmbedModel = integrationEmbedModel
		}

		// Verify Ollama is reachable (probe directly, not through LiteLLM).
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		Expect(probeOllamaIntegration(ctx, integrationOllamaHost)).To(Succeed(),
			"Ollama probe failed at %s", integrationOllamaHost)

		// Pull actual Ollama model names (not LiteLLM aliases).
		Expect(ensureModelIntegration(integrationOllamaHost, integrationOllamaChatModel)).To(Succeed(),
			"Failed to ensure chat model %q", integrationOllamaChatModel)
		Expect(ensureModelIntegration(integrationOllamaHost, integrationOllamaEmbedModel)).To(Succeed(),
			"Failed to ensure embed model %q", integrationOllamaEmbedModel)
	})
	Context("Direct Ollama Tests", func() {
		It("should pass health check", func() {
			client := newIntegrationTestClient()
			Expect(client.Health(context.Background())).To(Succeed())
		})

		It("should complete a chat request", func() {
			client := newIntegrationTestClient()
			ctx := context.Background()

			resp, err := client.Complete(ctx, &llmclient.CompletionRequest{
				Model:    integrationOllamaChatModel,
				TenantID: "test-tenant-a",
				Messages: []llmclient.ChatMessage{
					{Role: "user", Content: "Reply with exactly: hello"},
				},
				MaxTokens: 32,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Choices).NotTo(BeEmpty())
			Expect(resp.Choices[0].Message.Content).NotTo(BeEmpty())
			Expect(resp.Usage.TotalTokens).NotTo(BeZero())
			GinkgoWriter.Printf("Response: %q (tokens: %d)\n", resp.Choices[0].Message.Content, resp.Usage.TotalTokens)
		})

		It("should include telemetry spans for completion", func() {
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())

			client := newIntegrationTestClient(llmclient.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()))
			ctx := context.Background()

			resp, err := client.Complete(ctx, &llmclient.CompletionRequest{
				Model:    integrationOllamaChatModel,
				TenantID: "test-tenant-a",
				Messages: []llmclient.ChatMessage{
					{Role: "user", Content: "Reply with exactly: hello"},
				},
				MaxTokens: 32,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Usage.TotalTokens).NotTo(BeZero())

			spans := tp.GetSpans()
			span := telemetrytest.FindSpan(spans, "llmclient.Complete")
			Expect(span).NotTo(BeNil(), "expected span named llmclient.Complete")

			tenantAttr, ok := telemetrytest.SpanAttribute(span, "tenant.id")
			Expect(ok).To(BeTrue(), "expected tenant.id attribute on span")
			Expect(tenantAttr.AsString()).To(Equal("test-tenant-a"))

			modelAttr, ok := telemetrytest.SpanAttribute(span, "llm.model")
			Expect(ok).To(BeTrue(), "expected llm.model attribute on span")
			Expect(modelAttr.AsString()).To(Equal(integrationOllamaChatModel))

			tokensAttr, ok := telemetrytest.SpanAttribute(span, "llm.tokens.total")
			Expect(ok).To(BeTrue(), "expected llm.tokens.total attribute on span")
			Expect(tokensAttr.AsInt64()).NotTo(BeZero())
		})

		It("should emit audit events for completion", func() {
			emitter := &integrationCaptureEmitter{}
			client := newIntegrationTestClient(llmclient.WithAuditEmitter(emitter))
			ctx := context.Background()

			_, err := client.Complete(ctx, &llmclient.CompletionRequest{
				Model:    integrationOllamaChatModel,
				TenantID: "test-tenant-a",
				JobID:    "job-integration-001",
				Messages: []llmclient.ChatMessage{
					{Role: "user", Content: "Reply with exactly: hello"},
				},
				MaxTokens: 32,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(emitter.events).NotTo(BeEmpty())

			event := emitter.events[0]
			Expect(event.TenantID).To(Equal("test-tenant-a"))
			Expect(event.JobID).To(Equal("job-integration-001"))
			Expect(event.Operation).To(Equal("complete"))
			Expect(event.Success).To(BeTrue(), "expected success=true, error: %s", event.ErrorMessage)
			Expect(event.PromptHash).NotTo(BeEmpty())
			Expect(event.TokensUsed).NotTo(BeZero())
			Expect(event.DurationMS).To(BeNumerically(">", 0))
		})

		It("should generate embeddings", func() {
			client := newIntegrationTestClient()
			ctx := context.Background()

			resp, err := client.Embed(ctx, &llmclient.EmbeddingRequest{
				Model:    integrationOllamaEmbedModel,
				Input:    []string{"The quick brown fox", "jumps over the lazy dog"},
				TenantID: "test-tenant-a",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Data).To(HaveLen(2))
			Expect(resp.Data[0].Embedding).NotTo(BeEmpty())
			GinkgoWriter.Printf("Embedding dimensions: %d\n", len(resp.Data[0].Embedding))
		})

		It("should include telemetry spans for embedding", func() {
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())

			client := newIntegrationTestClient(llmclient.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()))
			ctx := context.Background()

			_, err = client.Embed(ctx, &llmclient.EmbeddingRequest{
				Model:    integrationOllamaEmbedModel,
				Input:    []string{"test embedding text"},
				TenantID: "test-tenant-a",
			})
			Expect(err).NotTo(HaveOccurred())

			spans := tp.GetSpans()
			span := telemetrytest.FindSpan(spans, "llmclient.Embed")
			Expect(span).NotTo(BeNil(), "expected span named llmclient.Embed")

			modelAttr, ok := telemetrytest.SpanAttribute(span, "llm.model")
			Expect(ok).To(BeTrue(), "expected llm.model attribute on span")
			Expect(modelAttr.AsString()).To(Equal(integrationOllamaEmbedModel))

			inputCountAttr, ok := telemetrytest.SpanAttribute(span, "llm.input_count")
			Expect(ok).To(BeTrue(), "expected llm.input_count attribute on span")
			Expect(inputCountAttr.AsInt64()).To(Equal(int64(1)))
		})

		It("should reject models not in allow-list", func() {
			cfg := config.LLMConfig{
				GatewayURL:     integrationOllamaHost,
				DefaultModel:   integrationOllamaChatModel,
				EmbeddingModel: integrationOllamaEmbedModel,
				Timeout:        30,
				MaxRetries:     0,
				AllowedModels:  []string{"some-other-model"},
			}
			client, err := llmclient.NewClient(cfg)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { client.Close() })

			_, err = client.Complete(context.Background(), &llmclient.CompletionRequest{
				Model:    integrationOllamaChatModel,
				TenantID: "test-tenant-a",
				Messages: []llmclient.ChatMessage{
					{Role: "user", Content: "hello"},
				},
			})
			Expect(err).To(HaveOccurred())
			GinkgoWriter.Printf("Got expected error: %v\n", err)
		})
	})

	Context("Full-stack Gateway Mode Tests (LiteLLM + Ollama + Jaeger)", func() {
		It("should pass gateway health check", func() {
			client := newIntegrationGatewayClient()
			Expect(client.Health(context.Background())).To(Succeed())
		})

		It("should route through model aliases", func() {
			if integrationLitellmURL == "" {
				Skip("TEST_LITELLM_URL not set; skipping gateway mode test")
			}

			aliases := []string{"small", "medium", "large", "judge"}
			for _, alias := range aliases {
				By(fmt.Sprintf("testing alias %q", alias))
				cfg := config.LLMConfig{
					GatewayURL:     integrationLitellmURL,
					GatewayMode:    true,
					DefaultModel:   alias,
					EmbeddingModel: integrationEmbedModel,
					Timeout:        120,
					MaxRetries:     0,
				}
				client, err := llmclient.NewClient(cfg)
				Expect(err).NotTo(HaveOccurred(), "NewClient (alias %s)", alias)
				defer client.Close()

				resp, err := client.Complete(context.Background(), &llmclient.CompletionRequest{
					Model:    alias,
					TenantID: "test-tenant-a",
					Messages: []llmclient.ChatMessage{
						{Role: "user", Content: "Reply with exactly: hello"},
					},
					MaxTokens: 32,
				})
				Expect(err).NotTo(HaveOccurred(), "Complete via alias %q", alias)
				Expect(resp.Choices).NotTo(BeEmpty(), "alias %q: expected at least one choice", alias)
				Expect(resp.Choices[0].Message.Content).NotTo(BeEmpty(), "alias %q: expected non-empty response", alias)
				GinkgoWriter.Printf("alias %q: %q\n", alias, resp.Choices[0].Message.Content)
			}
		})

		It("should generate embeddings via gateway", func() {
			client := newIntegrationGatewayClient()

			resp, err := client.Embed(context.Background(), &llmclient.EmbeddingRequest{
				Model:    integrationEmbedModel,
				Input:    []string{"compliance mapping test input"},
				TenantID: "test-tenant-a",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Data).NotTo(BeEmpty())
			Expect(resp.Data[0].Embedding).NotTo(BeEmpty())
			GinkgoWriter.Printf("Embedding dimensions: %d\n", len(resp.Data[0].Embedding))
		})

		It("should not retry in gateway mode", func() {
			if integrationLitellmURL == "" {
				Skip("TEST_LITELLM_URL not set; skipping gateway mode test")
			}

			cfg := config.LLMConfig{
				GatewayURL:     integrationLitellmURL,
				GatewayMode:    true,
				DefaultModel:   "nonexistent-model-xyz",
				EmbeddingModel: integrationEmbedModel,
				Timeout:        30,
				MaxRetries:     0,
			}
			client, err := llmclient.NewClient(cfg)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { client.Close() })

			start := time.Now()
			_, err = client.Complete(context.Background(), &llmclient.CompletionRequest{
				Model:    "nonexistent-model-xyz",
				TenantID: "test-tenant-a",
				Messages: []llmclient.ChatMessage{
					{Role: "user", Content: "hello"},
				},
				MaxTokens: 8,
			})
			elapsed := time.Since(start)

			Expect(err).To(HaveOccurred())
			Expect(elapsed).To(BeNumerically("<", 10*time.Second),
				"request took %v; expected < 10s (no retry backoff in gateway mode)", elapsed)
			GinkgoWriter.Printf("Got expected error in %v: %v\n", elapsed, err)
		})

		It("should export telemetry to Jaeger", func() {
			if integrationLitellmURL == "" {
				Skip("TEST_LITELLM_URL not set; skipping gateway mode test")
			}
			if integrationOtlpEndpoint == "" {
				Skip("TEST_OTLP_GRPC_ENDPOINT not set; skipping telemetry export test")
			}
			if integrationJaegerQueryURL == "" {
				Skip("TEST_JAEGER_QUERY_URL not set; skipping Jaeger query test")
			}

			ctx := context.Background()
			serviceName := "crosscodex-llm-integration"

			obsCfg := config.ObservabilityConfig{
				Endpoint: integrationOtlpEndpoint,
				Protocol: "grpc",
				Tracing: config.ObservabilityTracingConfig{
					SampleRate: 1.0,
				},
			}
			shutdown, err := telemetry.Init(ctx, obsCfg,
				telemetry.WithServiceName(serviceName),
				telemetry.WithServiceVersion("test"),
			)
			Expect(err).NotTo(HaveOccurred(), "telemetry.Init")

			client := newIntegrationGatewayClient(
				llmclient.WithTelemetry(otel.GetTracerProvider(), otel.GetMeterProvider()),
			)

			_, err = client.Complete(ctx, &llmclient.CompletionRequest{
				Model:    integrationChatModel,
				TenantID: "test-tenant-a",
				Messages: []llmclient.ChatMessage{
					{Role: "user", Content: "Reply with exactly: hello"},
				},
				MaxTokens: 32,
			})
			Expect(err).NotTo(HaveOccurred())

			// Flush traces to the collector. Shutdown may report a metrics export
			// error because Jaeger all-in-one only implements the OTLP traces
			// service, not the metrics service. Traces still get flushed.
			if shutdownErr := shutdown(ctx); shutdownErr != nil {
				GinkgoWriter.Printf("telemetry shutdown (expected metrics error with Jaeger): %v\n", shutdownErr)
			}

			// Poll Jaeger Query API for traces from our service.
			queryURL := fmt.Sprintf("%s/api/traces?service=%s&limit=1", integrationJaegerQueryURL, serviceName)
			var found bool
			for attempt := 0; attempt < 10; attempt++ {
				time.Sleep(2 * time.Second)

				req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, queryURL, nil)
				if reqErr != nil {
					GinkgoWriter.Printf("attempt %d: request build error: %v\n", attempt, reqErr)
					continue
				}
				resp, respErr := http.DefaultClient.Do(req)
				if respErr != nil {
					GinkgoWriter.Printf("attempt %d: query error: %v\n", attempt, respErr)
					continue
				}

				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()

				var result struct {
					Data []json.RawMessage `json:"data"`
				}
				if unmarshalErr := json.Unmarshal(body, &result); unmarshalErr != nil {
					GinkgoWriter.Printf("attempt %d: unmarshal error: %v\n", attempt, unmarshalErr)
					continue
				}
				if len(result.Data) > 0 {
					found = true
					GinkgoWriter.Printf("Found %d trace(s) in Jaeger after %d attempt(s)\n", len(result.Data), attempt+1)
					break
				}
			}
			Expect(found).To(BeTrue(), "no traces found in Jaeger for service "+serviceName)
		})

		It("should emit audit events via gateway", func() {
			emitter := &integrationCaptureEmitter{}
			client := newIntegrationGatewayClient(llmclient.WithAuditEmitter(emitter))

			_, err := client.Complete(context.Background(), &llmclient.CompletionRequest{
				Model:    integrationChatModel,
				TenantID: "test-tenant-a",
				Messages: []llmclient.ChatMessage{
					{Role: "user", Content: "Reply with exactly: hello"},
				},
				MaxTokens: 32,
			})
			Expect(err).NotTo(HaveOccurred())

			emitter.mu.Lock()
			defer emitter.mu.Unlock()

			Expect(emitter.events).To(HaveLen(1))
			ev := emitter.events[0]
			Expect(ev.TenantID).To(Equal("test-tenant-a"))
			Expect(ev.Model).To(Equal(integrationChatModel))
			Expect(ev.Success).To(BeTrue(), "expected success=true, error: %s", ev.ErrorMessage)
			Expect(ev.DurationMS).To(BeNumerically(">", 0))
			Expect(ev.PromptHash).NotTo(BeEmpty())
		})

		It("should enforce model allow-list in gateway mode", func() {
			if integrationLitellmURL == "" {
				Skip("TEST_LITELLM_URL not set; skipping gateway mode test")
			}

			cfg := config.LLMConfig{
				GatewayURL:     integrationLitellmURL,
				GatewayMode:    true,
				DefaultModel:   integrationChatModel,
				EmbeddingModel: integrationEmbedModel,
				Timeout:        30,
				MaxRetries:     0,
				AllowedModels:  []string{"small", "embed"},
			}
			client, err := llmclient.NewClient(cfg)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { client.Close() })

			_, err = client.Complete(context.Background(), &llmclient.CompletionRequest{
				Model:    "judge",
				TenantID: "test-tenant-a",
				Messages: []llmclient.ChatMessage{
					{Role: "user", Content: "hello"},
				},
				MaxTokens: 8,
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not in allowed list"))
			GinkgoWriter.Printf("Got expected allow-list error: %v\n", err)
		})
	})
})
