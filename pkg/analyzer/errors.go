package analyzer

import "errors"

var (
	// ErrNotFound indicates the requested analyzer does not exist in the registry.
	ErrNotFound = errors.New("analyzer not found")

	// ErrAlreadyRegistered indicates an analyzer with this name is already registered.
	ErrAlreadyRegistered = errors.New("analyzer already registered")

	// ErrCycleDetected indicates the analyzer dependency graph contains a cycle.
	ErrCycleDetected = errors.New("dependency cycle detected")

	// ErrMissingDependency indicates an analyzer depends on another that is not registered.
	ErrMissingDependency = errors.New("missing dependency")

	// ErrInvalidName indicates an analyzer name does not match the required format.
	ErrInvalidName = errors.New("invalid analyzer name")
)
