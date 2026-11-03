//go:build !integration

package gateway_test

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/gateway"
)

var _ = Describe("StartAnalysis", func() {
	// knownCatalog serves cat-1 to tenant-a only. Like the real catalog
	// service, it rejects a lookup without a tenant, so a NotFound proves the
	// ID is unknown, not that the request was malformed.
	knownCatalog := func() *mockCatalog {
		return &mockCatalog{
			getCatalogFn: func(_ context.Context, req *connect.Request[pb.GetCatalogRequest]) (*connect.Response[pb.GetCatalogResponse], error) {
				if req.Msg.GetTenantContext().GetTenantId() == "" {
					return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("missing tenant context"))
				}
				if req.Msg.GetTenantContext().GetTenantId() != "tenant-a" || req.Msg.GetCatalogId() != "cat-1" {
					return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("catalog %q not found. List catalogs to find a valid catalog_id", req.Msg.GetCatalogId()))
				}
				return connect.NewResponse(&pb.GetCatalogResponse{Catalog: &pb.Catalog{CatalogId: "cat-1"}}), nil
			},
		}
	}

	var (
		jobReqs  []*pb.CreateJobRequest
		pipeline *mockPipeline
	)

	BeforeEach(func() {
		jobReqs = nil
		pipeline = &mockPipeline{
			createJobFn: func(_ context.Context, req *connect.Request[pb.CreateJobRequest]) (*connect.Response[pb.CreateJobResponse], error) {
				jobReqs = append(jobReqs, req.Msg)
				return connect.NewResponse(&pb.CreateJobResponse{JobId: "job-1", Status: pb.JobStatus_JOB_STATUS_PENDING}), nil
			},
		}
	})

	It("creates a full-analysis job over the named catalog for the caller's tenant", func() {
		synth := &pb.SynthesisConfig{}
		svc := newTestService(gateway.WithCatalogBackend(knownCatalog()), gateway.WithPipelineBackend(pipeline))

		resp, err := svc.StartAnalysis(userCtx("user-a"), connect.NewRequest(&pb.StartAnalysisRequest{
			TenantContext:   &pb.TenantContext{TenantId: "tenant-b"},
			SourceCatalogId: "cat-1",
			SynthesisConfig: synth,
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.GetJobId()).To(Equal("job-1"))
		Expect(resp.Msg.GetStatus()).To(Equal(pb.JobStatus_JOB_STATUS_PENDING))

		Expect(jobReqs).To(HaveLen(1))
		Expect(jobReqs[0].GetTenantContext().GetTenantId()).To(Equal("tenant-a"))
		Expect(jobReqs[0].GetJobType()).To(Equal(pb.JobType_JOB_TYPE_FULL_ANALYSIS))
		Expect(jobReqs[0].GetConfig().GetCatalogId()).To(Equal("cat-1"))
		Expect(jobReqs[0].GetConfig().GetSynthesisConfig()).To(BeIdenticalTo(synth))
	})

	It("attests the catalog as material and the job as product", func() {
		att := &recordingAttestor{}
		svc := newTestService(gateway.WithCatalogBackend(knownCatalog()), gateway.WithPipelineBackend(pipeline), gateway.WithAttestor(att))

		_, err := svc.StartAnalysis(userCtx("user-a"), connect.NewRequest(&pb.StartAnalysisRequest{SourceCatalogId: "cat-1"}))
		Expect(err).NotTo(HaveOccurred())

		Expect(att.links).To(HaveLen(1))
		Expect(att.links[0].step).To(Equal("gateway.StartAnalysis"))
		Expect(artifactURIs(att.links[0].materials)).To(Equal([]string{"catalog://tenant-a/cat-1"}))
		Expect(artifactURIs(att.links[0].products)).To(Equal([]string{"job://tenant-a/job-1"}))
	})

	It("rejects an empty source_catalog_id with guidance and creates no job", func() {
		svc := newTestService(gateway.WithCatalogBackend(knownCatalog()), gateway.WithPipelineBackend(pipeline))

		_, err := svc.StartAnalysis(userCtx("user-a"), connect.NewRequest(&pb.StartAnalysisRequest{}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		Expect(err.Error()).To(ContainSubstring("source_catalog_id is required. List catalogs to find a valid catalog_id"))
		Expect(jobReqs).To(BeEmpty())
	})

	DescribeTable("rejects a catalog the caller's tenant does not have and creates no job",
		func(ctx context.Context, catalogID string) {
			svc := newTestService(gateway.WithCatalogBackend(knownCatalog()), gateway.WithPipelineBackend(pipeline))

			_, err := svc.StartAnalysis(ctx, connect.NewRequest(&pb.StartAnalysisRequest{SourceCatalogId: catalogID}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
			Expect(err.Error()).To(ContainSubstring("List catalogs to find a valid catalog_id"))
			Expect(jobReqs).To(BeEmpty())
		},
		Entry("unknown ID", userCtx("user-a"), "cat-unknown"),
		Entry("another tenant's catalog", ctxWithIdentity("user-b", "tenant-b", []string{"user"}), "cat-1"),
	)

	It("returns Internal when the pipeline backend returns no job_id", func() {
		empty := &mockPipeline{createJobFn: func(context.Context, *connect.Request[pb.CreateJobRequest]) (*connect.Response[pb.CreateJobResponse], error) {
			return connect.NewResponse(&pb.CreateJobResponse{}), nil
		}}
		att := &recordingAttestor{}
		svc := newTestService(gateway.WithCatalogBackend(knownCatalog()), gateway.WithPipelineBackend(empty), gateway.WithAttestor(att))

		_, err := svc.StartAnalysis(userCtx("user-a"), connect.NewRequest(&pb.StartAnalysisRequest{SourceCatalogId: "cat-1"}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInternal))
		Expect(att.links).To(BeEmpty())
	})

	DescribeTable("returns Unavailable when a required backend is not configured and creates no job",
		func(opts ...gateway.ServiceOption) {
			svc := newTestService(append([]gateway.ServiceOption{gateway.WithCatalogBackend(knownCatalog()), gateway.WithPipelineBackend(pipeline)}, opts...)...)

			_, err := svc.StartAnalysis(userCtx("user-a"), connect.NewRequest(&pb.StartAnalysisRequest{SourceCatalogId: "cat-1"}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnavailable))
			Expect(jobReqs).To(BeEmpty())
		},
		Entry("no catalog backend", gateway.WithCatalogBackend(nil)),
		Entry("no pipeline backend", gateway.WithPipelineBackend(nil)),
	)

	It("returns Unauthenticated without an identity", func() {
		_, err := newTestService().StartAnalysis(context.Background(), connect.NewRequest(&pb.StartAnalysisRequest{SourceCatalogId: "cat-1"}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated))
	})
})
