package analysis

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// Collector subscribes to result subjects and collects task results.
//
// Collection is two-phase to avoid a publish-before-subscribe race in embedded
// NATS: PrepareCollect establishes the subscription and returns a handle, the
// caller dispatches tasks, then AwaitResults waits on the handle.
type Collector interface {
	PrepareCollect(ctx context.Context, req CollectRequest) (*CollectionHandle, error)
	AwaitResults(ctx context.Context, handle *CollectionHandle) ([]analyzer.TaskResult, error)
}

// NATSCollector implements Collector using a natsbus.Client.
type NATSCollector struct {
	bus    natsbus.Client
	logger *slog.Logger
	tracer trace.Tracer

	completeCounter metric.Int64Counter
	failedCounter   metric.Int64Counter
	retriedCounter  metric.Int64Counter
	collectLatency  metric.Float64Histogram
	// abandonCounter counts abandon control messages by task_type and
	// outcome ("published" or "publish_failed").
	abandonCounter metric.Int64Counter
	// noSuccessCounter counts collections failed with ErrNoSuccessfulResult,
	// by task_type.
	noSuccessCounter metric.Int64Counter
}

// NewNATSCollector creates a Collector backed by a NATS client.
func NewNATSCollector(bus natsbus.Client, opts ...CollectorOption) *NATSCollector {
	c := &NATSCollector{
		bus:    bus,
		logger: slog.Default(),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// CollectorOption configures a NATSCollector.
type CollectorOption func(*NATSCollector)

// WithCollectorTelemetry enables OTel tracing and metrics.
func WithCollectorTelemetry(tp trace.TracerProvider, mp metric.MeterProvider) CollectorOption {
	return func(c *NATSCollector) {
		if tp != nil {
			c.tracer = tp.Tracer("crosscodex/internal/analysis")
		}
		if mp != nil {
			meter := mp.Meter("crosscodex")
			c.completeCounter, _ = meter.Int64Counter(
				"analysis.tasks.completed",
				metric.WithDescription("Tasks completed successfully"),
			)
			c.failedCounter, _ = meter.Int64Counter(
				"analysis.tasks.failed",
				metric.WithDescription("Tasks that failed after all retries"),
			)
			c.retriedCounter, _ = meter.Int64Counter(
				"analysis.tasks.retried",
				metric.WithDescription("Task retry attempts"),
			)
			c.collectLatency, _ = meter.Float64Histogram(
				"analysis.collect.duration_ms",
				metric.WithDescription("Collector result collection duration in milliseconds"),
			)
			c.abandonCounter, _ = meter.Int64Counter(
				"analysis.collections.abandoned",
				metric.WithDescription("Collections that ended without all results, by task_type and outcome of the abandon publish"),
			)
			c.noSuccessCounter, _ = meter.Int64Counter(
				"analysis.collections.no_success",
				metric.WithDescription("Collections failed because their tasks kept failing with no success for analysis.engine.success_timeout, by task_type"),
			)
		}
	}
}

// WithCollectorLogger sets the logger.
func WithCollectorLogger(logger *slog.Logger) CollectorOption {
	return func(c *NATSCollector) {
		if logger != nil {
			c.logger = logger
		}
	}
}

// PrepareCollect establishes the result subscription and returns a handle for later awaiting.
// Call this BEFORE dispatching tasks to avoid publish-before-subscribe races in embedded NATS.
func (c *NATSCollector) PrepareCollect(ctx context.Context, req CollectRequest) (*CollectionHandle, error) {
	_, span := telemetry.StartSpan(c.tracer, ctx, "analysis.PrepareCollect")
	defer span.End()

	span.SetAttributes(
		attribute.Int("task.count", len(req.ExpectedIDs)),
	)

	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("prepare collect: %w: %w", ErrNoTenant, err)
	}

	// Fail closed: worker progress ticks extend the idle timeout, so without
	// a stage cap a lost task would keep the stage waiting forever.
	if req.StageTimeout <= 0 {
		err := fmt.Errorf("prepare collect for job %s: stage timeout %s is not positive, so the stage would have no upper bound; "+
			"build the engine with analysis.WithStageTimeout fed by pipeline.stage_timeout: %w",
			req.JobID, req.StageTimeout, ErrNoStageTimeout)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	// Fail closed: without a success window, a stage whose every LLM call
	// fails keeps the idle timer alive with its error results and runs
	// until the stage cap.
	if req.SuccessTimeout <= 0 {
		err := fmt.Errorf("prepare collect for job %s: success timeout %s is not positive, so a stage whose every LLM call fails "+
			"would run until pipeline.stage_timeout; set CollectRequest.SuccessTimeout from analysis.engine.success_timeout: %w",
			req.JobID, req.SuccessTimeout, ErrNoSuccessTimeout)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	subject, err := natsbus.ResultSubject(tenantID, req.TaskType, req.JobID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("prepare collect: building subject: %w", err)
	}

	// A fresh ID per collection, never derived from the job: resume and
	// retry reuse job IDs, and an abandon must drop only this collection's
	// tasks (see CollectionHandle.CollectionID).
	collectionID := uuid.NewString()
	abandonSubject, err := natsbus.AbandonedSubject(natsbus.AbandonedCollection{
		TenantID: tenantID, TaskType: req.TaskType, JobID: req.JobID, CollectionID: collectionID,
	})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("prepare collect: building abandon subject: %w", err)
	}
	released := make(chan struct{})
	// redispatchMu makes "not yet released, so redispatch" atomic with
	// respect to release closing released: once release returns, no retry
	// can publish, so the abandon that follows it is the last word.
	redispatchMu := &sync.Mutex{}

	// State tracking
	pending := make(map[string]bool)
	for _, id := range req.ExpectedIDs {
		pending[id] = true
	}
	retryCounts := make(map[string]int)
	taskMap := make(map[string]*analyzer.Task)
	for i := range req.Tasks {
		taskMap[req.Tasks[i].TaskID] = &req.Tasks[i]
	}

	var mu sync.Mutex
	watch := &successWatch{}
	// failed wakes AwaitResults to check the no-success window on a
	// failure; one pending wake-up is enough.
	failed := make(chan struct{}, 1)
	resultChan := make(chan analyzer.TaskResult, len(req.ExpectedIDs))
	doneChan := make(chan struct{})

	// Subscribe to result subject with handler
	handler := func(handlerCtx context.Context, msg *natsbus.Message) error {
		taskID := getHeader(msg.Headers, headerTaskID)
		if taskID == "" {
			c.logger.Warn("result message missing task ID", "subject", msg.Subject)
			return nil
		}

		mu.Lock()
		defer mu.Unlock()

		// Ignore unknown task IDs
		if !pending[taskID] {
			c.logger.Debug("ignoring result for unknown task", "task_id", taskID)
			return nil
		}

		// Extract error from headers
		errorMsg := getHeader(msg.Headers, "X-Error")
		var taskErr error
		if errorMsg != "" {
			c.logger.Error("task error received",
				"task_id", taskID,
				"raw_error", errorMsg,
			)
			taskErr = sanitizeError(errorMsg)
		}

		// Deserialize result payload
		var resultPayload proto.Message
		if len(msg.Data) > 0 && taskErr == nil {
			s := &structpb.Struct{}
			if err := proto.Unmarshal(msg.Data, s); err != nil {
				c.logger.Error("failed to unmarshal result", "task_id", taskID, "error", err)
				taskErr = fmt.Errorf("task_failed")
			} else {
				resultPayload = s
			}
		}

		// Handle error results with retry
		if taskErr != nil {
			now := time.Now()
			// A failure more than the idle Timeout after the previous one
			// ends a starvation: the run before it no longer counts, and
			// the window restarts here.
			if watch.failures > 0 && now.Sub(watch.lastFailure) > req.Timeout {
				watch.failures = 0
			}
			if watch.failures == 0 {
				watch.firstFailure = now
			}
			watch.failures++
			watch.lastFailure = now
			watch.lastErr = taskErr
			select {
			case failed <- struct{}{}:
			default:
			}
			retryCount := retryCounts[taskID]
			// No answer within max_tokens is deterministic for the same request,
			// so a retry would pay for the same empty reply.
			if retryCount < req.MaxRetries && !errors.Is(taskErr, llmclient.ErrOutputTruncated) {
				// Retry: redispatch and keep in pending
				// The handler context comes from the NATS client and never
				// carries a tenant, but the dispatcher requires one. Use the
				// tenant this collection was prepared for (the same one that
				// built the result subject), not a message header.
				// The error branch is unreachable: tenantID was validated when the result subject was built.
				redispatchCtx, err := tenant.WithTenant(handlerCtx, tenantID)
				if err != nil {
					return fmt.Errorf("redispatch task %s: %w", taskID, err)
				}

				retryCounts[taskID] = retryCount + 1

				if c.retriedCounter != nil {
					c.retriedCounter.Add(handlerCtx, 1)
				}

				// Get the original task for redispatch
				task := taskMap[taskID]
				if task == nil {
					c.logger.Error("missing original task for redispatch", "task_id", taskID)
					delete(pending, taskID)
					resultChan <- analyzer.TaskResult{
						TaskID:   taskID,
						TaskType: string(req.TaskType),
						Error:    fmt.Errorf("%w: missing original task", ErrRetryExhausted),
					}
					return nil
				}

				// Compute backoff and schedule redispatch
				backoff := computeBackoff(retryCount, req.Backoff)
				go func(t *analyzer.Task, retry int, delay time.Duration) {
					timer := time.NewTimer(delay)
					defer timer.Stop()

					select {
					case <-timer.C:
						redispatchMu.Lock()
						defer redispatchMu.Unlock()
						select {
						case <-released:
							// The collection ended while the timer fired.
							return
						default:
						}
						if err := req.Dispatcher.Redispatch(redispatchCtx, *t, req.TaskType, req.JobID, collectionID, retry+1); err != nil {
							c.logger.Error("redispatch failed", "task_id", t.TaskID, "error", err)
							// On redispatch failure, mark as failed
							mu.Lock()
							delete(pending, t.TaskID)
							mu.Unlock()
							resultChan <- analyzer.TaskResult{
								TaskID:   t.TaskID,
								TaskType: t.TaskType,
								Error:    fmt.Errorf("%w: redispatch failed: %w", ErrRetryExhausted, err),
							}
						}
					case <-ctx.Done():
						return
					case <-released:
						// The collection ended; nobody awaits this retry.
						return
					}
				}(task, retryCount, backoff)

				return nil
			}

			// Retries exhausted
			if c.failedCounter != nil {
				c.failedCounter.Add(handlerCtx, 1)
			}

			delete(pending, taskID)
			finalErr := fmt.Errorf("%w: %w", ErrRetryExhausted, taskErr)
			if errors.Is(taskErr, llmclient.ErrOutputTruncated) {
				finalErr = taskErr
			}
			resultChan <- analyzer.TaskResult{
				TaskID:   taskID,
				TaskType: string(req.TaskType),
				Error:    finalErr,
			}
			return nil
		}

		// Success
		*watch = successWatch{lastSuccess: time.Now()}
		if c.completeCounter != nil {
			c.completeCounter.Add(handlerCtx, 1)
		}

		delete(pending, taskID)
		resultChan <- analyzer.TaskResult{
			TaskID:   taskID,
			TaskType: string(req.TaskType),
			Result:   resultPayload,
		}
		return nil
	}

	sub, err := c.bus.Subscribe(ctx, subject, handler)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("prepare collect: subscribing to %s: %w", subject, err)
	}

	// Ticks come from every worker in the pool, for any tenant, job or
	// daemon. A pending tick is enough to reset the idle timer, so a full
	// buffer drops the tick instead of blocking the NATS callback.
	progress := make(chan struct{}, 1)
	progressSub, err := c.bus.SubscribeWorkerProgress(ctx, func() {
		select {
		case progress <- struct{}{}:
		default:
		}
	})
	if err != nil {
		if unsubErr := sub.Unsubscribe(); unsubErr != nil {
			c.logger.Warn("failed to unsubscribe", "subject", subject, "error", unsubErr)
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("prepare collect: subscribing to worker progress: %w", err)
	}

	span.SetStatus(codes.Ok, "subscription established")

	return &CollectionHandle{
		sub:          sub,
		progressSub:  progressSub,
		progress:     progress,
		resultChan:   resultChan,
		doneChan:     doneChan,
		pending:      pending,
		mu:           &mu,
		watch:        watch,
		failed:       failed,
		subject:      subject,
		Req:          req,
		CollectionID: collectionID,
		released:     released,
		redispatchMu: redispatchMu,
		publishAbandon: func(ctx context.Context) {
			c.publishAbandon(ctx, tenantID, abandonSubject, req)
		},
	}, nil
}

// abandonPublishTimeout bounds an abandon publish, which outlives the
// collection's cancelled context, so a stuck NATS connection cannot hold up
// the stage's return or a shutdown waiting for it. It matches the bound on the
// pipeline's other final-state writes.
const abandonPublishTimeout = 30 * time.Second

// publishAbandon publishes the abandon control message for one collection.
// It runs as the collection ends, often because ctx was cancelled (a
// cancelled job), so it publishes on a context that keeps ctx's values and
// trace but not its cancellation, for at most abandonPublishTimeout, scoped
// to the collection's tenant. A failure is logged and counted here and never
// returned: the stage already has its own error, and a lost abandon only lets
// the workers finish tasks whose results nobody reads, which is what happened
// before abandons existed.
func (c *NATSCollector) publishAbandon(ctx context.Context, tenantID, subject string, req CollectRequest) {
	outcome := "published"
	// The error branch is unreachable: tenantID was validated when the
	// result subject was built.
	pubCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), abandonPublishTimeout)
	defer cancel()
	pubCtx, err := tenant.WithTenant(pubCtx, tenantID)
	if err == nil {
		err = c.bus.Publish(pubCtx, subject, nil)
	}
	if err != nil {
		outcome = "publish_failed"
		c.logger.Warn("failed to publish collection abandon; workers will still run this collection's queued LLM tasks "+
			"and their results will be discarded. Check the NATS connection",
			"job_id", req.JobID, "task_type", string(req.TaskType), "subject", subject, "error", err)
	}
	if c.abandonCounter != nil {
		c.abandonCounter.Add(ctx, 1, metric.WithAttributes(
			attribute.String("task_type", string(req.TaskType)),
			attribute.String("outcome", outcome),
		))
	}
}

