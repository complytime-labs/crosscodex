package gateway

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel/attribute"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/jobconfig"
)

// crosswalkMaxPageSize is the largest page a crosswalk request may ask for,
// and the page size the gateway uses to list the tenant's completed jobs.
const crosswalkMaxPageSize = 1000

// crosswalkJob is a completed job with its parsed catalog selection.
type crosswalkJob struct {
	msg  *pb.CrosswalkJob
	cats jobconfig.Catalogs
}

// GetCrosswalk validates the mode and filters, resolves the catalogs the mode
// names, selects the tenant's completed jobs the mode draws on and hands the
// graph walk to the crosswalk backend.
func (s *Service) GetCrosswalk(ctx context.Context, req *connect.Request[pb.GetCrosswalkRequest]) (*connect.Response[pb.GetCrosswalkResponse], error) {
	start := time.Now()
	identity := identityFromContext(ctx)
	if identity == nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	}

	mode := crosswalkModeName(req.Msg)
	ctx, endSpan := s.startHandlerSpan(ctx, "GetCrosswalk", identity,
		attribute.String("crosswalk.mode", mode))
	defer endSpan()

	if err := validateCrosswalkRequest(req.Msg); err != nil {
		return nil, err
	}

	if s.crosswalk == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("crosswalk backend not configured"))
	}
	if s.catalog == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("catalog backend not configured"))
	}
	if s.pipeline == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("pipeline backend not configured"))
	}

	tc := buildTenantContext(identity)

	resp, err := s.crosswalkViaBackend(ctx, identity, tc, req)
	if err != nil {
		err = s.publicError(ctx, "GetCrosswalk", err)
		s.recordMetrics(ctx, "GetCrosswalk", start, connect.CodeOf(err))
		return nil, err
	}

	s.recordMetrics(ctx, "GetCrosswalk", start, connect.Code(0))
	return resp, nil
}

// crosswalkViaBackend resolves the request's catalogs and jobs, overwrites the
// gateway-owned request fields and calls the backend. Its errors are
// unsanitized: GetCrosswalk passes them through publicError.
func (s *Service) crosswalkViaBackend(ctx context.Context, identity *authn.Identity, tc *pb.TenantContext, req *connect.Request[pb.GetCrosswalkRequest]) (*connect.Response[pb.GetCrosswalkResponse], error) {
	msg := req.Msg
	named := crosswalkNamedCatalogs(msg)

	summaries := make(map[string]*pb.CatalogSummary)
	for _, id := range named {
		sum, err := s.lookupCrosswalkCatalog(ctx, identity, tc, id, true)
		if err != nil {
			return nil, err
		}
		summaries[id] = sum
	}

	completed, err := s.listCompletedJobs(ctx, tc)
	if err != nil {
		return nil, err
	}
	selected, err := selectCrosswalkJobs(msg, completed)
	if err != nil {
		return nil, err
	}

	// The catalogs the jobs reach beyond the named ones: the sides of a job
	// in job mode, the other catalogs in one-to-many mode.
	var extra []string
	for _, j := range selected {
		for _, id := range j.cats.CatalogIDs() {
			if !slices.Contains(named, id) && !slices.Contains(extra, id) {
				extra = append(extra, id)
			}
		}
	}
	slices.Sort(extra)
	for _, id := range extra {
		sum, err := s.lookupCrosswalkCatalog(ctx, identity, tc, id, false)
		if err != nil {
			return nil, err
		}
		summaries[id] = sum
	}

	msg.TenantContext = tc
	msg.Jobs = make([]*pb.CrosswalkJob, 0, len(selected))
	for _, j := range selected {
		msg.Jobs = append(msg.Jobs, j.msg)
	}
	msg.Catalogs = make([]*pb.CatalogSummary, 0, len(summaries))
	for _, id := range append(slices.Clone(named), extra...) {
		sum := summaries[id]
		applyNewestSnapshot(sum, selected)
		msg.Catalogs = append(msg.Catalogs, sum)
	}

	return s.crosswalk.GetCrosswalk(ctx, req)
}

