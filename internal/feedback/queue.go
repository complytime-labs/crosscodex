package feedback

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/authn"
)

const (
	// defaultPageSize and maxPageSize bound review queue pages as the other
	// listings do; a larger or negative page_size is refused, not clamped.
	defaultPageSize = 50
	maxPageSize     = 1000
)

// pageToken is the keyset position a queue page ends at: the last item's
// (source, target). The wire form is base64url of its JSON.
type pageToken struct {
	Source string `json:"s"`
	Target string `json:"t"`
}

func (t pageToken) encode() string {
	// Marshalling two strings cannot fail.
	b, _ := json.Marshal(t)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodePageToken(s string) (pageToken, error) {
	var t pageToken
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err == nil {
		err = json.Unmarshal(b, &t)
	}
	if err != nil || t.Source == "" || t.Target == "" {
		return pageToken{}, errors.New("page_token is not one this server issued: pass the next_page_token of the previous page unchanged, or omit it to start from the first page")
	}
	return t, nil
}

// after reports whether m comes after the token's position in (source,
// target) order.
func (t pageToken) after(m Mapping) bool {
	return m.SourceID > t.Source || (m.SourceID == t.Source && m.TargetID > t.Target)
}

// queueReason reports whether a mapping with aggregation a belongs in the
// queue under filter, and the reason it is listed. UNSPECIFIED lists a
// mapping that is escalated, highly disputed or low-confidence, giving the
// first of those that holds.
func (s *Service) queueReason(filter pb.ReviewQueueFilter, m Mapping, a Aggregation) (pb.ReviewQueueFilter, bool) {
	escalated := a.Escalate > 0
	disputed := a.Votes() > 0 && a.AgreementScore < s.cfg.MinAgreement
	lowConfidence := m.Confidence < s.cfg.LowConfidence
	switch filter {
	case pb.ReviewQueueFilter_REVIEW_QUEUE_FILTER_ESCALATED:
		return filter, escalated
	case pb.ReviewQueueFilter_REVIEW_QUEUE_FILTER_HIGH_DISAGREEMENT:
		return filter, disputed
	case pb.ReviewQueueFilter_REVIEW_QUEUE_FILTER_LOW_CONFIDENCE:
		return filter, lowConfidence
	case pb.ReviewQueueFilter_REVIEW_QUEUE_FILTER_UNVOTED:
		return filter, a.Votes() == 0
	}
	switch {
	case escalated:
		return pb.ReviewQueueFilter_REVIEW_QUEUE_FILTER_ESCALATED, true
	case disputed:
		return pb.ReviewQueueFilter_REVIEW_QUEUE_FILTER_HIGH_DISAGREEMENT, true
	case lowConfidence:
		return pb.ReviewQueueFilter_REVIEW_QUEUE_FILTER_LOW_CONFIDENCE, true
	}
	return filter, false
}

// GetReviewQueue returns one page of a completed job's mappings that match
// the request's filter, in (source, target) order, with their aggregations.
func (s *Service) GetReviewQueue(ctx context.Context, req *connect.Request[pb.GetReviewQueueRequest]) (_ *connect.Response[pb.GetReviewQueueResponse], err error) {
	ctx, finish := s.begin(ctx, "GetReviewQueue", "review_queue")
	defer func() { finish(err) }()

	if _, err := requireRole(ctx, queueDenied, authn.RoleReviewer, authn.RoleAdmin); err != nil {
		return nil, err
	}
	ctx, tenantID, err := scopeCtx(ctx, req.Msg.GetTenantContext())
	if err != nil {
		return nil, err
	}
	msg := req.Msg
	jobID := msg.GetJobId()
	if jobID == "" {
		return nil, invalid("job_id is required: name the completed analysis job whose mappings to review")
	}
	if err := checkText("job_id", jobID, maxIDBytes, false); err != nil {
		return nil, invalid("%w", err)
	}
	filter := msg.GetFilter()
	if _, ok := pb.ReviewQueueFilter_name[int32(filter)]; !ok {
		return nil, invalid("filter %d is not a review queue filter: set REVIEW_QUEUE_FILTER_LOW_CONFIDENCE, _HIGH_DISAGREEMENT, "+
			"_ESCALATED or _UNVOTED, or leave it unset for any of the first three", filter)
	}
	opts := msg.GetOptions()
	if len(opts.GetFilters()) > 0 || len(opts.GetSorts()) > 0 {
		return nil, invalid("options.filters and options.sorts are not supported: the queue is filtered by filter and ordered by (source, target)")
	}
	size := opts.GetPagination().GetPageSize()
	switch {
	case size < 0 || size > maxPageSize:
		return nil, invalid("page_size %d is out of range: use 1 to %d, or 0 for the default of %d", size, maxPageSize, defaultPageSize)
	case size == 0:
		size = defaultPageSize
	}
	var from *pageToken
	if raw := opts.GetPagination().GetPageToken(); raw != "" {
		t, err := decodePageToken(raw)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		from = &t
	}

	mappings, err := s.store.JobMappings(ctx, tenantID, jobID)
	switch {
	case errors.Is(err, ErrJobNotFound):
		return nil, serviceError(fmt.Errorf("job %s is not one of your tenant's analysis jobs; check the job ID: %w", jobID, ErrJobNotFound))
	case err != nil:
		return nil, serviceError(fmt.Errorf("read job %s's mappings: %w", jobID, err))
	}
	votes, err := s.store.LatestVotes(ctx, tenantID, jobID)
	if err != nil {
		return nil, fmt.Errorf("read job %s's votes: %w", jobID, err)
	}
	byMapping := map[string][]Vote{}
	for _, v := range votes {
		byMapping[v.MappingID] = append(byMapping[v.MappingID], v)
	}

	resp := &pb.GetReviewQueueResponse{PageInfo: &pb.PageInfo{}}
	more := false
	for _, m := range mappings {
		a := aggregate(m, byMapping[m.ID], s.cfg.MinAgreement)
		reason, ok := s.queueReason(filter, m, a)
		if !ok {
			continue
		}
		resp.PageInfo.TotalCount++
		if from != nil && !from.after(m) {
			continue
		}
		if len(resp.Items) == int(size) {
			more = true
			continue
		}
		pa, err := aggregationToProto(m.ID, a)
		if err != nil {
			return nil, err
		}
		resp.Items = append(resp.Items, &pb.ReviewItem{Mapping: mappingToProto(m), Aggregation: pa, Reason: reason})
	}
	if more {
		last := resp.Items[len(resp.Items)-1].GetMapping()
		resp.PageInfo.NextPageToken = pageToken{Source: last.GetSourceControlId(), Target: last.GetTargetControlId()}.encode()
	}
	return connect.NewResponse(resp), nil
}

func mappingToProto(m Mapping) *pb.ControlMapping {
	return &pb.ControlMapping{
		MappingId: m.ID, SourceControlId: m.SourceID, TargetControlId: m.TargetID,
		RelationshipType: relationshipType(m.RelationshipType), Confidence: float32(m.Confidence),
		ViabilityScore: float32(m.Viability), IsViable: m.Viability > 0,
	}
}
