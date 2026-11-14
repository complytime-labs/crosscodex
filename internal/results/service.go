package results

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	analyzerresults "github.com/complytime-labs/crosscodex/pkg/analyzer/results"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

const (
	defaultPageSize = 50
	maxPageSize     = 1000
	// relationshipTypePrefix is the proto enum prefix; the analyzers name
	// relationship types without it.
	relationshipTypePrefix = "RELATIONSHIP_TYPE_"
	noRelationship         = "NO_RELATIONSHIP"
)

// Service serves stored pipeline mappings over Connect. It satisfies the
// gateway's MappingsBackend and CandidatesBackend; the composition root asserts
// that, because this package must not import internal/gateway.
type Service struct {
	reader   Reader
	logger   *slog.Logger
	tracer   trace.Tracer            // optional, nil-safe
	ops      metric.Int64Counter     // optional, nil-safe
	duration metric.Float64Histogram // optional, nil-safe
	returned metric.Int64Counter     // optional, nil-safe
	// candidatesReturned counts ListCandidates items, optional, nil-safe.
	candidatesReturned metric.Int64Counter
}

// Option configures a Service.
type Option func(*Service)

// WithLogger sets the logger. The default is slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(s *Service) { s.logger = l }
}

// WithTelemetry enables tracing and metrics. Either argument may be nil.
// Instruments: results.requests.total, results.request.duration_ms and
// results.mappings.returned.total and results.candidates.returned.total. The
// first two cover ListMappings and ListCandidates alike.
func WithTelemetry(t trace.Tracer, m metric.Meter) Option {
	return func(s *Service) {
		s.tracer = t
		if m == nil {
			return
		}
		var err error
		if s.ops, err = m.Int64Counter("results.requests.total",
			metric.WithDescription("ListMappings and ListCandidates calls by status (ok or the Connect error code)")); err != nil {
			s.logger.Warn("failed to create results.requests.total counter", "error", err)
		}
		if s.duration, err = m.Float64Histogram("results.request.duration_ms",
			metric.WithDescription("ListMappings and ListCandidates duration in milliseconds by status"), metric.WithUnit("ms")); err != nil {
			s.logger.Warn("failed to create results.request.duration_ms histogram", "error", err)
		}
		if s.returned, err = m.Int64Counter("results.mappings.returned.total",
			metric.WithDescription("Mappings returned by ListMappings")); err != nil {
			s.logger.Warn("failed to create results.mappings.returned.total counter", "error", err)
		}
		if s.candidatesReturned, err = m.Int64Counter("results.candidates.returned.total",
			metric.WithDescription("Candidates returned by ListCandidates")); err != nil {
			s.logger.Warn("failed to create results.candidates.returned.total counter", "error", err)
		}
	}
}

