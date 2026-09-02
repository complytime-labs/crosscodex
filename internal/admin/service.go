// Package admin implements the Connect AdminService handler for crosscodex.
// It exposes the data-retention and legal-hold RPCs, each gated by an admin
// RBAC check, and delegates the actual lifecycle work to a retention.Engine
// and retention.HoldStore. The remaining AdminService RPCs (tenant, health,
// and PKI management) are intentionally left as CodeUnimplemented via the
// embedded generated base: their features are out of scope for this service.
package admin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	crosscodexv1connect "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/retention"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// Service implements crosscodexv1connect.AdminServiceHandler. It overrides the
// six retention/legal-hold RPCs and inherits CodeUnimplemented for the rest via
// the embedded UnimplementedAdminServiceHandler (Ruling AA).
type Service struct {
	crosscodexv1connect.UnimplementedAdminServiceHandler

	// engineFor builds a retention.Engine scoped to a specific tenant, using the
	// given policy. Each RPC builds its engine for the resolved caller tenant so
	// collectors and storage providers only ever touch that tenant's data.
	engineFor func(tenantID string, policy retention.Policy) (*retention.Engine, error)
	// defaultPolicy is the configured policy used by scans that do not carry a
	// caller-supplied policy override (ScanRetention, GetRetentionStats).
	defaultPolicy retention.Policy
	// holds persists and retrieves legal/compliance holds.
	holds retention.HoldStore
	// driftSource reports audit JetStream retention drift (configured vs live
	// max age). It is assembled in bootstrap from the natsbus client and the
	// configured stream retention so Service stays decoupled from both.
	driftSource func(ctx context.Context) ([]retention.StreamDrift, error)
}

var _ crosscodexv1connect.AdminServiceHandler = (*Service)(nil)

// NewService wires an AdminService handler. engineFor builds a per-request
// engine scoped to the resolved caller tenant with the given policy;
// defaultPolicy is used when no policy override is supplied; holds backs the
// legal-hold CRUD RPCs; driftSource reports audit-stream retention drift for
// GetRetentionStats.
func NewService(
	engineFor func(tenantID string, policy retention.Policy) (*retention.Engine, error),
	defaultPolicy retention.Policy,
	holds retention.HoldStore,
	driftSource func(ctx context.Context) ([]retention.StreamDrift, error),
) *Service {
	return &Service{
		engineFor:     engineFor,
		defaultPolicy: defaultPolicy,
		holds:         holds,
		driftSource:   driftSource,
	}
}

// requireAdmin enforces the admin RBAC gate. A nil identity yields
// CodeUnauthenticated; a non-admin identity yields CodePermissionDenied.
func requireAdmin(ctx context.Context) error {
	identity := authn.IdentityFromContext(ctx)
	if identity == nil {
		return connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	}
	if err := authn.RequireRole(*identity, authn.RoleAdmin); err != nil {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("admin access required: %v", err))
	}
	return nil
}

// resolveTenant validates the request's TenantContext against the tenant the
// auth interceptor placed in the context, mirroring graph.Service.extractTenant.
// It returns the caller's tenant; scopeCtx then applies it to the context so the
// engine and holdstore operate under that tenant (RLS). This check rejects a
// request that declares a tenant other than the caller's.
func resolveTenant(ctx context.Context, tc *pb.TenantContext) (string, error) {
	if tc == nil || tc.GetTenantId() == "" {
		return "", connect.NewError(connect.CodeInvalidArgument, errors.New("tenant_context is required"))
	}
	ctxTenant, err := tenant.FromContext(ctx)
	if err != nil {
		return "", connect.NewError(connect.CodeUnauthenticated, errors.New("no tenant in context"))
	}
	if ctxTenant != tc.GetTenantId() {
		return "", connect.NewError(connect.CodePermissionDenied, errors.New("tenant mismatch"))
	}
	return ctxTenant, nil
}

// scopeCtx validates the request tenant against the caller's context tenant
// and returns a context carrying that tenant for RLS-scoped DB access.
func scopeCtx(ctx context.Context, tc *pb.TenantContext) (context.Context, string, error) {
	resolved, err := resolveTenant(ctx, tc)
	if err != nil {
		return nil, "", err
	}
	ctx, err = tenant.WithTenant(ctx, resolved)
	if err != nil {
		return nil, "", connect.NewError(connect.CodeInternal, fmt.Errorf("scope tenant: %w", err))
	}
	return ctx, resolved, nil
}

// ScanRetention runs a retention sweep with the configured default policy.
func (s *Service) ScanRetention(ctx context.Context, req *connect.Request[pb.ScanRetentionRequest]) (*connect.Response[pb.ScanRetentionResponse], error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	ctx, resolved, err := scopeCtx(ctx, req.Msg.GetTenantContext())
	if err != nil {
		return nil, err
	}

	engine, err := s.engineFor(resolved, s.defaultPolicy)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("build retention engine: %w", err))
	}
	rep, err := engine.Scan(ctx, retention.ScanOptions{DryRun: req.Msg.GetDryRun(), Now: time.Now().UTC()})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("scan retention: %w", err))
	}
	return connect.NewResponse(&pb.ScanRetentionResponse{Report: reportToProto(rep)}), nil
}

