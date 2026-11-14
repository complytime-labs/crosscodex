package analysis

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/complytime-labs/crosscodex/pkg/telemetry"
)

var (
	ErrNoTenant        = errors.New("analysis: tenant context required")
	ErrEmptyJobID      = errors.New("analysis: job ID must not be empty")
	ErrNilInput        = errors.New("analysis: input must not be nil")
	ErrUnknownTaskType = errors.New("analysis: no task type mapping for analyzer")
	ErrTaskTimeout     = errors.New("analysis: task collection timed out")
	ErrStageTimeout    = errors.New("analysis: stage exceeded pipeline.stage_timeout")
	ErrNoStageTimeout  = errors.New("analysis: collection has no stage timeout")
	// ErrNoSuccessfulResult reports a stage whose tasks kept failing with
	// no successful result for analysis.engine.success_timeout: the LLM
	// gateway or model server is not answering. See NoSuccessError.
	ErrNoSuccessfulResult = errors.New("analysis: no successful task result within analysis.engine.success_timeout")
	// ErrNoSuccessTimeout refuses a collection without a positive
	// SuccessTimeout, which would let a stage whose every call fails run
	// until pipeline.stage_timeout.
	ErrNoSuccessTimeout = errors.New("analysis: collection has no success timeout")
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

// NoSuccessError is the collection failure for ErrNoSuccessfulResult: the
// stage's tasks kept failing, and none succeeded, for Window. It unwraps to
// ErrNoSuccessfulResult. Its fields let the engine write the tenant's
// message; LastError is the sanitized error category of the last failed
// result (see sanitizeError), never a worker's raw error text.
type NoSuccessError struct {
	JobID string
	// Window is the collection's SuccessTimeout.
	Window time.Duration
	// Failures counts the failed task results, retried ones included,
	// since LastSuccess.
	Failures int
	// LastSuccess is when the stage's last successful result arrived; zero
	// when none did since the collection started.
	LastSuccess time.Time
	LastError   error
}

// Error returns the operator-facing description.
func (e *NoSuccessError) Error() string {
	return fmt.Sprintf("await results for job %s: no successful task result for %s, %d failed task results since %s, last error %v; "+
		"check the LLM gateway and model server, or raise analysis.engine.success_timeout if one answer legitimately takes longer: %v",
		e.JobID, e.Window, e.Failures, e.since(), e.LastError, ErrNoSuccessfulResult)
}

// Unwrap returns ErrNoSuccessfulResult.
func (e *NoSuccessError) Unwrap() error { return ErrNoSuccessfulResult }

// since names the start of the failing run: the last success, in UTC, or
// the stage start.
func (e *NoSuccessError) since() string {
	if e.LastSuccess.IsZero() {
		return "the stage started"
	}
	return "the last success at " + e.LastSuccess.UTC().Format(time.RFC3339)
}

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
