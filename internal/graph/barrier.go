package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// barrierSubject matches natsbus.MaterializeBarrierSubject for every tenant
// and job.
const barrierSubject = "crosscodex.materialize.*.*.barrier"

// barrierAnalyzer labels barrier handling in the shared event metric.
const barrierAnalyzer = "barrier"

// handleBarrier answers a job's materialization barrier, which the pipeline
// publishes after the last of the job's materialization events. MATERIALIZE
// is a work queue: a message leaves it only when acked or terminated, and an
// event that fails on its last delivery stores a "failed" outcome before it
// is terminated. So once no stage completion event of the job is left in the
// stream, every one was handled, and the AUDIT_EVENTS stream says whether any
// failed: a failed outcome counts only if the stream stored it at or after
// the barrier's NotBefore, since a retried or resumed job keeps its job ID and
// an earlier run's failures stay in AUDIT_EVENTS. Until the queue is empty the
// barrier returns natsbus.ErrNotReady and is asked again after
// nats.materialize.barrier_poll_interval; past its deadline it is dropped
// unanswered, since the pipeline has stopped waiting.
func (s *Service) handleBarrier(ctx context.Context, msg *natsbus.Message) error {
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "graph.barrier.handle")
	defer span.End()

	skip := func(reason string) error {
		s.recordEvent(ctx, barrierAnalyzer, "skipped")
		span.SetStatus(codes.Ok, "skipped")
		return fmt.Errorf("%w: barrier on %s: %s", natsbus.ErrSkip, msg.Subject, reason)
	}

	tenantID, jobID, err := parseBarrierSubject(msg.Subject)
	if err != nil {
		return skip(err.Error())
	}
	if err := verifySubjectTenant(ctx, tenantID); err != nil {
		s.logger.WarnContext(ctx, "rejecting barrier whose subject tenant is not its provenance tenant",
			"subject", msg.Subject, "error", err)
		s.recordEvent(ctx, barrierAnalyzer, "rejected")
		span.SetStatus(codes.Error, "tenant mismatch")
		return err
	}
	span.SetAttributes(attribute.String("tenant.id", tenantID), attribute.String("job.id", jobID))

	var barrier natsbus.MaterializeBarrier
	if err := json.Unmarshal(msg.Data, &barrier); err != nil {
		return skip("payload is not a barrier")
	}
	if barrier.JobID != jobID {
		return skip("payload job_id does not match the subject's job")
	}
	if barrier.Deadline.IsZero() {
		return skip("payload has no deadline")
	}
	if barrier.NotBefore.IsZero() {
		return skip("payload has no not_before")
	}
	if time.Now().After(barrier.Deadline) {
		s.logger.InfoContext(ctx, "dropping materialization barrier past its deadline",
			"job_id", jobID, "deadline", barrier.Deadline)
		s.recordEvent(ctx, barrierAnalyzer, "expired")
		span.SetStatus(codes.Ok, "expired")
		return fmt.Errorf("%w: barrier for job %s passed its deadline %s unanswered; the pipeline no longer waits for it",
			natsbus.ErrPermanent, jobID, barrier.Deadline.Format(time.RFC3339))
	}

	retry := func(err error) error {
		s.recordEvent(ctx, barrierAnalyzer, "error")
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("barrier for job %s: %w", jobID, err)
	}

	// The builders cannot fail: the subject's tenant and job were validated.
	stageSubject, err := natsbus.PipelineStageSubject(tenantID, jobID, natsbus.StageCompleted)
	if err != nil {
		return retry(err)
	}
	queued, err := s.bus.SubjectMessageCount(ctx, natsbus.MaterializeStream, stageSubject)
	if err != nil {
		return retry(fmt.Errorf("count queued materialization events: %w", err))
	}
	if queued > 0 {
		s.recordEvent(ctx, barrierAnalyzer, "waiting")
		span.SetStatus(codes.Ok, "waiting")
		return fmt.Errorf("%w: job %s still has %d materialization events queued in %s; asking again after the barrier poll interval",
			natsbus.ErrNotReady, jobID, queued, natsbus.MaterializeStream)
	}

	failedSubject, err := natsbus.GraphOutcomeSubject(tenantID, jobID, natsbus.GraphOutcomeFailed)
	if err != nil {
		return retry(err)
	}
	failedAt, found, err := s.bus.LastMessageTime(ctx, natsbus.AuditEventsStream, failedSubject)
	if err != nil {
		return retry(fmt.Errorf("read failed graph outcomes: %w", err))
	}
	outcome, reason := natsbus.GraphOutcomeMaterialized, ""
	if found && !failedAt.Before(barrier.NotBefore) {
		outcome = natsbus.GraphOutcomeFailed
		reason = "at least one materialization event of the job failed; see the earlier graph failure event for the analyzer"
	}
	if err := s.publishOutcome(ctx, tenantID, jobID, outcome, "", reason); err != nil {
		return retry(err)
	}
	s.recordEvent(ctx, barrierAnalyzer, outcome)
	span.SetStatus(codes.Ok, outcome)
	s.logger.InfoContext(ctx, "answered materialization barrier", "job_id", jobID, "outcome", outcome)
	return nil
}

// parseBarrierSubject splits crosscodex.materialize.{tenant}.{job_id}.barrier
// and validates the tenant ID.
func parseBarrierSubject(subject string) (tenantID, jobID string, err error) {
	parts := strings.Split(subject, ".")
	if len(parts) != 5 || parts[0] != "crosscodex" || parts[1] != "materialize" || parts[4] != "barrier" ||
		parts[3] == "" || strings.ContainsAny(parts[3], "*>") {
		return "", "", errors.New("subject does not match crosscodex.materialize.<tenant>.<job_id>.barrier")
	}
	if err := tenant.ValidateTenantID(parts[2]); err != nil {
		return "", "", fmt.Errorf("subject tenant is invalid: %w", err)
	}
	return parts[2], parts[3], nil
}
