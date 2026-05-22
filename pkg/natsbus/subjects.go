package natsbus

import (
	"fmt"
	"strings"

	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// subjectPrefix is the root prefix for all CrossCodex NATS subjects.
const subjectPrefix = "crosscodex"

// buildSubject validates the tenant ID and a single additional token,
// then joins all parts with dots under the subjectPrefix.
// parts should include the category, tenantID, and any trailing segments.
func buildSubject(tenantID string, tokenValue, tokenLabel string, parts ...string) (string, error) {
	if err := validateTenant(tenantID); err != nil {
		return "", err
	}
	if err := validateToken(tokenValue, tokenLabel); err != nil {
		return "", err
	}
	return subjectPrefix + "." + strings.Join(parts, "."), nil
}

// PipelineStageSubject builds a pipeline stage event subject.
//
//	crosscodex.pipeline.{tenant_id}.{job_id}.stage.{started|completed|failed}
func PipelineStageSubject(tenantID, jobID string, stage Stage) (string, error) {
	return buildSubject(tenantID, jobID, "job ID",
		"pipeline", tenantID, jobID, "stage", string(stage))
}

// PipelineStateSubject builds a pipeline state subject.
//
//	crosscodex.pipeline.{tenant_id}.{job_id}.state
func PipelineStateSubject(tenantID, jobID string) (string, error) {
	return buildSubject(tenantID, jobID, "job ID",
		"pipeline", tenantID, jobID, "state")
}

// WorkSubject builds a work distribution subject.
//
//	crosscodex.work.{tenant_id}.{task_type}.{job_id}
func WorkSubject(tenantID string, taskType TaskType, jobID string) (string, error) {
	return buildSubject(tenantID, jobID, "job ID",
		"work", tenantID, string(taskType), jobID)
}

// ResultSubject builds a result subject.
//
//	crosscodex.results.{tenant_id}.{task_type}.{job_id}
func ResultSubject(tenantID string, taskType TaskType, jobID string) (string, error) {
	return buildSubject(tenantID, jobID, "job ID",
		"results", tenantID, string(taskType), jobID)
}

// AuditSubject builds an audit record subject.
//
//	crosscodex.audit.{tenant_id}.{audit_type}.{job_id}
func AuditSubject(tenantID string, auditType AuditType, jobID string) (string, error) {
	return buildSubject(tenantID, jobID, "job ID",
		"audit", tenantID, string(auditType), jobID)
}

// FeedbackSubject builds a feedback subject.
//
//	crosscodex.feedback.{tenant_id}.{edge_id}
func FeedbackSubject(tenantID, edgeID string) (string, error) {
	return buildSubject(tenantID, edgeID, "edge ID",
		"feedback", tenantID, edgeID)
}

// validateTenant validates a tenant ID using pkg/tenant and wraps
// the error as ErrInvalidSubject.
func validateTenant(tenantID string) error {
	if err := tenant.ValidateTenantID(tenantID); err != nil {
		return fmt.Errorf("tenant validation: %w: %w", ErrInvalidSubject, err)
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
