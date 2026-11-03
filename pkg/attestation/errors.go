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

	// ErrChainBroken indicates hash chain verification failed between pipeline
	// steps, or a link does not record an artifact its layout step expects.
	ErrChainBroken = errors.New("attestation hash chain broken")

	// ErrMissingLink indicates a layout step has no link.
	ErrMissingLink = errors.New("attestation link missing")

	// ErrUnexpectedLink indicates a link for a step the layout does not
	// declare, or a second link for the same step.
	ErrUnexpectedLink = errors.New("unexpected attestation link")

	// ErrSubjectMismatch indicates a chain that is not bound to the run the
	// caller expects: a layout step or link that does not name it, or names
	// it with another digest.
	ErrSubjectMismatch = errors.New("attestation is for another run")

	// ErrKeyMismatch indicates a public key that is not the private key's.
	ErrKeyMismatch = errors.New("attestation key pair mismatch")

	// ErrInvalidBundle indicates data that is not an attestation bundle.
	ErrInvalidBundle = errors.New("invalid attestation bundle")
)
