package graph_test

import (
	"context"
	"encoding/base64"
	"strings"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/graph"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/graphdb/memdriver"
)

// traverseCounter wraps a graph and records the start node of every Traverse.
type traverseCounter struct {
	graphdb.GraphDB
	starts []string
}

func (c *traverseCounter) Traverse(ctx context.Context, tenantID string, q graphdb.TraversalQuery) ([]graphdb.Path, error) {
	c.starts = append(c.starts, q.StartNode)
	return c.GraphDB.Traverse(ctx, tenantID, q)
}

// readCounter wraps a graph and counts every read the crosswalk could make.
type readCounter struct {
	graphdb.GraphDB
	reads int
}

func (c *readCounter) QueryRelationships(ctx context.Context, tenantID string, q graphdb.RelationshipQuery) ([]graphdb.Relationship, error) {
	c.reads++
	return c.GraphDB.QueryRelationships(ctx, tenantID, q)
}

func (c *readCounter) Traverse(ctx context.Context, tenantID string, q graphdb.TraversalQuery) ([]graphdb.Path, error) {
	c.reads++
	return c.GraphDB.Traverse(ctx, tenantID, q)
}

var _ = Describe("GetCrosswalk", func() {
	const (
		tenantID = "test-tenant"
		catA     = testspecs.CrosswalkCatA
		catB     = testspecs.CrosswalkCatB
	)
	var (
		ctx  context.Context
		mem  graphdb.GraphDB
		svc  *graph.Service
		t0   = testspecs.CrosswalkEpoch
		jobs map[string]*pb.CrosswalkJob
	)

	addNode := func(id, catalog, identifier string) {
		Expect(testspecs.AddCrosswalkNode(ctx, mem, tenantID, id, catalog, identifier)).To(Succeed())
	}
	link := func(job, src, tgt, stage string, typ pb.RelationshipType, conf float64) {
		Expect(testspecs.LinkCrosswalk(ctx, mem, tenantID, job, src, tgt, stage, typ, conf)).To(Succeed())
	}
	addJob := func(id, src, tgt string, bidirectional bool, completed time.Time) {
		jobs[id] = &pb.CrosswalkJob{JobId: id, SourceCatalogId: src, TargetCatalogId: tgt,
			Bidirectional: bidirectional, CompletedAt: timestamppb.New(completed)}
	}
	selected := func(ids ...string) []*pb.CrosswalkJob {
		out := make([]*pb.CrosswalkJob, 0, len(ids))
		for _, id := range ids {
			out = append(out, jobs[id])
		}
		return out
	}
	pairReq := func(mods ...func(*pb.GetCrosswalkRequest)) *connect.Request[pb.GetCrosswalkRequest] {
		m := &pb.GetCrosswalkRequest{
			TenantContext: &pb.TenantContext{TenantId: tenantID},
			Mode:          &pb.GetCrosswalkRequest_Pair{Pair: &pb.CrosswalkPair{CatalogA: catA, CatalogB: catB}},
			Jobs:          selected("j1", "j2", "j3"),
			Catalogs:      []*pb.CatalogSummary{{CatalogId: catA, Name: "Framework A"}},
		}
		for _, mod := range mods {
			mod(m)
		}
		return connect.NewRequest(m)
	}
	get := func(req *connect.Request[pb.GetCrosswalkRequest]) *pb.GetCrosswalkResponse {
		resp, err := svc.GetCrosswalk(ctx, req)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		return resp.Msg
	}
	rowFor := func(resp *pb.GetCrosswalkResponse, src, tgt string) *pb.CrosswalkRow {
		for _, r := range resp.Rows {
			if r.Source.Identifier == src && r.Target.Identifier == tgt {
				return r
			}
		}
		Fail("no row " + src + " -> " + tgt)
		return nil
	}

	// Shared fixture: catalog A has AC-1 (parent of AC-1.a) and AC-2; catalog B
	// has SC-1 and SC-2. j1 and j2 run A to B, j3 runs B to A, "running" is
	// not in the selected set.
	BeforeEach(func() {
		ctx = testspecs.SetupTenantContext(tenantID)
		mem = memdriver.New()
		Expect(mem.CreateGraph(ctx, tenantID)).To(Succeed())
		svc = graph.New(mem, &mockVectorDB{}, nil)
		jobs = map[string]*pb.CrosswalkJob{}

		Expect(testspecs.SeedCrosswalkGraph(ctx, mem, tenantID)).To(Succeed())
		jobs = testspecs.CrosswalkJobs()
	})

	Describe("job mode", func() {
		It("returns only that job's verdicts, read from the job's source to target", func() {
			resp := get(connect.NewRequest(&pb.GetCrosswalkRequest{
				TenantContext: &pb.TenantContext{TenantId: tenantID},
				Mode:          &pb.GetCrosswalkRequest_JobId{JobId: "j3"},
				Jobs:          selected("j3"),
			}))
			Expect(resp.Provenance.Mode).To(Equal("job"))
			Expect(resp.Provenance.JobIds).To(Equal([]string{"j3"}))
			Expect(resp.Rows).To(HaveLen(2))
			row := rowFor(resp, "SC-1", "AC-1.a")
			Expect(row.RelationshipType).To(Equal(pb.RelationshipType_RELATIONSHIP_TYPE_SUPERSET_OF))
			Expect(row.JobsJudged).To(BeEquivalentTo(1))
			Expect(row.LatestJobId).To(Equal("j3"))
		})

		It("rejects a job that is not among the selected jobs with NotFound", func() {
			_, err := svc.GetCrosswalk(ctx, connect.NewRequest(&pb.GetCrosswalkRequest{
				TenantContext: &pb.TenantContext{TenantId: tenantID},
				Mode:          &pb.GetCrosswalkRequest_JobId{JobId: "running"},
				Jobs:          selected("j1"),
			}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
			Expect(err.Error()).To(ContainSubstring(`job "running" not found`))
		})

		It("keeps the stored direction for a job within one catalog", func() {
			addNode("a3", catA, "AC-3")
			addJob("inner", catA, catA, false, t0)
			link("inner", "a2", "a3", results.StageSynthesized, pb.RelationshipType_RELATIONSHIP_TYPE_SUBSET_OF, 0.7)
			resp := get(connect.NewRequest(&pb.GetCrosswalkRequest{
				TenantContext: &pb.TenantContext{TenantId: tenantID},
				Mode:          &pb.GetCrosswalkRequest_JobId{JobId: "inner"},
				Jobs:          selected("inner"),
			}))
			Expect(resp.Rows).To(HaveLen(1))
			Expect(resp.Rows[0].Source.Identifier).To(Equal("AC-2"))
			Expect(resp.Rows[0].Direction).To(Equal("direct"))
		})
	})

	Describe("pair mode", func() {
		It("merges forward and reverse jobs, inverting reversed verdicts", func() {
			resp := get(pairReq())
			Expect(resp.Rows).To(HaveLen(2))

			row := rowFor(resp, "AC-1.a", "SC-1")
			Expect(row.RelationshipType).To(Equal(pb.RelationshipType_RELATIONSHIP_TYPE_SUBSET_OF))
			Expect(row.Relationship).To(Equal("subset of"))
			Expect(row.Confidence).To(BeNumerically("~", 0.9, 1e-6))
			Expect(row.LatestJobId).To(Equal("j3"))
			Expect(row.Direction).To(Equal("direct"))
			Expect(row.JobsJudged).To(BeEquivalentTo(3))
			Expect(row.JobsAgreeing).To(BeEquivalentTo(3))
			Expect(row.Source.NodeId).To(Equal("a1"))
			Expect(row.Target.NodeId).To(Equal("b1"))
		})

		It("reports a reversed verdict without an inverse as reverse and counts disagreement", func() {
			row := rowFor(get(pairReq()), "AC-2", "SC-2")
			Expect(row.Direction).To(Equal("reverse"))
			Expect(row.Relationship).To(Equal("reverse: contributes to"))
			Expect(row.JobsJudged).To(BeEquivalentTo(2))
			Expect(row.JobsAgreeing).To(BeEquivalentTo(1))
			Expect(row.OtherVerdicts).To(Equal("equivalent@0.60 (j1)"))
		})

		It("ignores edges of jobs outside the selected set", func() {
			resp := get(pairReq())
			for _, r := range resp.Rows {
				Expect(r.LatestJobId).NotTo(Equal("running"))
			}
			Expect(resp.Provenance.JobIds).To(Equal([]string{"j1", "j2", "j3"}))
		})

		It("counts a bidirectional job once and lets its more confident direction decide", func() {
			link("bi", "a1", "b1", results.StageSynthesized, pb.RelationshipType_RELATIONSHIP_TYPE_SUBSET_OF, 0.85)
			link("bi", "b1", "a1", results.StageSynthesized, pb.RelationshipType_RELATIONSHIP_TYPE_SUPERSET_OF, 0.95)
			row := rowFor(get(pairReq(func(m *pb.GetCrosswalkRequest) { m.Jobs = selected("j1", "bi") })), "AC-1.a", "SC-1")
			Expect(row.JobsJudged).To(BeEquivalentTo(2))
			Expect(row.LatestJobId).To(Equal("bi"))
			Expect(row.Confidence).To(BeNumerically("~", 0.95, 1e-6))
			Expect(row.OtherVerdicts).To(Equal("subset of@0.85 (bi); subset of@0.70 (j1)"))
		})

		It("shows both verdicts of a bidirectional job whose directions disagree", func() {
			link("bi", "a1", "b1", results.StageSynthesized, pb.RelationshipType_RELATIONSHIP_TYPE_SUBSET_OF, 0.85)
			link("bi", "b1", "a1", results.StageSynthesized, pb.RelationshipType_RELATIONSHIP_TYPE_CONFLICTS_WITH, 0.95)
			row := rowFor(get(pairReq(func(m *pb.GetCrosswalkRequest) { m.Jobs = selected("bi") })), "AC-1.a", "SC-1")
			Expect(row.JobsJudged).To(BeEquivalentTo(1))
			Expect(row.JobsAgreeing).To(BeEquivalentTo(1))
			Expect(row.RelationshipType).To(Equal(pb.RelationshipType_RELATIONSHIP_TYPE_CONFLICTS_WITH))
			Expect(row.OtherVerdicts).To(Equal("subset of@0.85 (bi)"))
		})

		It("fills the parent identifier from PARENT_OF and leaves top-level controls empty", func() {
			resp := get(pairReq())
			Expect(rowFor(resp, "AC-1.a", "SC-1").Source.ParentIdentifier).To(Equal("AC-1"))
			Expect(rowFor(resp, "AC-1.a", "SC-1").Target.ParentIdentifier).To(BeEmpty())
			Expect(rowFor(resp, "AC-2", "SC-2").Source.ParentIdentifier).To(BeEmpty())
		})

		It("names frameworks from the request's catalogs, falling back to the catalog ID", func() {
			row := rowFor(get(pairReq()), "AC-1.a", "SC-1")
			Expect(row.Source.Framework).To(Equal("Framework A"))
			Expect(row.Target.Framework).To(Equal(catB))
			Expect(row.Source.Title).To(Equal("title a1"))
			Expect(row.Source.Statement).To(Equal("statement a1"))
		})

		It("skips a pair whose endpoint control lacks an identifier and counts it", func() {
			addNode("gone", catB, "")
			link("j1", "a2", "gone", results.StageSynthesized, pb.RelationshipType_RELATIONSHIP_TYPE_PARTIAL, 0.4)
			resp := get(pairReq())
			Expect(resp.Rows).To(HaveLen(2))
			Expect(resp.Provenance.SkippedMissingControls).To(BeEquivalentTo(1))
		})

		It("omits a stored NO_RELATIONSHIP edge", func() {
			link("j1", "a2", "b1", results.StageSynthesized, pb.RelationshipType_RELATIONSHIP_TYPE_NO_RELATIONSHIP, 0.9)
			Expect(get(pairReq()).Rows).To(HaveLen(2))
		})

		It("reports the provenance of the walk", func() {
			p := get(pairReq()).Provenance
			Expect(p.Mode).To(Equal("pair"))
			Expect(p.Stage).To(Equal(pb.CrosswalkStage_CROSSWALK_STAGE_SYNTHESIZED))
			Expect(p.Rows).To(BeEquivalentTo(2))
			Expect(p.ServerVersion).NotTo(BeEmpty())
			Expect(p.GeneratedAt).NotTo(BeNil())
			Expect(p.Catalogs).To(HaveLen(1))
		})
	})

	Describe("one_to_many mode", func() {
		const catC = "catalog-c"
		oneReq := func(mods ...func(*pb.GetCrosswalkRequest)) *connect.Request[pb.GetCrosswalkRequest] {
			m := &pb.GetCrosswalkRequest{
				TenantContext: &pb.TenantContext{TenantId: tenantID},
				Mode:          &pb.GetCrosswalkRequest_OneToManyCatalogId{OneToManyCatalogId: catA},
				Jobs:          selected("j1", "j2", "j3", "jc"),
				Catalogs: []*pb.CatalogSummary{
					{CatalogId: catA, Name: "Framework A"},
					{CatalogId: catB, Name: "Zeta B"},
					{CatalogId: catC, Name: "Beta C"},
				},
			}
			for _, mod := range mods {
				mod(m)
			}
			return connect.NewRequest(m)
		}

		BeforeEach(func() {
			addNode("c1", catC, "CC-1")
			addNode("c9", "catalog-unjobbed", "UU-1")
			addJob("jc", catC, catA, false, t0)
			link("jc", "c1", "a2", results.StageSynthesized, pb.RelationshipType_RELATIONSHIP_TYPE_EQUIVALENT, 0.8)
			link("jc", "c9", "a2", results.StageSynthesized, pb.RelationshipType_RELATIONSHIP_TYPE_EQUIVALENT, 0.8)
		})

		It("returns rows for every crosswalked catalog, merged per pair, read from A", func() {
			resp := get(oneReq())
			Expect(resp.Provenance.Mode).To(Equal("one_to_many"))
			Expect(resp.Rows).To(HaveLen(3))
			Expect(resp.Provenance.JobIds).To(Equal([]string{"j1", "j2", "j3", "jc"}))

			row := rowFor(resp, "AC-1.a", "SC-1")
			Expect(row.JobsJudged).To(BeEquivalentTo(3))
			Expect(row.Target.Framework).To(Equal("Zeta B"))
			Expect(row.Source.Framework).To(Equal("Framework A"))

			fromC := rowFor(resp, "AC-2", "CC-1")
			Expect(fromC.Target.Framework).To(Equal("Beta C"))
			Expect(fromC.JobsJudged).To(BeEquivalentTo(1))
		})

		It("leaves out catalogs that have no selected job with A", func() {
			for _, r := range get(oneReq()).Rows {
				Expect(r.Target.Identifier).NotTo(Equal("UU-1"))
			}
		})

		It("sorts by target framework, then source identifier, then descending confidence", func() {
			resp := get(oneReq())
			var order []string
			for _, r := range resp.Rows {
				order = append(order, r.Target.Framework+"/"+r.Source.Identifier)
			}
			Expect(order).To(Equal([]string{"Beta C/AC-2", "Zeta B/AC-1.a", "Zeta B/AC-2"}))
		})

		It("pages across catalogs and resolves parents only for the returned page", func() {
			first := get(oneReq(func(m *pb.GetCrosswalkRequest) { m.Pagination = &pb.Pagination{PageSize: 1} }))
			Expect(first.Rows).To(HaveLen(1))
			Expect(first.PageInfo.TotalCount).To(BeEquivalentTo(3))
			Expect(first.PageInfo.NextPageToken).NotTo(BeEmpty())
			second := get(oneReq(func(m *pb.GetCrosswalkRequest) {
				m.Pagination = &pb.Pagination{PageSize: 1, PageToken: first.PageInfo.NextPageToken}
			}))
			Expect(second.Rows[0].Source.Identifier).To(Equal("AC-1.a"))
			Expect(second.Rows[0].Source.ParentIdentifier).To(Equal("AC-1"))
		})

		It("rejects an empty catalog with an actionable error", func() {
			_, err := svc.GetCrosswalk(ctx, oneReq(func(m *pb.GetCrosswalkRequest) {
				m.Mode = &pb.GetCrosswalkRequest_OneToManyCatalogId{}
			}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(err.Error()).To(ContainSubstring("crosscodex crosswalk <a> --all"))
		})
	})

	Describe("stage", func() {
		BeforeEach(func() {
			link("j1", "a2", "b1", results.StageAnalyzer, pb.RelationshipType_RELATIONSHIP_TYPE_COMPLEMENTS, 0.5)
			link("j2", "a2", "b2", "", pb.RelationshipType_RELATIONSHIP_TYPE_PARTIAL, 0.4)
		})

		It("defaults to synthesized edges only", func() {
			resp := get(pairReq())
			Expect(resp.Rows).To(HaveLen(2))
			Expect(resp.Provenance.Stage).To(Equal(pb.CrosswalkStage_CROSSWALK_STAGE_SYNTHESIZED))
		})

		Describe("viability", func() {
			BeforeEach(func() {
				Expect(mem.CreateEdge(ctx, tenantID, "a2", "b1", graphdb.Edge{
					ID: graphdb.DerivedID("test-edge", "j2", "a2", "b1", "nonviable"), Label: results.SemanticMatchEdgeLabel,
					Properties: map[string]any{
						"relationship_type": "PARTIAL", "job_id": "j2",
						results.StageProperty: results.StageSynthesized, "is_viable": false,
					},
					ValidFrom: testspecs.CrosswalkEpoch, Confidence: 0.3,
				})).To(Succeed())
			})

			It("serves only viable synthesized mappings by default", func() {
				resp := get(pairReq())
				Expect(resp.Rows).To(HaveLen(2))
				for _, r := range resp.Rows {
					Expect(r.Source.Identifier + ">" + r.Target.Identifier).NotTo(Equal("AC-2>SC-1"))
				}
			})

			It("adds the non-viable mappings when asked", func() {
				resp := get(pairReq(func(m *pb.GetCrosswalkRequest) { m.IncludeNonViable = true }))
				Expect(resp.Rows).To(HaveLen(3))
				Expect(rowFor(resp, "AC-2", "SC-1").RelationshipType).To(Equal(pb.RelationshipType_RELATIONSHIP_TYPE_PARTIAL))
			})

			It("leaves the analyzer stage unaffected", func() {
				resp := get(pairReq(func(m *pb.GetCrosswalkRequest) { m.Stage = pb.CrosswalkStage_CROSSWALK_STAGE_ANALYZER }))
				Expect(resp.Rows).To(HaveLen(2))
				Expect(rowFor(resp, "AC-2", "SC-1").RelationshipType).To(Equal(pb.RelationshipType_RELATIONSHIP_TYPE_COMPLEMENTS))
			})
		})

		It("lists analyzer edges, including legacy edges without a stage, and no synthesized ones", func() {
			resp := get(pairReq(func(m *pb.GetCrosswalkRequest) { m.Stage = pb.CrosswalkStage_CROSSWALK_STAGE_ANALYZER }))
			Expect(resp.Provenance.Stage).To(Equal(pb.CrosswalkStage_CROSSWALK_STAGE_ANALYZER))
			Expect(resp.Rows).To(HaveLen(2))
			Expect(rowFor(resp, "AC-2", "SC-1").RelationshipType).To(Equal(pb.RelationshipType_RELATIONSHIP_TYPE_COMPLEMENTS))
			Expect(rowFor(resp, "AC-2", "SC-2").RelationshipType).To(Equal(pb.RelationshipType_RELATIONSHIP_TYPE_PARTIAL))
		})
	})

	Describe("filters", func() {
		DescribeTable("after the merge",
			func(mod func(*pb.GetCrosswalkRequest), want ...string) {
				resp := get(pairReq(mod))
				got := make([]string, len(resp.Rows))
				for i, r := range resp.Rows {
					got[i] = r.Source.Identifier
				}
				Expect(got).To(Equal(want))
			},
			Entry("min_confidence keeps the rows at or above it",
				func(m *pb.GetCrosswalkRequest) { m.MinConfidence = 0.9 }, "AC-1.a"),
			Entry("relationship_types keeps the merged type",
				func(m *pb.GetCrosswalkRequest) {
					m.RelationshipTypes = []pb.RelationshipType{pb.RelationshipType_RELATIONSHIP_TYPE_CONTRIBUTES_TO}
				}, "AC-2"),
			Entry("a type only an older job judged matches nothing",
				func(m *pb.GetCrosswalkRequest) {
					m.RelationshipTypes = []pb.RelationshipType{pb.RelationshipType_RELATIONSHIP_TYPE_EQUIVALENT}
				}),
		)
	})

	Describe("paging and order", func() {
		BeforeEach(func() {
			link("j1", "a1", "b2", results.StageSynthesized, pb.RelationshipType_RELATIONSHIP_TYPE_PARTIAL, 0.95)
		})

		It("sorts by source identifier, descending confidence, then target identifier", func() {
			resp := get(pairReq())
			var order []string
			for _, r := range resp.Rows {
				order = append(order, r.Source.Identifier+">"+r.Target.Identifier)
			}
			Expect(order).To(Equal([]string{"AC-1.a>SC-2", "AC-1.a>SC-1", "AC-2>SC-2"}))
		})

		It("pages stably over the sorted rows", func() {
			first := get(pairReq(func(m *pb.GetCrosswalkRequest) { m.Pagination = &pb.Pagination{PageSize: 2} }))
			Expect(first.Rows).To(HaveLen(2))
			Expect(first.PageInfo.TotalCount).To(BeEquivalentTo(3))
			Expect(first.PageInfo.NextPageToken).NotTo(BeEmpty())

			second := get(pairReq(func(m *pb.GetCrosswalkRequest) {
				m.Pagination = &pb.Pagination{PageSize: 2, PageToken: first.PageInfo.NextPageToken}
			}))
			Expect(second.Rows).To(HaveLen(1))
			Expect(second.Rows[0].Source.Identifier).To(Equal("AC-2"))
			Expect(second.PageInfo.NextPageToken).To(BeEmpty())
		})

		It("looks up parents only for the returned page, once per distinct control", func() {
			counter := &traverseCounter{GraphDB: mem}
			svc = graph.New(counter, &mockVectorDB{}, nil)
			resp := get(pairReq(func(m *pb.GetCrosswalkRequest) { m.Pagination = &pb.Pagination{PageSize: 2} }))
			Expect(resp.Rows).To(HaveLen(2))
			Expect(resp.PageInfo.TotalCount).To(BeEquivalentTo(3))
			// The page holds AC-1.a twice, SC-2 and SC-1; AC-2 is on page two.
			Expect(counter.starts).To(ConsistOf("a1", "b2", "b1"))
		})

		// tokenAt rewrites the offset of a token a previous response issued, keeping
		// the selection digest.
		tokenAt := func(token string, offset string) string {
			raw, err := base64.RawURLEncoding.DecodeString(token)
			Expect(err).NotTo(HaveOccurred())
			_, digest, _ := strings.Cut(string(raw), ":")
			return base64.RawURLEncoding.EncodeToString([]byte(offset + ":" + digest))
		}
		firstToken := func() string {
			return get(pairReq(func(m *pb.GetCrosswalkRequest) { m.Pagination = &pb.Pagination{PageSize: 1} })).PageInfo.NextPageToken
		}

		DescribeTable("a token from the same selection that points outside the rows",
			func(offset string) {
				_, err := svc.GetCrosswalk(ctx, pairReq(func(m *pb.GetCrosswalkRequest) {
					m.Pagination = &pb.Pagination{PageSize: 2, PageToken: tokenAt(firstToken(), offset)}
				}))
				Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
				Expect(err.Error()).To(ContainSubstring("page_token points past the last row"))
			},
			Entry("offset past the last row", "4"),
			Entry("offset that would overflow", "9223372036854775807"),
		)

		It("chains pages while the selection is unchanged", func() {
			var order []string
			token := ""
			for range 3 {
				resp := get(pairReq(func(m *pb.GetCrosswalkRequest) { m.Pagination = &pb.Pagination{PageSize: 1, PageToken: token} }))
				Expect(resp.Rows).To(HaveLen(1))
				order = append(order, resp.Rows[0].Source.Identifier+">"+resp.Rows[0].Target.Identifier)
				token = resp.PageInfo.NextPageToken
			}
			Expect(token).To(BeEmpty())
			Expect(order).To(Equal([]string{"AC-1.a>SC-2", "AC-1.a>SC-1", "AC-2>SC-2"}))
		})

		DescribeTable("a token whose selection no longer matches",
			func(mod func(*pb.GetCrosswalkRequest)) {
				token := firstToken()
				counter := &readCounter{GraphDB: mem}
				svc = graph.New(counter, &mockVectorDB{}, nil)
				_, err := svc.GetCrosswalk(ctx, pairReq(mod, func(m *pb.GetCrosswalkRequest) {
					m.Pagination = &pb.Pagination{PageSize: 1, PageToken: token}
				}))
				Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
				Expect(err).To(MatchError(ContainSubstring("the crosswalk changed while paging (a job completed or was removed): re-run the export")))
				Expect(counter.reads).To(BeZero())
			},
			Entry("a job joined the selection", func(m *pb.GetCrosswalkRequest) { m.Jobs = selected("j1", "j2", "j3", "bi") }),
			Entry("a job left the selection", func(m *pb.GetCrosswalkRequest) { m.Jobs = selected("j1", "j2") }),
			Entry("the stage changed", func(m *pb.GetCrosswalkRequest) { m.Stage = pb.CrosswalkStage_CROSSWALK_STAGE_ANALYZER }),
			Entry("non-viable mappings were added", func(m *pb.GetCrosswalkRequest) { m.IncludeNonViable = true }),
			Entry("a filter changed", func(m *pb.GetCrosswalkRequest) { m.MinConfidence = 0.5 }),
		)

		It("accepts the same jobs listed in another order", func() {
			token := firstToken()
			resp := get(pairReq(func(m *pb.GetCrosswalkRequest) {
				m.Jobs = selected("j3", "j1", "j2")
				m.Pagination = &pb.Pagination{PageSize: 1, PageToken: token}
			}))
			Expect(resp.Rows).To(HaveLen(1))
		})

		DescribeTable("invalid paging",
			func(p *pb.Pagination, message string) {
				query := graphdb.RelationshipQuery{EdgeLabel: results.SemanticMatchEdgeLabel}
				before, err := mem.QueryRelationships(ctx, tenantID, query)
				Expect(err).NotTo(HaveOccurred())
				_, err = svc.GetCrosswalk(ctx, pairReq(func(m *pb.GetCrosswalkRequest) { m.Pagination = p }))
				Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
				Expect(err.Error()).To(ContainSubstring(message))
				after, err := mem.QueryRelationships(ctx, tenantID, query)
				Expect(err).NotTo(HaveOccurred())
				Expect(after).To(Equal(before))
			},
			Entry("token that is not base64", &pb.Pagination{PageToken: "!!"}, "page_token is not valid"),
			Entry("token that is not an offset", &pb.Pagination{PageToken: base64.RawURLEncoding.EncodeToString([]byte("abc:d"))}, "page_token is not valid"),
			Entry("negative offset", &pb.Pagination{PageToken: base64.RawURLEncoding.EncodeToString([]byte("-1:d"))}, "page_token is not valid"),
			Entry("bare offset without a selection digest", &pb.Pagination{PageToken: base64.RawURLEncoding.EncodeToString([]byte("1"))}, "page_token is not valid"),
			Entry("empty selection digest", &pb.Pagination{PageToken: base64.RawURLEncoding.EncodeToString([]byte("1:"))}, "page_token is not valid"),
			Entry("page size over the maximum", &pb.Pagination{PageSize: 1001}, "page_size 1001 is out of range"),
			Entry("negative page size", &pb.Pagination{PageSize: -1}, "page_size -1 is out of range"),
		)
	})

	Describe("transitive mode", func() {
		const hub = testspecs.CrosswalkCatHub
		transitiveReq := func(mods ...func(*pb.GetCrosswalkRequest)) *connect.Request[pb.GetCrosswalkRequest] {
			m := &pb.GetCrosswalkRequest{
				TenantContext: &pb.TenantContext{TenantId: tenantID},
				Mode: &pb.GetCrosswalkRequest_Transitive{Transitive: &pb.CrosswalkTransitive{
					CatalogA: catA, CatalogC: catB, HubCatalogId: hub}},
				Jobs:     selected("j1", "j2", "j3", "hubA", "hubB"),
				Catalogs: []*pb.CatalogSummary{{CatalogId: catA, Name: "Framework A"}},
			}
			for _, mod := range mods {
				mod(m)
			}
			return connect.NewRequest(m)
		}
		derivedVia := func(resp *pb.GetCrosswalkResponse, src, tgt, via string) *pb.CrosswalkRow {
			for _, r := range resp.Rows {
				if r.Direction == "derived" && r.Source.Identifier == src && r.Target.Identifier == tgt && r.HubIdentifier == via {
					return r
				}
			}
			Fail("no derived row " + src + " -> " + tgt + " via " + via)
			return nil
		}

		It("composes equivalent with subset into subset of, naming the hub control", func() {
			resp := get(transitiveReq())
			Expect(resp.Provenance.Mode).To(Equal("transitive"))
			row := derivedVia(resp, "AC-2", "SC-1", "HB-1")
			Expect(row.RelationshipType).To(Equal(pb.RelationshipType_RELATIONSHIP_TYPE_SUBSET_OF))
			Expect(row.Relationship).To(Equal("subset of"))
			Expect(row.Source.Framework).To(Equal("Framework A"))
			Expect(row.OtherVerdicts).To(BeEmpty())
		})

		It("reports subset of superset as related with the type unknown", func() {
			row := derivedVia(get(transitiveReq()), "AC-1.a", "SC-2", "HB-1")
			Expect(row.RelationshipType).To(Equal(pb.RelationshipType_RELATIONSHIP_TYPE_UNSPECIFIED))
			Expect(row.Relationship).To(Equal("related (derived, type unknown)"))
		})

		It("multiplies the leg confidences, sums their job counts and names the newer leg's job", func() {
			row := derivedVia(get(transitiveReq()), "AC-1.a", "SC-1", "HB-1")
			Expect(row.Confidence).To(BeNumerically("~", 0.4, 1e-6))
			Expect(row.JobsJudged).To(BeEquivalentTo(2))
			Expect(row.JobsAgreeing).To(BeEquivalentTo(2))
			Expect(row.LatestJobId).To(Equal("hubB"))
		})

		It("lists direct and derived rows for the same pair, one derived row per hub control", func() {
			resp := get(transitiveReq())
			var got []string
			for _, r := range resp.Rows {
				if r.Source.Identifier == "AC-1.a" && r.Target.Identifier == "SC-1" {
					got = append(got, r.Direction+":"+r.HubIdentifier)
				}
			}
			Expect(got).To(ConsistOf("direct:", "derived:HB-1", "derived:HB-2"))
			Expect(got[0]).To(Equal("direct:"))
			Expect(rowFor(resp, "AC-1.a", "SC-1").Direction).To(Equal("direct"))
		})

		It("keeps a pair's rows together with the direct row first, even when a derived row is more confident", func() {
			addNode("a5", catA, "AC-5")
			addNode("b5", catB, "SC-5")
			addNode("b6", catB, "SC-6")
			addNode("h5", hub, "HB-5")
			syn := results.StageSynthesized
			link("j1", "a5", "b5", syn, pb.RelationshipType_RELATIONSHIP_TYPE_SUBSET_OF, 0.2)
			link("j1", "a5", "b6", syn, pb.RelationshipType_RELATIONSHIP_TYPE_SUBSET_OF, 0.5)
			link("hubA", "a5", "h5", syn, pb.RelationshipType_RELATIONSHIP_TYPE_EQUIVALENT, 0.9)
			link("hubB", "h5", "b5", syn, pb.RelationshipType_RELATIONSHIP_TYPE_EQUIVALENT, 0.9)

			var got []string
			for _, r := range get(transitiveReq()).Rows {
				if r.Source.Identifier == "AC-5" {
					got = append(got, r.Target.Identifier+":"+r.Direction+":"+r.HubIdentifier)
				}
			}
			Expect(got).To(Equal([]string{"SC-5:direct:", "SC-5:derived:HB-5", "SC-6:direct:"}))
		})

		It("never composes a leg whose only verdict is no-relationship", func() {
			addNode("h3", hub, "HB-3")
			link("hubA", "a2", "h3", results.StageSynthesized, pb.RelationshipType_RELATIONSHIP_TYPE_NO_RELATIONSHIP, 0.9)
			link("hubB", "h3", "b2", results.StageSynthesized, pb.RelationshipType_RELATIONSHIP_TYPE_EQUIVALENT, 0.9)
			for _, r := range get(transitiveReq()).Rows {
				Expect(r.HubIdentifier).NotTo(Equal("HB-3"))
			}
		})

		It("treats a leg judged in a direction with no inverse as an unknown type", func() {
			addNode("h4", hub, "HB-4")
			link("hubA", "h4", "a2", results.StageSynthesized, pb.RelationshipType_RELATIONSHIP_TYPE_CONTRIBUTES_TO, 0.8)
			link("hubB", "h4", "b2", results.StageSynthesized, pb.RelationshipType_RELATIONSHIP_TYPE_EQUIVALENT, 0.5)
			row := derivedVia(get(transitiveReq()), "AC-2", "SC-2", "HB-4")
			Expect(row.RelationshipType).To(Equal(pb.RelationshipType_RELATIONSHIP_TYPE_UNSPECIFIED))
			Expect(row.Relationship).To(Equal("related (derived, type unknown)"))
		})

		It("applies the confidence floor to the derived confidence", func() {
			resp := get(transitiveReq(func(m *pb.GetCrosswalkRequest) { m.MinConfidence = 0.42 }))
			for _, r := range resp.Rows {
				Expect(r.Confidence).To(BeNumerically(">=", 0.42))
			}
			Expect(resp.Rows).To(ContainElement(HaveField("HubIdentifier", "HB-1")))
		})

		It("returns only direct rows when no hub verdicts were selected", func() {
			resp := get(transitiveReq(func(m *pb.GetCrosswalkRequest) { m.Jobs = selected("j1", "j2", "j3") }))
			Expect(resp.Rows).To(HaveLen(2))
			for _, r := range resp.Rows {
				Expect(r.HubIdentifier).To(BeEmpty())
				Expect(r.Direction).NotTo(Equal("derived"))
			}
		})

		It("pages derived rows and fills parents for the returned page only", func() {
			resp := get(transitiveReq(func(m *pb.GetCrosswalkRequest) { m.Pagination = &pb.Pagination{PageSize: 2} }))
			Expect(resp.Rows).To(HaveLen(2))
			Expect(resp.PageInfo.TotalCount).To(BeEquivalentTo(7))
			Expect(resp.PageInfo.NextPageToken).NotTo(BeEmpty())
		})
	})

	Describe("rejected requests", func() {
		DescribeTable("return the error and read nothing",
			func(mod func(*pb.GetCrosswalkRequest), code connect.Code, message string) {
				counter := &readCounter{GraphDB: mem}
				svc = graph.New(counter, &mockVectorDB{}, nil)
				_, err := svc.GetCrosswalk(ctx, pairReq(mod))
				Expect(connect.CodeOf(err)).To(Equal(code))
				Expect(err.Error()).To(ContainSubstring(message))
				Expect(counter.reads).To(BeZero())
			},
			Entry("no-relationship type",
				func(m *pb.GetCrosswalkRequest) {
					m.RelationshipTypes = []pb.RelationshipType{pb.RelationshipType_RELATIONSHIP_TYPE_NO_RELATIONSHIP}
				}, connect.CodeInvalidArgument,
				"no-relationship verdicts are not stored: the relationship analyzer omits them, so a crosswalk lists related pairs only"),
			Entry("same catalog twice",
				func(m *pb.GetCrosswalkRequest) {
					m.Mode = &pb.GetCrosswalkRequest_Pair{Pair: &pb.CrosswalkPair{CatalogA: catA, CatalogB: catA}}
				}, connect.CodeInvalidArgument, "pair mode needs two different catalogs"),
			Entry("no mode",
				func(m *pb.GetCrosswalkRequest) { m.Mode = nil }, connect.CodeInvalidArgument, "no crosswalk mode set"),
			Entry("transitive through one of its own catalogs",
				func(m *pb.GetCrosswalkRequest) {
					m.Mode = &pb.GetCrosswalkRequest_Transitive{Transitive: &pb.CrosswalkTransitive{CatalogA: catA, CatalogC: catB, HubCatalogId: catB}}
				},
				connect.CodeInvalidArgument, "transitive mode needs three different catalogs"),
			Entry("a tenant context for another tenant",
				func(m *pb.GetCrosswalkRequest) { m.TenantContext = &pb.TenantContext{TenantId: "other-tenant"} },
				connect.CodePermissionDenied, "tenant mismatch"),
			Entry("no tenant context",
				func(m *pb.GetCrosswalkRequest) { m.TenantContext = nil }, connect.CodeInvalidArgument, "tenant_context is required"),
		)

		It("leaves the graph unchanged after a tenant mismatch", func() {
			before, err := mem.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{EdgeLabel: results.SemanticMatchEdgeLabel})
			Expect(err).NotTo(HaveOccurred())
			_, err = svc.GetCrosswalk(ctx, pairReq(func(m *pb.GetCrosswalkRequest) {
				m.TenantContext = &pb.TenantContext{TenantId: "other-tenant"}
			}))
			Expect(err).To(HaveOccurred())
			after, err := mem.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{EdgeLabel: results.SemanticMatchEdgeLabel})
			Expect(err).NotTo(HaveOccurred())
			Expect(after).To(HaveLen(len(before)))
		})

		It("does not see another tenant's graph", func() {
			Expect(mem.CreateGraph(ctx, "other-tenant")).To(Succeed())
			otherCtx := testspecs.SetupTenantContext("other-tenant")
			resp, err := svc.GetCrosswalk(otherCtx, pairReq(func(m *pb.GetCrosswalkRequest) {
				m.TenantContext = &pb.TenantContext{TenantId: "other-tenant"}
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.Rows).To(BeEmpty())
		})
	})
})
