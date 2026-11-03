package config

import (
	"errors"
	"fmt"
	"reflect"
)

var (
	// ErrInvalidConfig indicates configuration validation failed.
	ErrInvalidConfig = errors.New("invalid configuration")

	// ErrMissingRequired indicates a required configuration value is missing.
	ErrMissingRequired = errors.New("missing required configuration")

	// ErrLoadFailed indicates configuration loading failed.
	ErrLoadFailed = errors.New("failed to load configuration")

	// ErrProfileNotFound indicates the requested profile does not exist.
	ErrProfileNotFound = errors.New("profile not found")

	// ErrUnknownKey indicates a configuration file or CLI flag override sets a
	// key that is not a configuration field. Load wraps it together with
	// ErrInvalidConfig.
	ErrUnknownKey = errors.New("unknown configuration key")

	// ErrWrongType indicates a configuration value of the wrong type for its
	// setting, such as text where a number or a section belongs. It wraps
	// ErrInvalidConfig.
	ErrWrongType = fmt.Errorf("%w: value has the wrong type", ErrInvalidConfig)

	// ErrInvalidPath indicates a config path (such as a `crosscodex config
	// set` key) is malformed; see SplitPath.
	ErrInvalidPath = errors.New("invalid config path")

	// ErrMergeConflict indicates a type mismatch during deep merge.
	ErrMergeConflict = errors.New("merge conflict: incompatible types")
)

// FieldError is a configuration error about one setting: Path is its dotted
// config path (such as "logging.level" or "llm.tenant_overrides.acme"),
// the key a caller can compare across loads to tell which problems an edit
// fixed or introduced. Every validation and unknown-key error Load returns
// is one; an error about two settings is reported under the one an operator
// would change (its primary path), so it stays one problem until fixed.
//
// Error is Err's message, so wrapping an error in a FieldError does not
// change what an operator reads.
//
// Source names the layer that set the value at fault: a file path, "CLI
// flag <path>", "environment variable <name>", SourceDefaults when no layer
// set it (a required value is missing), or "" when unknown (as when
// validation runs without a load, or several layers set a section).
type FieldError struct {
	Path   string
	Source string
	Err    error
}

func (e *FieldError) Error() string { return e.Err.Error() }

func (e *FieldError) Unwrap() error { return e.Err }

// fieldErrorf returns a FieldError for path with a formatted error, which
// should wrap ErrInvalidConfig (or ErrMissingRequired) with %w.
func fieldErrorf(path, format string, args ...any) error {
	return &FieldError{Path: path, Err: fmt.Errorf(format, args...)}
}

// FieldErrors returns every FieldError in err's tree (through Unwrap() error
// and Unwrap() []error), depth first, without looking inside a FieldError.
// A path appears once per problem, so the result is a multiset.
func FieldErrors(err error) []*FieldError {
	var out []*FieldError
	var walk func(error)
	walk = func(err error) {
		switch u := err.(type) {
		case *FieldError:
			out = append(out, u)
		case interface{ Unwrap() []error }:
			for _, child := range u.Unwrap() {
				walk(child)
			}
		case interface{ Unwrap() error }:
			walk(u.Unwrap())
		}
	}
	walk(err)
	return out
}

// FieldErrorsComplete reports whether err is made of FieldErrors alone:
// every branch of its tree ends in one, apart from this package's sentinels
// that a message wraps next to them (ErrLoadFailed in fmt.Errorf("%w: %w")).
// A sentinel that is itself a member of an errors.Join is a problem without
// a path, so it makes the result false. When it is false, FieldErrors does
// not describe err fully.
func FieldErrorsComplete(err error) bool {
	switch u := err.(type) {
	case nil:
		return false
	case *FieldError:
		return true
	case interface{ Unwrap() []error }:
		isJoin := reflect.TypeOf(err) == joinErrorType
		children := 0
		for _, child := range u.Unwrap() {
			if !isJoin && isSentinel(child) {
				continue
			}
			if !FieldErrorsComplete(child) {
				return false
			}
			children++
		}
		return children > 0
	case interface{ Unwrap() error }:
		return FieldErrorsComplete(u.Unwrap())
	default:
		return false
	}
}

// joinErrorType is the type errors.Join returns, which is unexported.
var joinErrorType = reflect.TypeOf(errors.Join(ErrInvalidConfig))

func isSentinel(err error) bool {
	switch err {
	case ErrInvalidConfig, ErrMissingRequired, ErrLoadFailed, ErrUnknownKey:
		return true
	default:
		return false
	}
}
