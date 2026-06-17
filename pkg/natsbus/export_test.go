package natsbus

import (
	"github.com/complytime-labs/crosscodex/pkg/config"
	"go.opentelemetry.io/otel/trace"
)

// Export unexported functions for BDD tests in the natsbus_test package.
// This follows the Go standard library convention (e.g., export_test.go).

// InjectProvenance exposes injectProvenance for external tests.
var InjectProvenance = injectProvenance

// ExtractProvenance exposes extractProvenance for external tests.
func ExtractProvenance(headers map[string][]string) (MessageMetadata, error) {
	return extractProvenance(headers)
}

// MergeHeaders exposes mergeHeaders for external tests.
func MergeHeaders(user, provenance map[string][]string) map[string][]string {
	return mergeHeaders(user, provenance)
}

// XDGNATSStateDir exposes xdgNATSStateDir for external tests.
func XDGNATSStateDir() string {
	return xdgNATSStateDir()
}

// DefaultClientOptions exposes defaultClientOptions for external tests.
func DefaultClientOptions() clientOptions {
	return defaultClientOptions()
}

// ApplyOption applies an Option to clientOptions for external tests.
func ApplyOption(opt Option, opts *clientOptions) {
	opt(opts)
}

// ResolveStoreDir exposes resolveStoreDir for external tests.
func ResolveStoreDir(cfg config.NATSEmbeddedConfig) string {
	return resolveStoreDir(cfg)
}

// IsEmbeddedMode exposes isEmbeddedMode for external tests.
func IsEmbeddedMode(url string) bool {
	return isEmbeddedMode(url)
}

// AuditStreamConfigs exposes auditStreamConfigs for external tests.
func AuditStreamConfigs(cfg config.NATSStreamsConfig) []StreamConfig {
	return auditStreamConfigs(cfg)
}

// ClientOptionsAccessors provide read access to clientOptions fields.

func (o clientOptions) ConnectTimeout() interface{} { return o.connectTimeout }
func (o clientOptions) ReconnectWait() interface{}  { return o.reconnectWait }
func (o clientOptions) MaxReconnects() interface{}  { return o.maxReconnects }
func (o clientOptions) Logger() interface{}         { return o.logger }
func (o clientOptions) TLSConfig() interface{}      { return o.tlsConfig }

// ContentHash exposes contentHash for external tests.
func ContentHash(data []byte) string {
	return contentHash(data)
}

// ReconstructSpanContext exposes reconstructSpanContext for external tests.
func ReconstructSpanContext(headers map[string][]string) (trace.SpanContext, error) {
	return reconstructSpanContext(headers)
}

// ValidateToken exposes validateToken for property testing.
var ValidateToken = validateToken

// TelemetryFields exposes telemetry instrument state for test assertions.
type TelemetryFields struct {
	HasTracer         bool
	HasMeter          bool
	HasPublishCounter bool
	HasPublishLatency bool
	HasProcessCounter bool
	HasProcessLatency bool
}

// ExportTelemetryFields extracts telemetry state from a client for tests.
// Returns zero TelemetryFields if the Client is not a *client.
func ExportTelemetryFields(c Client) TelemetryFields {
	cc, ok := c.(*client)
	if !ok {
		return TelemetryFields{}
	}
	return TelemetryFields{
		HasTracer:         cc.tracer != nil,
		HasMeter:          cc.meter != nil,
		HasPublishCounter: cc.publishCounter != nil,
		HasPublishLatency: cc.publishLatency != nil,
		HasProcessCounter: cc.processCounter != nil,
		HasProcessLatency: cc.processLatency != nil,
	}
}
