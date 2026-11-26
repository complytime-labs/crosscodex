//go:build !integration

package gateway_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/gateway"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/authn"
)

func TestGatewayBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Gateway BDD Suite")
}

var _ = BeforeSuite(func() { DeferCleanup(testspecs.RedirectLogsToGinkgo()) })

// ---------------------------------------------------------------------------
// Compile-time interface checks
// ---------------------------------------------------------------------------

var _ gateway.IngestionBackend = (*mockIngestion)(nil)
var _ gateway.CatalogBackend = (*mockCatalog)(nil)
var _ gateway.PipelineBackend = (*mockPipeline)(nil)
var _ gateway.GraphBackend = (*mockGraph)(nil)
var _ gateway.FeedbackBackend = (*mockFeedback)(nil)
var _ gateway.AdminBackend = (*mockAdmin)(nil)

// ---------------------------------------------------------------------------
// Mock backend implementations
// ---------------------------------------------------------------------------

// orDefault calls fn when the spec scripted it, otherwise returns an empty response.
func orDefault[Q, R any](fn func(context.Context, *connect.Request[Q]) (*connect.Response[R], error), ctx context.Context, req *connect.Request[Q]) (*connect.Response[R], error) {
	if fn != nil {
		return fn(ctx, req)
	}
	return connect.NewResponse(new(R)), nil
}

type mockIngestion struct {
	convertFn func(context.Context, *connect.Request[pb.ConvertDocumentRequest]) (*connect.Response[pb.ConvertDocumentResponse], error)
	discardFn func(context.Context, string) error
}

func (m *mockIngestion) ConvertDocument(ctx context.Context, req *connect.Request[pb.ConvertDocumentRequest]) (*connect.Response[pb.ConvertDocumentResponse], error) {
	if m.convertFn != nil {
		return m.convertFn(ctx, req)
	}
	return connect.NewResponse(&pb.ConvertDocumentResponse{DocumentId: "doc-1"}), nil
}

func (m *mockIngestion) DiscardDocument(ctx context.Context, documentID string) error {
	if m.discardFn != nil {
		return m.discardFn(ctx, documentID)
	}
	return nil
}

type mockCatalog struct {
	parseFn          func(context.Context, *connect.Request[pb.ParseCatalogRequest]) (*connect.Response[pb.ParseCatalogResponse], error)
	listCatalogsFn   func(context.Context, *connect.Request[pb.ListCatalogsRequest]) (*connect.Response[pb.ListCatalogsResponse], error)
	getCatalogFn     func(context.Context, *connect.Request[pb.GetCatalogRequest]) (*connect.Response[pb.GetCatalogResponse], error)
	searchControlsFn func(context.Context, *connect.Request[pb.SearchControlsRequest]) (*connect.Response[pb.SearchControlsResponse], error)
	getControlFn     func(context.Context, *connect.Request[pb.GetControlRequest]) (*connect.Response[pb.GetControlResponse], error)
	retireFn         func(context.Context, *connect.Request[pb.RetireCatalogVersionRequest]) (*connect.Response[pb.RetireCatalogVersionResponse], error)
}

func (m *mockCatalog) ParseCatalog(ctx context.Context, req *connect.Request[pb.ParseCatalogRequest]) (*connect.Response[pb.ParseCatalogResponse], error) {
	if m.parseFn != nil {
		return m.parseFn(ctx, req)
	}
	return connect.NewResponse(&pb.ParseCatalogResponse{CatalogId: "cat-1", Status: pb.JobStatus_JOB_STATUS_COMPLETED}), nil
}

func (m *mockCatalog) ListCatalogs(ctx context.Context, req *connect.Request[pb.ListCatalogsRequest]) (*connect.Response[pb.ListCatalogsResponse], error) {
	return orDefault(m.listCatalogsFn, ctx, req)
}

func (m *mockCatalog) GetCatalog(ctx context.Context, req *connect.Request[pb.GetCatalogRequest]) (*connect.Response[pb.GetCatalogResponse], error) {
	return orDefault(m.getCatalogFn, ctx, req)
}

func (m *mockCatalog) SearchControls(ctx context.Context, req *connect.Request[pb.SearchControlsRequest]) (*connect.Response[pb.SearchControlsResponse], error) {
	return orDefault(m.searchControlsFn, ctx, req)
}

func (m *mockCatalog) GetControl(ctx context.Context, req *connect.Request[pb.GetControlRequest]) (*connect.Response[pb.GetControlResponse], error) {
	return orDefault(m.getControlFn, ctx, req)
}

func (m *mockCatalog) RetireCatalogVersion(ctx context.Context, req *connect.Request[pb.RetireCatalogVersionRequest]) (*connect.Response[pb.RetireCatalogVersionResponse], error) {
	return orDefault(m.retireFn, ctx, req)
}

type mockPipeline struct {
	createJobFn func(context.Context, *connect.Request[pb.CreateJobRequest]) (*connect.Response[pb.CreateJobResponse], error)
	getJobFn    func(context.Context, *connect.Request[pb.GetJobRequest]) (*connect.Response[pb.GetJobResponse], error)
	listJobsFn  func(context.Context, *connect.Request[pb.ListJobsRequest]) (*connect.Response[pb.ListJobsResponse], error)
	cancelJobFn func(context.Context, *connect.Request[pb.CancelJobRequest]) (*connect.Response[pb.CancelJobResponse], error)
	retryJobFn  func(context.Context, *connect.Request[pb.RetryJobRequest]) (*connect.Response[pb.RetryJobResponse], error)

	listCatalogControlsFn func(context.Context, *connect.Request[pb.ListCatalogControlsRequest]) (*connect.Response[pb.ListCatalogControlsResponse], error)
	renderPromptFn        func(context.Context, *connect.Request[pb.RenderPromptRequest]) (*connect.Response[pb.RenderPromptResponse], error)
}

func (m *mockPipeline) CreateJob(ctx context.Context, req *connect.Request[pb.CreateJobRequest]) (*connect.Response[pb.CreateJobResponse], error) {
	if m.createJobFn != nil {
		return m.createJobFn(ctx, req)
	}
	return connect.NewResponse(&pb.CreateJobResponse{JobId: "job-1", Status: pb.JobStatus_JOB_STATUS_PENDING}), nil
}

func (m *mockPipeline) GetJob(ctx context.Context, req *connect.Request[pb.GetJobRequest]) (*connect.Response[pb.GetJobResponse], error) {
	if m.getJobFn != nil {
		return m.getJobFn(ctx, req)
	}
	return connect.NewResponse(&pb.GetJobResponse{
		Job: &pb.PipelineJob{
			JobId: "job-1",
			Audit: &pb.AuditMetadata{CreatedBy: "user-a"},
		},
	}), nil
}

func (m *mockPipeline) ListJobs(ctx context.Context, req *connect.Request[pb.ListJobsRequest]) (*connect.Response[pb.ListJobsResponse], error) {
	return orDefault(m.listJobsFn, ctx, req)
}

func (m *mockPipeline) CancelJob(ctx context.Context, req *connect.Request[pb.CancelJobRequest]) (*connect.Response[pb.CancelJobResponse], error) {
	if m.cancelJobFn != nil {
		return m.cancelJobFn(ctx, req)
	}
	return connect.NewResponse(&pb.CancelJobResponse{Cancelled: true}), nil
}

func (m *mockPipeline) RetryJob(ctx context.Context, req *connect.Request[pb.RetryJobRequest]) (*connect.Response[pb.RetryJobResponse], error) {
	if m.retryJobFn != nil {
		return m.retryJobFn(ctx, req)
	}
	return connect.NewResponse(&pb.RetryJobResponse{NewJobId: req.Msg.GetJobId()}), nil
}

func (m *mockPipeline) ListCatalogControls(ctx context.Context, req *connect.Request[pb.ListCatalogControlsRequest]) (*connect.Response[pb.ListCatalogControlsResponse], error) {
	return orDefault(m.listCatalogControlsFn, ctx, req)
}

func (m *mockPipeline) RenderPrompt(ctx context.Context, req *connect.Request[pb.RenderPromptRequest]) (*connect.Response[pb.RenderPromptResponse], error) {
	return orDefault(m.renderPromptFn, ctx, req)
}

// mockMappings records the request it receives and returns a configured
// response or error.
type mockMappings struct {
	got  *pb.ListJobMappingsRequest
	resp *pb.ListJobMappingsResponse
	err  error
}

func (m *mockMappings) ListMappings(_ context.Context, req *connect.Request[pb.ListJobMappingsRequest]) (*connect.Response[pb.ListJobMappingsResponse], error) {
	m.got = req.Msg
	if m.err != nil {
		return nil, m.err
	}
	if m.resp != nil {
		return connect.NewResponse(m.resp), nil
	}
	return connect.NewResponse(&pb.ListJobMappingsResponse{}), nil
}

// mockCandidates records the request it receives and returns a configured
// response or error.
type mockCandidates struct {
	got  *pb.ListJobCandidatesRequest
	resp *pb.ListJobCandidatesResponse
	err  error
}

func (m *mockCandidates) ListCandidates(_ context.Context, req *connect.Request[pb.ListJobCandidatesRequest]) (*connect.Response[pb.ListJobCandidatesResponse], error) {
	m.got = req.Msg
	if m.err != nil {
		return nil, m.err
	}
	if m.resp != nil {
		return connect.NewResponse(m.resp), nil
	}
	return connect.NewResponse(&pb.ListJobCandidatesResponse{}), nil
}

type mockGraph struct {
	traverseFn         func(context.Context, *connect.Request[pb.TraverseRequest]) (*connect.Response[pb.TraverseResponse], error)
	queryFn            func(context.Context, *connect.Request[pb.QueryRequest]) (*connect.Response[pb.QueryResponse], error)
	similaritySearchFn func(context.Context, *connect.Request[pb.SimilaritySearchRequest]) (*connect.Response[pb.SimilaritySearchResponse], error)
}

func (m *mockGraph) Traverse(ctx context.Context, req *connect.Request[pb.TraverseRequest]) (*connect.Response[pb.TraverseResponse], error) {
	return orDefault(m.traverseFn, ctx, req)
}

func (m *mockGraph) Query(ctx context.Context, req *connect.Request[pb.QueryRequest]) (*connect.Response[pb.QueryResponse], error) {
	return orDefault(m.queryFn, ctx, req)
}

func (m *mockGraph) SimilaritySearch(ctx context.Context, req *connect.Request[pb.SimilaritySearchRequest]) (*connect.Response[pb.SimilaritySearchResponse], error) {
	return orDefault(m.similaritySearchFn, ctx, req)
}

type mockFeedback struct {
	submitVoteFn     func(context.Context, *connect.Request[pb.SubmitVoteRequest]) (*connect.Response[pb.SubmitVoteResponse], error)
	getReviewQueueFn func(context.Context, *connect.Request[pb.GetReviewQueueRequest]) (*connect.Response[pb.GetReviewQueueResponse], error)
	recordDetermFn   func(context.Context, *connect.Request[pb.RecordDeterminationRequest]) (*connect.Response[pb.RecordDeterminationResponse], error)
}

func (m *mockFeedback) SubmitVote(ctx context.Context, req *connect.Request[pb.SubmitVoteRequest]) (*connect.Response[pb.SubmitVoteResponse], error) {
	if m.submitVoteFn != nil {
		return m.submitVoteFn(ctx, req)
	}
	return connect.NewResponse(&pb.SubmitVoteResponse{VoteId: "vote-1"}), nil
}

