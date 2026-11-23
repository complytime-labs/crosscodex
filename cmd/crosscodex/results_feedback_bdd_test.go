package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"

	connectrpc "connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
)

// fakeFeedbackClient records the feedback requests and serves canned
// responses; other GatewayServiceClient methods panic through the nil embed.
type fakeFeedbackClient struct {
	crosscodexv1connect.GatewayServiceClient
	queue  *pb.GetReviewQueueResponse
	vote   *pb.SubmitVoteResponse
	det    *pb.RecordDeterminationResponse
	err    error
	queues []*pb.GetReviewQueueRequest
	votes  []*pb.SubmitVoteRequest
	dets   []*pb.RecordDeterminationRequest
}

func (f *fakeFeedbackClient) GetReviewQueue(_ context.Context, req *connectrpc.Request[pb.GetReviewQueueRequest]) (*connectrpc.Response[pb.GetReviewQueueResponse], error) {
	f.queues = append(f.queues, req.Msg)
	return connectrpc.NewResponse(f.queue), f.err
}

func (f *fakeFeedbackClient) SubmitVote(_ context.Context, req *connectrpc.Request[pb.SubmitVoteRequest]) (*connectrpc.Response[pb.SubmitVoteResponse], error) {
	f.votes = append(f.votes, req.Msg)
	return connectrpc.NewResponse(f.vote), f.err
}

func (f *fakeFeedbackClient) RecordDetermination(_ context.Context, req *connectrpc.Request[pb.RecordDeterminationRequest]) (*connectrpc.Response[pb.RecordDeterminationResponse], error) {
	f.dets = append(f.dets, req.Msg)
	return connectrpc.NewResponse(f.det), f.err
}

