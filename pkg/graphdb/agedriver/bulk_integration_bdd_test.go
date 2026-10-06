//go:build integration

// Suite bootstrap lives in graphdb_integration_bdd_test.go — do NOT add RunSpecs here.

package agedriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/graphdb/agedriver"
)

// cypherCounter is a pgx QueryTracer that counts statements calling
// ag_catalog.cypher, so specs can assert how many graph queries a driver
// method issues without depending on its SQL text.
type cypherCounter struct{ n atomic.Int64 }

func (c *cypherCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "ag_catalog.cypher(") {
		c.n.Add(1)
	}
	return ctx
}

func (c *cypherCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// openTracedGraphDB opens a graph_user pool of its own whose statements pass
// through tracer, so a tracer never observes another spec's queries.
func openTracedGraphDB(tracer pgx.QueryTracer) *sql.DB {
	cfg, err := pgx.ParseConfig(graphDSN)
	Expect(err).NotTo(HaveOccurred())
	cfg.Tracer = tracer
	conn := stdlib.OpenDB(*cfg)
	DeferCleanup(conn.Close)
	return conn
}

// endpointMutator is a pgx QueryTracer that, once armed, runs a cypher
// statement as the superuser just before the first bulk CREATE ("UNWIND [")
// starts. checkBulkEdgeWrites has passed by then, so the statement plays a
// concurrent writer that changes an endpoint inside the check-then-create
// window. The superuser statement autocommits, and the driver's
// READ COMMITTED transaction sees it in its next statement.
type endpointMutator struct {
	su    *sql.DB
	graph string
	mu    sync.Mutex
	// cypher is the armed statement; empty once it has run.
	cypher string
	// ran reports whether the statement ran; err holds a failure to run it.
	// The spec asserts both, because a failed assertion inside a pgx
	// callback would not stop the spec cleanly.
	ran bool
	err error
}

func (m *endpointMutator) arm(cypher string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cypher = cypher
}

func (m *endpointMutator) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cypher == "" || !strings.Contains(data.SQL, "UNWIND [") {
		return ctx
	}
	cypher := m.cypher
	m.cypher = ""
	// Holding mu across exec cannot deadlock: exec uses a separate superuser
	// pool, and the driver issues its statements one at a time.
	m.err = m.exec(cypher)
	m.ran = m.err == nil
	return ctx
}

func (m *endpointMutator) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// exec runs cypher against the tenant graph on one superuser session, which
// LOAD 'age' and the search_path apply to.
func (m *endpointMutator) exec(cypher string) error {
	ctx := context.Background()
	conn, err := m.su.Conn(ctx)
	if err != nil {
		return fmt.Errorf("superuser conn: %w", err)
	}
	defer conn.Close()
	for _, stmt := range []string{"LOAD 'age'", `SET search_path = ag_catalog, "$user", public`} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("%s: %w", stmt, err)
		}
	}
	q := fmt.Sprintf("SELECT * FROM ag_catalog.cypher('%s', $$ %s $$) AS (v agtype)", m.graph, cypher)
	if _, err := conn.ExecContext(ctx, q); err != nil {
		return fmt.Errorf("mutate endpoint: %w", err)
	}
	return nil
}

// result returns whether the armed statement ran and any error running it.
func (m *endpointMutator) result() (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ran, m.err
}

// endpoints is the source and target node ID of one stored edge.
type endpoints struct{ source, target string }