// AwaitResults waits for all expected results using a previously prepared collection handle.
// Call this AFTER dispatching tasks.
func (c *NATSCollector) AwaitResults(ctx context.Context, handle *CollectionHandle) ([]analyzer.TaskResult, error) {
	start := time.Now()
	ctx, span := telemetry.StartSpan(c.tracer, ctx, "analysis.AwaitResults")
	defer span.End()

	span.SetAttributes(
		attribute.Int("task.count", len(handle.Req.ExpectedIDs)),
	)

	// complete is set only when every expected result arrived; any other
	// return leaves tasks the workers would still run for nobody.
	complete := false
	// Release before abandon: release stops every retry, so no task of
	// this collection is published after its abandon.
	defer func() {
		if err := handle.release(); err != nil {
			c.logger.Warn("failed to release collection subscriptions", "job_id", handle.Req.JobID, "error", err)
		}
		if !complete {
			handle.abandon(ctx)
		}
	}()

	results := make([]analyzer.TaskResult, 0, len(handle.Req.ExpectedIDs))

	// Collection loop monitor
	// Timeout bounds the time the whole worker pool is silent, not the batch
	// and not this collection alone: a catalog's pair analyzers dispatch
	// thousands of LLM tasks that a healthy pool completes over hours, and
	// concurrent stages, jobs and daemons share that pool, so this
	// collection may get no result of its own while the pool works through
	// another's queue. Its own results and any worker progress tick reset
	// the timer; a stalled pool still fails the stage once both stop.
	idle := time.NewTimer(handle.Req.Timeout)
	defer idle.Stop()
	// StageTimeout caps the whole collection and nothing resets it: ticks
	// from other stages, jobs or tenants keep the idle timer alive, so this
	// is what fails a stage whose task was lost.
	stageCap := time.NewTimer(handle.Req.StageTimeout)
	defer stageCap.Stop()
	// noSuccess fails a collection whose tasks keep failing: their error
	// results and the ticks they cause keep the idle timer alive, so
	// without it a stage whose every LLM call fails runs until the stage
	// cap. Only this collection's own successes reset it, never ticks or
	// another collection's results: the question is whether this stage's
	// model calls work, not whether the pool is alive. See checkNoSuccess
	// for when it trips. A handle built by a test Collector has no watch,
	// and so no no-success check.
	var noSuccess *time.Timer
	var noSuccessC <-chan time.Time
	if handle.watch != nil {
		noSuccess = time.NewTimer(handle.Req.SuccessTimeout)
		defer noSuccess.Stop()
		noSuccessC = noSuccess.C
	}
	stopMonitor := make(chan struct{})
	defer close(stopMonitor)

	go func() {
		for {
			handle.mu.Lock()
			remaining := len(handle.pending)
			handle.mu.Unlock()

			if remaining == 0 {
				close(handle.doneChan)
				return
			}

			select {
			case <-ctx.Done():
				return
			case <-handle.doneChan:
				return
			case <-stopMonitor:
				return
			default:
				time.Sleep(10 * time.Millisecond)
			}
		}
	}()

	for {
		var nse *NoSuccessError
		select {
		case <-ctx.Done():
			span.RecordError(ctx.Err())
			span.SetStatus(codes.Error, "context cancelled")
			return results, fmt.Errorf("await results: %w", ctx.Err())

		case <-idle.C:
			span.RecordError(ErrTaskTimeout)
			span.SetStatus(codes.Error, "timeout")

			// Record metrics
			span.SetAttributes(
				attribute.Int("completed", len(results)),
				attribute.Int("failed", len(handle.Req.ExpectedIDs)-len(results)),
			)
			if c.collectLatency != nil {
				c.collectLatency.Record(ctx, float64(time.Since(start).Milliseconds()))
			}

			return results, fmt.Errorf("await results for job %s: no task result or worker progress for %s, %d of %d tasks complete; check that LLM workers are running, or raise analysis.engine.task_timeout if a single task takes longer than this: %w",
				handle.Req.JobID, handle.Req.Timeout, len(results), len(handle.Req.ExpectedIDs), ErrTaskTimeout)

		case <-stageCap.C:
			span.RecordError(ErrStageTimeout)
			span.SetStatus(codes.Error, "stage timeout")
			span.SetAttributes(
				attribute.Int("completed", len(results)),
				attribute.Int("failed", len(handle.Req.ExpectedIDs)-len(results)),
			)
			if c.collectLatency != nil {
				c.collectLatency.Record(ctx, float64(time.Since(start).Milliseconds()))
			}

			return results, fmt.Errorf("await results for job %s: stage ran longer than pipeline.stage_timeout (%s), %d of %d tasks complete; a task may have been lost, so rerun the job, or raise pipeline.stage_timeout if the catalog legitimately needs longer: %w",
				handle.Req.JobID, handle.Req.StageTimeout, len(results), len(handle.Req.ExpectedIDs), ErrStageTimeout)

		case <-noSuccessC:
			nse = checkNoSuccess(handle, noSuccess)

		case <-handle.failed:
			if noSuccess != nil {
				nse = checkNoSuccess(handle, noSuccess)
			}

		case result := <-handle.resultChan:
			results = append(results, result)
			idle.Reset(handle.Req.Timeout)

		case <-handle.progress:
			idle.Reset(handle.Req.Timeout)

		case <-handle.doneChan:
			// Drain any remaining results from the channel
		draining:
			for {
				select {
				case r := <-handle.resultChan:
					results = append(results, r)
				default:
					break draining
				}
			}

			// All results collected
			span.SetAttributes(
				attribute.Int("completed", len(results)),
			)
			span.SetStatus(codes.Ok, "")

			if c.collectLatency != nil {
				c.collectLatency.Record(ctx, float64(time.Since(start).Milliseconds()))
			}

			complete = true
			return results, nil
		}

		if nse != nil {
			// The handler pushes a result under the lock checkNoSuccess
			// read the watch under, so every failure it counted is queued:
			// return them all, so the caller sees the last error's result
			// (a truncated answer, say) and stores the results paid for.
			for len(handle.resultChan) > 0 {
				results = append(results, <-handle.resultChan)
			}
			span.RecordError(ErrNoSuccessfulResult)
			span.SetStatus(codes.Error, "no successful result")
			span.SetAttributes(
				attribute.Int("completed", len(results)),
				attribute.Int("failed", len(handle.Req.ExpectedIDs)-len(results)),
				attribute.Int("failures_since_success", nse.Failures),
			)
			if c.collectLatency != nil {
				c.collectLatency.Record(ctx, float64(time.Since(start).Milliseconds()))
			}
			if c.noSuccessCounter != nil {
				c.noSuccessCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("task_type", string(handle.Req.TaskType))))
			}
			return results, nse
		}
	}
}

