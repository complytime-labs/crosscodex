package worker

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

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

	mu  sync.Mutex
	sub natsbus.Subscription
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
	return w
}

// Start subscribes to NATS work subjects and begins processing tasks.
func (w *Worker) Start(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.sub != nil {
		return ErrAlreadyStarted
	}

	sub, err := w.bus.QueueSubscribe(ctx, workSubjectGlob, queueGroup(&w.cfg), w.handleMessage)
	if err != nil {
		return fmt.Errorf("worker start: subscribing to work subjects: %w", err)
	}

	w.sub = sub
	w.logger.Info("worker started",
		"queue_group", queueGroup(&w.cfg),
		"subject", workSubjectGlob,
	)
	return nil
}

// Stop drains the subscription and waits for in-flight tasks to complete.
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

	err := w.sub.Drain()
	w.sub = nil
	if err != nil {
		return fmt.Errorf("worker stop: draining subscription: %w", err)
	}

	w.logger.Info("worker stopped")
	return nil
}