// lookupCrosswalkCatalog reads one catalog's current name and version. With
// mustExist, a NotFound is replaced by one fixed message, so a missing catalog
// and another tenant's read the same and the error is no existence oracle;
// without it, a catalog the tenant no longer has comes back with imported
// false (a job can outlive its catalog).
func (s *Service) lookupCrosswalkCatalog(ctx context.Context, identity *authn.Identity, tc *pb.TenantContext, id string, mustExist bool) (*pb.CatalogSummary, error) {
	resp, err := s.catalog.GetCatalog(ctx, connect.NewRequest(&pb.GetCatalogRequest{TenantContext: tc, CatalogId: id}))
	if err == nil {
		return &pb.CatalogSummary{
			CatalogId: id,
			Name:      resp.Msg.GetCatalog().GetName(),
			Version:   resp.Msg.GetCatalog().GetVersion(),
			Imported:  true,
		}, nil
	}
	if connect.CodeOf(err) != connect.CodeNotFound {
		return nil, err
	}
	if !mustExist {
		return &pb.CatalogSummary{CatalogId: id}, nil
	}
	// The caller gets the fixed message only; the backend's is kept here,
	// once, for an operator. Every field is the caller's own: their tenant
	// and the ID they sent.
	s.logger.DebugContext(ctx, "catalog lookup returned NotFound; answering with the fixed message",
		"method", "GetCrosswalk", "tenant", identity.TenantID, "catalog_id", id, "error", err)
	return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf(
		"catalog %q not found: your tenant has imported no catalog with that ID. "+
			"Run \"crosscodex catalog list\" to find the ID of an imported catalog", id))
}

// listCompletedJobs returns every completed job of the tenant, oldest
// completion first (ties by job ID). It calls the pipeline backend, not the
// ListJobs handler: crosswalk data is tenant-scoped, not owner-scoped, as the
// mappings are.
func (s *Service) listCompletedJobs(ctx context.Context, tc *pb.TenantContext) ([]crosswalkJob, error) {
	var jobs []crosswalkJob
	token := ""
	for {
		resp, err := s.pipeline.ListJobs(ctx, connect.NewRequest(&pb.ListJobsRequest{
			TenantContext: tc,
			Status:        pb.JobStatus_JOB_STATUS_COMPLETED,
			Options: &pb.ListOptions{Pagination: &pb.Pagination{
				PageSize:  crosswalkMaxPageSize,
				PageToken: token,
			}},
		}))
		if err != nil {
			return nil, err
		}
		for _, job := range resp.Msg.GetJobs() {
			if job.GetStatus() != pb.JobStatus_JOB_STATUS_COMPLETED {
				continue
			}
			cj, ok, err := newCrosswalkJob(job)
			if err != nil {
				return nil, err
			}
			if ok {
				jobs = append(jobs, cj)
			}
		}
		next := resp.Msg.GetPageInfo().GetNextPageToken()
		if next == "" {
			break
		}
		if next == token {
			return nil, fmt.Errorf("pipeline backend returned page token %q again; the job listing is not advancing", next)
		}
		token = next
	}
	slices.SortFunc(jobs, func(a, b crosswalkJob) int {
		if c := compareTimestamps(a.msg.GetCompletedAt(), b.msg.GetCompletedAt()); c != 0 {
			return c
		}
		return cmp.Compare(a.msg.GetJobId(), b.msg.GetJobId())
	})
	return jobs, nil
}

// compareTimestamps orders timestamps, an unset one first.
func compareTimestamps(a, b *timestamppb.Timestamp) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	}
	return a.AsTime().Compare(b.AsTime())
}

