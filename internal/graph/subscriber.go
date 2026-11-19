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
	"go.opentelemetry.io/otel/metric"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

const (
	subscriberSubject = "crosscodex.pipeline.*.*.stage.completed"

	// materializerConsumer and barrierConsumer are the durable MATERIALIZE
	// consumers the graph role owns. Every graph role process attaches to
	// the same two, so each message goes to one of them.
	materializerConsumer = "graph-materializer"
	barrierConsumer      = "graph-barrier"
)

// Start creates the durable MATERIALIZE consumers: graph-materializer for
// pipeline stage completion events and, when a ControlsReader is set, catalog
// parsed events, and graph-barrier for job barriers. MATERIALIZE is a work
// queue, so a message stays in it until a handler acks or terminates it, and
// survives a graph role restart.
func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.sub != nil {
		return ErrAlreadyStarted
	}

	filters := []string{subscriberSubject}
	if s.controls != nil {
		filters = append(filters, catalogSubject)
	}
	sub, err := s.bus.ConsumeDurable(ctx, natsbus.ConsumerConfig{
		Stream:         natsbus.MaterializeStream,
		Durable:        materializerConsumer,
		FilterSubjects: filters,
		AckWait:        s.delivery.AckWait,
		MaxDeliver:     s.delivery.MaxDeliver,
		RedeliverDelay: s.delivery.RedeliverDelay,
	}, s.handleMaterialize)
	if err != nil {
		return fmt.Errorf("graph subscriber start: consumer %s: %w", materializerConsumer, err)
	}

	// The barrier is redelivered until the job's events are all handled or
	// its deadline passes, so it has no delivery limit.
	barrierSub, err := s.bus.ConsumeDurable(ctx, natsbus.ConsumerConfig{
		Stream:         natsbus.MaterializeStream,
		Durable:        barrierConsumer,
		FilterSubjects: []string{barrierSubject},
		AckWait:        s.delivery.AckWait,
		MaxDeliver:     -1,
		RedeliverDelay: s.delivery.BarrierPollInterval,
	}, s.handleBarrier)
	if err != nil {
		if uerr := sub.Unsubscribe(); uerr != nil {
			err = errors.Join(err, fmt.Errorf("unsubscribe consumer %s: %w", materializerConsumer, uerr))
		}
		return fmt.Errorf("graph subscriber start: consumer %s: %w", barrierConsumer, err)
	}

	s.sub = sub
	s.barrierSub = barrierSub
	s.logger.InfoContext(ctx, "graph subscriber started", "stream", natsbus.MaterializeStream,
		"consumers", []string{materializerConsumer, barrierConsumer}, "subjects", filters,
		"max_deliver", s.delivery.MaxDeliver, "ack_wait", s.delivery.AckWait)
	return nil
}

// Stop drains both consumers and waits for in-flight work.
func (s *Service) Stop(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.sub == nil {
		return ErrNotStarted
	}

	var errs []error
	if err := s.barrierSub.Drain(); err != nil {
		errs = append(errs, fmt.Errorf("consumer %s: %w", barrierConsumer, err))
	}
	if err := s.sub.Drain(); err != nil {
		errs = append(errs, fmt.Errorf("consumer %s: %w", materializerConsumer, err))
	}
	s.sub = nil
	s.barrierSub = nil
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("graph subscriber stop: %w", err)
	}
	s.logger.InfoContext(ctx, "graph subscriber stopped")
	return nil
}

// handleMaterialize routes a graph-materializer message by its subject: a
// catalog parsed event to the catalog projection, anything else to the stage
// completion handler, which rejects a subject it does not recognise.
func (s *Service) handleMaterialize(ctx context.Context, msg *natsbus.Message) error {
	if s.controls != nil && strings.HasPrefix(msg.Subject, catalogSubjectPrefix) {
		return s.handleCatalogEvent(ctx, msg)
	}
	return s.handleEvent(ctx, msg)
}

// lastDelivery reports whether msg is on its last delivery, after which the
// server will not redeliver it whatever the handler returns.
func (s *Service) lastDelivery(msg *natsbus.Message) bool {
	// MaxDeliver is at least 1 (see WithMaterializeConfig), so the
	// conversion cannot wrap.
	return msg.Metadata.NumDelivered >= uint64(s.delivery.MaxDeliver)
}

// errTenantMismatch marks a message whose subject names another tenant than
// its provenance headers.
var errTenantMismatch = errors.New("subject tenant does not match the message's provenance tenant")

// verifySubjectTenant requires subjectTenant, taken from a message subject,
// to be the tenant the consumer verified from the message's provenance
// headers and put in ctx. NATS isolates tenants by subject only, so a message
// whose two tenants differ is terminated rather than written into either
// tenant's graph. The error names neither tenant's data.
func verifySubjectTenant(ctx context.Context, subjectTenant string) error {
	provenance, err := tenant.FromContext(ctx)
	if err != nil {
		return fmt.Errorf("%w: %w: no provenance tenant: %w", natsbus.ErrPermanent, errTenantMismatch, err)
	}
	if provenance != subjectTenant {
		return fmt.Errorf("%w: %w: the subject names tenant %q; the event is dropped and written into no graph",
			natsbus.ErrPermanent, errTenantMismatch, subjectTenant)
	}
	return nil
}

