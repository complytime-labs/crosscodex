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

	"github.com/complytime-labs/crosscodex/pkg/telemetry"
)

// ProgressDelta is the change in one stage's task progress since the last
// write. Total is the stage's task count and replaces the stored one; every
// other count is added to the stored count, so a resumed stage accumulates
// onto its earlier run. Done is not carried: the store derives it as
// Succeeded+Failed. Succeeded includes CacheHits and other pre-built
// results. LastSuccessAt is the latest worker success in the delta, nil when
// there was none; results served without a worker never set it.
type ProgressDelta struct {
	Total, Succeeded, Failed, Retried, CacheHits int
	LastSuccessAt                                *time.Time
}

// ProgressStore persists stage progress deltas. The tenant comes from ctx.
// Implementations add each delta to the (tenant, job, stage) row, creating
// it on the first write.
type ProgressStore interface {
	AddStageProgress(ctx context.Context, jobID, stageName string, d ProgressDelta) error
}

// ProgressObserver receives a collection's task events (CollectRequest.Progress).
// Methods are called from the collector's result handler and retry goroutines,
// so implementations must be safe for concurrent use and must not block.
type ProgressObserver interface {
	// TaskSucceeded reports a task's successful result.
	TaskSucceeded()
	// TaskFailed reports a task's final failure, after any retries.
	TaskFailed()
	// TaskRetried reports one retry attempt of a task.
	TaskRetried()
}

// defaultProgressFlushInterval is used when WithProgressStore is given no
// positive interval. It matches the default of
// analysis.engine.progress_flush_interval in pkg/config, which crosscodexd
// passes; only engines built from unvalidated configs reach the fallback.
const defaultProgressFlushInterval = 10 * time.Second

// finalFlushTimeout bounds the end-of-stage write, which runs on a context
// detached from the stage's so a cancelled stage still records its final
// counts. It matches the bound on the pipeline's other final-state writes.
const finalFlushTimeout = 30 * time.Second

// progressMetrics are the stage progress instruments (nil-safe).
type progressMetrics struct {
	flushes       metric.Int64Counter
	flushDuration metric.Float64Histogram
}

func newProgressMetrics(mp metric.MeterProvider) progressMetrics {
	var m progressMetrics
	if mp == nil {
		return m
	}
	meter := mp.Meter("crosscodex")
	m.flushes, _ = meter.Int64Counter("analysis.progress.flushes.total",
		metric.WithDescription("Stage progress writes (attribute outcome: ok or failed)"))
	m.flushDuration, _ = meter.Float64Histogram("analysis.progress.flush.duration_ms",
		metric.WithDescription("Duration of one stage progress write"), metric.WithUnit("ms"))
	return m
}

// stageProgress accumulates one stage's task events in memory and writes
// them to a ProgressStore as deltas: on a ticker while the stage runs and
// once at Finish. A failed write keeps its delta for the next one, so no
// event is lost or counted twice. It implements ProgressObserver.
type stageProgress struct {
	store    ProgressStore
	jobID    string
	stage    string
	interval time.Duration
	logger   *slog.Logger
	tracer   trace.Tracer
	metrics  progressMetrics

	mu      sync.Mutex
	pending ProgressDelta
	dirty   bool

	// stop ends the ticker goroutine, which closes done on exit.
	stop       chan struct{}
	done       chan struct{}
	finishOnce sync.Once
}

func newStageProgress(store ProgressStore, jobID, stage string, interval time.Duration,
	logger *slog.Logger, tracer trace.Tracer, m progressMetrics) *stageProgress {
	if interval <= 0 {
		interval = defaultProgressFlushInterval
	}
	return &stageProgress{
		store: store, jobID: jobID, stage: stage, interval: interval,
		logger: logger, tracer: tracer, metrics: m,
	}
}

