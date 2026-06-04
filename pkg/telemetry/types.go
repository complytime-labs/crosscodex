package telemetry

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
