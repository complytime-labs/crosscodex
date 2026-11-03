package attestation

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

var (
	// ErrInvalidLayout indicates the layout is malformed.
	ErrInvalidLayout = errors.New("invalid attestation layout")

	// ErrSignatureFailed indicates signing failed.
	ErrSignatureFailed = errors.New("failed to sign attestation")

	// ErrVerificationFailed indicates signature verification failed.
	ErrVerificationFailed = errors.New("attestation verification failed")

	// ErrUntrustedKey indicates a signature that does not verify and that
	// no signature of the envelope names the trusted key's ID: the
	// envelope was signed by a key the verifier does not trust (rotated or
	// unknown), not tampered with under the trusted one. It always comes
	// wrapped together with ErrVerificationFailed.
	//
	// A DSSE signature's keyid is not covered by the signature, so whoever
	// can rewrite an envelope can make a bad signature under the trusted
	// key read as this error instead. That only turns "tampered" into
	// "untrusted": either way the envelope is not trusted.
	ErrUntrustedKey = errors.New("attestation signed by a key that is not trusted")

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

// UntrustedKeyError is the error Verify returns for an envelope none of
// whose signatures names the trusted key's ID (ErrUntrustedKey). KeyIDs are
// the key IDs the signatures do name, in envelope order, so an operator
// can compare them with retired keys. They are unsigned, like every DSSE
// keyid (see ErrUntrustedKey), so whoever wrote the envelope chose them:
// newUntrustedKeyError keeps at most maxUntrustedKeyIDs, each cut to
// maxKeyIDBytes, and counts the rest in Omitted, which bounds every log
// line and job error message that quotes them.
type UntrustedKeyError struct {
	KeyIDs []string
	// Omitted counts the signatures' key IDs not kept in KeyIDs.
	Omitted int
	// Err is the signature verification's own error.
	Err error
}

const (
	// maxUntrustedKeyIDs is how many key IDs an UntrustedKeyError keeps:
	// a genuine envelope has one signature.
	maxUntrustedKeyIDs = 4
	// maxKeyIDBytes bounds each kept key ID: a key ID this package makes
	// is 64 hex characters.
	maxKeyIDBytes = 128
)

// newUntrustedKeyError returns the UntrustedKeyError of an envelope whose
// signatures name keyIDs, capped as UntrustedKeyError describes. A key ID
// is cut at a rune boundary, so quoting it never shows a split rune.
func newUntrustedKeyError(keyIDs []string, err error) *UntrustedKeyError {
	e := &UntrustedKeyError{Err: err}
	for i, id := range keyIDs {
		if i == maxUntrustedKeyIDs {
			e.Omitted = len(keyIDs) - i
			break
		}
		if len(id) > maxKeyIDBytes {
			cut := maxKeyIDBytes
			for cut > 0 && !utf8.RuneStart(id[cut]) {
				cut--
			}
			id = id[:cut]
		}
		e.KeyIDs = append(e.KeyIDs, id)
	}
	return e
}

// Error quotes the key IDs.
func (e *UntrustedKeyError) Error() string {
	ids := "no key ID"
	if len(e.KeyIDs) > 0 {
		quoted := make([]string, len(e.KeyIDs))
		for i, id := range e.KeyIDs {
			quoted[i] = strconv.Quote(id)
		}
		ids = "key IDs " + strings.Join(quoted, ", ")
		if e.Omitted > 0 {
			ids += fmt.Sprintf(" and %d more", e.Omitted)
		}
	}
	return fmt.Sprintf("verify signature: %v: its signatures name %s, not the trusted key: %v: %v",
		ErrUntrustedKey, ids, ErrVerificationFailed, e.Err)
}

// Unwrap returns ErrUntrustedKey, ErrVerificationFailed and Err.
func (e *UntrustedKeyError) Unwrap() []error {
	return []error{ErrUntrustedKey, ErrVerificationFailed, e.Err}
}
