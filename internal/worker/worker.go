package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
)

// workSubjectGlob is the NATS subject wildcard for all work messages.
// It must stay in sync with natsbus.WorkSubject's schema:
//
//	crosscodex.work.{tenant_id}.{task_type}.{job_id}
const workSubjectGlob = "crosscodex.work.>"

// Worker executes LLM tasks received via NATS.
type Worker struct {
	bus    natsbus.Client
	llm    llmclient.Client
	cfg    WorkerConfig
	logger *slog.Logger
	tracer trace.Tracer

	// Metrics
	taskCounter  metric.Int64Counter
	taskDuration metric.Float64Histogram
	errorCounter metric.Int64Counter
	llmLatency   metric.Float64Histogram
	// droppedCounter counts tasks dropped before any LLM call, by reason
	// and task_type; abandonedGauge reports the size of abandoned.
	droppedCounter metric.Int64Counter
	abandonedGauge metric.Int64Gauge

	// abandonTTL is how long abandoned remembers a collection; see
	// WithAbandonTTL.
	abandonTTL time.Duration
	abandoned  *abandonedSet

	mu         sync.Mutex
	sub        natsbus.Subscription
	controlSub natsbus.Subscription
	// run holds the in-flight task state of the current Start; see taskRun.
	run *taskRun
}

// taskRun bounds and tracks the tasks one Start runs. Each Start makes a new
// one and its work subscription's callback captures it, so a callback the
// NATS client delivers after Stop never touches a later run's state.
type taskRun struct {
	// slots holds one token per running task; its capacity is
	// WorkerConfig.MaxInFlight.
	slots chan struct{}
	// stopping is closed by Stop, under mu, so no task starts after it.
	stopping chan struct{}
	mu       sync.Mutex
	tasks    sync.WaitGroup
}

// New creates a Worker with the given dependencies.
func New(bus natsbus.Client, llm llmclient.Client, cfg WorkerConfig, opts ...Option) *Worker {
	w := &Worker{
		bus:    bus,
		llm:    llm,
		cfg:    cfg,
		logger: slog.Default(),
	}
	for _, opt := range opts {
		opt(w)
	}
	w.abandoned = newAbandonedSet(w.abandonTTL, cfg.MaxAbandonedCollections, time.Now)
	if w.abandonedGauge != nil {
		w.abandoned.onResize = func(n int) {
			w.abandonedGauge.Record(context.Background(), int64(n))
		}
	}
	return w
}

// Start subscribes to NATS work subjects and begins processing tasks.
//
// It first subscribes, outside the queue group, to every tenant's abandon
// control messages (natsbus.AbandonedSubjectFilter): queue-group delivery
// would tell only one worker, and every worker may hold the abandoned
// collection's queued tasks. Subscribing before the work subjects means no
// task is taken before the worker can hear about abandons.
func (w *Worker) Start(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.sub != nil {
		return ErrAlreadyStarted
	}

	// Fail closed: without a TTL every abandon would expire on arrival and
	// the worker would silently run abandoned collections' tasks.
	if w.abandonTTL <= 0 {
		return fmt.Errorf("worker start: abandon TTL %s is not positive, so the worker could not drop the tasks of abandoned collections; "+
			"build the worker with worker.WithAbandonTTL fed by pipeline.stage_timeout: %w", w.abandonTTL, ErrNoAbandonTTL)
	}
	if w.cfg.MaxAbandonedCollections <= 0 {
		return fmt.Errorf("worker start: worker.max_abandoned_collections is %d, so the worker could not remember any abandoned collection; "+
			"set it to a positive count such as the default 10000: %w", w.cfg.MaxAbandonedCollections, ErrNoAbandonedCollectionsLimit)
	}
	if w.cfg.MaxInFlight <= 0 {
		return fmt.Errorf("worker start: worker.max_in_flight is %d, so the worker could never run a task; "+
			"set it to a positive count such as the default 1: %w", w.cfg.MaxInFlight, ErrNoInFlightLimit)
	}

	controlSub, err := w.bus.Subscribe(ctx, natsbus.AbandonedSubjectFilter, w.handleAbandoned)
	if err != nil {
		return fmt.Errorf("worker start: subscribing to abandon control subjects: %w", err)
	}

	run := &taskRun{
		slots:    make(chan struct{}, w.cfg.MaxInFlight),
		stopping: make(chan struct{}),
	}
	sub, err := w.bus.QueueSubscribe(ctx, workSubjectGlob, queueGroup(&w.cfg), func(ctx context.Context, msg *natsbus.Message) error {
		w.dispatch(ctx, run, msg)
		return nil
	})
	if err != nil {
		if unsubErr := controlSub.Unsubscribe(); unsubErr != nil {
			w.logger.Warn("failed to unsubscribe from abandon control subjects", "error", unsubErr)
		}
		return fmt.Errorf("worker start: subscribing to work subjects: %w", err)
	}

	w.sub = sub
	w.controlSub = controlSub
	w.run = run
	w.logger.Info("worker started",
		"queue_group", queueGroup(&w.cfg),
		"max_in_flight", w.cfg.MaxInFlight,
		"subject", workSubjectGlob,
		"control_subject", natsbus.AbandonedSubjectFilter,
	)
	return nil
}

