package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/complytime-labs/crosscodex/internal/analysis"
	"github.com/complytime-labs/crosscodex/pkg/jobconfig"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
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
// not the ones the daemon now runs, so the job cannot resume. Its errors
// are tenantErrors: their text reaches the tenant.
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

// ErrLLMUnavailable reports that the per-job model probe (#243) did not
// complete, so the job stopped before any analysis LLM call. The gateway
// may be down or may have refused the probe; a retry may succeed once it
// works.
var ErrLLMUnavailable = errors.New("the model gateway did not complete a probe request before analysis")

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
	// JobErrorLLMUnavailable is ErrLLMUnavailable: the model probe failed
	// before any analysis LLM call. Retryable.
	JobErrorLLMUnavailable JobErrorClass = "llm_unavailable"
	// JobErrorLLMOutputTruncated is llmclient.ErrOutputTruncated: a stage's
	// model gave no answer within an analyzer's max_tokens. Retryable: once
	// the model's reasoning is off in the gateway, or max_tokens is raised
	// and crosscodexd restarted, the same job succeeds.
	JobErrorLLMOutputTruncated JobErrorClass = "llm_output_truncated"
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
	case errors.Is(err, ErrLLMUnavailable):
		return JobErrorLLMUnavailable
	case errors.Is(err, llmclient.ErrOutputTruncated):
		return JobErrorLLMOutputTruncated
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

// jobFailure is a failure as it is recorded where the tenant reads it: a
// job's or a stage's error_message, which GetJob and ListJobs return. Error
// returns the message written for the tenant; Unwrap returns the error the
// job or stage failed with, so ClassifyJobError classifies by its sentinel,
// not by the message.
type jobFailure struct {
	message string
	err     error
}

func (f *jobFailure) Error() string { return f.message }

func (f *jobFailure) Unwrap() error { return f.err }

// tenantFailure returns err, the failure of job jobID or of one of its
// stages, as it is recorded for the tenant: its text is tenantJobMessage's.
// job is nil when it could not be loaded.
func tenantFailure(ctx context.Context, jobID string, job *Job, err error) *jobFailure {
	return &jobFailure{message: tenantJobMessage(ctx, jobID, job, err), err: err}
}

// tenantError is a job failure whose text is written for the tenant, as
// analysis.StageError's is for a stage: Error returns message only, and
// Unwrap returns sentinel only, so no other error's text can be in it.
// Wrapping a sentinel in fmt.Errorf instead makes its text internal.
type tenantError struct {
	sentinel error
	message  string
}

func (e *tenantError) Error() string { return e.message }

func (e *tenantError) Unwrap() error { return e.sentinel }

// tenantJobMessage returns the message recorded for job jobID's failure
// err, which GetJob shows to the tenant, so it never carries driver,
// storage or panic text (CWE-209); failJob logs err in full. It is the
// text of each analysis.StageError and tenantError in err, the only errors
// whose text is written for the tenant. Otherwise it is a fixed message
// naming err's class, the way to run the job again that its class allows,
// and the reference the operator finds err by. job is nil when it could
// not be loaded, and its commands then name placeholder catalogs.
func tenantJobMessage(ctx context.Context, jobID string, job *Job, err error) string {
	if msgs := tenantMessages(err); len(msgs) > 0 {
		return strings.Join(msgs, " ")
	}
	failed := Job{JobID: jobID, ErrorClass: ClassifyJobError(err)}
	if job != nil {
		failed.Config = job.Config
	}
	var head, remedy string
	if retryRefusal(failed.ErrorClass) == "" {
		class := string(failed.ErrorClass)
		if failed.ErrorClass == JobErrorRetryable {
			class = "retryable"
		}
		head = fmt.Sprintf("job %s failed because of an internal error (error class %s).", jobID, class)
		remedy = fmt.Sprintf("Resume it with \"crosscodex run retry --from-failure %s\", which keeps its completed stages, "+
			"or start a new job: %s.", jobID, newJobCommand(&failed))
	} else {
		head = fmt.Sprintf("job %s failed because of an internal error (error class %s), "+
			"which a retry from its failure cannot fix.", jobID, failed.ErrorClass)
		remedy = retryRemedy(&failed) + "."
	}
	return fmt.Sprintf("%s %s If it fails again, give your operator %s.", head, remedy, analysis.TraceReference(ctx, jobID))
}

// tenantMessages returns the text of every analysis.StageError and
// tenantError in err's tree, in order. errors.As would find only the
// first, and the analysis pass joins one per failed analyzer.
func tenantMessages(err error) []string {
	switch e := err.(type) {
	case *analysis.StageError:
		return []string{e.Message}
	case *tenantError:
		return []string{e.message}
	case interface{ Unwrap() error }:
		return tenantMessages(e.Unwrap())
	case interface{ Unwrap() []error }:
		var msgs []string
		for _, u := range e.Unwrap() {
			msgs = append(msgs, tenantMessages(u)...)
		}
		return msgs
	}
	return nil
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
// it, so the job fails before any analyzer spends LLM calls. Its errors are
// tenantErrors: their text reaches the tenant.
var ErrEmptyTargetCatalog = errors.New("target catalog has no control with a statement")
