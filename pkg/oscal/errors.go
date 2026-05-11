package oscal

import "errors"

var (
	// ErrInvalidFormat indicates the OSCAL document format is invalid.
	ErrInvalidFormat = errors.New("invalid OSCAL format")

	// ErrValidationFailed indicates OSCAL schema validation failed.
	ErrValidationFailed = errors.New("OSCAL validation failed")

	// ErrControlNotFound indicates the specified control does not exist.
	ErrControlNotFound = errors.New("control not found")

	// ErrParseFailed indicates parsing the OSCAL document failed.
	ErrParseFailed = errors.New("failed to parse OSCAL catalog")
)
