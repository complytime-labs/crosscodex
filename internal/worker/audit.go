package worker

import (
	"context"
	"encoding/json"
	"log/slog"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// NATSAuditEmitter publishes LLM audit events to NATS. It implements
// llmclient.AuditEmitter with best-effort semantics: publish failures
// are logged but never propagated to the caller.
type NATSAuditEmitter struct {
	bus                 natsbus.Client
	logger              *slog.Logger
	auditFailureCounter metric.Int64Counter
}

// NewNATSAuditEmitter creates an audit emitter backed by a NATS client.
// Audit publish failures are logged but not metered. Use
// NewNATSAuditEmitterWithMetrics to enable observable failure counting.
func NewNATSAuditEmitter(bus natsbus.Client, logger *slog.Logger) *NATSAuditEmitter {
	if logger == nil {
		logger = slog.Default()
	}
	return &NATSAuditEmitter{bus: bus, logger: logger}
}

// NewNATSAuditEmitterWithMetrics creates an audit emitter that increments a
// counter on every swallowed publish failure, enabling OWASP A09 alerting on
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

// EmitLLMAudit publishes an audit event to the NATS audit subject for the
// event's tenant and job. Best-effort: returns nil on publish failure.
func (e *NATSAuditEmitter) EmitLLMAudit(ctx context.Context, event *llmclient.AuditEvent) error {
	subject, err := natsbus.AuditSubject(event.TenantID, natsbus.AuditLLM, event.JobID)
	if err != nil {
		e.logger.Warn("audit emit: invalid subject",
			"tenant_id", event.TenantID,
			"job_id", event.JobID,
			"error", err,
		)
		if e.auditFailureCounter != nil {
			e.auditFailureCounter.Add(ctx, 1, metric.WithAttributes(
				attribute.String("reason", "invalid_subject"),
				attribute.String("tenant_id", event.TenantID),
			))
		}
		return nil
	}

	data, err := json.Marshal(event)
	if err != nil {
		e.logger.Warn("audit emit: marshal failed",
			"tenant_id", event.TenantID,
			"job_id", event.JobID,
			"error", err,
		)
		if e.auditFailureCounter != nil {
			e.auditFailureCounter.Add(ctx, 1, metric.WithAttributes(
				attribute.String("reason", "marshal_failed"),
				attribute.String("tenant_id", event.TenantID),
			))
		}
		return nil
	}

	// Ensure tenant is in context for natsbus provenance header injection.
	pubCtx, err := tenant.WithTenant(ctx, event.TenantID)
	if err != nil {
		e.logger.Warn("audit emit: invalid tenant ID",
			"tenant_id", event.TenantID,
			"job_id", event.JobID,
			"error", err,
		)
		if e.auditFailureCounter != nil {
			e.auditFailureCounter.Add(ctx, 1, metric.WithAttributes(
				attribute.String("reason", "invalid_tenant"),
				attribute.String("tenant_id", event.TenantID),
			))
		}
		return nil
	}
	if err := e.bus.Publish(pubCtx, subject, data); err != nil {
		e.logger.Warn("audit emit: publish failed",
			"tenant_id", event.TenantID,
			"job_id", event.JobID,
			"subject", subject,
			"error", err,
		)
		if e.auditFailureCounter != nil {
			e.auditFailureCounter.Add(ctx, 1, metric.WithAttributes(
				attribute.String("reason", "publish_failed"),
				attribute.String("tenant_id", event.TenantID),
			))
		}
		return nil
	}

	return nil
}

// Compile-time interface check.
var _ llmclient.AuditEmitter = (*NATSAuditEmitter)(nil)
