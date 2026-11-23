//go:build !integration

package feedback_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/feedback"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/cleaning"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

var errInjected = errors.New("injected graph failure")

// failingSupersede fails every SupersedeFact while fail is set, as a graph
// outage would.
type failingSupersede struct {
	graphdb.GraphDB
	fail bool
}

func (f *failingSupersede) SupersedeFact(ctx context.Context, tenantID string, req graphdb.SupersedeRequest) (bool, error) {
	if f.fail {
		return false, errInjected
	}
	return f.GraphDB.SupersedeFact(ctx, tenantID, req)
}

// forgetfulStore reports no review for any mapping, as a reader that raced
// a concurrent determination would.
type forgetfulStore struct{ *fakeStore }

func (forgetfulStore) Review(context.Context, string, string) (feedback.Review, bool, error) {
	return feedback.Review{}, false, nil
}

func determineReq(tenantID, mappingID string, valid bool) *pb.RecordDeterminationRequest {
	return &pb.RecordDeterminationRequest{
		TenantContext: tc(tenantID), JobId: svcJob, MappingId: mappingID, IsValid: valid,
		DeterminationRationale: "checked against both control texts",
	}
}

// changedOnMarkStore refuses every MarkProjected as if a newer
// determination had landed meanwhile.
type changedOnMarkStore struct{ *fakeStore }

func (changedOnMarkStore) MarkProjected(_ context.Context, _, mappingID, _ string, _ []string) error {
	return feedback.ReviewChanged(mappingID, "a-newer-determination")
}

// driverSecret stands for driver detail (hosts, SQL, other rows) a caller
// must never see.
const driverSecret = "pq: password authentication failed for host db.internal"

var errDriver = errors.New(driverSecret)

func joinedDriver(sentinel error) error {
	return errors.Join(fmt.Errorf("feedback store: %w", sentinel), errDriver)
}

// failingStore fails the methods given an error, as a store whose driver
// failed would.
type failingStore struct {
	*fakeStore
	jobMappings, recordVote, determine error
}

func (f failingStore) JobMappings(ctx context.Context, tenantID, jobID string) ([]feedback.Mapping, error) {
	if f.jobMappings != nil {
		return nil, f.jobMappings
	}
	return f.fakeStore.JobMappings(ctx, tenantID, jobID)
}

func (f failingStore) RecordVote(ctx context.Context, tenantID string, v feedback.Vote) error {
	if f.recordVote != nil {
		return f.recordVote
	}
	return f.fakeStore.RecordVote(ctx, tenantID, v)
}

func (f failingStore) Determine(ctx context.Context, tenantID string, d feedback.Determination, expectPrev string, next feedback.Review) error {
	if f.determine != nil {
		return f.determine
	}
	return f.fakeStore.Determine(ctx, tenantID, d, expectPrev, next)
}

// at is the service clock's n-th tick.
func at(n int) time.Time { return svcStart.Add(time.Duration(n) * time.Second) }

