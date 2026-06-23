//go:build !integration

package gateway_test

import (
	"context"
	"errors"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/gateway"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

type mockIngestion struct {
	convertFn func(context.Context, *pb.ConvertDocumentRequest) (*pb.ConvertDocumentResponse, error)
}

func (m *mockIngestion) ConvertDocument(ctx context.Context, req *pb.ConvertDocumentRequest) (*pb.ConvertDocumentResponse, error) {
	if m.convertFn != nil {
		return m.convertFn(ctx, req)
	}
	return &pb.ConvertDocumentResponse{DocumentId: "doc-1"}, nil
}

type mockCatalog struct {
	parseFn          func(context.Context, *pb.ParseCatalogRequest) (*pb.ParseCatalogResponse, error)
	listCatalogsFn   func(context.Context, *pb.ListCatalogsRequest) (*pb.ListCatalogsResponse, error)
	getCatalogFn     func(context.Context, *pb.GetCatalogRequest) (*pb.GetCatalogResponse, error)
	searchControlsFn func(context.Context, *pb.SearchControlsRequest) (*pb.SearchControlsResponse, error)
	getControlFn     func(context.Context, *pb.GetControlRequest) (*pb.GetControlResponse, error)
}

func (m *mockCatalog) ParseCatalog(ctx context.Context, req *pb.ParseCatalogRequest) (*pb.ParseCatalogResponse, error) {
	if m.parseFn != nil {
		return m.parseFn(ctx, req)
	}
	return &pb.ParseCatalogResponse{CatalogId: "cat-1", Status: pb.JobStatus_JOB_STATUS_COMPLETED}, nil
}

func (m *mockCatalog) ListCatalogs(ctx context.Context, req *pb.ListCatalogsRequest) (*pb.ListCatalogsResponse, error) {
	if m.listCatalogsFn != nil {
		return m.listCatalogsFn(ctx, req)
	}
	return &pb.ListCatalogsResponse{}, nil
}

func (m *mockCatalog) GetCatalog(ctx context.Context, req *pb.GetCatalogRequest) (*pb.GetCatalogResponse, error) {
	if m.getCatalogFn != nil {
		return m.getCatalogFn(ctx, req)
	}
	return &pb.GetCatalogResponse{}, nil
}

func (m *mockCatalog) SearchControls(ctx context.Context, req *pb.SearchControlsRequest) (*pb.SearchControlsResponse, error) {
	if m.searchControlsFn != nil {
		return m.searchControlsFn(ctx, req)
	}
	return &pb.SearchControlsResponse{}, nil
}

func (m *mockCatalog) GetControl(ctx context.Context, req *pb.GetControlRequest) (*pb.GetControlResponse, error) {
	if m.getControlFn != nil {
		return m.getControlFn(ctx, req)
	}
	return &pb.GetControlResponse{}, nil
}

type mockPipeline struct {
	createJobFn func(context.Context, *pb.CreateJobRequest) (*pb.CreateJobResponse, error)
	getJobFn    func(context.Context, *pb.GetJobRequest) (*pb.GetJobResponse, error)
	listJobsFn  func(context.Context, *pb.ListJobsRequest) (*pb.ListJobsResponse, error)
	cancelJobFn func(context.Context, *pb.CancelJobRequest) (*pb.CancelJobResponse, error)
}

func (m *mockPipeline) CreateJob(ctx context.Context, req *pb.CreateJobRequest) (*pb.CreateJobResponse, error) {
	if m.createJobFn != nil {
		return m.createJobFn(ctx, req)
	}
	return &pb.CreateJobResponse{JobId: "job-1", Status: pb.JobStatus_JOB_STATUS_PENDING}, nil
}

func (m *mockPipeline) GetJob(ctx context.Context, req *pb.GetJobRequest) (*pb.GetJobResponse, error) {
	if m.getJobFn != nil {
		return m.getJobFn(ctx, req)
	}
	return &pb.GetJobResponse{
		Job: &pb.PipelineJob{
			JobId: "job-1",
			Audit: &pb.AuditMetadata{CreatedBy: "user-a"},
		},
	}, nil
}

func (m *mockPipeline) ListJobs(ctx context.Context, req *pb.ListJobsRequest) (*pb.ListJobsResponse, error) {
	if m.listJobsFn != nil {
		return m.listJobsFn(ctx, req)
	}
	return &pb.ListJobsResponse{}, nil
}

func (m *mockPipeline) CancelJob(ctx context.Context, req *pb.CancelJobRequest) (*pb.CancelJobResponse, error) {
	if m.cancelJobFn != nil {
		return m.cancelJobFn(ctx, req)
	}
	return &pb.CancelJobResponse{Cancelled: true}, nil
}

type mockGraph struct {
	traverseFn         func(context.Context, *pb.TraverseRequest) (*pb.TraverseResponse, error)
	queryFn            func(context.Context, *pb.QueryRequest) (*pb.QueryResponse, error)
	similaritySearchFn func(context.Context, *pb.SimilaritySearchRequest) (*pb.SimilaritySearchResponse, error)
}

func (m *mockGraph) Traverse(ctx context.Context, req *pb.TraverseRequest) (*pb.TraverseResponse, error) {
	if m.traverseFn != nil {
		return m.traverseFn(ctx, req)
	}
	return &pb.TraverseResponse{}, nil
}

func (m *mockGraph) Query(ctx context.Context, req *pb.QueryRequest) (*pb.QueryResponse, error) {
	if m.queryFn != nil {
		return m.queryFn(ctx, req)
	}
	return &pb.QueryResponse{}, nil
}

func (m *mockGraph) SimilaritySearch(ctx context.Context, req *pb.SimilaritySearchRequest) (*pb.SimilaritySearchResponse, error) {
	if m.similaritySearchFn != nil {
		return m.similaritySearchFn(ctx, req)
	}
	return &pb.SimilaritySearchResponse{}, nil
}

type mockFeedback struct {
	submitVoteFn     func(context.Context, *pb.SubmitVoteRequest) (*pb.SubmitVoteResponse, error)
	getReviewQueueFn func(context.Context, *pb.GetReviewQueueRequest) (*pb.GetReviewQueueResponse, error)
}

func (m *mockFeedback) SubmitVote(ctx context.Context, req *pb.SubmitVoteRequest) (*pb.SubmitVoteResponse, error) {
	if m.submitVoteFn != nil {
		return m.submitVoteFn(ctx, req)
	}
	return &pb.SubmitVoteResponse{VoteId: "vote-1"}, nil
}

func (m *mockFeedback) GetReviewQueue(ctx context.Context, req *pb.GetReviewQueueRequest) (*pb.GetReviewQueueResponse, error) {
	if m.getReviewQueueFn != nil {
		return m.getReviewQueueFn(ctx, req)
	}
	return &pb.GetReviewQueueResponse{}, nil
}

type mockAdmin struct {
	healthCheckFn func(context.Context, *pb.HealthCheckRequest) (*pb.HealthCheckResponse, error)
}

func (m *mockAdmin) HealthCheck(ctx context.Context, req *pb.HealthCheckRequest) (*pb.HealthCheckResponse, error) {
	if m.healthCheckFn != nil {
		return m.healthCheckFn(ctx, req)
	}
	return &pb.HealthCheckResponse{Status: pb.HealthStatus_HEALTH_STATUS_HEALTHY}, nil
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
		resp, err := svc.Health(context.Background(), &pb.HealthRequest{})
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.GetStatus()).To(Equal(pb.HealthStatus_HEALTH_STATUS_HEALTHY))
	})

	It("returns error when admin backend is nil", func() {
		svc := newTestService(gateway.WithAdminBackend(nil))
		_, err := svc.Health(context.Background(), &pb.HealthRequest{})
		Expect(err).To(HaveOccurred())
		Expect(status.Code(err)).To(Equal(codes.Unavailable))
	})
})

