//go:build !integration

package results_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/results"
	analyzerresults "github.com/complytime-labs/crosscodex/pkg/analyzer/results"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// fakeReader records the Filter it receives and returns the configured result.
type fakeReader struct {
	page   results.Page
	err    error
	calls  int
	filter results.Filter
}

func (f *fakeReader) ListMappings(_ context.Context, flt results.Filter) (results.Page, error) {
	f.calls++
	f.filter = flt
	return f.page, f.err
}

func validRequest() *pb.ListJobMappingsRequest {
	return &pb.ListJobMappingsRequest{
		TenantContext: &pb.TenantContext{TenantId: "tenant-a"},
		JobId:         "job-1",
	}
}

func intCounterByAttr(tp *telemetrytest.TestProvider, name, key, value string) int64 {
	m := telemetrytest.FindMetric(tp.GetMetrics(), name)
	if m == nil {
		return 0
	}
	sum, ok := m.Data.(metricdata.Sum[int64])
	Expect(ok).To(BeTrue(), "metric %s data is %T", name, m.Data)
	var total int64
	for _, dp := range sum.DataPoints {
		if key == "" {
			total += dp.Value
		} else if v, ok := dp.Attributes.Value(attribute.Key(key)); ok && v.AsString() == value {
			total += dp.Value
		}
	}
	return total
}

