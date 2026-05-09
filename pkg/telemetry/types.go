package telemetry

// Config configures the telemetry provider.
type Config struct {
	ServiceName    string  // Service name for resource attributes
	ServiceVersion string  // Service version
	Endpoint       string  // OTLP collector endpoint
	SampleRate     float64 // Trace sampling rate (0.0 to 1.0)
	Enabled        bool    // Enable telemetry
}

// SpanKind represents the type of span.
type SpanKind string

const (
	// SpanKindInternal indicates an internal operation.
	SpanKindInternal SpanKind = "internal"

	// SpanKindServer indicates a server-side request handler.
	SpanKindServer SpanKind = "server"

	// SpanKindClient indicates a client-side request.
	SpanKindClient SpanKind = "client"

	// SpanKindProducer indicates a message producer.
	SpanKindProducer SpanKind = "producer"

	// SpanKindConsumer indicates a message consumer.
	SpanKindConsumer SpanKind = "consumer"
)
