package analysis

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/errgroup"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/resultcache"
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
	// stageTimeout caps each stage's collection; see WithStageTimeout.
	stageTimeout time.Duration
	taskTypes    map[string]natsbus.TaskType
	reporter     StageReporter
	logger       *slog.Logger
	tracer       trace.Tracer

	// Metrics (optional, nil-safe).
	execCounter  metric.Int64Counter
	execDuration metric.Float64Histogram

	// Telemetry providers for propagation to sub-components.
	tracerProvider trace.TracerProvider
	meterProvider  metric.MeterProvider

	// Result reuse (#243); see WithResultCache and cache.go.
	cacheStore             resultcache.Store
	cacheLookups           metric.Int64Counter
	cacheHits              metric.Int64Counter
	cachePuts              metric.Int64Counter
	cachePutErrors         metric.Int64Counter
	cachePutsRefused       metric.Int64Counter
	cacheIntegrityFailures metric.Int64Counter
	cacheLookupDuration    metric.Float64Histogram
	cacheInFlight          metric.Int64Gauge
	inFlight               atomic.Int64
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

	// Input is required unless Controls is supplied: per-control fan-out
	// (executeAnalyzer) drives GenerateWork from Controls instead of Input.
	if req.Input == nil && len(req.Controls) == 0 {
		span.RecordError(ErrNilInput)
		span.SetStatus(codes.Error, ErrNilInput.Error())
		return nil, fmt.Errorf("analysis: execute: %w", ErrNilInput)
	}

	if err := e.validateCache(req.Cache); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("analysis: execute: %w", err)
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
		JobID:      req.JobID,
		Outputs:    make(map[string]*analyzer.Output),
		Errors:     make(map[string]error),
		CacheStats: make(map[string]resultcache.Stats),
	}
	state := &executionState{result: result}

	// Pre-seed analyzers already completed by a prior Execute call for this job
	// (two-pass execution). They satisfy dependency gating for the requested
	// analyzers without being re-dispatched, and executeAnalyzer short-circuits
	// on them so no stage transitions are re-reported.
	for name, output := range req.CompletedOutputs {
		state.complete(name, output, nil)
	}

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

	// Already completed by a prior pass (pre-seeded via CompletedOutputs). Skip
	// re-execution entirely so its work is not re-dispatched and its stage
	// transitions are not re-reported.
	if state.isCompleted(name) {
		span.SetStatus(codes.Ok, "already completed")
		return
	}

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

	// Catalog IDs are set once, before GenerateWork/Aggregate. Only
	// embedding.Aggregate reads them; every other analyzer ignores them.
	// Each control's own catalog is recorded too: a job that maps one
	// catalog onto another fans out the controls of both in one request,
	// so req.CatalogID (the source) is not every control's catalog.
	if req.CatalogID != "" {
		ctx = analyzer.WithCatalogID(ctx, req.CatalogID)
	}
	if req.Controls != nil {
		catalogs := make(map[string]string, len(req.Controls))
		for _, c := range req.Controls {
			if c.GetCatalogId() != "" {
				catalogs[c.GetControlId()] = c.GetCatalogId()
			}
		}
		ctx = analyzer.WithControlCatalogs(ctx, catalogs)
	}

	// Generate work items. When Controls is non-nil (per-control mode
	// requested), call GenerateWork once per control and concatenate the
	// resulting task slices — Dispatch, Collect, and Aggregate are still
	// each called exactly once, over the combined set, exactly as the
	// single-Input path below. A non-nil but empty Controls (a catalog with
	// zero controls, or no catalog controls reader wired) naturally
	// produces zero tasks via the loop below, rather than falling through to
	// the single-Input path with a mismatched Input type. A per-control error
	// fails the whole analyzer for this job, matching existing all-or-nothing
	// semantics for task-level failures.
	cfg := req.AnalyzerConfig[name]
	var tasks []analyzer.Task
	// origins maps each per-control task to the control it was generated
	// from, so the result cache can store it under that control's catalog.
	var origins map[string]*pb.Control
	if req.Controls != nil {
		origins = map[string]*pb.Control{}
		for _, control := range req.Controls {
			controlTasks, err := ra.GenerateWorkFromProto(ctx, control, cfg)
			if err != nil {
				analyzerErr := fmt.Errorf("analysis: analyzer %q GenerateWork failed for job %q control %q: %w: %w",
					name, req.JobID, control.GetControlId(), ErrAnalyzerFailed, err)
				state.fail(name, analyzerErr)
				_ = e.reporter.ReportStageFailed(ctx, name, req.JobID, analyzerErr)
				span.RecordError(analyzerErr)
				span.SetStatus(codes.Error, analyzerErr.Error())
				return
			}
			for _, t := range controlTasks {
				origins[t.TaskID] = control
			}
			tasks = append(tasks, controlTasks...)
		}
	} else {
		var err error
		tasks, err = ra.GenerateWorkFromProto(ctx, req.Input, cfg)
		if err != nil {
			analyzerErr := fmt.Errorf("analysis: analyzer %q GenerateWork failed for job %q: %w: %w",
				name, req.JobID, ErrAnalyzerFailed, err)
			state.fail(name, analyzerErr)
			_ = e.reporter.ReportStageFailed(ctx, name, req.JobID, analyzerErr)
			span.RecordError(analyzerErr)
			span.SetStatus(codes.Error, analyzerErr.Error())
			return
		}
	}

	// Partition into tasks that need dispatch and pre-built results (e.g.
	// section auto-skip) that already carry their final result — the worker
	// only understands structpb.Struct payloads, not arbitrary result types
	// like *pb.AnalysisResult, so these must never be sent over NATS.
	var dispatchTasks []analyzer.Task
	var preBuiltResults []analyzer.TaskResult
	for _, t := range tasks {
		if t.PreBuiltResult {
			preBuiltResults = append(preBuiltResults, analyzer.TaskResult{
				TaskID:   t.TaskID,
				TaskType: t.TaskType,
				Result:   t.Payload,
			})
			continue
		}
		dispatchTasks = append(dispatchTasks, t)
	}

	// Result reuse (#243). From here on every stop goes through fail (the
	// error is final) or stop (seal the stage's writes first, so a retry
	// reuses the results it paid for).
	sc, err := e.planCache(ctx, name, ra, req, tasks, origins)
	fail := func(stageErr error, reportFailed bool) {
		state.fail(name, stageErr)
		if reportFailed {
			_ = e.reporter.ReportStageFailed(ctx, name, req.JobID, stageErr)
		}
		span.RecordError(stageErr)
		span.SetStatus(codes.Error, stageErr.Error())
	}
	stop := func(stageErr error) {
		e.sealAfterFailure(ctx, sc)
		fail(stageErr, true)
	}
	if err != nil {
		fail(fmt.Errorf("analysis: analyzer %q result cache for job %q: %w: %w", name, req.JobID, ErrAnalyzerFailed, err), true)
		return
	}
	// A verified cached result whose request matches a task's becomes that
	// task's pre-built result, so the task never goes to a worker. A failed
	// lookup, or any cached result that fails verification, stops the stage
	// before anything is dispatched or written.
	if sc.lookups() {
		cached, err := e.lookupCached(ctx, sc)
		if err != nil {
			fail(err, true)
			return
		}
		remaining := make([]analyzer.Task, 0, len(dispatchTasks))
		for _, t := range dispatchTasks {
			if r, ok := cached[t.TaskID]; ok {
				preBuiltResults = append(preBuiltResults, analyzer.TaskResult{TaskID: t.TaskID, TaskType: t.TaskType, Result: r})
				continue
			}
			remaining = append(remaining, t)
		}
		dispatchTasks = remaining
	}
	if sc != nil {
		sc.stats.Tasks = len(tasks)
		sc.stats.Hits = len(sc.hits)
		sc.stats.Dispatched = len(dispatchTasks)
	}
	// complete records the stage's cache use and seals it, all before the
	// stage's result is persisted: a reuse or write that cannot be attested
	// must not be. A sealed stage names its attempt in output's metadata
	// (MetadataCacheAttempt), which the reporter persists with the result.
	complete := func(output *analyzer.Output) bool {
		if !sc.records() {
			return true
		}
		if err := e.recordStage(ctx, sc); err != nil {
			sealed := e.sealAfterFailure(ctx, sc)
			fail(e.recordFailed(ctx, sc, err, sealed), true)
			return false
		}
		if err := e.seal(ctx, sc); err != nil {
			fail(e.checkpointFailed(ctx, sc, err), true)
			return false
		}
		if output.Metadata == nil {
			output.Metadata = make(map[string]string)
		}
		output.Metadata[MetadataCacheAttempt] = sc.attempt
		return true
	}

	// Zero dispatchable tasks and no pre-built results: skip straight to an
	// empty output, exactly as before.
	if len(dispatchTasks) == 0 && len(preBuiltResults) == 0 {
		output := &analyzer.Output{
			AnalyzerName: name,
			Metadata:     map[string]string{"tasks": "0"},
		}
		if !complete(output) {
			return
		}
		if err := e.reporter.ReportStageCompleted(ctx, name, req.JobID, output); err != nil {
			fail(fmt.Errorf("analysis: analyzer %q persist completion failed for job %q: %w: %w",
				name, req.JobID, ErrAnalyzerFailed, err), false)
			return
		}
		state.complete(name, output, sc)
		span.SetStatus(codes.Ok, "zero tasks")
		return
	}

	var results []analyzer.TaskResult
	if len(dispatchTasks) > 0 {
		taskType := e.taskTypes[name]
		expectedIDs := make([]string, len(dispatchTasks))
		for i, t := range dispatchTasks {
			expectedIDs[i] = t.TaskID
		}

		collectReq := CollectRequest{
			TaskType:     taskType,
			JobID:        req.JobID,
			ExpectedIDs:  expectedIDs,
			Tasks:        dispatchTasks,
			Timeout:      e.cfg.TaskTimeout,
			StageTimeout: e.stageTimeout,
			MaxRetries:   e.cfg.MaxRetries,
			Backoff:      e.cfg.RetryBackoff,
			Dispatcher:   e.dispatcher,
		}

		// PrepareCollect: subscribe to result subject BEFORE dispatching tasks.
		// This prevents publish-before-subscribe races in embedded NATS with
		// zero-latency workers (e.g., single-process RoleAll with deterministic fake LLM).
		handle, err := e.collector.PrepareCollect(ctx, collectReq)
		if err != nil {
			stop(fmt.Errorf("analysis: analyzer %q prepare collect failed for job %q: %w: %w",
				name, req.JobID, ErrAnalyzerFailed, err))
			return
		}

		// Dispatch tasks: publish to work subject. Results will be published to the
		// already-subscribed result subject, so no message can be lost.
		if err := e.dispatcher.Dispatch(ctx, dispatchTasks, taskType, req.JobID, handle.CollectionID); err != nil {
			// AwaitResults never runs, so its deferred release and abandon do
			// not either. Some tasks may already be out, so abandon them,
			// after release so no retry follows the abandon.
			if relErr := handle.release(); relErr != nil {
				e.logger.Warn("failed to release collection subscriptions", "job_id", req.JobID, "error", relErr)
			}
			handle.abandon(ctx)
			stop(fmt.Errorf("analysis: analyzer %q dispatch failed for job %q: %w: %w",
				name, req.JobID, ErrAnalyzerFailed, err))
			return
		}

		// AwaitResults: wait for all results on the subscription established above.
		collected, err := e.collector.AwaitResults(ctx, handle)
		if err != nil {
			// The results that did arrive were paid for: store them so the
			// failure seal covers them and a retry reuses them. A cancelled
			// stage stores nothing; it is not sealed either.
			if ctx.Err() == nil && sc.puts() {
				e.putResults(ctx, sc, collected)
			}
			stop(fmt.Errorf("analysis: analyzer %q collect failed for job %q: %w: %w",
				name, req.JobID, ErrAnalyzerFailed, err))
			return
		}
		results = collected
		if sc.puts() {
			e.putResults(ctx, sc, collected)
		}
	}
	results = append(results, preBuiltResults...)

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
		stop(fmt.Errorf("analysis: analyzer %q: all %d tasks failed for job %q: %w",
			name, failures, req.JobID, ErrAnalyzerFailed))
		return
	}

	// Aggregate: use only successful results when there are partial failures.
	aggregateInput := results
	if failures > 0 {
		aggregateInput = successes
	}

	output, err := ra.Aggregate(ctx, aggregateInput)
	if err != nil {
		stop(fmt.Errorf("analysis: analyzer %q Aggregate failed for job %q: %w: %w",
			name, req.JobID, ErrAnalyzerFailed, err))
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

	if !complete(output) {
		return
	}

	// Success (possibly partial). Persist the result before marking the
	// analyzer completed: a persistence failure must fail the analyzer, or
	// downstream logic (resume, candidate generation) would proceed against
	// data that was never durably committed.
	if err := e.reporter.ReportStageCompleted(ctx, name, req.JobID, output); err != nil {
		fail(fmt.Errorf("analysis: analyzer %q persist completion failed for job %q: %w: %w",
			name, req.JobID, ErrAnalyzerFailed, err), false)
		return
	}
	state.complete(name, output, sc)
	span.SetStatus(codes.Ok, "")
}

// executionState manages concurrent writes to ExecutionResult from parallel
// analyzer goroutines within a level.
type executionState struct {
	mu     sync.Mutex
	result *ExecutionResult
}

// complete records name's output and, when the stage used the result
// cache (sc non-nil), its effective cache use.
func (s *executionState) complete(name string, output *analyzer.Output, sc *stageCache) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.result.Outputs[name] = output
	s.result.Completed = append(s.result.Completed, name)
	if sc != nil {
		s.result.CacheStats[name] = sc.stats
	}
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
