//go:build integration

package graph_test

// Suite bootstrap lives in resolver_bdd_test.go — do NOT add RunSpecs here.

import (
	"context"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/graph"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/graphdb/agedriver"
	"github.com/complytime-labs/crosscodex/pkg/graphdb/memdriver"
)

// The crosswalk walk relies on endpoint-property filtering and ordering that
// each driver implements itself, so the agedriver must return exactly the rows
// the memdriver does for one fixture.
var _ = Describe("GetCrosswalk on Apache AGE", Ordered, func() {
	const tenantID = "crosswalk-int-test"
	var (
		ctx     context.Context
		memSvc  *graph.Service
		ageSvc  *graph.Service
		cleanup func()
	)

	// selected is evaluated while the table is built, so it reads a fresh
	// fixture rather than state set up in BeforeAll.
	selected := func(ids ...string) []*pb.CrosswalkJob {
		all := testspecs.CrosswalkJobs()
		out := make([]*pb.CrosswalkJob, 0, len(ids))
		for _, id := range ids {
			out = append(out, all[id])
		}
		return out
	}
	pair := func(stage pb.CrosswalkStage, ids ...string) *pb.GetCrosswalkRequest {
		return &pb.GetCrosswalkRequest{
			TenantContext: &pb.TenantContext{TenantId: tenantID},
			Mode: &pb.GetCrosswalkRequest_Pair{Pair: &pb.CrosswalkPair{
				CatalogA: testspecs.CrosswalkCatA, CatalogB: testspecs.CrosswalkCatB}},
			Stage:    stage,
			Jobs:     selected(ids...),
			Catalogs: []*pb.CatalogSummary{{CatalogId: testspecs.CrosswalkCatA, Name: "Framework A"}},
		}
	}

	BeforeAll(func() {
		db, dbCleanup := testspecs.SetupTestDatabase()
		cleanup = dbCleanup
		age, err := agedriver.New(db)
		Expect(err).NotTo(HaveOccurred(), "create agedriver")
		mem := memdriver.New()

		ctx = testspecs.SetupTenantContext(tenantID)
		for _, g := range []graphdb.GraphDB{mem, age} {
			Expect(g.CreateGraph(context.Background(), tenantID)).To(Succeed())
			Expect(testspecs.SeedCrosswalkGraph(ctx, g, tenantID)).To(Succeed())
			// Analyzer-stage and stage-less (legacy) edges beside the synthesized ones.
			Expect(testspecs.LinkCrosswalk(ctx, g, tenantID, "j1", "a2", "b1", results.StageAnalyzer,
				pb.RelationshipType_RELATIONSHIP_TYPE_COMPLEMENTS, 0.5)).To(Succeed())
			Expect(testspecs.LinkCrosswalk(ctx, g, tenantID, "j2", "a2", "b2", "",
				pb.RelationshipType_RELATIONSHIP_TYPE_PARTIAL, 0.4)).To(Succeed())
		}
		memSvc = graph.New(mem, nil, nil)
		ageSvc = graph.New(age, nil, nil)
	})

	AfterAll(func() {
		if cleanup != nil {
			cleanup()
		}
	})

	DescribeTable("returns the memdriver's rows",
		func(msg *pb.GetCrosswalkRequest) {
			want, err := memSvc.GetCrosswalk(ctx, connect.NewRequest(proto.Clone(msg).(*pb.GetCrosswalkRequest)))
			Expect(err).NotTo(HaveOccurred())
			got, err := ageSvc.GetCrosswalk(ctx, connect.NewRequest(proto.Clone(msg).(*pb.GetCrosswalkRequest)))
			Expect(err).NotTo(HaveOccurred())

			Expect(want.Msg.Rows).NotTo(BeEmpty(), "fixture must yield rows or the parity check proves nothing")
			Expect(got.Msg.Rows).To(HaveLen(len(want.Msg.Rows)))
			for i := range want.Msg.Rows {
				Expect(proto.Equal(got.Msg.Rows[i], want.Msg.Rows[i])).To(BeTrue(),
					"row %d differs on AGE:\n got  %v\n want %v", i, got.Msg.Rows[i], want.Msg.Rows[i])
			}
			Expect(got.Msg.Provenance.JobIds).To(Equal(want.Msg.Provenance.JobIds))
		},
		Entry("pair over forward and reversed jobs, synthesized", pair(pb.CrosswalkStage_CROSSWALK_STAGE_SYNTHESIZED, "j1", "j2", "j3")),
		Entry("pair, analyzer stage", pair(pb.CrosswalkStage_CROSSWALK_STAGE_ANALYZER, "j1", "j2", "j3")),
		Entry("transitive through the hub catalog", &pb.GetCrosswalkRequest{
			TenantContext: &pb.TenantContext{TenantId: tenantID},
			Mode: &pb.GetCrosswalkRequest_Transitive{Transitive: &pb.CrosswalkTransitive{
				CatalogA: testspecs.CrosswalkCatA, CatalogC: testspecs.CrosswalkCatB, HubCatalogId: testspecs.CrosswalkCatHub}},
			Stage:    pb.CrosswalkStage_CROSSWALK_STAGE_SYNTHESIZED,
			Jobs:     selected("j1", "j2", "j3", "hubA", "hubB"),
			Catalogs: []*pb.CatalogSummary{{CatalogId: testspecs.CrosswalkCatA, Name: "Framework A"}},
		}),
		Entry("single reversed job", &pb.GetCrosswalkRequest{
			TenantContext: &pb.TenantContext{TenantId: tenantID},
			Mode:          &pb.GetCrosswalkRequest_JobId{JobId: "j3"},
			Jobs:          selected("j3"),
		}),
	)
})
