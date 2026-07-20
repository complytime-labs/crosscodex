package graph

import "errors"

var (
	// ErrNotStarted is returned when Stop is called before Start.
	ErrNotStarted = errors.New("graph subscriber not started")

	// ErrAlreadyStarted is returned when Start is called twice.
	ErrAlreadyStarted = errors.New("graph subscriber already started")

	// ErrResolverNotFound is returned when no resolver is registered for a URI scheme.
	ErrResolverNotFound = errors.New("no resolver registered for scheme")
)