// dispatch runs msg's task on its own goroutine once one of run's
// WorkerConfig.MaxInFlight slots is free. It blocks the subscription's
// delivery goroutine while every slot is taken, so later tasks wait in the
// NATS client's pending queue, and returns once the task has started or been
// dropped because Stop was called. Each task keeps its own retry and abandon
// handling (handleMessage); only the start is bounded here.
func (w *Worker) dispatch(ctx context.Context, run *taskRun, msg *natsbus.Message) {
	select {
	case run.slots <- struct{}{}:
	case <-run.stopping:
		w.dropStopping(ctx, msg)
		return
	}

	run.mu.Lock()
	select {
	case <-run.stopping:
		run.mu.Unlock()
		<-run.slots
		w.dropStopping(ctx, msg)
		return
	default:
	}
	run.tasks.Add(1)
	run.mu.Unlock()

	go func() {
		defer run.tasks.Done()
		defer func() { <-run.slots }()
		if err := w.handleMessage(ctx, msg); err != nil {
			w.logger.Error("task handler error", "subject", msg.Subject, "error", err)
		}
	}()
}

// dropStopping records a task that Stop kept from starting. Its stage gets no
// result from this worker for it.
func (w *Worker) dropStopping(ctx context.Context, msg *natsbus.Message) {
	taskType := getHeader(msg.Headers, headerTaskType)
	w.logger.Warn("worker stopping: a task waiting for a free slot was not run and gets no result from this worker",
		"task_id", getHeader(msg.Headers, headerTaskID),
		"task_type", taskType,
		"job_id", getHeader(msg.Headers, headerJobID),
		"tenant_id", msg.Metadata.TenantID,
	)
	if w.droppedCounter != nil {
		w.droppedCounter.Add(ctx, 1, metric.WithAttributes(
			attribute.String("reason", "worker_stopping"),
			attribute.String("task_type", taskType),
		))
	}
}

// Stop drains the work and control subscriptions, keeps tasks still waiting
// for a slot from starting, and waits for running tasks to finish.
//
// ctx bounds only the wait for running tasks: when it ends first, Stop
// returns an error wrapping ctx.Err() and the tasks keep running in the
// background. It cannot bound Drain: the natsbus.Subscription interface
// exposes only a no-context Drain(). Callers that need bounded draining
// should set a DrainTimeout on the underlying NATS connection before calling
// Stop.
func (w *Worker) Stop(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.sub == nil {
		return ErrNotStarted
	}

	var errs []error
	if err := w.sub.Drain(); err != nil {
		errs = append(errs, fmt.Errorf("draining work subscription: %w", err))
	}
	if err := w.controlSub.Drain(); err != nil {
		errs = append(errs, fmt.Errorf("draining abandon control subscription: %w", err))
	}
	run := w.run
	w.sub = nil
	w.controlSub = nil
	w.run = nil

	run.mu.Lock()
	close(run.stopping)
	run.mu.Unlock()
	finished := make(chan struct{})
	go func() {
		run.tasks.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-ctx.Done():
		errs = append(errs, fmt.Errorf("waiting for in-flight tasks: %d tasks still running: %w", len(run.slots), ctx.Err()))
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("worker stop: %w", err)
	}

	w.logger.Info("worker stopped")
	return nil
}
