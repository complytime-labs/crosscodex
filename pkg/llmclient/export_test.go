package llmclient

// Test-only exports for internal functions.
var (
	ShouldRetry     = shouldRetry
	BackoffDuration = backoffDuration
	ParseRetryAfter = parseRetryAfter
)

// TelemetryFields exposes telemetry instrument presence for test assertions.
type TelemetryFields struct {
	HasTracer            bool
	HasMeter             bool
	HasCompletionCounter bool
	HasCompletionLatency bool
	HasEmbedCounter      bool
	HasEmbedLatency      bool
	HasErrorCounter      bool
	HasAuditEmitter      bool
}

// ExportIsModelAllowed exposes client.isModelAllowed for property testing.
// Accepts a Client interface and a model name, returns whether the model is allowed.
func ExportIsModelAllowed(c Client, model string) bool {
	impl, ok := c.(*client)
	if !ok {
		return false
	}
	return impl.isModelAllowed(model)
}

// ExportTelemetryFields returns the telemetry state of a client for testing.
func ExportTelemetryFields(c Client) TelemetryFields {
	impl, ok := c.(*client)
	if !ok {
		return TelemetryFields{}
	}
	return TelemetryFields{
		HasTracer:            impl.tracer != nil,
		HasMeter:             impl.meter != nil,
		HasCompletionCounter: impl.completionCounter != nil,
		HasCompletionLatency: impl.completionLatency != nil,
		HasEmbedCounter:      impl.embedCounter != nil,
		HasEmbedLatency:      impl.embedLatency != nil,
		HasErrorCounter:      impl.errorCounter != nil,
		HasAuditEmitter:      impl.emitter != nil,
	}
}