func (m *mockFeedback) GetReviewQueue(ctx context.Context, req *connect.Request[pb.GetReviewQueueRequest]) (*connect.Response[pb.GetReviewQueueResponse], error) {
	return orDefault(m.getReviewQueueFn, ctx, req)
}

func (m *mockFeedback) RecordDetermination(ctx context.Context, req *connect.Request[pb.RecordDeterminationRequest]) (*connect.Response[pb.RecordDeterminationResponse], error) {
	return orDefault(m.recordDetermFn, ctx, req)
}

type mockAdmin struct {
	healthCheckFn func(context.Context, *connect.Request[pb.HealthCheckRequest]) (*connect.Response[pb.HealthCheckResponse], error)
}

func (m *mockAdmin) HealthCheck(ctx context.Context, req *connect.Request[pb.HealthCheckRequest]) (*connect.Response[pb.HealthCheckResponse], error) {
	if m.healthCheckFn != nil {
		return m.healthCheckFn(ctx, req)
	}
	return connect.NewResponse(&pb.HealthCheckResponse{Status: pb.HealthStatus_HEALTH_STATUS_HEALTHY}), nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func newTestService(opts ...gateway.ServiceOption) *gateway.Service {
	defaults := []gateway.ServiceOption{
		gateway.WithIngestionBackend(&mockIngestion{}),
		gateway.WithCatalogBackend(&mockCatalog{}),
		gateway.WithPipelineBackend(&mockPipeline{}),
		gateway.WithGraphBackend(&mockGraph{}),
		gateway.WithFeedbackBackend(&mockFeedback{}),
		gateway.WithAdminBackend(&mockAdmin{}),
	}
	return gateway.NewService(append(defaults, opts...)...)
}

// summaryBackend is a pipeline mock whose job-1 is created by createdBy.
// It records whether each GetJob asked for cache summaries, returns one
// summary when asked, and fails the request when failSummaries is set.
type summaryBackend struct {
	*mockPipeline
	mu            sync.Mutex
	asked         []bool
	failSummaries bool
}

func newSummaryBackend(createdBy string) *summaryBackend {
	b := &summaryBackend{}
	b.mockPipeline = &mockPipeline{getJobFn: func(_ context.Context, req *connect.Request[pb.GetJobRequest]) (*connect.Response[pb.GetJobResponse], error) {
		b.mu.Lock()
		b.asked = append(b.asked, req.Msg.GetIncludeCacheSummaries())
		fail := b.failSummaries
		b.mu.Unlock()
		job := &pb.PipelineJob{JobId: "job-1", Audit: &pb.AuditMetadata{CreatedBy: createdBy}}
		if req.Msg.GetIncludeCacheSummaries() {
			if fail {
				return nil, connect.NewError(connect.CodeInternal, errors.New("get cache summaries: object storage unreachable"))
			}
			job.CacheSummaries = []*pb.AnalyzerCacheSummary{{Analyzer: "classify"}}
		}
		return connect.NewResponse(&pb.GetJobResponse{Job: job}), nil
	}}
	return b
}

func (b *summaryBackend) includes() []bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.asked)
}

// ownedJobBackend returns a pipeline mock whose GetJob reports job-1 as
// created by createdBy, for ownership-check specs.
func ownedJobBackend(createdBy string) *mockPipeline {
	return &mockPipeline{
		getJobFn: func(context.Context, *connect.Request[pb.GetJobRequest]) (*connect.Response[pb.GetJobResponse], error) {
			return connect.NewResponse(&pb.GetJobResponse{
				Job: &pb.PipelineJob{
					JobId: "job-1",
					Audit: &pb.AuditMetadata{CreatedBy: createdBy},
				},
			}), nil
		},
	}
}

func ctxWithIdentity(subject, tenantID string, roles []string) context.Context {
	return gateway.ExportContextWithIdentity(context.Background(), &authn.Identity{
		Subject:  subject,
		TenantID: tenantID,
		Roles:    roles,
		Method:   authn.AuthMethodMTLS,
	})
}

func adminCtx() context.Context {
	return ctxWithIdentity("admin-user", "tenant-a", []string{authn.RoleAdmin})
}

func userCtx(subject string) context.Context {
	return ctxWithIdentity(subject, "tenant-a", []string{"user"})
}

// ---------------------------------------------------------------------------
// BDD Specs
// ---------------------------------------------------------------------------

var _ = Describe("Health handler", func() {
	It("returns healthy status when backend is healthy", func() {
		svc := newTestService()
		resp, err := svc.Health(context.Background(), connect.NewRequest(&pb.HealthRequest{}))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.GetStatus()).To(Equal(pb.HealthStatus_HEALTH_STATUS_HEALTHY))
	})

	It("returns error when admin backend is nil", func() {
		svc := newTestService(gateway.WithAdminBackend(nil))
		_, err := svc.Health(context.Background(), connect.NewRequest(&pb.HealthRequest{}))
		Expect(err).To(HaveOccurred())
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnavailable))
	})
})

var _ = Describe("Catalog handlers", func() {
	It("injects auth-derived TenantContext, ignoring client-supplied value", func() {
		var captured *pb.TenantContext
		cat := &mockCatalog{
			listCatalogsFn: func(_ context.Context, req *connect.Request[pb.ListCatalogsRequest]) (*connect.Response[pb.ListCatalogsResponse], error) {
				captured = req.Msg.GetTenantContext()
				return connect.NewResponse(&pb.ListCatalogsResponse{}), nil
			},
		}
		svc := newTestService(gateway.WithCatalogBackend(cat))

		ctx := ctxWithIdentity("user-1", "real-tenant", []string{"user"})
		req := &pb.ListCatalogsRequest{
			TenantContext: &pb.TenantContext{TenantId: "spoofed-tenant"},
		}

		_, err := svc.ListCatalogs(ctx, connect.NewRequest(req))
		Expect(err).NotTo(HaveOccurred())
		Expect(captured).NotTo(BeNil())
		Expect(captured.GetTenantId()).To(Equal("real-tenant"))
	})

	It("returns Unauthenticated when no identity in context", func() {
		svc := newTestService()
		_, err := svc.ListCatalogs(context.Background(), connect.NewRequest(&pb.ListCatalogsRequest{}))
		Expect(err).To(HaveOccurred())
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated))
	})

	It("propagates backend errors with Connect error codes", func() {
		cat := &mockCatalog{
			listCatalogsFn: func(context.Context, *connect.Request[pb.ListCatalogsRequest]) (*connect.Response[pb.ListCatalogsResponse], error) {
				return nil, connect.NewError(connect.CodeInternal, errors.New("db connection lost"))
			},
		}
		svc := newTestService(gateway.WithCatalogBackend(cat))

		ctx := userCtx("user-1")
		_, err := svc.ListCatalogs(ctx, connect.NewRequest(&pb.ListCatalogsRequest{}))
		Expect(err).To(HaveOccurred())
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInternal))
	})
})

var _ = Describe("ListCatalogControls handler", func() {
	It("sends the auth-derived TenantContext and passes the backend's response through", func() {
		var captured *pb.ListCatalogControlsRequest
		want := connect.NewResponse(&pb.ListCatalogControlsResponse{Controls: []*pb.Control{
			{ControlId: "cat-1/ac-2", CatalogId: "cat-1", Title: "Account Management", Statement: "The organization:"},
		}})
		svc := newTestService(gateway.WithPipelineBackend(&mockPipeline{
			listCatalogControlsFn: func(_ context.Context, req *connect.Request[pb.ListCatalogControlsRequest]) (*connect.Response[pb.ListCatalogControlsResponse], error) {
				captured = req.Msg
				return want, nil
			},
		}))

		got, err := svc.ListCatalogControls(ctxWithIdentity("user-1", "real-tenant", []string{"user"}),
			connect.NewRequest(&pb.ListCatalogControlsRequest{
				TenantContext: &pb.TenantContext{TenantId: "spoofed-tenant"},
				CatalogId:     "cat-1",
			}))

		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeIdenticalTo(want))
		Expect(captured.GetCatalogId()).To(Equal("cat-1"))
		Expect(captured.GetTenantContext().GetTenantId()).To(Equal("real-tenant"))
	})

	It("returns Unauthenticated when no identity in context", func() {
		_, err := newTestService().ListCatalogControls(context.Background(),
			connect.NewRequest(&pb.ListCatalogControlsRequest{CatalogId: "cat-1"}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated))
	})

	It("passes the backend's NotFound through unchanged", func() {
		notFound := connect.NewError(connect.CodeNotFound, errors.New(`catalog "cat-9" is not a catalog of your tenant`))
		svc := newTestService(gateway.WithPipelineBackend(&mockPipeline{
			listCatalogControlsFn: func(context.Context, *connect.Request[pb.ListCatalogControlsRequest]) (*connect.Response[pb.ListCatalogControlsResponse], error) {
				return nil, notFound
			},
		}))
		_, err := svc.ListCatalogControls(userCtx("user-1"), connect.NewRequest(&pb.ListCatalogControlsRequest{CatalogId: "cat-9"}))
		Expect(err).To(BeIdenticalTo(notFound))
	})
})