// NewService returns a Service reading from reader.
func NewService(reader Reader, opts ...Option) *Service {
	s := &Service{reader: reader, logger: slog.Default()}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// startCall opens the span for one RPC and returns the context to run it
// under plus a finish function that records the span status, the request
// counter and the duration histogram for the RPC's result.
func (s *Service) startCall(ctx context.Context, spanName string, attrs ...attribute.KeyValue) (context.Context, func(error)) {
	start := time.Now()
	ctx, span := telemetry.StartSpan(s.tracer, ctx, spanName)
	span.SetAttributes(attrs...)
	return ctx, func(err error) {
		defer span.End()
		status := "ok"
		if err != nil {
			status = connect.CodeOf(err).String()
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		} else {
			span.SetStatus(codes.Ok, "")
		}
		attrs := metric.WithAttributes(attribute.String("status", status))
		if s.ops != nil {
			s.ops.Add(ctx, 1, attrs)
		}
		if s.duration != nil {
			s.duration.Record(ctx, float64(time.Since(start).Microseconds())/1000, attrs)
		}
	}
}

// ListMappings validates the request, reads one page of mappings and converts
// it. Nothing is read until every check passes.
func (s *Service) ListMappings(ctx context.Context, req *connect.Request[pb.ListJobMappingsRequest]) (*connect.Response[pb.ListJobMappingsResponse], error) {
	msg := req.Msg
	ctx, finish := s.startCall(ctx, "results.ListMappings",
		attribute.String("tenant.id", msg.GetTenantContext().GetTenantId()),
		attribute.String("job.id", msg.GetJobId()),
		attribute.String("stage", msg.GetStage().String()),
	)

	resp, err := s.list(ctx, msg)
	finish(err)
	if err != nil {
		return nil, err
	}
	if s.returned != nil {
		s.returned.Add(ctx, int64(len(resp.Mappings)))
	}
	return connect.NewResponse(resp), nil
}

// ListCandidates validates the request, reads one page of a completed job's
// relationship candidates and converts it. Nothing is read until every check
// passes.
func (s *Service) ListCandidates(ctx context.Context, req *connect.Request[pb.ListJobCandidatesRequest]) (*connect.Response[pb.ListJobCandidatesResponse], error) {
	msg := req.Msg
	ctx, finish := s.startCall(ctx, "results.ListCandidates",
		attribute.String("tenant.id", msg.GetTenantContext().GetTenantId()),
		attribute.String("job.id", msg.GetJobId()),
	)

	resp, err := s.listCandidates(ctx, msg)
	finish(err)
	if err != nil {
		return nil, err
	}
	if s.candidatesReturned != nil {
		s.candidatesReturned.Add(ctx, int64(len(resp.Candidates)))
	}
	return connect.NewResponse(resp), nil
}

func (s *Service) listCandidates(ctx context.Context, msg *pb.ListJobCandidatesRequest) (*pb.ListJobCandidatesResponse, error) {
	ctx, err := scopeToTenant(ctx, msg.GetTenantContext().GetTenantId())
	if err != nil {
		return nil, err
	}
	if msg.GetJobId() == "" {
		return nil, invalidArgument(`job_id is required; pass the job ID printed by "crosscodex run start"`)
	}
	pageSize, after, err := pageParams(msg.GetPagination(), msg.GetJobId())
	if err != nil {
		return nil, err
	}
	filter := CandidateFilter{
		TenantID:  msg.GetTenantContext().GetTenantId(),
		JobID:     msg.GetJobId(),
		ControlID: msg.GetControlId(),
		PageSize:  pageSize,
		After:     after,
	}
	page, err := s.reader.ListCandidates(ctx, filter)
	if err != nil {
		return nil, s.readError(ctx, "ListCandidates", "candidates", filter.JobID, err)
	}
	return s.candidatesResponse(page), nil
}

func (s *Service) list(ctx context.Context, msg *pb.ListJobMappingsRequest) (*pb.ListJobMappingsResponse, error) {
	ctx, filter, err := s.buildFilter(ctx, msg)
	if err != nil {
		return nil, err
	}
	page, err := s.reader.ListMappings(ctx, filter)
	if err != nil {
		return nil, s.readError(ctx, "ListMappings", "mappings", filter.JobID, err)
	}
	return s.toResponse(ctx, page), nil
}

// buildFilter validates msg and returns the tenant-scoped context and Filter.
func (s *Service) buildFilter(ctx context.Context, msg *pb.ListJobMappingsRequest) (context.Context, Filter, error) {
	tenantID := msg.GetTenantContext().GetTenantId()
	ctx, err := scopeToTenant(ctx, tenantID)
	if err != nil {
		return ctx, Filter{}, err
	}
	var stage Stage
	switch msg.GetStage() {
	case pb.MappingStage_MAPPING_STAGE_UNSPECIFIED, pb.MappingStage_MAPPING_STAGE_SYNTHESIZED:
		stage = StageSynthesized
	case pb.MappingStage_MAPPING_STAGE_ANALYZER:
		stage = StageAnalyzer
	default:
		return ctx, Filter{}, invalidArgument("unknown stage %d; valid stages are: synthesized, analyzer", int32(msg.GetStage()))
	}

	if msg.GetJobId() == "" && (stage != StageSynthesized || msg.GetControlId() == "") {
		return ctx, Filter{}, invalidArgument(`job_id is required; pass the job ID printed by "crosscodex run start"`)
	}

	if c := msg.GetMinConfidence(); math.IsNaN(float64(c)) || c < 0 || c > 1 {
		return ctx, Filter{}, invalidArgument("min_confidence must be between 0 and 1, got %v", c)
	}

	if msg.GetIncludeNonViable() && stage == StageAnalyzer {
		return ctx, Filter{}, invalidArgument("include_non_viable applies only to the synthesized stage; analyzer mappings have no viability")
	}

	if msg.GetIncludeVetoed() && stage != StageAnalyzer {
		return ctx, Filter{}, invalidArgument("include_vetoed applies only to the analyzer stage: synthesis never receives vetoed pairs; request the analyzer stage to list them")
	}

	types, err := relationshipNames(msg.GetRelationshipTypes())
	if err != nil {
		return ctx, Filter{}, err
	}

	pageSize, after, err := pageParams(msg.GetPagination(), msg.GetJobId())
	if err != nil {
		return ctx, Filter{}, err
	}

	// The wire type is float32. Widening 0.8f straight to float64 yields
	// 0.800000011920929, which would exclude stored rows at exactly 0.8, so
	// go through the shortest decimal that identifies the float32.
	minConfidence, err := strconv.ParseFloat(strconv.FormatFloat(float64(msg.GetMinConfidence()), 'g', -1, 32), 64)
	if err != nil {
		return ctx, Filter{}, invalidArgument("min_confidence %v cannot be read as a number; pass a value between 0 and 1", msg.GetMinConfidence())
	}

	return ctx, Filter{
		TenantID:          tenantID,
		JobID:             msg.GetJobId(),
		Stage:             stage,
		ControlID:         msg.GetControlId(),
		MinConfidence:     minConfidence,
		IncludeNonViable:  msg.GetIncludeNonViable(),
		IncludeVetoed:     msg.GetIncludeVetoed(),
		RelationshipTypes: types,
		PageSize:          pageSize,
		After:             after,
	}, nil
}

func invalidArgument(format string, args ...any) error {
	return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(format, args...))
}

