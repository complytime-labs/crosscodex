package llmclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// maxResponseSize is the maximum allowed LLM gateway response body size (10 MB).
const maxResponseSize = 10 * 1024 * 1024

type client struct {
	cfg        config.LLMConfig
	httpClient *http.Client
	apiKey     string

	mu     sync.Mutex
	closed bool

	// Telemetry
	tracer            trace.Tracer
	meter             metric.Meter
	completionCounter metric.Int64Counter
	completionLatency metric.Int64Histogram
	embedCounter      metric.Int64Counter
	embedLatency      metric.Int64Histogram
	errorCounter      metric.Int64Counter

	// Audit
	emitter AuditEmitter
}

// NewClient creates a new LLM client configured for an OpenAI-compatible gateway.
func NewClient(cfg config.LLMConfig, opts ...Option) (Client, error) {
	if cfg.GatewayURL == "" {
		return nil, ErrNoGateway
	}

	timeout := time.Duration(cfg.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	c := &client{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}

	// Resolve API key if configured.
	if cfg.APIKeyRef != "" {
		key, err := ResolveCredential(cfg.APIKeyRef)
		if err != nil {
			return nil, fmt.Errorf("resolving API key: %w", err)
		}
		c.apiKey = key
	}

	for _, opt := range opts {
		if err := opt(c); err != nil {
			return nil, fmt.Errorf("applying option: %w", err)
		}
	}

	return c, nil
}

// Complete generates a chat completion via the OpenAI-compatible API.
func (c *client) Complete(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	if err := c.prepareCompletionRequest(req); err != nil {
		return nil, err
	}

	start := time.Now()
	ctx, span := c.startSpan(ctx, "llmclient.Complete")
	defer span.End()

	span.SetAttributes(
		attribute.String("tenant.id", req.TenantID),
		attribute.String("llm.model", req.Model),
		attribute.String("llm.operation", OpComplete),
	)
	if req.JobID != "" {
		span.SetAttributes(attribute.String("llm.job_id", req.JobID))
	}
	if req.PromptName != "" {
		span.SetAttributes(attribute.String("llm.prompt.name", req.PromptName))
	}
	if req.PromptVersion != "" {
		span.SetAttributes(attribute.String("llm.prompt.version", req.PromptVersion))
	}

	promptHash := ContentHash(req.Messages)

	body, err := json.Marshal(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		c.recordErrorMetric(ctx)
		return nil, fmt.Errorf("marshaling completion request: %w", err)
	}

	var resp CompletionResponse
	if err := c.doWithRetry(ctx, "/v1/chat/completions", body, &resp); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		c.recordErrorMetric(ctx)
		c.recordCompletionMetrics(ctx, start, false, req.Model, req.TenantID)
		c.emitAudit(ctx, req.TenantID, req.JobID, req.Model, OpComplete,
			promptHash, req.PromptName, req.PromptVersion,
			0, time.Since(start), false, err.Error())
		return nil, err
	}

	span.SetAttributes(
		attribute.Int("llm.tokens.prompt", resp.Usage.PromptTokens),
		attribute.Int("llm.tokens.completion", resp.Usage.CompletionTokens),
		attribute.Int("llm.tokens.total", resp.Usage.TotalTokens),
	)
	span.SetStatus(codes.Ok, "")
	c.recordCompletionMetrics(ctx, start, true, req.Model, req.TenantID)
	c.emitAudit(ctx, req.TenantID, req.JobID, req.Model, OpComplete,
		promptHash, req.PromptName, req.PromptVersion,
		resp.Usage.TotalTokens, time.Since(start), true, "")

	return &resp, nil
}

// Embed generates vector embeddings via the OpenAI-compatible API.
func (c *client) Embed(ctx context.Context, req *EmbeddingRequest) (*EmbeddingResponse, error) {
	if err := c.prepareEmbeddingRequest(req); err != nil {
		return nil, err
	}

	start := time.Now()
	ctx, span := c.startSpan(ctx, "llmclient.Embed")
	defer span.End()

	span.SetAttributes(
		attribute.String("tenant.id", req.TenantID),
		attribute.String("llm.model", req.Model),
		attribute.String("llm.operation", OpEmbed),
		attribute.Int("llm.input_count", len(req.Input)),
	)
	if req.JobID != "" {
		span.SetAttributes(attribute.String("llm.job_id", req.JobID))
	}

	promptHash := ContentHash(req.Input)

	body, err := json.Marshal(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		c.recordErrorMetric(ctx)
		return nil, fmt.Errorf("marshaling embedding request: %w", err)
	}

	var resp EmbeddingResponse
	if err := c.doWithRetry(ctx, "/v1/embeddings", body, &resp); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		c.recordErrorMetric(ctx)
		c.recordEmbedMetrics(ctx, start, false, req.Model, req.TenantID)
		c.emitAudit(ctx, req.TenantID, req.JobID, req.Model, OpEmbed,
			promptHash, "", "",
			0, time.Since(start), false, err.Error())
		return nil, err
	}

	span.SetAttributes(
		attribute.Int("llm.tokens.prompt", resp.Usage.PromptTokens),
		attribute.Int("llm.tokens.total", resp.Usage.TotalTokens),
	)
	span.SetStatus(codes.Ok, "")
	c.recordEmbedMetrics(ctx, start, true, req.Model, req.TenantID)
	c.emitAudit(ctx, req.TenantID, req.JobID, req.Model, OpEmbed,
		promptHash, "", "",
		resp.Usage.TotalTokens, time.Since(start), true, "")

	return &resp, nil
}

