package telemetry

import (
	"log/slog"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

// NewTraceHandler exposes newTraceHandler for L4 tests.
func NewTraceHandler(inner slog.Handler) slog.Handler {
	return newTraceHandler(inner)
}

// ResolveEndpoint exposes resolveEndpoint for L4 tests.
func ResolveEndpoint(signalEndpoint, sharedEndpoint string) string {
	return resolveEndpoint(signalEndpoint, sharedEndpoint)
}

// ResolveProtocol exposes resolveProtocol for L4 tests.
func ResolveProtocol(signalProtocol, sharedProtocol string) string {
	return resolveProtocol(signalProtocol, sharedProtocol)
}

// ExportValidateConfig exposes validateConfig for fuzz testing.
func ExportValidateConfig(cfg config.ObservabilityConfig) error {
	return validateConfig(cfg)
}
