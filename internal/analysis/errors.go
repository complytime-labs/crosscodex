package analysis

import (
	"context"
	"errors"

	"github.com/complytime-labs/crosscodex/pkg/telemetry"
)

var (
	ErrNoTenant         = errors.New("analysis: tenant context required")
	ErrEmptyJobID       = errors.New("analysis: job ID must not be empty")
	ErrNilInput         = errors.New("analysis: input must not be nil")
	ErrUnknownTaskType  = errors.New("analysis: no task type mapping for analyzer")
	ErrTaskTimeout      = errors.New("analysis: task collection timed out")
	ErrStageTimeout     = errors.New("analysis: stage exceeded pipeline.stage_timeout")
	ErrNoStageTimeout   = errors.New("analysis: collection has no stage timeout")
	ErrRetryExhausted   = errors.New("analysis: task retries exhausted")
	ErrAnalyzerFailed   = errors.New("analysis: analyzer execution failed")
	ErrDependencyFailed = errors.New("analysis: dependency analyzer failed")
	// ErrResultCacheNotWired reports a request that uses the result cache
	// on an engine built without WithResultCache: a wiring bug, refused
	// before any work.
	ErrResultCacheNotWired = errors.New("analysis: result cache requested but no store is wired")
	// ErrInvalidCacheRequest reports CacheSettings the engine cannot honour.
	ErrInvalidCacheRequest = errors.New("analysis: invalid result cache settings")
)

// StageError is a stage failure whose text is fixed for the tenant: Error
// returns Message only, never Cause's text (CWE-209). It unwraps to
// Sentinel alone, the error class the pipeline classifies the job by, so a
// Cause that happens to wrap another class's sentinel cannot change the
// class. Whoever creates one logs Cause once, with the trace.
type StageError struct {
	Analyzer string
	Sentinel error
	Cause    error
	Message  string
}

// Error returns the tenant-facing message.
func (e *StageError) Error() string { return e.Message }

// Unwrap returns Sentinel.
func (e *StageError) Unwrap() error { return e.Sentinel }

// TraceReference is what a stage error asks the tenant to give the
// operator: "trace ID <id>", or, with tracing off, the job ID.
func TraceReference(ctx context.Context, jobID string) string {
	if id := telemetry.TraceIDFromContext(ctx); id != "" {
		return "trace ID " + id
	}
	return "job ID " + jobID + " (tracing is off, so there is no trace ID)"
}
