package cleaning

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/cleaning"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

const (
	// defaultPageSize and maxPageSize bound ListFindings pages as the
	// crosswalk listings do; a larger or negative page_size is refused, not
	// clamped.
	defaultPageSize = 50
	maxPageSize     = 1000

	runDenied = "running a cleaning pass needs the admin role. " +
		"Ask an operator to run it, or to grant you the admin role"
	readDenied = "reading cleaning findings needs the reviewer or admin role. " +
		"Ask an operator to grant you one of them"
	decideDenied = "recording a decision needs the reviewer role. Ask an operator to grant it to your " +
		"client certificate through auth.x509_mappings. An admin alone cannot decide findings"
)

// Engine is the part of *cleaning.Engine the handler uses.
type Engine interface {
	Preview(ctx context.Context, tenantID string, scope cleaning.Scope) (cleaning.Plan, error)
	Apply(ctx context.Context, tenantID string, scope cleaning.Scope, expectToken string) (cleaning.Plan, error)
	Resume(ctx context.Context, tenantID string) (int, error)
	Decide(ctx context.Context, tenantID string, r cleaning.DecideRequest) (cleaning.StoredFinding, string, error)
	ListFindings(ctx context.Context, tenantID string, f cleaning.FindingFilter) ([]cleaning.StoredFinding, string, error)
	GetFinding(ctx context.Context, tenantID, findingID string) (cleaning.StoredFinding, []cleaning.Decision, string, error)
}

var _ Engine = (*cleaning.Engine)(nil)

// Service implements crosscodexv1connect.EdgeCleaningServiceHandler.
type Service struct {
	crosscodexv1connect.UnimplementedEdgeCleaningServiceHandler

	engine Engine
}

var _ crosscodexv1connect.EdgeCleaningServiceHandler = (*Service)(nil)

// NewService returns a handler that runs every RPC on engine.
func NewService(engine Engine) *Service {
	return &Service{engine: engine}
}

// requireRole returns the caller's identity when it holds one of roles. No
// identity is Unauthenticated; an identity without the roles is
// PermissionDenied with denied as the message.
func requireRole(ctx context.Context, denied string, roles ...string) (*authn.Identity, error) {
	identity := authn.IdentityFromContext(ctx)
	if identity == nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	}
	if err := authn.RequireRole(*identity, roles...); err != nil {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New(denied))
	}
	return identity, nil
}

// scopeCtx checks the request's TenantContext against the tenant the auth
// interceptor placed in ctx and returns ctx scoped to that tenant, with the
// tenant itself. The engine is always called with this tenant, so a request
// can never name another tenant's data. The refusal names neither tenant.
func scopeCtx(ctx context.Context, tc *pb.TenantContext) (context.Context, string, error) {
	if tc.GetTenantId() == "" {
		return nil, "", connect.NewError(connect.CodeInvalidArgument, errors.New("tenant_context is required: set tenant_context.tenant_id to your tenant"))
	}
	ctxTenant, err := tenant.FromContext(ctx)
	if err != nil {
		return nil, "", connect.NewError(connect.CodeUnauthenticated, errors.New("no tenant in context"))
	}
	if ctxTenant != tc.GetTenantId() {
		return nil, "", connect.NewError(connect.CodePermissionDenied, cleaning.ErrTenantMismatch)
	}
	ctx, err = tenant.WithTenant(ctx, ctxTenant)
	if err != nil {
		return nil, "", fmt.Errorf("scope tenant: %w", err)
	}
	return ctx, ctxTenant, nil
}

func invalid(format string, args ...any) error {
	return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(format, args...))
}