// Health checks LLM gateway availability by querying the models endpoint.
func (c *client) Health(ctx context.Context) error {
	url := strings.TrimRight(c.cfg.GatewayURL, "/") + "/v1/models"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("creating health request: %w", err)
	}
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("LLM gateway health check failed: %w: %w", err, ErrGatewayUnavailable)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("LLM gateway returned HTTP %d: %w", resp.StatusCode, ErrGatewayUnavailable)
	}
	return nil
}

// Close releases client resources. Safe to call multiple times.
func (c *client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil
	}
	c.closed = true

	c.httpClient.CloseIdleConnections()
	return nil
}

// doWithRetry executes a POST request with exponential backoff retry on
// transient failures (HTTP 429, 5xx, and network errors).
func (c *client) doWithRetry(ctx context.Context, path string, body []byte, result any) error {
	url := strings.TrimRight(c.cfg.GatewayURL, "/") + path
	maxAttempts := c.cfg.MaxRetries + 1
	if c.cfg.GatewayMode {
		maxAttempts = 1
	}
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	var lastErr error
	var overrideDelay time.Duration // non-zero when Retry-After should replace backoff
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			delay := backoffDuration(attempt - 1)
			if overrideDelay > 0 {
				delay = overrideDelay
				overrideDelay = 0
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("context cancelled during retry backoff: %w", ctx.Err())
			case <-time.After(delay):
			}
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("creating request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if c.apiKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
		}

		resp, err := c.httpClient.Do(httpReq)
		if err != nil {
			// Network errors are always retryable.
			lastErr = fmt.Errorf("LLM gateway request failed: %w: %w", err, ErrGatewayUnavailable)
			continue
		}

		respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
		resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("reading response body: %w", readErr)
			continue
		}
		if len(respBody) >= maxResponseSize {
			return fmt.Errorf("response body reached %d byte limit: %w", maxResponseSize, ErrResponseTooLarge)
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if err := json.Unmarshal(respBody, result); err != nil {
				return fmt.Errorf("decoding response: %w", err)
			}
			return nil
		}

		apiErr := c.parseAPIError(resp.StatusCode, respBody)
		retry, retryDelay := shouldRetry(resp.StatusCode, resp.Header.Get("Retry-After"))
		if retry {
			lastErr = apiErr
			overrideDelay = retryDelay // replaces next backoff if non-zero
			continue
		}

		// Non-retryable HTTP error.
		return c.classifyError(apiErr)
	}

	// Exhausted all retries.
	if lastErr != nil {
		return fmt.Errorf("LLM gateway request failed after %d attempts: %w", maxAttempts, lastErr)
	}
	return fmt.Errorf("LLM gateway request failed: %w", ErrGatewayUnavailable)
}

