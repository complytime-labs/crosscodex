//go:build integration

// Suite bootstrap lives in graphdb_integration_bdd_test.go — do NOT add RunSpecs here.

package agedriver_test

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/graphdb/agedriver"
)

// sqlRecorder is a pgx QueryTracer that keeps the SQL of every statement
// containing contains, so specs can assert the exact Cypher the driver
// generated.
type sqlRecorder struct {
	contains string
	mu       sync.Mutex
	sqls     []string
}

func (r *sqlRecorder) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, r.contains) {
		r.mu.Lock()
		r.sqls = append(r.sqls, data.SQL)
		r.mu.Unlock()
	}
	return ctx
}

func (r *sqlRecorder) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (r *sqlRecorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	got := r.sqls
	r.sqls = nil
	return got
}

var _ = Describe("Relationship query property filters", func() {
	// Go randomizes map iteration, so unsorted keys change order across
	// calls; 20 calls with 3 keys make an accidental pass vanishingly rare.
	const calls = 20

	var (
		client   graphdb.GraphDB
		recorder *sqlRecorder
		tenantID string
		ctx      context.Context
		now      time.Time
		filter   map[string]any
	)

	BeforeEach(func() {
		tenantID = testID("query-order")
		setupTenant(tenantID)
		DeferCleanup(func() { cleanupTenant(tenantID) })

		recorder = &sqlRecorder{contains: "RETURN s, e, t"}
		var err error
		client, err = agedriver.New(openTracedGraphDB(recorder))
		Expect(err).NotTo(HaveOccurred())
		ctx = context.Background()
		now = time.Now().UTC().Truncate(time.Microsecond)

		Expect(client.CreateGraph(ctx, tenantID)).To(Succeed())
		Expect(client.CreateNode(ctx, tenantID, graphdb.Node{ID: "a", Label: "Control", ValidFrom: now})).To(Succeed())
		Expect(client.CreateNode(ctx, tenantID, graphdb.Node{ID: "b", Label: "Control", ValidFrom: now})).To(Succeed())
		filter = map[string]any{"zeta": "z", "alpha": "a", "mid": "m"}
		Expect(client.CreateEdge(ctx, tenantID, "a", "b", graphdb.Edge{
			ID: "e1", Label: "MAPS", ValidFrom: now, Properties: filter,
		})).To(Succeed())
	})

	DescribeTable("generates the same WHERE text on every call, with property conditions in sorted key order",
		func(query func() ([]graphdb.Relationship, error)) {
			for range calls {
				rels, err := query()
				Expect(err).NotTo(HaveOccurred())
				Expect(rels).To(HaveLen(1), "the filter must still match the edge")
				Expect(rels[0].Edge.ID).To(Equal("e1"))
			}
			sqls := recorder.take()
			Expect(sqls).To(HaveLen(calls))
			Expect(sqls[0]).To(ContainSubstring("e.alpha = 'a' AND e.mid = 'm' AND e.zeta = 'z'"))
			for i, s := range sqls[1:] {
				Expect(s).To(Equal(sqls[0]), "call %d generated different SQL from call 0", i+1)
			}
		},
		Entry("QueryRelationships", func() ([]graphdb.Relationship, error) {
			return client.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{Properties: filter})
		}),
		Entry("QueryAsOf", func() ([]graphdb.Relationship, error) {
			return client.QueryAsOf(ctx, tenantID, graphdb.RelationshipQuery{Properties: filter}, now.Add(time.Second))
		}),
	)
})
