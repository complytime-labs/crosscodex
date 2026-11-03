package pipeline

import (
	"errors"

	"github.com/complytime-labs/crosscodex/pkg/jobconfig"
	"github.com/complytime-labs/crosscodex/pkg/resultcache"
)

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
	// JobErrorEmptyTargetCatalog is ErrEmptyTargetCatalog. Catalog IDs are
	// content hashes, so a retry reads the same empty target.
	JobErrorEmptyTargetCatalog JobErrorClass = "empty_target_catalog"
	// JobErrorResultCacheUnavailable is resultcache.ErrUnavailable: the
	// cache could not be read, its audit rows written or its record
	// sealed. Retryable.
	JobErrorResultCacheUnavailable JobErrorClass = "result_cache_unavailable"
	// JobErrorResultCacheCorrupt is resultcache.ErrCorrupt: a cached result
	// or a stage's cache record failed its integrity check. A retry reads
	// the same cache, so both retry forms are refused, except a new job
	// from a --no-cache job, which looks nothing up; a new job with
	// --no-cache replaces the cached results.
	JobErrorResultCacheCorrupt JobErrorClass = "result_cache_corrupt"
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
	case errors.Is(err, ErrEmptyTargetCatalog):
		return JobErrorEmptyTargetCatalog
	// Corrupt before unavailable: a job whose analyzers failed with both
	// takes the class a retry cannot fix.
	case errors.Is(err, resultcache.ErrCorrupt):
		return JobErrorResultCacheCorrupt
	case errors.Is(err, resultcache.ErrUnavailable):
		return JobErrorResultCacheUnavailable
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
	case JobErrorEmptyTargetCatalog:
		return "its target catalog has no control with a statement, and a retry reads the same catalog"
	case JobErrorResultCacheCorrupt:
		return "a cached result failed its integrity check, or a stage run's cache record did not match what the stage wrote, " +
			"and a retry reads the same cache"
	default:
		return ""
	}
}

// retryRemedy is the sentence a refused retry of job ends with: the command
// that starts the new job it needs, built from job's config rather than read
// from its stored error message, which may predate the current CLI. A job
// whose target has nothing to map onto needs another target: re-importing
// the target with statements gives it a new catalog ID (IDs are content
// hashes).
func retryRemedy(job *Job) string {
	if cats, ok := jobconfig.ExtractCatalogs(job.Config); ok && job.ErrorClass == JobErrorEmptyTargetCatalog {
		return "Importing the target again with control statements gives it a new catalog ID, " +
			"so start a new job with that catalog or another target: " + jobCommand(cats, cats.SourceID, "<catalog-id>")
	}
	if job.ErrorClass == JobErrorResultCacheCorrupt {
		return "Start a new job with --no-cache, which recomputes and replaces the cached results: " + noCacheCommand(job)
	}
	return "Start a new job: " + newJobCommand(job)
}

// ErrCatalogNotFound reports that a catalog a job names is not a catalog of
// the job's tenant: it was deleted or purged after the job was created.
var ErrCatalogNotFound = errors.New("catalog not found for the tenant")

// ErrEmptyTargetCatalog reports a job mapping a source catalog onto a target
// catalog that has no control with a statement: nothing could be mapped onto
// it, so the job fails before any analyzer spends LLM calls.
var ErrEmptyTargetCatalog = errors.New("target catalog has no control with a statement")