// scopeToTenant checks the request's tenant against the authenticated one and
// returns ctx scoped to it.
func scopeToTenant(ctx context.Context, tenantID string) (context.Context, error) {
	if tenantID == "" {
		return ctx, invalidArgument("tenant_context.tenant_id is required")
	}
	if ctxTenant, err := tenant.FromContext(ctx); err == nil && ctxTenant != tenantID {
		return ctx, connect.NewError(connect.CodePermissionDenied,
			errors.New("tenant_context does not match the authenticated tenant"))
	}
	ctx, err := tenant.WithTenant(ctx, tenantID)
	if err != nil {
		return ctx, invalidArgument("invalid tenant_context.tenant_id: %v", err)
	}
	return ctx, nil
}

// pageParams validates the page size and token. A token must belong to jobID
// when the request names one.
func pageParams(p *pb.Pagination, jobID string) (int, *Cursor, error) {
	pageSize := int(p.GetPageSize())
	switch {
	case pageSize == 0:
		pageSize = defaultPageSize
	case pageSize < 0 || pageSize > maxPageSize:
		return 0, nil, invalidArgument("page_size must be between 1 and %d", maxPageSize)
	}
	after, err := DecodeCursor(p.GetPageToken())
	if err != nil {
		return 0, nil, invalidArgument("%v", err)
	}
	if after != nil && jobID != "" && after.JobID != jobID {
		return 0, nil, invalidArgument("page_token belongs to a different job; pass the next_page_token from the previous response for this job unchanged")
	}
	return pageSize, after, nil
}

// relationshipNames returns the analyzer names for requested. An empty list
// means every type except NO_RELATIONSHIP, read from the enum so new values
// are included automatically.
func relationshipNames(requested []pb.RelationshipType) ([]string, error) {
	valid := make([]string, 0, len(pb.RelationshipType_name))
	for n, name := range pb.RelationshipType_name {
		if n != int32(pb.RelationshipType_RELATIONSHIP_TYPE_UNSPECIFIED) {
			valid = append(valid, strings.TrimPrefix(name, relationshipTypePrefix))
		}
	}
	sort.Strings(valid)

	if len(requested) == 0 {
		names := make([]string, 0, len(valid))
		for _, name := range valid {
			if name != noRelationship {
				names = append(names, name)
			}
		}
		return names, nil
	}
	names := make([]string, 0, len(requested))
	for _, t := range requested {
		name, ok := pb.RelationshipType_name[int32(t)]
		if !ok || t == pb.RelationshipType_RELATIONSHIP_TYPE_UNSPECIFIED {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(
				"unknown relationship type %d in relationship_types; valid values are: %s", int32(t), strings.Join(valid, ", ")))
		}
		names = append(names, strings.TrimPrefix(name, relationshipTypePrefix))
	}
	return names, nil
}

// readError maps a Reader error from method to the Connect error the client
// sees. noun names what the method lists, for the not-completed message.
func (s *Service) readError(ctx context.Context, method, noun, jobID string, err error) error {
	var notCompleted *JobNotCompletedError
	switch {
	case errors.Is(err, ErrJobNotFound):
		return connect.NewError(connect.CodeNotFound, fmt.Errorf(
			`job %q not found for this tenant. Check the job ID; "crosscodex run status <job-id>" shows a job you submitted`, jobID))
	case errors.Is(err, ErrInvalidPageToken):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.As(err, &notCompleted):
		return connect.NewError(connect.CodeFailedPrecondition, notCompletedMessage(noun, notCompleted))
	default:
		return s.internalError(ctx, method, err)
	}
}

// notCompletedMessage tells the client what to do next: wait for a job that
// is still in flight, resubmit one that ended without producing mappings.
func notCompletedMessage(noun string, e *JobNotCompletedError) error {
	switch e.Status {
	case "failed", "cancelled":
		return fmt.Errorf(`job %q is %s and will not produce %s. See why with "crosscodex run status %s", then submit a new job with "crosscodex run start"`,
			e.JobID, e.Status, noun, e.JobID)
	default:
		return fmt.Errorf(`job %q is %s; %s are available once it completes. Check progress with "crosscodex run status %s"`,
			e.JobID, e.Status, noun, e.JobID)
	}
}