// RunCleaning previews, applies or resumes a cleaning pass.
func (s *Service) RunCleaning(ctx context.Context, req *connect.Request[pb.RunCleaningRequest]) (*connect.Response[pb.RunCleaningResponse], error) {
	if _, err := requireRole(ctx, runDenied, authn.RoleAdmin); err != nil {
		return nil, err
	}
	ctx, tenantID, err := scopeCtx(ctx, req.Msg.GetTenantContext())
	if err != nil {
		return nil, err
	}

	mode := req.Msg.GetMode()
	if mode == pb.CleaningMode_CLEANING_MODE_RESUME {
		if sc := req.Msg.GetScope(); sc.GetSourceCatalogId() != "" || sc.GetTargetCatalogId() != "" {
			return nil, invalid("CLEANING_MODE_RESUME projects every recorded change and takes no scope: omit scope")
		}
		n, err := s.engine.Resume(ctx, tenantID)
		if err != nil {
			return nil, engineError(err)
		}
		projected, err := count32("projected findings", n)
		if err != nil {
			return nil, err
		}
		return connect.NewResponse(&pb.RunCleaningResponse{Projected: projected, Audit: audit(ctx)}), nil
	}

	var plan cleaning.Plan
	switch mode {
	case pb.CleaningMode_CLEANING_MODE_PREVIEW:
		scope, err := scopeFromProto(req.Msg.GetScope())
		if err != nil {
			return nil, err
		}
		if plan, err = s.engine.Preview(ctx, tenantID, scope); err != nil {
			return nil, engineError(err)
		}
	case pb.CleaningMode_CLEANING_MODE_APPLY:
		if req.Msg.GetExpectToken() == "" {
			return nil, invalid("expect_token is required for CLEANING_MODE_APPLY: preview first and pass its plan_token")
		}
		scope, err := scopeFromProto(req.Msg.GetScope())
		if err != nil {
			return nil, err
		}
		if plan, err = s.engine.Apply(ctx, tenantID, scope, req.Msg.GetExpectToken()); err != nil {
			return nil, engineError(err)
		}
	default:
		return nil, invalid("mode %s is not a cleaning mode: set CLEANING_MODE_PREVIEW to plan a pass, "+
			"CLEANING_MODE_APPLY with expect_token to write a previewed plan, or CLEANING_MODE_RESUME to finish an interrupted one", mode)
	}

	resp, err := planToProto(plan)
	if err != nil {
		return nil, err
	}
	resp.Audit = audit(ctx)
	return connect.NewResponse(resp), nil
}

// ListFindings returns one page of the tenant's findings.
func (s *Service) ListFindings(ctx context.Context, req *connect.Request[pb.ListFindingsRequest]) (*connect.Response[pb.ListFindingsResponse], error) {
	if _, err := requireRole(ctx, readDenied, authn.RoleReviewer, authn.RoleAdmin); err != nil {
		return nil, err
	}
	ctx, tenantID, err := scopeCtx(ctx, req.Msg.GetTenantContext())
	if err != nil {
		return nil, err
	}
	size := int(req.Msg.GetPagination().GetPageSize())
	if size < 0 || size > maxPageSize {
		return nil, invalid("page_size %d is out of range: use a value from 0 to %d (0 means the default)", size, maxPageSize)
	}
	if size == 0 {
		size = defaultPageSize
	}
	signal, status := cleaning.Signal(req.Msg.GetSignal()), cleaning.Status(req.Msg.GetStatus())
	if signal != "" && !signal.Valid() {
		return nil, invalid("signal %q is not a cleaning signal: use one of %s, or leave it empty for all", signal, joinValues(cleaning.Signals()))
	}
	if status != "" && !status.Valid() {
		return nil, invalid("status %q is not a finding status: use one of %s, or leave it empty for all", status, joinValues(cleaning.Statuses()))
	}
	scope, err := scopeFromProto(req.Msg.GetScope())
	if err != nil {
		return nil, err
	}

	found, next, err := s.engine.ListFindings(ctx, tenantID, cleaning.FindingFilter{
		Signal: signal, Status: status,
		SourceCatalog: scope.SourceCatalog, TargetCatalog: scope.TargetCatalog,
		PageSize: size, PageToken: req.Msg.GetPagination().GetPageToken(),
	})
	if err != nil {
		return nil, engineError(err)
	}
	resp := &pb.ListFindingsResponse{PageInfo: &pb.PageInfo{NextPageToken: next}}
	for _, f := range found {
		resp.Findings = append(resp.Findings, findingToProto(f))
	}
	return connect.NewResponse(resp), nil
}

// GetFinding returns one finding with its decisions and state token.
func (s *Service) GetFinding(ctx context.Context, req *connect.Request[pb.GetFindingRequest]) (*connect.Response[pb.GetFindingResponse], error) {
	if _, err := requireRole(ctx, readDenied, authn.RoleReviewer, authn.RoleAdmin); err != nil {
		return nil, err
	}
	ctx, tenantID, err := scopeCtx(ctx, req.Msg.GetTenantContext())
	if err != nil {
		return nil, err
	}
	if req.Msg.GetFindingId() == "" {
		return nil, invalid("finding_id is required: take it from ListFindings")
	}

	f, decisions, token, err := s.engine.GetFinding(ctx, tenantID, req.Msg.GetFindingId())
	if err != nil {
		return nil, engineError(err)
	}
	resp := &pb.GetFindingResponse{Finding: findingToProto(f), StateToken: token, Audit: audit(ctx)}
	for _, d := range decisions {
		resp.Decisions = append(resp.Decisions, decisionToProto(d))
	}
	return connect.NewResponse(resp), nil
}

