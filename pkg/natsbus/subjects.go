package natsbus

import (
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// subjectPrefix is the root prefix for all CrossCodex NATS subjects.
const subjectPrefix = "crosscodex"

// WorkerProgressSubject is the one subject the worker progress signal uses:
// LLM workers publish a tick on it after every task result they publish, and
// analysis collections listen to it to tell a busy worker pool from a
// stalled one (see Client.PublishWorkerProgress).
//
//	crosscodex.workers.progress
//
// It is tenant-free by design, the single exception to tenant-scoped subjects
// and to provenance enforcement. Workers are shared by every tenant, every
// job and every daemon attached to the server, so a collection waiting on its
// own tenant's results must still see that the pool is busy with another
// tenant's tasks. A tick has an empty payload and no headers: it must never
// carry a tenant ID, trace context, task identity or any other data, and the
// subscriber's handler receives none. This is safe because a tick can only
// keep a collection waiting: a forged or legitimate tick delays a stage's
// idle timeout (analysis.engine.task_timeout) at most until the stage's hard
// cap (pipeline.stage_timeout), and never delivers or alters a result.
const WorkerProgressSubject = subjectPrefix + ".workers.progress"

// MaxJobIDLength is the longest job ID, in bytes, that a subject may carry.
// crosscodexd generates job IDs as UUIDs (36 bytes), and the longest it
// derives is artifact-adjudication-<uuid> (58 bytes); the cap leaves room for
// those while bounding what every subscriber that remembers a job ID (such
// as the worker's abandoned-collection set) must hold per entry, instead of
// NATS max_control_line (4 KiB by default) bounding it.
const MaxJobIDLength = 128

// buildSubject validates the tenant ID, then returns tokenErr (the result of
// validating the subject's one caller-supplied token) if it is non-nil, and
// otherwise joins all parts with dots under the subjectPrefix.
// parts should include the category, tenantID, and any trailing segments.
func buildSubject(tenantID string, tokenErr error, parts ...string) (string, error) {
	if err := validateTenant(tenantID); err != nil {
		return "", err
	}
	if tokenErr != nil {
		return "", tokenErr
	}
	return subjectPrefix + "." + strings.Join(parts, "."), nil
}

// PipelineStageSubject builds a pipeline stage event subject.
//
//	crosscodex.pipeline.{tenant_id}.{job_id}.stage.{started|completed|failed}
func PipelineStageSubject(tenantID, jobID string, stage Stage) (string, error) {
	return buildSubject(tenantID, validateJobID(jobID),
		"pipeline", tenantID, jobID, "stage", string(stage))
}

// PipelineStateSubject builds a pipeline state subject.
//
//	crosscodex.pipeline.{tenant_id}.{job_id}.state
func PipelineStateSubject(tenantID, jobID string) (string, error) {
	return buildSubject(tenantID, validateJobID(jobID),
		"pipeline", tenantID, jobID, "state")
}

// WorkSubject builds a work distribution subject.
//
//	crosscodex.work.{tenant_id}.{task_type}.{job_id}
func WorkSubject(tenantID string, taskType TaskType, jobID string) (string, error) {
	return buildSubject(tenantID, validateJobID(jobID),
		"work", tenantID, string(taskType), jobID)
}

// ResultSubject builds a result subject.
//
//	crosscodex.results.{tenant_id}.{task_type}.{job_id}
func ResultSubject(tenantID string, taskType TaskType, jobID string) (string, error) {
	return buildSubject(tenantID, validateJobID(jobID),
		"results", tenantID, string(taskType), jobID)
}

// AuditSubject builds an audit record subject.
//
//	crosscodex.audit.{tenant_id}.{audit_type}.{job_id}
func AuditSubject(tenantID string, auditType AuditType, jobID string) (string, error) {
	return buildSubject(tenantID, validateJobID(jobID),
		"audit", tenantID, string(auditType), jobID)
}

// FeedbackSubject builds a feedback subject.
//
//	crosscodex.feedback.{tenant_id}.{edge_id}
func FeedbackSubject(tenantID, edgeID string) (string, error) {
	return buildSubject(tenantID, validateToken(edgeID, "edge ID"),
		"feedback", tenantID, edgeID)
}

// CatalogParsedSubject builds the subject catalog ingest publishes after it
// has persisted a catalog's controls; the graph role projects them into
// Control nodes.
//
//	crosscodex.catalog.{tenant_id}.{catalog_id}.parsed
func CatalogParsedSubject(tenantID, catalogID string) (string, error) {
	return buildSubject(tenantID, validateToken(catalogID, "catalog ID"),
		"catalog", tenantID, catalogID, "parsed")
}

// AbandonedSubjectFilter matches every abandon control subject, across all
// tenants. Every LLM worker subscribes to it with a plain subscription, not
// the work queue group, so each worker learns of every abandon.
const AbandonedSubjectFilter = subjectPrefix + ".control.*.*.*.*.abandoned"

// abandonedSubjectFormat is the schema AbandonedSubject builds and
// ParseAbandonedSubject accepts, quoted in parse errors.
const abandonedSubjectFormat = subjectPrefix + ".control.<tenant_id>.<task_type>.<job_id>.<collection_id>.abandoned"

// AbandonedCollection names one analysis collection that ended without all of
// its results, so the LLM tasks it dispatched are no longer wanted.
//
// CollectionID is the token the analysis collector generates for each
// collection and sends with every task it dispatches or redispatches (the
// X-Collection-Id work header). It, not the job, is what makes an abandon
// safe: a resumed or retried job reuses its job ID, but each run's
// collections get new IDs, so an old abandon never matches the new run's
// tasks.
type AbandonedCollection struct {
	TenantID     string
	TaskType     TaskType
	JobID        string
	CollectionID string
}

// AbandonedSubject builds the control subject announcing that collection c
// was abandoned. The message carries no payload: the subject is the whole key.
//
//	crosscodex.control.{tenant_id}.{task_type}.{job_id}.{collection_id}.abandoned
//
// It is stricter than the work and result builders, because workers parse
// this subject from untrusted messages and remember what it names: the task
// type must be a TaskType constant and the collection ID a canonical
// lower-case UUID (what uuid.NewString produces), so each collection has
// exactly one subject and a remembered key has a bounded size. The job ID
// gets the same check as in every other subject, including MaxJobIDLength.
func AbandonedSubject(c AbandonedCollection) (string, error) {
	if err := validateToken(string(c.TaskType), "task type"); err != nil {
		return "", err
	}
	if !slices.Contains(taskTypes, c.TaskType) {
		names := make([]string, len(taskTypes))
		for i, t := range taskTypes {
			names[i] = string(t)
		}
		return "", fmt.Errorf("task type %q is not one of %s: %w", c.TaskType, strings.Join(names, ", "), ErrInvalidSubject)
	}
	if err := validateToken(c.CollectionID, "collection ID"); err != nil {
		return "", err
	}
	if u, err := uuid.Parse(c.CollectionID); err != nil || u.String() != c.CollectionID {
		return "", fmt.Errorf("collection ID %q is not a lower-case RFC 4122 UUID such as uuid.NewString returns: %w",
			c.CollectionID, ErrInvalidSubject)
	}
	return buildSubject(c.TenantID, validateJobID(c.JobID),
		"control", c.TenantID, string(c.TaskType), c.JobID, c.CollectionID, "abandoned")
}

// ParseAbandonedSubject is the inverse of AbandonedSubject. Workers call it
// on every control message they receive, so it validates the subject as
// untrusted input: it accepts exactly the subjects AbandonedSubject can
// build and returns an ErrInvalidSubject error for anything else.
func ParseAbandonedSubject(subject string) (AbandonedCollection, error) {
	parts := strings.Split(subject, ".")
	if len(parts) != 7 || parts[0] != subjectPrefix || parts[1] != "control" || parts[6] != "abandoned" {
		return AbandonedCollection{}, fmt.Errorf("subject %q is not an abandon control subject %s: %w",
			subject, abandonedSubjectFormat, ErrInvalidSubject)
	}
	// Clone each field: a worker keeps the result for hours, and a
	// substring would pin the whole subject's backing array.
	c := AbandonedCollection{
		TenantID:     strings.Clone(parts[2]),
		TaskType:     TaskType(strings.Clone(parts[3])),
		JobID:        strings.Clone(parts[4]),
		CollectionID: strings.Clone(parts[5]),
	}
	if _, err := AbandonedSubject(c); err != nil {
		return AbandonedCollection{}, fmt.Errorf("abandon control subject %q: %w", subject, err)
	}
	return c, nil
}

// validateTenant validates a tenant ID using pkg/tenant and wraps
// the error as ErrInvalidSubject.
func validateTenant(tenantID string) error {
	if err := tenant.ValidateTenantID(tenantID); err != nil {
		return fmt.Errorf("tenant validation: %w: %w", ErrInvalidSubject, err)
	}
	return nil
}

// validateJobID is the one job ID check every subject builder that takes a
// job ID applies: a valid subject token of at most MaxJobIDLength bytes. Its
// errors wrap ErrInvalidJobID and ErrInvalidSubject.
func validateJobID(jobID string) error {
	if err := validateToken(jobID, "job ID"); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidJobID, err)
	}
	if len(jobID) > MaxJobIDLength {
		// The ID is not quoted: it can be up to the NATS control line long.
		return fmt.Errorf("job ID is %d bytes, longer than the %d-byte limit; "+
			"use the job ID crosscodexd assigned (a UUID) instead: %w: %w",
			len(jobID), MaxJobIDLength, ErrInvalidJobID, ErrInvalidSubject)
	}
	return nil
}

// validateToken checks that a NATS subject token (job ID, edge ID) is
// non-empty and contains no NATS subject delimiters (., *, >).
func validateToken(token, label string) error {
	if token == "" {
		return fmt.Errorf("%s must not be empty: %w", label, ErrInvalidSubject)
	}
	if strings.ContainsAny(token, ".*>") {
		return fmt.Errorf("%s %q contains NATS subject delimiter: %w", label, token, ErrInvalidSubject)
	}
	return nil
}