// newCrosswalkJob reads a job's catalog selection with the shared jobconfig
// reader, which parses the JSON form the pipeline stores (proto field names).
// ok is false for a job whose config names no catalog: it cannot match a mode.
func newCrosswalkJob(job *pb.PipelineJob) (crosswalkJob, bool, error) {
	stored, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(job.GetConfig())
	if err != nil {
		return crosswalkJob{}, false, fmt.Errorf("encoding the config of job %q: %w", job.GetJobId(), err)
	}
	cats, ok := jobconfig.ExtractCatalogs(stored)
	if !ok {
		return crosswalkJob{}, false, nil
	}
	var completedAt *timestamppb.Timestamp
	for _, stage := range job.GetStages() {
		if t := stage.GetCompletedAt(); t != nil && compareTimestamps(t, completedAt) > 0 {
			completedAt = t
		}
	}
	return crosswalkJob{
		msg: &pb.CrosswalkJob{
			JobId:           job.GetJobId(),
			SourceCatalogId: cats.SourceID,
			TargetCatalogId: cats.TargetID,
			Bidirectional:   cats.Bidirectional,
			CompletedAt:     completedAt,
		},
		cats: cats,
	}, true, nil
}

// selectCrosswalkJobs keeps the completed jobs the request's mode draws on,
// in the order given. A job-mode request for a job that is not among the
// tenant's completed jobs (unknown, other tenant's, or not completed) gets one
// fixed NotFound.
func selectCrosswalkJobs(msg *pb.GetCrosswalkRequest, completed []crosswalkJob) ([]crosswalkJob, error) {
	var selected []crosswalkJob
	for _, j := range completed {
		if crosswalkJobMatches(msg, j.cats) || (msg.GetJobId() != "" && j.msg.GetJobId() == msg.GetJobId()) {
			selected = append(selected, j)
		}
	}
	if msg.GetJobId() != "" && len(selected) == 0 {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf(
			"job %q not found: your tenant has no completed job with that ID. "+
				"Only completed jobs can be exported; check the job's status, or crosswalk its catalogs instead", msg.GetJobId()))
	}
	return selected, nil
}

// crosswalkJobMatches reports whether a job's catalogs fall under the
// request's catalog modes. Job mode matches by ID, not here.
func crosswalkJobMatches(msg *pb.GetCrosswalkRequest, c jobconfig.Catalogs) bool {
	connects := func(x, y string) bool {
		return c.CrossCatalog() && (c.SourceID == x && c.TargetID == y || c.SourceID == y && c.TargetID == x)
	}
	switch m := msg.GetMode().(type) {
	case *pb.GetCrosswalkRequest_Pair:
		return connects(m.Pair.GetCatalogA(), m.Pair.GetCatalogB())
	case *pb.GetCrosswalkRequest_OneToManyCatalogId:
		return c.CrossCatalog() && (c.SourceID == m.OneToManyCatalogId || c.TargetID == m.OneToManyCatalogId)
	case *pb.GetCrosswalkRequest_Transitive:
		t := m.Transitive
		return connects(t.GetCatalogA(), t.GetHubCatalogId()) ||
			connects(t.GetHubCatalogId(), t.GetCatalogC()) ||
			connects(t.GetCatalogA(), t.GetCatalogC())
	}
	return false
}

// applyNewestSnapshot sets sum's name and version to the ones the most recent
// contributing job recorded for the catalog, and marks it a snapshot. A
// catalog no selected job recorded a label for keeps its current name.
func applyNewestSnapshot(sum *pb.CatalogSummary, selected []crosswalkJob) {
	for _, j := range slices.Backward(selected) {
		if label := j.cats.LabelOf(sum.GetCatalogId()); label.Recorded {
			sum.Name, sum.Version, sum.Snapshot = label.Name, label.Version, true
			return
		}
	}
}

// crosswalkNamedCatalogs lists the catalogs the request names, which must
// exist: none in job mode, whose catalogs come from the job.
func crosswalkNamedCatalogs(msg *pb.GetCrosswalkRequest) []string {
	switch m := msg.GetMode().(type) {
	case *pb.GetCrosswalkRequest_Pair:
		return []string{m.Pair.GetCatalogA(), m.Pair.GetCatalogB()}
	case *pb.GetCrosswalkRequest_OneToManyCatalogId:
		return []string{m.OneToManyCatalogId}
	case *pb.GetCrosswalkRequest_Transitive:
		return []string{m.Transitive.GetCatalogA(), m.Transitive.GetHubCatalogId(), m.Transitive.GetCatalogC()}
	}
	return nil
}

