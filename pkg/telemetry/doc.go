// Package telemetry provides OpenTelemetry setup for traces, metrics, and logs.
//
// Configures exporters, samplers, and resource attributes for distributed tracing.
//
// Example usage:
//
//	provider, err := telemetry.NewProvider(ctx, telemetry.Config{
//	    ServiceName: "crosscodex-ingestion",
//	    Endpoint:    "localhost:4317",
//	})
//	if err != nil {
//	    return err
//	}
//	defer provider.Shutdown(ctx)
//
//	tracer := provider.TracerProvider().Tracer("ingestion")
//	ctx, span := tracer.Start(ctx, "process-document")
//	defer span.End()
package telemetry
