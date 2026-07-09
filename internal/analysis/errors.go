package analysis

import "errors"

var (
	ErrNoTenant         = errors.New("analysis: tenant context required")
	ErrEmptyJobID       = errors.New("analysis: job ID must not be empty")
	ErrNilInput         = errors.New("analysis: input must not be nil")
	ErrUnknownTaskType  = errors.New("analysis: no task type mapping for analyzer")
	ErrTaskTimeout      = errors.New("analysis: task collection timed out")
	ErrRetryExhausted   = errors.New("analysis: task retries exhausted")
	ErrAnalyzerFailed   = errors.New("analysis: analyzer execution failed")
	ErrDependencyFailed = errors.New("analysis: dependency analyzer failed")
)
