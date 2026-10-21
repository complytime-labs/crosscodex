//go:build integration

// Suite bootstrap lives in graphdb_integration_bdd_test.go — do NOT add RunSpecs here.

package agedriver_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/graphdb/agedriver"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
)

// statementRecorder is a pgx QueryTracer that keeps the SQL of every
// statement the driver sends, so specs can assert a statement was or was not
// issued, and EXPLAIN the exact SQL the driver ran.
type statementRecorder struct {
	mu   sync.Mutex
	sqls []string
}

func (r *statementRecorder) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sqls = append(r.sqls, data.SQL)
	return ctx
}

func (r *statementRecorder) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// containing returns the recorded statements that contain substr, oldest first.
func (r *statementRecorder) containing(substr string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, s := range r.sqls {
		if strings.Contains(s, substr) {
			out = append(out, s)
		}
	}
	return out
}

func (r *statementRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sqls = nil
}

// labelIndexes returns the sorted index names on label's table in graph gn.
// pg_indexes is readable by graph_user.
func labelIndexes(ctx context.Context, gn, label string) []string {
	rows, err := testDB.QueryContext(ctx,
		"SELECT indexname FROM pg_indexes WHERE schemaname = $1 AND tablename = $2 ORDER BY indexname", gn, label)
	Expect(err).NotTo(HaveOccurred())
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		Expect(rows.Scan(&n)).To(Succeed())
		names = append(names, n)
	}
	Expect(rows.Err()).NotTo(HaveOccurred())
	return names
}

func wantVertexIndexes(label string) []string {
	return sortedStrings(agedriver.LabelIndexName('v', label, "gid"), agedriver.LabelIndexName('v', label, "pid"))
}

func wantEdgeIndexes(label string) []string {
	return sortedStrings(
		agedriver.LabelIndexName('e', label, "gid"), agedriver.LabelIndexName('e', label, "pid"),
		agedriver.LabelIndexName('e', label, "sid"), agedriver.LabelIndexName('e', label, "eid"))
}

func sortedStrings(s ...string) []string {
	slices.Sort(s)
	return s
}

// explainNoSeqScan returns the plan PostgreSQL picks for sqlText with
// sequential scans disabled. Test tables are tiny, so the planner would
// otherwise prefer a sequential scan even where an index applies; disabling
// it shows whether the index can serve the query at all. Plain index scans
// are disabled too, so the planner cannot satisfy an unanchored edge match
// with an ordered scan of the start_id/end_id index (an ordered Merge Append
// that ignores the id-property index); bitmap scans remain.
func explainNoSeqScan(ctx context.Context, sqlText string) string {
	tx, err := testDB.BeginTx(ctx, nil)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `SET LOCAL search_path = ag_catalog, "$user", public`)
	Expect(err).NotTo(HaveOccurred())
	_, err = tx.ExecContext(ctx, "SET LOCAL enable_seqscan = off")
	Expect(err).NotTo(HaveOccurred())
	_, err = tx.ExecContext(ctx, "SET LOCAL enable_indexscan = off")
	Expect(err).NotTo(HaveOccurred())
	rows, err := tx.QueryContext(ctx, "EXPLAIN "+sqlText)
	Expect(err).NotTo(HaveOccurred())
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var l string
		Expect(rows.Scan(&l)).To(Succeed())
		lines = append(lines, l)
	}
	Expect(rows.Err()).NotTo(HaveOccurred())
	return strings.Join(lines, "\n")
}