// parseAPIError extracts a structured error from the OpenAI error envelope.
// Falls back to using the raw body as the message if JSON parsing fails.
func (c *client) parseAPIError(statusCode int, body []byte) *APIError {
	var envelope struct {
		Error APIError `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error.Message != "" {
		envelope.Error.StatusCode = statusCode
		return &envelope.Error
	}
	return &APIError{
		StatusCode: statusCode,
		Message:    string(body),
	}
}

// classifyError maps HTTP status codes to sentinel errors.
func (c *client) classifyError(apiErr *APIError) error {
	switch apiErr.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: %s", ErrAuthentication, apiErr.Message)
	case http.StatusTooManyRequests:
		return fmt.Errorf("%w: %s", ErrRateLimitExceeded, apiErr.Message)
	default:
		return apiErr
	}
}

// prepareCompletionRequest validates required fields and applies defaults (e.g., model).
func (c *client) prepareCompletionRequest(req *CompletionRequest) error {
	if req == nil {
		return fmt.Errorf("completion request must not be nil: %w", ErrInvalidRequest)
	}
	if req.TenantID == "" {
		return fmt.Errorf("tenant ID is required: %w", ErrInvalidRequest)
	}
	if err := tenant.ValidateTenantID(req.TenantID); err != nil {
		return fmt.Errorf("invalid tenant ID: %w: %w", err, ErrInvalidRequest)
	}
	if req.Model == "" {
		req.Model = c.cfg.DefaultModel
	}
	if len(req.Messages) == 0 {
		return fmt.Errorf("at least one message is required: %w", ErrInvalidRequest)
	}
	if !c.isModelAllowed(req.Model) {
		return fmt.Errorf("model %q is not in allowed list %v: %w", req.Model, c.cfg.AllowedModels, ErrModelNotAllowed)
	}
	return nil
}

// prepareEmbeddingRequest validates required fields and applies defaults (e.g., model).
func (c *client) prepareEmbeddingRequest(req *EmbeddingRequest) error {
	if req == nil {
		return fmt.Errorf("embedding request must not be nil: %w", ErrInvalidRequest)
	}
	if req.TenantID == "" {
		return fmt.Errorf("tenant ID is required: %w", ErrInvalidRequest)
	}
	if err := tenant.ValidateTenantID(req.TenantID); err != nil {
		return fmt.Errorf("invalid tenant ID: %w: %w", err, ErrInvalidRequest)
	}
	if req.Model == "" {
		req.Model = c.cfg.EmbeddingModel
	}
	if len(req.Input) == 0 {
		return fmt.Errorf("at least one input text is required: %w", ErrInvalidRequest)
	}
	if !c.isModelAllowed(req.Model) {
		return fmt.Errorf("model %q is not in allowed list %v: %w", req.Model, c.cfg.AllowedModels, ErrModelNotAllowed)
	}
	return nil
}

// isModelAllowed checks if the model is permitted by the allow-list.
// An empty AllowedModels list means all models are allowed.
func (c *client) isModelAllowed(model string) bool {
	if len(c.cfg.AllowedModels) == 0 {
		return true
	}
	for _, allowed := range c.cfg.AllowedModels {
		if allowed == model {
			return true
		}
	}
	return false
}

// startSpan creates a traced span. Falls back to the context's TracerProvider
// if no tracer was configured via WithTelemetry.
func (c *client) startSpan(ctx context.Context, name string) (context.Context, trace.Span) {
	tracer := c.tracer
	if tracer == nil {
		tracer = trace.SpanFromContext(ctx).TracerProvider().Tracer("crosscodex/pkg/llmclient")
	}
	return tracer.Start(ctx, name)
}

// recordCompletionMetrics records completion counter and latency if instruments exist.
func (c *client) recordCompletionMetrics(ctx context.Context, start time.Time, success bool, model, tenantID string) {
	attrs := metric.WithAttributes(
		attribute.Bool("success", success),
		attribute.String("llm.model", model),
		attribute.String("tenant.id", tenantID),
	)
	if c.completionCounter != nil {
		c.completionCounter.Add(ctx, 1, attrs)
	}
	if c.completionLatency != nil {
		c.completionLatency.Record(ctx, time.Since(start).Milliseconds(), attrs)
	}
}

// recordEmbedMetrics records embedding counter and latency if instruments exist.
func (c *client) recordEmbedMetrics(ctx context.Context, start time.Time, success bool, model, tenantID string) {
	attrs := metric.WithAttributes(
		attribute.Bool("success", success),
		attribute.String("llm.model", model),
		attribute.String("tenant.id", tenantID),
	)
	if c.embedCounter != nil {
		c.embedCounter.Add(ctx, 1, attrs)
	}
	if c.embedLatency != nil {
		c.embedLatency.Record(ctx, time.Since(start).Milliseconds(), attrs)
	}
}

// recordErrorMetric increments the error counter if the instrument exists.
func (c *client) recordErrorMetric(ctx context.Context) {
	if c.errorCounter != nil {
		c.errorCounter.Add(ctx, 1)
	}
}

// emitAudit sends an audit event via the configured emitter. Best-effort:
// logs a warning on failure but never returns an error to the caller.
func (c *client) emitAudit(ctx context.Context, tenantID, jobID, model, operation, promptHash, promptName, promptVersion string, tokensUsed int, duration time.Duration, success bool, errMsg string) {
	if c.emitter == nil {
		return
	}
	event := &AuditEvent{
		Timestamp:     time.Now(),
		TenantID:      tenantID,
		JobID:         jobID,
		Model:         model,
		Operation:     operation,
		PromptHash:    promptHash,
		TokensUsed:    tokensUsed,
		DurationMS:    duration.Milliseconds(),
		Success:       success,
		ErrorMessage:  errMsg,
		TraceID:       telemetry.TraceIDFromContext(ctx),
		PromptName:    promptName,
		PromptVersion: promptVersion,
	}
	if err := c.emitter.EmitLLMAudit(ctx, event); err != nil {
		slog.WarnContext(ctx, "failed to emit LLM audit event",
			"error", err,
			"operation", operation,
			"tenant_id", tenantID,
		)
	}
}

// Verify interface compliance at compile time.
var _ Client = (*client)(nil)