// pipelineEvent is the JSON payload published by NATSStageReporter. Metadata
// is the completed analyzer's output metadata.
type pipelineEvent struct {
	Analyzer  string            `json:"analyzer"`
	JobID     string            `json:"job_id"`
	Stage     string            `json:"stage"`
	Timestamp string            `json:"timestamp"`
	Metadata  map[string]string `json:"metadata"`
}

// graphDeferred reports whether the pipeline deferred this relationship or
// requires event's edges to the job's critique event (see
// results.GraphDeferredMetadataKey). Only those two analyzers' edges are
// written by the critique handler, so the mark is ignored on any other event.
func (e pipelineEvent) graphDeferred() bool {
	switch e.Analyzer {
	case results.RelationshipAnalyzer, results.RequiresAnalyzer:
		return e.Metadata[results.GraphDeferredMetadataKey] == "true"
	default:
		return false
	}
}

// handleEvent materializes one pipeline stage completion event. Its return
// decides what the durable consumer does with the message:
//
//   - nil acks it: written, deferred to the critique stage, or from a stage
//     the graph has no handler for.
//   - natsbus.ErrSkip terminates it quietly: the subject or payload can never
//     be processed, or it names no analyzer.
//   - natsbus.ErrPermanent terminates it: its tenants disagree, or it failed
//     on its last delivery after a "failed" graph outcome was stored.
//   - natsbus.ErrNotReady naks it quietly: an edge's endpoint node is not
//     projected yet.
//   - any other error naks it for redelivery after the redelivery delay.
//
// Every write is idempotent, so a redelivered event, including one still
// running when ack_wait ended, writes nothing twice.
func (s *Service) handleEvent(ctx context.Context, msg *natsbus.Message) error {
	start := time.Now()
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "graph.subscribe.handle")
	defer span.End()

	// skip terminates an event that can never be processed. It is not an
	// error of the graph role (the issue: ignore events not from analyzers),
	// so ErrSkip has the consumer log it at debug, without the payload.
	skip := func(reason string) error {
		s.recordEvent(ctx, "unknown", "skipped")
		span.SetStatus(codes.Ok, "skipped")
		return fmt.Errorf("%w: stage event on %s: %s", natsbus.ErrSkip, msg.Subject, reason)
	}

	tenantID, jobID, err := parseStageSubject(msg.Subject)
	if err != nil {
		return skip(err.Error())
	}
	if err := verifySubjectTenant(ctx, tenantID); err != nil {
		s.logger.WarnContext(ctx, "rejecting stage event whose subject tenant is not its provenance tenant",
			"subject", msg.Subject, "error", err)
		s.recordEvent(ctx, "unknown", "rejected")
		span.SetStatus(codes.Error, "tenant mismatch")
		return err
	}
	span.SetAttributes(attribute.String("tenant.id", tenantID))

	var event pipelineEvent
	if err := json.Unmarshal(msg.Data, &event); err != nil {
		return skip("payload is not a stage event")
	}
	if event.Analyzer == "" {
		return skip("payload names no analyzer")
	}
	if event.JobID != jobID {
		return skip("payload job_id does not match the subject's job")
	}

	span.SetAttributes(
		attribute.String("analyzer", event.Analyzer),
		attribute.String("job.id", event.JobID),
	)

	// The job runs the critique stage: its event writes this stage's
	// surviving edges, since an edge written now could not be removed after
	// a veto.
	if event.graphDeferred() {
		s.logger.DebugContext(ctx, "graph edges deferred to the critique stage", "analyzer", event.Analyzer, "job_id", event.JobID)
		s.recordEvent(ctx, event.Analyzer, "deferred")
		span.SetStatus(codes.Ok, "deferred")
		return nil
	}

	handle, ok := s.handlerFor(event.Analyzer)
	if !ok {
		s.logger.DebugContext(ctx, "no graph handler for stage", "analyzer", event.Analyzer, "job_id", event.JobID)
		s.recordEvent(ctx, event.Analyzer, "skipped")
		span.SetStatus(codes.Ok, "no handler")
		return nil
	}

	// Build resource ref for resolution.
	ref := ResourceRef{
		Type: "analysis_result",
		ID:   event.JobID,
		URI:  fmt.Sprintf("pg://results/%s/%s", event.JobID, event.Analyzer),
	}

	pending := &pendingEndpoints{final: s.lastDelivery(msg)}
	data, err := s.resolvers.Resolve(ctx, ref)
	if err != nil {
		err = fmt.Errorf("resolve: %w", err)
	} else {
		err = s.materialize(context.WithValue(ctx, pendingEndpointsKey{}, pending), handle, tenantID, event.Analyzer, event.JobID, data)
		if err != nil {
			err = fmt.Errorf("materialize: %w", err)
		}
	}
	if err == nil && pending.count > 0 {
		// Only reached before the last delivery: on it, missing endpoints are
		// skipped instead of counted.
		s.recordEvent(ctx, event.Analyzer, "retry")
		span.SetStatus(codes.Error, "endpoint nodes not projected yet")
		return fmt.Errorf("%w: %d edges of the %s results for job %s have an endpoint node that is not projected yet; "+
			"retrying the event (delivery %d of %d) so the catalog projection can write it; "+
			"on the last delivery those edges are skipped", natsbus.ErrNotReady, pending.count, event.Analyzer, event.JobID,
			msg.Metadata.NumDelivered, s.delivery.MaxDeliver)
	}
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return s.failEvent(ctx, msg, tenantID, event, err)
	}

	s.recordEvent(ctx, event.Analyzer, "ok")
	span.SetStatus(codes.Ok, "")
	s.logger.InfoContext(ctx, "graph materialized",
		"analyzer", event.Analyzer, "job_id", event.JobID,
		"duration_ms", time.Since(start).Milliseconds())
	return nil
}

