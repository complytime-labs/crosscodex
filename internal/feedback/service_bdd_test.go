//go:build !integration

package feedback_test

import (
	"context"
	"encoding/base64"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/feedback"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/cleaning"
	"github.com/complytime-labs/crosscodex/pkg/cleaning/cleaningtest"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/graphdb/memdriver"
	"github.com/complytime-labs/crosscodex/pkg/jobconfig"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

const (
	svcTenant   = "tenant-svc"
	svcOther    = "tenant-svc-other"
	svcReviewer = "rev-1"
	svcJob      = "j1"
	runningJob  = "j-running"
)

var (
	// edgesFrom is every fixture edge's valid_from; svcStart is the
	// service's clock before its first tick.
	edgesFrom = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	svcStart  = time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	svcCfg    = config.FeedbackConfig{MinAgreement: 0.5, LowConfidence: 0.5}

	// Job j1 maps cat-a (SP 800-53 identifiers) onto cat-b (SP 800-171r3).
	// a1->b1 has an analyzer-stage and a synthesized edge, a1->b2 only a
	// synthesized one, and edge cleaning retired both of a2->b1's.
	mA1B1 = feedback.MappingID(svcJob, "a1", "b1")
	mA1B2 = feedback.MappingID(svcJob, "a1", "b2")
	mA2B1 = feedback.MappingID(svcJob, "a2", "b1")

	svcMappings = []feedback.Mapping{
		{SourceID: "a1", TargetID: "b1", RelationshipType: "EQUIVALENT", Confidence: 0.9, Viability: 0.8},
		{SourceID: "a1", TargetID: "b2", RelationshipType: "EQUIVALENT", Confidence: 0.3},
		{SourceID: "a2", TargetID: "b1", RelationshipType: "SUBSET_OF", Confidence: 0.6, Viability: 0.5},
	}
)

func control(id, catalog, identifier string) graphdb.Node {
	return graphdb.Node{ID: id, Label: "Control", ValidFrom: edgesFrom, Properties: map[string]any{
		"identifier": identifier, "label": strings.ToLower(identifier), "title": "Title of " + identifier, "catalog_id": catalog,
	}}
}

// matchEdge is a SEMANTIC_MATCH edge of job, of the synthesized stage when
// synthesized is set.
func matchEdge(id, job, typ string, synthesized bool, from time.Time) graphdb.Edge {
	props := map[string]any{"job_id": job, "relationship_type": typ}
	if synthesized {
		props[results.StageProperty], props["is_viable"] = results.StageSynthesized, true
	}
	return graphdb.Edge{ID: id, Label: results.SemanticMatchEdgeLabel, ValidFrom: from, Properties: props}
}

// svcNodes creates the fixture controls in a new graph.
func svcNodes(ctx context.Context) graphdb.GraphDB {
	g := memdriver.New()
	Expect(g.CreateGraph(ctx, svcTenant)).To(Succeed())
	for _, n := range []graphdb.Node{
		control("a1", "cat-a", "AC-2"), control("a2", "cat-a", "AC-3"),
		control("b1", "cat-b", "03.01.01"), control("b2", "cat-b", "03.01.02"),
	} {
		Expect(g.CreateNode(ctx, svcTenant, n)).To(Succeed())
	}
	return g
}

// svcGraph is job j1's graph as the analysis wrote it, after edge cleaning
// retired a2->b1.
func svcGraph(ctx context.Context) graphdb.GraphDB {
	g := svcNodes(ctx)
	for _, e := range []struct {
		src, tgt string
		edge     graphdb.Edge
	}{
		{"a1", "b1", matchEdge("an-j1-a1-b1", svcJob, "EQUIVALENT", false, edgesFrom)},
		{"a1", "b1", matchEdge(mA1B1, svcJob, "EQUIVALENT", true, edgesFrom)},
		{"a1", "b2", matchEdge(mA1B2, svcJob, "EQUIVALENT", true, edgesFrom)},
		{"a2", "b1", matchEdge("an-j1-a2-b1", svcJob, "SUBSET_OF", false, edgesFrom)},
		{"a2", "b1", matchEdge(mA2B1, svcJob, "SUBSET_OF", true, edgesFrom)},
	} {
		Expect(g.CreateEdge(ctx, svcTenant, e.src, e.tgt, e.edge)).To(Succeed())
	}
	_, err := cleaning.RetireEdges(ctx, g, svcTenant, []string{"an-j1-a2-b1", mA2B1}, edgesFrom.Add(time.Hour))
	Expect(err).NotTo(HaveOccurred())
	return g
}

func svcStore() *fakeStore {
	return &fakeStore{
		Reviewers: map[string]map[string]string{svcTenant: {svcReviewer: reviewerActive}},
		Jobs: map[string][]jobSeed{
			svcTenant: {{ID: svcJob, Status: "completed", Mappings: svcMappings}, {ID: runningJob, Status: "running", Mappings: svcMappings}},
			svcOther:  {{ID: "j-other", Status: "completed", Mappings: svcMappings}},
		},
	}
}

// cleaningJobs is the cleaning store over which a real cleaning.Engine reads
// j1's verdicts.
func cleaningJobs() *cleaningtest.Fake {
	return &cleaningtest.Fake{Jobs: map[string][]cleaning.Job{svcTenant: {{
		ID: svcJob, CompletedAt: edgesFrom, Catalogs: jobconfig.Catalogs{SourceID: "cat-a", TargetID: "cat-b"},
	}}}}
}

func newSigner() attestation.Generator {
	kp, err := attestation.NewEphemeralKeyProvider()
	Expect(err).NotTo(HaveOccurred())
	signer, err := attestation.NewGenerator(kp)
	Expect(err).NotTo(HaveOccurred())
	return signer
}

// ticking returns a clock that moves one second forward on every call.
func ticking() func() time.Time {
	t := svcStart
	return func() time.Time {
		t = t.Add(time.Second)
		return t
	}
}

func callerCtx(tenantID string, roles ...string) context.Context {
	ctx, err := tenant.WithTenant(context.Background(), tenantID)
	Expect(err).NotTo(HaveOccurred())
	return authn.WithIdentity(ctx, &authn.Identity{Subject: svcReviewer, TenantID: tenantID, Roles: roles})
}

func reviewerCtx() context.Context { return callerCtx(svcTenant, authn.RoleReviewer) }

func tc(tenantID string) *pb.TenantContext { return &pb.TenantContext{TenantId: tenantID} }

// svcState is everything a service call may write.
type svcState struct {
	Votes          []feedback.Vote
	Determinations []feedback.Determination
	Reviews        []feedback.Review
	Live           []string
	Edges          map[string]*time.Time
}

// knownEdges are the edge IDs whose valid_to a snapshot records.
var knownEdges = []string{"an-j1-a1-b1", mA1B1, mA1B2, "an-j1-a2-b1", mA2B1}

func snapshot(store *fakeStore, g graphdb.GraphDB) svcState {
	ctx, err := tenant.WithTenant(context.Background(), svcTenant)
	Expect(err).NotTo(HaveOccurred())
	votes, err := store.LatestVotes(ctx, svcTenant, svcJob)
	Expect(err).NotTo(HaveOccurred())
	reviews, err := store.Reviews(ctx, svcTenant)
	Expect(err).NotTo(HaveOccurred())
	st := svcState{Votes: votes, Determinations: store.Determinations(svcTenant), Reviews: reviews,
		Live: liveEdges(ctx, g), Edges: map[string]*time.Time{}}
	for _, id := range knownEdges {
		e, err := g.GetEdge(ctx, svcTenant, id)
		if err == nil {
			st.Edges[id] = e.ValidTo
		}
	}
	return st
}

func liveEdges(ctx context.Context, g graphdb.GraphDB) []string {
	rels, err := g.QueryRelationships(ctx, svcTenant, graphdb.RelationshipQuery{EdgeLabel: results.SemanticMatchEdgeLabel})
	Expect(err).NotTo(HaveOccurred())
	ids := make([]string, len(rels))
	for i, r := range rels {
		ids[i] = r.Edge.ID
	}
	slices.Sort(ids)
	return ids
}

// expectConnectError asserts err's Connect code and that its message holds
// every part.
func expectConnectError(err error, code connect.Code, parts ...string) {
	GinkgoHelper()
	Expect(err).To(HaveOccurred())
	Expect(connect.CodeOf(err)).To(Equal(code), err.Error())
	for _, p := range parts {
		Expect(err.Error()).To(ContainSubstring(p))
	}
}

var _ = Describe("Service votes and review queue", func() {
	var (
		store *fakeStore
		g     graphdb.GraphDB
		svc   *feedback.Service
		tp    *telemetrytest.TestProvider
	)

	BeforeEach(func() {
		store = svcStore()
		ctx, err := tenant.WithTenant(context.Background(), svcTenant)
		Expect(err).NotTo(HaveOccurred())
		g = svcGraph(ctx)
		tp, err = telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(tp.Shutdown(context.Background())).To(Succeed()) })
		svc = feedback.NewService(store, svcCfg, feedback.WithClock(ticking()),
			feedback.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()))
	})

	vote := func(ctx context.Context, mappingID string, t pb.VoteType, rationale string) (*connect.Response[pb.SubmitVoteResponse], error) {
		return svc.SubmitVote(ctx, connect.NewRequest(&pb.SubmitVoteRequest{
			TenantContext: tc(svcTenant), JobId: svcJob, MappingId: mappingID, VoteType: t, Rationale: rationale,
		}))
	}

	Describe("SubmitVote", func() {
		It("records the caller's vote and returns the mapping's aggregation with it counted", func() {
			resp, err := svc.SubmitVote(reviewerCtx(), connect.NewRequest(&pb.SubmitVoteRequest{
				TenantContext: tc(svcTenant), JobId: svcJob, MappingId: mA1B1, VoteType: pb.VoteType_VOTE_TYPE_REFINE,
				SuggestedType: pb.RelationshipType_RELATIONSHIP_TYPE_SUBSET_OF, Rationale: "b1 covers only part of a1",
				Evidence: map[string]string{"section": "3.1.1"},
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetVoteId()).NotTo(BeEmpty())
			Expect(resp.Msg.GetAggregation()).To(Equal(&pb.VoteAggregation{
				MappingId: mA1B1, RefineCount: 1, ConsensusType: pb.RelationshipType_RELATIONSHIP_TYPE_SUBSET_OF, NeedsEscalation: true,
			}))

			votes := snapshot(store, g).Votes
			Expect(votes).To(HaveLen(1))
			Expect(votes[0]).To(Equal(feedback.Vote{
				ID: resp.Msg.GetVoteId(), MappingID: mA1B1, JobID: svcJob, SourceID: "a1", TargetID: "b1", Voter: svcReviewer,
				Type: feedback.VoteRefine, SuggestedType: "SUBSET_OF", Rationale: "b1 covers only part of a1",
				Evidence: map[string]string{"section": "3.1.1"}, CreatedAt: svcStart.Add(time.Second),
			}))
		})

		It("counts only the voter's latest vote", func() {
			_, err := vote(reviewerCtx(), mA1B1, pb.VoteType_VOTE_TYPE_DISAGREE, "wrong pair")
			Expect(err).NotTo(HaveOccurred())
			resp, err := vote(reviewerCtx(), mA1B1, pb.VoteType_VOTE_TYPE_AGREE, "")
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetAggregation()).To(Equal(&pb.VoteAggregation{
				MappingId: mA1B1, AgreeCount: 1, AgreementScore: 1, ConsensusType: pb.RelationshipType_RELATIONSHIP_TYPE_EQUIVALENT,
			}))
		})

		It("counts votes by type and times the call", func() {
			_, err := vote(reviewerCtx(), mA1B1, pb.VoteType_VOTE_TYPE_AGREE, "")
			Expect(err).NotTo(HaveOccurred())

			span := telemetrytest.FindSpan(tp.GetSpans(), "feedback.SubmitVote")
			Expect(span).NotTo(BeNil())
			v, ok := telemetrytest.SpanAttribute(span, "tenant.id")
			Expect(ok).To(BeTrue())
			Expect(v.AsString()).To(Equal(svcTenant))
			v, ok = telemetrytest.SpanAttribute(span, "feedback.operation")
			Expect(ok).To(BeTrue())
			Expect(v.AsString()).To(Equal("submit_vote"))

			rm := tp.GetMetrics()
			n, err := telemetrytest.CounterValue(telemetrytest.FindMetric(rm, "feedback.votes.total"))
			Expect(err).NotTo(HaveOccurred())
			Expect(n).To(Equal(int64(1)))
			count, err := telemetrytest.Float64HistogramCount(telemetrytest.FindMetric(rm, "feedback.operation.duration_ms"))
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(Equal(uint64(1)))
		})

		DescribeTable("refuses the vote and records nothing",
			func(ctx context.Context, req *pb.SubmitVoteRequest, code connect.Code, parts ...string) {
				before := snapshot(store, g)
				_, err := svc.SubmitVote(ctx, connect.NewRequest(req))
				expectConnectError(err, code, parts...)
				Expect(snapshot(store, g)).To(Equal(before))
			},
			Entry("without an identity", func() context.Context {
				ctx, _ := tenant.WithTenant(context.Background(), svcTenant)
				return ctx
			}(), &pb.SubmitVoteRequest{TenantContext: tc(svcTenant), JobId: svcJob, MappingId: mA1B1, VoteType: pb.VoteType_VOTE_TYPE_AGREE},
				connect.CodeUnauthenticated, "not authenticated"),
			Entry("from an admin without the reviewer role", callerCtx(svcTenant, authn.RoleAdmin),
				&pb.SubmitVoteRequest{TenantContext: tc(svcTenant), JobId: svcJob, MappingId: mA1B1, VoteType: pb.VoteType_VOTE_TYPE_AGREE},
				connect.CodePermissionDenied, "needs the reviewer role"),
			Entry("for another tenant, naming neither", reviewerCtx(),
				&pb.SubmitVoteRequest{TenantContext: tc(svcOther), JobId: "j-other", MappingId: mA1B1, VoteType: pb.VoteType_VOTE_TYPE_AGREE},
				connect.CodePermissionDenied, "Send the request for your own tenant"),
			Entry("without job_id", reviewerCtx(),
				&pb.SubmitVoteRequest{TenantContext: tc(svcTenant), MappingId: mA1B1, VoteType: pb.VoteType_VOTE_TYPE_AGREE},
				connect.CodeInvalidArgument, "job_id is required"),
			Entry("without mapping_id", reviewerCtx(),
				&pb.SubmitVoteRequest{TenantContext: tc(svcTenant), JobId: svcJob, VoteType: pb.VoteType_VOTE_TYPE_AGREE},
				connect.CodeInvalidArgument, "mapping_id is required", "crosscodex results export"),
			Entry("without a vote type", reviewerCtx(),
				&pb.SubmitVoteRequest{TenantContext: tc(svcTenant), JobId: svcJob, MappingId: mA1B1},
				connect.CodeInvalidArgument, "is not a vote", "VOTE_TYPE_AGREE"),
			Entry("for a refine vote without a suggested type", reviewerCtx(),
				&pb.SubmitVoteRequest{TenantContext: tc(svcTenant), JobId: svcJob, MappingId: mA1B1, VoteType: pb.VoteType_VOTE_TYPE_REFINE, Rationale: "r"},
				connect.CodeInvalidArgument, "must suggest the relationship type"),
			Entry("for a suggested type on an agree vote", reviewerCtx(),
				&pb.SubmitVoteRequest{TenantContext: tc(svcTenant), JobId: svcJob, MappingId: mA1B1, VoteType: pb.VoteType_VOTE_TYPE_AGREE,
					SuggestedType: pb.RelationshipType_RELATIONSHIP_TYPE_PARTIAL},
				connect.CodeInvalidArgument, "only a refine vote suggests"),
			Entry("for an unknown suggested type", reviewerCtx(),
				&pb.SubmitVoteRequest{TenantContext: tc(svcTenant), JobId: svcJob, MappingId: mA1B1, VoteType: pb.VoteType_VOTE_TYPE_REFINE,
					SuggestedType: pb.RelationshipType(99), Rationale: "r"},
				connect.CodeInvalidArgument, "suggested_type 99"),
			Entry("for a disagree vote without a rationale", reviewerCtx(),
				&pb.SubmitVoteRequest{TenantContext: tc(svcTenant), JobId: svcJob, MappingId: mA1B1, VoteType: pb.VoteType_VOTE_TYPE_DISAGREE},
				connect.CodeInvalidArgument, "need a rationale"),
			Entry("for a rationale with a control character", reviewerCtx(),
				&pb.SubmitVoteRequest{TenantContext: tc(svcTenant), JobId: svcJob, MappingId: mA1B1, VoteType: pb.VoteType_VOTE_TYPE_DISAGREE,
					Rationale: "bad\x1b[31m"},
				connect.CodeInvalidArgument, "control character"),
			Entry("for a rationale over 4096 bytes", reviewerCtx(),
				&pb.SubmitVoteRequest{TenantContext: tc(svcTenant), JobId: svcJob, MappingId: mA1B1, VoteType: pb.VoteType_VOTE_TYPE_DISAGREE,
					Rationale: strings.Repeat("x", 4097)},
				connect.CodeInvalidArgument, "over the 4096-byte limit"),
			Entry("for an unknown job", reviewerCtx(),
				&pb.SubmitVoteRequest{TenantContext: tc(svcTenant), JobId: "j-none", MappingId: mA1B1, VoteType: pb.VoteType_VOTE_TYPE_AGREE},
				connect.CodeNotFound, "job j-none is not one of your tenant's analysis jobs"),
			Entry("for another tenant's job", reviewerCtx(),
				&pb.SubmitVoteRequest{TenantContext: tc(svcTenant), JobId: "j-other", MappingId: feedback.MappingID("j-other", "a1", "b1"),
					VoteType: pb.VoteType_VOTE_TYPE_AGREE},
				connect.CodeNotFound, "job j-other is not one of your tenant's analysis jobs"),
			Entry("for a job that has not completed", reviewerCtx(),
				&pb.SubmitVoteRequest{TenantContext: tc(svcTenant), JobId: runningJob, MappingId: feedback.MappingID(runningJob, "a1", "b1"),
					VoteType: pb.VoteType_VOTE_TYPE_AGREE},
				connect.CodeFailedPrecondition, "is running, not completed", "wait for it to complete"),
			Entry("for a mapping that is not the job's", reviewerCtx(),
				&pb.SubmitVoteRequest{TenantContext: tc(svcTenant), JobId: svcJob, MappingId: feedback.MappingID(svcJob, "a2", "b2"),
					VoteType: pb.VoteType_VOTE_TYPE_AGREE},
				connect.CodeNotFound, "is not a mapping of job j1", "crosscodex results export j1"),
		)
	})

	Describe("GetReviewQueue", func() {
		queue := func(ctx context.Context, filter pb.ReviewQueueFilter, size int32, token string) (*pb.GetReviewQueueResponse, error) {
			resp, err := svc.GetReviewQueue(ctx, connect.NewRequest(&pb.GetReviewQueueRequest{
				TenantContext: tc(svcTenant), JobId: svcJob, Filter: filter,
				Options: &pb.ListOptions{Pagination: &pb.Pagination{PageSize: size, PageToken: token}},
			}))
			if err != nil {
				return nil, err
			}
			return resp.Msg, nil
		}
		ids := func(resp *pb.GetReviewQueueResponse) []string {
			var out []string
			for _, it := range resp.GetItems() {
				out = append(out, it.GetMapping().GetMappingId())
			}
			return out
		}

		BeforeEach(func() {
			// a1->b1: escalated (and disputed); a1->b2: unvoted, low
			// confidence; a2->b1: disputed by one disagree vote.
			for _, v := range []struct {
				id string
				t  pb.VoteType
			}{{mA1B1, pb.VoteType_VOTE_TYPE_ESCALATE}, {mA2B1, pb.VoteType_VOTE_TYPE_DISAGREE}} {
				_, err := vote(reviewerCtx(), v.id, v.t, "needs a second look")
				Expect(err).NotTo(HaveOccurred())
			}
		})

		It("lists mappings matching any of the first three filters, each with the first reason that holds", func() {
			resp, err := queue(callerCtx(svcTenant, authn.RoleAdmin), pb.ReviewQueueFilter_REVIEW_QUEUE_FILTER_UNSPECIFIED, 0, "")
			Expect(err).NotTo(HaveOccurred())
			Expect(ids(resp)).To(Equal([]string{mA1B1, mA1B2, mA2B1}))
			var reasons []pb.ReviewQueueFilter
			for _, it := range resp.GetItems() {
				reasons = append(reasons, it.GetReason())
			}
			Expect(reasons).To(Equal([]pb.ReviewQueueFilter{
				pb.ReviewQueueFilter_REVIEW_QUEUE_FILTER_ESCALATED, pb.ReviewQueueFilter_REVIEW_QUEUE_FILTER_LOW_CONFIDENCE,
				pb.ReviewQueueFilter_REVIEW_QUEUE_FILTER_HIGH_DISAGREEMENT,
			}))
			Expect(resp.GetItems()[0].GetMapping()).To(Equal(&pb.ControlMapping{
				MappingId: mA1B1, SourceControlId: "a1", TargetControlId: "b1",
				RelationshipType: pb.RelationshipType_RELATIONSHIP_TYPE_EQUIVALENT, Confidence: 0.9, ViabilityScore: 0.8, IsViable: true,
			}))
			Expect(resp.GetItems()[0].GetAggregation().GetEscalateCount()).To(Equal(int32(1)))
			Expect(resp.GetPageInfo().GetTotalCount()).To(Equal(int64(3)))
			Expect(resp.GetPageInfo().GetNextPageToken()).To(BeEmpty())
		})

		DescribeTable("filters the job's mappings",
			func(filter pb.ReviewQueueFilter, want ...string) {
				resp, err := queue(reviewerCtx(), filter, 0, "")
				Expect(err).NotTo(HaveOccurred())
				Expect(ids(resp)).To(Equal(want))
				for _, it := range resp.GetItems() {
					Expect(it.GetReason()).To(Equal(filter))
				}
			},
			Entry("LOW_CONFIDENCE lists confidence below feedback.low_confidence", pb.ReviewQueueFilter_REVIEW_QUEUE_FILTER_LOW_CONFIDENCE, mA1B2),
			Entry("HIGH_DISAGREEMENT lists voted mappings with agreement below feedback.min_agreement",
				pb.ReviewQueueFilter_REVIEW_QUEUE_FILTER_HIGH_DISAGREEMENT, mA1B1, mA2B1),
			Entry("ESCALATED lists mappings with an escalate vote", pb.ReviewQueueFilter_REVIEW_QUEUE_FILTER_ESCALATED, mA1B1),
			Entry("UNVOTED lists mappings without a vote", pb.ReviewQueueFilter_REVIEW_QUEUE_FILTER_UNVOTED, mA1B2),
		)

		It("pages in (source, target) order with an opaque token", func() {
			var got []string
			token := ""
			for range 3 {
				resp, err := queue(reviewerCtx(), pb.ReviewQueueFilter_REVIEW_QUEUE_FILTER_UNSPECIFIED, 1, token)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.GetItems()).To(HaveLen(1))
				Expect(resp.GetPageInfo().GetTotalCount()).To(Equal(int64(3)))
				got = append(got, ids(resp)...)
				token = resp.GetPageInfo().GetNextPageToken()
			}
			Expect(got).To(Equal([]string{mA1B1, mA1B2, mA2B1}))
			Expect(token).To(BeEmpty())
		})

		DescribeTable("refuses the request",
			func(ctx context.Context, req *pb.GetReviewQueueRequest, code connect.Code, parts ...string) {
				if req.TenantContext == nil {
					req.TenantContext = tc(svcTenant)
				}
				_, err := svc.GetReviewQueue(ctx, connect.NewRequest(req))
				expectConnectError(err, code, parts...)
			},
			Entry("without an identity", func() context.Context {
				ctx, _ := tenant.WithTenant(context.Background(), svcTenant)
				return ctx
			}(), &pb.GetReviewQueueRequest{JobId: svcJob}, connect.CodeUnauthenticated, "not authenticated"),
			Entry("from a caller with neither role", callerCtx(svcTenant, "viewer"), &pb.GetReviewQueueRequest{JobId: svcJob},
				connect.CodePermissionDenied, "needs the reviewer or admin role"),
			Entry("for another tenant", reviewerCtx(), &pb.GetReviewQueueRequest{TenantContext: tc(svcOther), JobId: "j-other"},
				connect.CodePermissionDenied, "Send the request for your own tenant"),
			Entry("without job_id", reviewerCtx(), &pb.GetReviewQueueRequest{}, connect.CodeInvalidArgument, "job_id is required"),
			Entry("for an unknown filter", reviewerCtx(), &pb.GetReviewQueueRequest{JobId: svcJob, Filter: pb.ReviewQueueFilter(9)},
				connect.CodeInvalidArgument, "filter 9 is not a review queue filter"),
			Entry("for a negative page size", reviewerCtx(),
				&pb.GetReviewQueueRequest{JobId: svcJob, Options: &pb.ListOptions{Pagination: &pb.Pagination{PageSize: -1}}},
				connect.CodeInvalidArgument, "page_size -1 is out of range"),
			Entry("for a page size over 1000", reviewerCtx(),
				&pb.GetReviewQueueRequest{JobId: svcJob, Options: &pb.ListOptions{Pagination: &pb.Pagination{PageSize: 1001}}},
				connect.CodeInvalidArgument, "page_size 1001 is out of range"),
			Entry("for a page token this server did not issue", reviewerCtx(),
				&pb.GetReviewQueueRequest{JobId: svcJob, Options: &pb.ListOptions{Pagination: &pb.Pagination{PageToken: "not-a-token!"}}},
				connect.CodeInvalidArgument, "page_token is not one this server issued"),
			Entry("for a page token without a position", reviewerCtx(),
				&pb.GetReviewQueueRequest{JobId: svcJob, Options: &pb.ListOptions{Pagination: &pb.Pagination{
					PageToken: base64.RawURLEncoding.EncodeToString([]byte(`{"s":""}`))}}},
				connect.CodeInvalidArgument, "page_token is not one this server issued"),
			Entry("for options.filters", reviewerCtx(),
				&pb.GetReviewQueueRequest{JobId: svcJob, Options: &pb.ListOptions{Filters: []*pb.FieldFilter{{Field: "status"}}}},
				connect.CodeInvalidArgument, "options.filters and options.sorts are not supported"),
			Entry("for an unknown job", reviewerCtx(), &pb.GetReviewQueueRequest{JobId: "j-none"},
				connect.CodeNotFound, "job j-none is not one of your tenant's analysis jobs"),
			Entry("for a job that has not completed", reviewerCtx(), &pb.GetReviewQueueRequest{JobId: runningJob},
				connect.CodeFailedPrecondition, "is running, not completed"),
		)
	})

	DescribeTable("aggregates a mapping's latest votes",
		func(votes []feedback.Vote, want feedback.Aggregation) {
			m := feedback.Mapping{RelationshipType: "EQUIVALENT"}
			Expect(feedback.Aggregate(m, votes, 0.5)).To(Equal(want))
		},
		Entry("to zeros and the mapping's type without votes", nil,
			feedback.Aggregation{ConsensusType: "EQUIVALENT"}),
		Entry("with agreement at the threshold not escalated", []feedback.Vote{agreeV(), disagreeV()},
			feedback.Aggregation{Agree: 1, Disagree: 1, AgreementScore: 0.5, ConsensusType: "EQUIVALENT"}),
		Entry("with agreement below the threshold escalated", []feedback.Vote{agreeV(), disagreeV(), disagreeV()},
			feedback.Aggregation{Agree: 1, Disagree: 2, AgreementScore: 1.0 / 3, ConsensusType: "EQUIVALENT", NeedsEscalation: true}),
		Entry("with any escalate vote escalated", []feedback.Vote{agreeV(), agreeV(), {Type: feedback.VoteEscalate}},
			feedback.Aggregation{Agree: 2, Escalate: 1, AgreementScore: 2.0 / 3, ConsensusType: "EQUIVALENT", NeedsEscalation: true}),
		Entry("to the leading refine type when refine votes outnumber every other type",
			[]feedback.Vote{refineV("SUBSET_OF"), refineV("SUBSET_OF"), refineV("PARTIAL"), agreeV()},
			feedback.Aggregation{Agree: 1, Refine: 3, AgreementScore: 0.25, ConsensusType: "SUBSET_OF", NeedsEscalation: true}),
		Entry("to the mapping's type when the refine types tie",
			[]feedback.Vote{refineV("SUBSET_OF"), refineV("PARTIAL")},
			feedback.Aggregation{Refine: 2, ConsensusType: "EQUIVALENT", NeedsEscalation: true}),
		Entry("to the mapping's type when refine votes only equal another type",
			[]feedback.Vote{refineV("SUBSET_OF"), disagreeV()},
			feedback.Aggregation{Disagree: 1, Refine: 1, ConsensusType: "EQUIVALENT", NeedsEscalation: true}),
	)
})

func agreeV() feedback.Vote    { return feedback.Vote{Type: feedback.VoteAgree} }
func disagreeV() feedback.Vote { return feedback.Vote{Type: feedback.VoteDisagree} }
func refineV(t string) feedback.Vote {
	return feedback.Vote{Type: feedback.VoteRefine, SuggestedType: t}
}
