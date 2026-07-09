package analysis

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/errgroup"

	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// Engine orchestrates analyzer execution by building execution DAGs from the
// analyzer registry, dispatching work items via the Dispatcher, collecting
// results via the Collector, and reporting stage transitions.
type Engine struct {
	registry   *analyzer.Registry
	dispatcher Dispatcher
	collector  Collector
	cfg        config.EngineConfig
	taskTypes  map[string]natsbus.TaskType
	reporter   StageReporter
	logger     *slog.Logger
	tracer     trace.Tracer

	// Metrics (optional, nil-safe).
	execCounter  metric.Int64Counter
	execDuration metric.Float64Histogram

	// Telemetry providers for propagation to sub-components.
	tracerProvider trace.TracerProvider
	meterProvider  metric.MeterProvider
}

// New creates an Engine with explicit Dispatcher and Collector.
// Panics if registry is nil.
func New(registry *analyzer.Registry, dispatcher Dispatcher, collector Collector,
	cfg config.EngineConfig, taskTypes map[string]natsbus.TaskType, opts ...Option) *Engine {
	if registry == nil {
		panic("analysis.New: registry must not be nil")
	}

	e := &Engine{
		registry:   registry,
		dispatcher: dispatcher,
		collector:  collector,
		cfg:        cfg,
		taskTypes:  taskTypes,
		reporter:   noopReporter{},
		logger:     slog.Default(),
	}

	for _, opt := range opts {
		opt(e)
	}

	return e
}

// NewWithNATS creates an Engine using NATS-backed Dispatcher and Collector.
// Panics if registry is nil.
func NewWithNATS(registry *analyzer.Registry, bus natsbus.Client,
	cfg config.EngineConfig, taskTypes map[string]natsbus.TaskType, opts ...Option) *Engine {
	// Apply options to a probe Engine to extract telemetry providers.
	probe := &Engine{}
	for _, opt := range opts {
		opt(probe)
	}

	// Create sub-components with telemetry if available.
	var dispatcherOpts []DispatcherOption
	var collectorOpts []CollectorOption
	if probe.logger != nil {
		dispatcherOpts = append(dispatcherOpts, WithDispatcherLogger(probe.logger))
		collectorOpts = append(collectorOpts, WithCollectorLogger(probe.logger))
	}
	if probe.tracerProvider != nil || probe.meterProvider != nil {
		dispatcherOpts = append(dispatcherOpts, WithDispatcherTelemetry(probe.tracerProvider, probe.meterProvider))
		collectorOpts = append(collectorOpts, WithCollectorTelemetry(probe.tracerProvider, probe.meterProvider))
	}

	dispatcher := NewNATSDispatcher(bus, dispatcherOpts...)
	collector := NewNATSCollector(bus, collectorOpts...)
	return New(registry, dispatcher, collector, cfg, taskTypes, opts...)
}

// Execute runs the analysis pipeline for the given request. It validates the
// request, builds a DAG from the registry, and iterates levels — dispatching
// work, collecting results, and aggregating outputs for each analyzer.
//
// On context cancellation, Execute returns the results accumulated so far
// along with a wrapped context.Canceled error. Analyzers that had not yet
// started are added to ExecutionResult.Skipped.
func (e *Engine) Execute(ctx context.Context, req ExecutionRequest) (*ExecutionResult, error) {
	start := time.Now()
	ctx, span := telemetry.StartSpan(e.tracer, ctx, "analysis.Execute")
	defer span.End()

	if e.execCounter != nil {
		e.execCounter.Add(ctx, 1)
	}
	defer func() {
		if e.execDuration != nil {
			e.execDuration.Record(ctx, float64(time.Since(start).Milliseconds()))
		}
	}()

	// Validate request.
	if _, err := tenant.FromContext(ctx); err != nil {
		span.RecordError(ErrNoTenant)
		span.SetStatus(codes.Error, ErrNoTenant.Error())
		return nil, fmt.Errorf("analysis: execute: %w: %w", ErrNoTenant, err)
	}

	if req.JobID == "" {
		span.RecordError(ErrEmptyJobID)
		span.SetStatus(codes.Error, ErrEmptyJobID.Error())
		return nil, fmt.Errorf("analysis: execute: %w", ErrEmptyJobID)
	}

	if req.Input == nil {
		span.RecordError(ErrNilInput)
		span.SetStatus(codes.Error, ErrNilInput.Error())
		return nil, fmt.Errorf("analysis: execute: %w", ErrNilInput)
	}

	// Build DAG.
	dag, err := e.buildDAG(ctx, req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("analysis: execute: building DAG: %w", err)
	}

	// Validate TaskType mappings for all analyzers in the DAG.
	for _, a := range dag.Analyzers() {
		if _, ok := e.taskTypes[a.Name()]; !ok {
			err := fmt.Errorf("analysis: execute: analyzer %q: %w", a.Name(), ErrUnknownTaskType)
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return nil, err
		}
	}

	result := &ExecutionResult{
		JobID:   req.JobID,
		Outputs: make(map[string]*analyzer.Output),
		Errors:  make(map[string]error),
	}
	state := &executionState{result: result}

	span.SetAttributes(
		attribute.String("job.id", req.JobID),
		attribute.Int("analyzer.count", len(dag.Order())),
	)

	// Execute levels sequentially; within each level, analyzers run in parallel.
	levels := dag.Levels()
	for i, level := range levels {
		e.executeLevel(ctx, level, req, state)

		// On context cancellation, mark remaining analyzers as skipped.
		if ctx.Err() != nil {
			for _, remainingLevel := range levels[i+1:] {
				for _, name := range remainingLevel {
					state.skip(name)
				}
			}
			return result, fmt.Errorf("analysis: execution cancelled: %w", ctx.Err())
		}
	}

	span.SetStatus(codes.Ok, "")
	return result, nil
}

