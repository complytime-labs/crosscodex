package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"

	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

const (
	subscriberSubject = "crosscodex.pipeline.*.*.stage.completed"
	queueGroup        = "graph-materializer"
)

// Start begins consuming pipeline stage completion events.
func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.sub != nil {
		return ErrAlreadyStarted
	}

	sub, err := s.bus.QueueSubscribe(ctx, subscriberSubject, queueGroup, s.handleEvent)
	if err != nil {
		return fmt.Errorf("graph subscriber start: %w", err)
	}
	s.sub = sub
	s.logger.InfoContext(ctx, "graph subscriber started", "subject", subscriberSubject, "queue", queueGroup)
	return nil
}

// Stop drains the NATS subscription and waits for in-flight work.
func (s *Service) Stop(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.sub == nil {
		return ErrNotStarted
	}

	if err := s.sub.Drain(); err != nil {
		return fmt.Errorf("graph subscriber stop: %w", err)
	}
	s.sub = nil
	s.logger.InfoContext(ctx, "graph subscriber stopped")
	return nil
}

// pipelineEvent is the JSON payload published by NATSStageReporter.
type pipelineEvent struct {
	Analyzer  string `json:"analyzer"`
	JobID     string `json:"job_id"`
	Stage     string `json:"stage"`
	Timestamp string `json:"timestamp"`
}

// handleEvent processes a single pipeline stage completion event.
func (s *Service) handleEvent(ctx context.Context, msg *natsbus.Message) error {
	start := time.Now()
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "graph.subscribe.handle")
	defer span.End()

	// Extract tenant from subject: crosscodex.pipeline.{tenant}.{job_id}.stage.completed
	tenantID, err := extractTenantFromSubject(msg.Subject)
	if err != nil {
		s.logger.ErrorContext(ctx, "invalid event subject", "subject", msg.Subject, "error", err)
		s.recordEvent(ctx, "unknown", "error")
		span.SetStatus(codes.Error, err.Error())
		return nil // don't redeliver malformed subjects
	}

	if err := tenant.ValidateTenantID(tenantID); err != nil {
		s.logger.ErrorContext(ctx, "invalid tenant in event", "tenant", tenantID, "error", err)
		s.recordEvent(ctx, "unknown", "error")
		span.SetStatus(codes.Error, err.Error())
		return nil
	}

	span.SetAttributes(attribute.String("tenant.id", tenantID))

	var event pipelineEvent
	if err := json.Unmarshal(msg.Data, &event); err != nil {
		s.logger.ErrorContext(ctx, "invalid event payload", "error", err)
		s.recordEvent(ctx, "unknown", "error")
		span.SetStatus(codes.Error, err.Error())
		return nil
	}

	if event.Analyzer == "" || event.JobID == "" {
		s.logger.ErrorContext(ctx, "missing analyzer or job_id in event")
		s.recordEvent(ctx, "unknown", "error")
		span.SetStatus(codes.Error, "missing fields")
		return nil
	}

	span.SetAttributes(
		attribute.String("analyzer", event.Analyzer),
		attribute.String("job.id", event.JobID),
	)

	// Build resource ref for resolution.
	ref := ResourceRef{
		Type: "analysis_result",
		ID:   event.JobID,
		URI:  fmt.Sprintf("pg://results/%s/%s", event.JobID, event.Analyzer),
	}

	// Resolve the analysis result data.
	data, err := s.resolvers.Resolve(ctx, ref)
	if err != nil {
		s.logger.ErrorContext(ctx, "resource resolution failed",
			"analyzer", event.Analyzer, "job_id", event.JobID, "error", err)
		s.recordEvent(ctx, event.Analyzer, "error")
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("resolve: %w", err) // NAK for redelivery
	}

	// Dispatch to analyzer-specific handler.
	if err := s.materialize(ctx, tenantID, event.Analyzer, event.JobID, data); err != nil {
		s.logger.ErrorContext(ctx, "materialization failed",
			"analyzer", event.Analyzer, "job_id", event.JobID, "error", err)
		s.recordEvent(ctx, event.Analyzer, "error")
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("materialize: %w", err) // NAK for redelivery
	}

	s.recordEvent(ctx, event.Analyzer, "ok")
	span.SetStatus(codes.Ok, "")
	s.logger.InfoContext(ctx, "graph materialized",
		"analyzer", event.Analyzer, "job_id", event.JobID,
		"duration_ms", time.Since(start).Milliseconds())
	return nil
}

// extractTenantFromSubject parses tenant from subject hierarchy.
// Subject format: crosscodex.pipeline.{tenant}.{job_id}.stage.completed
func extractTenantFromSubject(subject string) (string, error) {
	parts := strings.Split(subject, ".")
	if len(parts) < 6 {
		return "", fmt.Errorf("subject has %d parts, expected 6+: %s", len(parts), subject)
	}
	return parts[2], nil
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
