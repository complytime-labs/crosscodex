//go:build integration

// Suite bootstrap lives in graphdb_integration_bdd_test.go — do NOT add RunSpecs here.

package agedriver_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/codes"

	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/graphdb/agedriver"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
)

type killCtxKey struct{}

// commitKiller is a pgx QueryTracer that, once armed, terminates the backend
// right after the first successful statement whose SQL contains marker. The
// write has run but is uncommitted, so the driver's COMMIT is the first call
// to fail. A deferred constraint trigger cannot inject this failure because
// AGE writes label tables directly and PostgreSQL fires no triggers for them.
type commitKiller struct {
	su     *sql.DB
	mu     sync.Mutex
	marker string
	// killed reports whether a backend was terminated; killErr holds a
	// failure to terminate it. The spec asserts both, because a failed
	// assertion inside a pgx callback would not stop the spec cleanly.
	killed  bool
	killErr error
}

func (k *commitKiller) arm(marker string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.marker = marker
}

func (k *commitKiller) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.marker != "" && strings.Contains(data.SQL, k.marker) {
		k.marker = ""
		return context.WithValue(ctx, killCtxKey{}, true)
	}
	return ctx
}

func (k *commitKiller) TraceQueryEnd(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryEndData) {
	if ctx.Value(killCtxKey{}) == nil || data.Err != nil {
		return
	}
	// The 5 s timeout makes pg_terminate_backend wait until the backend exits.
	var ok bool
	err := k.su.QueryRowContext(context.Background(),
		"SELECT pg_terminate_backend($1, 5000)", int(conn.PgConn().PID())).Scan(&ok)
	k.mu.Lock()
	defer k.mu.Unlock()
	switch {
	case err != nil:
		k.killErr = err
	case !ok:
		k.killErr = errors.New("pg_terminate_backend returned false")
	default:
		k.killed = true
	}
}

// result returns whether a backend was terminated and any error doing so.
func (k *commitKiller) result() (bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.killed, k.killErr
}

