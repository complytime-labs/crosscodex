package feedback

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/cleaning"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

const (
	voteDenied = "voting on a mapping needs the reviewer role. Ask an operator to grant it to your " +
		"client certificate through auth.x509_mappings"
	queueDenied = "reading the review queue needs the reviewer or admin role. " +
		"Ask an operator to grant you one of them"
	determineDenied = "recording a mapping determination needs the reviewer role. Ask an operator to grant it to your " +
		"client certificate through auth.x509_mappings. An admin alone cannot record determinations"
)

// Sentinels the service returns, wrapped with an actionable message.
var (
	// ErrDeterminationsUnavailable marks a determination or re-projection
	// asked of a service built without WithDeterminations: it has no graph to
	// change. It maps to FailedPrecondition, whose message the gateway passes
	// through (it withholds Unavailable messages), so the caller reads which
	// daemon to use.
	ErrDeterminationsUnavailable = errors.New("mapping determinations need the crosscodexd daemon (role all, or gateway with a graph role)")
	// ErrAlreadyInvalid marks an invalid determination on a mapping whose
	// review is already invalid.
	ErrAlreadyInvalid = errors.New("mapping is already invalid")
	// ErrNoLiveEdge marks an invalid determination on a mapping whose verdict
	// has no live edge left to retire.
	ErrNoLiveEdge = errors.New("mapping has no live edge in the graph")
	// ErrEdgesGone marks a restore whose retired edges a graph rebuild
	// removed: they can no longer be recreated.
	ErrEdgesGone = errors.New("mapping's retired edges are no longer in the graph")
	// ErrSigningDisabled marks a determination or re-projection refused
	// because no attestation signer is configured.
	ErrSigningDisabled = errors.New("mapping determinations are signed; set attestation.private_key_path and attestation.public_key_path")
)

// Verdicts reads a verdict's live edges, as cleaning does; *cleaning.Engine
// satisfies it. Every key's job must be a completed job of the tenant, else
// the error wraps cleaning.ErrVerdictJobNotFound.
type Verdicts interface {
	VerdictEdges(ctx context.Context, tenantID string, keys []cleaning.VerdictKey) (map[cleaning.VerdictKey][]string, error)
}

var _ Verdicts = (*cleaning.Engine)(nil)

// Service serves mapping feedback: votes, the review queue and
// determinations (issue #230). It implements
// crosscodexv1connect.FeedbackServiceHandler and the gateway's feedback
// backend. GetVotes, GetUserVotes and EscalateMapping stay Unimplemented:
// the spec leaves them out (escalation is a vote type, VOTE_TYPE_ESCALATE).
//
// The service is the role gate: the store reads a job's mappings
// tenant-wide, so every RPC checks the caller's role before any read.
type Service struct {
	crosscodexv1connect.UnimplementedFeedbackServiceHandler

	store    Store
	cfg      config.FeedbackConfig
	g        graphdb.GraphDB
	verdicts Verdicts
	signer   attestation.Generator
	now      func() time.Time
	tel      serviceTelemetry
}

var _ crosscodexv1connect.FeedbackServiceHandler = (*Service)(nil)

// Option configures a Service.
type Option func(*Service)

// WithDeterminations enables RecordDetermination and Reproject over the
// graph g, reading verdicts' live edges through v and signing each
// determination with signer. The crosscodexd daemon passes its graph, its
// cleaning engine and its attestation signer; the embedded daemon passes
// nothing, so determinations are refused there with
// ErrDeterminationsUnavailable. A nil signer refuses every write with
// ErrSigningDisabled; a dry-run Reproject still works.
func WithDeterminations(g graphdb.GraphDB, v Verdicts, signer attestation.Generator) Option {
	return func(s *Service) {
		s.g, s.verdicts, s.signer = g, v, signer
	}
}

