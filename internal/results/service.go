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
// gateway's MappingsBackend; the composition root asserts that, because this
// package must not import internal/gateway.
type Service struct {
	reader   Reader
	logger   *slog.Logger
	tracer   trace.Tracer            // optional, nil-safe
	ops      metric.Int64Counter     // optional, nil-safe
	duration metric.Float64Histogram // optional, nil-safe
	returned metric.Int64Counter     // optional, nil-safe
}

// Option configures a Service.
type Option func(*Service)

// WithLogger sets the logger. The default is slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(s *Service) { s.logger = l }
}

// WithTelemetry enables tracing and metrics. Either argument may be nil.
// Instruments: results.requests.total, results.request.duration_ms and
// results.mappings.returned.total.
func WithTelemetry(t trace.Tracer, m metric.Meter) Option {
	return func(s *Service) {
		s.tracer = t
		if m == nil {
			return
		}
		var err error
		if s.ops, err = m.Int64Counter("results.requests.total",
			metric.WithDescription("ListMappings calls by status (ok or the Connect error code)")); err != nil {
			s.logger.Warn("failed to create results.requests.total counter", "error", err)
		}
		if s.duration, err = m.Float64Histogram("results.request.duration_ms",
			metric.WithDescription("ListMappings duration in milliseconds by status"), metric.WithUnit("ms")); err != nil {
			s.logger.Warn("failed to create results.request.duration_ms histogram", "error", err)
		}
		if s.returned, err = m.Int64Counter("results.mappings.returned.total",
			metric.WithDescription("Mappings returned by ListMappings")); err != nil {
			s.logger.Warn("failed to create results.mappings.returned.total counter", "error", err)
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

// ListMappings validates the request, reads one page of mappings and converts
// it. Nothing is read until every check passes.
func (s *Service) ListMappings(ctx context.Context, req *connect.Request[pb.ListJobMappingsRequest]) (*connect.Response[pb.ListJobMappingsResponse], error) {
	start := time.Now()
	msg := req.Msg
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "results.ListMappings")
	defer span.End()
	span.SetAttributes(
		attribute.String("tenant.id", msg.GetTenantContext().GetTenantId()),
		attribute.String("job.id", msg.GetJobId()),
		attribute.String("stage", msg.GetStage().String()),
	)

	resp, err := s.list(ctx, msg)

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
	if err != nil {
		return nil, err
	}
	if s.returned != nil {
		s.returned.Add(ctx, int64(len(resp.Mappings)))
	}
	return connect.NewResponse(resp), nil
}

func (s *Service) list(ctx context.Context, msg *pb.ListJobMappingsRequest) (*pb.ListJobMappingsResponse, error) {
	ctx, filter, err := s.buildFilter(ctx, msg)
	if err != nil {
		return nil, err
	}
	page, err := s.reader.ListMappings(ctx, filter)
	if err != nil {
		return nil, s.readError(ctx, filter.JobID, err)
	}
	return s.toResponse(ctx, page), nil
}

// buildFilter validates msg and returns the tenant-scoped context and Filter.
func (s *Service) buildFilter(ctx context.Context, msg *pb.ListJobMappingsRequest) (context.Context, Filter, error) {
	invalid := func(format string, args ...any) error {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(format, args...))
	}

	tenantID := msg.GetTenantContext().GetTenantId()
	if tenantID == "" {
		return ctx, Filter{}, invalid("tenant_context.tenant_id is required")
	}
	if ctxTenant, err := tenant.FromContext(ctx); err == nil && ctxTenant != tenantID {
		return ctx, Filter{}, connect.NewError(connect.CodePermissionDenied,
			errors.New("tenant_context does not match the authenticated tenant"))
	}
	ctx, err := tenant.WithTenant(ctx, tenantID)
	if err != nil {
		return ctx, Filter{}, invalid("invalid tenant_context.tenant_id: %v", err)
	}

	var stage Stage
	switch msg.GetStage() {
	case pb.MappingStage_MAPPING_STAGE_UNSPECIFIED, pb.MappingStage_MAPPING_STAGE_SYNTHESIZED:
		stage = StageSynthesized
	case pb.MappingStage_MAPPING_STAGE_ANALYZER:
		stage = StageAnalyzer
	default:
		return ctx, Filter{}, invalid("unknown stage %d; valid stages are: synthesized, analyzer", int32(msg.GetStage()))
	}

	if msg.GetJobId() == "" && (stage != StageSynthesized || msg.GetControlId() == "") {
		return ctx, Filter{}, invalid(`job_id is required; pass the job ID printed by "crosscodex run start" or "catalog import"`)
	}

	if c := msg.GetMinConfidence(); math.IsNaN(float64(c)) || c < 0 || c > 1 {
		return ctx, Filter{}, invalid("min_confidence must be between 0 and 1, got %v", c)
	}

	if msg.GetIncludeNonViable() && stage == StageAnalyzer {
		return ctx, Filter{}, invalid("include_non_viable applies only to the synthesized stage; analyzer mappings have no viability")
	}

	types, err := relationshipNames(msg.GetRelationshipTypes())
	if err != nil {
		return ctx, Filter{}, err
	}

	pageSize := int(msg.GetPagination().GetPageSize())
	switch {
	case pageSize == 0:
		pageSize = defaultPageSize
	case pageSize < 0 || pageSize > maxPageSize:
		return ctx, Filter{}, invalid("page_size must be between 1 and %d", maxPageSize)
	}
	after, err := DecodeCursor(msg.GetPagination().GetPageToken())
	if err != nil {
		return ctx, Filter{}, invalid("%v", err)
	}

	if after != nil && msg.GetJobId() != "" && after.JobID != msg.GetJobId() {
		return ctx, Filter{}, invalid("page_token belongs to a different job; pass the next_page_token from the previous response for this job unchanged")
	}

	// The wire type is float32. Widening 0.8f straight to float64 yields
	// 0.800000011920929, which would exclude stored rows at exactly 0.8, so
	// go through the shortest decimal that identifies the float32.
	minConfidence, err := strconv.ParseFloat(strconv.FormatFloat(float64(msg.GetMinConfidence()), 'g', -1, 32), 64)
	if err != nil {
		return ctx, Filter{}, invalid("min_confidence %v cannot be read as a number; pass a value between 0 and 1", msg.GetMinConfidence())
	}

	return ctx, Filter{
		TenantID:          tenantID,
		JobID:             msg.GetJobId(),
		Stage:             stage,
		ControlID:         msg.GetControlId(),
		MinConfidence:     minConfidence,
		IncludeNonViable:  msg.GetIncludeNonViable(),
		RelationshipTypes: types,
		PageSize:          pageSize,
		After:             after,
	}, nil
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

// readError maps a Reader error to the Connect error the client sees.
func (s *Service) readError(ctx context.Context, jobID string, err error) error {
	var notCompleted *JobNotCompletedError
	switch {
	case errors.Is(err, ErrJobNotFound):
		return connect.NewError(connect.CodeNotFound, fmt.Errorf(
			`job %q not found for this tenant. Check the job ID; "crosscodex run status <job-id>" shows a job you submitted`, jobID))
	case errors.Is(err, ErrInvalidPageToken):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.As(err, &notCompleted):
		return connect.NewError(connect.CodeFailedPrecondition, notCompletedMessage(notCompleted))
	default:
		return s.internalError(ctx, "ListMappings", err)
	}
}

// notCompletedMessage tells the client what to do next: wait for a job that
// is still in flight, resubmit one that ended without producing mappings.
func notCompletedMessage(e *JobNotCompletedError) error {
	switch e.Status {
	case "failed", "cancelled":
		return fmt.Errorf(`job %q is %s and will not produce mappings. See why with "crosscodex run status %s", then submit a new job with "crosscodex run start"`,
			e.JobID, e.Status, e.JobID)
	default:
		return fmt.Errorf(`job %q is %s; mappings are available once it completes. Check progress with "crosscodex run status %s"`,
			e.JobID, e.Status, e.JobID)
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
		resp.Mappings = append(resp.Mappings, cm)
	}
	if len(unknown) > 0 {
		s.logger.WarnContext(ctx, "results contain relationship types this build does not know; reported as UNSPECIFIED",
			"relationship_types", unknown)
	}
	if page.Next != nil {
		resp.PageInfo.NextPageToken = EncodeCursor(*page.Next)
	}
	return resp
}
