//go:build !integration

package gateway_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

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

type mockIngestion struct {
	convertFn func(context.Context, *connect.Request[pb.ConvertDocumentRequest]) (*connect.Response[pb.ConvertDocumentResponse], error)
}

func (m *mockIngestion) ConvertDocument(ctx context.Context, req *connect.Request[pb.ConvertDocumentRequest]) (*connect.Response[pb.ConvertDocumentResponse], error) {
	if m.convertFn != nil {
		return m.convertFn(ctx, req)
	}
	return connect.NewResponse(&pb.ConvertDocumentResponse{DocumentId: "doc-1"}), nil
}

type mockCatalog struct {
	parseFn          func(context.Context, *connect.Request[pb.ParseCatalogRequest]) (*connect.Response[pb.ParseCatalogResponse], error)
	listCatalogsFn   func(context.Context, *connect.Request[pb.ListCatalogsRequest]) (*connect.Response[pb.ListCatalogsResponse], error)
	getCatalogFn     func(context.Context, *connect.Request[pb.GetCatalogRequest]) (*connect.Response[pb.GetCatalogResponse], error)
	searchControlsFn func(context.Context, *connect.Request[pb.SearchControlsRequest]) (*connect.Response[pb.SearchControlsResponse], error)
	getControlFn     func(context.Context, *connect.Request[pb.GetControlRequest]) (*connect.Response[pb.GetControlResponse], error)
}

func (m *mockCatalog) ParseCatalog(ctx context.Context, req *connect.Request[pb.ParseCatalogRequest]) (*connect.Response[pb.ParseCatalogResponse], error) {
	if m.parseFn != nil {
		return m.parseFn(ctx, req)
	}
	return connect.NewResponse(&pb.ParseCatalogResponse{CatalogId: "cat-1", Status: pb.JobStatus_JOB_STATUS_COMPLETED}), nil
}

func (m *mockCatalog) ListCatalogs(ctx context.Context, req *connect.Request[pb.ListCatalogsRequest]) (*connect.Response[pb.ListCatalogsResponse], error) {
	if m.listCatalogsFn != nil {
		return m.listCatalogsFn(ctx, req)
	}
	return connect.NewResponse(&pb.ListCatalogsResponse{}), nil
}

func (m *mockCatalog) GetCatalog(ctx context.Context, req *connect.Request[pb.GetCatalogRequest]) (*connect.Response[pb.GetCatalogResponse], error) {
	if m.getCatalogFn != nil {
		return m.getCatalogFn(ctx, req)
	}
	return connect.NewResponse(&pb.GetCatalogResponse{}), nil
}

func (m *mockCatalog) SearchControls(ctx context.Context, req *connect.Request[pb.SearchControlsRequest]) (*connect.Response[pb.SearchControlsResponse], error) {
	if m.searchControlsFn != nil {
		return m.searchControlsFn(ctx, req)
	}
	return connect.NewResponse(&pb.SearchControlsResponse{}), nil
}

func (m *mockCatalog) GetControl(ctx context.Context, req *connect.Request[pb.GetControlRequest]) (*connect.Response[pb.GetControlResponse], error) {
	if m.getControlFn != nil {
		return m.getControlFn(ctx, req)
	}
	return connect.NewResponse(&pb.GetControlResponse{}), nil
}

type mockPipeline struct {
	createJobFn func(context.Context, *connect.Request[pb.CreateJobRequest]) (*connect.Response[pb.CreateJobResponse], error)
	getJobFn    func(context.Context, *connect.Request[pb.GetJobRequest]) (*connect.Response[pb.GetJobResponse], error)
	listJobsFn  func(context.Context, *connect.Request[pb.ListJobsRequest]) (*connect.Response[pb.ListJobsResponse], error)
	cancelJobFn func(context.Context, *connect.Request[pb.CancelJobRequest]) (*connect.Response[pb.CancelJobResponse], error)
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
	if m.listJobsFn != nil {
		return m.listJobsFn(ctx, req)
	}
	return connect.NewResponse(&pb.ListJobsResponse{}), nil
}

