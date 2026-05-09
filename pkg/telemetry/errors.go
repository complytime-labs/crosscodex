package telemetry

import "errors"

var (
	// ErrProviderNotInitialized indicates the telemetry provider was not initialized.
	ErrProviderNotInitialized = errors.New("telemetry provider not initialized")

	// ErrShutdownFailed indicates telemetry shutdown failed.
	ErrShutdownFailed = errors.New("failed to shutdown telemetry")

	// ErrInvalidConfig indicates the telemetry configuration is invalid.
	ErrInvalidConfig = errors.New("invalid telemetry configuration")
)
