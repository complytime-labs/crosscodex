package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// NATSAuditEmitter publishes LLM audit events to NATS. It implements
// llmclient.AuditEmitter with store-or-fail semantics: a failure to store an
// event is returned to the caller, never swallowed.
type NATSAuditEmitter struct {
	bus                 natsbus.Client
	logger              *slog.Logger
	auditFailureCounter metric.Int64Counter
}

// NewNATSAuditEmitter creates an audit emitter backed by a NATS client.
// Audit failures are logged and returned but not metered. Use
// NewNATSAuditEmitterWithMetrics to enable observable failure counting.
func NewNATSAuditEmitter(bus natsbus.Client, logger *slog.Logger) *NATSAuditEmitter {
	if logger == nil {
		logger = slog.Default()
	}
	return &NATSAuditEmitter{bus: bus, logger: logger}
}

// NewNATSAuditEmitterWithMetrics creates an audit emitter that increments a
// counter on every failure to store an event, enabling OWASP A09 alerting on
// audit loss. The counter is named "crosscodex.worker.audit.failures.total".
func NewNATSAuditEmitterWithMetrics(bus natsbus.Client, logger *slog.Logger, mp metric.MeterProvider) *NATSAuditEmitter {
	if logger == nil {
		logger = slog.Default()
	}
	e := &NATSAuditEmitter{bus: bus, logger: logger}
	if mp != nil {
		counter, err := mp.Meter("crosscodex/internal/worker").Int64Counter(
			"crosscodex.worker.audit.failures.total",
			metric.WithDescription("Number of audit events that could not be published to NATS"),
		)
		if err != nil {
			logger.Warn("audit emitter: failed to create failure counter", "error", err)
		} else {
			e.auditFailureCounter = counter
		}
	}
	return e
}

// EmitLLMAudit stores an audit event on the audit stream for the event's
// tenant and job and waits for the stream's acknowledgement. Every failure
// (invalid subject or tenant, marshal, publish, no acknowledgement) is
// returned, wrapped with what failed, and counted: the caller must treat the
// LLM call as unaudited.
func (e *NATSAuditEmitter) EmitLLMAudit(ctx context.Context, event *llmclient.AuditEvent) error {
	fail := func(reason, what string, err error) error {
		e.logger.Error("audit emit failed",
			"reason", reason,
			"tenant_id", event.TenantID,
			"job_id", event.JobID,
			"error", err,
		)
		if e.auditFailureCounter != nil {
			e.auditFailureCounter.Add(ctx, 1, metric.WithAttributes(
				attribute.String("reason", reason),
				attribute.String("tenant_id", event.TenantID),
			))
		}
		return fmt.Errorf("audit emit for job %q: %s: %w", event.JobID, what, err)
	}

	subject, err := natsbus.AuditSubject(event.TenantID, natsbus.AuditLLM, event.JobID)
	if err != nil {
		return fail("invalid_subject", "building the audit subject", err)
	}

	data, err := json.Marshal(event)
	if err != nil {
		return fail("marshal_failed", "encoding the audit event", err)
	}

	// Ensure tenant is in context for natsbus provenance header injection.
	pubCtx, err := tenant.WithTenant(ctx, event.TenantID)
	if err != nil {
		return fail("invalid_tenant", "validating the event tenant", err)
	}
	if err := e.bus.PublishDurable(pubCtx, subject, data, nil); err != nil {
		return fail("publish_failed", "the audit stream did not acknowledge the event", err)
	}
	return nil
}

// Compile-time interface check.
var _ llmclient.AuditEmitter = (*NATSAuditEmitter)(nil)