// checkNoSuccess runs when the no-success timer fires or a failed result
// arrives, and returns the collection's NoSuccessError once it must fail:
// no success for SuccessTimeout since the first failure after the last
// success (or after the start), and the latest failure arrived within the
// idle Timeout (analysis.engine.task_timeout), so failures are still
// arriving. A collection whose own results stopped is starved behind
// another's queue or silent; that is the idle timeout's and the stage cap's
// to judge, so the timer is stopped. Its next failure, more than Timeout
// after the previous one, restarts the window (the handler resets the run),
// so it trips only after a full window of failures arriving again.
// Otherwise it re-arms timer for when the window could next end. It
// returns nil once every result is in, so doneChan completes the
// collection instead of racing it.
func checkNoSuccess(handle *CollectionHandle, timer *time.Timer) *NoSuccessError {
	handle.mu.Lock()
	w := *handle.watch
	remaining := len(handle.pending)
	handle.mu.Unlock()
	window := handle.Req.SuccessTimeout
	if remaining == 0 {
		return nil
	}
	if w.failures == 0 {
		timer.Reset(window)
		return nil
	}
	if left := window - time.Since(w.firstFailure); left > 0 {
		timer.Reset(left)
		return nil
	}
	if time.Since(w.lastFailure) > handle.Req.Timeout {
		timer.Stop()
		return nil
	}
	return &NoSuccessError{
		JobID: handle.Req.JobID, Window: window, Failures: w.failures,
		LastSuccess: w.lastSuccess, LastError: w.lastErr,
	}
}

