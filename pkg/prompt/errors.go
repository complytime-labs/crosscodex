package prompt

import "errors"

var (
	// ErrPromptNotFound is returned when no layer provides a named prompt.
	ErrPromptNotFound = errors.New("prompt not found")

	// ErrMissingPlaceholder is returned when a template contains a ${placeholder}
	// that has no corresponding entry in the vars map.
	ErrMissingPlaceholder = errors.New("missing placeholder value")

	// ErrInvalidPromptSpec is returned when a YAML file cannot be parsed
	// into a valid PromptSpec or fails validation (e.g., missing name).
	ErrInvalidPromptSpec = errors.New("invalid prompt specification")

	// ErrCommandDisabled is returned when a few-shot source uses cmd: protocol
	// but allow_commands is false in config.
	ErrCommandDisabled = errors.New("command execution disabled")

	// ErrLayerConflict is returned when prompts with different names share
	// the same layer slot during resolution.
	ErrLayerConflict = errors.New("prompt name mismatch across layers")
)
