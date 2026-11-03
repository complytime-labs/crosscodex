package pipeline

import (
	"context"
	"fmt"
	"maps"

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
// Each generator is registered with its own entry's weight and config as
// its GenerateRequest.Parameters (see configuredGenerator).
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
		// Copy so the config map is never mutated; an unset (0) weight means
		// the documented default of 1.0.
		params := make(map[string]interface{}, len(entry.Config)+1)
		maps.Copy(params, entry.Config)
		weight := entry.Weight
		if weight == 0 {
			weight = 1.0
		}
		params["weight"] = weight
		if err := reg.Register(configuredGenerator{Generator: gen, params: params}); err != nil {
			return nil, fmt.Errorf("BuildCandidateRegistry: registering %q: %w", entry.Name, err)
		}
	}
	return reg, nil
}

// configuredGenerator gives a generator its own config entry's parameters.
// The registry hands one GenerateRequest to every generator, and weights and
// settings differ per generator, so they cannot travel in that shared
// request; this wrapper substitutes them per generator instead. Name is
// promoted from the embedded generator, so registry spans and errors are
// unchanged.
type configuredGenerator struct {
	candidate.Generator
	params map[string]interface{}
}

// Generate replaces req.Parameters (req is a copy) with this generator's own.
func (g configuredGenerator) Generate(ctx context.Context, req candidate.GenerateRequest) ([]candidate.Candidate, error) {
	req.Parameters = g.params
	return g.Generator.Generate(ctx, req)
}
