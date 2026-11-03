package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

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

	controlSub, err := w.bus.Subscribe(ctx, natsbus.AbandonedSubjectFilter, w.handleAbandoned)
	if err != nil {
		return fmt.Errorf("worker start: subscribing to abandon control subjects: %w", err)
	}

	sub, err := w.bus.QueueSubscribe(ctx, workSubjectGlob, queueGroup(&w.cfg), w.handleMessage)
	if err != nil {
		if unsubErr := controlSub.Unsubscribe(); unsubErr != nil {
			w.logger.Warn("failed to unsubscribe from abandon control subjects", "error", unsubErr)
		}
		return fmt.Errorf("worker start: subscribing to work subjects: %w", err)
	}

	w.sub = sub
	w.controlSub = controlSub
	w.logger.Info("worker started",
		"queue_group", queueGroup(&w.cfg),
		"subject", workSubjectGlob,
		"control_subject", natsbus.AbandonedSubjectFilter,
	)
	return nil
}

// Stop drains the work and control subscriptions and waits for in-flight
// tasks to complete.
//
// The ctx parameter is accepted for interface consistency but cannot be used
// to enforce a deadline on Drain: the natsbus.Subscription interface exposes
// only a no-context Drain(). Callers that need bounded shutdown should set a
// DrainTimeout on the underlying NATS connection before calling Stop.
func (w *Worker) Stop(_ context.Context) error {
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
	w.sub = nil
	w.controlSub = nil
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("worker stop: %w", err)
	}

	w.logger.Info("worker stopped")
	return nil
}
