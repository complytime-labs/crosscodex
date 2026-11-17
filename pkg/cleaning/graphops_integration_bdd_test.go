//go:build integration

package cleaning_test

import (
	"context"
	"database/sql"
	"maps"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
	"github.com/complytime-labs/crosscodex/pkg/cleaning"
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/graphdb/agedriver"
)

// The graph operations against Apache AGE through agedriver, connected as
// the restricted graph_user role the daemon's graph role uses. The unit
// specs run them on memdriver; these prove the AGE property filter in
// liveSuccessor and the copy of an edge's properties after the agtype round
// trip.
var _ = Describe("Graph retire and restore against AGE", Ordered, func() {
	var (
		graphDB *sql.DB
		ctx     context.Context
		g       graphdb.GraphDB
		tenant  string

		validFrom = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		retireAt  = time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
		restoreAt = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	)

	BeforeAll(func() {
		suDSN := os.Getenv("TEST_DATABASE_DSN")
		password, err := dbtest.NewRolePassword()
		Expect(err).NotTo(HaveOccurred(), "graph_user password")
		Expect(dbtest.SetRolePassword(context.Background(), suDSN, "graph_user", password)).To(Succeed(), "set graph_user password")
		dsn, err := dbtest.RoleDSN(suDSN, "graph_user", password)
		Expect(err).NotTo(HaveOccurred(), "graph_user DSN")
		graphDB, err = sql.Open("pgx", dsn)
		Expect(err).NotTo(HaveOccurred(), "graph_user connection")
		DeferCleanup(graphDB.Close)
	})

	// A fresh tenant per spec holds controls a1 (cat-a) and b1 (cat-b) and
	// one analyzer SEMANTIC_MATCH edge e1 between them with every
	// caller-owned field set, including properties of several agtype kinds.
	BeforeEach(func() {
		ctx = context.Background()
		tenant = dbtest.TestID("cleaning-age")
		createTenants(tenant)
		var err error
		g, err = agedriver.New(graphDB)
		Expect(err).NotTo(HaveOccurred())
		Expect(g.CreateGraph(ctx, tenant)).To(Succeed())
		for _, n := range []struct{ id, catalog string }{{"a1", "cat-a"}, {"b1", "cat-b"}} {
			Expect(g.CreateNode(ctx, tenant, graphdb.Node{
				ID: n.id, Label: "Control", ValidFrom: validFrom,
				Properties: map[string]any{"catalog_id": n.catalog, "identifier": n.id},
			})).To(Succeed())
		}
		Expect(g.CreateEdge(ctx, tenant, "a1", "b1", graphdb.Edge{
			ID: "e1", Label: results.SemanticMatchEdgeLabel, ValidFrom: validFrom,
			DeterminedBy: "model-x", DeterminationType: "llm", Confidence: 0.75,
			Properties: map[string]any{
				"job_id": "j1", "relationship_type": "SUBSET_OF", "stage": results.StageAnalyzer,
				"viable": true, "votes": int64(3), "agreement": 0.6,
			},
		})).To(Succeed())
	})

	liveEdges := func() []graphdb.Relationship {
		rels, err := g.QueryRelationships(ctx, tenant, graphdb.RelationshipQuery{EdgeLabel: results.SemanticMatchEdgeLabel})
		Expect(err).NotTo(HaveOccurred())
		return rels
	}
	retire := func(ids []string, at time.Time) {
		got, err := cleaning.RetireEdges(ctx, g, tenant, ids, at)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(ids))
	}

	It("retires a live edge at the given time", func() {
		retire([]string{"e1"}, retireAt)
		e, err := g.GetEdge(ctx, tenant, "e1")
		Expect(err).NotTo(HaveOccurred())
		Expect(e.ValidTo).To(HaveValue(BeTemporally("==", retireAt)))
		Expect(liveEdges()).To(BeEmpty())
	})

	It("restores a retired edge with the original's fields and properties after the agtype round trip", func() {
		retire([]string{"e1"}, retireAt)
		ids, err := cleaning.RestoreEdges(ctx, g, tenant, []string{"e1"}, "dec-1", restoreAt)
		Expect(err).NotTo(HaveOccurred())
		Expect(ids).To(Equal([]string{graphdb.DerivedID("semantic-match-restored", "e1", "dec-1")}))

		orig, err := g.GetEdge(ctx, tenant, "e1")
		Expect(err).NotTo(HaveOccurred())
		got, err := g.GetEdge(ctx, tenant, ids[0])
		Expect(err).NotTo(HaveOccurred())
		Expect(got.SourceID).To(Equal(orig.SourceID))
		Expect(got.TargetID).To(Equal(orig.TargetID))
		Expect(got.Label).To(Equal(orig.Label))
		Expect(got.DeterminedBy).To(Equal(orig.DeterminedBy))
		Expect(got.DeterminationType).To(Equal(orig.DeterminationType))
		Expect(got.Confidence).To(Equal(orig.Confidence))
		// AGE returns the driver-written keys (id, valid_from, ...) in
		// Properties too; those are compared as fields above.
		caller := func(props map[string]any) map[string]any {
			out := maps.Clone(props)
			for k := range graphdb.ReservedEdgeKeys() {
				delete(out, k)
			}
			return out
		}
		Expect(caller(got.Properties)).To(Equal(caller(orig.Properties)))
		Expect(caller(got.Properties)).To(HaveLen(6))
		Expect(got.Supersedes).To(Equal("e1"))
		Expect(got.ValidFrom).To(BeTemporally("==", restoreAt))
		Expect(got.ValidTo).To(BeNil())

		live := liveEdges()
		Expect(live).To(HaveLen(1))
		Expect(live[0].Edge.ID).To(Equal(ids[0]))
	})

	It("keeps one live successor when a shared verdict is restored again under a second decision", func() {
		retire([]string{"e1"}, retireAt)
		first, err := cleaning.RestoreEdges(ctx, g, tenant, []string{"e1"}, "dec-1", restoreAt)
		Expect(err).NotTo(HaveOccurred())

		second, err := cleaning.RestoreEdges(ctx, g, tenant, []string{"e1"}, "dec-2", restoreAt.Add(time.Hour))
		Expect(err).NotTo(HaveOccurred())
		Expect(second).To(Equal(first))
		Expect(liveEdges()).To(HaveLen(1))
		_, err = g.GetEdge(ctx, tenant, graphdb.DerivedID("semantic-match-restored", "e1", "dec-2"))
		Expect(err).To(MatchError(graphdb.ErrEdgeNotFound))
	})

	It("retires a restored edge, leaving no live edge and the restore queryable by time", func() {
		retire([]string{"e1"}, retireAt)
		ids, err := cleaning.RestoreEdges(ctx, g, tenant, []string{"e1"}, "dec-1", restoreAt)
		Expect(err).NotTo(HaveOccurred())

		again := restoreAt.Add(24 * time.Hour)
		retire(ids, again)
		Expect(liveEdges()).To(BeEmpty())
		e, err := g.GetEdge(ctx, tenant, ids[0])
		Expect(err).NotTo(HaveOccurred())
		Expect(e.ValidTo).To(HaveValue(BeTemporally("==", again)))
		between, err := g.QueryAsOf(ctx, tenant, graphdb.RelationshipQuery{EdgeLabel: results.SemanticMatchEdgeLabel}, restoreAt.Add(time.Hour))
		Expect(err).NotTo(HaveOccurred())
		Expect(between).To(HaveLen(1))
		Expect(between[0].Edge.ID).To(Equal(ids[0]))
	})

	It("refuses a superseded edge in CheckLive, naming it", func() {
		Expect(cleaning.CheckLive(ctx, g, tenant, []string{"e1"})).To(Succeed())
		retire([]string{"e1"}, retireAt)

		err := cleaning.CheckLive(ctx, g, tenant, []string{"e1"})
		Expect(err).To(MatchError(cleaning.ErrEdgeNotLive))
		Expect(err).To(MatchError(ContainSubstring(`edge "e1"`)))
		Expect(err).To(MatchError(ContainSubstring("superseded at 2026-02-01T00:00:00Z")))
	})
})