// DecideFinding records the caller's decision on a finding.
func (s *Service) DecideFinding(ctx context.Context, req *connect.Request[pb.DecideFindingRequest]) (*connect.Response[pb.DecideFindingResponse], error) {
	identity, err := requireRole(ctx, decideDenied, authn.RoleReviewer)
	if err != nil {
		return nil, err
	}
	ctx, tenantID, err := scopeCtx(ctx, req.Msg.GetTenantContext())
	if err != nil {
		return nil, err
	}
	if req.Msg.GetFindingId() == "" {
		return nil, invalid("finding_id is required: take it from ListFindings")
	}
	if req.Msg.GetExpectStateToken() == "" {
		return nil, invalid("expect_state_token is required: read the finding with GetFinding and pass its state_token")
	}
	retire, err := refsFromProto(req.Msg.GetRetireVerdicts())
	if err != nil {
		return nil, err
	}

	f, decisionID, err := s.engine.Decide(ctx, tenantID, cleaning.DecideRequest{
		FindingID: req.Msg.GetFindingId(), Verdict: req.Msg.GetVerdict(), Rationale: req.Msg.GetRationale(),
		ExpectState: req.Msg.GetExpectStateToken(), RetireVerdicts: retire, Reviewer: identity.Subject,
	})
	if err != nil {
		return nil, engineError(err)
	}
	return connect.NewResponse(&pb.DecideFindingResponse{
		Finding: findingToProto(f), DecisionId: decisionID, StateToken: cleaning.StateToken(f), Audit: audit(ctx),
	}), nil
}

// mapped lists the engine sentinels a caller can act on and their codes,
// first match wins. Anything else is returned raw.
var mapped = []struct {
	sentinel error
	code     connect.Code
}{
	{cleaning.ErrProjectionIncomplete, connect.CodeFailedPrecondition},
	{cleaning.ErrTenantMismatch, connect.CodePermissionDenied},
	{cleaning.ErrFindingNotFound, connect.CodeNotFound},
	{cleaning.ErrInvalidScope, connect.CodeInvalidArgument},
	{cleaning.ErrInvalidDecision, connect.CodeInvalidArgument},
	{cleaning.ErrVerdictNotInFinding, connect.CodeInvalidArgument},
	{cleaning.ErrRetireNeedsVerdicts, connect.CodeInvalidArgument},
	{cleaning.ErrTokenMismatch, connect.CodeFailedPrecondition},
	{cleaning.ErrStateChanged, connect.CodeFailedPrecondition},
	{cleaning.ErrUnprojected, connect.CodeFailedPrecondition},
	{cleaning.ErrSigningDisabled, connect.CodeFailedPrecondition},
	{cleaning.ErrInvalidTransition, connect.CodeFailedPrecondition},
	{cleaning.ErrEdgeNotLive, connect.CodeFailedPrecondition},
	{cleaning.ErrRetireCapExceeded, connect.CodeFailedPrecondition},
	{cleaning.ErrFindingTooLarge, connect.CodeFailedPrecondition},
	{cleaning.ErrReviewerNotRegistered, connect.CodeFailedPrecondition},
	{cleaning.ErrReviewerDeleted, connect.CodeFailedPrecondition},
}

// engineError maps an engine error to the Connect error the caller gets. A
// mapped sentinel keeps the engine's message only when every leaf of err's
// tree is a mapped sentinel, so the whole text is the engine's; otherwise a
// leaf may be a driver error, and only the sentinel's own text is sent. A
// *cleaning.ProjectionError sends its Public text instead, which adds the
// count applied, the resume instruction and any recorded decision.
func engineError(err error) error {
	for _, m := range mapped {
		if !errors.Is(err, m.sentinel) {
			continue
		}
		msg := m.sentinel.Error()
		var pe *cleaning.ProjectionError
		switch {
		case onlySentinels(err):
			msg = err.Error()
		case errors.As(err, &pe):
			msg = pe.Public()
		}
		return connect.NewError(m.code, errors.New(msg))
	}
	return err
}

// onlySentinels reports whether every leaf of err's tree is a mapped
// sentinel. It trusts the wrappers to add only their own text, which holds
// for every engine error wrapping a mapped sentinel (they use %w, never %v).
func onlySentinels(err error) bool {
	switch e := err.(type) {
	case interface{ Unwrap() []error }:
		for _, inner := range e.Unwrap() {
			if !onlySentinels(inner) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		if inner := e.Unwrap(); inner != nil {
			return onlySentinels(inner)
		}
	}
	for _, m := range mapped {
		if err == m.sentinel { // a leaf, so identity, not errors.Is
			return true
		}
	}
	return false
}
