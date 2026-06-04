package authn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/pkg/telemetry"
)

// Registry holds an ordered list of authenticators and dispatches
// authentication requests. ErrUnsupportedMethod means "try next."
// Any other error stops iteration.
type Registry struct {
	authenticators []Authenticator
	emitter        AuditEmitter

	// Telemetry (optional, nil-safe)
	tracer      trace.Tracer
	meter       metric.Meter
	authCounter metric.Int64Counter
	authLatency metric.Int64Histogram
}

// NewRegistry creates a Registry with the given audit emitter and authenticators.
// A nil emitter disables audit emission (no panic, no-op).
func NewRegistry(emitter AuditEmitter, authenticators []Authenticator, opts ...RegistryOption) (*Registry, error) {
	r := &Registry{
		authenticators: authenticators,
		emitter:        emitter,
	}
	for _, opt := range opts {
		if err := opt(r); err != nil {
			return nil, fmt.Errorf("apply registry option: %w", err)
		}
	}
	return r, nil
}

// Authenticate tries each registered authenticator in order.
//
// Dispatch rules:
//  1. If an authenticator returns ErrUnsupportedMethod, try the next one.
//  2. If an authenticator returns any other error, emit a failure audit event
//     and return that error.
//  3. If an authenticator returns an Identity, emit a success audit event
//     and return the Identity.
//  4. If all authenticators return ErrUnsupportedMethod, emit a failure audit
//     event and return ErrAuthenticationFailed.
func (r *Registry) Authenticate(ctx context.Context, req *Request) (*Identity, error) {
	start := time.Now()
	var span trace.Span
	if r.tracer != nil {
		ctx, span = r.tracer.Start(ctx, "authn.Authenticate")
		defer span.End()
		if req.Method != "" {
			span.SetAttributes(attribute.String("auth.method", string(req.Method)))
		}
	}

	for _, auth := range r.authenticators {
		identity, err := auth.Authenticate(ctx, req)
		if err != nil {
			if errors.Is(err, ErrUnsupportedMethod) {
				continue
			}
			r.recordMetrics(ctx, start, false)
			if span != nil {
				span.SetAttributes(attribute.Bool("auth.success", false))
				span.SetStatus(codes.Error, err.Error())
			}
			r.emitFailure(ctx, req, err)
			return nil, err
		}
		r.recordMetrics(ctx, start, true)
		if span != nil {
			span.SetAttributes(
				attribute.Bool("auth.success", true),
				attribute.String("tenant.id", identity.TenantID),
			)
			span.SetStatus(codes.Ok, "")
		}
		// Populate SessionID from trace context for audit correlation
		if req.SessionID == "" {
			req.SessionID = telemetry.TraceIDFromContext(ctx)
		}
		r.emitSuccess(ctx, req, identity)
		return identity, nil
	}

	// All authenticators returned ErrUnsupportedMethod
	err := fmt.Errorf("no authenticator accepted the request: %w", ErrAuthenticationFailed)
	r.recordMetrics(ctx, start, false)
	if span != nil {
		span.SetAttributes(attribute.Bool("auth.success", false))
		span.SetStatus(codes.Error, err.Error())
	}
	r.emitFailure(ctx, req, err)
	return nil, err
}

func (r *Registry) recordMetrics(ctx context.Context, start time.Time, success bool) {
	if r.authCounter != nil {
		r.authCounter.Add(ctx, 1,
			metric.WithAttributes(attribute.Bool("success", success)))
	}
	if r.authLatency != nil {
		r.authLatency.Record(ctx, time.Since(start).Milliseconds())
	}
}

func (r *Registry) emitSuccess(ctx context.Context, req *Request, identity *Identity) {
	if r.emitter == nil {
		return
	}
	event := &AuthEvent{
		Timestamp: time.Now(),
		Principal: identity.Subject,
		TenantID:  identity.TenantID,
		Roles:     identity.Roles,
		Method:    identity.Method,
		ClientIP:  req.ClientIP,
		Success:   true,
		SessionID: req.SessionID,
	}
	// Best-effort audit emission; do not fail authentication due to audit errors.
	if err := r.emitter.EmitAuthEvent(ctx, event); err != nil {
		slog.WarnContext(ctx, "audit emission failed for success event",
			"error", err,
			"principal", identity.Subject,
			"tenant_id", identity.TenantID,
		)
	}
}

func (r *Registry) emitFailure(ctx context.Context, req *Request, authErr error) {
	if r.emitter == nil {
		return
	}
	principal := "unknown"
	if req.TLSState != nil && len(req.TLSState.PeerCertificates) > 0 {
		principal = req.TLSState.PeerCertificates[0].Subject.CommonName
	}
	event := &AuthEvent{
		Timestamp:     time.Now(),
		Principal:     principal,
		TenantID:      "unknown",
		Method:        req.Method,
		ClientIP:      req.ClientIP,
		Success:       false,
		FailureReason: authErr.Error(),
		SessionID:     req.SessionID,
	}
	if err := r.emitter.EmitAuthEvent(ctx, event); err != nil {
		slog.WarnContext(ctx, "audit emission failed for failure event",
			"error", err,
			"principal", principal,
		)
	}
}