func (e *Engine) buildDAG(ctx context.Context, req ExecutionRequest) (*analyzer.DAG, error) {
	fullDAG, err := e.registry.BuildDAG(ctx)
	if err != nil {
		return nil, err
	}

	if len(req.AnalyzerNames) == 0 {
		return fullDAG, nil
	}

	return fullDAG.Subset(req.AnalyzerNames...)
}

// executeLevel runs all analyzers in a single DAG level concurrently using
// errgroup with collect-all semantics: goroutines always return nil so that
// all analyzers in the level run to completion regardless of individual
// failures.
func (e *Engine) executeLevel(ctx context.Context, level []string, req ExecutionRequest, state *executionState) {
	_, span := telemetry.StartSpan(e.tracer, ctx, "analysis.ExecuteLevel")
	defer span.End()
	span.SetAttributes(attribute.StringSlice("analyzer.names", level))

	g, levelCtx := errgroup.WithContext(ctx)

	for _, name := range level {
		g.Go(func() error {
			e.executeAnalyzer(levelCtx, name, req, state)
			return nil // collect-all: never propagate errors to errgroup
		})
	}

	_ = g.Wait()
}

// executeAnalyzer runs a single analyzer through the full lifecycle:
// dependency check → GenerateWork → Dispatch → Collect → Aggregate.
func (e *Engine) executeAnalyzer(ctx context.Context, name string, req ExecutionRequest, state *executionState) {
	ctx, span := telemetry.StartSpan(e.tracer, ctx, "analysis.ExecuteAnalyzer")
	defer span.End()
	span.SetAttributes(
		attribute.String("analyzer.name", name),
		attribute.String("job.id", req.JobID),
	)

	ra, err := e.registry.Get(name)
	if err != nil {
		analyzerErr := fmt.Errorf("analysis: analyzer %q: registry lookup failed: %w: %w",
			name, ErrAnalyzerFailed, err)
		state.fail(name, analyzerErr)
		span.RecordError(analyzerErr)
		span.SetStatus(codes.Error, analyzerErr.Error())
		return
	}

	// Dependency gating: skip if any dependency is not completed.
	for _, dep := range ra.DependsOn() {
		if !state.isCompleted(dep) {
			state.skip(name)
			e.logger.InfoContext(ctx, "skipping analyzer: dependency not completed",
				"analyzer", name, "dependency", dep)
			span.SetStatus(codes.Ok, "skipped: dependency not completed")
			return
		}
	}

	// Report stage started.
	if err := e.reporter.ReportStageStarted(ctx, name, req.JobID); err != nil {
		e.logger.WarnContext(ctx, "failed to report stage started",
			"analyzer", name, "error", err)
	}

	// Generate work items.
	cfg := req.AnalyzerConfig[name]
	tasks, err := ra.GenerateWorkFromProto(ctx, req.Input, cfg)
	if err != nil {
		analyzerErr := fmt.Errorf("analysis: analyzer %q GenerateWork failed for job %q: %w: %w",
			name, req.JobID, ErrAnalyzerFailed, err)
		state.fail(name, analyzerErr)
		_ = e.reporter.ReportStageFailed(ctx, name, req.JobID, analyzerErr)
		span.RecordError(analyzerErr)
		span.SetStatus(codes.Error, analyzerErr.Error())
		return
	}

	// Zero tasks: skip dispatch/collect, record empty output.
	if len(tasks) == 0 {
		output := &analyzer.Output{
			AnalyzerName: name,
			Metadata:     map[string]string{"tasks": "0"},
		}
		state.complete(name, output)
		_ = e.reporter.ReportStageCompleted(ctx, name, req.JobID, output)
		span.SetStatus(codes.Ok, "zero tasks")
		return
	}

	// Dispatch tasks.
	taskType := e.taskTypes[name]
	if err := e.dispatcher.Dispatch(ctx, tasks, taskType, req.JobID); err != nil {
		analyzerErr := fmt.Errorf("analysis: analyzer %q dispatch failed for job %q: %w: %w",
			name, req.JobID, ErrAnalyzerFailed, err)
		state.fail(name, analyzerErr)
		_ = e.reporter.ReportStageFailed(ctx, name, req.JobID, analyzerErr)
		span.RecordError(analyzerErr)
		span.SetStatus(codes.Error, analyzerErr.Error())
		return
	}

	// Collect results.
	expectedIDs := make([]string, len(tasks))
	for i, t := range tasks {
		expectedIDs[i] = t.TaskID
	}

	results, err := e.collector.Collect(ctx, CollectRequest{
		TaskType:    taskType,
		JobID:       req.JobID,
		ExpectedIDs: expectedIDs,
		Tasks:       tasks,
		Timeout:     e.cfg.TaskTimeout,
		MaxRetries:  e.cfg.MaxRetries,
		Backoff:     e.cfg.RetryBackoff,
		Dispatcher:  e.dispatcher,
	})
	if err != nil {
		analyzerErr := fmt.Errorf("analysis: analyzer %q collect failed for job %q: %w: %w",
			name, req.JobID, ErrAnalyzerFailed, err)
		state.fail(name, analyzerErr)
		_ = e.reporter.ReportStageFailed(ctx, name, req.JobID, analyzerErr)
		span.RecordError(analyzerErr)
		span.SetStatus(codes.Error, analyzerErr.Error())
		return
	}

	// Separate successful and failed results.
	var successes []analyzer.TaskResult
	var failures int
	for _, r := range results {
		if r.Error != nil {
			failures++
		} else {
			successes = append(successes, r)
		}
	}

	// All tasks failed: analyzer fails.
	if len(successes) == 0 && failures > 0 {
		analyzerErr := fmt.Errorf("analysis: analyzer %q: all %d tasks failed for job %q: %w",
			name, failures, req.JobID, ErrAnalyzerFailed)
		state.fail(name, analyzerErr)
		_ = e.reporter.ReportStageFailed(ctx, name, req.JobID, analyzerErr)
		span.RecordError(analyzerErr)
		span.SetStatus(codes.Error, analyzerErr.Error())
		return
	}

	// Aggregate: use only successful results when there are partial failures.
	aggregateInput := results
	if failures > 0 {
		aggregateInput = successes
	}

	output, err := ra.Aggregate(ctx, aggregateInput)
	if err != nil {
		analyzerErr := fmt.Errorf("analysis: analyzer %q Aggregate failed for job %q: %w: %w",
			name, req.JobID, ErrAnalyzerFailed, err)
		state.fail(name, analyzerErr)
		_ = e.reporter.ReportStageFailed(ctx, name, req.JobID, analyzerErr)
		span.RecordError(analyzerErr)
		span.SetStatus(codes.Error, analyzerErr.Error())
		return
	}

	// Flag partial results in metadata.
	if failures > 0 {
		if output.Metadata == nil {
			output.Metadata = make(map[string]string)
		}
		output.Metadata["incomplete"] = "true"
		output.Metadata["failed_tasks"] = fmt.Sprintf("%d", failures)
	}

	// Success (possibly partial).
	state.complete(name, output)
	_ = e.reporter.ReportStageCompleted(ctx, name, req.JobID, output)
	span.SetStatus(codes.Ok, "")
}

// executionState manages concurrent writes to ExecutionResult from parallel
// analyzer goroutines within a level.
type executionState struct {
	mu     sync.Mutex
	result *ExecutionResult
}

func (s *executionState) complete(name string, output *analyzer.Output) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.result.Outputs[name] = output
	s.result.Completed = append(s.result.Completed, name)
}

func (s *executionState) fail(name string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.result.Errors[name] = err
	s.result.Failed = append(s.result.Failed, name)
}

func (s *executionState) skip(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.result.Skipped = append(s.result.Skipped, name)
}

func (s *executionState) isCompleted(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.result.Completed {
		if c == name {
			return true
		}
	}
	return false
}
