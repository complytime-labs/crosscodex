package analyzer

import (
	"context"
	"fmt"
	"regexp"
	"sync"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

// namePattern enforces the analyzer name format:
//   - starts with a lowercase letter
//   - middle characters: lowercase letters, digits, hyphens, or underscores
//   - ends with a lowercase letter or digit
//   - length: 1–64 characters
//
// Single-character names (one lowercase letter) are permitted.
var namePattern = regexp.MustCompile(`^[a-z](?:[a-z0-9_-]{0,62}[a-z0-9])?$`)

// ValidateName checks whether name is a well-formed analyzer identifier.
// Valid names start with a lowercase letter, contain only lowercase letters,
// digits, hyphens, and underscores, end with a letter or digit, and are
// 1–64 characters long.
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("analyzer name must not be empty: %w", ErrInvalidName)
	}
	if !namePattern.MatchString(name) {
		return fmt.Errorf("analyzer name %q does not match required pattern %s: %w",
			name, namePattern.String(), ErrInvalidName)
	}
	return nil
}

// Registry holds registered analyzers and builds execution DAGs.
// It is safe for concurrent use.
type Registry struct {
	mu        sync.RWMutex
	analyzers map[string]RegisteredAnalyzer

	// Telemetry (optional, nil-safe)
	tracer          trace.Tracer
	registerCounter metric.Int64Counter
	buildDAGLatency metric.Float64Histogram
	analyzerGauge   metric.Int64Gauge
}

// NewRegistry creates an empty Registry.
func NewRegistry(opts ...RegistryOption) *Registry {
	r := &Registry{
		analyzers: make(map[string]RegisteredAnalyzer),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// RegistryOption configures a Registry.
type RegistryOption func(*Registry)

// WithTelemetry configures OpenTelemetry tracing and metrics for the registry.
// The tracer is used for spans on BuildDAG. The meter provides counters for
// registrations, a histogram for BuildDAG duration, and a gauge for analyzer
// count. Either provider may be nil; nil providers are silently ignored.
func WithTelemetry(tp trace.TracerProvider, mp metric.MeterProvider) RegistryOption {
	return func(r *Registry) {
		if tp != nil {
			r.tracer = tp.Tracer("crosscodex/pkg/analyzer")
		}
		if mp != nil {
			meter := mp.Meter("crosscodex")
			// Instrument creation errors are extremely unlikely with valid
			// OTel providers. If they occur, the instruments remain nil and
			// recording is skipped (nil-safe checks in Register/BuildDAG).
			r.registerCounter, _ = meter.Int64Counter(
				"analyzer.registrations.total",
				metric.WithDescription("Total number of analyzer registrations"),
			)
			r.buildDAGLatency, _ = meter.Float64Histogram(
				"analyzer.build_dag.duration_ms",
				metric.WithDescription("Duration of BuildDAG in milliseconds"),
			)
			r.analyzerGauge, _ = meter.Int64Gauge(
				"analyzer.registered.count",
				metric.WithDescription("Number of registered analyzers"),
			)
		}
	}
}

// Get retrieves a registered analyzer by name.
// Returns ErrNotFound if no analyzer with that name exists.
func (r *Registry) Get(name string) (RegisteredAnalyzer, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	a, ok := r.analyzers[name]
	if !ok {
		return nil, fmt.Errorf("analyzer %q not found: %w", name, ErrNotFound)
	}
	return a, nil
}

// All returns all registered analyzers in no particular order.
func (r *Registry) All() []RegisteredAnalyzer {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]RegisteredAnalyzer, 0, len(r.analyzers))
	for _, a := range r.analyzers {
		result = append(result, a)
	}
	return result
}

// Register wraps a typed Analyzer[T] and stores it in the registry.
// The wrapper handles proto.Message -> T type assertion at the boundary.
// Returns ErrInvalidName if the analyzer name is malformed.
// Returns ErrAlreadyRegistered if an analyzer with the same name exists.
func Register[T proto.Message](r *Registry, a Analyzer[T]) error {
	name := a.Name()

	if err := ValidateName(name); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.analyzers[name]; exists {
		return fmt.Errorf("analyzer %q already registered: %w", name, ErrAlreadyRegistered)
	}

	r.analyzers[name] = &registeredWrapper[T]{inner: a}

	if r.registerCounter != nil {
		r.registerCounter.Add(context.Background(), 1)
	}
	if r.analyzerGauge != nil {
		r.analyzerGauge.Record(context.Background(), int64(len(r.analyzers)))
	}

	return nil
}

// registeredWrapper adapts Analyzer[T] to RegisteredAnalyzer.
type registeredWrapper[T proto.Message] struct {
	inner Analyzer[T]
}

func (w *registeredWrapper[T]) Name() string {
	return w.inner.Name()
}

func (w *registeredWrapper[T]) DependsOn() []string {
	deps := w.inner.DependsOn()
	cp := make([]string, len(deps))
	copy(cp, deps)
	return cp
}

func (w *registeredWrapper[T]) GenerateWorkFromProto(ctx context.Context, input proto.Message, config AnalyzerConfig) ([]Task, error) {
	typed, ok := input.(T)
	if !ok {
		var zero T
		return nil, fmt.Errorf(
			"analyzer %q: expected input type %T, got %T",
			w.inner.Name(), zero, input,
		)
	}
	return w.inner.GenerateWork(ctx, typed, config)
}

func (w *registeredWrapper[T]) Aggregate(ctx context.Context, results []TaskResult) (*Output, error) {
	return w.inner.Aggregate(ctx, results)
}

func (w *registeredWrapper[T]) ResultSchema() proto.Message {
	return w.inner.ResultSchema()
}

func (w *registeredWrapper[T]) InputType() string {
	var zero T
	return string(proto.MessageName(zero))
}

// Compile-time interface verification.
var _ RegisteredAnalyzer = (*registeredWrapper[*emptypb.Empty])(nil)