// WithClock sets the clock that times votes and determinations; the default
// is time.Now. Specs pass a fixed clock.
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// NewService returns a feedback service over store, with cfg's review
// thresholds.
func NewService(store Store, cfg config.FeedbackConfig, opts ...Option) *Service {
	s := &Service{store: store, cfg: cfg, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// stamp is the service's current time as the store keeps it: UTC at
// microsecond precision, so a time read back equals the one signed.
func (s *Service) stamp() time.Time {
	return s.now().Round(time.Microsecond).UTC()
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
// tenant itself. The store is always called with this tenant, so a request
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
	setTenant(ctx, ctxTenant)
	return ctx, ctxTenant, nil
}

func invalid(format string, args ...any) error {
	return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(format, args...))
}

// checkRequestIDs refuses a request without its job and mapping IDs, or with
// IDs that are not text. A mapping ID is derived from a kind and three parts
// of at most maxIDBytes each, so it is bounded at four times that.
func checkRequestIDs(jobID, mappingID string) error {
	if jobID == "" {
		return invalid("job_id is required: name the completed analysis job whose mapping this is")
	}
	if mappingID == "" {
		return invalid("mapping_id is required: take it from 'crosscodex results export <job>'")
	}
	if err := checkText("job_id", jobID, maxIDBytes, false); err != nil {
		return invalid("%w", err)
	}
	if err := checkText("mapping_id", mappingID, 4*maxIDBytes, false); err != nil {
		return invalid("%w", err)
	}
	return nil
}

// lookupMapping returns the job's mapping mappingID. An unknown job wraps
// ErrJobNotFound, a job not completed is a *JobNotCompletedError, and a
// mapping that is not the job's wraps ErrMappingNotFound.
func (s *Service) lookupMapping(ctx context.Context, tenantID, jobID, mappingID string) (Mapping, error) {
	ms, err := s.store.JobMappings(ctx, tenantID, jobID)
	switch {
	case errors.Is(err, ErrJobNotFound):
		return Mapping{}, fmt.Errorf("job %s is not one of your tenant's analysis jobs, so it has no mapping %s; check the job ID: %w",
			jobID, mappingID, ErrJobNotFound)
	case err != nil:
		return Mapping{}, fmt.Errorf("read job %s's mappings: %w", jobID, err)
	}
	for _, m := range ms {
		if m.ID == mappingID {
			return m, nil
		}
	}
	return Mapping{}, mappingNotFound(mappingID, jobID)
}

// relationshipName returns a pb.RelationshipType's name without its prefix,
// "" for UNSPECIFIED, and false for a number this build does not know.
func relationshipName(t pb.RelationshipType) (string, bool) {
	if t == pb.RelationshipType_RELATIONSHIP_TYPE_UNSPECIFIED {
		return "", true
	}
	name, ok := pb.RelationshipType_name[int32(t)]
	return strings.TrimPrefix(name, relationshipTypePrefix), ok
}

// relationshipType returns the pb.RelationshipType of a stored type name;
// a name this build does not know is UNSPECIFIED.
func relationshipType(name string) pb.RelationshipType {
	return pb.RelationshipType(pb.RelationshipType_value[relationshipTypePrefix+name])
}

var voteTypeFromProto = map[pb.VoteType]VoteType{
	pb.VoteType_VOTE_TYPE_AGREE:    VoteAgree,
	pb.VoteType_VOTE_TYPE_DISAGREE: VoteDisagree,
	pb.VoteType_VOTE_TYPE_REFINE:   VoteRefine,
	pb.VoteType_VOTE_TYPE_ESCALATE: VoteEscalate,
}

// SubmitVote records the caller's vote on one of a completed job's mappings
// and returns the mapping's aggregation with it counted.
func (s *Service) SubmitVote(ctx context.Context, req *connect.Request[pb.SubmitVoteRequest]) (_ *connect.Response[pb.SubmitVoteResponse], err error) {
	ctx, finish := s.begin(ctx, "SubmitVote", "submit_vote")
	defer func() { finish(err) }()

	identity, err := requireRole(ctx, voteDenied, authn.RoleReviewer)
	if err != nil {
		return nil, err
	}
	ctx, tenantID, err := scopeCtx(ctx, req.Msg.GetTenantContext())
	if err != nil {
		return nil, err
	}
	msg := req.Msg
	if err := checkRequestIDs(msg.GetJobId(), msg.GetMappingId()); err != nil {
		return nil, err
	}
	vt, ok := voteTypeFromProto[msg.GetVoteType()]
	if !ok {
		return nil, invalid("vote_type %s is not a vote: set VOTE_TYPE_AGREE, VOTE_TYPE_DISAGREE, "+
			"VOTE_TYPE_REFINE (with suggested_type) or VOTE_TYPE_ESCALATE", msg.GetVoteType())
	}
	suggested, ok := relationshipName(msg.GetSuggestedType())
	if !ok {
		return nil, invalid("suggested_type %d is not a relationship type this server knows; use one of the RelationshipType values", msg.GetSuggestedType())
	}

	m, err := s.lookupMapping(ctx, tenantID, msg.GetJobId(), msg.GetMappingId())
	if err != nil {
		return nil, serviceError(err)
	}
	v := Vote{
		ID: uuid.NewString(), MappingID: m.ID, JobID: m.JobID, SourceID: m.SourceID, TargetID: m.TargetID,
		Voter: identity.Subject, Type: vt, SuggestedType: suggested, Rationale: msg.GetRationale(),
		Evidence: msg.GetEvidence(), CreatedAt: s.stamp(),
	}
	// Validated here as well as in the store so the caller gets the whole
	// message: it is built from the request alone.
	if err := v.Validate(); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err := s.store.RecordVote(ctx, tenantID, v); err != nil {
		return nil, serviceError(fmt.Errorf("record vote on mapping %s: %w", m.ID, err))
	}
	if s.tel.votes != nil {
		s.tel.votes.Add(ctx, 1, metric.WithAttributes(attribute.String("vote_type", string(vt))))
	}

	agg, err := s.aggregation(ctx, tenantID, m)
	if err != nil {
		return nil, fmt.Errorf("vote %s was recorded; aggregate mapping %s's votes: %w", v.ID, m.ID, err)
	}
	pa, err := aggregationToProto(m.ID, agg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&pb.SubmitVoteResponse{VoteId: v.ID, Aggregation: pa}), nil
}

// mapped lists the sentinels a caller can act on and their codes, first
// match wins. Anything else is returned raw, and the gateway withholds it.
var mapped = []struct {
	sentinel error
	code     connect.Code
}{
	{cleaning.ErrTenantMismatch, connect.CodePermissionDenied},
	{ErrInvalid, connect.CodeInvalidArgument},
	{ErrJobNotFound, connect.CodeNotFound},
	{ErrMappingNotFound, connect.CodeNotFound},
	{cleaning.ErrVerdictJobNotFound, connect.CodeNotFound},
	{ErrDeterminationsUnavailable, connect.CodeFailedPrecondition},
	{ErrReviewChanged, connect.CodeAborted},
	{ErrUnprojected, connect.CodeFailedPrecondition},
	{ErrAlreadyInvalid, connect.CodeFailedPrecondition},
	{ErrNoLiveEdge, connect.CodeFailedPrecondition},
	{ErrEdgesGone, connect.CodeFailedPrecondition},
	{ErrSigningDisabled, connect.CodeFailedPrecondition},
	{cleaning.ErrReviewerNotRegistered, connect.CodeFailedPrecondition},
	{cleaning.ErrReviewerDeleted, connect.CodeFailedPrecondition},
}

// serviceError maps an error to the Connect error the caller gets. A
// *connect.Error is already mapped and returned as is. A
// *JobNotCompletedError is FailedPrecondition with its own text. A mapped
// sentinel keeps the whole message only when every leaf of err's tree is a
// mapped sentinel, so the whole text is this package's; otherwise a leaf may
// be a driver error, and only the sentinel's own text is sent.
func serviceError(err error) error {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return err
	}
	var nc *JobNotCompletedError
	if errors.As(err, &nc) {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New(nc.Error()))
	}
	for _, m := range mapped {
		if !errors.Is(err, m.sentinel) {
			continue
		}
		msg := m.sentinel.Error()
		if onlySentinels(err) {
			msg = err.Error()
		}
		return connect.NewError(m.code, errors.New(msg))
	}
	return err
}

// onlySentinels reports whether every leaf of err's tree is a mapped
// sentinel. It trusts the wrappers to add only their own text, which holds
// for every error here wrapping a mapped sentinel (they use %w, never %v).
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
