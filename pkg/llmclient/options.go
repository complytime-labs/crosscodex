package llmclient

import (
	"fmt"
	"net/http"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Option configures the LLM client.
type Option func(*client) error

// WithTelemetry configures OpenTelemetry tracing and metrics using the
// provided TracerProvider and MeterProvider. The package creates its own
// tracer and meter internally, ensuring the "crosscodex" meter namespace
// convention is enforced regardless of what the caller passes.
func WithTelemetry(tp trace.TracerProvider, mp metric.MeterProvider) Option {
	return func(c *client) error {
		c.tracer = tp.Tracer("crosscodex/pkg/llmclient")
		meter := mp.Meter("crosscodex")
		c.meter = meter

		var err error
		c.completionCounter, err = meter.Int64Counter(
			"llmclient.completions.total",
			metric.WithDescription("Total number of completion requests"),
		)
		if err != nil {
			return fmt.Errorf("failed to create completion counter: %w", err)
		}

		c.completionLatency, err = meter.Int64Histogram(
			"llmclient.completion.duration_ms",
			metric.WithDescription("Duration of completion requests in milliseconds"),
		)
		if err != nil {
			return fmt.Errorf("failed to create completion latency histogram: %w", err)
		}

		c.embedCounter, err = meter.Int64Counter(
			"llmclient.embeddings.total",
			metric.WithDescription("Total number of embedding requests"),
		)
		if err != nil {
			return fmt.Errorf("failed to create embed counter: %w", err)
		}

		c.embedLatency, err = meter.Int64Histogram(
			"llmclient.embedding.duration_ms",
			metric.WithDescription("Duration of embedding requests in milliseconds"),
		)
		if err != nil {
			return fmt.Errorf("failed to create embed latency histogram: %w", err)
		}

		c.errorCounter, err = meter.Int64Counter(
			"llmclient.errors.total",
			metric.WithDescription("Total number of LLM client errors"),
		)
		if err != nil {
			return fmt.Errorf("failed to create error counter: %w", err)
		}

		return nil
	}
}

// WithAuditEmitter configures audit event emission for LLM operations.
func WithAuditEmitter(emitter AuditEmitter) Option {
	return func(c *client) error {
		c.emitter = emitter
		return nil
	}
}

// WithHTTPClient overrides the default HTTP client used for API calls.
// Use this to inject TLS configuration, custom transports, or test doubles.
func WithHTTPClient(httpClient *http.Client) Option {
	return func(c *client) error {
		if httpClient == nil {
			return fmt.Errorf("HTTP client must not be nil: %w", ErrInvalidRequest)
		}
		c.httpClient = httpClient
		return nil
	}
}