// internalError turns an error the client cannot fix into the Connect error
// an RPC returns. Reader errors carry SQL, SQLSTATE codes and table names that
// must not reach a client (CWE-209), so the client gets a fixed message
// pointing at the trace and the full error is logged here, once, because the
// log is then the only record of it.
func (s *Service) internalError(ctx context.Context, method string, err error) *connect.Error {
	traceID := telemetry.TraceIDFromContext(ctx)
	s.logger.ErrorContext(ctx, "results RPC failed", "method", method, "trace_id", traceID, "error", err)
	if traceID == "" {
		return connect.NewError(connect.CodeInternal, fmt.Errorf(
			"internal results error; the server logged the cause. Retry the request, and if it keeps failing, give an operator the time of the failure and the method name (%s)", method))
	}
	return connect.NewError(connect.CodeInternal, fmt.Errorf(
		"internal results error; the server logged the cause under trace %s. Retry the request, and if it keeps failing, give that trace ID to an operator", traceID))
}

// toResponse converts a Page to its wire form. A relationship name this build
// does not know is reported as UNSPECIFIED and warned about once per call.
func (s *Service) toResponse(ctx context.Context, page Page) *pb.ListJobMappingsResponse {
	resp := &pb.ListJobMappingsResponse{
		Mappings: make([]*pb.ControlMapping, 0, len(page.Mappings)),
		PageInfo: &pb.PageInfo{},
	}
	var unknown []string
	for _, m := range page.Mappings {
		rel, ok := pb.RelationshipType_value[relationshipTypePrefix+m.RelationshipType]
		if !ok && !slices.Contains(unknown, m.RelationshipType) {
			unknown = append(unknown, m.RelationshipType)
		}
		cm := &pb.ControlMapping{
			MappingId:        graphdb.DerivedID(analyzerresults.SemanticMatchEdgeIDKind, m.JobID, m.SourceID, m.TargetID),
			SourceControlId:  m.SourceID,
			TargetControlId:  m.TargetID,
			RelationshipType: pb.RelationshipType(rel),
			Confidence:       float32(m.Confidence),
		}
		if m.HasViability {
			cm.ViabilityScore = float32(m.Viability)
			cm.IsViable = m.Viability > 0
		}
		if c := m.Critique; c != nil {
			// The store refuses counts outside [0, MaxInt32] as a corrupt
			// critique result, so the conversions cannot overflow.
			cm.Critique = &pb.MappingCritique{Vetoed: c.Vetoed, Upheld: int32(c.Upheld), Rejected: int32(c.Rejected),
				Failed: int32(c.Failed), Reasons: c.Reasons}
		}
		resp.Mappings = append(resp.Mappings, cm)
	}
	if len(unknown) > 0 {
		s.logger.WarnContext(ctx, "results contain relationship types this build does not know; reported as UNSPECIFIED",
			"relationship_types", unknown)
	}
	resp.Analysis = analysisResponse(page.Analysis)
	if page.Next != nil {
		resp.PageInfo.NextPageToken = EncodeCursor(*page.Next)
	}
	return resp
}

// candidatesResponse converts a CandidatePage to its wire form.
func (s *Service) candidatesResponse(page CandidatePage) *pb.ListJobCandidatesResponse {
	resp := &pb.ListJobCandidatesResponse{
		Candidates: make([]*pb.RelationshipCandidate, 0, len(page.Candidates)),
		PageInfo:   &pb.PageInfo{},
		Analysis:   analysisResponse(page.Analysis),
	}
	for _, c := range page.Candidates {
		rc := &pb.RelationshipCandidate{
			SourceControlId: c.SourceID,
			TargetControlId: c.TargetID,
			SimilarityScore: float32(c.Similarity),
		}
		for _, contribution := range c.Contributions {
			rc.Contributions = append(rc.Contributions, &pb.CandidateContribution{
				Generator: contribution.Generator,
				Score:     contribution.Score,
				Weight:    contribution.Weight,
				Metadata:  contribution.Metadata,
			})
		}
		resp.Candidates = append(resp.Candidates, rc)
	}
	if page.Next != nil {
		resp.PageInfo.NextPageToken = EncodeCursor(*page.Next)
	}
	return resp
}

// analysisResponse converts a job's catalog selection; nil stays unset.
func analysisResponse(a *JobAnalysis) *pb.JobAnalysis {
	if a == nil {
		return nil
	}
	summary := func(c CatalogSummary) *pb.CatalogSummary {
		return &pb.CatalogSummary{CatalogId: c.ID, Name: c.Name, Version: c.Version, Imported: c.Imported, Snapshot: c.Snapshot}
	}
	return &pb.JobAnalysis{SourceCatalog: summary(a.Source), TargetCatalog: summary(a.Target), Bidirectional: a.Bidirectional}
}
