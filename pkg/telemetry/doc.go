// Package telemetry provides OpenTelemetry setup for traces, metrics, and logs.
//
// Init creates TracerProvider and MeterProvider with OTLP exporters, registers
// them globally, wraps the default slog handler with trace ID injection, and
// returns a shutdown function. An empty resolved endpoint disables the signal
// (no-op provider, no error).
//
// Example usage:
//
//	shutdown, err := telemetry.Init(ctx, cfg.Observability,
//	    telemetry.WithServiceName("crosscodex-ingestion"),
//	    telemetry.WithServiceVersion("0.1.0"),
//	)
//	if err != nil {
//	    return err
//	}
//	defer shutdown(ctx)
//
//	tracer := otel.GetTracerProvider().Tracer("ingestion")
//	ctx, span := tracer.Start(ctx, "process-document")
//	defer span.End()
package telemetry
