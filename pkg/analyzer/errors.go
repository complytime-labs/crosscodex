package analyzer

import "errors"

var (
	// ErrNotFound indicates the analyzer does not exist.
	ErrNotFound = errors.New("analyzer not found")

	// ErrAlreadyRegistered indicates an analyzer with this name is already registered.
	ErrAlreadyRegistered = errors.New("analyzer already registered")

	// ErrAnalysisFailed indicates the analysis process failed.
	ErrAnalysisFailed = errors.New("analysis failed")

	// ErrInvalidArtifact indicates the artifact is malformed or unsupported.
	ErrInvalidArtifact = errors.New("invalid artifact")
)