func crosswalkModeName(msg *pb.GetCrosswalkRequest) string {
	switch msg.GetMode().(type) {
	case *pb.GetCrosswalkRequest_Pair:
		return "pair"
	case *pb.GetCrosswalkRequest_JobId:
		return "job"
	case *pb.GetCrosswalkRequest_OneToManyCatalogId:
		return "one_to_many"
	case *pb.GetCrosswalkRequest_Transitive:
		return "transitive"
	}
	return "unset"
}

// validateCrosswalkRequest checks the mode arguments and filters before any
// lookup. Every message says what to do instead.
func validateCrosswalkRequest(msg *pb.GetCrosswalkRequest) error {
	invalid := func(format string, args ...any) error {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(format, args...))
	}

	switch m := msg.GetMode().(type) {
	case nil:
		return invalid("no crosswalk mode set: name two catalogs (crosscodex crosswalk <a> <b>), a job (--job <job-id>), " +
			"one catalog against all others (<a> --all), or two catalogs through a hub (<a> <c> --via <hub>)")
	case *pb.GetCrosswalkRequest_Pair:
		a, b := m.Pair.GetCatalogA(), m.Pair.GetCatalogB()
		if a == "" || b == "" {
			return invalid("pair mode needs two catalogs: crosscodex crosswalk <a> <b>")
		}
		if a == b {
			return invalid("pair mode needs two different catalogs; for one catalog's internal mappings use --job <job-id>")
		}
	case *pb.GetCrosswalkRequest_JobId:
		if m.JobId == "" {
			return invalid("job mode needs a job ID: crosscodex crosswalk --job <job-id>")
		}
	case *pb.GetCrosswalkRequest_OneToManyCatalogId:
		if m.OneToManyCatalogId == "" {
			return invalid("--all needs a catalog: crosscodex crosswalk <a> --all")
		}
	case *pb.GetCrosswalkRequest_Transitive:
		a, c, hub := m.Transitive.GetCatalogA(), m.Transitive.GetCatalogC(), m.Transitive.GetHubCatalogId()
		if a == "" || c == "" || hub == "" {
			return invalid("--via needs two catalogs: crosscodex crosswalk <a> <c> --via <hub>")
		}
		if a == c || a == hub || c == hub {
			return invalid("transitive mode needs three different catalogs (a, c and the hub); " +
				"for the direct crosswalk of two catalogs use: crosscodex crosswalk <a> <c>")
		}
	}

	if _, ok := pb.CrosswalkStage_name[int32(msg.GetStage())]; !ok {
		return invalid("unknown crosswalk stage %d: use synthesized or analyzer", int32(msg.GetStage()))
	}
	if c := msg.GetMinConfidence(); math.IsNaN(float64(c)) || c < 0 || c > 1 {
		return invalid("min_confidence %v is out of range: use a value from 0 to 1", c)
	}
	for _, t := range msg.GetRelationshipTypes() {
		if _, ok := pb.RelationshipType_name[int32(t)]; !ok || t == pb.RelationshipType_RELATIONSHIP_TYPE_UNSPECIFIED {
			return invalid("relationship type %d is not a verdict: name one of the relationship types, for example equivalent", int32(t))
		}
		if t == pb.RelationshipType_RELATIONSHIP_TYPE_NO_RELATIONSHIP {
			return invalid("no-relationship verdicts are not stored: the relationship analyzer omits them, so a crosswalk lists related pairs only")
		}
	}
	if size := msg.GetPagination().GetPageSize(); size < 0 || size > crosswalkMaxPageSize {
		return invalid("page_size %d is out of range: use a value from 0 to %d (0 means the default)", size, crosswalkMaxPageSize)
	}
	return nil
}
