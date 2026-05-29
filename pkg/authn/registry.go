package authn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Registry holds an ordered list of authenticators and dispatches
// authentication requests. ErrUnsupportedMethod means "try next."
// Any other error stops iteration.
type Registry struct {
	authenticators []Authenticator
	emitter        AuditEmitter
}

// NewRegistry creates a Registry with the given audit emitter and authenticators.
// A nil emitter disables audit emission (no panic, no-op).
func NewRegistry(emitter AuditEmitter, authenticators ...Authenticator) *Registry {
	return &Registry{
		authenticators: authenticators,
		emitter:        emitter,
	}
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
	for _, auth := range r.authenticators {
		identity, err := auth.Authenticate(ctx, req)
		if err != nil {
			if errors.Is(err, ErrUnsupportedMethod) {
				continue
			}
			// Non-ErrUnsupportedMethod error: stop, emit failure, return
			r.emitFailure(ctx, req, err)
			return nil, err
		}
		// Success
		r.emitSuccess(ctx, req, identity)
		return identity, nil
	}

	// All authenticators returned ErrUnsupportedMethod
	err := fmt.Errorf("no authenticator accepted the request: %w", ErrAuthenticationFailed)
	r.emitFailure(ctx, req, err)
	return nil, err
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
