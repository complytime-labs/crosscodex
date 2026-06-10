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
	"testing"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
)

const (
	// Default models — override via TEST_LLM_CHAT_MODEL / TEST_LLM_EMBED_MODEL.
	defaultChatModel  = "llama3.1:8b"
	defaultEmbedModel = "granite-embedding:30m"

	// Timeout for pulling a model that is not present.
	modelPullTimeout = 10 * time.Minute
)

var (
	ollamaHost string
	chatModel  string // Model name for tests (may be a LiteLLM alias in full-stack mode).
	embedModel string // Embed model name for tests (may be a LiteLLM alias in full-stack mode).

	// Actual Ollama model names for direct-connect tests.
	// In full-stack mode these differ from chatModel/embedModel (which are aliases).
	ollamaChatModel  string
	ollamaEmbedModel string

	// Full-stack mode (LiteLLM + Ollama + Jaeger).
	litellmURL     string
	otlpEndpoint   string
	jaegerQueryURL string
)

func TestMain(m *testing.M) {
	litellmURL = os.Getenv("TEST_LITELLM_URL")
	otlpEndpoint = os.Getenv("TEST_OTLP_GRPC_ENDPOINT")
	jaegerQueryURL = os.Getenv("TEST_JAEGER_QUERY_URL")

	if litellmURL != "" {
		// Full-stack mode: LiteLLM + Ollama + Jaeger.
		ollamaHost = os.Getenv("TEST_OLLAMA_HOST")
		if ollamaHost == "" {
			ollamaHost = "http://localhost:11434"
		}
	} else {
		ollamaHost = os.Getenv("OLLAMA_HOST")
		if ollamaHost == "" {
			fmt.Fprintln(os.Stderr, "Neither TEST_LITELLM_URL nor OLLAMA_HOST set — skipping llmclient integration tests.")
			fmt.Fprintln(os.Stderr, "Set OLLAMA_HOST for direct-connect tests, or TEST_LITELLM_URL for full-stack tests.")
			os.Exit(0)
		}
	}

	// Normalize scheme.
	if !strings.HasPrefix(ollamaHost, "http://") && !strings.HasPrefix(ollamaHost, "https://") {
		ollamaHost = "http://" + ollamaHost
	}

	chatModel = os.Getenv("TEST_LLM_CHAT_MODEL")
	if chatModel == "" {
		chatModel = defaultChatModel
	}
	embedModel = os.Getenv("TEST_LLM_EMBED_MODEL")
	if embedModel == "" {
		embedModel = defaultEmbedModel
	}

	// In full-stack mode, chatModel/embedModel are LiteLLM aliases (e.g. "small").
	// Direct-connect tests talk to Ollama and need actual model names.
	if litellmURL != "" {
		ollamaChatModel = "llama3.2:1b"
		ollamaEmbedModel = "granite-embedding:30m"
	} else {
		ollamaChatModel = chatModel
		ollamaEmbedModel = embedModel
	}

	// Verify Ollama is reachable (probe directly, not through LiteLLM).
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := probeOllama(ctx, ollamaHost); err != nil {
		fmt.Fprintf(os.Stderr, "Ollama probe failed at %s: %v\n", ollamaHost, err)
		os.Exit(1)
	}

	// Pull actual Ollama model names (not LiteLLM aliases).
	if err := ensureModel(ollamaHost, ollamaChatModel); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to ensure chat model %q: %v\n", ollamaChatModel, err)
		os.Exit(1)
	}
	if err := ensureModel(ollamaHost, ollamaEmbedModel); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to ensure embed model %q: %v\n", ollamaEmbedModel, err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

