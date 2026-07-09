package analysis

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// StageReporter reports analyzer stage events to the pipeline.
type StageReporter interface {
	ReportStageStarted(ctx context.Context, analyzerName, jobID string) error
	ReportStageCompleted(ctx context.Context, analyzerName, jobID string, output *analyzer.Output) error
	ReportStageFailed(ctx context.Context, analyzerName, jobID string, err error) error
}

// noopReporter discards all stage events.
type noopReporter struct{}

func (noopReporter) ReportStageStarted(context.Context, string, string) error { return nil }
func (noopReporter) ReportStageCompleted(context.Context, string, string, *analyzer.Output) error {
	return nil
}
func (noopReporter) ReportStageFailed(context.Context, string, string, error) error { return nil }

// NATSStageReporter publishes stage events to NATS pipeline subjects.
type NATSStageReporter struct {
	bus    natsbus.Client
	logger *slog.Logger
	tracer trace.Tracer
}

// ReporterOption configures a NATSStageReporter.
type ReporterOption func(*NATSStageReporter)

// WithReporterTelemetry enables OTel tracing for the stage reporter.
func WithReporterTelemetry(tp trace.TracerProvider, _ metric.MeterProvider) ReporterOption {
	return func(r *NATSStageReporter) {
		if tp != nil {
			r.tracer = tp.Tracer("crosscodex/internal/analysis")
		}
	}
}

// NewNATSStageReporter creates a StageReporter that publishes to NATS.
func NewNATSStageReporter(bus natsbus.Client, opts ...ReporterOption) *NATSStageReporter {
	r := &NATSStageReporter{bus: bus, logger: slog.Default()}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

func (r *NATSStageReporter) ReportStageStarted(ctx context.Context, analyzerName, jobID string) error {
	ctx, span := telemetry.StartSpan(r.tracer, ctx, "analysis.ReportStageStarted")
	defer span.End()
	span.SetAttributes(attribute.String("analyzer.name", analyzerName), attribute.String("job.id", jobID))
	err := r.publish(ctx, analyzerName, jobID, natsbus.StageStarted, nil, nil)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	} else {
		span.SetStatus(codes.Ok, "")
	}
	return err
}

func (r *NATSStageReporter) ReportStageCompleted(ctx context.Context, analyzerName, jobID string, output *analyzer.Output) error {
	ctx, span := telemetry.StartSpan(r.tracer, ctx, "analysis.ReportStageCompleted")
	defer span.End()
	span.SetAttributes(attribute.String("analyzer.name", analyzerName), attribute.String("job.id", jobID))
	err := r.publish(ctx, analyzerName, jobID, natsbus.StageCompleted, output, nil)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	} else {
		span.SetStatus(codes.Ok, "")
	}
	return err
}

func (r *NATSStageReporter) ReportStageFailed(ctx context.Context, analyzerName, jobID string, err error) error {
	ctx, span := telemetry.StartSpan(r.tracer, ctx, "analysis.ReportStageFailed")
	defer span.End()
	span.SetAttributes(attribute.String("analyzer.name", analyzerName), attribute.String("job.id", jobID))
	pubErr := r.publish(ctx, analyzerName, jobID, natsbus.StageFailed, nil, err)
	if pubErr != nil {
		span.RecordError(pubErr)
		span.SetStatus(codes.Error, pubErr.Error())
	} else {
		span.SetStatus(codes.Ok, "")
	}
	return pubErr
}

func (r *NATSStageReporter) publish(ctx context.Context, analyzerName, jobID string, stage natsbus.Stage, output *analyzer.Output, stageErr error) error {
	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		return fmt.Errorf("stage report: %w", err)
	}

	subject, err := natsbus.PipelineStageSubject(tenantID, jobID, stage)
	if err != nil {
		return fmt.Errorf("stage report: building subject: %w", err)
	}

	event := map[string]interface{}{
		"analyzer":  analyzerName,
		"job_id":    jobID,
		"stage":     string(stage),
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	}
	if output != nil {
		event["metadata"] = output.Metadata
	}
	if stageErr != nil {
		// Log the raw error server-side before sanitizing.
		r.logger.ErrorContext(ctx, "stage failed",
			"analyzer", analyzerName,
			"stage", string(stage),
			"raw_error", stageErr.Error())
		event["error"] = sanitizeError(stageErr.Error()).Error()
	}

	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("stage report: marshaling event: %w", err)
	}

	if err := r.bus.Publish(ctx, subject, data); err != nil {
		r.logger.WarnContext(ctx, "failed to publish stage event",
			"analyzer", analyzerName, "stage", string(stage), "error", err)
		return nil
	}
	return nil
}

var _ StageReporter = (*NATSStageReporter)(nil)
var _ StageReporter = noopReporter{}