// failEvent returns the error that settles an event whose results could not
// be written. Before its last delivery the event is redelivered. On the last,
// a "failed" graph outcome naming the analyzer is stored first, so the job's
// barrier sees the failure even if nothing is listening now, and the event is
// then terminated. If the outcome is not stored the event is naked instead,
// leaving it in the stream, so the job's barrier keeps waiting rather than
// reporting the graph materialized.
func (s *Service) failEvent(ctx context.Context, msg *natsbus.Message, tenantID string, event pipelineEvent, cause error) error {
	// The consumer logs the returned error with the subject and delivery.
	if !s.lastDelivery(msg) {
		s.recordEvent(ctx, event.Analyzer, "error")
		return fmt.Errorf("delivery %d of %d of the %s results for job %s failed; the event will be redelivered: %w",
			msg.Metadata.NumDelivered, s.delivery.MaxDeliver, event.Analyzer, event.JobID, cause)
	}

	// The outcome is read by the job's tenant: it names the analyzer and
	// where to look, never the driver error.
	reason := fmt.Sprintf("graph materialization of the %s results failed on all %d deliveries; "+
		"see the graph role logs for job %s", event.Analyzer, s.delivery.MaxDeliver, event.JobID)
	if err := s.publishOutcome(ctx, tenantID, event.JobID, natsbus.GraphOutcomeFailed, event.Analyzer, reason); err != nil {
		s.recordEvent(ctx, event.Analyzer, "error")
		return fmt.Errorf("last delivery of the %s results for job %s failed, and its failed graph outcome was not stored, "+
			"so the event stays in %s and the job's barrier waits until its deadline: %w",
			event.Analyzer, event.JobID, natsbus.MaterializeStream, errors.Join(cause, err))
	}
	s.recordEvent(ctx, event.Analyzer, "failed")
	return fmt.Errorf("%w: last delivery (%d) of the %s results for job %s failed and a failed graph outcome was stored: %w",
		natsbus.ErrPermanent, s.delivery.MaxDeliver, event.Analyzer, event.JobID, cause)
}

// publishOutcome durably publishes a job's graph outcome to AUDIT_EVENTS.
func (s *Service) publishOutcome(ctx context.Context, tenantID, jobID, outcome, analyzer, reason string) error {
	subject, err := natsbus.GraphOutcomeSubject(tenantID, jobID, outcome)
	if err != nil {
		return fmt.Errorf("graph outcome subject: %w", err)
	}
	data, err := json.Marshal(natsbus.GraphOutcome{JobID: jobID, Outcome: outcome, Analyzer: analyzer, Error: reason})
	if err != nil {
		return fmt.Errorf("marshal graph outcome: %w", err)
	}
	if err := s.bus.PublishDurable(ctx, subject, data, nil); err != nil {
		return fmt.Errorf("store %s graph outcome for job %s: %w", outcome, jobID, err)
	}
	return nil
}

// parseStageSubject splits crosscodex.pipeline.{tenant}.{job_id}.stage.completed
// and validates the tenant ID.
func parseStageSubject(subject string) (tenantID, jobID string, err error) {
	parts := strings.Split(subject, ".")
	if len(parts) != 6 || parts[0] != "crosscodex" || parts[1] != "pipeline" ||
		parts[4] != "stage" || parts[5] != "completed" || parts[3] == "" || strings.ContainsAny(parts[3], "*>") {
		return "", "", errors.New("subject does not match crosscodex.pipeline.<tenant>.<job_id>.stage.completed")
	}
	if err := tenant.ValidateTenantID(parts[2]); err != nil {
		return "", "", fmt.Errorf("subject tenant is invalid: %w", err)
	}
	return parts[2], parts[3], nil
}

func (s *Service) recordEvent(ctx context.Context, analyzer, status string) {
	if s.eventCounter != nil {
		s.eventCounter.Add(ctx, 1,
			metric.WithAttributes(
				attribute.String("analyzer", analyzer),
				attribute.String("status", status),
			))
	}
}
