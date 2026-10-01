package pipeline

import (
	"fmt"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/candidate"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/candidate/builtin"
	"github.com/complytime-labs/crosscodex/pkg/config"
)

// BuildCandidateRegistry constructs a candidate.Registry with the builtin
// generators enabled in cfg.Generators registered. Unknown generator names
// are rejected rather than silently ignored, since a typo in config would
// otherwise silently disable a generator with no error anywhere.
//
// tp and mp are forwarded to the registry and, when tp is non-nil, to every
// builtin generator via a shared "crosscodex/pkg/analyzer/candidate/builtin"
// tracer, so their Generate spans share the caller's tracer provider.
// Either may be nil, which preserves the previous uninstrumented behaviour.
func BuildCandidateRegistry(cfg config.CandidateConfig, tp trace.TracerProvider, mp metric.MeterProvider, opts ...candidate.RegistryOption) (*candidate.Registry, error) {
	registryOpts := append([]candidate.RegistryOption{candidate.WithTelemetry(tp, mp)}, opts...)
	reg := candidate.NewRegistry(registryOpts...)

	var builtinTracer trace.Tracer
	if tp != nil {
		builtinTracer = tp.Tracer("crosscodex/pkg/analyzer/candidate/builtin")
	}

	for _, entry := range cfg.Generators {
		if !entry.Enabled {
			continue
		}
		var gen candidate.Generator
		switch entry.Name {
		case "keyword":
			gen = builtin.NewKeywordGenerator(builtin.WithKeywordTelemetry(builtinTracer))
		case "level":
			gen = builtin.NewLevelGenerator(builtin.WithLevelTelemetry(builtinTracer))
		case "semantic":
			gen = builtin.NewSemanticGenerator(builtin.WithSemanticTelemetry(builtinTracer))
		default:
			return nil, fmt.Errorf("BuildCandidateRegistry: unknown generator %q", entry.Name)
		}
		if err := reg.Register(gen); err != nil {
			return nil, fmt.Errorf("BuildCandidateRegistry: registering %q: %w", entry.Name, err)
		}
	}
	return reg, nil
}