// probeOllama checks that Ollama responds at the given host.
func probeOllama(ctx context.Context, host string) error {
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

// ensureModel checks if a model is available and pulls it if not.
func ensureModel(host, model string) error {
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
	return pullModel(host, model)
}

// pullModel pulls a model from the Ollama registry.
func pullModel(host, model string) error {
	ctx, cancel := context.WithTimeout(context.Background(), modelPullTimeout)
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

// newTestClient creates a configured llmclient.Client pointing at the Ollama instance.
// Uses actual Ollama model names (not LiteLLM aliases) for direct-connect tests.
func newTestClient(t *testing.T, opts ...llmclient.Option) llmclient.Client {
	t.Helper()
	cfg := config.LLMConfig{
		GatewayURL:     ollamaHost,
		DefaultModel:   ollamaChatModel,
		EmbeddingModel: ollamaEmbedModel,
		Timeout:        120,
		MaxRetries:     1,
	}
	c, err := llmclient.NewClient(cfg, opts...)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// newGatewayClient creates a llmclient.Client pointing at the LiteLLM proxy
// with gateway_mode enabled. For full-stack integration tests only.
func newGatewayClient(t *testing.T, opts ...llmclient.Option) llmclient.Client {
	t.Helper()
	if litellmURL == "" {
		t.Skip("TEST_LITELLM_URL not set; skipping gateway mode test")
	}
	cfg := config.LLMConfig{
		GatewayURL:     litellmURL,
		GatewayMode:    true,
		DefaultModel:   chatModel,
		EmbeddingModel: embedModel,
		Timeout:        120,
		MaxRetries:     0,
	}
	c, err := llmclient.NewClient(cfg, opts...)
	if err != nil {
		t.Fatalf("NewClient (gateway): %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// --- Tests ---

func TestIntegrationHealth(t *testing.T) {
	client := newTestClient(t)
	if err := client.Health(context.Background()); err != nil {
		t.Fatalf("Health check failed: %v", err)
	}
}

func TestIntegrationComplete(t *testing.T) {
	client := newTestClient(t)
	ctx := context.Background()

	resp, err := client.Complete(ctx, &llmclient.CompletionRequest{
		Model:    ollamaChatModel,
		TenantID: "test-tenant-a",
		Messages: []llmclient.ChatMessage{
			{Role: "user", Content: "Reply with exactly: hello"},
		},
		MaxTokens: 32,
	})
	if err != nil {
		t.Fatalf("Complete failed: %v", err)
	}
	if len(resp.Choices) == 0 {
		t.Fatal("expected at least one choice")
	}
	if resp.Choices[0].Message.Content == "" {
		t.Fatal("expected non-empty response content")
	}
	if resp.Usage.TotalTokens == 0 {
		t.Fatal("expected non-zero token usage")
	}
	t.Logf("Response: %q (tokens: %d)", resp.Choices[0].Message.Content, resp.Usage.TotalTokens)
}

func TestIntegrationCompleteWithTelemetry(t *testing.T) {
	tp, err := telemetrytest.NewTestProvider()
	if err != nil {
		t.Fatalf("NewTestProvider: %v", err)
	}

	client := newTestClient(t, llmclient.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()))
	ctx := context.Background()

	resp, err := client.Complete(ctx, &llmclient.CompletionRequest{
		Model:    ollamaChatModel,
		TenantID: "test-tenant-a",
		Messages: []llmclient.ChatMessage{
			{Role: "user", Content: "Reply with exactly: hello"},
		},
		MaxTokens: 32,
	})
	if err != nil {
		t.Fatalf("Complete failed: %v", err)
	}
	if resp.Usage.TotalTokens == 0 {
		t.Fatal("expected non-zero token usage")
	}

	spans := tp.GetSpans()
	span := telemetrytest.FindSpan(spans, "llmclient.Complete")
	if span == nil {
		t.Fatal("expected span named llmclient.Complete")
	}

	tenantAttr, ok := telemetrytest.SpanAttribute(span, "tenant.id")
	if !ok {
		t.Fatal("expected tenant.id attribute on span")
	}
	if tenantAttr.AsString() != "test-tenant-a" {
		t.Fatalf("expected tenant.id=test-tenant-a, got %q", tenantAttr.AsString())
	}

	modelAttr, ok := telemetrytest.SpanAttribute(span, "llm.model")
	if !ok {
		t.Fatal("expected llm.model attribute on span")
	}
	if modelAttr.AsString() != ollamaChatModel {
		t.Fatalf("expected llm.model=%s, got %q", ollamaChatModel, modelAttr.AsString())
	}

	tokensAttr, ok := telemetrytest.SpanAttribute(span, "llm.tokens.total")
	if !ok {
		t.Fatal("expected llm.tokens.total attribute on span")
	}
	if tokensAttr.AsInt64() == 0 {
		t.Fatal("expected non-zero llm.tokens.total")
	}
}

func TestIntegrationCompleteWithAudit(t *testing.T) {
	emitter := &captureEmitter{}
	client := newTestClient(t, llmclient.WithAuditEmitter(emitter))
	ctx := context.Background()

	_, err := client.Complete(ctx, &llmclient.CompletionRequest{
		Model:    ollamaChatModel,
		TenantID: "test-tenant-a",
		JobID:    "job-integration-001",
		Messages: []llmclient.ChatMessage{
			{Role: "user", Content: "Reply with exactly: hello"},
		},
		MaxTokens: 32,
	})
	if err != nil {
		t.Fatalf("Complete failed: %v", err)
	}
	if len(emitter.events) == 0 {
		t.Fatal("expected at least one audit event")
	}
	event := emitter.events[0]
	if event.TenantID != "test-tenant-a" {
		t.Fatalf("expected tenant_id=test-tenant-a, got %q", event.TenantID)
	}
	if event.JobID != "job-integration-001" {
		t.Fatalf("expected job_id=job-integration-001, got %q", event.JobID)
	}
	if event.Operation != "complete" {
		t.Fatalf("expected operation=complete, got %q", event.Operation)
	}
	if !event.Success {
		t.Fatalf("expected success=true, got false (error: %s)", event.ErrorMessage)
	}
	if event.PromptHash == "" {
		t.Fatal("expected non-empty prompt hash")
	}
	if event.TokensUsed == 0 {
		t.Fatal("expected non-zero tokens_used")
	}
	if event.DurationMS <= 0 {
		t.Fatal("expected positive duration_ms")
	}
}

func TestIntegrationEmbed(t *testing.T) {
	client := newTestClient(t)
	ctx := context.Background()

	resp, err := client.Embed(ctx, &llmclient.EmbeddingRequest{
		Model:    ollamaEmbedModel,
		Input:    []string{"The quick brown fox", "jumps over the lazy dog"},
		TenantID: "test-tenant-a",
	})
	if err != nil {
		t.Fatalf("Embed failed: %v", err)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("expected 2 embedding vectors, got %d", len(resp.Data))
	}
	if len(resp.Data[0].Embedding) == 0 {
		t.Fatal("expected non-empty embedding vector")
	}
	t.Logf("Embedding dimensions: %d", len(resp.Data[0].Embedding))
}

func TestIntegrationEmbedWithTelemetry(t *testing.T) {
	tp, err := telemetrytest.NewTestProvider()
	if err != nil {
		t.Fatalf("NewTestProvider: %v", err)
	}
	client := newTestClient(t, llmclient.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()))
	ctx := context.Background()

	_, err = client.Embed(ctx, &llmclient.EmbeddingRequest{
		Model:    ollamaEmbedModel,
		Input:    []string{"test embedding text"},
		TenantID: "test-tenant-a",
	})
	if err != nil {
		t.Fatalf("Embed failed: %v", err)
	}

	spans := tp.GetSpans()
	span := telemetrytest.FindSpan(spans, "llmclient.Embed")
	if span == nil {
		t.Fatal("expected span named llmclient.Embed")
	}

	modelAttr, ok := telemetrytest.SpanAttribute(span, "llm.model")
	if !ok {
		t.Fatal("expected llm.model attribute on span")
	}
	if modelAttr.AsString() != ollamaEmbedModel {
		t.Fatalf("expected llm.model=%s, got %q", ollamaEmbedModel, modelAttr.AsString())
	}

	inputCountAttr, ok := telemetrytest.SpanAttribute(span, "llm.input_count")
	if !ok {
		t.Fatal("expected llm.input_count attribute on span")
	}
	if inputCountAttr.AsInt64() != 1 {
		t.Fatalf("expected llm.input_count=1, got %d", inputCountAttr.AsInt64())
	}
}

func TestIntegrationModelNotAllowed(t *testing.T) {
	cfg := config.LLMConfig{
		GatewayURL:     ollamaHost,
		DefaultModel:   ollamaChatModel,
		EmbeddingModel: ollamaEmbedModel,
		Timeout:        30,
		MaxRetries:     0,
		AllowedModels:  []string{"some-other-model"},
	}
	client, err := llmclient.NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

	_, err = client.Complete(context.Background(), &llmclient.CompletionRequest{
		Model:    ollamaChatModel,
		TenantID: "test-tenant-a",
		Messages: []llmclient.ChatMessage{
			{Role: "user", Content: "hello"},
		},
	})
	if err == nil {
		t.Fatal("expected error for disallowed model")
	}
	t.Logf("Got expected error: %v", err)
}

// --- Full-stack gateway mode tests (LiteLLM + Ollama + Jaeger) ---

func TestGatewayHealthCheck(t *testing.T) {
	client := newGatewayClient(t)
	if err := client.Health(context.Background()); err != nil {
		t.Fatalf("Gateway health check failed: %v", err)
	}
}

func TestGatewayAliasRouting(t *testing.T) {
	if litellmURL == "" {
		t.Skip("TEST_LITELLM_URL not set; skipping gateway mode test")
	}

	aliases := []string{"small", "medium", "large", "judge"}
	for _, alias := range aliases {
		t.Run(alias, func(t *testing.T) {
			cfg := config.LLMConfig{
				GatewayURL:     litellmURL,
				GatewayMode:    true,
				DefaultModel:   alias,
				EmbeddingModel: embedModel,
				Timeout:        120,
				MaxRetries:     0,
			}
			client, err := llmclient.NewClient(cfg)
			if err != nil {
				t.Fatalf("NewClient (alias %s): %v", alias, err)
			}
			defer client.Close()

			resp, err := client.Complete(context.Background(), &llmclient.CompletionRequest{
				Model:    alias,
				TenantID: "test-tenant-a",
				Messages: []llmclient.ChatMessage{
					{Role: "user", Content: "Reply with exactly: hello"},
				},
				MaxTokens: 32,
			})
			if err != nil {
				t.Fatalf("Complete via alias %q failed: %v", alias, err)
			}
			if len(resp.Choices) == 0 {
				t.Fatalf("alias %q: expected at least one choice", alias)
			}
			if resp.Choices[0].Message.Content == "" {
				t.Fatalf("alias %q: expected non-empty response content", alias)
			}
			t.Logf("alias %q: %q", alias, resp.Choices[0].Message.Content)
		})
	}
}

func TestGatewayEmbedding(t *testing.T) {
	client := newGatewayClient(t)

	resp, err := client.Embed(context.Background(), &llmclient.EmbeddingRequest{
		Model:    embedModel,
		Input:    []string{"compliance mapping test input"},
		TenantID: "test-tenant-a",
	})
	if err != nil {
		t.Fatalf("Embed via gateway failed: %v", err)
	}
	if len(resp.Data) == 0 {
		t.Fatal("expected at least one embedding vector")
	}
	if len(resp.Data[0].Embedding) == 0 {
		t.Fatal("expected non-empty embedding vector")
	}
	t.Logf("Embedding dimensions: %d", len(resp.Data[0].Embedding))
}

func TestGatewayModeNoRetry(t *testing.T) {
	if litellmURL == "" {
		t.Skip("TEST_LITELLM_URL not set; skipping gateway mode test")
	}

	cfg := config.LLMConfig{
		GatewayURL:     litellmURL,
		GatewayMode:    true,
		DefaultModel:   "nonexistent-model-xyz",
		EmbeddingModel: embedModel,
		Timeout:        30,
		MaxRetries:     0,
	}
	client, err := llmclient.NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

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

	if err == nil {
		t.Fatal("expected error for nonexistent model")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("request took %v; expected < 10s (no retry backoff in gateway mode)", elapsed)
	}
	t.Logf("Got expected error in %v: %v", elapsed, err)
}

func TestGatewayTelemetryToJaeger(t *testing.T) {
	if litellmURL == "" {
		t.Skip("TEST_LITELLM_URL not set; skipping gateway mode test")
	}
	if otlpEndpoint == "" {
		t.Skip("TEST_OTLP_GRPC_ENDPOINT not set; skipping telemetry export test")
	}
	if jaegerQueryURL == "" {
		t.Skip("TEST_JAEGER_QUERY_URL not set; skipping Jaeger query test")
	}

	ctx := context.Background()
	serviceName := "crosscodex-llm-integration"

	obsCfg := config.ObservabilityConfig{
		Endpoint: otlpEndpoint,
		Protocol: "grpc",
		Tracing: config.ObservabilityTracingConfig{
			SampleRate: 1.0,
		},
	}
	shutdown, err := telemetry.Init(ctx, obsCfg,
		telemetry.WithServiceName(serviceName),
		telemetry.WithServiceVersion("test"),
	)
	if err != nil {
		t.Fatalf("telemetry.Init: %v", err)
	}

	client := newGatewayClient(t,
		llmclient.WithTelemetry(otel.GetTracerProvider(), otel.GetMeterProvider()),
	)

	_, err = client.Complete(ctx, &llmclient.CompletionRequest{
		Model:    chatModel,
		TenantID: "test-tenant-a",
		Messages: []llmclient.ChatMessage{
			{Role: "user", Content: "Reply with exactly: hello"},
		},
		MaxTokens: 32,
	})
	if err != nil {
		t.Fatalf("Complete failed: %v", err)
	}

	// Flush traces to the collector. Shutdown may report a metrics export
	// error because Jaeger all-in-one only implements the OTLP traces
	// service, not the metrics service. Traces still get flushed.
	if err := shutdown(ctx); err != nil {
		t.Logf("telemetry shutdown (expected metrics error with Jaeger): %v", err)
	}

	// Poll Jaeger Query API for traces from our service.
	queryURL := fmt.Sprintf("%s/api/traces?service=%s&limit=1", jaegerQueryURL, serviceName)
	var found bool
	for attempt := 0; attempt < 10; attempt++ {
		time.Sleep(2 * time.Second)

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, queryURL, nil)
		if err != nil {
			t.Logf("attempt %d: request build error: %v", attempt, err)
			continue
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Logf("attempt %d: query error: %v", attempt, err)
			continue
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var result struct {
			Data []json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			t.Logf("attempt %d: unmarshal error: %v", attempt, err)
			continue
		}
		if len(result.Data) > 0 {
			found = true
			t.Logf("Found %d trace(s) in Jaeger after %d attempt(s)", len(result.Data), attempt+1)
			break
		}
	}
	if !found {
		t.Fatal("no traces found in Jaeger for service " + serviceName)
	}
}

func TestGatewayAuditEmission(t *testing.T) {
	emitter := &captureEmitter{}
	client := newGatewayClient(t, llmclient.WithAuditEmitter(emitter))

	_, err := client.Complete(context.Background(), &llmclient.CompletionRequest{
		Model:    chatModel,
		TenantID: "test-tenant-a",
		Messages: []llmclient.ChatMessage{
			{Role: "user", Content: "Reply with exactly: hello"},
		},
		MaxTokens: 32,
	})
	if err != nil {
		t.Fatalf("Complete failed: %v", err)
	}

	emitter.mu.Lock()
	defer emitter.mu.Unlock()

	if len(emitter.events) != 1 {
		t.Fatalf("expected 1 audit event, got %d", len(emitter.events))
	}
	ev := emitter.events[0]
	if ev.TenantID != "test-tenant-a" {
		t.Fatalf("expected tenant_id=test-tenant-a, got %q", ev.TenantID)
	}
	if ev.Model != chatModel {
		t.Fatalf("expected model=%s, got %q", chatModel, ev.Model)
	}
	if !ev.Success {
		t.Fatalf("expected success=true, got false (error: %s)", ev.ErrorMessage)
	}
	if ev.DurationMS <= 0 {
		t.Fatal("expected positive duration_ms")
	}
	if ev.PromptHash == "" {
		t.Fatal("expected non-empty prompt hash")
	}
}

func TestGatewayModelAllowList(t *testing.T) {
	if litellmURL == "" {
		t.Skip("TEST_LITELLM_URL not set; skipping gateway mode test")
	}

	cfg := config.LLMConfig{
		GatewayURL:     litellmURL,
		GatewayMode:    true,
		DefaultModel:   chatModel,
		EmbeddingModel: embedModel,
		Timeout:        30,
		MaxRetries:     0,
		AllowedModels:  []string{"small", "embed"},
	}
	client, err := llmclient.NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

	_, err = client.Complete(context.Background(), &llmclient.CompletionRequest{
		Model:    "judge",
		TenantID: "test-tenant-a",
		Messages: []llmclient.ChatMessage{
			{Role: "user", Content: "hello"},
		},
		MaxTokens: 8,
	})
	if err == nil {
		t.Fatal("expected error for model not in allow-list")
	}
	if !strings.Contains(err.Error(), "not in allowed list") {
		t.Fatalf("expected error to contain 'not in allowed list', got: %v", err)
	}
	t.Logf("Got expected allow-list error: %v", err)
}

// captureEmitter records emitted audit events for test assertions. Safe for concurrent use.
type captureEmitter struct {
	mu     sync.Mutex
	events []*llmclient.AuditEvent
}

func (e *captureEmitter) EmitLLMAudit(_ context.Context, event *llmclient.AuditEvent) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, event)
	return nil
}