// ApplyRetentionPolicy runs a scan immediately with a caller-supplied policy
// (Ruling AB). The policy is applied per-invocation; nothing is persisted.
func (s *Service) ApplyRetentionPolicy(ctx context.Context, req *connect.Request[pb.ApplyRetentionPolicyRequest]) (*connect.Response[pb.ApplyRetentionPolicyResponse], error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	ctx, resolved, err := scopeCtx(ctx, req.Msg.GetTenantContext())
	if err != nil {
		return nil, err
	}
	if req.Msg.GetPolicy() == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("policy is required"))
	}

	policy, err := retention.NewPolicy(policyConfigFromProto(req.Msg.GetPolicy()))
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid retention policy: %w", err))
	}

	engine, err := s.engineFor(resolved, policy)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("build retention engine: %w", err))
	}
	rep, err := engine.Scan(ctx, retention.ScanOptions{DryRun: req.Msg.GetDryRun(), Now: time.Now().UTC()})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("apply retention policy: %w", err))
	}
	return connect.NewResponse(&pb.ApplyRetentionPolicyResponse{Report: reportToProto(rep)}), nil
}

// GetRetentionStats returns a dry-run scan report (what would be eligible now)
// plus the count of currently-active holds (Ruling AC).
func (s *Service) GetRetentionStats(ctx context.Context, req *connect.Request[pb.GetRetentionStatsRequest]) (*connect.Response[pb.GetRetentionStatsResponse], error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	ctx, resolved, err := scopeCtx(ctx, req.Msg.GetTenantContext())
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	engine, err := s.engineFor(resolved, s.defaultPolicy)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("build retention engine: %w", err))
	}
	rep, err := engine.Scan(ctx, retention.ScanOptions{DryRun: true, Now: now})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("retention stats scan: %w", err))
	}
	active, err := s.holds.Active(ctx, now)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("count active holds: %w", err))
	}
	drift, err := s.driftSource(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("verify stream retention: %w", err))
	}
	return connect.NewResponse(&pb.GetRetentionStatsResponse{
		Eligible:     reportToProto(rep),
		ActiveHolds:  int64(len(active)),
		StreamDrifts: streamDriftToProto(drift),
	}), nil
}

// streamDriftToProto maps audit-stream retention drift to its proto form.
func streamDriftToProto(drift []retention.StreamDrift) []*pb.StreamDrift {
	out := make([]*pb.StreamDrift, 0, len(drift))
	for _, d := range drift {
		out = append(out, &pb.StreamDrift{
			Stream:     d.Stream,
			Configured: durationpb.New(d.Configured),
			Live:       durationpb.New(d.Live),
			Ok:         d.OK,
		})
	}
	return out
}

// CreateHold creates a legal/compliance hold stamped with the caller's subject.
func (s *Service) CreateHold(ctx context.Context, req *connect.Request[pb.CreateHoldRequest]) (*connect.Response[pb.CreateHoldResponse], error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	ctx, resolved, err := scopeCtx(ctx, req.Msg.GetTenantContext())
	if err != nil {
		return nil, err
	}
	if req.Msg.GetName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name is required"))
	}

	// Force the hold's tenant to the resolved caller tenant so a caller cannot
	// plant a hold for another tenant (defense-in-depth atop the RLS WITH CHECK).
	scope := holdScopeFromProto(req.Msg.GetScope())
	scope.TenantID = resolved
	h := retention.Hold{
		Name:      req.Msg.GetName(),
		Scope:     scope,
		CreatedBy: authn.IdentityFromContext(ctx).Subject,
	}
	if req.Msg.ExpiresAt != nil {
		expires := req.Msg.GetExpiresAt().AsTime()
		h.ExpiresAt = &expires
	}

	created, err := s.holds.Create(ctx, h)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create hold: %w", err))
	}
	return connect.NewResponse(&pb.CreateHoldResponse{Hold: holdToProto(created)}), nil
}

// ReleaseHold releases the named active hold, stamped with the caller's subject.
func (s *Service) ReleaseHold(ctx context.Context, req *connect.Request[pb.ReleaseHoldRequest]) (*connect.Response[pb.ReleaseHoldResponse], error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	ctx, _, err := scopeCtx(ctx, req.Msg.GetTenantContext())
	if err != nil {
		return nil, err
	}
	if req.Msg.GetName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name is required"))
	}

	if err := s.holds.Release(ctx, req.Msg.GetName(), authn.IdentityFromContext(ctx).Subject); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("release hold: %w", err))
	}
	return connect.NewResponse(&pb.ReleaseHoldResponse{}), nil
}