var _ = Describe("Catalog handlers", func() {
	It("injects auth-derived TenantContext, ignoring client-supplied value", func() {
		var captured *pb.TenantContext
		cat := &mockCatalog{
			listCatalogsFn: func(_ context.Context, req *pb.ListCatalogsRequest) (*pb.ListCatalogsResponse, error) {
				captured = req.GetTenantContext()
				return &pb.ListCatalogsResponse{}, nil
			},
		}
		svc := newTestService(gateway.WithCatalogBackend(cat))

		ctx := ctxWithIdentity("user-1", "real-tenant", []string{"user"})
		req := &pb.ListCatalogsRequest{
			TenantContext: &pb.TenantContext{TenantId: "spoofed-tenant"},
		}

		_, err := svc.ListCatalogs(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(captured).NotTo(BeNil())
		Expect(captured.GetTenantId()).To(Equal("real-tenant"))
	})

	It("returns InvalidArgument for empty catalog_id on GetCatalog", func() {
		svc := newTestService()
		ctx := userCtx("user-1")
		_, err := svc.GetCatalog(ctx, &pb.GetCatalogRequest{CatalogId: ""})
		Expect(err).To(HaveOccurred())
		Expect(status.Code(err)).To(Equal(codes.InvalidArgument))
	})

	It("returns InvalidArgument for empty control_id on GetControl", func() {
		svc := newTestService()
		ctx := userCtx("user-1")
		_, err := svc.GetControl(ctx, &pb.GetControlRequest{ControlId: ""})
		Expect(err).To(HaveOccurred())
		Expect(status.Code(err)).To(Equal(codes.InvalidArgument))
	})

	It("returns InvalidArgument for empty query on SearchControls", func() {
		svc := newTestService()
		ctx := userCtx("user-1")
		_, err := svc.SearchControls(ctx, &pb.SearchControlsRequest{Query: ""})
		Expect(err).To(HaveOccurred())
		Expect(status.Code(err)).To(Equal(codes.InvalidArgument))
	})

	It("returns Unauthenticated when no identity in context", func() {
		svc := newTestService()
		_, err := svc.ListCatalogs(context.Background(), &pb.ListCatalogsRequest{})
		Expect(err).To(HaveOccurred())
		Expect(status.Code(err)).To(Equal(codes.Unauthenticated))
	})

	It("propagates backend errors", func() {
		backendErr := status.Error(codes.Internal, "db connection lost")
		cat := &mockCatalog{
			listCatalogsFn: func(context.Context, *pb.ListCatalogsRequest) (*pb.ListCatalogsResponse, error) {
				return nil, backendErr
			},
		}
		svc := newTestService(gateway.WithCatalogBackend(cat))

		ctx := userCtx("user-1")
		_, err := svc.ListCatalogs(ctx, &pb.ListCatalogsRequest{})
		Expect(err).To(HaveOccurred())
		Expect(status.Code(err)).To(Equal(codes.Internal))
	})
})

var _ = Describe("Job handlers", func() {
	Context("GetJob", func() {
		It("returns job to its owner", func() {
			pipeline := &mockPipeline{
				getJobFn: func(_ context.Context, _ *pb.GetJobRequest) (*pb.GetJobResponse, error) {
					return &pb.GetJobResponse{
						Job: &pb.PipelineJob{
							JobId: "job-1",
							Audit: &pb.AuditMetadata{CreatedBy: "user-a"},
						},
					}, nil
				},
			}
			svc := newTestService(gateway.WithPipelineBackend(pipeline))

			ctx := userCtx("user-a")
			resp, err := svc.GetJob(ctx, &pb.GetJobRequest{JobId: "job-1"})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.GetJob().GetJobId()).To(Equal("job-1"))
		})

		It("returns PermissionDenied when non-owner non-admin accesses job", func() {
			pipeline := &mockPipeline{
				getJobFn: func(_ context.Context, _ *pb.GetJobRequest) (*pb.GetJobResponse, error) {
					return &pb.GetJobResponse{
						Job: &pb.PipelineJob{
							JobId: "job-1",
							Audit: &pb.AuditMetadata{CreatedBy: "user-a"},
						},
					}, nil
				},
			}
			svc := newTestService(gateway.WithPipelineBackend(pipeline))

			ctx := userCtx("user-b")
			_, err := svc.GetJob(ctx, &pb.GetJobRequest{JobId: "job-1"})
			Expect(err).To(HaveOccurred())
			Expect(status.Code(err)).To(Equal(codes.PermissionDenied))
		})

		It("allows admin to access any job", func() {
			pipeline := &mockPipeline{
				getJobFn: func(_ context.Context, _ *pb.GetJobRequest) (*pb.GetJobResponse, error) {
					return &pb.GetJobResponse{
						Job: &pb.PipelineJob{
							JobId: "job-1",
							Audit: &pb.AuditMetadata{CreatedBy: "user-a"},
						},
					}, nil
				},
			}
			svc := newTestService(gateway.WithPipelineBackend(pipeline))

			resp, err := svc.GetJob(adminCtx(), &pb.GetJobRequest{JobId: "job-1"})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.GetJob().GetJobId()).To(Equal("job-1"))
		})

		It("returns InvalidArgument for empty job_id", func() {
			svc := newTestService()
			ctx := userCtx("user-a")
			_, err := svc.GetJob(ctx, &pb.GetJobRequest{JobId: ""})
			Expect(err).To(HaveOccurred())
			Expect(status.Code(err)).To(Equal(codes.InvalidArgument))
		})
	})

	Context("ListJobs", func() {
		It("filters jobs for non-admin users", func() {
			pipeline := &mockPipeline{
				listJobsFn: func(_ context.Context, _ *pb.ListJobsRequest) (*pb.ListJobsResponse, error) {
					return &pb.ListJobsResponse{
						Jobs: []*pb.PipelineJob{
							{JobId: "job-1", Audit: &pb.AuditMetadata{CreatedBy: "user-a"}},
							{JobId: "job-2", Audit: &pb.AuditMetadata{CreatedBy: "user-b"}},
							{JobId: "job-3", Audit: &pb.AuditMetadata{CreatedBy: "user-a"}},
						},
					}, nil
				},
			}
			svc := newTestService(gateway.WithPipelineBackend(pipeline))

			ctx := userCtx("user-a")
			resp, err := svc.ListJobs(ctx, &pb.ListJobsRequest{})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.GetJobs()).To(HaveLen(2))
			for _, job := range resp.GetJobs() {
				Expect(job.GetAudit().GetCreatedBy()).To(Equal("user-a"))
			}
		})

		It("returns all jobs for admin", func() {
			pipeline := &mockPipeline{
				listJobsFn: func(_ context.Context, _ *pb.ListJobsRequest) (*pb.ListJobsResponse, error) {
					return &pb.ListJobsResponse{
						Jobs: []*pb.PipelineJob{
							{JobId: "job-1", Audit: &pb.AuditMetadata{CreatedBy: "user-a"}},
							{JobId: "job-2", Audit: &pb.AuditMetadata{CreatedBy: "user-b"}},
							{JobId: "job-3", Audit: &pb.AuditMetadata{CreatedBy: "user-c"}},
						},
					}, nil
				},
			}
			svc := newTestService(gateway.WithPipelineBackend(pipeline))

			resp, err := svc.ListJobs(adminCtx(), &pb.ListJobsRequest{})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.GetJobs()).To(HaveLen(3))
		})
	})

	Context("CancelJob", func() {
		It("allows owner to cancel own job", func() {
			pipeline := &mockPipeline{
				getJobFn: func(_ context.Context, _ *pb.GetJobRequest) (*pb.GetJobResponse, error) {
					return &pb.GetJobResponse{
						Job: &pb.PipelineJob{
							JobId: "job-1",
							Audit: &pb.AuditMetadata{CreatedBy: "user-a"},
						},
					}, nil
				},
			}
			svc := newTestService(gateway.WithPipelineBackend(pipeline))

			ctx := userCtx("user-a")
			resp, err := svc.CancelJob(ctx, &pb.CancelJobRequest{JobId: "job-1"})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.GetCancelled()).To(BeTrue())
		})

		It("returns PermissionDenied for non-owner", func() {
			pipeline := &mockPipeline{
				getJobFn: func(_ context.Context, _ *pb.GetJobRequest) (*pb.GetJobResponse, error) {
					return &pb.GetJobResponse{
						Job: &pb.PipelineJob{
							JobId: "job-1",
							Audit: &pb.AuditMetadata{CreatedBy: "user-a"},
						},
					}, nil
				},
			}
			svc := newTestService(gateway.WithPipelineBackend(pipeline))

			ctx := userCtx("user-b")
			_, err := svc.CancelJob(ctx, &pb.CancelJobRequest{JobId: "job-1"})
			Expect(err).To(HaveOccurred())
			Expect(status.Code(err)).To(Equal(codes.PermissionDenied))
		})

		It("allows admin to cancel any job", func() {
			pipeline := &mockPipeline{
				getJobFn: func(_ context.Context, _ *pb.GetJobRequest) (*pb.GetJobResponse, error) {
					return &pb.GetJobResponse{
						Job: &pb.PipelineJob{
							JobId: "job-1",
							Audit: &pb.AuditMetadata{CreatedBy: "user-a"},
						},
					}, nil
				},
			}
			svc := newTestService(gateway.WithPipelineBackend(pipeline))

			resp, err := svc.CancelJob(adminCtx(), &pb.CancelJobRequest{JobId: "job-1"})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.GetCancelled()).To(BeTrue())
		})
	})
})

