package telemetry_test

import (
	"testing"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
)

func FuzzValidateConfig(f *testing.F) {
	f.Add("localhost:4317", "grpc", 1.0)
	f.Add("", "", 0.0)
	f.Add("localhost:4318", "http", 0.5)
	f.Add("localhost:4317", "invalid", 2.0)
	f.Add("localhost:4317", "grpc", -0.1)

	f.Fuzz(func(t *testing.T, endpoint, protocol string, sampleRate float64) {
		cfg := config.ObservabilityConfig{
			Endpoint: endpoint,
			Protocol: protocol,
			Tracing: config.ObservabilityTracingConfig{
				SampleRate: sampleRate,
			},
		}
		_ = telemetry.ExportValidateConfig(cfg)
	})
}
