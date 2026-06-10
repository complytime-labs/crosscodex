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