var _ = Describe("RenderPrompt handler", func() {
	rendered := connect.NewResponse(&pb.RenderPromptResponse{
		Messages:   []*pb.PromptMessage{{Role: "system", Content: "S"}, {Role: "user", Content: "U"}},
		PromptName: "relationship", PromptVersion: "1.3.0",
	})
	pipelineFor := func(captured **pb.RenderPromptRequest, getJobs *int) *mockPipeline {
		return &mockPipeline{
			renderPromptFn: func(_ context.Context, req *connect.Request[pb.RenderPromptRequest]) (*connect.Response[pb.RenderPromptResponse], error) {
				*captured = req.Msg
				return rendered, nil
			},
			getJobFn: func(_ context.Context, req *connect.Request[pb.GetJobRequest]) (*connect.Response[pb.GetJobResponse], error) {
				*getJobs++
				return connect.NewResponse(&pb.GetJobResponse{Job: &pb.PipelineJob{
					JobId: req.Msg.GetJobId(), Audit: &pb.AuditMetadata{CreatedBy: "user-a"},
				}}), nil
			},
		}
	}

	It("sends the auth-derived TenantContext and passes the backend's response through", func() {
		var captured *pb.RenderPromptRequest
		var getJobs int
		svc := newTestService(gateway.WithPipelineBackend(pipelineFor(&captured, &getJobs)))

		got, err := svc.RenderPrompt(ctxWithIdentity("user-1", "real-tenant", []string{"user"}),
			connect.NewRequest(&pb.RenderPromptRequest{
				TenantContext: &pb.TenantContext{TenantId: "spoofed-tenant"},
				Analyzer:      "relationship", SourceCatalogId: "cat-1", SourceControlId: "cat-1/ac-2",
				TargetCatalogId: "cat-2", TargetControlId: "cat-2/po.1",
			}))

		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeIdenticalTo(rendered))
		Expect(captured.GetTenantContext().GetTenantId()).To(Equal("real-tenant"))
		Expect(captured.GetSourceControlId()).To(Equal("cat-1/ac-2"))
		Expect(getJobs).To(BeZero(), "a render from catalogs alone reads no job")
	})

	It("returns Unauthenticated when no identity in context", func() {
		_, err := newTestService().RenderPrompt(context.Background(), connect.NewRequest(&pb.RenderPromptRequest{Analyzer: "relationship"}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated))
	})

	It("passes the backend's NotFound through unchanged", func() {
		notFound := connect.NewError(connect.CodeNotFound, errors.New("no prompt to render: a catalog, control or job is not your tenant's"))
		svc := newTestService(gateway.WithPipelineBackend(&mockPipeline{
			renderPromptFn: func(context.Context, *connect.Request[pb.RenderPromptRequest]) (*connect.Response[pb.RenderPromptResponse], error) {
				return nil, notFound
			},
		}))
		_, err := svc.RenderPrompt(userCtx("user-a"), connect.NewRequest(&pb.RenderPromptRequest{Analyzer: "critique", JobId: "job-9"}))
		Expect(err).To(BeIdenticalTo(notFound))
	})

	DescribeTable("renders a job's prompt only for the job's owner or an admin",
		func(ctx context.Context, wantCode connect.Code) {
			var captured *pb.RenderPromptRequest
			var getJobs int
			svc := newTestService(gateway.WithPipelineBackend(pipelineFor(&captured, &getJobs)))
			got, err := svc.RenderPrompt(ctx, connect.NewRequest(&pb.RenderPromptRequest{Analyzer: "critique", JobId: "job-1",
				SourceControlId: "cat-1/ac-2", TargetControlId: "cat-2/po.1"}))
			if wantCode == 0 {
				Expect(err).NotTo(HaveOccurred())
				Expect(got).To(BeIdenticalTo(rendered))
				return
			}
			Expect(got).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(wantCode))
			Expect(err.Error()).To(ContainSubstring("not the job owner"))
		},
		Entry("the owner", userCtx("user-a"), connect.Code(0)),
		Entry("an admin", adminCtx(), connect.Code(0)),
		Entry("another user of the tenant", userCtx("user-b"), connect.CodePermissionDenied),
	)

	// A refusal about a job tells of the job's data (its catalogs, its
	// verdicts, whether it ran), so only its owner or an admin may read it.
	jobRefusals := map[string]error{
		"no accepted verdict": connect.NewError(connect.CodeFailedPrecondition,
			errors.New("job job-1 accepted no relationship or requires verdict for cat-1/ac-2 -> cat-2/po.1")),
		"two verdicts": connect.NewError(connect.CodeFailedPrecondition,
			errors.New("job job-1 accepted both a relationship (SUBSET_OF) and a requires verdict")),
		"not one of the job's catalogs": connect.NewError(connect.CodeInvalidArgument,
			errors.New(`source_catalog_id "cat-9" is not a catalog of job job-1, whose catalogs are cat-1 and cat-2`)),
		"never ran analysis": connect.NewError(connect.CodeFailedPrecondition,
			errors.New("cannot render a prompt of job job-1: its stored config names no catalog")),
	}
	DescribeTable("shows a refusal about a job only to the job's owner or an admin",
		func(refusal string, ctx context.Context, wantOwn bool) {
			want := jobRefusals[refusal]
			svc := newTestService(gateway.WithPipelineBackend(&mockPipeline{
				renderPromptFn: func(context.Context, *connect.Request[pb.RenderPromptRequest]) (*connect.Response[pb.RenderPromptResponse], error) {
					return nil, want
				},
			}))
			_, err := svc.RenderPrompt(ctx, connect.NewRequest(&pb.RenderPromptRequest{Analyzer: "critique", JobId: "job-1",
				SourceControlId: "cat-1/ac-2", TargetControlId: "cat-2/po.1"}))
			if wantOwn {
				Expect(err).To(BeIdenticalTo(want))
				return
			}
			Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied))
			Expect(err.Error()).To(ContainSubstring("not the job owner"))
			Expect(err.Error()).NotTo(ContainSubstring("cat-"))
		},
		Entry("no accepted verdict, to the owner", "no accepted verdict", userCtx("user-a"), true),
		Entry("no accepted verdict, to an admin", "no accepted verdict", adminCtx(), true),
		Entry("no accepted verdict, to another user", "no accepted verdict", userCtx("user-b"), false),
		Entry("two verdicts, to the owner", "two verdicts", userCtx("user-a"), true),
		Entry("two verdicts, to another user", "two verdicts", userCtx("user-b"), false),
		Entry("not one of the job's catalogs, to the owner", "not one of the job's catalogs", userCtx("user-a"), true),
		Entry("not one of the job's catalogs, to another user", "not one of the job's catalogs", userCtx("user-b"), false),
		Entry("never ran analysis, to an admin", "never ran analysis", adminCtx(), true),
		Entry("never ran analysis, to another user", "never ran analysis", userCtx("user-b"), false),
	)

	It("gives another user the pipeline's NotFound unchanged, without a job lookup", func() {
		notFound := connect.NewError(connect.CodeNotFound, errors.New("no prompt to render"))
		var getJobs int
		svc := newTestService(gateway.WithPipelineBackend(&mockPipeline{
			renderPromptFn: func(context.Context, *connect.Request[pb.RenderPromptRequest]) (*connect.Response[pb.RenderPromptResponse], error) {
				return nil, notFound
			},
			getJobFn: func(context.Context, *connect.Request[pb.GetJobRequest]) (*connect.Response[pb.GetJobResponse], error) {
				getJobs++
				return nil, errors.New("unexpected job lookup")
			},
		}))
		_, err := svc.RenderPrompt(userCtx("user-b"), connect.NewRequest(&pb.RenderPromptRequest{Analyzer: "critique", JobId: "job-1"}))
		Expect(err).To(BeIdenticalTo(notFound))
		Expect(getJobs).To(BeZero())
	})

	It("gives a non-admin NotFound for an unknown job when the pipeline fails with another error, leaking nothing of it", func() {
		svc := newTestService(gateway.WithPipelineBackend(&mockPipeline{
			renderPromptFn: func(context.Context, *connect.Request[pb.RenderPromptRequest]) (*connect.Response[pb.RenderPromptResponse], error) {
				return nil, connect.NewError(connect.CodeInternal, errors.New("reading job-1 config: pq: secret-detail"))
			},
			getJobFn: func(context.Context, *connect.Request[pb.GetJobRequest]) (*connect.Response[pb.GetJobResponse], error) {
				return nil, connect.NewError(connect.CodeNotFound, errors.New("job not found"))
			},
		}))
		got, err := svc.RenderPrompt(userCtx("user-a"), connect.NewRequest(&pb.RenderPromptRequest{Analyzer: "critique", JobId: "job-1"}))
		Expect(got).To(BeNil())
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
		Expect(err.Error()).NotTo(ContainSubstring("secret-detail"))
	})
})

var _ = Describe("RetireCatalogVersion handler", func() {
	retireReq := func() *connect.Request[pb.RetireCatalogVersionRequest] {
		return connect.NewRequest(&pb.RetireCatalogVersionRequest{
			TenantContext: &pb.TenantContext{TenantId: "spoofed-tenant"},
			DocumentSlug:  "nist", VersionLabel: []string{"Rev 4"},
		})
	}

	It("forwards an admin's request with the authenticated tenant and returns the retired IDs", func() {
		var captured *pb.RetireCatalogVersionRequest
		svc := newTestService(gateway.WithCatalogBackend(&mockCatalog{
			retireFn: func(_ context.Context, req *connect.Request[pb.RetireCatalogVersionRequest]) (*connect.Response[pb.RetireCatalogVersionResponse], error) {
				captured = req.Msg
				return connect.NewResponse(&pb.RetireCatalogVersionResponse{RetiredCatalogIds: []string{"cat-2"}}), nil
			},
		}))
		resp, err := svc.RetireCatalogVersion(adminCtx(), retireReq())
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.GetRetiredCatalogIds()).To(Equal([]string{"cat-2"}))
		Expect(captured.GetTenantContext().GetTenantId()).To(Equal("tenant-a"))
		Expect(captured.GetDocumentSlug()).To(Equal("nist"))
		Expect(captured.GetVersionLabel()).To(Equal([]string{"Rev 4"}))
	})

	DescribeTable("refuses a caller who may not delete catalog data, without reaching the backend",
		func(ctx context.Context, code connect.Code, wantMsg string) {
			called := false
			svc := newTestService(gateway.WithCatalogBackend(&mockCatalog{
				retireFn: func(context.Context, *connect.Request[pb.RetireCatalogVersionRequest]) (*connect.Response[pb.RetireCatalogVersionResponse], error) {
					called = true
					return connect.NewResponse(&pb.RetireCatalogVersionResponse{}), nil
				},
			}))
			_, err := svc.RetireCatalogVersion(ctx, retireReq())
			Expect(connect.CodeOf(err)).To(Equal(code))
			Expect(err.Error()).To(ContainSubstring(wantMsg))
			Expect(called).To(BeFalse())
		},
		Entry("an unauthenticated caller", context.Background(), connect.CodeUnauthenticated, "not authenticated"),
		Entry("a non-admin user", userCtx("user-1"), connect.CodePermissionDenied, "requires the admin role"),
	)

	It("passes a protected-version refusal through verbatim", func() {
		const msg = `catalog version is protected: cannot retire from nist: legal hold "litigation" covers nist@"Rev 4"`
		svc := newTestService(gateway.WithCatalogBackend(&mockCatalog{
			retireFn: func(context.Context, *connect.Request[pb.RetireCatalogVersionRequest]) (*connect.Response[pb.RetireCatalogVersionResponse], error) {
				return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New(msg))
			},
		}))
		_, err := svc.RetireCatalogVersion(adminCtx(), retireReq())
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		Expect(err.Error()).To(ContainSubstring(msg))
	})
})

