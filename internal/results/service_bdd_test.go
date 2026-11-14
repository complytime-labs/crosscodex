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
	"google.golang.org/protobuf/proto"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/results"
	analyzerresults "github.com/complytime-labs/crosscodex/pkg/analyzer/results"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// fakeReader records the filters it receives and returns the configured results.
type fakeReader struct {
	page   results.Page
	err    error
	calls  int
	filter results.Filter

	candidatePage   results.CandidatePage
	candidateErr    error
	candidateCalls  int
	candidateFilter results.CandidateFilter
}

func (f *fakeReader) ListMappings(_ context.Context, flt results.Filter) (results.Page, error) {
	f.calls++
	f.filter = flt
	return f.page, f.err
}

func (f *fakeReader) ListCandidates(_ context.Context, flt results.CandidateFilter) (results.CandidatePage, error) {
	f.candidateCalls++
	f.candidateFilter = flt
	return f.candidatePage, f.candidateErr
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
			connect.CodeInvalidArgument, `job_id is required; pass the job ID printed by "crosscodex run start"`),
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
		Entry("vetoed pairs on the synthesized stage", func(r *pb.ListJobMappingsRequest) {
			r.Stage, r.IncludeVetoed = pb.MappingStage_MAPPING_STAGE_SYNTHESIZED, true
		}, connect.CodeInvalidArgument, "include_vetoed applies only to the analyzer stage: synthesis never receives vetoed pairs; request the analyzer stage to list them"),
		Entry("vetoed pairs on the default stage", func(r *pb.ListJobMappingsRequest) {
			r.Stage, r.IncludeVetoed = pb.MappingStage_MAPPING_STAGE_UNSPECIFIED, true
		}, connect.CodeInvalidArgument, "include_vetoed applies only to the analyzer stage"),
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
			Expect(reader.filter.IncludeVetoed).To(BeFalse())
		})

		It("passes include_vetoed through on the analyzer stage", func() {
			req := validRequest()
			req.Stage, req.IncludeVetoed = pb.MappingStage_MAPPING_STAGE_ANALYZER, true
			_, err := call(context.Background(), req)
			Expect(err).NotTo(HaveOccurred())
			Expect(reader.filter.IncludeVetoed).To(BeTrue())
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

		It("carries a mapping's critique and leaves it unset on a mapping without one", func() {
			reader.page = results.Page{Mappings: []results.Mapping{
				{JobID: "j", SourceID: "s", TargetID: "t", RelationshipType: "SUBSET_OF", Critique: &analyzerresults.CritiqueResult{
					Kind: analyzerresults.RelationshipAnalyzer, SourceControlID: "s", TargetControlID: "t", Upheld: 1, Rejected: 2, Failed: 0,
					Outcome: analyzerresults.CritiqueVetoed, Vetoed: true, Reasons: []string{"panel-1 s0 UPHOLD: ok", "panel-2 s0 REJECT: no", "panel-3 s0 REJECT: no"}}},
				{JobID: "j", SourceID: "s", TargetID: "u", RelationshipType: "SUBSET_OF"},
			}}
			resp, err := call(context.Background(), validRequest())
			Expect(err).NotTo(HaveOccurred())
			want := &pb.MappingCritique{Vetoed: true, Upheld: 1, Rejected: 2,
				Reasons: []string{"panel-1 s0 UPHOLD: ok", "panel-2 s0 REJECT: no", "panel-3 s0 REJECT: no"}}
			Expect(proto.Equal(resp.Mappings[0].Critique, want)).To(BeTrue(), "critique %v", resp.Mappings[0].Critique)
			Expect(resp.Mappings[1].Critique).To(BeNil())
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

	Describe("the job's analysis", func() {
		It("converts the page's analysis to the response", func() {
			reader.page = results.Page{Analysis: &results.JobAnalysis{
				Source:        results.CatalogSummary{ID: "cat-src", Name: "CSF 2.0", Version: "2.0", Imported: true, Snapshot: true},
				Target:        results.CatalogSummary{ID: "cat-gone"},
				Bidirectional: true,
			}}
			resp, err := call(context.Background(), validRequest())
			Expect(err).NotTo(HaveOccurred())
			Expect(proto.Equal(resp.GetAnalysis(), &pb.JobAnalysis{
				SourceCatalog: &pb.CatalogSummary{CatalogId: "cat-src", Name: "CSF 2.0", Version: "2.0", Imported: true, Snapshot: true},
				TargetCatalog: &pb.CatalogSummary{CatalogId: "cat-gone"},
				Bidirectional: true,
			})).To(BeTrue(), "got %v", resp.GetAnalysis())
		})

		It("leaves the analysis unset when the page has none", func() {
			resp, err := call(context.Background(), validRequest())
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.GetAnalysis()).To(BeNil())
		})
	})
})

var _ = Describe("Service.ListCandidates", func() {
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

	validCandidatesRequest := func() *pb.ListJobCandidatesRequest {
		return &pb.ListJobCandidatesRequest{
			TenantContext: &pb.TenantContext{TenantId: "tenant-a"},
			JobId:         "job-1",
		}
	}
	call := func(ctx context.Context, msg *pb.ListJobCandidatesRequest) (*pb.ListJobCandidatesResponse, error) {
		resp, err := service.ListCandidates(ctx, connect.NewRequest(msg))
		if err != nil {
			return nil, err
		}
		return resp.Msg, nil
	}

	DescribeTable("rejects invalid requests without reading",
		func(mutate func(*pb.ListJobCandidatesRequest), code connect.Code, substr string) {
			req := validCandidatesRequest()
			mutate(req)
			_, err := call(context.Background(), req)
			Expect(connect.CodeOf(err)).To(Equal(code))
			Expect(err.Error()).To(ContainSubstring(substr))
			Expect(reader.candidateCalls).To(BeZero())
		},
		Entry("missing tenant context", func(r *pb.ListJobCandidatesRequest) { r.TenantContext = nil },
			connect.CodeInvalidArgument, "tenant_context.tenant_id is required"),
		Entry("malformed tenant id", func(r *pb.ListJobCandidatesRequest) { r.TenantContext.TenantId = "../other" },
			connect.CodeInvalidArgument, "invalid tenant_context.tenant_id"),
		Entry("empty job id", func(r *pb.ListJobCandidatesRequest) { r.JobId = "" },
			connect.CodeInvalidArgument, "job_id is required"),
		Entry("negative page size", func(r *pb.ListJobCandidatesRequest) { r.Pagination = &pb.Pagination{PageSize: -1} },
			connect.CodeInvalidArgument, "page_size must be between 1 and 1000"),
		Entry("oversized page", func(r *pb.ListJobCandidatesRequest) { r.Pagination = &pb.Pagination{PageSize: 1001} },
			connect.CodeInvalidArgument, "page_size must be between 1 and 1000"),
		Entry("forged page token", func(r *pb.ListJobCandidatesRequest) { r.Pagination = &pb.Pagination{PageToken: "!!not-a-token"} },
			connect.CodeInvalidArgument, "next_page_token"),
		Entry("page token of another job", func(r *pb.ListJobCandidatesRequest) {
			r.Pagination = &pb.Pagination{PageToken: results.EncodeCursor(results.Cursor{JobID: "job-2", SourceID: "s", TargetID: "t"})}
		}, connect.CodeInvalidArgument, "page_token belongs to a different job"),
	)

	It("refuses a tenant_context that differs from the authenticated tenant", func() {
		ctx, err := tenant.WithTenant(context.Background(), "tenant-b")
		Expect(err).NotTo(HaveOccurred())
		_, err = call(ctx, validCandidatesRequest())
		Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied))
		Expect(err.Error()).To(ContainSubstring("tenant_context does not match the authenticated tenant"))
		Expect(reader.candidateCalls).To(BeZero())
	})

	It("applies the default page size", func() {
		_, err := call(context.Background(), validCandidatesRequest())
		Expect(err).NotTo(HaveOccurred())
		Expect(reader.candidateFilter).To(Equal(results.CandidateFilter{TenantID: "tenant-a", JobID: "job-1", PageSize: 50}))
	})

	It("passes every field through", func() {
		cursor := results.Cursor{JobID: "job-1", SourceID: "s", TargetID: "t"}
		req := validCandidatesRequest()
		req.ControlId = "AC-1"
		req.Pagination = &pb.Pagination{PageSize: 7, PageToken: results.EncodeCursor(cursor)}
		_, err := call(context.Background(), req)
		Expect(err).NotTo(HaveOccurred())
		Expect(reader.candidateFilter).To(Equal(results.CandidateFilter{
			TenantID: "tenant-a", JobID: "job-1", ControlID: "AC-1", PageSize: 7, After: &cursor,
		}))
	})

	It("converts similarity, contributions and analysis and sets the next token", func() {
		next := results.Cursor{JobID: "job-1", SourceID: "s1", TargetID: "t1"}
		reader.candidatePage = results.CandidatePage{
			Candidates: []results.Candidate{
				{JobID: "job-1", SourceID: "s1", TargetID: "t1", Similarity: 87.5, Contributions: []results.Contribution{
					{Generator: "embedding", Score: 0.9, Weight: 0.6, Metadata: map[string]string{"rank": "2"}},
					{Generator: "bm25", Score: 3.5, Weight: 0.4},
				}},
				{JobID: "job-1", SourceID: "s2", TargetID: "t2"},
			},
			Next: &next,
			Analysis: &results.JobAnalysis{
				Source: results.CatalogSummary{ID: "cat-src", Name: "SSDF", Imported: true},
				Target: results.CatalogSummary{ID: "cat-tgt"},
			},
		}
		resp, err := call(context.Background(), validCandidatesRequest())
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Candidates).To(HaveLen(2))

		first := resp.Candidates[0]
		Expect(first.SourceControlId).To(Equal("s1"))
		Expect(first.TargetControlId).To(Equal("t1"))
		Expect(first.SimilarityScore).To(Equal(float32(87.5)))
		Expect(first.Contributions).To(HaveLen(2))
		Expect(first.Contributions[0].Generator).To(Equal("embedding"))
		Expect(first.Contributions[0].Score).To(Equal(0.9))
		Expect(first.Contributions[0].Weight).To(Equal(0.6))
		Expect(first.Contributions[0].Metadata).To(Equal(map[string]string{"rank": "2"}))
		Expect(first.Contributions[1].Generator).To(Equal("bm25"))
		Expect(resp.Candidates[1].Contributions).To(BeEmpty())

		Expect(resp.Analysis.GetSourceCatalog().GetCatalogId()).To(Equal("cat-src"))
		Expect(resp.Analysis.GetSourceCatalog().GetImported()).To(BeTrue())
		Expect(resp.Analysis.GetTargetCatalog().GetCatalogId()).To(Equal("cat-tgt"))

		decoded, err := results.DecodeCursor(resp.PageInfo.NextPageToken)
		Expect(err).NotTo(HaveOccurred())
		Expect(decoded).To(Equal(&next))
	})

	It("leaves the next token empty and the analysis unset on the last page without one", func() {
		resp, err := call(context.Background(), validCandidatesRequest())
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.PageInfo.GetNextPageToken()).To(BeEmpty())
		Expect(resp.Analysis).To(BeNil())
	})

	Describe("reader errors", func() {
		It("maps a missing job to the mappings NotFound message", func() {
			reader.candidateErr = results.ErrJobNotFound
			_, err := call(context.Background(), validCandidatesRequest())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
			Expect(err.Error()).To(ContainSubstring(`job "job-1" not found for this tenant`))
			Expect(err.Error()).To(ContainSubstring("crosscodex run status"))
		})

		It("maps a job that has not completed to FailedPrecondition with the next step", func() {
			reader.candidateErr = &results.JobNotCompletedError{JobID: "job-1", Status: "running"}
			_, err := call(context.Background(), validCandidatesRequest())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
			Expect(err.Error()).To(ContainSubstring("candidates are available once it completes"))
		})

		It("maps an invalid page token to InvalidArgument", func() {
			reader.candidateErr = fmt.Errorf("decode cursor: %w", results.ErrInvalidPageToken)
			_, err := call(context.Background(), validCandidatesRequest())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		})

		It("hides corrupt provenance behind Internal and logs the cause once under the right method", func() {
			reader.candidateErr = fmt.Errorf("row s1->t1: %w: generator is not a string", results.ErrCorruptProvenance)
			_, err := call(context.Background(), validCandidatesRequest())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInternal))
			Expect(err.Error()).NotTo(ContainSubstring("provenance"))
			Expect(err.Error()).NotTo(ContainSubstring("s1->t1"))
			Expect(strings.Count(logs.String(), "generator is not a string")).To(Equal(1))
			Expect(logs.String()).To(ContainSubstring(`"method":"ListCandidates"`))
		})
	})

	Describe("telemetry", func() {
		It("traces the call and counts the returned candidates", func() {
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { Expect(tp.Shutdown(context.Background())).To(Succeed()) })
			service = results.NewService(reader, results.WithTelemetry(tp.TracerProvider().Tracer("test"), tp.MeterProvider().Meter("test")))
			reader.candidatePage = results.CandidatePage{Candidates: []results.Candidate{
				{JobID: "j", SourceID: "s", TargetID: "t"}, {JobID: "j", SourceID: "s", TargetID: "u"},
			}}
			_, err = call(context.Background(), validCandidatesRequest())
			Expect(err).NotTo(HaveOccurred())

			span := telemetrytest.FindSpan(tp.GetSpans(), "results.ListCandidates")
			Expect(span).NotTo(BeNil())
			for key, want := range map[string]string{"tenant.id": "tenant-a", "job.id": "job-1"} {
				v, ok := telemetrytest.SpanAttribute(span, key)
				Expect(ok).To(BeTrue(), key)
				Expect(v.AsString()).To(Equal(want))
			}
			Expect(intCounterByAttr(tp, "results.requests.total", "status", "ok")).To(Equal(int64(1)))
			Expect(intCounterByAttr(tp, "results.candidates.returned.total", "", "")).To(Equal(int64(2)))
		})
	})
})
