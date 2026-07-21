package pipeline

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/internal/analysis"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
)

// DBStageReporter wraps a NATS stage reporter to also persist stage
// transitions to the database. DB write failures are logged but do not
// block NATS publishing. NATS errors are returned to the caller.
type DBStageReporter struct {
	nats   analysis.StageReporter
	store  Store
	logger *slog.Logger
	tracer trace.Tracer
}

// NewDBStageReporter creates a DBStageReporter that writes to both DB and NATS.
func NewDBStageReporter(natsReporter analysis.StageReporter, store Store, opts ...DBStageReporterOption) *DBStageReporter {
	r := &DBStageReporter{
		nats:   natsReporter,
		store:  store,
		logger: slog.Default(),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// DBStageReporterOption configures a DBStageReporter.
type DBStageReporterOption func(*DBStageReporter)

// WithReporterLogger sets the logger for the DBStageReporter.
func WithReporterLogger(logger *slog.Logger) DBStageReporterOption {
	return func(r *DBStageReporter) {
		if logger != nil {
			r.logger = logger
		}
	}
}

// WithReporterTracer enables tracing for the DBStageReporter.
func WithReporterTracer(tp trace.TracerProvider) DBStageReporterOption {
	return func(r *DBStageReporter) {
		if tp != nil {
			r.tracer = tp.Tracer("crosscodex/internal/pipeline")
		}
	}
}

// ReportStageStarted updates DB to running, then publishes to NATS.
func (r *DBStageReporter) ReportStageStarted(ctx context.Context, analyzerName, jobID string) error {
	ctx, span := telemetry.StartSpan(r.tracer, ctx, "pipeline.ReportStageStarted")
	defer span.End()
	span.SetAttributes(
		attribute.String("analyzer.name", analyzerName),
		attribute.String("job.id", jobID),
	)

	if err := r.store.UpdateStageStatus(ctx, jobID, analyzerName, StageStatusRunning); err != nil {
		r.logger.WarnContext(ctx, "failed to update stage status in DB",
			"analyzer", analyzerName, "job_id", jobID, "error", err)
	}

	return r.nats.ReportStageStarted(ctx, analyzerName, jobID)
}

// ReportStageCompleted updates DB to completed, then publishes to NATS.
func (r *DBStageReporter) ReportStageCompleted(ctx context.Context, analyzerName, jobID string, output *analyzer.Output) error {
	ctx, span := telemetry.StartSpan(r.tracer, ctx, "pipeline.ReportStageCompleted")
	defer span.End()
	span.SetAttributes(
		attribute.String("analyzer.name", analyzerName),
		attribute.String("job.id", jobID),
	)

	if err := r.store.UpdateStageStatus(ctx, jobID, analyzerName, StageStatusCompleted); err != nil {
		r.logger.WarnContext(ctx, "failed to update stage status in DB",
			"analyzer", analyzerName, "job_id", jobID, "error", err)
	}

	return r.nats.ReportStageCompleted(ctx, analyzerName, jobID, output)
}

// ReportStageFailed updates DB with error, then publishes to NATS.
func (r *DBStageReporter) ReportStageFailed(ctx context.Context, analyzerName, jobID string, stageErr error) error {
	ctx, span := telemetry.StartSpan(r.tracer, ctx, "pipeline.ReportStageFailed")
	defer span.End()
	span.SetAttributes(
		attribute.String("analyzer.name", analyzerName),
		attribute.String("job.id", jobID),
	)

	if updateErr := r.store.UpdateStageError(ctx, jobID, analyzerName, stageErr); updateErr != nil {
		r.logger.WarnContext(ctx, "failed to update stage error in DB",
			"analyzer", analyzerName, "job_id", jobID, "error", updateErr)
	}

	return r.nats.ReportStageFailed(ctx, analyzerName, jobID, stageErr)
}

// Compile-time check that DBStageReporter implements StageReporter.
var _ analysis.StageReporter = (*DBStageReporter)(nil)