var _ = Describe("Job handlers", func() {
	Context("GetJob", func() {
		It("returns job to its owner", func() {
			pipeline := ownedJobBackend("user-a")
			svc := newTestService(gateway.WithPipelineBackend(pipeline))

			ctx := userCtx("user-a")
			resp, err := svc.GetJob(ctx, connect.NewRequest(&pb.GetJobRequest{JobId: "job-1"}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetJob().GetJobId()).To(Equal("job-1"))
		})

		It("passes each stage's task progress through unchanged", func() {
			progress := &pb.StageProgress{TasksTotal: 10, TasksDone: 9, TasksSucceeded: 8, TasksFailed: 1,
				TasksRetried: 3, TasksCacheHits: 2, LastSuccessAt: timestamppb.New(time.Date(2026, 11, 24, 9, 30, 0, 0, time.UTC))}
			backend := &mockPipeline{getJobFn: func(context.Context, *connect.Request[pb.GetJobRequest]) (*connect.Response[pb.GetJobResponse], error) {
				return connect.NewResponse(&pb.GetJobResponse{Job: &pb.PipelineJob{
					JobId: "job-1", Audit: &pb.AuditMetadata{CreatedBy: "user-a"},
					Stages: []*pb.JobStage{{StageName: "classify", Progress: progress}, {StageName: "synthesis"}},
				}}), nil
			}}
			svc := newTestService(gateway.WithPipelineBackend(backend))

			resp, err := svc.GetJob(userCtx("user-a"), connect.NewRequest(&pb.GetJobRequest{JobId: "job-1"}))
			Expect(err).NotTo(HaveOccurred())
			stages := resp.Msg.GetJob().GetStages()
			Expect(proto.Equal(stages[0].GetProgress(), progress)).To(BeTrue(), "progress: %v", stages[0].GetProgress())
			Expect(stages[1].GetProgress()).To(BeNil())
		})

		It("returns PermissionDenied when non-owner non-admin accesses job", func() {
			pipeline := ownedJobBackend("user-a")
			svc := newTestService(gateway.WithPipelineBackend(pipeline))

			ctx := userCtx("user-b")
			_, err := svc.GetJob(ctx, connect.NewRequest(&pb.GetJobRequest{JobId: "job-1"}))
			Expect(err).To(HaveOccurred())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied))
		})

		DescribeTable("reads cache summaries only when asked, and only after the caller is shown to own the job or be an admin",
			func(ctx context.Context, include bool, wantCode connect.Code, wantRequests []bool) {
				backend := newSummaryBackend("user-a")
				svc := newTestService(gateway.WithPipelineBackend(backend))
				resp, err := svc.GetJob(ctx, connect.NewRequest(&pb.GetJobRequest{JobId: "job-1", IncludeCacheSummaries: include}))
				if wantCode != 0 {
					Expect(connect.CodeOf(err)).To(Equal(wantCode))
				} else {
					Expect(err).NotTo(HaveOccurred())
				}
				Expect(backend.includes()).To(Equal(wantRequests))
				if err == nil {
					Expect(resp.Msg.GetJob().GetCacheSummaries()).To(HaveLen(map[bool]int{true: 1, false: 0}[include]))
				}
			},
			Entry("the owner, asking", userCtx("user-a"), true, connect.Code(0), []bool{false, true}),
			Entry("an admin, asking", adminCtx(), true, connect.Code(0), []bool{false, true}),
			Entry("the owner, not asking", userCtx("user-a"), false, connect.Code(0), []bool{false}),
			Entry("another user, asking", userCtx("user-b"), true, connect.CodePermissionDenied, []bool{false}),
		)

		It("allows admin to access any job", func() {
			pipeline := ownedJobBackend("user-a")
			svc := newTestService(gateway.WithPipelineBackend(pipeline))

			resp, err := svc.GetJob(adminCtx(), connect.NewRequest(&pb.GetJobRequest{JobId: "job-1"}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetJob().GetJobId()).To(Equal("job-1"))
		})
	})

	Context("ListJobs", func() {
		It("filters jobs for non-admin users", func() {
			pipeline := &mockPipeline{
				listJobsFn: func(_ context.Context, _ *connect.Request[pb.ListJobsRequest]) (*connect.Response[pb.ListJobsResponse], error) {
					return connect.NewResponse(&pb.ListJobsResponse{
						Jobs: []*pb.PipelineJob{
							{JobId: "job-1", Audit: &pb.AuditMetadata{CreatedBy: "user-a"}},
							{JobId: "job-2", Audit: &pb.AuditMetadata{CreatedBy: "user-b"}},
							{JobId: "job-3", Audit: &pb.AuditMetadata{CreatedBy: "user-a"}},
						},
					}), nil
				},
			}
			svc := newTestService(gateway.WithPipelineBackend(pipeline))

			ctx := userCtx("user-a")
			resp, err := svc.ListJobs(ctx, connect.NewRequest(&pb.ListJobsRequest{}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetJobs()).To(HaveLen(2))
			for _, job := range resp.Msg.GetJobs() {
				Expect(job.GetAudit().GetCreatedBy()).To(Equal("user-a"))
			}
		})

		It("returns all jobs for admin", func() {
			pipeline := &mockPipeline{
				listJobsFn: func(_ context.Context, _ *connect.Request[pb.ListJobsRequest]) (*connect.Response[pb.ListJobsResponse], error) {
					return connect.NewResponse(&pb.ListJobsResponse{
						Jobs: []*pb.PipelineJob{
							{JobId: "job-1", Audit: &pb.AuditMetadata{CreatedBy: "user-a"}},
							{JobId: "job-2", Audit: &pb.AuditMetadata{CreatedBy: "user-b"}},
							{JobId: "job-3", Audit: &pb.AuditMetadata{CreatedBy: "user-c"}},
						},
					}), nil
				},
			}
			svc := newTestService(gateway.WithPipelineBackend(pipeline))

			resp, err := svc.ListJobs(adminCtx(), connect.NewRequest(&pb.ListJobsRequest{}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetJobs()).To(HaveLen(3))
		})
	})

	Context("ownership checks of cancel and retry", func() {
		It("never read cache summaries, so they succeed while those reads fail", func() {
			backend := newSummaryBackend("user-a")
			backend.failSummaries = true
			svc := newTestService(gateway.WithPipelineBackend(backend))
			_, err := svc.CancelJob(userCtx("user-a"), connect.NewRequest(&pb.CancelJobRequest{JobId: "job-1"}))
			Expect(err).NotTo(HaveOccurred())
			_, err = svc.RetryJob(userCtx("user-a"), connect.NewRequest(&pb.RetryJobRequest{JobId: "job-1"}))
			Expect(err).NotTo(HaveOccurred())
			Expect(backend.includes()).To(HaveEach(BeFalse()))
		})
	})

	Context("CancelJob", func() {
		It("allows owner to cancel own job", func() {
			pipeline := ownedJobBackend("user-a")
			svc := newTestService(gateway.WithPipelineBackend(pipeline))

			ctx := userCtx("user-a")
			resp, err := svc.CancelJob(ctx, connect.NewRequest(&pb.CancelJobRequest{JobId: "job-1"}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetCancelled()).To(BeTrue())
		})

		It("returns PermissionDenied for non-owner", func() {
			pipeline := ownedJobBackend("user-a")
			svc := newTestService(gateway.WithPipelineBackend(pipeline))

			ctx := userCtx("user-b")
			_, err := svc.CancelJob(ctx, connect.NewRequest(&pb.CancelJobRequest{JobId: "job-1"}))
			Expect(err).To(HaveOccurred())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied))
		})

		It("allows admin to cancel any job", func() {
			pipeline := ownedJobBackend("user-a")
			svc := newTestService(gateway.WithPipelineBackend(pipeline))

			resp, err := svc.CancelJob(adminCtx(), connect.NewRequest(&pb.CancelJobRequest{JobId: "job-1"}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetCancelled()).To(BeTrue())
		})
	})

	Context("RetryJob", func() {
		// retryingBackend is a backend whose job-1 was created by createdBy
		// and that records the RetryJob requests it receives.
		retryingBackend := func(createdBy string) (*mockPipeline, *[]*pb.RetryJobRequest) {
			var got []*pb.RetryJobRequest
			pipeline := ownedJobBackend(createdBy)
			pipeline.retryJobFn = func(_ context.Context, req *connect.Request[pb.RetryJobRequest]) (*connect.Response[pb.RetryJobResponse], error) {
				got = append(got, req.Msg)
				return connect.NewResponse(&pb.RetryJobResponse{NewJobId: "job-1"}), nil
			}
			return pipeline, &got
		}

		It("lets the owner retry its job from its failure, in the caller's tenant whatever the request says", func() {
			pipeline, got := retryingBackend("user-a")
			svc := newTestService(gateway.WithPipelineBackend(pipeline))

			resp, err := svc.RetryJob(userCtx("user-a"), connect.NewRequest(&pb.RetryJobRequest{
				JobId: "job-1", RetryFromFailure: true,
				TenantContext: &pb.TenantContext{TenantId: "someone-else"},
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetNewJobId()).To(Equal("job-1"))
			Expect(*got).To(HaveLen(1))
			Expect((*got)[0].GetRetryFromFailure()).To(BeTrue())
			Expect((*got)[0].GetTenantContext().GetTenantId()).To(Equal("tenant-a"))
		})

		It("refuses a caller that does not own the job, retrying nothing", func() {
			pipeline, got := retryingBackend("user-a")
			svc := newTestService(gateway.WithPipelineBackend(pipeline))

			_, err := svc.RetryJob(userCtx("user-b"), connect.NewRequest(&pb.RetryJobRequest{JobId: "job-1", RetryFromFailure: true}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied))
			Expect(*got).To(BeEmpty())
		})

		It("lets an admin retry any job", func() {
			pipeline, got := retryingBackend("user-a")
			svc := newTestService(gateway.WithPipelineBackend(pipeline))

			_, err := svc.RetryJob(adminCtx(), connect.NewRequest(&pb.RetryJobRequest{JobId: "job-1"}))
			Expect(err).NotTo(HaveOccurred())
			Expect(*got).To(HaveLen(1))
		})

		It("requires a job_id", func() {
			pipeline, got := retryingBackend("user-a")
			svc := newTestService(gateway.WithPipelineBackend(pipeline))

			_, err := svc.RetryJob(userCtx("user-a"), connect.NewRequest(&pb.RetryJobRequest{}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(*got).To(BeEmpty())
		})

		It("passes the pipeline's refusal through with its message, so the caller learns what to do", func() {
			pipeline := ownedJobBackend("user-a")
			pipeline.retryJobFn = func(context.Context, *connect.Request[pb.RetryJobRequest]) (*connect.Response[pb.RetryJobResponse], error) {
				return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("cannot retry job job-1 from its failure: start a new job"))
			}
			svc := newTestService(gateway.WithPipelineBackend(pipeline))

			_, err := svc.RetryJob(userCtx("user-a"), connect.NewRequest(&pb.RetryJobRequest{JobId: "job-1", RetryFromFailure: true}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
			Expect(err.Error()).To(ContainSubstring("start a new job"))
		})
	})
})

var _ = Describe("SubmitDocument", func() {
	It("imports the document and returns its catalog without creating a job", func() {
		createJobCalled := false
		svc := newTestService(gateway.WithPipelineBackend(&mockPipeline{
			createJobFn: func(context.Context, *connect.Request[pb.CreateJobRequest]) (*connect.Response[pb.CreateJobResponse], error) {
				createJobCalled = true
				return connect.NewResponse(&pb.CreateJobResponse{JobId: "job-1"}), nil
			},
		}))

		resp, err := svc.SubmitDocument(userCtx("user-a"), connect.NewRequest(&pb.SubmitDocumentRequest{
			Source: &pb.SubmitDocumentRequest_Content{Content: []byte("test doc")},
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.GetDocumentId()).To(Equal("doc-1"))
		Expect(resp.Msg.GetCatalogId()).To(Equal("cat-1"))
		Expect(createJobCalled).To(BeFalse())
	})

	It("copies the parse response's textless controls, uncapped, into the response", func() {
		var textless []*pb.TextlessControl
		for i := 0; i < 25; i++ {
			textless = append(textless, &pb.TextlessControl{Identifier: fmt.Sprintf("C-%d", i), PartNames: []string{"overview"}})
		}
		svc := newTestService(gateway.WithCatalogBackend(&mockCatalog{
			parseFn: func(context.Context, *connect.Request[pb.ParseCatalogRequest]) (*connect.Response[pb.ParseCatalogResponse], error) {
				return connect.NewResponse(&pb.ParseCatalogResponse{CatalogId: "cat-1", TextlessControls: textless}), nil
			},
		}))

		submit, err := svc.SubmitDocument(userCtx("user-a"), connect.NewRequest(&pb.SubmitDocumentRequest{
			Source: &pb.SubmitDocumentRequest_Content{Content: []byte("test doc")},
		}))
		Expect(err).NotTo(HaveOccurred())
		streamed, err := gateway.ExportHandleStreamedDocument(svc, userCtx("user-a"), time.Now(), &pb.StreamDocumentMetadata{}, []byte("test doc"))
		Expect(err).NotTo(HaveOccurred())

		for _, resp := range []*pb.SubmitDocumentResponse{submit.Msg, streamed.Msg} {
			Expect(resp.GetTextlessControls()).To(HaveLen(25))
			Expect(resp.GetTextlessControls()[24].GetIdentifier()).To(Equal("C-24"))
			Expect(resp.GetTextlessControls()[0].GetPartNames()).To(Equal([]string{"overview"}))
		}
	})

	// A plain backend error is withheld as Internal; backend_errors_bdd_test.go
	// covers the message, the log and the pass-through of actionable codes.
	DescribeTable("fails with Internal when a chained backend fails internally",
		func(opt gateway.ServiceOption, wantCode connect.Code) {
			svc := newTestService(opt)

			_, err := svc.SubmitDocument(userCtx("user-a"), connect.NewRequest(&pb.SubmitDocumentRequest{
				Source: &pb.SubmitDocumentRequest_Content{Content: []byte("test doc")},
			}))
			Expect(err).To(HaveOccurred())
			Expect(connect.CodeOf(err)).To(Equal(wantCode))
		},
		Entry("ingestion backend fails", gateway.WithIngestionBackend(&mockIngestion{
			convertFn: func(context.Context, *connect.Request[pb.ConvertDocumentRequest]) (*connect.Response[pb.ConvertDocumentResponse], error) {
				return nil, errors.New("ingestion failure")
			},
		}), connect.CodeInternal),
		Entry("catalog backend fails", gateway.WithCatalogBackend(&mockCatalog{
			parseFn: func(context.Context, *connect.Request[pb.ParseCatalogRequest]) (*connect.Response[pb.ParseCatalogResponse], error) {
				return nil, connect.NewError(connect.CodeInternal, errors.New("catalog unavailable"))
			},
		}), connect.CodeInternal),
	)

	It("imports without a pipeline backend", func() {
		svc := gateway.NewService(gateway.WithIngestionBackend(&mockIngestion{}), gateway.WithCatalogBackend(&mockCatalog{}))

		resp, err := svc.SubmitDocument(userCtx("user-a"), connect.NewRequest(&pb.SubmitDocumentRequest{
			Source: &pb.SubmitDocumentRequest_Content{Content: []byte("test doc")},
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.GetCatalogId()).To(Equal("cat-1"))
	})

	It("returns error when no source provided", func() {
		svc := newTestService()
		ctx := userCtx("user-a")
		_, err := svc.SubmitDocument(ctx, connect.NewRequest(&pb.SubmitDocumentRequest{}))
		Expect(err).To(HaveOccurred())
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
	})

	DescribeTable("discards the stored document only when the catalog rejected it",
		func(parseCode connect.Code, parseMsg string, wantDiscard bool) {
			var discarded []string
			ing := &mockIngestion{
				discardFn: func(_ context.Context, documentID string) error {
					discarded = append(discarded, documentID)
					return nil
				},
			}
			cat := &mockCatalog{
				parseFn: func(context.Context, *connect.Request[pb.ParseCatalogRequest]) (*connect.Response[pb.ParseCatalogResponse], error) {
					return nil, connect.NewError(parseCode, errors.New(parseMsg))
				},
			}
			svc := newTestService(gateway.WithIngestionBackend(ing), gateway.WithCatalogBackend(cat))

			_, err := svc.SubmitDocument(userCtx("user-a"), connect.NewRequest(&pb.SubmitDocumentRequest{
				Source: &pb.SubmitDocumentRequest_Content{Content: []byte("not a catalog")},
			}))
			Expect(connect.CodeOf(err)).To(Equal(parseCode))
			if wantDiscard {
				Expect(discarded).To(Equal([]string{"doc-1"}))
			} else {
				Expect(discarded).To(BeEmpty(), "the catalog may reference the blob after an uncertain failure")
			}
		},
		Entry("invalid content", connect.CodeInvalidArgument, "parse failed", true),
		Entry("unsupported format", connect.CodeFailedPrecondition, "parse failed", true),
		Entry("unimplemented format", connect.CodeUnimplemented, "parse failed", true),
		Entry("internal error after possible persistence", connect.CodeInternal, "parse failed", false),
		Entry("catalog unreachable", connect.CodeUnavailable, "parse failed", false),
		Entry("catalog stored but graph projection not queued, so its rows reference the document",
			connect.CodeUnavailable,
			"catalog 0123456789abcdef was stored but its graph projection could not be queued: the event bus is unavailable. Re-import the same document to retry",
			false),
	)

	It("returns the parse error when discarding the document also fails", func() {
		ing := &mockIngestion{
			discardFn: func(context.Context, string) error { return errors.New("storage down") },
		}
		cat := &mockCatalog{
			parseFn: func(context.Context, *connect.Request[pb.ParseCatalogRequest]) (*connect.Response[pb.ParseCatalogResponse], error) {
				return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("parse OSCAL: bad json"))
			},
		}
		svc := newTestService(gateway.WithIngestionBackend(ing), gateway.WithCatalogBackend(cat))

		_, err := svc.SubmitDocument(userCtx("user-a"), connect.NewRequest(&pb.SubmitDocumentRequest{
			Source: &pb.SubmitDocumentRequest_Content{Content: []byte("not a catalog")},
		}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		Expect(err.Error()).To(ContainSubstring("parse OSCAL: bad json"))
		Expect(err.Error()).NotTo(ContainSubstring("storage down"))
	})

	It("discards the document even when the request context is already cancelled", func() {
		discardCtxErr := errors.New("discard not called")
		ing := &mockIngestion{
			discardFn: func(ctx context.Context, _ string) error {
				discardCtxErr = ctx.Err()
				return nil
			},
		}
		ctx, cancel := context.WithCancel(userCtx("user-a"))
		cat := &mockCatalog{
			parseFn: func(context.Context, *connect.Request[pb.ParseCatalogRequest]) (*connect.Response[pb.ParseCatalogResponse], error) {
				cancel()
				return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("parse failed"))
			},
		}
		svc := newTestService(gateway.WithIngestionBackend(ing), gateway.WithCatalogBackend(cat))

		_, err := svc.SubmitDocument(ctx, connect.NewRequest(&pb.SubmitDocumentRequest{
			Source: &pb.SubmitDocumentRequest_Content{Content: []byte("not a catalog")},
		}))
		Expect(err).To(HaveOccurred())
		Expect(discardCtxErr).NotTo(HaveOccurred(), "a client disconnect must not strand the blob")
	})

	DescribeTable("rejects empty content before it reaches ingestion storage",
		func(content []byte) {
			convertCalls := 0
			ing := &mockIngestion{
				convertFn: func(context.Context, *connect.Request[pb.ConvertDocumentRequest]) (*connect.Response[pb.ConvertDocumentResponse], error) {
					convertCalls++
					return connect.NewResponse(&pb.ConvertDocumentResponse{DocumentId: "doc-1"}), nil
				},
			}
			svc := newTestService(gateway.WithIngestionBackend(ing))

			_, err := svc.SubmitDocument(userCtx("user-a"), connect.NewRequest(&pb.SubmitDocumentRequest{
				Source: &pb.SubmitDocumentRequest_Content{Content: content},
			}))
			Expect(err).To(HaveOccurred())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(err.Error()).To(ContainSubstring("document is empty"))
			Expect(err.Error()).To(ContainSubstring("Send the catalog file's contents"))
			Expect(convertCalls).To(BeZero(), "empty content must not be handed to ingestion, which stores it")
		},
		Entry("nil content", nil),
		Entry("zero-length content", []byte{}),
	)
})

var _ = Describe("Graph handlers", func() {
	Context("QueryGraph", func() {
		It("returns PermissionDenied for non-admin", func() {
			svc := newTestService()
			ctx := userCtx("user-a")
			_, err := svc.QueryGraph(ctx, connect.NewRequest(&pb.QueryGraphRequest{Cypher: "MATCH (n) RETURN n"}))
			Expect(err).To(HaveOccurred())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied))
		})

		It("succeeds for admin", func() {
			graph := &mockGraph{
				queryFn: func(_ context.Context, _ *connect.Request[pb.QueryRequest]) (*connect.Response[pb.QueryResponse], error) {
					return connect.NewResponse(&pb.QueryResponse{RowCount: 1}), nil
				},
			}
			svc := newTestService(gateway.WithGraphBackend(graph))

			resp, err := svc.QueryGraph(adminCtx(), connect.NewRequest(&pb.QueryGraphRequest{Cypher: "MATCH (n) RETURN n"}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetResponse().GetRowCount()).To(Equal(int32(1)))
		})
	})

	Context("ListJobMappings", func() {
		It("returns Unauthenticated without an identity", func() {
			svc := newTestService(gateway.WithMappingsBackend(&mockMappings{}))
			_, err := svc.ListJobMappings(context.Background(), connect.NewRequest(&pb.ListJobMappingsRequest{JobId: "job-1"}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated))
		})

		It("returns Unavailable when the mappings backend is nil", func() {
			svc := newTestService()
			_, err := svc.ListJobMappings(userCtx("user-a"), connect.NewRequest(&pb.ListJobMappingsRequest{JobId: "job-1"}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnavailable))
		})

		It("overwrites a client-supplied tenant_context with the identity's tenant", func() {
			m := &mockMappings{}
			svc := newTestService(gateway.WithMappingsBackend(m))
			_, err := svc.ListJobMappings(userCtx("user-a"), connect.NewRequest(&pb.ListJobMappingsRequest{
				JobId:         "job-1",
				TenantContext: &pb.TenantContext{TenantId: "tenant-evil"},
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(m.got.GetTenantContext().GetTenantId()).To(Equal("tenant-a"))
			Expect(m.got.GetJobId()).To(Equal("job-1"))
		})

		It("passes backend errors through with their code", func() {
			m := &mockMappings{err: connect.NewError(connect.CodeNotFound, errors.New("job not found"))}
			svc := newTestService(gateway.WithMappingsBackend(m))
			_, err := svc.ListJobMappings(userCtx("user-a"), connect.NewRequest(&pb.ListJobMappingsRequest{JobId: "job-1"}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
		})

		It("returns the backend's mappings", func() {
			m := &mockMappings{resp: &pb.ListJobMappingsResponse{Mappings: []*pb.ControlMapping{{MappingId: "m1"}}}}
			svc := newTestService(gateway.WithMappingsBackend(m))
			resp, err := svc.ListJobMappings(userCtx("user-a"), connect.NewRequest(&pb.ListJobMappingsRequest{JobId: "job-1"}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetMappings()).To(HaveLen(1))
			Expect(resp.Msg.GetMappings()[0].GetMappingId()).To(Equal("m1"))
		})
	})

	Context("ListJobCandidates", func() {
		It("returns Unauthenticated without an identity", func() {
			svc := newTestService(gateway.WithCandidatesBackend(&mockCandidates{}))
			_, err := svc.ListJobCandidates(context.Background(), connect.NewRequest(&pb.ListJobCandidatesRequest{JobId: "job-1"}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated))
		})

		It("returns Unavailable when the candidates backend is nil", func() {
			svc := newTestService()
			_, err := svc.ListJobCandidates(userCtx("user-a"), connect.NewRequest(&pb.ListJobCandidatesRequest{JobId: "job-1"}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnavailable))
		})

		It("overwrites a client-supplied tenant_context with the identity's tenant", func() {
			m := &mockCandidates{}
			svc := newTestService(gateway.WithCandidatesBackend(m))
			_, err := svc.ListJobCandidates(userCtx("user-a"), connect.NewRequest(&pb.ListJobCandidatesRequest{
				JobId:         "job-1",
				ControlId:     "ctrl-1",
				TenantContext: &pb.TenantContext{TenantId: "tenant-evil"},
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(m.got.GetTenantContext().GetTenantId()).To(Equal("tenant-a"))
			Expect(m.got.GetJobId()).To(Equal("job-1"))
			Expect(m.got.GetControlId()).To(Equal("ctrl-1"))
		})

		It("passes backend errors through with their code", func() {
			m := &mockCandidates{err: connect.NewError(connect.CodeNotFound, errors.New("job not found"))}
			svc := newTestService(gateway.WithCandidatesBackend(m))
			_, err := svc.ListJobCandidates(userCtx("user-a"), connect.NewRequest(&pb.ListJobCandidatesRequest{JobId: "job-1"}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
		})

		It("returns the backend's candidates", func() {
			m := &mockCandidates{resp: &pb.ListJobCandidatesResponse{Candidates: []*pb.RelationshipCandidate{{SourceControlId: "s1", TargetControlId: "t1"}}}}
			svc := newTestService(gateway.WithCandidatesBackend(m))
			resp, err := svc.ListJobCandidates(userCtx("user-a"), connect.NewRequest(&pb.ListJobCandidatesRequest{JobId: "job-1"}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetCandidates()).To(HaveLen(1))
			Expect(resp.Msg.GetCandidates()[0].GetSourceControlId()).To(Equal("s1"))
		})
	})

	Context("GetControlMappings", func() {
		It("returns Unavailable when the mappings backend is nil", func() {
			svc := newTestService()
			_, err := svc.GetControlMappings(userCtx("user-a"), connect.NewRequest(&pb.GetControlMappingsRequest{ControlId: "ctrl-1"}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnavailable))
		})

		It("translates the request to a synthesized, all-jobs mappings query", func() {
			m := &mockMappings{}
			svc := newTestService(gateway.WithMappingsBackend(m))
			_, err := svc.GetControlMappings(userCtx("user-a"), connect.NewRequest(&pb.GetControlMappingsRequest{
				ControlId: "ctrl-1", MinConfidence: 0.7, Limit: 5,
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(m.got.GetTenantContext().GetTenantId()).To(Equal("tenant-a"))
			Expect(m.got.GetStage()).To(Equal(pb.MappingStage_MAPPING_STAGE_SYNTHESIZED))
			Expect(m.got.GetJobId()).To(BeEmpty())
			Expect(m.got.GetControlId()).To(Equal("ctrl-1"))
			Expect(m.got.GetMinConfidence()).To(BeNumerically("~", 0.7, 1e-6))
			Expect(m.got.GetPagination().GetPageSize()).To(Equal(int32(5)))
		})

		It("sends no pagination when limit is zero", func() {
			m := &mockMappings{}
			svc := newTestService(gateway.WithMappingsBackend(m))
			_, err := svc.GetControlMappings(userCtx("user-a"), connect.NewRequest(&pb.GetControlMappingsRequest{ControlId: "ctrl-1"}))
			Expect(err).NotTo(HaveOccurred())
			Expect(m.got.GetPagination()).To(BeNil())
		})

		It("returns the backend's mappings and page info", func() {
			m := &mockMappings{resp: &pb.ListJobMappingsResponse{
				Mappings: []*pb.ControlMapping{{MappingId: "m1"}},
				PageInfo: &pb.PageInfo{NextPageToken: "next"},
			}}
			svc := newTestService(gateway.WithMappingsBackend(m))
			resp, err := svc.GetControlMappings(userCtx("user-a"), connect.NewRequest(&pb.GetControlMappingsRequest{ControlId: "ctrl-1"}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetMappings()).To(HaveLen(1))
			Expect(resp.Msg.GetPageInfo().GetNextPageToken()).To(BeEmpty(), "the request has no page_token field, so a next token would be unusable")
		})

		It("keeps the total count when clearing the next page token", func() {
			m := &mockMappings{resp: &pb.ListJobMappingsResponse{
				PageInfo: &pb.PageInfo{NextPageToken: "next", TotalCount: 42},
			}}
			svc := newTestService(gateway.WithMappingsBackend(m))
			resp, err := svc.GetControlMappings(userCtx("user-a"), connect.NewRequest(&pb.GetControlMappingsRequest{ControlId: "ctrl-1"}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetPageInfo().GetTotalCount()).To(Equal(int64(42)))
		})

		DescribeTable("rejects an out-of-range limit before calling the backend",
			func(limit int32) {
				m := &mockMappings{}
				svc := newTestService(gateway.WithMappingsBackend(m))
				_, err := svc.GetControlMappings(userCtx("user-a"), connect.NewRequest(&pb.GetControlMappingsRequest{ControlId: "ctrl-1", Limit: limit}))
				Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
				Expect(err.Error()).To(ContainSubstring("limit must be between 0 and 1000"))
				Expect(m.got).To(BeNil())
			},
			Entry("above the maximum", int32(1001)),
			Entry("negative", int32(-1)),
		)

		It("accepts the maximum limit", func() {
			m := &mockMappings{}
			svc := newTestService(gateway.WithMappingsBackend(m))
			_, err := svc.GetControlMappings(userCtx("user-a"), connect.NewRequest(&pb.GetControlMappingsRequest{ControlId: "ctrl-1", Limit: 1000}))
			Expect(err).NotTo(HaveOccurred())
			Expect(m.got.GetPagination().GetPageSize()).To(Equal(int32(1000)))
		})

		It("passes backend errors through with their code", func() {
			m := &mockMappings{err: connect.NewError(connect.CodeInternal, errors.New("boom"))}
			svc := newTestService(gateway.WithMappingsBackend(m))
			_, err := svc.GetControlMappings(userCtx("user-a"), connect.NewRequest(&pb.GetControlMappingsRequest{ControlId: "ctrl-1"}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInternal))
		})
	})

	Context("FindSimilar", func() {
		It("succeeds for authenticated user", func() {
			svc := newTestService()
			ctx := userCtx("user-a")
			resp, err := svc.FindSimilar(ctx, connect.NewRequest(&pb.FindSimilarRequest{ControlId: "ctrl-1"}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())
		})

		It("forwards catalog_ids and the control ID to the graph backend", func() {
			var got *pb.SimilaritySearchRequest
			svc := newTestService(gateway.WithGraphBackend(&mockGraph{
				similaritySearchFn: func(_ context.Context, req *connect.Request[pb.SimilaritySearchRequest]) (*connect.Response[pb.SimilaritySearchResponse], error) {
					got = req.Msg
					return connect.NewResponse(&pb.SimilaritySearchResponse{}), nil
				},
			}))
			_, err := svc.FindSimilar(userCtx("user-a"), connect.NewRequest(&pb.FindSimilarRequest{
				ControlId:  "cat-a/ac-1",
				CatalogIds: []string{"cat-a", "cat-b"},
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(got.GetCatalogIds()).To(Equal([]string{"cat-a", "cat-b"}))
			Expect(got.GetControlId()).To(Equal("cat-a/ac-1"))
		})

		It("passes a backend NotFound through with its code", func() {
			svc := newTestService(gateway.WithGraphBackend(&mockGraph{
				similaritySearchFn: func(context.Context, *connect.Request[pb.SimilaritySearchRequest]) (*connect.Response[pb.SimilaritySearchResponse], error) {
					return nil, connect.NewError(connect.CodeNotFound, errors.New(`control "cat-a/ac-1" not found`))
				},
			}))
			_, err := svc.FindSimilar(userCtx("user-a"), connect.NewRequest(&pb.FindSimilarRequest{ControlId: "cat-a/ac-1"}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
		})
	})
})

var _ = Describe("StreamDocument", func() {
	// Note: These tests exercise handleStreamedDocument (the business logic)
	// rather than StreamDocument (the stream handler) because connect.ClientStream
	// is a concrete struct that requires a real server. Stream ordering enforcement
	// (metadata must be first, reject chunks before metadata) is validated in
	// integration tests.

	It("reassembles content and delegates to backend chain", func() {
		svc := newTestService()
		ctx := userCtx("user-a")

		meta := &pb.StreamDocumentMetadata{
			CatalogFormat: pb.CatalogFormat_CATALOG_FORMAT_OSCAL,
			CatalogName:   "test-catalog",
		}
		content := []byte("test document content for streaming")

		resp, err := gateway.ExportHandleStreamedDocument(svc, ctx, time.Now(), meta, content)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.GetDocumentId()).To(Equal("doc-1"))
	})

	It("returns the imported catalog without creating a job", func() {
		createJobCalled := false
		svc := newTestService(gateway.WithPipelineBackend(&mockPipeline{
			createJobFn: func(context.Context, *connect.Request[pb.CreateJobRequest]) (*connect.Response[pb.CreateJobResponse], error) {
				createJobCalled = true
				return connect.NewResponse(&pb.CreateJobResponse{JobId: "job-1"}), nil
			},
		}))
		resp, err := gateway.ExportHandleStreamedDocument(svc, userCtx("user-a"), time.Now(), &pb.StreamDocumentMetadata{}, []byte("test doc"))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.GetCatalogId()).To(Equal("cat-1"))
		Expect(createJobCalled).To(BeFalse())
	})

	It("returns the structure method the catalog backend reported", func() {
		svc := newTestService(gateway.WithCatalogBackend(&mockCatalog{
			parseFn: func(context.Context, *connect.Request[pb.ParseCatalogRequest]) (*connect.Response[pb.ParseCatalogResponse], error) {
				return connect.NewResponse(&pb.ParseCatalogResponse{CatalogId: "cat-1", IngestionStructureMethod: "table"}), nil
			},
		}))
		resp, err := gateway.ExportHandleStreamedDocument(svc, userCtx("user-a"), time.Now(), &pb.StreamDocumentMetadata{}, []byte("test doc"))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.GetIngestionStructureMethod()).To(Equal("table"))
	})

	It("returns error when metadata is nil", func() {
		svc := newTestService()
		ctx := userCtx("user-a")

		_, err := gateway.ExportHandleStreamedDocument(svc, ctx, time.Now(), nil, []byte("data"))
		Expect(err).To(HaveOccurred())
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
	})

	It("returns Unauthenticated when no identity in context", func() {
		svc := newTestService()

		meta := &pb.StreamDocumentMetadata{}
		_, err := gateway.ExportHandleStreamedDocument(svc, context.Background(), time.Now(), meta, []byte("data"))
		Expect(err).To(HaveOccurred())
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated))
	})

	It("propagates metadata to the catalog backend", func() {
		var capturedParseReq *pb.ParseCatalogRequest
		cat := &mockCatalog{
			parseFn: func(_ context.Context, req *connect.Request[pb.ParseCatalogRequest]) (*connect.Response[pb.ParseCatalogResponse], error) {
				capturedParseReq = req.Msg
				return connect.NewResponse(&pb.ParseCatalogResponse{CatalogId: "cat-1", Status: pb.JobStatus_JOB_STATUS_COMPLETED}), nil
			},
		}
		svc := newTestService(gateway.WithCatalogBackend(cat))
		ctx := userCtx("user-a")

		meta := &pb.StreamDocumentMetadata{
			CatalogFormat: pb.CatalogFormat_CATALOG_FORMAT_OSCAL,
			CatalogName:   "test-catalog",
		}

		resp, err := gateway.ExportHandleStreamedDocument(svc, ctx, time.Now(), meta, []byte("doc content"))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.GetDocumentId()).To(Equal("doc-1"))

		Expect(capturedParseReq).NotTo(BeNil())
		Expect(capturedParseReq.GetFormat()).To(Equal(pb.CatalogFormat_CATALOG_FORMAT_OSCAL))
		Expect(capturedParseReq.GetCatalogName()).To(Equal("test-catalog"))
	})

	It("returns Unavailable when backends are nil", func() {
		svc := gateway.NewService()
		ctx := userCtx("user-a")

		meta := &pb.StreamDocumentMetadata{}
		_, err := gateway.ExportHandleStreamedDocument(svc, ctx, time.Now(), meta, []byte("data"))
		Expect(err).To(HaveOccurred())
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnavailable))
	})
})

var _ = Describe("Feedback handlers", func() {
	Context("GetReviewQueue", func() {
		It("returns PermissionDenied naming the roles for a caller with neither reviewer nor admin", func() {
			reached := false
			svc := newTestService(gateway.WithFeedbackBackend(&mockFeedback{
				getReviewQueueFn: func(context.Context, *connect.Request[pb.GetReviewQueueRequest]) (*connect.Response[pb.GetReviewQueueResponse], error) {
					reached = true
					return connect.NewResponse(&pb.GetReviewQueueResponse{}), nil
				},
			}))
			_, err := svc.GetReviewQueue(userCtx("user-a"), connect.NewRequest(&pb.GetReviewQueueRequest{}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied))
			Expect(err.Error()).To(ContainSubstring("reviewer or admin role required"))
			Expect(reached).To(BeFalse())
		})

		DescribeTable("reaches the feedback backend",
			func(roles []string) {
				svc := newTestService()
				resp, err := svc.GetReviewQueue(ctxWithIdentity("caller", "tenant-a", roles),
					connect.NewRequest(&pb.GetReviewQueueRequest{}))
				Expect(err).NotTo(HaveOccurred())
				Expect(resp).NotTo(BeNil())
			},
			Entry("for an admin", []string{authn.RoleAdmin}),
			Entry("for a reviewer", []string{authn.RoleReviewer}),
		)
	})

	Context("SubmitVote", func() {
		It("succeeds for authenticated user", func() {
			svc := newTestService()
			ctx := userCtx("user-a")
			resp, err := svc.SubmitVote(ctx, connect.NewRequest(&pb.SubmitVoteRequest{MappingId: "map-1"}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetVoteId()).To(Equal("vote-1"))
		})
	})

	Context("RecordDetermination", func() {
		determinationReq := func() *connect.Request[pb.RecordDeterminationRequest] {
			return connect.NewRequest(&pb.RecordDeterminationRequest{
				TenantContext: &pb.TenantContext{TenantId: "spoofed-tenant"},
				MappingId:     "map-1",
				JobId:         "job-1",
			})
		}

		It("forwards the request with the authenticated tenant and returns the response", func() {
			var captured *pb.RecordDeterminationRequest
			svc := newTestService(gateway.WithFeedbackBackend(&mockFeedback{
				recordDetermFn: func(_ context.Context, req *connect.Request[pb.RecordDeterminationRequest]) (*connect.Response[pb.RecordDeterminationResponse], error) {
					captured = req.Msg
					return connect.NewResponse(&pb.RecordDeterminationResponse{DeterminationId: "det-1", MappingStatus: "invalid"}), nil
				},
			}))
			resp, err := svc.RecordDetermination(userCtx("user-a"), determinationReq())
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetDeterminationId()).To(Equal("det-1"))
			Expect(resp.Msg.GetMappingStatus()).To(Equal("invalid"))
			Expect(captured.GetTenantContext().GetTenantId()).To(Equal("tenant-a"))
			Expect(captured.GetJobId()).To(Equal("job-1"))
		})

		It("returns Unauthenticated without an identity", func() {
			_, err := newTestService().RecordDetermination(context.Background(), determinationReq())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated))
		})

		It("returns InvalidArgument for an empty mapping_id without reaching the backend", func() {
			called := false
			svc := newTestService(gateway.WithFeedbackBackend(&mockFeedback{
				recordDetermFn: func(context.Context, *connect.Request[pb.RecordDeterminationRequest]) (*connect.Response[pb.RecordDeterminationResponse], error) {
					called = true
					return connect.NewResponse(&pb.RecordDeterminationResponse{}), nil
				},
			}))
			_, err := svc.RecordDetermination(userCtx("user-a"), connect.NewRequest(&pb.RecordDeterminationRequest{JobId: "job-1"}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(err.Error()).To(ContainSubstring("mapping_id is required"))
			Expect(called).To(BeFalse())
		})

		It("returns Unavailable when the feedback backend is nil", func() {
			svc := newTestService(gateway.WithFeedbackBackend(nil))
			_, err := svc.RecordDetermination(userCtx("user-a"), determinationReq())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnavailable))
			Expect(err.Error()).To(ContainSubstring("feedback backend not configured"))
		})
	})
})

var _ = Describe("Required request arguments", func() {
	DescribeTable("returns InvalidArgument for a missing argument",
		func(ctx context.Context, call func(context.Context, *gateway.Service) error) {
			// Every backend method an argument-taking RPC could reach records its
			// name, so any entry fails if validation stops short-circuiting.
			// mockMappings has no hook to record from; it keeps the request in
			// got, which is checked after the call.
			var reached []string
			record := func(name string) { reached = append(reached, name) }
			mappings := &mockMappings{}
			candidates := &mockCandidates{}
			crosswalk := &mockCrosswalk{}
			svc := newTestService(
				gateway.WithMappingsBackend(mappings),
				gateway.WithCandidatesBackend(candidates),
				gateway.WithCrosswalkBackend(crosswalk),
				gateway.WithCatalogBackend(&mockCatalog{
					getCatalogFn: func(context.Context, *connect.Request[pb.GetCatalogRequest]) (*connect.Response[pb.GetCatalogResponse], error) {
						record("GetCatalog")
						return nil, errors.New("backend reached")
					},
					getControlFn: func(context.Context, *connect.Request[pb.GetControlRequest]) (*connect.Response[pb.GetControlResponse], error) {
						record("GetControl")
						return nil, errors.New("backend reached")
					},
					searchControlsFn: func(context.Context, *connect.Request[pb.SearchControlsRequest]) (*connect.Response[pb.SearchControlsResponse], error) {
						record("SearchControls")
						return nil, errors.New("backend reached")
					},
				}),
				gateway.WithPipelineBackend(&mockPipeline{
					getJobFn: func(context.Context, *connect.Request[pb.GetJobRequest]) (*connect.Response[pb.GetJobResponse], error) {
						record("GetJob")
						return nil, errors.New("backend reached")
					},
				}),
				gateway.WithGraphBackend(&mockGraph{
					queryFn: func(context.Context, *connect.Request[pb.QueryRequest]) (*connect.Response[pb.QueryResponse], error) {
						record("Query")
						return nil, errors.New("backend reached")
					},
					similaritySearchFn: func(context.Context, *connect.Request[pb.SimilaritySearchRequest]) (*connect.Response[pb.SimilaritySearchResponse], error) {
						record("SimilaritySearch")
						return nil, errors.New("backend reached")
					},
				}),
				gateway.WithFeedbackBackend(&mockFeedback{
					submitVoteFn: func(context.Context, *connect.Request[pb.SubmitVoteRequest]) (*connect.Response[pb.SubmitVoteResponse], error) {
						record("SubmitVote")
						return nil, errors.New("backend reached")
					},
					recordDetermFn: func(context.Context, *connect.Request[pb.RecordDeterminationRequest]) (*connect.Response[pb.RecordDeterminationResponse], error) {
						record("RecordDetermination")
						return nil, errors.New("backend reached")
					},
				}),
			)

			err := call(ctx, svc)
			Expect(err).To(HaveOccurred())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
			if mappings.got != nil {
				record("ListMappings")
			}
			if candidates.got != nil {
				record("ListCandidates")
			}
			if crosswalk.got != nil {
				record("GetCrosswalk")
			}
			Expect(reached).To(BeEmpty(), "validation must reject the request before any backend call")
		},
		Entry("empty catalog_id on GetCatalog", userCtx("user-1"), func(ctx context.Context, svc *gateway.Service) error {
			_, err := svc.GetCatalog(ctx, connect.NewRequest(&pb.GetCatalogRequest{CatalogId: ""}))
			return err
		}),
		Entry("empty control_id on GetControl", userCtx("user-1"), func(ctx context.Context, svc *gateway.Service) error {
			_, err := svc.GetControl(ctx, connect.NewRequest(&pb.GetControlRequest{ControlId: ""}))
			return err
		}),
		Entry("empty query on SearchControls", userCtx("user-1"), func(ctx context.Context, svc *gateway.Service) error {
			_, err := svc.SearchControls(ctx, connect.NewRequest(&pb.SearchControlsRequest{Query: ""}))
			return err
		}),
		Entry("empty catalog_id on ListCatalogControls", userCtx("user-1"), func(ctx context.Context, svc *gateway.Service) error {
			_, err := svc.ListCatalogControls(ctx, connect.NewRequest(&pb.ListCatalogControlsRequest{CatalogId: ""}))
			return err
		}),
		Entry("empty job_id on GetJob", userCtx("user-a"), func(ctx context.Context, svc *gateway.Service) error {
			_, err := svc.GetJob(ctx, connect.NewRequest(&pb.GetJobRequest{JobId: ""}))
			return err
		}),
		// QueryGraph checks the admin role before validating arguments, so only
		// an admin reaches the cypher check.
		Entry("empty cypher on QueryGraph (admin: RBAC precedes validation)", adminCtx(), func(ctx context.Context, svc *gateway.Service) error {
			_, err := svc.QueryGraph(ctx, connect.NewRequest(&pb.QueryGraphRequest{Cypher: ""}))
			return err
		}),
		Entry("empty job_id on ListJobMappings", userCtx("user-a"), func(ctx context.Context, svc *gateway.Service) error {
			_, err := svc.ListJobMappings(ctx, connect.NewRequest(&pb.ListJobMappingsRequest{}))
			return err
		}),
		Entry("empty job_id on ListJobCandidates", userCtx("user-a"), func(ctx context.Context, svc *gateway.Service) error {
			_, err := svc.ListJobCandidates(ctx, connect.NewRequest(&pb.ListJobCandidatesRequest{}))
			return err
		}),
		Entry("no mode on GetCrosswalk", userCtx("user-a"), func(ctx context.Context, svc *gateway.Service) error {
			_, err := svc.GetCrosswalk(ctx, connect.NewRequest(&pb.GetCrosswalkRequest{}))
			return err
		}),
		Entry("empty control_id on GetControlMappings", userCtx("user-a"), func(ctx context.Context, svc *gateway.Service) error {
			_, err := svc.GetControlMappings(ctx, connect.NewRequest(&pb.GetControlMappingsRequest{}))
			return err
		}),
		Entry("empty control_id on FindSimilar", userCtx("user-a"), func(ctx context.Context, svc *gateway.Service) error {
			_, err := svc.FindSimilar(ctx, connect.NewRequest(&pb.FindSimilarRequest{ControlId: ""}))
			return err
		}),
		Entry("empty mapping_id on SubmitVote", userCtx("user-a"), func(ctx context.Context, svc *gateway.Service) error {
			_, err := svc.SubmitVote(ctx, connect.NewRequest(&pb.SubmitVoteRequest{MappingId: ""}))
			return err
		}),
		Entry("empty mapping_id on RecordDetermination", userCtx("user-a"), func(ctx context.Context, svc *gateway.Service) error {
			_, err := svc.RecordDetermination(ctx, connect.NewRequest(&pb.RecordDeterminationRequest{MappingId: ""}))
			return err
		}),
	)
})

var _ = Describe("Document version arguments through the gateway", func() {
	type importFn func(svc *gateway.Service, slug, label string) (*pb.SubmitDocumentResponse, error)
	submit := func(svc *gateway.Service, slug, label string) (*pb.SubmitDocumentResponse, error) {
		resp, err := svc.SubmitDocument(userCtx("user-a"), connect.NewRequest(&pb.SubmitDocumentRequest{
			Source:       &pb.SubmitDocumentRequest_Content{Content: []byte("doc")},
			DocumentSlug: slug,
			VersionLabel: label,
		}))
		if err != nil {
			return nil, err
		}
		return resp.Msg, nil
	}
	stream := func(svc *gateway.Service, slug, label string) (*pb.SubmitDocumentResponse, error) {
		resp, err := gateway.ExportHandleStreamedDocument(svc, userCtx("user-a"), time.Now(),
			&pb.StreamDocumentMetadata{DocumentSlug: slug, VersionLabel: label}, []byte("doc"))
		if err != nil {
			return nil, err
		}
		return resp.Msg, nil
	}

	DescribeTable("passes the slug and label to the catalog and returns the version results",
		func(run importFn) {
			var got *pb.ParseCatalogRequest
			report := &pb.ChangeReport{Added: []string{"ac-3"}, Removed: []string{"ac-2"}, Changed: []string{"ac-1"}, IdsUnstable: true}
			cat := &mockCatalog{
				parseFn: func(_ context.Context, req *connect.Request[pb.ParseCatalogRequest]) (*connect.Response[pb.ParseCatalogResponse], error) {
					got = req.Msg
					return connect.NewResponse(&pb.ParseCatalogResponse{
						CatalogId: "cat-2", Version: "nist-800-53 @ rev5 (2026-01-02 · abc123)",
						SupersededCatalogId: "cat-1", ChangeReport: report,
					}), nil
				},
			}
			svc := newTestService(gateway.WithCatalogBackend(cat))

			resp, err := run(svc, "nist-800-53", "rev5")
			Expect(err).NotTo(HaveOccurred())
			Expect(got.GetDocumentSlug()).To(Equal("nist-800-53"))
			Expect(got.GetVersionLabel()).To(Equal("rev5"))
			Expect(resp.GetVersion()).To(Equal("nist-800-53 @ rev5 (2026-01-02 · abc123)"))
			Expect(resp.GetSupersededCatalogId()).To(Equal("cat-1"))
			Expect(resp.GetChangeReport().GetAdded()).To(ConsistOf("ac-3"))
			Expect(resp.GetChangeReport().GetRemoved()).To(ConsistOf("ac-2"))
			Expect(resp.GetChangeReport().GetChanged()).To(ConsistOf("ac-1"))
			Expect(resp.GetChangeReport().GetIdsUnstable()).To(BeTrue())
		},
		Entry("SubmitDocument", importFn(submit)),
		Entry("StreamDocument", importFn(stream)),
	)

	DescribeTable("returns the backend's actionable refusal to the caller with its message intact",
		func(run importFn, code connect.Code, msg string) {
			cat := &mockCatalog{
				parseFn: func(context.Context, *connect.Request[pb.ParseCatalogRequest]) (*connect.Response[pb.ParseCatalogResponse], error) {
					return nil, connect.NewError(code, errors.New(msg))
				},
			}
			svc := newTestService(gateway.WithCatalogBackend(cat))

			_, err := run(svc, "nist-800-53", "")
			Expect(connect.CodeOf(err)).To(Equal(code))
			Expect(err.Error()).To(ContainSubstring(msg))
		},
		Entry("SubmitDocument, invalid slug", importFn(submit), connect.CodeInvalidArgument, `invalid document slug "NIST": use a-z0-9`),
		Entry("StreamDocument, invalid slug", importFn(stream), connect.CodeInvalidArgument, `invalid document slug "NIST": use a-z0-9`),
		Entry("SubmitDocument, content in another document", importFn(submit), connect.CodeFailedPrecondition, `already a version of document "other-doc"`),
		Entry("StreamDocument, content in another document", importFn(stream), connect.CodeFailedPrecondition, `already a version of document "other-doc"`),
	)
})

var _ = Describe("Ingestion mapping through the gateway", func() {
	type importFn func(svc *gateway.Service, mapping string) error
	submit := func(svc *gateway.Service, mapping string) error {
		_, err := svc.SubmitDocument(userCtx("user-a"), connect.NewRequest(&pb.SubmitDocumentRequest{
			Source:           &pb.SubmitDocumentRequest_Content{Content: []byte(`{"chapters":[]}`)},
			IngestionMapping: mapping,
		}))
		return err
	}
	stream := func(svc *gateway.Service, mapping string) error {
		_, err := gateway.ExportHandleStreamedDocument(svc, userCtx("user-a"), time.Now(),
			&pb.StreamDocumentMetadata{IngestionMapping: mapping}, []byte(`{"chapters":[]}`))
		return err
	}
	// recording returns an ingestion backend that accepts every request and
	// records the ones it receives.
	recording := func(got *[]*pb.ConvertDocumentRequest) *mockIngestion {
		return &mockIngestion{
			convertFn: func(_ context.Context, req *connect.Request[pb.ConvertDocumentRequest]) (*connect.Response[pb.ConvertDocumentResponse], error) {
				*got = append(*got, req.Msg)
				return connect.NewResponse(&pb.ConvertDocumentResponse{DocumentId: "doc-1"}), nil
			},
		}
	}

	DescribeTable("forwards the mapping to the ingestion backend",
		func(run importFn, mapping string) {
			var got []*pb.ConvertDocumentRequest
			svc := newTestService(gateway.WithIngestionBackend(recording(&got)))
			Expect(run(svc, mapping)).To(Succeed())
			Expect(got).To(HaveLen(1))
			Expect(got[0].GetMapping()).To(Equal(mapping))
			Expect(got[0].GetContent()).To(Equal([]byte(`{"chapters":[]}`)))
		},
		Entry("SubmitDocument", importFn(submit), "owasp-asvs-5.0.0-json"),
		Entry("StreamDocument", importFn(stream), "owasp-asvs-5.0.0-json"),
		Entry("SubmitDocument, a 128-character ID", importFn(submit), strings.Repeat("a", 128)),
		Entry("StreamDocument, a one-character ID", importFn(stream), "0"),
		Entry("SubmitDocument, no mapping for a document", importFn(submit), ""),
		Entry("StreamDocument, no mapping for a document", importFn(stream), ""),
	)

	DescribeTable("refuses a malformed mapping ID before calling the ingestion backend",
		func(run importFn, mapping, wantEcho string) {
			var got []*pb.ConvertDocumentRequest
			svc := newTestService(gateway.WithIngestionBackend(recording(&got)))
			err := run(svc, mapping)
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(err).To(MatchError(ContainSubstring("invalid mapping " + wantEcho)))
			Expect(err).To(MatchError(ContainSubstring("1-128 lowercase letters and digits, separated by single '.' or '-'")))
			Expect(err).To(MatchError(ContainSubstring("the ID of a mapping registered on the ingestion server")))
			Expect(err.Error()).NotTo(ContainSubstring("\n"), "the echo is escaped")
			Expect(len(err.Error())).To(BeNumerically("<", 600), "the echo is bounded")
			Expect(got).To(BeEmpty(), "the backend accepts any request, so an empty record proves none was sent")
		},
		Entry("SubmitDocument, uppercase", importFn(submit), "OWASP-ASVS", `"OWASP-ASVS"`),
		Entry("StreamDocument, uppercase", importFn(stream), "OWASP-ASVS", `"OWASP-ASVS"`),
		Entry("SubmitDocument, leading hyphen", importFn(submit), "-asvs", `"-asvs"`),
		Entry("SubmitDocument, a trailing separator", importFn(submit), "asvs-", `"asvs-"`),
		Entry("SubmitDocument, doubled separators", importFn(submit), "asvs..json", `"asvs..json"`),
		Entry("SubmitDocument, a path", importFn(submit), "../etc/passwd", `"../etc/passwd"`),
		Entry("StreamDocument, a newline", importFn(stream), "asvs\nx", `"asvs\nx"`),
		Entry("SubmitDocument, 129 characters", importFn(submit), strings.Repeat("a", 129), `"`+strings.Repeat("a", 64)+`..." (129 bytes)`),
		Entry("StreamDocument, 4096 characters", importFn(stream), strings.Repeat("x", 4096), `"`+strings.Repeat("x", 64)+`..." (4096 bytes)`),
	)

	DescribeTable("refuses a mapping combined with an explicit catalog format before calling the ingestion backend",
		func(_ pb.CatalogFormat, run func(svc *gateway.Service) error, wantFormat string) {
			var got []*pb.ConvertDocumentRequest
			svc := newTestService(gateway.WithIngestionBackend(recording(&got)))
			err := run(svc)
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(err).To(MatchError(ContainSubstring(`mapping "owasp-asvs-5.0.0-json" cannot be combined with catalog format ` + wantFormat)))
			Expect(err).To(MatchError(ContainSubstring("Leave the catalog format unspecified")))
			Expect(got).To(BeEmpty(), "the backend accepts any request, so an empty record proves none was sent")
		},
		Entry("SubmitDocument, OSCAL", pb.CatalogFormat_CATALOG_FORMAT_OSCAL, func(svc *gateway.Service) error {
			_, err := svc.SubmitDocument(userCtx("user-a"), connect.NewRequest(&pb.SubmitDocumentRequest{
				Source:           &pb.SubmitDocumentRequest_Content{Content: []byte(`{"chapters":[]}`)},
				CatalogFormat:    pb.CatalogFormat_CATALOG_FORMAT_OSCAL,
				IngestionMapping: "owasp-asvs-5.0.0-json",
			}))
			return err
		}, "CATALOG_FORMAT_OSCAL"),
		Entry("SubmitDocument, GEMARA", pb.CatalogFormat_CATALOG_FORMAT_GEMARA, func(svc *gateway.Service) error {
			_, err := svc.SubmitDocument(userCtx("user-a"), connect.NewRequest(&pb.SubmitDocumentRequest{
				Source:           &pb.SubmitDocumentRequest_Content{Content: []byte(`{"chapters":[]}`)},
				CatalogFormat:    pb.CatalogFormat_CATALOG_FORMAT_GEMARA,
				IngestionMapping: "owasp-asvs-5.0.0-json",
			}))
			return err
		}, "CATALOG_FORMAT_GEMARA"),
		Entry("StreamDocument, OSCAL", pb.CatalogFormat_CATALOG_FORMAT_OSCAL, func(svc *gateway.Service) error {
			_, err := gateway.ExportHandleStreamedDocument(svc, userCtx("user-a"), time.Now(),
				&pb.StreamDocumentMetadata{CatalogFormat: pb.CatalogFormat_CATALOG_FORMAT_OSCAL, IngestionMapping: "owasp-asvs-5.0.0-json"}, []byte(`{"chapters":[]}`))
			return err
		}, "CATALOG_FORMAT_OSCAL"),
		Entry("StreamDocument, GEMARA", pb.CatalogFormat_CATALOG_FORMAT_GEMARA, func(svc *gateway.Service) error {
			_, err := gateway.ExportHandleStreamedDocument(svc, userCtx("user-a"), time.Now(),
				&pb.StreamDocumentMetadata{CatalogFormat: pb.CatalogFormat_CATALOG_FORMAT_GEMARA, IngestionMapping: "owasp-asvs-5.0.0-json"}, []byte(`{"chapters":[]}`))
			return err
		}, "CATALOG_FORMAT_GEMARA"),
	)

	It("returns the ingestion backend's refusal of a mapping with its actionable message", func() {
		msg := `ingestion refused mapping "owasp-asvs-9-json": check the ID`
		ing := &mockIngestion{
			convertFn: func(context.Context, *connect.Request[pb.ConvertDocumentRequest]) (*connect.Response[pb.ConvertDocumentResponse], error) {
				return nil, connect.NewError(connect.CodeInvalidArgument, errors.New(msg))
			},
		}
		svc := newTestService(gateway.WithIngestionBackend(ing))
		err := submit(svc, "owasp-asvs-9-json")
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		Expect(err).To(MatchError(ContainSubstring(msg)))
	})
})
