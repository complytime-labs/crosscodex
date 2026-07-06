package candidate

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Registry holds registered candidate generators and orchestrates
// candidate generation with configurable aggregation strategies.
// It is safe for concurrent use.
type Registry struct {
	mu         sync.RWMutex
	generators map[string]Generator

	// Telemetry (optional, nil-safe)
	tracer          trace.Tracer
	registerCounter metric.Int64Counter
	generateLatency metric.Float64Histogram
	generatorGauge  metric.Int64Gauge
}

// NewRegistry creates an empty Registry.
func NewRegistry(opts ...RegistryOption) *Registry {
	r := &Registry{
		generators: make(map[string]Generator),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// RegistryOption configures a Registry.
type RegistryOption func(*Registry)

// WithTelemetry configures OpenTelemetry tracing and metrics for the registry.
func WithTelemetry(tp trace.TracerProvider, mp metric.MeterProvider) RegistryOption {
	return func(r *Registry) {
		if tp != nil {
			r.tracer = tp.Tracer("crosscodex/pkg/analyzer/candidate")
		}
		if mp != nil {
			meter := mp.Meter("crosscodex")
			r.registerCounter, _ = meter.Int64Counter(
				"candidate.registrations.total",
				metric.WithDescription("Total number of generator registrations"),
			)
			r.generateLatency, _ = meter.Float64Histogram(
				"candidate.generate.duration_ms",
				metric.WithDescription("Duration of Generate in milliseconds"),
			)
			r.generatorGauge, _ = meter.Int64Gauge(
				"candidate.generators.count",
				metric.WithDescription("Number of registered generators"),
			)
		}
	}
}

// Register adds a generator to the registry.
// Returns an error if a generator with the same name already exists.
func (r *Registry) Register(g Generator) error {
	name := g.Name()
	if name == "" {
		return fmt.Errorf("generator name must not be empty")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.generators[name]; exists {
		return fmt.Errorf("generator %q already registered", name)
	}

	r.generators[name] = g

	if r.registerCounter != nil {
		r.registerCounter.Add(context.Background(), 1)
	}
	if r.generatorGauge != nil {
		r.generatorGauge.Record(context.Background(), int64(len(r.generators)))
	}

	return nil
}

// Get retrieves a registered generator by name.
// Returns an error if no generator with that name exists.
func (r *Registry) Get(name string) (Generator, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	g, ok := r.generators[name]
	if !ok {
		return nil, fmt.Errorf("generator %q not found", name)
	}
	return g, nil
}

// All returns all registered generators in no particular order.
func (r *Registry) All() []Generator {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]Generator, 0, len(r.generators))
	for _, g := range r.generators {
		result = append(result, g)
	}
	return result
}

// Generate runs all registered generators and aggregates their candidates
// using the specified strategy.
func (r *Registry) Generate(ctx context.Context, req GenerateRequest, strategy AggregationStrategy, opts ...GenerateOption) ([]Candidate, error) {
	start := time.Now()

	defer func() {
		if r.generateLatency != nil {
			elapsed := float64(time.Since(start).Milliseconds())
			r.generateLatency.Record(ctx, elapsed)
		}
	}()

	var span trace.Span
	if r.tracer != nil {
		ctx, span = r.tracer.Start(ctx, "candidate.Registry.Generate")
		defer span.End()
		span.SetAttributes(
			attribute.String("tenant_id", req.TenantID),
			attribute.String("job_id", req.JobID),
			attribute.String("strategy", string(strategy)),
		)
	}

	// Apply options
	cfg := &generateConfig{}
	for _, opt := range opts {
		opt(cfg)
	}

	// Get all generators
	r.mu.RLock()
	generators := make([]Generator, 0, len(r.generators))
	for _, g := range r.generators {
		generators = append(generators, g)
	}
	r.mu.RUnlock()

	if len(generators) == 0 {
		return []Candidate{}, nil
	}

	// Run each generator
	var allCandidates [][]Candidate
	for _, gen := range generators {
		var genSpan trace.Span
		if r.tracer != nil {
			_, genSpan = r.tracer.Start(ctx, "candidate.Generator.Generate")
			genSpan.SetAttributes(attribute.String("generator", gen.Name()))
		}

		candidates, err := gen.Generate(ctx, req)
		if genSpan != nil {
			if err != nil {
				genSpan.RecordError(err)
			}
			genSpan.SetAttributes(attribute.Int("candidate_count", len(candidates)))
			genSpan.End()
		}

		if err != nil {
			if span != nil {
				span.RecordError(err)
			}
			return nil, fmt.Errorf("generator %q failed: %w", gen.Name(), err)
		}

		allCandidates = append(allCandidates, candidates)
	}

	// Aggregate based on strategy
	result, err := aggregate(allCandidates, strategy, cfg)
	if err != nil {
		if span != nil {
			span.RecordError(err)
		}
		return nil, err
	}

	if span != nil {
		span.SetAttributes(attribute.Int("result_count", len(result)))
	}

	return result, nil
}

// generateConfig holds options for Generate
type generateConfig struct {
	minScore float64
}

// GenerateOption configures Generate behavior
type GenerateOption func(*generateConfig)

// WithMinScore sets the minimum score threshold for weighted strategies
func WithMinScore(score float64) GenerateOption {
	return func(cfg *generateConfig) {
		cfg.minScore = score
	}
}

// AggregationStrategy defines how to combine candidates from multiple generators
type AggregationStrategy string

const (
	// StrategyUnion includes all candidates from any generator (deduplicated)
	StrategyUnion AggregationStrategy = "union"

	// StrategyWeightedUnion includes candidates where sum(weight * score) >= threshold
	StrategyWeightedUnion AggregationStrategy = "weighted_union"
)