var _ = Describe("Results review, vote and determine", func() {
	var (
		fake *fakeFeedbackClient
		out  *bytes.Buffer
	)

	BeforeEach(func() {
		fake = &fakeFeedbackClient{
			queue: &pb.GetReviewQueueResponse{
				Items: []*pb.ReviewItem{{
					Mapping:     &pb.ControlMapping{MappingId: "m-1", SourceControlId: "ac-1", TargetControlId: "pr-2", RelationshipType: pb.RelationshipType_RELATIONSHIP_TYPE_SUBSET_OF, Confidence: 0.55},
					Aggregation: &pb.VoteAggregation{AgreeCount: 1, DisagreeCount: 2, RefineCount: 3, EscalateCount: 4},
					Reason:      pb.ReviewQueueFilter_REVIEW_QUEUE_FILTER_HIGH_DISAGREEMENT,
				}},
				PageInfo: &pb.PageInfo{NextPageToken: "tok-2"},
			},
			vote: &pb.SubmitVoteResponse{VoteId: "v-9", Aggregation: &pb.VoteAggregation{AgreeCount: 5, AgreementScore: 0.5, NeedsEscalation: true}},
			det:  &pb.RecordDeterminationResponse{DeterminationId: "d-7", GraphUpdated: true, MappingStatus: "invalid"},
		}
		out = &bytes.Buffer{}
	})

	run := func(args ...string) error {
		cmd := newResultsCmd(&cliState{client: fake})
		cmd.PersistentFlags().Bool("json", false, "output as JSON") // root-level in production
		cmd.SetOut(out)
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(args)
		return cmd.Execute()
	}

	Describe("review", func() {
		It("sends the job id and renders the queue as a table with the next page", func() {
			Expect(run("review", "job-1")).To(Succeed())
			Expect(fake.queues).To(HaveLen(1))
			Expect(fake.queues[0].GetJobId()).To(Equal("job-1"))
			Expect(fake.queues[0].GetFilter()).To(Equal(pb.ReviewQueueFilter_REVIEW_QUEUE_FILTER_UNSPECIFIED))
			for _, want := range []string{"m-1", "ac-1", "pr-2", "subset_of", "0.55", "high_disagreement", "--page-token tok-2"} {
				Expect(out.String()).To(ContainSubstring(want))
			}
			Expect(out.String()).To(MatchRegexp(`1\s+2\s+3\s+4`))
		})

		DescribeTable("maps flags onto the request",
			func(args []string, filter pb.ReviewQueueFilter, size int32, token string) {
				Expect(run(append([]string{"review", "job-1"}, args...)...)).To(Succeed())
				req := fake.queues[0]
				Expect(req.GetFilter()).To(Equal(filter))
				Expect(req.GetOptions().GetPagination().GetPageSize()).To(Equal(size))
				Expect(req.GetOptions().GetPagination().GetPageToken()).To(Equal(token))
			},
			Entry("unvoted", []string{"--filter", "unvoted"}, pb.ReviewQueueFilter_REVIEW_QUEUE_FILTER_UNVOTED, int32(0), ""),
			Entry("low confidence, paged", []string{"--filter", "low-confidence", "--page-size", "20", "--page-token", "t"}, pb.ReviewQueueFilter_REVIEW_QUEUE_FILTER_LOW_CONFIDENCE, int32(20), "t"),
			Entry("escalated", []string{"--filter", "escalated"}, pb.ReviewQueueFilter_REVIEW_QUEUE_FILTER_ESCALATED, int32(0), ""),
			Entry("high disagreement", []string{"--filter", "high-disagreement"}, pb.ReviewQueueFilter_REVIEW_QUEUE_FILTER_HIGH_DISAGREEMENT, int32(0), ""),
			Entry("contested", []string{"--filter", "contested"}, pb.ReviewQueueFilter_REVIEW_QUEUE_FILTER_UNSPECIFIED, int32(0), ""),
			Entry("smallest page", []string{"--page-size", "1"}, pb.ReviewQueueFilter_REVIEW_QUEUE_FILTER_UNSPECIFIED, int32(1), ""),
			Entry("largest page", []string{"--page-size", "1000"}, pb.ReviewQueueFilter_REVIEW_QUEUE_FILTER_UNSPECIFIED, int32(1000), ""),
		)

		It("lists the valid filters for an unknown one and never calls the server", func() {
			err := run("review", "job-1", "--filter", "bogus")
			Expect(err).To(MatchError(ContainSubstring(`invalid --filter "bogus": use one of contested, escalated, high-disagreement, low-confidence, unvoted`)))
			Expect(fake.queues).To(BeEmpty())
		})

		It("passes an out-of-range --page-size to the server and shows its refusal", func() {
			fake.err = connectrpc.NewError(connectrpc.CodeInvalidArgument, errors.New("page_size 1001 is out of range: use 1 to 1000"))
			err := run("review", "job-1", "--page-size", "1001")
			Expect(err).To(MatchError(ContainSubstring("failed to get review queue")))
			Expect(err).To(MatchError(ContainSubstring("use 1 to 1000")))
		})

		It("says so when the page is empty", func() {
			fake.queue = &pb.GetReviewQueueResponse{}
			Expect(run("review", "job-1")).To(Succeed())
			Expect(out.String()).To(ContainSubstring("No mappings match this filter."))
		})

		It("keeps the table intact when an ID carries control characters", func() {
			fake.queue.Items[0].Mapping.MappingId = "m\n\x1b[2Jx\ty"
			fake.queue.Items[0].Mapping.SourceControlId = "a\tb"
			Expect(run("review", "job-1")).To(Succeed())
			Expect(out.String()).NotTo(ContainSubstring("\x1b"))
			lines := strings.Split(strings.TrimSpace(out.String()), "\n")
			Expect(lines).To(HaveLen(6), "title, blank, table header, one row, blank, next-page hint")
			Expect(lines[3]).To(ContainSubstring("m [2Jx y"))
			Expect(lines[3]).To(ContainSubstring("a b"))
		})

		It("emits the proto field names as JSON", func() {
			Expect(run("review", "job-1", "--json")).To(Succeed())
			var got map[string]any
			Expect(json.Unmarshal(out.Bytes(), &got)).To(Succeed())
			item := got["items"].([]any)[0].(map[string]any)
			Expect(item["reason"]).To(Equal("REVIEW_QUEUE_FILTER_HIGH_DISAGREEMENT"))
			Expect(item["mapping"].(map[string]any)["mapping_id"]).To(Equal("m-1"))
		})

		It("wraps a server error", func() {
			fake.err = errors.New("not completed")
			Expect(run("review", "job-1")).To(MatchError(ContainSubstring("failed to get review queue: not completed")))
		})
	})

	Describe("vote", func() {
		It("maps flags onto the request and prints the vote and aggregation", func() {
			Expect(run("vote", "job-1", "m-1", "--vote", "refine", "--suggest", "equivalent", "--rationale", "same scope",
				"--evidence", "a=1", "--evidence", "b=x=y")).To(Succeed())
			req := fake.votes[0]
			Expect(req.GetJobId()).To(Equal("job-1"))
			Expect(req.GetMappingId()).To(Equal("m-1"))
			Expect(req.GetVoteType()).To(Equal(pb.VoteType_VOTE_TYPE_REFINE))
			Expect(req.GetSuggestedType()).To(Equal(pb.RelationshipType_RELATIONSHIP_TYPE_EQUIVALENT))
			Expect(req.GetRationale()).To(Equal("same scope"))
			Expect(req.GetEvidence()).To(Equal(map[string]string{"a": "1", "b": "x=y"}))
			Expect(out.String()).To(ContainSubstring("Vote: v-9"))
			Expect(out.String()).To(ContainSubstring("agree 5"))
			Expect(out.String()).To(ContainSubstring("Needs escalation: yes"))
		})

		DescribeTable("rejects bad flags with an actionable error and never calls the server",
			func(want string, args ...string) {
				err := run(append([]string{"vote", "job-1", "m-1"}, args...)...)
				Expect(err).To(MatchError(ContainSubstring(want)))
				Expect(fake.votes).To(BeEmpty())
			},
			Entry("no --vote", "--vote is required and must be one of agree, disagree, escalate, refine"),
			Entry("unknown --vote", "must be one of agree", "--vote", "maybe"),
			Entry("unknown --suggest", `invalid --suggest "nope": use one of`, "--vote", "refine", "--suggest", "nope"),
			Entry("evidence without =", "use key=value", "--vote", "agree", "--evidence", "oops"),
			Entry("evidence with empty key", "use key=value", "--vote", "agree", "--evidence", "=v"),
			Entry("evidence with blank key", "use key=value", "--vote", "agree", "--evidence", "  =v"),
			Entry("evidence with empty value", "use key=value", "--vote", "agree", "--evidence", "k="),
			Entry("refine without --suggest", "--vote refine needs --suggest", "--vote", "refine"),
			Entry("--suggest with another vote", "only goes with --vote refine", "--vote", "agree", "--suggest", "equivalent"),
			Entry("duplicate evidence key", `key "a" is given twice`, "--vote", "agree", "--evidence", "a=1", "--evidence", "a=2"),
		)

		It("requires both ids", func() {
			Expect(run("vote", "job-1", "--vote", "agree")).To(MatchError(ContainSubstring("requires a job-id and mapping-id")))
		})

		It("emits the proto field names as JSON", func() {
			Expect(run("vote", "job-1", "m-1", "--vote", "agree", "--json")).To(Succeed())
			var got map[string]any
			Expect(json.Unmarshal(out.Bytes(), &got)).To(Succeed())
			Expect(got["vote_id"]).To(Equal("v-9"))
		})

		It("wraps a server error", func() {
			fake.err = errors.New("permission denied")
			Expect(run("vote", "job-1", "m-1", "--vote", "agree")).To(MatchError(ContainSubstring("failed to submit vote: permission denied")))
		})
	})

	Describe("determine", func() {
		DescribeTable("sends the verdict and prints the outcome",
			func(flag string, valid bool) {
				Expect(run("determine", "job-1", "m-1", flag, "--rationale", "checked", "--evidence", "t=1")).To(Succeed())
				req := fake.dets[0]
				Expect(req.GetIsValid()).To(Equal(valid))
				Expect(req.GetDeterminationRationale()).To(Equal("checked"))
				Expect(req.GetEvidence()).To(Equal(map[string]string{"t": "1"}))
				Expect(out.String()).To(ContainSubstring("Determination: d-7"))
				Expect(out.String()).To(ContainSubstring("Status: invalid"))
				Expect(out.String()).To(ContainSubstring("Graph updated: true"))
			},
			Entry("valid", "--valid", true),
			Entry("invalid", "--invalid", false),
		)

		DescribeTable("rejects bad flags with an actionable error and never calls the server",
			func(want string, args ...string) {
				err := run(append([]string{"determine", "job-1", "m-1"}, args...)...)
				Expect(err).To(MatchError(ContainSubstring(want)))
				Expect(fake.dets).To(BeEmpty())
			},
			Entry("neither verdict", "exactly one of --valid or --invalid", "--rationale", "r"),
			Entry("both verdicts", "exactly one of --valid or --invalid", "--valid", "--invalid", "--rationale", "r"),
			Entry("no rationale", "needs --rationale", "--valid"),
			Entry("blank rationale", "needs --rationale", "--valid", "--rationale", "  "),
			Entry("bad evidence", "use key=value", "--valid", "--rationale", "r", "--evidence", "x"),
		)

		It("emits the proto field names as JSON", func() {
			Expect(run("determine", "job-1", "m-1", "--valid", "--rationale", "r", "--json")).To(Succeed())
			var got map[string]any
			Expect(json.Unmarshal(out.Bytes(), &got)).To(Succeed())
			Expect(got["determination_id"]).To(Equal("d-7"))
			Expect(got["graph_updated"]).To(BeTrue())
		})

		It("wraps a server error", func() {
			fake.err = errors.New("already determined")
			Expect(run("determine", "job-1", "m-1", "--valid", "--rationale", "r")).To(MatchError(ContainSubstring("failed to record determination: already determined")))
		})
	})
})