// Start records the stage's task count and its pre-built results, preBuilt
// of which cacheHits came from the result cache, and starts the ticker. The
// pre-built results count as succeeded but not as worker successes, so they
// leave LastSuccessAt unset. Call it once, before any observer event.
func (p *stageProgress) Start(ctx context.Context, total, preBuilt, cacheHits int) {
	p.mu.Lock()
	p.pending.Total = total
	p.pending.Succeeded += preBuilt
	p.pending.CacheHits += cacheHits
	p.dirty = true
	p.mu.Unlock()

	p.stop = make(chan struct{})
	p.done = make(chan struct{})
	go func() {
		defer close(p.done)
		ticker := time.NewTicker(p.interval)
		defer ticker.Stop()
		for {
			select {
			case <-p.stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				// A running stage never fails for its progress record: the
				// delta stays pending for the next write.
				if err := p.flush(ctx); err != nil {
					p.logger.WarnContext(ctx, "could not write stage progress; the stored counts lag until a later write succeeds",
						"analyzer", p.stage, "job_id", p.jobID, "error", err)
				}
			}
		}
	}()
}

func (p *stageProgress) TaskSucceeded() {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pending.Succeeded++
	p.pending.LastSuccessAt = &now
	p.dirty = true
}

func (p *stageProgress) TaskFailed() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pending.Failed++
	p.dirty = true
}

func (p *stageProgress) TaskRetried() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pending.Retried++
	p.dirty = true
}

// Finish stops the ticker and writes what is pending. It returns the final
// write's error, so the caller can mark the stage's record stale; later calls
// do nothing and return nil. The write runs detached from ctx's
// cancellation, bounded by finalFlushTimeout.
func (p *stageProgress) Finish(ctx context.Context) error {
	var err error
	p.finishOnce.Do(func() {
		if p.stop != nil {
			close(p.stop)
			<-p.done
		}
		flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalFlushTimeout)
		defer cancel()
		err = p.flush(flushCtx)
	})
	return err
}

// flush writes the pending delta, if any. On success the delta is cleared;
// on failure it is kept, merged with events that arrived meanwhile, and the
// error is counted and returned. Only the ticker goroutine and, after
// it has stopped, Finish call flush, so two writes never overlap.
func (p *stageProgress) flush(ctx context.Context) error {
	p.mu.Lock()
	if !p.dirty {
		p.mu.Unlock()
		return nil
	}
	d := p.pending
	p.pending = ProgressDelta{Total: d.Total}
	p.dirty = false
	p.mu.Unlock()

	ctx, span := telemetry.StartSpan(p.tracer, ctx, "analysis.progress.flush")
	defer span.End()
	span.SetAttributes(attribute.String("job.id", p.jobID), attribute.String("stage.name", p.stage))
	start := time.Now()
	err := p.store.AddStageProgress(ctx, p.jobID, p.stage, d)
	outcome := "ok"
	if err != nil {
		outcome = "failed"
	}
	if p.metrics.flushes != nil {
		p.metrics.flushes.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
	}
	if p.metrics.flushDuration != nil {
		p.metrics.flushDuration.Record(ctx, float64(time.Since(start).Milliseconds()))
	}
	if err == nil {
		span.SetStatus(codes.Ok, "")
		return nil
	}

	// Re-queue the failed delta. Accepted limit: the store has no
	// idempotency key, so a write that committed but whose response was lost
	// (e.g. the connection dropped after COMMIT) is re-queued here and its
	// counts are added twice by the next write.
	p.mu.Lock()
	p.pending.Succeeded += d.Succeeded
	p.pending.Failed += d.Failed
	p.pending.Retried += d.Retried
	p.pending.CacheHits += d.CacheHits
	if p.pending.LastSuccessAt == nil {
		p.pending.LastSuccessAt = d.LastSuccessAt
	}
	p.dirty = true
	p.mu.Unlock()

	err = fmt.Errorf("analysis: write stage progress of analyzer %q for job %q: %w", p.stage, p.jobID, err)
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
	return err
}