var _ = Describe("Service.RecordDetermination", func() {
	var (
		ctx    context.Context
		store  *fakeStore
		g      *failingSupersede
		signer attestation.Generator
		svc    *feedback.Service
		tp     *telemetrytest.TestProvider
	)

	newSvc := func(st feedback.Store, opts ...feedback.Option) *feedback.Service {
		return feedback.NewService(st, svcCfg, append([]feedback.Option{feedback.WithClock(ticking()),
			feedback.WithTelemetry(tp.TracerProvider(), tp.MeterProvider())}, opts...)...)
	}
	withGraph := func() feedback.Option {
		return feedback.WithDeterminations(g, cleaning.NewEngine(cleaningJobs(), g, &config.CleaningConfig{}, nil, nil), signer)
	}
	determine := func(mappingID string, valid bool) (*pb.RecordDeterminationResponse, error) {
		resp, err := svc.RecordDetermination(reviewerCtx(), connect.NewRequest(determineReq(svcTenant, mappingID, valid)))
		if err != nil {
			return nil, err
		}
		return resp.Msg, nil
	}
	review := func(mappingID string) feedback.Review {
		r, ok, err := store.Review(ctx, svcTenant, mappingID)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		return r
	}
	gaugeValue := func() int64 {
		n, err := telemetrytest.GaugeValue(telemetrytest.FindMetric(tp.GetMetrics(), "feedback.reviews.unprojected"))
		Expect(err).NotTo(HaveOccurred())
		return n
	}

	BeforeEach(func() {
		var err error
		ctx, err = tenant.WithTenant(context.Background(), svcTenant)
		Expect(err).NotTo(HaveOccurred())
		store = svcStore()
		g = &failingSupersede{GraphDB: svcGraph(ctx)}
		signer = newSigner()
		tp, err = telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(tp.Shutdown(context.Background())).To(Succeed()) })
		svc = newSvc(store, withGraph())
	})

	It("retires the verdict's live edges of both stages for an invalid determination, signed and timed by it", func() {
		_, err := svc.SubmitVote(reviewerCtx(), connect.NewRequest(&pb.SubmitVoteRequest{
			TenantContext: tc(svcTenant), JobId: svcJob, MappingId: mA1B1, VoteType: pb.VoteType_VOTE_TYPE_DISAGREE, Rationale: "too broad",
		}))
		Expect(err).NotTo(HaveOccurred())
		req := determineReq(svcTenant, mA1B1, false)
		req.Evidence = map[string]string{"z": "last", "a": "first"}
		req.FinalType = pb.RelationshipType_RELATIONSHIP_TYPE_EQUIVALENT
		resp, err := svc.RecordDetermination(reviewerCtx(), connect.NewRequest(req))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.GetGraphUpdated()).To(BeTrue())
		Expect(resp.Msg.GetMappingStatus()).To(Equal("invalid"))
		id := resp.Msg.GetDeterminationId()

		decided := at(2) // the vote took the first tick
		st := snapshot(store, g)
		Expect(st.Live).To(Equal([]string{mA1B2}))
		Expect(st.Edges["an-j1-a1-b1"]).To(HaveValue(Equal(decided)))
		Expect(st.Edges[mA1B1]).To(HaveValue(Equal(decided)))
		Expect(review(mA1B1)).To(Equal(feedback.Review{
			MappingID: mA1B1, JobID: svcJob, SourceID: "a1", TargetID: "b1", Status: feedback.ReviewInvalid,
			DeterminationID: id, DecidedAt: decided, EdgeIDs: []string{"an-j1-a1-b1", mA1B1}, GraphApplied: true,
		}))

		Expect(st.Determinations).To(HaveLen(1))
		d := st.Determinations[0]
		sum := sha256.Sum256(d.Request)
		Expect(d.ID).To(Equal(hex.EncodeToString(sum[:])))
		Expect(d.ID).To(Equal(id))
		Expect(d.DecidedBy).To(Equal(svcReviewer))
		var got map[string]any
		Expect(json.Unmarshal(d.Request, &got)).To(Succeed())
		Expect(got).To(Equal(map[string]any{
			"tenant": svcTenant, "job_id": svcJob, "mapping_id": mA1B1, "verdict": "j1:a1:b1", "is_valid": false,
			"final_type": "EQUIVALENT", "rationale": "checked against both control texts",
			"evidence":               []any{map[string]any{"key": "a", "value": "first"}, map[string]any{"key": "z", "value": "last"}},
			"reviewer":               svcReviewer,
			"previous_determination": "",
			"aggregation": map[string]any{"agree": 0.0, "disagree": 1.0, "refine": 0.0, "escalate": 0.0, "agreement_score": 0.0,
				"consensus_type": "EQUIVALENT", "needs_escalation": true},
			"edge_ids":   []any{"an-j1-a1-b1", mA1B1},
			"decided_at": decided.Format(time.RFC3339Nano),
		}))
		link, err := signer.Verify(ctx, d.Attestation)
		Expect(err).NotTo(HaveOccurred())
		Expect(link.Step).To(Equal("feedback.determination"))
		Expect(link.Materials).To(Equal([]attestation.Artifact{{URI: "crosscodex:mapping-determination/" + id, Digest: id}}))
		Expect(link.Products).To(Equal([]attestation.Artifact{{URI: "crosscodex:mapping/" + mA1B1, Digest: mA1B1}}))
	})

	It("restores the retired edges for a valid determination after an invalid one", func() {
		inv, err := determine(mA1B1, false)
		Expect(err).NotTo(HaveOccurred())
		resp, err := determine(mA1B1, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.GetGraphUpdated()).To(BeTrue())
		Expect(resp.GetMappingStatus()).To(Equal("valid"))

		r := review(mA1B1)
		Expect(r.Status).To(Equal(feedback.ReviewValid))
		Expect(r.GraphApplied).To(BeTrue())
		Expect(r.EdgeIDs).To(HaveLen(2))
		st := snapshot(store, g)
		Expect(st.Live).To(ConsistOf(append([]string{mA1B2}, r.EdgeIDs...)))
		var supersedes []string
		for _, id := range r.EdgeIDs {
			e, err := g.GetEdge(ctx, svcTenant, id)
			Expect(err).NotTo(HaveOccurred())
			Expect(e.ValidFrom).To(Equal(at(2)))
			supersedes = append(supersedes, e.Supersedes)
		}
		Expect(supersedes).To(ConsistOf("an-j1-a1-b1", mA1B1))
		Expect(st.Edges[mA1B1]).To(HaveValue(Equal(at(1))))

		var req map[string]any
		Expect(json.Unmarshal(st.Determinations[1].Request, &req)).To(Succeed())
		Expect(req["previous_determination"]).To(Equal(inv.GetDeterminationId()))
	})

	It("retires the restored successors for an invalid determination after a restore", func() {
		for _, valid := range []bool{false, true} {
			_, err := determine(mA1B1, valid)
			Expect(err).NotTo(HaveOccurred())
		}
		successors := review(mA1B1).EdgeIDs
		resp, err := determine(mA1B1, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.GetGraphUpdated()).To(BeTrue())
		Expect(review(mA1B1).EdgeIDs).To(Equal(successors))
		Expect(snapshot(store, g).Live).To(Equal([]string{mA1B2}))
	})

	It("records a valid determination on an unreviewed mapping without changing the graph", func() {
		before := snapshot(store, g)
		resp, err := determine(mA1B2, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.GetGraphUpdated()).To(BeFalse())
		Expect(resp.GetMappingStatus()).To(Equal("valid"))
		after := snapshot(store, g)
		Expect(after.Live).To(Equal(before.Live))
		Expect(after.Edges).To(Equal(before.Edges))
		Expect(review(mA1B2)).To(Equal(feedback.Review{
			MappingID: mA1B2, JobID: svcJob, SourceID: "a1", TargetID: "b2", Status: feedback.ReviewValid,
			DeterminationID: resp.GetDeterminationId(), DecidedAt: at(1), GraphApplied: true,
		}))
	})

	DescribeTable("refuses the determination and changes nothing",
		func(ctx context.Context, req *pb.RecordDeterminationRequest, code connect.Code, parts ...string) {
			before := snapshot(store, g)
			_, err := svc.RecordDetermination(ctx, connect.NewRequest(req))
			expectConnectError(err, code, parts...)
			Expect(snapshot(store, g)).To(Equal(before))
		},
		Entry("from an admin without the reviewer role", callerCtx(svcTenant, authn.RoleAdmin), determineReq(svcTenant, mA1B1, false),
			connect.CodePermissionDenied, "needs the reviewer role", "An admin alone cannot"),
		Entry("for another tenant, naming neither", reviewerCtx(), determineReq(svcOther, mA1B1, false),
			connect.CodePermissionDenied, "Send the request for your own tenant"),
		Entry("without a rationale", reviewerCtx(), func() *pb.RecordDeterminationRequest {
			r := determineReq(svcTenant, mA1B1, false)
			r.DeterminationRationale = " \n"
			return r
		}(), connect.CodeInvalidArgument, "determination_rationale is required"),
		Entry("for a rationale over 4096 bytes", reviewerCtx(), func() *pb.RecordDeterminationRequest {
			r := determineReq(svcTenant, mA1B1, false)
			r.DeterminationRationale = strings.Repeat("x", 4097)
			return r
		}(), connect.CodeInvalidArgument, "over the 4096-byte limit"),
		Entry("for evidence with a control character", reviewerCtx(), func() *pb.RecordDeterminationRequest {
			r := determineReq(svcTenant, mA1B1, false)
			r.Evidence = map[string]string{"k": "line\nbreak"}
			return r
		}(), connect.CodeInvalidArgument, "control character"),
		Entry("for an unknown final type", reviewerCtx(), func() *pb.RecordDeterminationRequest {
			r := determineReq(svcTenant, mA1B1, false)
			r.FinalType = pb.RelationshipType(99)
			return r
		}(), connect.CodeInvalidArgument, "final_type 99"),
		Entry("for a final type that changes the mapping's type", reviewerCtx(), func() *pb.RecordDeterminationRequest {
			r := determineReq(svcTenant, mA1B1, true)
			r.FinalType = pb.RelationshipType_RELATIONSHIP_TYPE_SUBSET_OF
			return r
		}(), connect.CodeInvalidArgument, "cannot change a mapping's type", "Record is_valid=false and re-run the analysis"),
		Entry("when edge cleaning already retired the mapping", reviewerCtx(), determineReq(svcTenant, mA2B1, false),
			connect.CodeFailedPrecondition, "has no live edge in the graph", "edge cleaning retired it", "crosscodex results export j1"),
		Entry("for an unknown job", reviewerCtx(), func() *pb.RecordDeterminationRequest {
			r := determineReq(svcTenant, mA1B1, false)
			r.JobId = "j-none"
			return r
		}(), connect.CodeNotFound, "job j-none is not one of your tenant's analysis jobs"),
		Entry("for a job that has not completed", reviewerCtx(), func() *pb.RecordDeterminationRequest {
			r := determineReq(svcTenant, feedback.MappingID(runningJob, "a1", "b1"), false)
			r.JobId = runningJob
			return r
		}(), connect.CodeFailedPrecondition, "is running, not completed"),
		Entry("for a mapping that is not the job's", reviewerCtx(), determineReq(svcTenant, feedback.MappingID(svcJob, "a2", "b2"), false),
			connect.CodeNotFound, "is not a mapping of job j1", "crosscodex results export j1"),
		Entry("from a reviewer who is not registered", authn.WithIdentity(reviewerCtx(), &authn.Identity{
			Subject: "rev-unregistered", TenantID: svcTenant, Roles: []string{authn.RoleReviewer},
		}), determineReq(svcTenant, mA1B1, false), connect.CodeFailedPrecondition, "reviewer not registered"),
	)

	It("refuses a second invalid determination, naming the first, and changes nothing", func() {
		first, err := determine(mA1B1, false)
		Expect(err).NotTo(HaveOccurred())
		before := snapshot(store, g)
		_, err = determine(mA1B1, false)
		expectConnectError(err, connect.CodeFailedPrecondition, "already invalid by determination "+first.GetDeterminationId(), "is_valid=true")
		Expect(snapshot(store, g)).To(Equal(before))
	})

	It("refuses in the embedded daemon, which has no graph to change, and changes nothing", func() {
		svc = newSvc(store)
		before := snapshot(store, g)
		_, err := determine(mA1B1, false)
		expectConnectError(err, connect.CodeFailedPrecondition,
			"mapping determinations need the crosscodexd daemon (role all, or gateway with a graph role)")
		Expect(snapshot(store, g)).To(Equal(before))
	})

	It("refuses without a signer and changes nothing", func() {
		svc = newSvc(store, feedback.WithDeterminations(g, cleaning.NewEngine(cleaningJobs(), g, &config.CleaningConfig{}, nil, nil), nil))
		before := snapshot(store, g)
		_, err := determine(mA1B1, false)
		expectConnectError(err, connect.CodeFailedPrecondition, "mapping determinations are signed; set attestation.private_key_path")
		Expect(snapshot(store, g)).To(Equal(before))
	})

	It("refuses with Aborted when the review changed since it was read, and changes nothing", func() {
		_, err := determine(mA1B1, true)
		Expect(err).NotTo(HaveOccurred())
		svc = newSvc(forgetfulStore{store}, withGraph())
		before := snapshot(store, g)
		_, err = determine(mA1B1, false)
		expectConnectError(err, connect.CodeAborted, "changed since its review was read", "try again")
		Expect(snapshot(store, g)).To(Equal(before))
	})

	It("keeps a determination whose graph change failed and applies it before the next one", func() {
		g.fail = true
		first, err := svc.RecordDetermination(reviewerCtx(), connect.NewRequest(determineReq(svcTenant, mA1B1, false)))
		Expect(first).To(BeNil())
		expectConnectError(err, connect.CodeFailedPrecondition, "was recorded and stands", "0 of 1 recorded determinations applied",
			"check the daemon log")
		Expect(err.Error()).NotTo(ContainSubstring(errInjected.Error()))
		Expect(review(mA1B1).GraphApplied).To(BeFalse())
		Expect(gaugeValue()).To(Equal(int64(1)))

		By("refusing a new determination while the graph still fails, recording nothing")
		before := snapshot(store, g)
		_, err = determine(mA1B2, true)
		expectConnectError(err, connect.CodeFailedPrecondition, "1 mapping determinations are recorded but not yet applied to the graph",
			"nothing was recorded")
		Expect(snapshot(store, g)).To(Equal(before))

		By("applying it first once the graph recovers")
		g.fail = false
		_, err = determine(mA1B2, true)
		Expect(err).NotTo(HaveOccurred())
		r := review(mA1B1)
		Expect(r.GraphApplied).To(BeTrue())
		Expect(snapshot(store, g).Live).To(Equal([]string{mA1B2}))
		Expect(snapshot(store, g).Edges[mA1B1]).To(HaveValue(Equal(r.DecidedAt)))
		Expect(gaugeValue()).To(Equal(int64(0)))
	})

	It("refuses a restore whose retired edges a graph rebuild removed, records nothing, and later determinations still work", func() {
		_, err := determine(mA1B1, false)
		Expect(err).NotTo(HaveOccurred())
		// The rebuild wrote a1->b1 under another ID and kept a1->b2.
		g2 := svcNodes(ctx)
		for _, e := range []struct {
			src, tgt string
			edge     graphdb.Edge
		}{
			{"a1", "b1", matchEdge("rb-s-j1-a1-b1", svcJob, "EQUIVALENT", true, edgesFrom)},
			{"a1", "b2", matchEdge(mA1B2, svcJob, "EQUIVALENT", true, edgesFrom)},
		} {
			Expect(g2.CreateEdge(ctx, svcTenant, e.src, e.tgt, e.edge)).To(Succeed())
		}
		g = &failingSupersede{GraphDB: g2}
		svc = newSvc(store, withGraph())

		before := snapshot(store, g)
		_, err = determine(mA1B1, true)
		expectConnectError(err, connect.CodeFailedPrecondition,
			"mapping "+mA1B1+"'s retired edges are no longer in the graph (it was rebuilt), so this restore was refused and nothing was recorded",
			"crosscodexd admin cleaning --reproject --tenant "+svcTenant)
		Expect(snapshot(store, g)).To(Equal(before))

		resp, err := determine(mA1B2, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.GetGraphUpdated()).To(BeTrue())
	})

	It("leaves a review that changed while it was applied to its newer determination, without failing", func() {
		svc = newSvc(changedOnMarkStore{store}, withGraph())
		resp, err := determine(mA1B1, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.GetMappingStatus()).To(Equal("invalid"))
		Expect(review(mA1B1).GraphApplied).To(BeFalse())
		Expect(snapshot(store, g).Live).To(Equal([]string{mA1B2}))

		_, err = determine(mA1B2, true)
		Expect(err).NotTo(HaveOccurred())
	})

	DescribeTable("maps store errors so driver text never reaches the caller",
		func(st feedback.Store, call string, wantCode connect.Code, wantMsg string) {
			svc = newSvc(st, withGraph())
			var err error
			switch call {
			case "vote":
				_, err = svc.SubmitVote(reviewerCtx(), connect.NewRequest(&pb.SubmitVoteRequest{
					TenantContext: tc(svcTenant), JobId: svcJob, MappingId: mA1B1, VoteType: pb.VoteType_VOTE_TYPE_AGREE,
				}))
			case "queue":
				_, err = svc.GetReviewQueue(reviewerCtx(), connect.NewRequest(&pb.GetReviewQueueRequest{TenantContext: tc(svcTenant), JobId: svcJob}))
			default:
				_, err = determine(mA1B1, false)
			}
			Expect(err).To(HaveOccurred())
			if wantCode == 0 {
				var ce *connect.Error
				Expect(errors.As(err, &ce)).To(BeFalse(), "the error must reach the gateway unmapped, so it withholds it")
				Expect(err).To(MatchError(errDriver))
				return
			}
			Expect(connect.CodeOf(err)).To(Equal(wantCode))
			Expect(err.Error()).To(Equal(wantCode.String() + ": " + wantMsg))
			Expect(err.Error()).NotTo(ContainSubstring(driverSecret))
		},
		Entry("JobMappings joining a sentinel with a driver error sends only the sentinel's text",
			failingStore{fakeStore: svcStore(), jobMappings: joinedDriver(feedback.ErrMappingNotFound)}, "vote",
			connect.CodeNotFound, feedback.ErrMappingNotFound.Error()),
		Entry("RecordVote joining a sentinel with a driver error sends only the sentinel's text",
			failingStore{fakeStore: svcStore(), recordVote: joinedDriver(feedback.ErrReviewChanged)}, "vote",
			connect.CodeAborted, feedback.ErrReviewChanged.Error()),
		Entry("Determine joining a sentinel with a driver error sends only the sentinel's text",
			failingStore{fakeStore: svcStore(), determine: joinedDriver(cleaning.ErrReviewerNotRegistered)}, "determine",
			connect.CodeFailedPrecondition, cleaning.ErrReviewerNotRegistered.Error()),
		Entry("a plain JobMappings driver error is returned unmapped", failingStore{fakeStore: svcStore(), jobMappings: errDriver}, "queue",
			connect.Code(0), ""),
		Entry("a plain RecordVote driver error is returned unmapped", failingStore{fakeStore: svcStore(), recordVote: errDriver}, "vote",
			connect.Code(0), ""),
		Entry("a plain Determine driver error is returned unmapped", failingStore{fakeStore: svcStore(), determine: errDriver}, "determine",
			connect.Code(0), ""),
	)

	It("records a span with the tenant and operation, and the operation's duration", func() {
		_, err := determine(mA1B1, false)
		Expect(err).NotTo(HaveOccurred())
		span := telemetrytest.FindSpan(tp.GetSpans(), "feedback.RecordDetermination")
		Expect(span).NotTo(BeNil())
		v, ok := telemetrytest.SpanAttribute(span, "tenant.id")
		Expect(ok).To(BeTrue())
		Expect(v.AsString()).To(Equal(svcTenant))
		v, ok = telemetrytest.SpanAttribute(span, "feedback.operation")
		Expect(ok).To(BeTrue())
		Expect(v.AsString()).To(Equal("record_determination"))
		count, err := telemetrytest.Float64HistogramCount(telemetrytest.FindMetric(tp.GetMetrics(), "feedback.operation.duration_ms"))
		Expect(err).NotTo(HaveOccurred())
		Expect(count).To(Equal(uint64(1)))
		Expect(gaugeValue()).To(Equal(int64(0)))
	})
})