var _ = Describe("BulkCreateEdges (agedriver batched checks)", func() {
	var (
		client   graphdb.GraphDB
		counter  *cypherCounter
		tenantID string
		ctx      context.Context
		now      time.Time
	)

	edge := func(src, tgt, id string) graphdb.BulkEdge {
		return graphdb.BulkEdge{SourceID: src, TargetID: tgt, Edge: graphdb.Edge{ID: id, Label: "MAPS", ValidFrom: now}}
	}

	allEdgeEndpoints := func() []endpoints {
		rels, err := client.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{})
		Expect(err).NotTo(HaveOccurred())
		pairs := make([]endpoints, 0, len(rels))
		for _, r := range rels {
			pairs = append(pairs, endpoints{r.Source.ID, r.Target.ID})
		}
		return pairs
	}

	allEdgeIDs := func() []string {
		rels, err := client.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{})
		Expect(err).NotTo(HaveOccurred())
		ids := make([]string, 0, len(rels))
		for _, r := range rels {
			ids = append(ids, r.Edge.ID)
		}
		return ids
	}

	BeforeEach(func() {
		tenantID = testID("bulk-edges")
		setupTenant(tenantID)
		DeferCleanup(func() { cleanupTenant(tenantID) })

		counter = &cypherCounter{}
		var err error
		client, err = agedriver.New(openTracedGraphDB(counter))
		Expect(err).NotTo(HaveOccurred())
		ctx = context.Background()
		now = time.Now().UTC().Truncate(time.Microsecond)

		Expect(client.CreateGraph(ctx, tenantID)).To(Succeed())
		for _, n := range []graphdb.Node{
			{ID: "a", Label: "Control", ValidFrom: now},
			{ID: "b", Label: "Control", ValidFrom: now},
			{ID: "c", Label: "Control", ValidFrom: now},
			{ID: "dup", Label: "Control", ValidFrom: now},
			{ID: `q'x\y"z`, Label: "Control", ValidFrom: now},
			{ID: "x" + agedriver.ExportCypherDollarTag + "y", Label: "Control", ValidFrom: now},
		} {
			Expect(client.CreateNode(ctx, tenantID, n)).To(Succeed())
		}
		seedLegacyNode(ctx, agedriver.GraphName(tenantID), "Artifact", "dup")
		Expect(client.CreateEdge(ctx, tenantID, "a", "b", graphdb.Edge{ID: "e0", Label: "MAPS", ValidFrom: now})).To(Succeed())
	})

	DescribeTable("rejects the whole batch with the offending index and persists nothing",
		func(batch func() []graphdb.BulkEdge, sentinel error, wantMsg string) {
			ids, err := client.BulkCreateEdges(ctx, tenantID, batch())
			Expect(err).To(HaveOccurred())
			if sentinel != nil {
				Expect(err).To(MatchError(sentinel))
			}
			Expect(err.Error()).To(Equal(wantMsg))
			Expect(ids).To(BeNil())
			Expect(allEdgeIDs()).To(ConsistOf("e0"))
		},
		Entry("a missing target endpoint",
			func() []graphdb.BulkEdge {
				return []graphdb.BulkEdge{edge("a", "c", "e1"), edge("b", "zz", "e2"), edge("c", "a", "e3")}
			},
			graphdb.ErrNodeNotFound, `bulk create edges [1]: target: node "zz": node not found`),
		Entry("a missing source endpoint",
			func() []graphdb.BulkEdge { return []graphdb.BulkEdge{edge("a", "c", "e1"), edge("zz", "b", "e2")} },
			graphdb.ErrNodeNotFound, `bulk create edges [1]: source: node "zz": node not found`),
		Entry("an ambiguous endpoint",
			func() []graphdb.BulkEdge {
				return []graphdb.BulkEdge{edge("a", "c", "e1"), edge("b", "c", "e2"), edge("dup", "a", "e3")}
			},
			nil, `bulk create edges [2]: source: ambiguous node ID "dup": 2 nodes share it. Node IDs have been unique per graph since #148, so this graph was written before then; rebuild the tenant's graph (see "Upgrade note (#148)" in docs/dev/design-principles.md)`),
		Entry("an edge ID that already exists in the graph",
			func() []graphdb.BulkEdge { return []graphdb.BulkEdge{edge("a", "c", "e1"), edge("b", "c", "e0")} },
			graphdb.ErrEdgeExists, `bulk create edges [1]: edge "e0": edge already exists`),
		Entry("an edge ID repeated within the batch",
			func() []graphdb.BulkEdge {
				return []graphdb.BulkEdge{edge("a", "c", "e1"), edge("b", "c", "e2"), edge("c", "a", "e1")}
			},
			graphdb.ErrEdgeExists, `bulk create edges [2]: edge "e1": edge already exists`),
		Entry("an edge ID that matches an existing one once the dollar-quote tag is stripped",
			func() []graphdb.BulkEdge {
				return []graphdb.BulkEdge{edge("a", "c", "e1"), edge("b", "c", "e"+agedriver.ExportCypherDollarTag+"0")}
			},
			graphdb.ErrEdgeExists, `bulk create edges [1]: edge "e$cypher$0": edge already exists`),
		Entry("edge IDs within the batch that match once the dollar-quote tag is stripped",
			func() []graphdb.BulkEdge {
				return []graphdb.BulkEdge{edge("a", "c", "e1"), edge("b", "c", "e"+agedriver.ExportCypherDollarTag+"1")}
			},
			graphdb.ErrEdgeExists, `bulk create edges [1]: edge "e$cypher$1": edge already exists`),
		Entry("the first failure when several edges fail",
			func() []graphdb.BulkEdge {
				return []graphdb.BulkEdge{edge("a", "c", "e1"), edge("a", "b", "e0"), edge("zz", "b", "e2")}
			},
			graphdb.ErrEdgeExists, `bulk create edges [1]: edge "e0": edge already exists`),
	)

	It("allows repeated empty edge IDs and endpoints whose IDs need escaping", func() {
		ids, err := client.BulkCreateEdges(ctx, tenantID, []graphdb.BulkEdge{
			edge("a", `q'x\y"z`, ""), edge(`q'x\y"z`, "c", ""),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(ids).To(Equal([]string{"", ""}))
		Expect(allEdgeEndpoints()).To(ConsistOf(
			endpoints{"a", "b"}, endpoints{"a", `q'x\y"z`}, endpoints{`q'x\y"z`, "c"},
		))
	})

	It("resolves an endpoint ID containing the dollar-quote tag the way CreateEdge does", func() {
		tagged := "x" + agedriver.ExportCypherDollarTag + "y"
		Expect(client.CreateEdge(ctx, tenantID, tagged, "a", graphdb.Edge{ID: "single", Label: "MAPS", ValidFrom: now})).To(Succeed())
		ids, err := client.BulkCreateEdges(ctx, tenantID, []graphdb.BulkEdge{edge(tagged, "a", "t1"), edge("b", tagged, "t2")})
		Expect(err).NotTo(HaveOccurred())
		Expect(ids).To(Equal([]string{"t1", "t2"}))
		Expect(allEdgeIDs()).To(ConsistOf("e0", "single", "t1", "t2"))
	})

	It("resolves two endpoint spellings that match once the dollar-quote tag is stripped to the one stored node", func() {
		// The fixture's "x$cypher$y" node is stored as "xy". A batch naming it
		// both ways must resolve both to that node, not report one missing or
		// count it twice and call it ambiguous.
		tagged := "x" + agedriver.ExportCypherDollarTag + "y"
		ids, err := client.BulkCreateEdges(ctx, tenantID, []graphdb.BulkEdge{edge(tagged, "a", "t1"), edge("xy", "b", "t2")})
		Expect(err).NotTo(HaveOccurred())
		Expect(ids).To(Equal([]string{"t1", "t2"}))
		Expect(allEdgeEndpoints()).To(ConsistOf(
			endpoints{"a", "b"}, endpoints{"xy", "a"}, endpoints{"xy", "b"},
		))
	})

	It("writes each edge's own properties and leaves absent ones unset across a mixed-label batch", func() {
		validTo := now.Add(time.Hour)
		_, err := client.BulkCreateEdges(ctx, tenantID, []graphdb.BulkEdge{
			{SourceID: "a", TargetID: "c", Edge: graphdb.Edge{ID: "m1", Label: "MAPS", ValidFrom: now, Confidence: 0.75, Properties: map[string]any{"kind": "strong", "count": 3}}},
			{SourceID: "b", TargetID: "c", Edge: graphdb.Edge{ID: "d1", Label: "DEMANDS", ValidFrom: now, ValidTo: &validTo}},
			{SourceID: "c", TargetID: "a", Edge: graphdb.Edge{ID: "m2", Label: "MAPS", ValidFrom: now, Properties: map[string]any{"end": true}}},
		})
		Expect(err).NotTo(HaveOccurred())

		m1, err := client.GetEdge(ctx, tenantID, "m1")
		Expect(err).NotTo(HaveOccurred())
		Expect(m1.SourceID).To(Equal("a"))
		Expect(m1.TargetID).To(Equal("c"))
		Expect(m1.Edge.Label).To(Equal("MAPS"))
		Expect(m1.Edge.Confidence).To(Equal(0.75))
		Expect(m1.Edge.Properties).To(HaveKeyWithValue("kind", "strong"))
		Expect(m1.Edge.Properties).To(HaveKeyWithValue("count", BeNumerically("==", 3)))
		Expect(m1.Edge.Properties).NotTo(HaveKey("end"))

		m2, err := client.GetEdge(ctx, tenantID, "m2")
		Expect(err).NotTo(HaveOccurred())
		Expect(m2.Edge.Properties).To(HaveKeyWithValue("end", true))
		Expect(m2.Edge.Properties).NotTo(HaveKey("kind"))
		Expect(m2.Edge.Confidence).To(BeZero())

		d1, err := client.GetEdge(ctx, tenantID, "d1")
		Expect(err).NotTo(HaveOccurred())
		Expect(d1.Edge.Label).To(Equal("DEMANDS"))
		Expect(d1.Edge.ValidTo).NotTo(BeNil())
		Expect(d1.Edge.ValidTo.Equal(validTo)).To(BeTrue())
	})

	DescribeTable("issues a number of cypher statements that does not grow with the batch size",
		func(n int) {
			batch := make([]graphdb.BulkEdge, n)
			for i := range batch {
				batch[i] = edge("a", "c", fmt.Sprintf("n%d", i))
			}
			before := counter.n.Load()
			ids, err := client.BulkCreateEdges(ctx, tenantID, batch)
			Expect(err).NotTo(HaveOccurred())
			Expect(ids).To(HaveLen(n))
			// One endpoint lookup, one edge-ID lookup, one CREATE per label.
			Expect(counter.n.Load() - before).To(Equal(int64(3)))
			Expect(allEdgeIDs()).To(HaveLen(n + 1))
		},
		Entry("1 edge", 1),
		Entry("10 edges", 10),
		Entry("200 edges", 200),
	)

	It("issues one CREATE per distinct edge label", func() {
		before := counter.n.Load()
		_, err := client.BulkCreateEdges(ctx, tenantID, []graphdb.BulkEdge{
			edge("a", "c", "l1"),
			{SourceID: "b", TargetID: "c", Edge: graphdb.Edge{ID: "l2", Label: "DEMANDS", ValidFrom: now}},
			edge("c", "a", "l3"),
		})
		Expect(err).NotTo(HaveOccurred())
		// Endpoint lookup, edge-ID lookup, and a CREATE for each of MAPS and DEMANDS.
		Expect(counter.n.Load() - before).To(Equal(int64(4)))
	})

	It("skips the edge-ID lookup when no edge in the batch has an ID", func() {
		before := counter.n.Load()
		ids, err := client.BulkCreateEdges(ctx, tenantID, []graphdb.BulkEdge{edge("a", "c", ""), edge("b", "c", ""), edge("c", "a", "")})
		Expect(err).NotTo(HaveOccurred())
		Expect(ids).To(Equal([]string{"", "", ""}))
		// Endpoint lookup and one CREATE for MAPS.
		Expect(counter.n.Load() - before).To(Equal(int64(2)))
		Expect(allEdgeIDs()).To(HaveLen(4))
	})

	Context("when a concurrent writer changes an endpoint after the endpoint check", func() {
		var mutator *endpointMutator

		BeforeEach(func() {
			su, err := sql.Open("pgx", suDSN)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(su.Close)
			mutator = &endpointMutator{su: su, graph: agedriver.GraphName(tenantID)}
			client, err = agedriver.New(openTracedGraphDB(mutator))
			Expect(err).NotTo(HaveOccurred())
		})

		DescribeTable("rejects the batch because the CREATE count differs from the batch, and writes nothing",
			func(mutation, wantCount string) {
				before := allEdgeEndpoints()
				mutator.arm(mutation)

				ids, err := client.BulkCreateEdges(ctx, tenantID, []graphdb.BulkEdge{edge("a", "c", "e1"), edge("b", "a", "e2")})
				ran, mutErr := mutator.result()
				Expect(mutErr).NotTo(HaveOccurred())
				Expect(ran).To(BeTrue(), "no bulk CREATE statement ran, so no endpoint was changed")

				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("created " + wantCount + " of 2 MAPS edges"))
				Expect(err.Error()).To(ContainSubstring("concurrent writer"))
				Expect(err.Error()).To(ContainSubstring("retry the batch"))
				Expect(ids).To(BeNil())
				Expect(allEdgeEndpoints()).To(ConsistOf(before))
				Expect(allEdgeIDs()).To(ConsistOf("e0"))
			},
			Entry("an endpoint deleted, so fewer edges are created",
				"MATCH (n:Control {id: 'c'}) DELETE n", "1"),
			Entry("an endpoint ID duplicated under another label, so more edges are created",
				"CREATE (:Artifact {id: 'c'})", "3"),
		)
	})
})