// successWatch is a collection's run of failed task results since its last
// success, which the no-success window measures. CollectionHandle.mu guards
// it.
type successWatch struct {
	// lastSuccess is when the last successful result arrived; zero before
	// the first.
	lastSuccess time.Time
	// firstFailure is when the first failure after lastSuccess arrived;
	// zero while failures is 0.
	firstFailure time.Time
	// lastFailure is when the latest failure arrived.
	lastFailure time.Time
	failures    int
	lastErr     error
}

// getHeader extracts the first value of a header, or empty string if not present.
func getHeader(headers map[string][]string, key string) string {
	vals := headers[key]
	if len(vals) == 0 {
		return ""
	}
	return vals[0]
}

// sanitizeError wraps task errors with generic categories.
func sanitizeError(errorMsg string) error {
	if errorMsg == natsbus.ResultErrorOutputTruncated {
		return fmt.Errorf("task_output_truncated: %w", llmclient.ErrOutputTruncated)
	}
	lower := strings.ToLower(errorMsg)
	if strings.Contains(lower, "timeout") || strings.Contains(lower, "deadline") {
		return fmt.Errorf("task_timeout")
	}
	if strings.Contains(lower, "invalid") || strings.Contains(lower, "malformed") {
		return fmt.Errorf("task_invalid")
	}
	return fmt.Errorf("task_failed")
}

// Compile-time interface check.
var _ Collector = (*NATSCollector)(nil)