var _ = Describe("SubmitDocument", func() {
	It("chains ConvertDocument -> ParseCatalog -> CreateJob successfully", func() {
		svc := newTestService()

		ctx := userCtx("user-a")
		resp, err := svc.SubmitDocument(ctx, &pb.SubmitDocumentRequest{
			Source: &pb.SubmitDocumentRequest_Content{Content: []byte("test doc")},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.GetDocumentId()).To(Equal("doc-1"))
		Expect(resp.GetJobId()).To(Equal("job-1"))
		Expect(resp.GetStatus()).To(Equal(pb.JobStatus_JOB_STATUS_PENDING))
	})

	It("returns error when ingestion backend fails", func() {
		ing := &mockIngestion{
			convertFn: func(context.Context, *pb.ConvertDocumentRequest) (*pb.ConvertDocumentResponse, error) {
				return nil, errors.New("ingestion failure")
			},
		}
		svc := newTestService(gateway.WithIngestionBackend(ing))

		ctx := userCtx("user-a")
		_, err := svc.SubmitDocument(ctx, &pb.SubmitDocumentRequest{
			Source: &pb.SubmitDocumentRequest_Content{Content: []byte("test doc")},
		})
		Expect(err).To(HaveOccurred())
	})

	It("returns error when no source provided", func() {
		svc := newTestService()
		ctx := userCtx("user-a")
		_, err := svc.SubmitDocument(ctx, &pb.SubmitDocumentRequest{})
		Expect(err).To(HaveOccurred())
		Expect(status.Code(err)).To(Equal(codes.InvalidArgument))
	})
})

var _ = Describe("Graph handlers", func() {
	Context("QueryGraph", func() {
		It("returns PermissionDenied for non-admin", func() {
			svc := newTestService()
			ctx := userCtx("user-a")
			_, err := svc.QueryGraph(ctx, &pb.QueryGraphRequest{Cypher: "MATCH (n) RETURN n"})
			Expect(err).To(HaveOccurred())
			Expect(status.Code(err)).To(Equal(codes.PermissionDenied))
		})

		It("succeeds for admin", func() {
			graph := &mockGraph{
				queryFn: func(_ context.Context, _ *pb.QueryRequest) (*pb.QueryResponse, error) {
					return &pb.QueryResponse{RowCount: 1}, nil
				},
			}
			svc := newTestService(gateway.WithGraphBackend(graph))

			resp, err := svc.QueryGraph(adminCtx(), &pb.QueryGraphRequest{Cypher: "MATCH (n) RETURN n"})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.GetResponse().GetRowCount()).To(Equal(int32(1)))
		})

		It("returns InvalidArgument for empty cypher", func() {
			svc := newTestService()
			_, err := svc.QueryGraph(adminCtx(), &pb.QueryGraphRequest{Cypher: ""})
			Expect(err).To(HaveOccurred())
			Expect(status.Code(err)).To(Equal(codes.InvalidArgument))
		})
	})

	Context("FindSimilar", func() {
		It("succeeds for authenticated user", func() {
			svc := newTestService()
			ctx := userCtx("user-a")
			resp, err := svc.FindSimilar(ctx, &pb.FindSimilarRequest{ControlId: "ctrl-1"})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())
		})

		It("returns InvalidArgument for empty control_id", func() {
			svc := newTestService()
			ctx := userCtx("user-a")
			_, err := svc.FindSimilar(ctx, &pb.FindSimilarRequest{ControlId: ""})
			Expect(err).To(HaveOccurred())
			Expect(status.Code(err)).To(Equal(codes.InvalidArgument))
		})
	})
})

