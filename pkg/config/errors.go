package config

import "errors"

var (
	// ErrInvalidConfig indicates configuration validation failed.
	ErrInvalidConfig = errors.New("invalid configuration")

	// ErrMissingRequired indicates a required configuration value is missing.
	ErrMissingRequired = errors.New("missing required configuration")

	// ErrLoadFailed indicates configuration loading failed.
	ErrLoadFailed = errors.New("failed to load configuration")
)
