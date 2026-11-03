package pipeline

import "errors"

var ErrNotFound = errors.New("pipeline: not found")

var ErrInvalidJobID = errors.New("pipeline: invalid job ID")

// ErrJobCancelled is the cancellation cause of a job's context when CancelJob
// cancels it. A job context cancelled for any other cause (errServiceStopping
// at shutdown) is an interruption, not a cancellation: the job stays running
// and resumes on the next start.
var ErrJobCancelled = errors.New("pipeline: job cancelled")

// errServiceStopping is the cancellation cause of every running job's context
// when Stop shuts the service down.
var errServiceStopping = errors.New("pipeline: service stopping")

// ErrAnalyzersChanged reports that the analyzers a job was created with are
// not the ones the daemon now runs, so the job cannot resume.
var ErrAnalyzersChanged = errors.New("the enabled analyzers changed since the job was created")

// ErrAttestationSigning reports that part of a job's attestation (its
// layout, a link, or the bundle) could not be signed or encoded. It persists
// until the key pair is fixed and crosscodexd restarted.
var ErrAttestationSigning = errors.New("attestation could not be signed")

// ErrAttestationStorage reports that object storage did not accept or return
// part of a job's attestation, after every retry for a write. It is
// transient: once storage works, the job can be retried from its failure.
var ErrAttestationStorage = errors.New("attestation could not be stored")

// ErrAttestationInvalid reports a job attestation that does not verify: a
// stored link is missing, modified or signed with another key, or the chain
// is broken. Retrying the job cannot fix it.
var ErrAttestationInvalid = errors.New("attestation does not verify")

// JobErrorClass classifies why a job failed, so RetryJob can refuse a retry
// that cannot succeed. It is stored with the job (jobs.error_class), because
// the stored error message is text and must not be matched.
type JobErrorClass string

const (
	// JobErrorRetryable is a failure a retry may fix (or no failure).
	JobErrorRetryable JobErrorClass = ""
	// JobErrorAttestationInvalid is ErrAttestationInvalid.
	JobErrorAttestationInvalid JobErrorClass = "attestation_invalid"
	// JobErrorAnalyzersChanged is ErrAnalyzersChanged.
	JobErrorAnalyzersChanged JobErrorClass = "analyzers_changed"
	// JobErrorMissingCatalog is ErrMissingCatalogID.
	JobErrorMissingCatalog JobErrorClass = "missing_catalog"
)

// ClassifyJobError returns the class of a job's failure err, by the
// sentinel it wraps.
func ClassifyJobError(err error) JobErrorClass {
	switch {
	case errors.Is(err, ErrAttestationInvalid):
		return JobErrorAttestationInvalid
	case errors.Is(err, ErrAnalyzersChanged):
		return JobErrorAnalyzersChanged
	case errors.Is(err, ErrMissingCatalogID):
		return JobErrorMissingCatalog
	default:
		return JobErrorRetryable
	}
}

// retryRefusal says why a job of class cannot be retried, or "" if it can.
func retryRefusal(class JobErrorClass) string {
	switch class {
	case JobErrorAttestationInvalid:
		return "its attestation does not verify, and a retry reads the same stored attestation"
	case JobErrorAnalyzersChanged:
		return "the enabled analyzers changed since it was created, and a retry checks them again"
	case JobErrorMissingCatalog:
		return "its config names no catalog to analyse"
	default:
		return ""
	}
}
