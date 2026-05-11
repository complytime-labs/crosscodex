package attestation

import "errors"

var (
	// ErrInvalidLayout indicates the layout is malformed.
	ErrInvalidLayout = errors.New("invalid attestation layout")

	// ErrSignatureFailed indicates signing failed.
	ErrSignatureFailed = errors.New("failed to sign attestation")

	// ErrVerificationFailed indicates signature verification failed.
	ErrVerificationFailed = errors.New("attestation verification failed")

	// ErrExpired indicates the attestation has expired.
	ErrExpired = errors.New("attestation expired")
)