var _ = Describe("Service.ListMappings", func() {
	var (
		reader  *fakeReader
		logs    *bytes.Buffer
		service *results.Service
	)
	BeforeEach(func() {
		reader = &fakeReader{}
		logs = &bytes.Buffer{}
		service = results.NewService(reader, results.WithLogger(slog.New(slog.NewJSONHandler(logs, nil))))
	})

	call := func(ctx context.Context, msg *pb.ListJobMappingsRequest) (*pb.ListJobMappingsResponse, error) {
		resp, err := service.ListMappings(ctx, connect.NewRequest(msg))
		if err != nil {
			return nil, err
		}
		return resp.Msg, nil
	}

	DescribeTable("rejects invalid requests without reading",
		func(mutate func(*pb.ListJobMappingsRequest), code connect.Code, substr string) {
			req := validRequest()
			mutate(req)
			_, err := call(context.Background(), req)
			Expect(connect.CodeOf(err)).To(Equal(code))
			Expect(err.Error()).To(ContainSubstring(substr))
			Expect(reader.calls).To(BeZero())
		},
		Entry("missing tenant context", func(r *pb.ListJobMappingsRequest) { r.TenantContext = nil },
			connect.CodeInvalidArgument, "tenant_context.tenant_id is required"),
		Entry("empty tenant id", func(r *pb.ListJobMappingsRequest) { r.TenantContext.TenantId = "" },
			connect.CodeInvalidArgument, "tenant_context.tenant_id is required"),
		Entry("malformed tenant id", func(r *pb.ListJobMappingsRequest) { r.TenantContext.TenantId = "../other" },
			connect.CodeInvalidArgument, "tenant"),
		Entry("unknown stage", func(r *pb.ListJobMappingsRequest) { r.Stage = pb.MappingStage(99) },
			connect.CodeInvalidArgument, "synthesized, analyzer"),
		Entry("no job and no control", func(r *pb.ListJobMappingsRequest) { r.JobId = "" },
			connect.CodeInvalidArgument, `job_id is required; pass the job ID printed by "crosscodex run start" or "catalog import"`),
		Entry("no job with control on the analyzer stage", func(r *pb.ListJobMappingsRequest) {
			r.JobId, r.ControlId, r.Stage = "", "AC-1", pb.MappingStage_MAPPING_STAGE_ANALYZER
		}, connect.CodeInvalidArgument, "job_id is required"),
		Entry("NaN confidence", func(r *pb.ListJobMappingsRequest) { r.MinConfidence = float32(math.NaN()) },
			connect.CodeInvalidArgument, "min_confidence must be between 0 and 1"),
		Entry("negative confidence", func(r *pb.ListJobMappingsRequest) { r.MinConfidence = -0.1 },
			connect.CodeInvalidArgument, "min_confidence must be between 0 and 1, got -0.1"),
		Entry("confidence above one", func(r *pb.ListJobMappingsRequest) { r.MinConfidence = 1.5 },
			connect.CodeInvalidArgument, "min_confidence must be between 0 and 1, got 1.5"),
		Entry("non-viable on the analyzer stage", func(r *pb.ListJobMappingsRequest) {
			r.Stage, r.IncludeNonViable = pb.MappingStage_MAPPING_STAGE_ANALYZER, true
		}, connect.CodeInvalidArgument, "include_non_viable applies only to the synthesized stage"),
		Entry("unspecified relationship type", func(r *pb.ListJobMappingsRequest) {
			r.RelationshipTypes = []pb.RelationshipType{pb.RelationshipType_RELATIONSHIP_TYPE_UNSPECIFIED}
		}, connect.CodeInvalidArgument, "EQUIVALENT"),
		Entry("unknown relationship type", func(r *pb.ListJobMappingsRequest) {
			r.RelationshipTypes = []pb.RelationshipType{pb.RelationshipType(99)}
		}, connect.CodeInvalidArgument, "EQUIVALENT"),
		Entry("negative page size", func(r *pb.ListJobMappingsRequest) { r.Pagination = &pb.Pagination{PageSize: -1} },
			connect.CodeInvalidArgument, "page_size must be between 1 and 1000"),
		Entry("oversized page", func(r *pb.ListJobMappingsRequest) { r.Pagination = &pb.Pagination{PageSize: 1001} },
			connect.CodeInvalidArgument, "page_size must be between 1 and 1000"),
		Entry("forged page token", func(r *pb.ListJobMappingsRequest) { r.Pagination = &pb.Pagination{PageToken: "!!not-a-token"} },
			connect.CodeInvalidArgument, "next_page_token"),
	)

	It("refuses a tenant_context that differs from the authenticated tenant", func() {
		ctx, err := tenant.WithTenant(context.Background(), "tenant-b")
		Expect(err).NotTo(HaveOccurred())
		_, err = call(ctx, validRequest())
		Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied))
		Expect(err.Error()).To(ContainSubstring("tenant_context does not match the authenticated tenant"))
		Expect(reader.calls).To(BeZero())
	})

	It("accepts a tenant_context equal to the authenticated tenant", func() {
		ctx, err := tenant.WithTenant(context.Background(), "tenant-a")
		Expect(err).NotTo(HaveOccurred())
		_, err = call(ctx, validRequest())
		Expect(err).NotTo(HaveOccurred())
		Expect(reader.calls).To(Equal(1))
	})

	Describe("filter construction", func() {
		It("applies defaults", func() {
			_, err := call(context.Background(), validRequest())
			Expect(err).NotTo(HaveOccurred())
			Expect(reader.filter.TenantID).To(Equal("tenant-a"))
			Expect(reader.filter.Stage).To(Equal(results.StageSynthesized))
			Expect(reader.filter.PageSize).To(Equal(50))
			Expect(reader.filter.After).To(BeNil())
			Expect(reader.filter.RelationshipTypes).To(ContainElement("EQUIVALENT"))
			Expect(reader.filter.RelationshipTypes).NotTo(ContainElement("NO_RELATIONSHIP"))
			Expect(reader.filter.RelationshipTypes).To(HaveLen(len(pb.RelationshipType_name) - 2))
			Expect(sort.StringsAreSorted(reader.filter.RelationshipTypes)).To(BeTrue())
		})

		It("passes every field through", func() {
			cursor := results.Cursor{JobID: "job-1", SourceID: "s", TargetID: "t"}
			req := validRequest()
			req.Stage = pb.MappingStage_MAPPING_STAGE_SYNTHESIZED
			req.ControlId = "AC-1"
			req.MinConfidence = 0.5
			req.IncludeNonViable = true
			req.RelationshipTypes = []pb.RelationshipType{pb.RelationshipType_RELATIONSHIP_TYPE_PARTIAL, pb.RelationshipType_RELATIONSHIP_TYPE_NO_RELATIONSHIP}
			req.Pagination = &pb.Pagination{PageSize: 7, PageToken: results.EncodeCursor(cursor)}
			_, err := call(context.Background(), req)
			Expect(err).NotTo(HaveOccurred())
			Expect(reader.filter).To(Equal(results.Filter{
				TenantID: "tenant-a", JobID: "job-1", Stage: results.StageSynthesized,
				ControlID: "AC-1", MinConfidence: 0.5, IncludeNonViable: true,
				RelationshipTypes: []string{"PARTIAL", "NO_RELATIONSHIP"},
				PageSize:          7, After: &cursor,
			}))
		})

		It("passes min_confidence 0.8 to the reader as exactly 0.8", func() {
			req := validRequest()
			req.MinConfidence = 0.8
			_, err := call(context.Background(), req)
			Expect(err).NotTo(HaveOccurred())
			Expect(reader.filter.MinConfidence).To(Equal(0.8))
		})

		It("rejects a page token that belongs to a different job without reading", func() {
			req := validRequest()
			req.Pagination = &pb.Pagination{PageToken: results.EncodeCursor(results.Cursor{JobID: "job-2", SourceID: "s", TargetID: "t"})}
			_, err := call(context.Background(), req)
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(err.Error()).To(ContainSubstring("page_token belongs to a different job; pass the next_page_token from the previous response for this job unchanged"))
			Expect(reader.calls).To(BeZero())
		})

		It("accepts any cursor job on the all-jobs path", func() {
			req := validRequest()
			req.JobId, req.ControlId = "", "AC-1"
			req.Pagination = &pb.Pagination{PageToken: results.EncodeCursor(results.Cursor{JobID: "job-2", SourceID: "s", TargetID: "t"})}
			_, err := call(context.Background(), req)
			Expect(err).NotTo(HaveOccurred())
		})

		It("maps the analyzer stage", func() {
			req := validRequest()
			req.Stage = pb.MappingStage_MAPPING_STAGE_ANALYZER
			_, err := call(context.Background(), req)
			Expect(err).NotTo(HaveOccurred())
			Expect(reader.filter.Stage).To(Equal(results.StageAnalyzer))
		})

		It("allows a cross-job read of one control on the synthesized stage", func() {
			req := validRequest()
			req.JobId, req.ControlId = "", "AC-1"
			_, err := call(context.Background(), req)
			Expect(err).NotTo(HaveOccurred())
			Expect(reader.filter.JobID).To(BeEmpty())
			Expect(reader.filter.ControlID).To(Equal("AC-1"))
		})
	})

	Describe("response conversion", func() {
		It("derives mapping IDs, enums and viability", func() {
			next := results.Cursor{JobID: "job-1", SourceID: "s2", TargetID: "t2"}
			reader.page = results.Page{
				Mappings: []results.Mapping{
					{JobID: "job-1", SourceID: "s1", TargetID: "t1", RelationshipType: "SUBSET_OF", Confidence: 0.75, Viability: 0.5, HasViability: true},
					{JobID: "job-1", SourceID: "s2", TargetID: "t2", RelationshipType: "EQUIVALENT", Confidence: 0.25, Viability: 0, HasViability: true},
					{JobID: "job-1", SourceID: "s3", TargetID: "t3", RelationshipType: "PARTIAL", Confidence: 0.5},
				},
				Next: &next,
			}
			resp, err := call(context.Background(), validRequest())
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Mappings).To(HaveLen(3))

			first := resp.Mappings[0]
			Expect(first.MappingId).To(Equal(graphdb.DerivedID(analyzerresults.SemanticMatchEdgeIDKind, "job-1", "s1", "t1")))
			Expect(first.MappingId).To(Equal(graphdb.DerivedID("semantic-match", "job-1", "s1", "t1")))
			Expect(first.SourceControlId).To(Equal("s1"))
			Expect(first.TargetControlId).To(Equal("t1"))
			Expect(first.RelationshipType).To(Equal(pb.RelationshipType_RELATIONSHIP_TYPE_SUBSET_OF))
			Expect(first.Confidence).To(Equal(float32(0.75)))
			Expect(first.ViabilityScore).To(Equal(float32(0.5)))
			Expect(first.IsViable).To(BeTrue())

			Expect(resp.Mappings[1].IsViable).To(BeFalse())
			Expect(resp.Mappings[2].ViabilityScore).To(BeZero())
			Expect(resp.Mappings[2].IsViable).To(BeFalse())

			decoded, err := results.DecodeCursor(resp.PageInfo.NextPageToken)
			Expect(err).NotTo(HaveOccurred())
			Expect(decoded).To(Equal(&next))
		})

		It("leaves the next token empty on the last page", func() {
			resp, err := call(context.Background(), validRequest())
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.PageInfo.GetNextPageToken()).To(BeEmpty())
		})

		It("reports an unknown relationship name as UNSPECIFIED and warns", func() {
			reader.page = results.Page{Mappings: []results.Mapping{
				{JobID: "j", SourceID: "s", TargetID: "t", RelationshipType: "FUTURE_TYPE"},
				{JobID: "j", SourceID: "s", TargetID: "u", RelationshipType: "FUTURE_TYPE"},
			}}
			resp, err := call(context.Background(), validRequest())
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Mappings[0].RelationshipType).To(Equal(pb.RelationshipType_RELATIONSHIP_TYPE_UNSPECIFIED))
			Expect(strings.Count(logs.String(), "FUTURE_TYPE")).To(Equal(1), "warns once per call")
			Expect(logs.String()).To(ContainSubstring(`"level":"WARN"`))
		})
	})

	Describe("reader errors", func() {
		It("maps a missing job to NotFound with guidance", func() {
			reader.err = results.ErrJobNotFound
			_, err := call(context.Background(), validRequest())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
			Expect(err.Error()).To(ContainSubstring(`"job-1"`))
			Expect(err.Error()).To(ContainSubstring("crosscodex run status"))
		})

		It("maps an invalid page token to InvalidArgument and keeps the actionable message", func() {
			reader.err = fmt.Errorf("decode cursor: %w: malformed; pass the next_page_token from the previous response unchanged", results.ErrInvalidPageToken)
			_, err := call(context.Background(), validRequest())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(err.Error()).To(ContainSubstring("pass the next_page_token from the previous response unchanged"))
		})

		DescribeTable("maps an incomplete job to FailedPrecondition with status-specific guidance",
			func(status string, wantSubstrings ...string) {
				reader.err = &results.JobNotCompletedError{JobID: "job-1", Status: status}
				_, err := call(context.Background(), validRequest())
				Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
				for _, want := range wantSubstrings {
					Expect(err.Error()).To(ContainSubstring(want))
				}
			},
			Entry("pending", "pending", `job "job-1" is pending; mappings are available once it completes`, `crosscodex run status job-1`),
			Entry("running", "running", `job "job-1" is running; mappings are available once it completes`, `crosscodex run status job-1`),
			Entry("failed", "failed", `job "job-1" is failed and will not produce mappings`, `crosscodex run status job-1`, `crosscodex run start`),
			Entry("cancelled", "cancelled", `job "job-1" is cancelled and will not produce mappings`, `crosscodex run status job-1`, `crosscodex run start`),
			Entry("unrecognized status", "paused", `job "job-1" is paused; mappings are available once it completes`, `crosscodex run status job-1`),
		)

		It("hides an unexpected error from the client and logs it once", func() {
			reader.err = errors.New(`pq: relation "vote_summaries" SQLSTATE 42P01`)
			_, err := call(context.Background(), validRequest())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInternal))
			Expect(err.Error()).NotTo(ContainSubstring("vote_summaries"))
			Expect(err.Error()).NotTo(ContainSubstring("SQLSTATE"))
			Expect(strings.Count(logs.String(), "SQLSTATE 42P01")).To(Equal(1))
		})
	})

	Describe("telemetry", func() {
		var tp *telemetrytest.TestProvider
		BeforeEach(func() {
			var err error
			tp, err = telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { Expect(tp.Shutdown(context.Background())).To(Succeed()) })
			service = results.NewService(reader, results.WithTelemetry(tp.TracerProvider().Tracer("test"), tp.MeterProvider().Meter("test")))
		})

		It("traces, counts and times a successful call and counts returned mappings", func() {
			reader.page = results.Page{Mappings: []results.Mapping{
				{JobID: "j", SourceID: "s", TargetID: "t", RelationshipType: "EQUIVALENT"},
				{JobID: "j", SourceID: "s", TargetID: "u", RelationshipType: "EQUIVALENT"},
			}}
			req := validRequest()
			req.Stage = pb.MappingStage_MAPPING_STAGE_SYNTHESIZED
			_, err := call(context.Background(), req)
			Expect(err).NotTo(HaveOccurred())

			span := telemetrytest.FindSpan(tp.GetSpans(), "results.ListMappings")
			Expect(span).NotTo(BeNil())
			for key, want := range map[string]string{"tenant.id": "tenant-a", "job.id": "job-1", "stage": "MAPPING_STAGE_SYNTHESIZED"} {
				v, ok := telemetrytest.SpanAttribute(span, key)
				Expect(ok).To(BeTrue(), key)
				Expect(v.AsString()).To(Equal(want))
			}
			Expect(intCounterByAttr(tp, "results.requests.total", "status", "ok")).To(Equal(int64(1)))
			Expect(intCounterByAttr(tp, "results.mappings.returned.total", "", "")).To(Equal(int64(2)))
			m := telemetrytest.FindMetric(tp.GetMetrics(), "results.request.duration_ms")
			Expect(m).NotTo(BeNil())
			Expect(telemetrytest.Float64HistogramCount(m)).To(Equal(uint64(1)))
		})

		It("records the Connect code and span error for a refused call", func() {
			req := validRequest()
			req.JobId = ""
			_, err := call(context.Background(), req)
			Expect(err).To(HaveOccurred())
			span := telemetrytest.FindSpan(tp.GetSpans(), "results.ListMappings")
			Expect(span).NotTo(BeNil())
			Expect(span.Status().Code).To(Equal(codes.Error))
			Expect(intCounterByAttr(tp, "results.requests.total", "status", "invalid_argument")).To(Equal(int64(1)))
		})
	})
})
