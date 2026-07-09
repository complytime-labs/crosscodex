package analysis

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// Collector subscribes to result subjects and collects task results.
type Collector interface {
	Collect(ctx context.Context, req CollectRequest) ([]analyzer.TaskResult, error)
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

func (c *NATSCollector) Collect(ctx context.Context, req CollectRequest) ([]analyzer.TaskResult, error) {
	start := time.Now()
	ctx, span := telemetry.StartSpan(c.tracer, ctx, "analysis.CollectResults")
	defer span.End()

	span.SetAttributes(
		attribute.Int("task.count", len(req.ExpectedIDs)),
	)

	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("collect results: %w: %w", ErrNoTenant, err)
	}

	subject, err := natsbus.ResultSubject(tenantID, req.TaskType, req.JobID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("collect results: building subject: %w", err)
	}

	// State tracking
	pending := make(map[string]bool)
	for _, id := range req.ExpectedIDs {
		pending[id] = true
	}
	retryCounts := make(map[string]int)
	results := make([]analyzer.TaskResult, 0, len(req.ExpectedIDs))
	taskMap := make(map[string]*analyzer.Task) // Store original tasks for retry
	for i := range req.Tasks {
		taskMap[req.Tasks[i].TaskID] = &req.Tasks[i]
	}

	var mu sync.Mutex
	resultChan := make(chan analyzer.TaskResult, len(req.ExpectedIDs))
	doneChan := make(chan struct{})

	// Subscribe to result subject
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
			retryCount := retryCounts[taskID]
			if retryCount < req.MaxRetries {
				// Retry: redispatch and keep in pending
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
						if err := req.Dispatcher.Redispatch(handlerCtx, *t, req.TaskType, req.JobID, retry+1); err != nil {
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
					}
				}(task, retryCount, backoff)

				return nil
			}

			// Retries exhausted
			if c.failedCounter != nil {
				c.failedCounter.Add(handlerCtx, 1)
			}

			delete(pending, taskID)
			resultChan <- analyzer.TaskResult{
				TaskID:   taskID,
				TaskType: string(req.TaskType),
				Error:    fmt.Errorf("%w: %w", ErrRetryExhausted, taskErr),
			}
			return nil
		}

		// Success
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
		return nil, fmt.Errorf("collect results: subscribing to %s: %w", subject, err)
	}
	defer func() {
		if err := sub.Unsubscribe(); err != nil {
			c.logger.Warn("failed to unsubscribe", "subject", subject, "error", err)
		}
	}()

	// Collection loop
	timeout := time.After(req.Timeout)
	go func() {
		for {
			mu.Lock()
			remaining := len(pending)
			mu.Unlock()

			if remaining == 0 {
				close(doneChan)
				return
			}

			select {
			case <-ctx.Done():
				return
			case <-doneChan:
				return
			default:
				time.Sleep(10 * time.Millisecond)
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			span.RecordError(ctx.Err())
			span.SetStatus(codes.Error, "context cancelled")
			return results, fmt.Errorf("collect results: %w", ctx.Err())

		case <-timeout:
			span.RecordError(ErrTaskTimeout)
			span.SetStatus(codes.Error, "timeout")

			// Record metrics
			span.SetAttributes(
				attribute.Int("completed", len(results)),
				attribute.Int("failed", len(req.ExpectedIDs)-len(results)),
			)
			if c.collectLatency != nil {
				c.collectLatency.Record(ctx, float64(time.Since(start).Milliseconds()))
			}

			return results, fmt.Errorf("collect results for job %s: %w", req.JobID, ErrTaskTimeout)

		case result := <-resultChan:
			results = append(results, result)

		case <-doneChan:
			// Drain any remaining results from the channel
		draining:
			for {
				select {
				case r := <-resultChan:
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

			return results, nil
		}
	}
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