func (m *mockPipeline) CancelJob(ctx context.Context, req *connect.Request[pb.CancelJobRequest]) (*connect.Response[pb.CancelJobResponse], error) {
	if m.cancelJobFn != nil {
		return m.cancelJobFn(ctx, req)
	}
	return connect.NewResponse(&pb.CancelJobResponse{Cancelled: true}), nil
}

type mockGraph struct {
	traverseFn         func(context.Context, *connect.Request[pb.TraverseRequest]) (*connect.Response[pb.TraverseResponse], error)
	queryFn            func(context.Context, *connect.Request[pb.QueryRequest]) (*connect.Response[pb.QueryResponse], error)
	similaritySearchFn func(context.Context, *connect.Request[pb.SimilaritySearchRequest]) (*connect.Response[pb.SimilaritySearchResponse], error)
}

func (m *mockGraph) Traverse(ctx context.Context, req *connect.Request[pb.TraverseRequest]) (*connect.Response[pb.TraverseResponse], error) {
	if m.traverseFn != nil {
		return m.traverseFn(ctx, req)
	}
	return connect.NewResponse(&pb.TraverseResponse{}), nil
}

func (m *mockGraph) Query(ctx context.Context, req *connect.Request[pb.QueryRequest]) (*connect.Response[pb.QueryResponse], error) {
	if m.queryFn != nil {
		return m.queryFn(ctx, req)
	}
	return connect.NewResponse(&pb.QueryResponse{}), nil
}

func (m *mockGraph) SimilaritySearch(ctx context.Context, req *connect.Request[pb.SimilaritySearchRequest]) (*connect.Response[pb.SimilaritySearchResponse], error) {
	if m.similaritySearchFn != nil {
		return m.similaritySearchFn(ctx, req)
	}
	return connect.NewResponse(&pb.SimilaritySearchResponse{}), nil
}

type mockFeedback struct {
	submitVoteFn     func(context.Context, *connect.Request[pb.SubmitVoteRequest]) (*connect.Response[pb.SubmitVoteResponse], error)
	getReviewQueueFn func(context.Context, *connect.Request[pb.GetReviewQueueRequest]) (*connect.Response[pb.GetReviewQueueResponse], error)
}

func (m *mockFeedback) SubmitVote(ctx context.Context, req *connect.Request[pb.SubmitVoteRequest]) (*connect.Response[pb.SubmitVoteResponse], error) {
	if m.submitVoteFn != nil {
		return m.submitVoteFn(ctx, req)
	}
	return connect.NewResponse(&pb.SubmitVoteResponse{VoteId: "vote-1"}), nil
}

