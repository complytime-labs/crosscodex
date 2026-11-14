//go:build !integration

package gateway_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/gateway"
)

var _ gateway.CrosswalkBackend = (*mockCrosswalk)(nil)

// mockCrosswalk records the request it receives and returns a configured
// response or error.
type mockCrosswalk struct {
	got  *pb.GetCrosswalkRequest
	resp *pb.GetCrosswalkResponse
	err  error
}

func (m *mockCrosswalk) GetCrosswalk(_ context.Context, req *connect.Request[pb.GetCrosswalkRequest]) (*connect.Response[pb.GetCrosswalkResponse], error) {
	m.got = req.Msg
	if m.err != nil {
		return nil, m.err
	}
	if m.resp != nil {
		return connect.NewResponse(m.resp), nil
	}
	return connect.NewResponse(&pb.GetCrosswalkResponse{}), nil
}

var _ = Describe("GetCrosswalk", func() {
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

	// job builds a job whose last stage completed at t0 plus minutes; an
	// earlier stage completed before it, so the job's time is the latest
	// stage's.
	job := func(id, src, tgt string, status pb.JobStatus, minutes int) *pb.PipelineJob {
		return &pb.PipelineJob{
			JobId:  id,
			Status: status,
			Config: &pb.JobConfig{CatalogId: src, TargetCatalogId: tgt},
			Stages: []*pb.JobStage{
				{StageName: "analysis", CompletedAt: timestamppb.New(t0.Add(time.Duration(minutes-1) * time.Minute))},
				{StageName: "graph", CompletedAt: timestamppb.New(t0.Add(time.Duration(minutes) * time.Minute))},
				{StageName: "pending"},
			},
			Audit: &pb.AuditMetadata{CreatedBy: "user-b"},
		}
	}
	done := pb.JobStatus_JOB_STATUS_COMPLETED

	var (
		pipelineReqs []*pb.ListJobsRequest
		lookups      []*pb.GetCatalogRequest
		backend      *mockCrosswalk
		jobs         []*pb.PipelineJob
		pages        map[string][]*pb.PipelineJob
		known        map[string]*pb.Catalog
	)

	newSvc := func(opts ...gateway.ServiceOption) *gateway.Service {
		// Like a backend that ignores the status filter, the pipeline mock
		// returns every job: the handler must drop the unfinished ones.
		pipeline := &mockPipeline{listJobsFn: func(_ context.Context, req *connect.Request[pb.ListJobsRequest]) (*connect.Response[pb.ListJobsResponse], error) {
			pipelineReqs = append(pipelineReqs, req.Msg)
			if pages != nil {
				token := req.Msg.GetOptions().GetPagination().GetPageToken()
				next := map[string]string{"": "p2", "p2": ""}[token]
				return connect.NewResponse(&pb.ListJobsResponse{Jobs: pages[token], PageInfo: &pb.PageInfo{NextPageToken: next}}), nil
			}
			return connect.NewResponse(&pb.ListJobsResponse{Jobs: jobs}), nil
		}}
		catalog := &mockCatalog{getCatalogFn: func(_ context.Context, req *connect.Request[pb.GetCatalogRequest]) (*connect.Response[pb.GetCatalogResponse], error) {
			lookups = append(lookups, req.Msg)
			c, ok := known[req.Msg.GetCatalogId()]
			if !ok {
				return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("catalog %s: no rows", req.Msg.GetCatalogId()))
			}
			return connect.NewResponse(&pb.GetCatalogResponse{Catalog: c}), nil
		}}
		return newTestService(append([]gateway.ServiceOption{
			gateway.WithPipelineBackend(pipeline),
			gateway.WithCatalogBackend(catalog),
			gateway.WithCrosswalkBackend(backend),
		}, opts...)...)
	}

	pair := func(a, b string) *pb.GetCrosswalkRequest {
		return &pb.GetCrosswalkRequest{Mode: &pb.GetCrosswalkRequest_Pair{Pair: &pb.CrosswalkPair{CatalogA: a, CatalogB: b}}}
	}
	call := func(svc *gateway.Service, msg *pb.GetCrosswalkRequest) (*connect.Response[pb.GetCrosswalkResponse], error) {
		return svc.GetCrosswalk(userCtx("user-a"), connect.NewRequest(msg))
	}
	jobIDs := func(msg *pb.GetCrosswalkRequest) []string {
		ids := []string{}
		for _, j := range msg.GetJobs() {
			ids = append(ids, j.GetJobId())
		}
		return ids
	}

	BeforeEach(func() {
		pipelineReqs, lookups, pages = nil, nil, nil
		backend = &mockCrosswalk{}
		known = map[string]*pb.Catalog{}
		for _, id := range []string{"cat-a", "cat-b", "cat-c", "cat-hub"} {
			known[id] = &pb.Catalog{CatalogId: id, Name: "name " + id, Version: "v-now"}
		}
		jobs = []*pb.PipelineJob{
			job("ab", "cat-a", "cat-b", done, 1),
			job("ba", "cat-b", "cat-a", done, 2),
			job("ac", "cat-a", "cat-c", done, 3),
			job("bc", "cat-b", "cat-c", done, 4),
			job("aa", "cat-a", "cat-a", done, 5),
			job("hub-a", "cat-hub", "cat-a", done, 6),
			job("hub-c", "cat-c", "cat-hub", done, 7),
			job("running", "cat-a", "cat-b", pb.JobStatus_JOB_STATUS_RUNNING, 8),
			job("failed", "cat-a", "cat-b", pb.JobStatus_JOB_STATUS_FAILED, 9),
			job("cancelled", "cat-a", "cat-b", pb.JobStatus_JOB_STATUS_CANCELLED, 10),
		}
	})

	DescribeTable("selects the completed jobs the mode draws on, oldest first",
		func(msg *pb.GetCrosswalkRequest, want []string) {
			_, err := call(newSvc(), msg)
			Expect(err).NotTo(HaveOccurred())
			Expect(jobIDs(backend.got)).To(Equal(want))
		},
		Entry("pair takes either direction", pair("cat-a", "cat-b"), []string{"ab", "ba"}),
		Entry("pair is symmetric in its arguments", pair("cat-b", "cat-a"), []string{"ab", "ba"}),
		Entry("pair skips jobs of other catalog pairs", pair("cat-b", "cat-c"), []string{"bc"}),
		Entry("job takes that job", &pb.GetCrosswalkRequest{Mode: &pb.GetCrosswalkRequest_JobId{JobId: "aa"}}, []string{"aa"}),
		Entry("one-to-many takes every cross-catalog job touching the catalog",
			&pb.GetCrosswalkRequest{Mode: &pb.GetCrosswalkRequest_OneToManyCatalogId{OneToManyCatalogId: "cat-a"}},
			[]string{"ab", "ba", "ac", "hub-a"}),
		Entry("transitive takes both legs and the direct jobs",
			&pb.GetCrosswalkRequest{Mode: &pb.GetCrosswalkRequest_Transitive{
				Transitive: &pb.CrosswalkTransitive{CatalogA: "cat-a", CatalogC: "cat-c", HubCatalogId: "cat-hub"}}},
			[]string{"ac", "hub-a", "hub-c"}),
	)

	It("reads direction, catalogs and the latest stage time from each job", func() {
		jobs[0].Config.Bidirectional = true
		_, err := call(newSvc(), pair("cat-a", "cat-b"))
		Expect(err).NotTo(HaveOccurred())
		first := backend.got.GetJobs()[0]
		Expect(first.GetSourceCatalogId()).To(Equal("cat-a"))
		Expect(first.GetTargetCatalogId()).To(Equal("cat-b"))
		Expect(first.GetBidirectional()).To(BeTrue())
		Expect(first.GetCompletedAt().AsTime()).To(Equal(t0.Add(time.Minute)))
	})

	It("lists the tenant's completed jobs, not the caller's own", func() {
		_, err := call(newSvc(), pair("cat-a", "cat-b"))
		Expect(err).NotTo(HaveOccurred())
		Expect(pipelineReqs).To(HaveLen(1))
		Expect(pipelineReqs[0].GetTenantContext().GetTenantId()).To(Equal("tenant-a"))
		Expect(pipelineReqs[0].GetStatus()).To(Equal(done))
		Expect(jobIDs(backend.got)).To(ContainElements("ab", "ba"), "jobs created by user-b reach user-a: crosswalks are tenant-scoped")
	})

	It("follows the job listing's page tokens", func() {
		pages = map[string][]*pb.PipelineJob{"": {jobs[0]}, "p2": {jobs[1]}}
		_, err := call(newSvc(), pair("cat-a", "cat-b"))
		Expect(err).NotTo(HaveOccurred())
		Expect(pipelineReqs).To(HaveLen(2))
		Expect(jobIDs(backend.got)).To(Equal([]string{"ab", "ba"}))
	})

	It("fails when the job listing's page token does not advance", func() {
		svc := newSvc(gateway.WithPipelineBackend(&mockPipeline{listJobsFn: func(context.Context, *connect.Request[pb.ListJobsRequest]) (*connect.Response[pb.ListJobsResponse], error) {
			return connect.NewResponse(&pb.ListJobsResponse{PageInfo: &pb.PageInfo{NextPageToken: "stuck"}}), nil
		}}))
		_, err := call(svc, pair("cat-a", "cat-b"))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInternal))
		Expect(backend.got).To(BeNil())
	})

	It("tenant-scopes the catalog lookups and sets the tenant on the backend request", func() {
		msg := pair("cat-a", "cat-b")
		msg.TenantContext = &pb.TenantContext{TenantId: "tenant-other"}
		_, err := call(newSvc(), msg)
		Expect(err).NotTo(HaveOccurred())
		Expect(backend.got.GetTenantContext().GetTenantId()).To(Equal("tenant-a"))
		Expect(lookups).To(HaveLen(2))
		for _, l := range lookups {
			Expect(l.GetTenantContext().GetTenantId()).To(Equal("tenant-a"))
		}
	})

	It("overwrites the jobs and catalogs a client supplies", func() {
		msg := pair("cat-a", "cat-b")
		msg.Jobs = []*pb.CrosswalkJob{{JobId: "forged"}}
		msg.Catalogs = []*pb.CatalogSummary{{CatalogId: "forged", Name: "forged", Imported: true}}
		_, err := call(newSvc(), msg)
		Expect(err).NotTo(HaveOccurred())
		Expect(jobIDs(backend.got)).To(Equal([]string{"ab", "ba"}))
		Expect(backend.got.GetCatalogs()).To(HaveLen(2))
		Expect(backend.got.GetCatalogs()[0].GetCatalogId()).To(Equal("cat-a"))
	})

	Describe("catalogs", func() {
		It("reports the named catalogs' current names when no job recorded a label", func() {
			_, err := call(newSvc(), pair("cat-a", "cat-b"))
			Expect(err).NotTo(HaveOccurred())
			Expect(backend.got.GetCatalogs()).To(HaveLen(2))
			a := backend.got.GetCatalogs()[0]
			Expect(a.GetName()).To(Equal("name cat-a"))
			Expect(a.GetVersion()).To(Equal("v-now"))
			Expect(a.GetImported()).To(BeTrue())
			Expect(a.GetSnapshot()).To(BeFalse())
		})

		It("uses the newest contributing job's recorded label as a snapshot", func() {
			jobs[0].Config.SourceCatalog = &pb.CatalogSnapshot{CatalogId: "cat-a", Name: "old A", Version: "v1"}
			jobs[1].Config.TargetCatalog = &pb.CatalogSnapshot{CatalogId: "cat-a", Name: "newer A", Version: "v2"}
			// The newest job (ba) recorded nothing for cat-b, the older one did.
			jobs[0].Config.TargetCatalog = &pb.CatalogSnapshot{CatalogId: "cat-b", Name: "B at ab", Version: "v9"}
			_, err := call(newSvc(), pair("cat-a", "cat-b"))
			Expect(err).NotTo(HaveOccurred())
			a, b := backend.got.GetCatalogs()[0], backend.got.GetCatalogs()[1]
			Expect(a.GetName()).To(Equal("newer A"))
			Expect(a.GetVersion()).To(Equal("v2"))
			Expect(a.GetSnapshot()).To(BeTrue())
			Expect(b.GetName()).To(Equal("B at ab"))
			Expect(b.GetSnapshot()).To(BeTrue())
		})

		It("ignores a snapshot of another catalog", func() {
			jobs[0].Config.SourceCatalog = &pb.CatalogSnapshot{CatalogId: "cat-zzz", Name: "other", Version: "v1"}
			_, err := call(newSvc(), pair("cat-a", "cat-b"))
			Expect(err).NotTo(HaveOccurred())
			Expect(backend.got.GetCatalogs()[0].GetName()).To(Equal("name cat-a"))
			Expect(backend.got.GetCatalogs()[0].GetSnapshot()).To(BeFalse())
		})

		It("lists the other catalogs of one-to-many mode in ID order, after the named one", func() {
			_, err := call(newSvc(), &pb.GetCrosswalkRequest{Mode: &pb.GetCrosswalkRequest_OneToManyCatalogId{OneToManyCatalogId: "cat-a"}})
			Expect(err).NotTo(HaveOccurred())
			ids := []string{}
			for _, c := range backend.got.GetCatalogs() {
				ids = append(ids, c.GetCatalogId())
			}
			Expect(ids).To(Equal([]string{"cat-a", "cat-b", "cat-c", "cat-hub"}))
		})

		It("derives the catalogs of job mode from the job and tolerates a deleted one", func() {
			delete(known, "cat-b")
			_, err := call(newSvc(), &pb.GetCrosswalkRequest{Mode: &pb.GetCrosswalkRequest_JobId{JobId: "ab"}})
			Expect(err).NotTo(HaveOccurred())
			Expect(backend.got.GetCatalogs()).To(HaveLen(2))
			Expect(backend.got.GetCatalogs()[0].GetImported()).To(BeTrue())
			Expect(backend.got.GetCatalogs()[1].GetCatalogId()).To(Equal("cat-b"))
			Expect(backend.got.GetCatalogs()[1].GetImported()).To(BeFalse())
		})
	})

	Describe("no completed jobs between known catalogs", func() {
		It("calls the backend with no jobs and passes its response through", func() {
			known["cat-new"] = &pb.Catalog{CatalogId: "cat-new"}
			backend.resp = &pb.GetCrosswalkResponse{Provenance: &pb.CrosswalkProvenance{Mode: "pair"}}
			resp, err := call(newSvc(), pair("cat-a", "cat-new"))
			Expect(err).NotTo(HaveOccurred())
			Expect(backend.got).NotTo(BeNil())
			Expect(backend.got.GetJobs()).To(BeEmpty())
			Expect(resp.Msg.GetProvenance().GetMode()).To(Equal("pair"))
		})
	})

	Describe("NotFound", func() {
		It("answers an unknown catalog and another tenant's catalog identically", func() {
			// The store words its two refusals differently; the caller must
			// not be able to tell them apart.
			svc := newSvc(gateway.WithCatalogBackend(&mockCatalog{getCatalogFn: func(_ context.Context, req *connect.Request[pb.GetCatalogRequest]) (*connect.Response[pb.GetCatalogResponse], error) {
				switch req.Msg.GetCatalogId() {
				case "cat-other":
					return nil, connect.NewError(connect.CodeNotFound, errors.New("catalog owned by tenant-b"))
				case "cat-unknown":
					return nil, connect.NewError(connect.CodeNotFound, errors.New("no such catalog"))
				}
				return connect.NewResponse(&pb.GetCatalogResponse{Catalog: known[req.Msg.GetCatalogId()]}), nil
			}}))
			_, unknown := call(svc, pair("cat-a", "cat-unknown"))
			_, foreign := call(svc, pair("cat-a", "cat-other"))
			Expect(connect.CodeOf(unknown)).To(Equal(connect.CodeNotFound))
			Expect(connect.CodeOf(foreign)).To(Equal(connect.CodeNotFound))
			Expect(unknown.Error()).To(ContainSubstring(`catalog "cat-unknown" not found`))
			Expect(strings.ReplaceAll(foreign.Error(), "cat-other", "cat-unknown")).To(Equal(unknown.Error()))
			Expect(foreign.Error()).NotTo(ContainSubstring("tenant-b"))
			Expect(backend.got).To(BeNil())
		})

		DescribeTable("answers a job that is not a completed job of the tenant with one message",
			func(jobID string) {
				_, err := call(newSvc(), &pb.GetCrosswalkRequest{Mode: &pb.GetCrosswalkRequest_JobId{JobId: jobID}})
				Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
				Expect(err.Error()).To(ContainSubstring(fmt.Sprintf("job %q not found: your tenant has no completed job with that ID", jobID)))
				Expect(backend.got).To(BeNil())
			},
			Entry("running job", "running"),
			Entry("failed job", "failed"),
			Entry("cancelled job", "cancelled"),
			Entry("unknown or other tenant's job", "job-of-tenant-b"),
		)
	})

	Describe("argument errors", func() {
		transitive := func(a, c, hub string) *pb.GetCrosswalkRequest {
			return &pb.GetCrosswalkRequest{Mode: &pb.GetCrosswalkRequest_Transitive{
				Transitive: &pb.CrosswalkTransitive{CatalogA: a, CatalogC: c, HubCatalogId: hub}}}
		}
		withFilters := func(mutate func(*pb.GetCrosswalkRequest)) *pb.GetCrosswalkRequest {
			msg := pair("cat-a", "cat-b")
			mutate(msg)
			return msg
		}

		DescribeTable("rejects the request with an actionable InvalidArgument before any lookup",
			func(msg *pb.GetCrosswalkRequest, want string) {
				_, err := call(newSvc(), msg)
				Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
				Expect(err.Error()).To(ContainSubstring(want))
				Expect(backend.got).To(BeNil())
				Expect(lookups).To(BeEmpty())
				Expect(pipelineReqs).To(BeEmpty())
			},
			Entry("no mode", &pb.GetCrosswalkRequest{}, "no crosswalk mode set: name two catalogs"),
			Entry("pair without a second catalog", pair("cat-a", ""), "pair mode needs two catalogs: crosscodex crosswalk <a> <b>"),
			Entry("pair of one catalog twice", pair("cat-a", "cat-a"),
				"pair mode needs two different catalogs; for one catalog's internal mappings use --job <job-id>"),
			Entry("job without an ID", &pb.GetCrosswalkRequest{Mode: &pb.GetCrosswalkRequest_JobId{}}, "job mode needs a job ID"),
			Entry("one-to-many without a catalog", &pb.GetCrosswalkRequest{Mode: &pb.GetCrosswalkRequest_OneToManyCatalogId{}}, "--all needs a catalog"),
			Entry("transitive without a hub", transitive("cat-a", "cat-c", ""), "--via needs two catalogs: crosscodex crosswalk <a> <c> --via <hub>"),
			Entry("transitive with the hub equal to an end", transitive("cat-a", "cat-c", "cat-a"), "transitive mode needs three different catalogs"),
			Entry("transitive with equal ends", transitive("cat-a", "cat-a", "cat-hub"), "transitive mode needs three different catalogs"),
			Entry("unknown stage", withFilters(func(m *pb.GetCrosswalkRequest) { m.Stage = 99 }), "unknown crosswalk stage 99"),
			Entry("negative confidence", withFilters(func(m *pb.GetCrosswalkRequest) { m.MinConfidence = -0.1 }), "min_confidence -0.1 is out of range"),
			Entry("confidence above one", withFilters(func(m *pb.GetCrosswalkRequest) { m.MinConfidence = 1.5 }), "min_confidence 1.5 is out of range"),
			Entry("unspecified relationship type", withFilters(func(m *pb.GetCrosswalkRequest) {
				m.RelationshipTypes = []pb.RelationshipType{pb.RelationshipType_RELATIONSHIP_TYPE_UNSPECIFIED}
			}), "relationship type 0 is not a verdict"),
			Entry("unknown relationship type", withFilters(func(m *pb.GetCrosswalkRequest) {
				m.RelationshipTypes = []pb.RelationshipType{99}
			}), "relationship type 99 is not a verdict"),
			Entry("no-relationship type", withFilters(func(m *pb.GetCrosswalkRequest) {
				m.RelationshipTypes = []pb.RelationshipType{
					pb.RelationshipType_RELATIONSHIP_TYPE_EQUIVALENT, pb.RelationshipType_RELATIONSHIP_TYPE_NO_RELATIONSHIP}
			}), "no-relationship verdicts are not stored: the relationship analyzer omits them, so a crosswalk lists related pairs only"),
			Entry("negative page size", withFilters(func(m *pb.GetCrosswalkRequest) { m.Pagination = &pb.Pagination{PageSize: -1} }), "page_size -1 is out of range"),
			Entry("page size above the maximum", withFilters(func(m *pb.GetCrosswalkRequest) { m.Pagination = &pb.Pagination{PageSize: 1001} }), "page_size 1001 is out of range"),
		)

		It("accepts the boundary filter values", func() {
			msg := withFilters(func(m *pb.GetCrosswalkRequest) {
				m.MinConfidence = 1
				m.Pagination = &pb.Pagination{PageSize: 1000}
				m.Stage = pb.CrosswalkStage_CROSSWALK_STAGE_ANALYZER
				m.RelationshipTypes = []pb.RelationshipType{pb.RelationshipType_RELATIONSHIP_TYPE_PARTIAL}
			})
			_, err := call(newSvc(), msg)
			Expect(err).NotTo(HaveOccurred())
			Expect(backend.got.GetStage()).To(Equal(pb.CrosswalkStage_CROSSWALK_STAGE_ANALYZER))
		})

		It("passes include_non_viable to the backend", func() {
			_, err := call(newSvc(), withFilters(func(m *pb.GetCrosswalkRequest) { m.IncludeNonViable = true }))
			Expect(err).NotTo(HaveOccurred())
			Expect(backend.got.GetIncludeNonViable()).To(BeTrue())
		})
	})

	Describe("backends", func() {
		It("returns Unavailable when the crosswalk backend is nil", func() {
			_, err := call(newTestService(), pair("cat-a", "cat-b"))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnavailable))
		})

		It("returns a backend's raw error as an opaque internal error", func() {
			backend.err = errors.New(`pq: relation "graph_tenant_a" does not exist`)
			_, err := call(newSvc(), pair("cat-a", "cat-b"))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInternal))
			Expect(err.Error()).NotTo(ContainSubstring("graph_tenant_a"))
		})

		It("returns an unauthenticated error without an identity", func() {
			_, err := newSvc().GetCrosswalk(context.Background(), connect.NewRequest(pair("cat-a", "cat-b")))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated))
		})

		It("returns a pipeline error as an opaque internal error", func() {
			svc := newSvc(gateway.WithPipelineBackend(&mockPipeline{listJobsFn: func(context.Context, *connect.Request[pb.ListJobsRequest]) (*connect.Response[pb.ListJobsResponse], error) {
				return nil, errors.New("dial tcp 10.0.0.7:5432: refused")
			}}))
			_, err := call(svc, pair("cat-a", "cat-b"))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInternal))
			Expect(err.Error()).NotTo(ContainSubstring("10.0.0.7"))
		})
	})
})
