package pipeline

import (
	"context"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
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

// ReportStageCompleted persists the analyzer's result and marks the stage
// completed, then publishes to NATS.
//
// Persistence is unconditional for every completed analyzer regardless of
// result size, including zero-task analyzers (a JSON null literal is
// substituted for an empty result). Unlike its siblings, any failure to
// persist is NOT swallowed: durable results are the point of the job, so the
// error propagates to the caller (failing the analysis stage) instead of
// being logged and ignored. An analyzer must never be marked completed
// without its result already durably committed, or resume logic would skip
// it forever.
func (r *DBStageReporter) ReportStageCompleted(ctx context.Context, analyzerName, jobID string, output *analyzer.Output) error {
	ctx, span := telemetry.StartSpan(r.tracer, ctx, "pipeline.ReportStageCompleted")
	defer span.End()
	span.SetAttributes(
		attribute.String("analyzer.name", analyzerName),
		attribute.String("job.id", jobID),
	)

	// analysis_results.result_data is JSONB NOT NULL, and completion must
	// always be durably persisted (see doc comment above) -- including for a
	// zero-task analyzer, whose ResultData is nil. Substitute the JSON null
	// literal in that case: it is the same on-disk representation
	// classify/artifacts/relationship/requires already produce when
	// json.Marshal encodes a nil result slice, so this introduces no new
	// convention. Always persisting (never falling back to a bare
	// UpdateStageStatus) is what lets GetCompletedAnalysisResults find every
	// completed analyzer on resume, regardless of whether it produced output.
	resultData := output.ResultData
	if len(resultData) == 0 {
		resultData = []byte("null")
	}
	if err := r.store.CompleteAnalysisStage(ctx, jobID, analyzerName, resultData); err != nil {
		r.logger.ErrorContext(ctx, "failed to persist analysis result",
			"analyzer", analyzerName, "job_id", jobID, "error", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("pipeline.ReportStageCompleted: persisting result: %w", err)
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