var _ = Describe("Commit failure telemetry", func() {
	var (
		client           graphdb.GraphDB
		killer           *commitKiller
		tp               *telemetrytest.TestProvider
		tenantID         string
		ctx              context.Context
		now              time.Time
		requires         graphdb.RequiresEdge
		existingRequires graphdb.RequiresEdge
	)

	BeforeEach(func() {
		tenantID = testID("commit-fail")
		setupTenant(tenantID)
		DeferCleanup(func() { cleanupTenant(tenantID) })

		su, err := sql.Open("pgx", suDSN)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(su.Close)
		killer = &commitKiller{su: su}

		conn := openTracedGraphDB(killer)

		tp, err = telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { tp.Shutdown(context.Background()) }) //nolint:errcheck

		client, err = agedriver.New(conn, agedriver.WithTelemetry(tp.TracerProvider().Tracer("test"), tp.MeterProvider().Meter("test")))
		Expect(err).NotTo(HaveOccurred())
		ctx = context.Background()
		now = time.Now().UTC().Truncate(time.Microsecond)

		Expect(client.CreateGraph(ctx, tenantID)).To(Succeed())
		Expect(client.CreateNode(ctx, tenantID, graphdb.Node{ID: "a", Label: "Control", ValidFrom: now})).To(Succeed())
		Expect(client.CreateNode(ctx, tenantID, graphdb.Node{ID: "b", Label: "Control", ValidFrom: now})).To(Succeed())
		Expect(client.CreateEdge(ctx, tenantID, "a", "b", graphdb.Edge{ID: "e0", Label: "MAPS", ValidFrom: now})).To(Succeed())
		requires = graphdb.RequiresEdge{
			SourceID: "a", TargetID: "b", Confidence: 0.9, ValidVotes: 3, TotalVotes: 3,
			Models: []string{"m"}, SamplesPerModel: 1, PromptVersion: "1.0.0",
			AnalyzedAt: now, TenantID: tenantID, JobID: "job-commit",
		}
		// existingRequires is already stored, so re-creating it meets
		// ErrEdgeExists. It has its own job ID, so its edge ID differs from
		// requires', which the commit-failure table must be able to create.
		existingRequires = requires
		existingRequires.JobID = "job-existing"
		Expect(client.CreateRequiresEdge(ctx, tenantID, existingRequires)).To(Succeed())
	})

	DescribeTable("records a successful call under its operation with status ok",
		func(metricOp string, op func() error) {
			before, beforeLatencies := queryMetrics(tp, metricOp)
			Expect(op()).To(Succeed())
			after, latencies := queryMetrics(tp, metricOp)
			Expect(after["ok/ok"]).To(Equal(before["ok/ok"] + 1))
			Expect(after["error/error"]).To(Equal(before["error/error"]))
			Expect(latencies).To(Equal(beforeLatencies + 1))
		},
		Entry("CreateNode", "create_node", func() error {
			return client.CreateNode(ctx, tenantID, graphdb.Node{ID: "c", Label: "Control", ValidFrom: now})
		}),
		Entry("QueryRelationships", "query_relationships", func() error {
			_, err := client.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{})
			return err
		}),
		Entry("QueryAsOf", "query_as_of", func() error {
			_, err := client.QueryAsOf(ctx, tenantID, graphdb.RelationshipQuery{}, now)
			return err
		}),
	)

	DescribeTable("records a domain sentinel as status ok with its result and marks the span Ok, not Error",
		func(spanName, metricOp, wantResult string, sentinel error, op func() error) {
			before, beforeLatencies := queryMetrics(tp, metricOp)
			Expect(op()).To(MatchError(sentinel))
			after, latencies := queryMetrics(tp, metricOp)
			Expect(after["ok/"+wantResult]).To(Equal(before["ok/"+wantResult] + 1))
			Expect(after["error/error"]).To(Equal(before["error/error"]))
			Expect(latencies).To(Equal(beforeLatencies + 1))

			spans := telemetrytest.FindSpans(tp.GetSpans(), spanName)
			Expect(spans).NotTo(BeEmpty())
			last := spans[len(spans)-1]
			Expect(last.Status().Code).To(Equal(codes.Ok), "span %s has status %v: %s", spanName, last.Status().Code, last.Status().Description)
			got, found := telemetrytest.SpanAttribute(last, "graphdb.result")
			Expect(found).To(BeTrue(), "span %s has no graphdb.result attribute", spanName)
			Expect(got.AsString()).To(Equal(wantResult))
		},
		Entry("CreateNode of an existing node", "graphdb.CreateNode", "create_node", "exists", graphdb.ErrNodeExists, func() error {
			return client.CreateNode(ctx, tenantID, graphdb.Node{ID: "a", Label: "Control", ValidFrom: now})
		}),
		Entry("CreateEdge with an existing edge ID", "graphdb.CreateEdge", "create_edge", "exists", graphdb.ErrEdgeExists, func() error {
			return client.CreateEdge(ctx, tenantID, "b", "a", graphdb.Edge{ID: "e0", Label: "MAPS", ValidFrom: now})
		}),
		// A missing endpoint is the caller's outcome, not a backend failure.
		Entry("CreateEdge with a missing target endpoint", "graphdb.CreateEdge", "create_edge", "not_found", graphdb.ErrNodeNotFound, func() error {
			return client.CreateEdge(ctx, tenantID, "a", "missing", graphdb.Edge{ID: "e-missing", Label: "MAPS", ValidFrom: now})
		}),
		Entry("CreateRequiresEdge of an existing edge", "graphdb.CreateRequiresEdge", "create_requires_edge", "exists", graphdb.ErrEdgeExists, func() error {
			return client.CreateRequiresEdge(ctx, tenantID, existingRequires)
		}),
		Entry("CreateRequiresEdge with a missing target endpoint", "graphdb.CreateRequiresEdge", "create_requires_edge", "not_found", graphdb.ErrNodeNotFound, func() error {
			missing := requires
			missing.TargetID = "missing"
			return client.CreateRequiresEdge(ctx, tenantID, missing)
		}),
		Entry("BulkCreateEdges with an existing edge ID", "graphdb.BulkCreateEdges", "bulk_create_edges", "exists", graphdb.ErrEdgeExists, func() error {
			_, err := client.BulkCreateEdges(ctx, tenantID, []graphdb.BulkEdge{
				{SourceID: "b", TargetID: "a", Edge: graphdb.Edge{ID: "e0", Label: "MAPS", ValidFrom: now}},
			})
			return err
		}),
		Entry("BulkCreateEdges with a missing target endpoint", "graphdb.BulkCreateEdges", "bulk_create_edges", "not_found", graphdb.ErrNodeNotFound, func() error {
			_, err := client.BulkCreateEdges(ctx, tenantID, []graphdb.BulkEdge{
				{SourceID: "a", TargetID: "missing", Edge: graphdb.Edge{ID: "e-missing", Label: "MAPS", ValidFrom: now}},
			})
			return err
		}),
		Entry("GetNode of a missing node", "graphdb.GetNode", "get_node", "not_found", graphdb.ErrNodeNotFound, func() error {
			_, err := client.GetNode(ctx, tenantID, "missing")
			return err
		}),
		Entry("GetEdge of a missing edge", "graphdb.GetEdge", "get_edge", "not_found", graphdb.ErrEdgeNotFound, func() error {
			_, err := client.GetEdge(ctx, tenantID, "missing")
			return err
		}),
	)

	// The write paths are covered here. CreateGraph and the read paths
	// (QueryRelationships, QueryAsOf, Traverse, GetNode, GetEdge,
	// ExecuteQuery) finish through the same commit helper, or commit before
	// reporting success, and are not covered individually. Read-path error
	// status is covered without a database by "counts a call rejected inside
	// its span as an error of its operation" in graphdb_bdd_test.go.
	DescribeTable("marks the span Error, counts the call as an error of its operation, and writes nothing",
		func(marker, spanName, metricOp, wantPrefix string, op func() error, unchanged func()) {
			before, _ := queryMetrics(tp, metricOp)
			killer.arm(marker)

			err := op()
			killed, killErr := killer.result()
			Expect(killErr).NotTo(HaveOccurred())
			Expect(killed).To(BeTrue(), "no statement matched %q, so no commit failure was injected", marker)
			Expect(err).To(MatchError(ContainSubstring(wantPrefix + ": commit: ")))

			spans := telemetrytest.FindSpans(tp.GetSpans(), spanName)
			Expect(spans).NotTo(BeEmpty())
			Expect(spans[len(spans)-1].Status().Code).To(Equal(codes.Error))
			after, _ := queryMetrics(tp, metricOp)
			Expect(after["error/error"]).To(Equal(before["error/error"] + 1))
			Expect(after["ok/ok"]).To(Equal(before["ok/ok"]))
			unchanged()
		},
		Entry("CreateNode", "CREATE (n:", "graphdb.CreateNode", "create_node", "create node",
			func() error {
				return client.CreateNode(ctx, tenantID, graphdb.Node{ID: "c", Label: "Control", ValidFrom: now})
			},
			func() {
				_, err := client.GetNode(ctx, tenantID, "c")
				Expect(err).To(MatchError(graphdb.ErrNodeNotFound))
			}),
		Entry("UpsertNode creating a node", "CREATE (n:", "graphdb.UpsertNode", "upsert_node", "upsert node",
			func() error {
				_, err := client.UpsertNode(ctx, tenantID, graphdb.Node{ID: "c", Label: "Control", ValidFrom: now})
				return err
			},
			func() {
				_, err := client.GetNode(ctx, tenantID, "c")
				Expect(err).To(MatchError(graphdb.ErrNodeNotFound))
			}),
		Entry("UpsertNode updating a node", " SET n = ", "graphdb.UpsertNode", "upsert_node", "upsert node",
			func() error {
				_, err := client.UpsertNode(ctx, tenantID, graphdb.Node{ID: "a", Label: "Control", ValidFrom: now, Properties: map[string]any{"rev": "2"}})
				return err
			},
			func() {
				n, err := client.GetNode(ctx, tenantID, "a")
				Expect(err).NotTo(HaveOccurred())
				Expect(n.Properties).NotTo(HaveKey("rev"))
			}),
		Entry("CreateEdge", "CREATE (s)-[e:", "graphdb.CreateEdge", "create_edge", "create edge",
			func() error {
				return client.CreateEdge(ctx, tenantID, "b", "a", graphdb.Edge{ID: "e1", Label: "MAPS", ValidFrom: now})
			},
			func() {
				_, err := client.GetEdge(ctx, tenantID, "e1")
				Expect(err).To(MatchError(graphdb.ErrEdgeNotFound))
			}),
		Entry("BulkCreateEdges", "UNWIND [", "graphdb.BulkCreateEdges", "bulk_create_edges", "bulk create edges",
			func() error {
				_, err := client.BulkCreateEdges(ctx, tenantID, []graphdb.BulkEdge{
					{SourceID: "b", TargetID: "a", Edge: graphdb.Edge{ID: "e1", Label: "MAPS", ValidFrom: now}},
				})
				return err
			},
			func() {
				_, err := client.GetEdge(ctx, tenantID, "e1")
				Expect(err).To(MatchError(graphdb.ErrEdgeNotFound))
			}),
		Entry("CreateRequiresEdge", "CREATE (s)-[e:", "graphdb.CreateRequiresEdge", "create_requires_edge", "create requires edge",
			func() error {
				return client.CreateRequiresEdge(ctx, tenantID, requires)
			},
			func() {
				_, err := client.GetEdge(ctx, tenantID, requires.EdgeID())
				Expect(err).To(MatchError(graphdb.ErrEdgeNotFound))
			}),
		Entry("SupersedeFact", " SET ", "graphdb.SupersedeFact", "supersede_fact", "supersede fact",
			func() error {
				_, err := client.SupersedeFact(ctx, tenantID, graphdb.SupersedeRequest{EdgeID: "e0", SupersededAt: now.Add(time.Hour), SupersededByJobID: "job-1"})
				return err
			},
			func() {
				e, err := client.GetEdge(ctx, tenantID, "e0")
				Expect(err).NotTo(HaveOccurred())
				Expect(e.Edge.ValidTo).To(BeNil())
			}),
	)
})