// ListHolds returns all holds (active and released) mapped to proto.
func (s *Service) ListHolds(ctx context.Context, req *connect.Request[pb.ListHoldsRequest]) (*connect.Response[pb.ListHoldsResponse], error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	ctx, _, err := scopeCtx(ctx, req.Msg.GetTenantContext())
	if err != nil {
		return nil, err
	}

	holds, err := s.holds.List(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list holds: %w", err))
	}
	protoHolds := make([]*pb.Hold, len(holds))
	for i, h := range holds {
		protoHolds[i] = holdToProto(h)
	}
	return connect.NewResponse(&pb.ListHoldsResponse{Holds: protoHolds}), nil
}

// reportToProto maps a retention.Report to its proto representation, widening
// the int counters to int64 and keying PerClass by the DataClass string.
func reportToProto(r retention.Report) *pb.RetentionReport {
	perClass := make(map[string]*pb.ClassCount, len(r.PerClass))
	for class, cc := range r.PerClass {
		perClass[string(class)] = &pb.ClassCount{
			Scanned:  int64(cc.Scanned),
			Held:     int64(cc.Held),
			Archived: int64(cc.Archived),
			Purged:   int64(cc.Purged),
		}
	}
	return &pb.RetentionReport{
		Scanned:  int64(r.Scanned),
		Held:     int64(r.Held),
		Archived: int64(r.Archived),
		Purged:   int64(r.Purged),
		PerClass: perClass,
		Errors:   r.Errors,
	}
}

// holdToProto maps a retention.Hold to proto. Optional timestamps map to nil
// when unset.
func holdToProto(h retention.Hold) *pb.Hold {
	p := &pb.Hold{
		Id:         h.ID,
		Name:       h.Name,
		Scope:      holdScopeToProto(h.Scope),
		CreatedAt:  timestamppb.New(h.CreatedAt),
		CreatedBy:  h.CreatedBy,
		ReleasedBy: h.ReleasedBy,
	}
	if h.ExpiresAt != nil {
		p.ExpiresAt = timestamppb.New(*h.ExpiresAt)
	}
	if h.ReleasedAt != nil {
		p.ReleasedAt = timestamppb.New(*h.ReleasedAt)
	}
	return p
}

// holdFromProto maps a proto Hold back to a retention.Hold. It is the inverse
// of holdToProto.
func holdFromProto(p *pb.Hold) retention.Hold {
	if p == nil {
		return retention.Hold{}
	}
	h := retention.Hold{
		ID:         p.GetId(),
		Name:       p.GetName(),
		Scope:      holdScopeFromProto(p.GetScope()),
		CreatedAt:  p.GetCreatedAt().AsTime(),
		CreatedBy:  p.GetCreatedBy(),
		ReleasedBy: p.GetReleasedBy(),
	}
	if p.ExpiresAt != nil {
		expires := p.GetExpiresAt().AsTime()
		h.ExpiresAt = &expires
	}
	if p.ReleasedAt != nil {
		released := p.GetReleasedAt().AsTime()
		h.ReleasedAt = &released
	}
	return h
}

// holdScopeToProto maps a retention.HoldScope to proto. An unset CreatedBefore
// maps to nil.
func holdScopeToProto(s retention.HoldScope) *pb.HoldScope {
	p := &pb.HoldScope{
		TenantId: s.TenantID,
		JobId:    s.JobID,
	}
	if s.CreatedBefore != nil {
		p.CreatedBefore = timestamppb.New(*s.CreatedBefore)
	}
	return p
}

// holdScopeFromProto maps a proto HoldScope back to a retention.HoldScope. A nil
// proto scope maps to the zero value (all wildcards).
func holdScopeFromProto(p *pb.HoldScope) retention.HoldScope {
	if p == nil {
		return retention.HoldScope{}
	}
	s := retention.HoldScope{
		TenantID: p.GetTenantId(),
		JobID:    p.GetJobId(),
	}
	if p.CreatedBefore != nil {
		before := p.GetCreatedBefore().AsTime()
		s.CreatedBefore = &before
	}
	return s
}

// policyConfigFromProto builds a config.RetentionConfig whose defaults carry the
// caller-supplied tier strings, for per-invocation policy override.
func policyConfigFromProto(p *pb.RetentionPolicy) config.RetentionConfig {
	return config.RetentionConfig{
		Defaults: config.RetentionTiers{
			Attestation:    p.GetAttestation(),
			JobResults:     p.GetJobResults(),
			Catalogs:       p.GetCatalogs(),
			Embeddings:     p.GetEmbeddings(),
			AuditDecisions: p.GetAuditDecisions(),
			AuditLLM:       p.GetAuditLlm(),
			AuditEvents:    p.GetAuditEvents(),
		},
	}
}