func (m *mockFeedback) GetReviewQueue(ctx context.Context, req *connect.Request[pb.GetReviewQueueRequest]) (*connect.Response[pb.GetReviewQueueResponse], error) {
	if m.getReviewQueueFn != nil {
		return m.getReviewQueueFn(ctx, req)
	}
	return connect.NewResponse(&pb.GetReviewQueueResponse{}), nil
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

	It("returns InvalidArgument for empty catalog_id on GetCatalog", func() {
		svc := newTestService()
		ctx := userCtx("user-1")
		_, err := svc.GetCatalog(ctx, connect.NewRequest(&pb.GetCatalogRequest{CatalogId: ""}))
		Expect(err).To(HaveOccurred())
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
	})

	It("returns InvalidArgument for empty control_id on GetControl", func() {
		svc := newTestService()
		ctx := userCtx("user-1")
		_, err := svc.GetControl(ctx, connect.NewRequest(&pb.GetControlRequest{ControlId: ""}))
		Expect(err).To(HaveOccurred())
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
	})

	It("returns InvalidArgument for empty query on SearchControls", func() {
		svc := newTestService()
		ctx := userCtx("user-1")
		_, err := svc.SearchControls(ctx, connect.NewRequest(&pb.SearchControlsRequest{Query: ""}))
		Expect(err).To(HaveOccurred())
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
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

var _ = Describe("Job handlers", func() {
	Context("GetJob", func() {
		It("returns job to its owner", func() {
			pipeline := &mockPipeline{
				getJobFn: func(_ context.Context, _ *connect.Request[pb.GetJobRequest]) (*connect.Response[pb.GetJobResponse], error) {
					return connect.NewResponse(&pb.GetJobResponse{
						Job: &pb.PipelineJob{
							JobId: "job-1",
							Audit: &pb.AuditMetadata{CreatedBy: "user-a"},
						},
					}), nil
				},
			}
			svc := newTestService(gateway.WithPipelineBackend(pipeline))

			ctx := userCtx("user-a")
			resp, err := svc.GetJob(ctx, connect.NewRequest(&pb.GetJobRequest{JobId: "job-1"}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetJob().GetJobId()).To(Equal("job-1"))
		})

		It("returns PermissionDenied when non-owner non-admin accesses job", func() {
			pipeline := &mockPipeline{
				getJobFn: func(_ context.Context, _ *connect.Request[pb.GetJobRequest]) (*connect.Response[pb.GetJobResponse], error) {
					return connect.NewResponse(&pb.GetJobResponse{
						Job: &pb.PipelineJob{
							JobId: "job-1",
							Audit: &pb.AuditMetadata{CreatedBy: "user-a"},
						},
					}), nil
				},
			}
			svc := newTestService(gateway.WithPipelineBackend(pipeline))

			ctx := userCtx("user-b")
			_, err := svc.GetJob(ctx, connect.NewRequest(&pb.GetJobRequest{JobId: "job-1"}))
			Expect(err).To(HaveOccurred())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied))
		})

		It("allows admin to access any job", func() {
			pipeline := &mockPipeline{
				getJobFn: func(_ context.Context, _ *connect.Request[pb.GetJobRequest]) (*connect.Response[pb.GetJobResponse], error) {
					return connect.NewResponse(&pb.GetJobResponse{
						Job: &pb.PipelineJob{
							JobId: "job-1",
							Audit: &pb.AuditMetadata{CreatedBy: "user-a"},
						},
					}), nil
				},
			}
			svc := newTestService(gateway.WithPipelineBackend(pipeline))

			resp, err := svc.GetJob(adminCtx(), connect.NewRequest(&pb.GetJobRequest{JobId: "job-1"}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetJob().GetJobId()).To(Equal("job-1"))
		})

		It("returns InvalidArgument for empty job_id", func() {
			svc := newTestService()
			ctx := userCtx("user-a")
			_, err := svc.GetJob(ctx, connect.NewRequest(&pb.GetJobRequest{JobId: ""}))
			Expect(err).To(HaveOccurred())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
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

	Context("CancelJob", func() {
		It("allows owner to cancel own job", func() {
			pipeline := &mockPipeline{
				getJobFn: func(_ context.Context, _ *connect.Request[pb.GetJobRequest]) (*connect.Response[pb.GetJobResponse], error) {
					return connect.NewResponse(&pb.GetJobResponse{
						Job: &pb.PipelineJob{
							JobId: "job-1",
							Audit: &pb.AuditMetadata{CreatedBy: "user-a"},
						},
					}), nil
				},
			}
			svc := newTestService(gateway.WithPipelineBackend(pipeline))

			ctx := userCtx("user-a")
			resp, err := svc.CancelJob(ctx, connect.NewRequest(&pb.CancelJobRequest{JobId: "job-1"}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetCancelled()).To(BeTrue())
		})

		It("returns PermissionDenied for non-owner", func() {
			pipeline := &mockPipeline{
				getJobFn: func(_ context.Context, _ *connect.Request[pb.GetJobRequest]) (*connect.Response[pb.GetJobResponse], error) {
					return connect.NewResponse(&pb.GetJobResponse{
						Job: &pb.PipelineJob{
							JobId: "job-1",
							Audit: &pb.AuditMetadata{CreatedBy: "user-a"},
						},
					}), nil
				},
			}
			svc := newTestService(gateway.WithPipelineBackend(pipeline))

			ctx := userCtx("user-b")
			_, err := svc.CancelJob(ctx, connect.NewRequest(&pb.CancelJobRequest{JobId: "job-1"}))
			Expect(err).To(HaveOccurred())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied))
		})

		It("allows admin to cancel any job", func() {
			pipeline := &mockPipeline{
				getJobFn: func(_ context.Context, _ *connect.Request[pb.GetJobRequest]) (*connect.Response[pb.GetJobResponse], error) {
					return connect.NewResponse(&pb.GetJobResponse{
						Job: &pb.PipelineJob{
							JobId: "job-1",
							Audit: &pb.AuditMetadata{CreatedBy: "user-a"},
						},
					}), nil
				},
			}
			svc := newTestService(gateway.WithPipelineBackend(pipeline))

			resp, err := svc.CancelJob(adminCtx(), connect.NewRequest(&pb.CancelJobRequest{JobId: "job-1"}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetCancelled()).To(BeTrue())
		})
	})
})

var _ = Describe("SubmitDocument", func() {
	It("chains ConvertDocument -> ParseCatalog -> CreateJob successfully", func() {
		svc := newTestService()

		ctx := userCtx("user-a")
		resp, err := svc.SubmitDocument(ctx, connect.NewRequest(&pb.SubmitDocumentRequest{
			Source: &pb.SubmitDocumentRequest_Content{Content: []byte("test doc")},
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.GetDocumentId()).To(Equal("doc-1"))
		Expect(resp.Msg.GetJobId()).To(Equal("job-1"))
		Expect(resp.Msg.GetStatus()).To(Equal(pb.JobStatus_JOB_STATUS_PENDING))
	})

	It("returns error when ingestion backend fails", func() {
		ing := &mockIngestion{
			convertFn: func(context.Context, *connect.Request[pb.ConvertDocumentRequest]) (*connect.Response[pb.ConvertDocumentResponse], error) {
				return nil, errors.New("ingestion failure")
			},
		}
		svc := newTestService(gateway.WithIngestionBackend(ing))

		ctx := userCtx("user-a")
		_, err := svc.SubmitDocument(ctx, connect.NewRequest(&pb.SubmitDocumentRequest{
			Source: &pb.SubmitDocumentRequest_Content{Content: []byte("test doc")},
		}))
		Expect(err).To(HaveOccurred())
	})

	It("returns error when catalog backend fails", func() {
		cat := &mockCatalog{
			parseFn: func(context.Context, *connect.Request[pb.ParseCatalogRequest]) (*connect.Response[pb.ParseCatalogResponse], error) {
				return nil, connect.NewError(connect.CodeInternal, errors.New("catalog unavailable"))
			},
		}
		svc := newTestService(gateway.WithCatalogBackend(cat))

		ctx := userCtx("user-a")
		_, err := svc.SubmitDocument(ctx, connect.NewRequest(&pb.SubmitDocumentRequest{
			Source: &pb.SubmitDocumentRequest_Content{Content: []byte("test doc")},
		}))
		Expect(err).To(HaveOccurred())
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInternal))
	})

	It("returns error when pipeline backend fails", func() {
		pipeline := &mockPipeline{
			createJobFn: func(context.Context, *connect.Request[pb.CreateJobRequest]) (*connect.Response[pb.CreateJobResponse], error) {
				return nil, connect.NewError(connect.CodeInternal, errors.New("pipeline unavailable"))
			},
		}
		svc := newTestService(gateway.WithPipelineBackend(pipeline))

		ctx := userCtx("user-a")
		_, err := svc.SubmitDocument(ctx, connect.NewRequest(&pb.SubmitDocumentRequest{
			Source: &pb.SubmitDocumentRequest_Content{Content: []byte("test doc")},
		}))
		Expect(err).To(HaveOccurred())
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInternal))
	})

	It("returns error when no source provided", func() {
		svc := newTestService()
		ctx := userCtx("user-a")
		_, err := svc.SubmitDocument(ctx, connect.NewRequest(&pb.SubmitDocumentRequest{}))
		Expect(err).To(HaveOccurred())
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
	})
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

		It("returns InvalidArgument for empty cypher", func() {
			svc := newTestService()
			_, err := svc.QueryGraph(adminCtx(), connect.NewRequest(&pb.QueryGraphRequest{Cypher: ""}))
			Expect(err).To(HaveOccurred())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		})
	})

	Context("GetControlMappings", func() {
		It("returns InvalidArgument for empty control_id", func() {
			svc := newTestService()
			ctx := userCtx("user-a")
			_, err := svc.GetControlMappings(ctx, connect.NewRequest(&pb.GetControlMappingsRequest{ControlId: ""}))
			Expect(err).To(HaveOccurred())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		})

		It("returns mappings from traverse results", func() {
			graph := &mockGraph{
				traverseFn: func(_ context.Context, _ *connect.Request[pb.TraverseRequest]) (*connect.Response[pb.TraverseResponse], error) {
					return connect.NewResponse(&pb.TraverseResponse{
						Edges: []*pb.Edge{
							{
								EdgeId:           "edge-1",
								SourceNodeId:     "ctrl-source",
								TargetNodeId:     "ctrl-target",
								Label:            "maps_to",
								RelationshipType: pb.RelationshipType_RELATIONSHIP_TYPE_EQUIVALENT,
							},
						},
					}), nil
				},
			}
			svc := newTestService(gateway.WithGraphBackend(graph))

			ctx := userCtx("user-a")
			resp, err := svc.GetControlMappings(ctx, connect.NewRequest(&pb.GetControlMappingsRequest{ControlId: "ctrl-source"}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetMappings()).To(HaveLen(1))

			m := resp.Msg.GetMappings()[0]
			Expect(m.GetMappingId()).To(Equal("edge-1"))
			Expect(m.GetSourceControlId()).To(Equal("ctrl-source"))
			Expect(m.GetTargetControlId()).To(Equal("ctrl-target"))
		})

		It("propagates pagination limit to traverse request", func() {
			var capturedReq *pb.TraverseRequest
			graph := &mockGraph{
				traverseFn: func(_ context.Context, req *connect.Request[pb.TraverseRequest]) (*connect.Response[pb.TraverseResponse], error) {
					capturedReq = req.Msg
					return connect.NewResponse(&pb.TraverseResponse{}), nil
				},
			}
			svc := newTestService(gateway.WithGraphBackend(graph))

			ctx := userCtx("user-a")
			_, err := svc.GetControlMappings(ctx, connect.NewRequest(&pb.GetControlMappingsRequest{
				ControlId: "ctrl-1",
				Limit:     5,
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(capturedReq).NotTo(BeNil())
			Expect(capturedReq.GetOptions()).NotTo(BeNil())
			Expect(capturedReq.GetOptions().GetPagination().GetPageSize()).To(Equal(int32(5)))
		})

		It("returns Unavailable when graph backend is nil", func() {
			svc := newTestService(gateway.WithGraphBackend(nil))
			ctx := userCtx("user-a")
			_, err := svc.GetControlMappings(ctx, connect.NewRequest(&pb.GetControlMappingsRequest{ControlId: "ctrl-1"}))
			Expect(err).To(HaveOccurred())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnavailable))
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

		It("returns InvalidArgument for empty control_id", func() {
			svc := newTestService()
			ctx := userCtx("user-a")
			_, err := svc.FindSimilar(ctx, connect.NewRequest(&pb.FindSimilarRequest{ControlId: ""}))
			Expect(err).To(HaveOccurred())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
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
		Expect(resp.Msg.GetJobId()).To(Equal("job-1"))
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

	It("propagates metadata to catalog and pipeline backends", func() {
		var capturedParseReq *pb.ParseCatalogRequest
		var capturedJobReq *pb.CreateJobRequest
		cat := &mockCatalog{
			parseFn: func(_ context.Context, req *connect.Request[pb.ParseCatalogRequest]) (*connect.Response[pb.ParseCatalogResponse], error) {
				capturedParseReq = req.Msg
				return connect.NewResponse(&pb.ParseCatalogResponse{CatalogId: "cat-1", Status: pb.JobStatus_JOB_STATUS_COMPLETED}), nil
			},
		}
		pipeline := &mockPipeline{
			createJobFn: func(_ context.Context, req *connect.Request[pb.CreateJobRequest]) (*connect.Response[pb.CreateJobResponse], error) {
				capturedJobReq = req.Msg
				return connect.NewResponse(&pb.CreateJobResponse{JobId: "job-1", Status: pb.JobStatus_JOB_STATUS_PENDING}), nil
			},
		}
		svc := newTestService(gateway.WithCatalogBackend(cat), gateway.WithPipelineBackend(pipeline))
		ctx := userCtx("user-a")

		meta := &pb.StreamDocumentMetadata{
			CatalogFormat:   pb.CatalogFormat_CATALOG_FORMAT_OSCAL,
			CatalogName:     "test-catalog",
			TargetCatalogId: "target-cat-1",
		}

		resp, err := gateway.ExportHandleStreamedDocument(svc, ctx, time.Now(), meta, []byte("doc content"))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.GetDocumentId()).To(Equal("doc-1"))

		Expect(capturedParseReq).NotTo(BeNil())
		Expect(capturedParseReq.GetFormat()).To(Equal(pb.CatalogFormat_CATALOG_FORMAT_OSCAL))
		Expect(capturedParseReq.GetCatalogName()).To(Equal("test-catalog"))

		Expect(capturedJobReq).NotTo(BeNil())
		Expect(capturedJobReq.GetConfig().GetCatalogFormat()).To(Equal(pb.CatalogFormat_CATALOG_FORMAT_OSCAL))
		Expect(capturedJobReq.GetConfig().GetCatalogName()).To(Equal("test-catalog"))
		Expect(capturedJobReq.GetConfig().GetTargetCatalogId()).To(Equal("target-cat-1"))
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
		It("returns PermissionDenied for non-admin", func() {
			svc := newTestService()
			ctx := userCtx("user-a")
			_, err := svc.GetReviewQueue(ctx, connect.NewRequest(&pb.GetReviewQueueRequest{}))
			Expect(err).To(HaveOccurred())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied))
		})

		It("succeeds for admin", func() {
			svc := newTestService()
			resp, err := svc.GetReviewQueue(adminCtx(), connect.NewRequest(&pb.GetReviewQueueRequest{}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())
		})
	})

	Context("SubmitVote", func() {
		It("succeeds for authenticated user", func() {
			svc := newTestService()
			ctx := userCtx("user-a")
			resp, err := svc.SubmitVote(ctx, connect.NewRequest(&pb.SubmitVoteRequest{MappingId: "map-1"}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetVoteId()).To(Equal("vote-1"))
		})

		It("returns InvalidArgument for empty mapping_id", func() {
			svc := newTestService()
			ctx := userCtx("user-a")
			_, err := svc.SubmitVote(ctx, connect.NewRequest(&pb.SubmitVoteRequest{MappingId: ""}))
			Expect(err).To(HaveOccurred())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		})
	})
})