var _ = Describe("Label indexes", func() {
	var (
		ctx      context.Context
		tenantID string
		gn       string
		rec      *statementRecorder
		tp       *telemetrytest.TestProvider
		client   graphdb.GraphDB
		now      time.Time
	)

	newClient := func(conn *sql.DB) graphdb.GraphDB {
		c, err := agedriver.New(conn, agedriver.WithTelemetry(tp.TracerProvider().Tracer("test"), tp.MeterProvider().Meter("test")))
		Expect(err).NotTo(HaveOccurred())
		return c
	}
	ensures := func() map[string]int64 {
		m, _ := queryMetrics(tp, "ensure_label")
		return m
	}

	BeforeEach(func() {
		ctx = context.Background()
		tenantID = testID("labels")
		setupTenant(tenantID)
		DeferCleanup(func() { cleanupTenant(tenantID) })
		gn = agedriver.GraphName(tenantID)
		now = time.Now().UTC().Truncate(time.Microsecond)

		var err error
		tp, err = telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = tp.Shutdown(context.Background()) })
		rec = &statementRecorder{}
		client = newClient(openTracedGraphDB(rec))
	})

	It("indexes a vertex label and an edge label on first write, as graph_user", func() {
		Expect(client.CreateNode(ctx, tenantID, graphdb.Node{ID: "a", Label: "Control", ValidFrom: now})).To(Succeed())
		Expect(client.CreateNode(ctx, tenantID, graphdb.Node{ID: "b", Label: "Control", ValidFrom: now})).To(Succeed())
		Expect(client.CreateEdge(ctx, tenantID, "a", "b", graphdb.Edge{ID: "e1", Label: "MAPS", ValidFrom: now})).To(Succeed())

		Expect(labelIndexes(ctx, gn, "Control")).To(Equal(wantVertexIndexes("Control")))
		Expect(labelIndexes(ctx, gn, "MAPS")).To(Equal(wantEdgeIndexes("MAPS")))
	})

	It("ensures each (graph, kind, label) once per client across every write method", func() {
		_, err := client.UpsertNode(ctx, tenantID, graphdb.Node{ID: "a", Label: "Control", ValidFrom: now})
		Expect(err).NotTo(HaveOccurred())
		Expect(client.CreateNode(ctx, tenantID, graphdb.Node{ID: "b", Label: "Control", ValidFrom: now})).To(Succeed())
		Expect(client.CreateNode(ctx, tenantID, graphdb.Node{ID: "b", Label: "Control", ValidFrom: now})).To(MatchError(graphdb.ErrNodeExists))
		Expect(ensures()).To(Equal(map[string]int64{"ok/ok": 1}))

		Expect(client.CreateEdge(ctx, tenantID, "a", "b", graphdb.Edge{ID: "e1", Label: "MAPS", ValidFrom: now})).To(Succeed())
		_, err = client.BulkCreateEdges(ctx, tenantID, []graphdb.BulkEdge{
			{SourceID: "a", TargetID: "b", Edge: graphdb.Edge{ID: "e2", Label: "MAPS", ValidFrom: now}},
			{SourceID: "b", TargetID: "a", Edge: graphdb.Edge{ID: "e3", Label: "LINKS", ValidFrom: now}},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(client.CreateRequiresEdge(ctx, tenantID, graphdb.RequiresEdge{
			SourceID: "a", TargetID: "b", Confidence: 0.9, ValidVotes: 1, TotalVotes: 1,
			Models: []string{"m"}, SamplesPerModel: 1, AnalyzedAt: now, TenantID: tenantID, JobID: "job-1",
		})).To(Succeed())
		// Control, MAPS, LINKS, REQUIRES.
		Expect(ensures()).To(Equal(map[string]int64{"ok/ok": 4}))
		Expect(labelIndexes(ctx, gn, "LINKS")).To(Equal(wantEdgeIndexes("LINKS")))
		Expect(labelIndexes(ctx, gn, "REQUIRES")).To(Equal(wantEdgeIndexes("REQUIRES")))
	})

	It("refuses an edge label that is already a node label, writes nothing and does not cache the failure", func() {
		Expect(client.CreateNode(ctx, tenantID, graphdb.Node{ID: "a", Label: "Dual", ValidFrom: now})).To(Succeed())
		Expect(client.CreateNode(ctx, tenantID, graphdb.Node{ID: "b", Label: "Dual", ValidFrom: now})).To(Succeed())
		rec.reset()

		for range 2 {
			err := client.CreateEdge(ctx, tenantID, "a", "b", graphdb.Edge{ID: "e1", Label: "Dual", ValidFrom: now})
			Expect(err).To(MatchError(graphdb.ErrLabelKindConflict))
			var conflict *graphdb.LabelKindConflictError
			Expect(errors.As(err, &conflict)).To(BeTrue())
			Expect(*conflict).To(Equal(graphdb.LabelKindConflictError{Label: "Dual", Kind: "edge", StoredKind: "node"}))
			// The typed error names the label, so ensureLabel adds no prefix.
			Expect(err.Error()).To(HavePrefix(`create edge: label already used for the other element kind: cannot write edge label "Dual"`))
			Expect(err.Error()).To(ContainSubstring(`"Dual" is already a node label`))
			Expect(err.Error()).To(ContainSubstring("use a different label name"))
			// CWE-209: the caller never sees the internal graph name.
			Expect(err.Error()).NotTo(ContainSubstring(gn))
		}
		Expect(ensures()).To(Equal(map[string]int64{"ok/ok": 1, "error/error": 2}))
		Expect(rec.containing("CREATE (s)-[e:Dual")).To(BeEmpty())
		rels, err := client.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{})
		Expect(err).NotTo(HaveOccurred())
		Expect(rels).To(BeEmpty())
		Expect(labelIndexes(ctx, gn, "Dual")).To(Equal(wantVertexIndexes("Dual")))
	})

	It("lets concurrent first writers of a new label all succeed, with exactly one creating it", func() {
		const writers = 4
		// One traced pool for every writer, so rec sees all their statements;
		// separate clients have separate caches, so every writer runs ensure.
		conn := openTracedGraphDB(rec)
		start := make(chan struct{})
		var wg sync.WaitGroup
		errs := make([]error, writers)
		for i := range writers {
			c := newClient(conn)
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer GinkgoRecover()
				<-start
				errs[i] = c.CreateNode(ctx, tenantID, graphdb.Node{ID: fmt.Sprintf("n%d", i), Label: "Fresh", ValidFrom: now})
			}()
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			Expect(err).NotTo(HaveOccurred(), "writer %d", i)
		}
		Expect(rec.containing("create_vlabel")).To(HaveLen(1), "the label lock must let exactly one writer create the label")
		Expect(ensures()).To(Equal(map[string]int64{"ok/ok": writers}))
		Expect(labelIndexes(ctx, gn, "Fresh")).To(Equal(wantVertexIndexes("Fresh")))
	})

	It("lets concurrent first writers of one label name as a node and as an edge get one success and one LabelKindConflictError", func() {
		Expect(client.CreateNode(ctx, tenantID, graphdb.Node{ID: "a", Label: "Ends", ValidFrom: now})).To(Succeed())
		Expect(client.CreateNode(ctx, tenantID, graphdb.Node{ID: "b", Label: "Ends", ValidFrom: now})).To(Succeed())
		nodeClient, edgeClient := newClient(testDB), newClient(testDB)

		start := make(chan struct{})
		var wg sync.WaitGroup
		var nodeErr, edgeErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			defer GinkgoRecover()
			<-start
			nodeErr = nodeClient.CreateNode(ctx, tenantID, graphdb.Node{ID: "r", Label: "Race", ValidFrom: now})
		}()
		go func() {
			defer wg.Done()
			defer GinkgoRecover()
			<-start
			edgeErr = edgeClient.CreateEdge(ctx, tenantID, "a", "b", graphdb.Edge{ID: "e-race", Label: "Race", ValidFrom: now})
		}()
		close(start)
		wg.Wait()

		Expect((nodeErr == nil) != (edgeErr == nil)).To(BeTrue(),
			"want exactly one success; CreateNode: %v, CreateEdge: %v", nodeErr, edgeErr)
		loser, wantConflict, stored := edgeErr, graphdb.LabelKindConflictError{Label: "Race", Kind: "edge", StoredKind: "node"}, `"Race" is already a node label`
		if nodeErr != nil {
			loser, wantConflict, stored = nodeErr, graphdb.LabelKindConflictError{Label: "Race", Kind: "node", StoredKind: "edge"}, `"Race" is already an edge label`
		}
		Expect(loser).To(MatchError(graphdb.ErrLabelKindConflict))
		var conflict *graphdb.LabelKindConflictError
		Expect(errors.As(loser, &conflict)).To(BeTrue())
		Expect(*conflict).To(Equal(wantConflict))
		Expect(loser.Error()).To(ContainSubstring(stored))
		Expect(loser.Error()).NotTo(ContainSubstring("already exists"))
	})

	It("indexes a legacy label table that Cypher CREATE made without indexes on its next driver write", func() {
		seedLegacyNode(ctx, gn, "Legacy", "old-1")
		Expect(labelIndexes(ctx, gn, "Legacy")).To(BeEmpty())

		Expect(client.CreateNode(ctx, tenantID, graphdb.Node{ID: "new-1", Label: "Legacy", ValidFrom: now})).To(Succeed())
		Expect(labelIndexes(ctx, gn, "Legacy")).To(Equal(wantVertexIndexes("Legacy")))
		_, err := client.GetNode(ctx, tenantID, "old-1")
		Expect(err).NotTo(HaveOccurred())
	})

	It("forgets a graph's labels when CreateGraph recreates the graph", func() {
		// Only a superuser can create a graph; graph_user cannot (the tenant
		// trigger does it in production).
		su, err := sql.Open("pgx", suDSN)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(su.Close)
		suClient := newClient(su)

		Expect(suClient.CreateNode(ctx, tenantID, graphdb.Node{ID: "a", Label: "Cached", ValidFrom: now})).To(Succeed())
		Expect(ensures()).To(Equal(map[string]int64{"ok/ok": 1}))

		// CreateGraph on the existing graph creates nothing and keeps the cache.
		Expect(suClient.CreateGraph(ctx, tenantID)).To(Succeed())
		Expect(suClient.CreateNode(ctx, tenantID, graphdb.Node{ID: "b", Label: "Cached", ValidFrom: now})).To(Succeed())
		Expect(ensures()).To(Equal(map[string]int64{"ok/ok": 1}))

		_, err = su.ExecContext(ctx, "SELECT ag_catalog.drop_graph($1, true)", gn)
		Expect(err).NotTo(HaveOccurred())
		Expect(suClient.CreateGraph(ctx, tenantID)).To(Succeed())

		Expect(suClient.CreateNode(ctx, tenantID, graphdb.Node{ID: "a", Label: "Cached", ValidFrom: now})).To(Succeed())
		Expect(ensures()).To(Equal(map[string]int64{"ok/ok": 2}))
		Expect(labelIndexes(ctx, gn, "Cached")).To(Equal(wantVertexIndexes("Cached")))
	})

	Describe("ID lookups", func() {
		BeforeEach(func() {
			Expect(client.CreateNode(ctx, tenantID, graphdb.Node{ID: "a", Label: "Control", ValidFrom: now})).To(Succeed())
			Expect(client.CreateNode(ctx, tenantID, graphdb.Node{ID: "b", Label: "Control", ValidFrom: now})).To(Succeed())
			Expect(client.CreateEdge(ctx, tenantID, "a", "b", graphdb.Edge{ID: "e1", Label: "MAPS", ValidFrom: now})).To(Succeed())
			rec.reset()
		})

		// planOf EXPLAINs the single recorded statement containing marker.
		planOf := func(marker string) string {
			stmts := rec.containing(marker)
			Expect(stmts).To(HaveLen(1), "statements containing %q: %v", marker, rec.containing(""))
			return explainNoSeqScan(ctx, stmts[0])
		}

		It("finds a node by ID through the vertex id-property index (storedNodeLabels)", func() {
			Expect(client.CreateNode(ctx, tenantID, graphdb.Node{ID: "c", Label: "Control", ValidFrom: now})).To(Succeed())
			Expect(planOf("WHERE n.id = 'c' RETURN n")).To(ContainSubstring(agedriver.LabelIndexName('v', "Control", "pid")))
		})

		It("checks an edge ID through the edge id-property index", func() {
			Expect(client.CreateEdge(ctx, tenantID, "b", "a", graphdb.Edge{ID: "e2", Label: "MAPS", ValidFrom: now})).To(Succeed())
			Expect(planOf("WHERE e.id = 'e2' RETURN count(e)")).To(ContainSubstring(agedriver.LabelIndexName('e', "MAPS", "pid")))
		})

		It("matches CreateEdge's endpoints through the vertex id-property index", func() {
			Expect(client.CreateEdge(ctx, tenantID, "a", "b", graphdb.Edge{ID: "e3", Label: "MAPS", ValidFrom: now})).To(Succeed())
			Expect(planOf("WHERE s.id = 'a' AND t.id = 'b' CREATE")).To(ContainSubstring(agedriver.LabelIndexName('v', "Control", "pid")))
		})

		// GetNode's lookup shares its SQL with storedNodeLabels; this spec guards
		// against the two drifting apart.
		It("serves GetNode from the vertex id-property index", func() {
			_, err := client.GetNode(ctx, tenantID, "a")
			Expect(err).NotTo(HaveOccurred())
			Expect(planOf("WHERE n.id = 'a' RETURN n")).To(ContainSubstring(agedriver.LabelIndexName('v', "Control", "pid")))
		})

		It("finds a depth-1 traversal's start node through the vertex id-property index", func() {
			paths, err := client.Traverse(ctx, tenantID, graphdb.TraversalQuery{StartNode: "a", EdgeLabels: []string{"MAPS"}, MaxDepth: 1})
			Expect(err).NotTo(HaveOccurred())
			Expect(paths).To(HaveLen(1))
			Expect(planOf("WHERE start_node.id = 'a'")).To(ContainSubstring(agedriver.LabelIndexName('v', "Control", "pid")))
		})
	})
})
