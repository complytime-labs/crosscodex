//go:build !integration

package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/gateway"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
)

var _ = Describe("StartAnalysis", func() {
	var (
		jobReqs  []*pb.CreateJobRequest
		pipeline *mockPipeline
		lookups  []string
	)

	// catalogsOwnedBy serves each catalog only to the tenant owners names
	// for it, recording every lookup. Like the real catalog service, it
	// rejects a lookup without a tenant, so a NotFound proves the ID is
	// unknown to the caller's tenant, not that the request was malformed;
	// its NotFound message is the real service's.
	catalogsOwnedBy := func(owners map[string]string) *mockCatalog {
		return &mockCatalog{
			getCatalogFn: func(_ context.Context, req *connect.Request[pb.GetCatalogRequest]) (*connect.Response[pb.GetCatalogResponse], error) {
				tenantID, id := req.Msg.GetTenantContext().GetTenantId(), req.Msg.GetCatalogId()
				if tenantID == "" {
					return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("missing tenant context"))
				}
				lookups = append(lookups, id)
				if owners[id] != tenantID {
					return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("catalog %q not found. List catalogs to find a valid catalog_id", id))
				}
				return connect.NewResponse(&pb.GetCatalogResponse{Catalog: &pb.Catalog{CatalogId: id}}), nil
			},
		}
	}
	// knownCatalog serves cat-1 and cat-2 to tenant-a, and cat-b to tenant-b.
	knownCatalog := func() *mockCatalog {
		return catalogsOwnedBy(map[string]string{"cat-1": "tenant-a", "cat-2": "tenant-a", "cat-b": "tenant-b"})
	}
	crosswalk := func() *pb.StartAnalysisRequest {
		return &pb.StartAnalysisRequest{SourceCatalogId: "cat-1", TargetCatalogId: "cat-2"}
	}

	BeforeEach(func() {
		jobReqs, lookups = nil, nil
		pipeline = &mockPipeline{
			createJobFn: func(_ context.Context, req *connect.Request[pb.CreateJobRequest]) (*connect.Response[pb.CreateJobResponse], error) {
				jobReqs = append(jobReqs, req.Msg)
				return connect.NewResponse(&pb.CreateJobResponse{JobId: "job-1", Status: pb.JobStatus_JOB_STATUS_PENDING}), nil
			},
		}
	})

	It("creates a full-analysis job mapping the source onto the target for the caller's tenant", func() {
		synth := &pb.SynthesisConfig{}
		svc := newTestService(gateway.WithCatalogBackend(knownCatalog()), gateway.WithPipelineBackend(pipeline))

		resp, err := svc.StartAnalysis(userCtx("user-a"), connect.NewRequest(&pb.StartAnalysisRequest{
			TenantContext:   &pb.TenantContext{TenantId: "tenant-b"},
			SourceCatalogId: "cat-1",
			TargetCatalogId: "cat-2",
			Bidirectional:   true,
			SynthesisConfig: synth,
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.GetJobId()).To(Equal("job-1"))
		Expect(resp.Msg.GetStatus()).To(Equal(pb.JobStatus_JOB_STATUS_PENDING))

		Expect(lookups).To(Equal([]string{"cat-1", "cat-2"}), "source first, then target")
		Expect(jobReqs).To(HaveLen(1))
		Expect(jobReqs[0].GetTenantContext().GetTenantId()).To(Equal("tenant-a"))
		Expect(jobReqs[0].GetJobType()).To(Equal(pb.JobType_JOB_TYPE_FULL_ANALYSIS))
		Expect(jobReqs[0].GetConfig().GetCatalogId()).To(Equal("cat-1"))
		Expect(jobReqs[0].GetConfig().GetTargetCatalogId()).To(Equal("cat-2"))
		Expect(jobReqs[0].GetConfig().GetBidirectional()).To(BeTrue())
		Expect(jobReqs[0].GetConfig().GetSynthesisConfig()).To(BeIdenticalTo(synth))
	})

	It("accepts a target equal to the source as an analysis within one catalog, looking it up once", func() {
		svc := newTestService(gateway.WithCatalogBackend(knownCatalog()), gateway.WithPipelineBackend(pipeline))

		_, err := svc.StartAnalysis(userCtx("user-a"), connect.NewRequest(&pb.StartAnalysisRequest{SourceCatalogId: "cat-1", TargetCatalogId: "cat-1"}))
		Expect(err).NotTo(HaveOccurred())
		Expect(lookups).To(Equal([]string{"cat-1"}))
		Expect(jobReqs).To(HaveLen(1))
		Expect(jobReqs[0].GetConfig().GetCatalogId()).To(Equal("cat-1"))
		Expect(jobReqs[0].GetConfig().GetTargetCatalogId()).To(Equal("cat-1"))
		Expect(jobReqs[0].GetConfig().GetBidirectional()).To(BeFalse())
	})

	DescribeTable("attests every catalog the job reads as a material and the job as product",
		func(req *pb.StartAnalysisRequest, wantMaterials []string) {
			att := &recordingAttestor{}
			svc := newTestService(gateway.WithCatalogBackend(knownCatalog()), gateway.WithPipelineBackend(pipeline), gateway.WithAttestor(att))

			_, err := svc.StartAnalysis(userCtx("user-a"), connect.NewRequest(req))
			Expect(err).NotTo(HaveOccurred())

			Expect(att.links).To(HaveLen(1))
			Expect(att.links[0].step).To(Equal("gateway.StartAnalysis"))
			Expect(artifactURIs(att.links[0].materials)).To(Equal(wantMaterials))
			Expect(artifactURIs(att.links[0].products)).To(Equal([]string{"job://tenant-a/job-1"}))
		},
		Entry("a crosswalk: source then target", crosswalk(), []string{"catalog://tenant-a/cat-1", "catalog://tenant-a/cat-2"}),
		Entry("within one catalog: that catalog once", &pb.StartAnalysisRequest{SourceCatalogId: "cat-1", TargetCatalogId: "cat-1"}, []string{"catalog://tenant-a/cat-1"}),
	)

	It("records the target catalog on the span", func() {
		tp, err := telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = tp.Shutdown(context.Background()) })
		svc := newTestService(gateway.WithCatalogBackend(knownCatalog()), gateway.WithPipelineBackend(pipeline),
			gateway.WithTelemetry(tp.TracerProvider().Tracer("test"), tp.MeterProvider().Meter("test")))

		_, err = svc.StartAnalysis(userCtx("user-a"), connect.NewRequest(crosswalk()))
		Expect(err).NotTo(HaveOccurred())

		span := telemetrytest.FindSpan(tp.GetSpans(), "gateway.StartAnalysis")
		Expect(span).NotTo(BeNil())
		v, ok := telemetrytest.SpanAttribute(span, "target_catalog.id")
		Expect(ok).To(BeTrue())
		Expect(v.AsString()).To(Equal("cat-2"))
	})

	DescribeTable("rejects an invalid request with guidance before any lookup, and creates no job",
		func(req *pb.StartAnalysisRequest, want []string) {
			svc := newTestService(gateway.WithCatalogBackend(knownCatalog()), gateway.WithPipelineBackend(pipeline))

			_, err := svc.StartAnalysis(userCtx("user-a"), connect.NewRequest(req))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
			for _, w := range want {
				Expect(err.Error()).To(ContainSubstring(w))
			}
			Expect(lookups).To(BeEmpty())
			Expect(jobReqs).To(BeEmpty())
		},
		Entry("no source", &pb.StartAnalysisRequest{TargetCatalogId: "cat-2"},
			[]string{"source_catalog_id is required. List catalogs to find a valid catalog_id"}),
		Entry("no target: names the field and both CLI forms", &pb.StartAnalysisRequest{SourceCatalogId: "cat-1"},
			[]string{"target_catalog_id is required", "crosscodex run start --source cat-1 --target <catalog-id>", "crosscodex run start --source cat-1 --target cat-1"}),
		Entry("bidirectional within one catalog", &pb.StartAnalysisRequest{SourceCatalogId: "cat-1", TargetCatalogId: "cat-1", Bidirectional: true},
			[]string{"bidirectional cannot be set when target_catalog_id equals source_catalog_id", "already pairs its controls in both directions", "Unset bidirectional, or set target_catalog_id to a different catalog"}),
	)

	DescribeTable("rejects a source catalog the caller's tenant does not have and creates no job",
		func(ctx context.Context, catalogID string) {
			svc := newTestService(gateway.WithCatalogBackend(knownCatalog()), gateway.WithPipelineBackend(pipeline))

			_, err := svc.StartAnalysis(ctx, connect.NewRequest(&pb.StartAnalysisRequest{SourceCatalogId: catalogID, TargetCatalogId: catalogID}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
			Expect(err.Error()).To(ContainSubstring("List catalogs to find a valid catalog_id"))
			Expect(jobReqs).To(BeEmpty())
		},
		Entry("unknown ID", userCtx("user-a"), "cat-unknown"),
		Entry("another tenant's catalog", ctxWithIdentity("user-b", "tenant-b", []string{"user"}), "cat-1"),
	)

	DescribeTable("rejects a target catalog the caller's tenant does not have, naming it, and creates no job",
		func(targetID string) {
			svc := newTestService(gateway.WithCatalogBackend(knownCatalog()), gateway.WithPipelineBackend(pipeline))

			_, err := svc.StartAnalysis(userCtx("user-a"), connect.NewRequest(&pb.StartAnalysisRequest{SourceCatalogId: "cat-1", TargetCatalogId: targetID}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
			Expect(err.Error()).To(SatisfyAll(
				ContainSubstring(fmt.Sprintf("target catalog %q not found", targetID)),
				ContainSubstring(`crosscodex catalog list`),
			))
			Expect(lookups).To(Equal([]string{"cat-1", targetID}))
			Expect(jobReqs).To(BeEmpty())
		},
		Entry("unknown ID", "cat-unknown"),
		Entry("another tenant's catalog", "cat-b"),
	)

	// No existence oracle: a caller must not learn that another tenant has
	// a catalog with some ID. The backend here leaks, answering another
	// tenant's catalog with a message that differs from an unknown one's,
	// so equal bytes prove the gateway, not the backend, withholds it.
	It("answers a missing target and another tenant's target with byte-identical errors", func() {
		start := func(targetNotFound error) *connect.Error {
			backend := &mockCatalog{
				getCatalogFn: func(_ context.Context, req *connect.Request[pb.GetCatalogRequest]) (*connect.Response[pb.GetCatalogResponse], error) {
					if id := req.Msg.GetCatalogId(); id == "cat-1" {
						return connect.NewResponse(&pb.GetCatalogResponse{Catalog: &pb.Catalog{CatalogId: id}}), nil
					}
					return nil, targetNotFound
				},
			}
			svc := newTestService(gateway.WithCatalogBackend(backend), gateway.WithPipelineBackend(pipeline))
			_, err := svc.StartAnalysis(userCtx("user-a"), connect.NewRequest(&pb.StartAnalysisRequest{SourceCatalogId: "cat-1", TargetCatalogId: "cat-b"}))
			var connectErr *connect.Error
			Expect(errors.As(err, &connectErr)).To(BeTrue(), "want a *connect.Error, got %v", err)
			return connectErr
		}
		othersErr := start(connect.NewError(connect.CodeNotFound, errors.New(`catalog "cat-b" belongs to tenant "tenant-b"`)))
		missingErr := start(connect.NewError(connect.CodeNotFound, errors.New(`catalog "cat-b" not found. List catalogs to find a valid catalog_id`)))

		Expect(othersErr.Code()).To(Equal(connect.CodeNotFound))
		Expect(missingErr.Code()).To(Equal(connect.CodeNotFound))
		Expect([]byte(othersErr.Error())).To(Equal([]byte(missingErr.Error())))
		Expect(othersErr.Details()).To(BeEmpty())
		Expect(missingErr.Details()).To(BeEmpty())
		Expect(othersErr.Meta()).To(Equal(missingErr.Meta()))
		Expect(othersErr.Error()).NotTo(ContainSubstring("tenant-b"))
		Expect(jobReqs).To(BeEmpty())
	})

	// The fixed NotFound withholds the backend's message from the caller,
	// so the server keeps it, once, for an operator debugging a lookup.
	It("logs the target lookup's original NotFound once at debug level", func() {
		var logs bytes.Buffer
		backendErr := connect.NewError(connect.CodeNotFound, errors.New(`catalog "cat-x" not found. List catalogs to find a valid catalog_id`))
		backend := &mockCatalog{
			getCatalogFn: func(_ context.Context, req *connect.Request[pb.GetCatalogRequest]) (*connect.Response[pb.GetCatalogResponse], error) {
				if req.Msg.GetCatalogId() == "cat-1" {
					return connect.NewResponse(&pb.GetCatalogResponse{Catalog: &pb.Catalog{CatalogId: "cat-1"}}), nil
				}
				return nil, backendErr
			},
		}
		svc := newTestService(gateway.WithCatalogBackend(backend), gateway.WithPipelineBackend(pipeline),
			gateway.WithLogger(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))))

		_, err := svc.StartAnalysis(userCtx("user-a"), connect.NewRequest(&pb.StartAnalysisRequest{SourceCatalogId: "cat-1", TargetCatalogId: "cat-x"}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))

		var records []map[string]any
		dec := json.NewDecoder(&logs)
		for dec.More() {
			var r map[string]any
			Expect(dec.Decode(&r)).To(Succeed())
			records = append(records, r)
		}
		Expect(records).To(HaveLen(1))
		Expect(records[0]).To(HaveKeyWithValue("level", "DEBUG"))
		Expect(records[0]).To(HaveKeyWithValue("target_catalog_id", "cat-x"))
		Expect(records[0]).To(HaveKeyWithValue("tenant", "tenant-a"))
		Expect(records[0]).To(HaveKeyWithValue("error", backendErr.Error()))
		Expect(jobReqs).To(BeEmpty())
	})

	It("returns Internal when the pipeline backend returns no job_id", func() {
		empty := &mockPipeline{createJobFn: func(context.Context, *connect.Request[pb.CreateJobRequest]) (*connect.Response[pb.CreateJobResponse], error) {
			return connect.NewResponse(&pb.CreateJobResponse{}), nil
		}}
		att := &recordingAttestor{}
		svc := newTestService(gateway.WithCatalogBackend(knownCatalog()), gateway.WithPipelineBackend(empty), gateway.WithAttestor(att))

		_, err := svc.StartAnalysis(userCtx("user-a"), connect.NewRequest(crosswalk()))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInternal))
		Expect(att.links).To(BeEmpty())
	})

	DescribeTable("returns Unavailable when a required backend is not configured and creates no job",
		func(opts ...gateway.ServiceOption) {
			svc := newTestService(append([]gateway.ServiceOption{gateway.WithCatalogBackend(knownCatalog()), gateway.WithPipelineBackend(pipeline)}, opts...)...)

			_, err := svc.StartAnalysis(userCtx("user-a"), connect.NewRequest(crosswalk()))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnavailable))
			Expect(jobReqs).To(BeEmpty())
		},
		Entry("no catalog backend", gateway.WithCatalogBackend(nil)),
		Entry("no pipeline backend", gateway.WithPipelineBackend(nil)),
	)

	It("returns Unauthenticated without an identity", func() {
		_, err := newTestService().StartAnalysis(context.Background(), connect.NewRequest(crosswalk()))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated))
	})
})
