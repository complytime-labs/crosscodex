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

	// ErrKeyNotFound indicates the key provider could not locate the requested key.
	ErrKeyNotFound = errors.New("attestation key not found")

	// ErrKeyLoadFailed indicates key material exists but could not be parsed.
	ErrKeyLoadFailed = errors.New("attestation key load failed")

	// ErrNonFIPSAlgorithm indicates the signing key uses a non-FIPS-approved algorithm.
	ErrNonFIPSAlgorithm = errors.New("non-FIPS signing algorithm")

	// ErrChainBroken indicates hash chain verification failed between pipeline steps.
	ErrChainBroken = errors.New("attestation hash chain broken")
)