var _ = Describe("Feedback handlers", func() {
	Context("GetReviewQueue", func() {
		It("returns PermissionDenied for non-admin", func() {
			svc := newTestService()
			ctx := userCtx("user-a")
			_, err := svc.GetReviewQueue(ctx, &pb.GetReviewQueueRequest{})
			Expect(err).To(HaveOccurred())
			Expect(status.Code(err)).To(Equal(codes.PermissionDenied))
		})

		It("succeeds for admin", func() {
			svc := newTestService()
			resp, err := svc.GetReviewQueue(adminCtx(), &pb.GetReviewQueueRequest{})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())
		})
	})

	Context("SubmitVote", func() {
		It("succeeds for authenticated user", func() {
			svc := newTestService()
			ctx := userCtx("user-a")
			resp, err := svc.SubmitVote(ctx, &pb.SubmitVoteRequest{MappingId: "map-1"})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.GetVoteId()).To(Equal("vote-1"))
		})

		It("returns InvalidArgument for empty mapping_id", func() {
			svc := newTestService()
			ctx := userCtx("user-a")
			_, err := svc.SubmitVote(ctx, &pb.SubmitVoteRequest{MappingId: ""})
			Expect(err).To(HaveOccurred())
			Expect(status.Code(err)).To(Equal(codes.InvalidArgument))
		})
	})
})
