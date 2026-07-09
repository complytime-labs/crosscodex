package analysis

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"

	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

const (
	headerTaskID     = "X-Task-Id"
	headerTaskType   = "X-Task-Type"
	headerJobID      = "X-Job-Id"
	headerRetryCount = "X-Retry-Count"
)

// NATSDispatcher implements Dispatcher using a natsbus.Client.
type NATSDispatcher struct {
	bus    natsbus.Client
	logger *slog.Logger
	tracer trace.Tracer

	dispatchCounter metric.Int64Counter
}

// NewNATSDispatcher creates a Dispatcher backed by a NATS client.
func NewNATSDispatcher(bus natsbus.Client, opts ...DispatcherOption) *NATSDispatcher {
	d := &NATSDispatcher{
		bus:    bus,
		logger: slog.Default(),
	}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// DispatcherOption configures a NATSDispatcher.
type DispatcherOption func(*NATSDispatcher)

// WithDispatcherTelemetry enables OTel tracing and metrics.
func WithDispatcherTelemetry(tp trace.TracerProvider, mp metric.MeterProvider) DispatcherOption {
	return func(d *NATSDispatcher) {
		if tp != nil {
			d.tracer = tp.Tracer("crosscodex/internal/analysis")
		}
		if mp != nil {
			meter := mp.Meter("crosscodex")
			d.dispatchCounter, _ = meter.Int64Counter(
				"analysis.tasks.dispatched",
				metric.WithDescription("Tasks published to NATS"),
			)
		}
	}
}

// WithDispatcherLogger sets the logger.
func WithDispatcherLogger(logger *slog.Logger) DispatcherOption {
	return func(d *NATSDispatcher) {
		if logger != nil {
			d.logger = logger
		}
	}
}

func (d *NATSDispatcher) Dispatch(ctx context.Context, tasks []analyzer.Task, taskType natsbus.TaskType, jobID string) error {
	for _, task := range tasks {
		if err := d.publishTask(ctx, task, taskType, jobID, 0); err != nil {
			return err
		}
	}
	return nil
}

func (d *NATSDispatcher) Redispatch(ctx context.Context, task analyzer.Task, taskType natsbus.TaskType, jobID string, retryCount int) error {
	return d.publishTask(ctx, task, taskType, jobID, retryCount)
}

func (d *NATSDispatcher) publishTask(ctx context.Context, task analyzer.Task, taskType natsbus.TaskType, jobID string, retryCount int) error {
	ctx, span := telemetry.StartSpan(d.tracer, ctx, "analysis.DispatchTask")
	defer span.End()

	span.SetAttributes(
		attribute.String("task.id", task.TaskID),
		attribute.String("task.type", string(taskType)),
		attribute.Int("retry.count", retryCount),
	)

	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("dispatch task %s: %w: %w", task.TaskID, ErrNoTenant, err)
	}

	subject, err := natsbus.WorkSubject(tenantID, taskType, jobID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("dispatch task %s: building subject: %w", task.TaskID, err)
	}

	data, err := proto.Marshal(task.Payload)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("dispatch task %s: marshaling payload: %w", task.TaskID, err)
	}

	headers := map[string][]string{
		headerTaskID:     {task.TaskID},
		headerTaskType:   {task.TaskType},
		headerJobID:      {jobID},
		headerRetryCount: {strconv.Itoa(retryCount)},
	}

	if err := d.bus.PublishWithHeaders(ctx, subject, data, headers); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("dispatch task %s to %s: %w", task.TaskID, subject, err)
	}

	if d.dispatchCounter != nil {
		d.dispatchCounter.Add(ctx, 1)
	}

	span.SetStatus(codes.Ok, "")
	return nil
}

// Compile-time interface check.
var _ Dispatcher = (*NATSDispatcher)(nil)
