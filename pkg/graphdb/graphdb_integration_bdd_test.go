//go:build integration

package graphdb_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
)

func TestGraphDBIntegrationBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "GraphDB Integration Suite")
}

// ---------------------------------------------------------------------------
// Package-level state initialised by SynchronizedBeforeSuite
// ---------------------------------------------------------------------------

var (
	suDSN  string
	testDB *sql.DB
)

// Redirect slog output to GinkgoWriter so log noise only appears on failure.
var _ = BeforeEach(func() { DeferCleanup(testspecs.RedirectLogsToGinkgo()) })

var _ = SynchronizedBeforeSuite(func() []byte {
	// Runs on node 1 only: migrate + set role passwords.
	dsn := os.Getenv("TEST_DATABASE_DSN")
	if dsn == "" {
		Fail("TEST_DATABASE_DSN not set — run: task test:integration:db")
	}

	ctx := context.Background()

	// Run migrations.
	migrator, err := db.NewMigrator(dsn)
	Expect(err).NotTo(HaveOccurred(), "failed to create migrator")
	Expect(migrator.Up(ctx)).To(Succeed(), "failed to run migrations")
	Expect(migrator.Close()).To(Succeed(), "failed to close migrator")

	// Set up graph_user password.
	adminDB, err := sql.Open("pgx", dsn)
	Expect(err).NotTo(HaveOccurred(), "failed to open admin connection")
	_, err = adminDB.ExecContext(ctx, "ALTER ROLE graph_user WITH PASSWORD 'graphpass'")
	Expect(err).NotTo(HaveOccurred(), "failed to set graph_user password")
	Expect(adminDB.Close()).To(Succeed(), "failed to close admin connection")

	return []byte(dsn)
}, func(data []byte) {
	// Runs on all nodes: store DSN and open graph_user connection.
	suDSN = string(data)

	u, err := url.Parse(suDSN)
	Expect(err).NotTo(HaveOccurred(), "failed to parse DSN")
	u.User = url.UserPassword("graph_user", "graphpass")

	testDB, err = sql.Open("pgx", u.String())
	Expect(err).NotTo(HaveOccurred(), "failed to open graph_user connection")
	testDB.SetMaxOpenConns(5)
})

var _ = AfterSuite(func() {
	if testDB != nil {
		testDB.Close()
	}
})

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func setupTenant(tenantID string) {
	suDB, err := sql.Open("pgx", suDSN)
	Expect(err).NotTo(HaveOccurred(), "open superuser conn")
	DeferCleanup(func() { suDB.Close() })

	_, err = suDB.ExecContext(context.Background(),
		"INSERT INTO tenants (tenant_id, display_name) VALUES ($1, $2) ON CONFLICT DO NOTHING",
		tenantID, "Test Tenant "+tenantID)
	Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("setupTenant(%q)", tenantID))
}

func testID(prefix string) string {
	b := make([]byte, 4)
	_, err := rand.Read(b)
	Expect(err).NotTo(HaveOccurred(), "testID rand")
	return fmt.Sprintf("%s-%x", prefix, b)
}

func cleanupTenant(tenantID string) {
	suDB, err := sql.Open("pgx", suDSN)
	if err != nil {
		GinkgoWriter.Printf("cleanupTenant open: %v\n", err)
		return
	}
	defer suDB.Close()

	ctx := context.Background()
	if _, err := suDB.ExecContext(ctx, "LOAD 'age'"); err != nil {
		GinkgoWriter.Printf("cleanupTenant LOAD age: %v\n", err)
	}
	if _, err := suDB.ExecContext(ctx,
		"DELETE FROM tenants WHERE tenant_id = $1", tenantID); err != nil {
		GinkgoWriter.Printf("cleanupTenant(%q): %v\n", tenantID, err)
	}
}

// ---------------------------------------------------------------------------
// Integration Tests
// ---------------------------------------------------------------------------

var _ = Describe("GraphDB Integration", Ordered, func() {

	// ===================================================================
	// Graph Creation
	// ===================================================================

	Describe("Graph Creation", func() {
		It("is idempotent", func() {
			tenantID := "graphdb-idempotent"
			setupTenant(tenantID)

			client, err := graphdb.New(testDB)
			Expect(err).NotTo(HaveOccurred())

			// Graph already exists via the tenant-insert trigger.
			// Calling CreateGraph again must not error.
			Expect(client.CreateGraph(context.Background(), tenantID)).To(Succeed())
			// A third call must also succeed.
			Expect(client.CreateGraph(context.Background(), tenantID)).To(Succeed())
		})
	})

	// ===================================================================
	// Node Operations
	// ===================================================================

	Describe("Node Operations", func() {
		It("creates a node and allows subsequent queries", func() {
			tenantID := "graphdb-create-node"
			setupTenant(tenantID)

			client, err := graphdb.New(testDB)
			Expect(err).NotTo(HaveOccurred())

			now := time.Now().UTC().Truncate(time.Microsecond)
			node := graphdb.Node{
				ID:             "req-001",
				Label:          "Requirement",
				ValidFrom:      now,
				CreatedBy:      "test-job",
				CreationMethod: "import",
				Properties:     map[string]any{"framework": "NIST-800-53"},
			}
			Expect(client.CreateNode(context.Background(), tenantID, node)).To(Succeed())

			// Verify graph is queryable (no edges yet).
			results, err := client.QueryRelationships(context.Background(), tenantID, graphdb.RelationshipQuery{
				SourceLabel: "Requirement",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(BeEmpty())
		})
	})

	// ===================================================================
	// Edge Operations
	// ===================================================================

	Describe("Edge Operations", func() {
		It("creates an edge and round-trips all temporal/provenance attributes", func() {
			tenantID := testID("graphdb-edge")
			setupTenant(tenantID)
			DeferCleanup(func() { cleanupTenant(tenantID) })

			client, err := graphdb.New(testDB)
			Expect(err).NotTo(HaveOccurred())
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Microsecond)

			// Create source node.
			Expect(client.CreateNode(ctx, tenantID, graphdb.Node{
				ID:             "src-001",
				Label:          "Requirement",
				ValidFrom:      now,
				CreatedBy:      "import-job-7",
				CreationMethod: "oscal-import",
				Properties:     map[string]any{"framework": "NIST-800-53"},
			})).To(Succeed())

			// Create target node.
			Expect(client.CreateNode(ctx, tenantID, graphdb.Node{
				ID:             "tgt-001",
				Label:          "Document",
				ValidFrom:      now,
				CreatedBy:      "catalog-loader",
				CreationMethod: "bulk-import",
			})).To(Succeed())

			// Create edge.
			Expect(client.CreateEdge(ctx, tenantID, "src-001", "tgt-001", graphdb.Edge{
				ID:                "edge-001",
				Label:             "DEFINED_IN",
				ValidFrom:         now,
				DeterminedBy:      "job-1",
				DeterminationType: "llm_initial",
				Confidence:        0.85,
			})).To(Succeed())

			// Query back and verify.
			results, err := client.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{
				EdgeLabel: "DEFINED_IN",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(1))

			rel := results[0]

			// Edge assertions.
			Expect(rel.Edge.Label).To(Equal("DEFINED_IN"))
			Expect(rel.Edge.ID).To(Equal("edge-001"))
			Expect(rel.Edge.DeterminedBy).To(Equal("job-1"))
			Expect(rel.Edge.DeterminationType).To(Equal("llm_initial"))
			Expect(rel.Edge.Confidence).To(Equal(0.85))
			Expect(rel.Edge.ValidFrom.IsZero()).To(BeFalse())
			Expect(rel.Edge.ValidTo).To(BeNil())

			// Source node assertions.
			Expect(rel.Source.Label).To(Equal("Requirement"))
			Expect(rel.Source.ID).To(Equal("src-001"))
			Expect(rel.Source.CreatedBy).To(Equal("import-job-7"))
			Expect(rel.Source.CreationMethod).To(Equal("oscal-import"))
			Expect(rel.Source.ValidFrom.IsZero()).To(BeFalse())
			Expect(rel.Source.Properties).To(HaveKeyWithValue("framework", "NIST-800-53"))

			// Target node assertions.
			Expect(rel.Target.Label).To(Equal("Document"))
			Expect(rel.Target.ID).To(Equal("tgt-001"))
			Expect(rel.Target.CreatedBy).To(Equal("catalog-loader"))
			Expect(rel.Target.CreationMethod).To(Equal("bulk-import"))
		})
	})

	// ===================================================================
	// Temporal Queries
	// ===================================================================

	Describe("Temporal Queries", func() {
		Context("QueryAsOf", func() {
			It("returns edges valid at the specified point in time", func() {
				tenantID := testID("graphdb-asof")
				setupTenant(tenantID)
				DeferCleanup(func() { cleanupTenant(tenantID) })

				client, err := graphdb.New(testDB)
				Expect(err).NotTo(HaveOccurred())
				ctx := context.Background()

				baseTime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

				// Create two nodes.
				Expect(client.CreateNode(ctx, tenantID, graphdb.Node{
					ID: "asof-src", Label: "Requirement", ValidFrom: baseTime,
				})).To(Succeed())
				Expect(client.CreateNode(ctx, tenantID, graphdb.Node{
					ID: "asof-tgt", Label: "Document", ValidFrom: baseTime,
				})).To(Succeed())

				// Old edge: valid 2025-01-01 to 2025-06-01.
				oldValidTo := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
				Expect(client.CreateEdge(ctx, tenantID, "asof-src", "asof-tgt", graphdb.Edge{
					ID: "asof-edge-old", Label: "MAPS_TO",
					ValidFrom: baseTime, ValidTo: &oldValidTo,
					DeterminedBy: "job-old", DeterminationType: "llm_initial",
					Confidence: 0.7,
				})).To(Succeed())

				// New edge: valid from 2025-06-01.
				newValidFrom := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
				Expect(client.CreateEdge(ctx, tenantID, "asof-src", "asof-tgt", graphdb.Edge{
					ID: "asof-edge-new", Label: "MAPS_TO",
					ValidFrom:    newValidFrom,
					DeterminedBy: "job-new", DeterminationType: "human_feedback",
					Confidence: 0.95, Supersedes: "asof-edge-old",
				})).To(Succeed())

				q := graphdb.RelationshipQuery{EdgeLabel: "MAPS_TO"}

				By("returning 0 results before any edge existed")
				beforeAll := time.Date(2024, 12, 1, 0, 0, 0, 0, time.UTC)
				results, err := client.QueryAsOf(ctx, tenantID, q, beforeAll)
				Expect(err).NotTo(HaveOccurred())
				Expect(results).To(BeEmpty())

				By("returning the old edge during its validity period")
				duringOld := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)
				results, err = client.QueryAsOf(ctx, tenantID, q, duringOld)
				Expect(err).NotTo(HaveOccurred())
				Expect(results).To(HaveLen(1))
				Expect(results[0].Edge.DeterminationType).To(Equal("llm_initial"))

				By("returning the new edge after supersession")
				afterSupersede := time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC)
				results, err = client.QueryAsOf(ctx, tenantID, q, afterSupersede)
				Expect(err).NotTo(HaveOccurred())
				Expect(results).To(HaveLen(1))
				Expect(results[0].Edge.DeterminationType).To(Equal("human_feedback"))
				Expect(results[0].Edge.Supersedes).To(Equal("asof-edge-old"))
			})
		})

		Context("current state queries", func() {
			It("returns only current edges (valid_to IS NULL)", func() {
				tenantID := testID("graphdb-temporal")
				setupTenant(tenantID)
				DeferCleanup(func() { cleanupTenant(tenantID) })

				client, err := graphdb.New(testDB)
				Expect(err).NotTo(HaveOccurred())
				ctx := context.Background()
				now := time.Now().UTC().Truncate(time.Microsecond)

				// Create two nodes.
				Expect(client.CreateNode(ctx, tenantID, graphdb.Node{
					ID: "tc-src", Label: "Requirement", ValidFrom: now,
				})).To(Succeed())
				Expect(client.CreateNode(ctx, tenantID, graphdb.Node{
					ID: "tc-tgt", Label: "Document", ValidFrom: now,
				})).To(Succeed())

				// Closed edge (valid_to set).
				closedEnd := now.Add(-1 * time.Hour)
				closedStart := closedEnd.Add(-24 * time.Hour)
				Expect(client.CreateEdge(ctx, tenantID, "tc-src", "tc-tgt", graphdb.Edge{
					ID: "tc-edge-closed", Label: "REFERENCES",
					ValidFrom: closedStart, ValidTo: &closedEnd,
					DeterminedBy: "job-old", DeterminationType: "llm_initial",
					Confidence: 0.6,
				})).To(Succeed())

				// Current edge (valid_to nil).
				Expect(client.CreateEdge(ctx, tenantID, "tc-src", "tc-tgt", graphdb.Edge{
					ID: "tc-edge-current", Label: "REFERENCES",
					ValidFrom:    now,
					DeterminedBy: "job-new", DeterminationType: "human_feedback",
					Confidence: 0.95, Supersedes: "tc-edge-closed",
				})).To(Succeed())

				results, err := client.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{
					EdgeLabel: "REFERENCES",
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(results).To(HaveLen(1))
				Expect(results[0].Edge.ID).To(Equal("tc-edge-current"))
			})
		})
	})

	// ===================================================================
	// Edge Versioning
	// ===================================================================

	Describe("Edge Versioning", func() {
		It("supersedes old edges and supports historical queries", func() {
			tenantID := testID("graphdb-edgever")
			setupTenant(tenantID)
			DeferCleanup(func() { cleanupTenant(tenantID) })

			client, err := graphdb.New(testDB)
			Expect(err).NotTo(HaveOccurred())
			ctx := context.Background()
			baseTime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

			// Create nodes.
			Expect(client.CreateNode(ctx, tenantID, graphdb.Node{
				ID: "ev-src", Label: "Control", ValidFrom: baseTime,
			})).To(Succeed())
			Expect(client.CreateNode(ctx, tenantID, graphdb.Node{
				ID: "ev-tgt", Label: "Evidence", ValidFrom: baseTime,
			})).To(Succeed())

			// Initial LLM edge (will be closed).
			closedEnd := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
			Expect(client.CreateEdge(ctx, tenantID, "ev-src", "ev-tgt", graphdb.Edge{
				ID: "ev-edge-v1", Label: "SATISFIED_BY",
				ValidFrom: baseTime, ValidTo: &closedEnd,
				DeterminedBy: "llm-job", DeterminationType: "llm_initial",
				Confidence: 0.7,
			})).To(Succeed())

			// Superseding human_feedback edge.
			Expect(client.CreateEdge(ctx, tenantID, "ev-src", "ev-tgt", graphdb.Edge{
				ID: "ev-edge-v2", Label: "SATISFIED_BY",
				ValidFrom:    closedEnd,
				DeterminedBy: "human-reviewer", DeterminationType: "human_feedback",
				Confidence: 0.99, Supersedes: "ev-edge-v1",
			})).To(Succeed())

			// Current state finds only the superseding edge.
			results, err := client.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{
				EdgeLabel: "SATISFIED_BY",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(1))
			Expect(results[0].Edge.ID).To(Equal("ev-edge-v2"))
			Expect(results[0].Edge.DeterminationType).To(Equal("human_feedback"))

			// Historical query finds the original.
			oldTime := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)
			results, err = client.QueryAsOf(ctx, tenantID, graphdb.RelationshipQuery{
				EdgeLabel: "SATISFIED_BY",
			}, oldTime)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(1))
			Expect(results[0].Edge.ID).To(Equal("ev-edge-v1"))
			Expect(results[0].Edge.DeterminationType).To(Equal("llm_initial"))
		})
	})

	// ===================================================================
	// Tenant Isolation
	// ===================================================================

	Describe("Tenant Isolation", func() {
		It("isolates graph data between tenants", func() {
			tenantA := testID("graphdb-iso-a")
			tenantB := testID("graphdb-iso-b")
			setupTenant(tenantA)
			setupTenant(tenantB)
			DeferCleanup(func() {
				cleanupTenant(tenantA)
				cleanupTenant(tenantB)
			})

			client, err := graphdb.New(testDB)
			Expect(err).NotTo(HaveOccurred())
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Microsecond)

			// Create node and edge in tenant A.
			Expect(client.CreateNode(ctx, tenantA, graphdb.Node{
				ID: "iso-src", Label: "Requirement", ValidFrom: now,
			})).To(Succeed())
			Expect(client.CreateNode(ctx, tenantA, graphdb.Node{
				ID: "iso-tgt", Label: "Document", ValidFrom: now,
			})).To(Succeed())
			Expect(client.CreateEdge(ctx, tenantA, "iso-src", "iso-tgt", graphdb.Edge{
				ID: "iso-edge", Label: "DEFINED_IN",
				ValidFrom:    now,
				DeterminedBy: "job-a", DeterminationType: "llm_initial",
				Confidence: 0.8,
			})).To(Succeed())

			// Query from tenant B: should see 0 results.
			resultsB, err := client.QueryRelationships(ctx, tenantB, graphdb.RelationshipQuery{
				EdgeLabel: "DEFINED_IN",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(resultsB).To(BeEmpty(), "tenant B sees relationships from tenant A")

			// Query from tenant A: should see 1 result.
			resultsA, err := client.QueryRelationships(ctx, tenantA, graphdb.RelationshipQuery{
				EdgeLabel: "DEFINED_IN",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(resultsA).To(HaveLen(1))
		})
	})

	// ===================================================================
	// Traversal
	// ===================================================================

	Describe("Traversal", func() {
		It("traverses multi-hop paths", func() {
			tenantID := "graphdb-traverse"
			setupTenant(tenantID)

			client, err := graphdb.New(testDB)
			Expect(err).NotTo(HaveOccurred())
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Microsecond)

			// Create chain: A -> B -> C via PARENT_OF edges.
			for _, n := range []graphdb.Node{
				{ID: "trav-a", Label: "Category", ValidFrom: now},
				{ID: "trav-b", Label: "Category", ValidFrom: now},
				{ID: "trav-c", Label: "Category", ValidFrom: now},
			} {
				Expect(client.CreateNode(ctx, tenantID, n)).To(Succeed())
			}

			Expect(client.CreateEdge(ctx, tenantID, "trav-a", "trav-b", graphdb.Edge{
				ID: "trav-ab", Label: "PARENT_OF", ValidFrom: now,
			})).To(Succeed())
			Expect(client.CreateEdge(ctx, tenantID, "trav-b", "trav-c", graphdb.Edge{
				ID: "trav-bc", Label: "PARENT_OF", ValidFrom: now,
			})).To(Succeed())

			// Traverse outbound from A with MaxDepth=2.
			paths, err := client.Traverse(ctx, tenantID, graphdb.TraversalQuery{
				StartNode:  "trav-a",
				Direction:  "outbound",
				EdgeLabels: []string{"PARENT_OF"},
				MaxDepth:   2,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(paths).NotTo(BeEmpty())

			// Verify node C was reached.
			reachedC := false
			for _, p := range paths {
				for _, n := range p.Nodes {
					if n.ID == "trav-c" {
						reachedC = true
						break
					}
				}
				if reachedC {
					break
				}
			}
			Expect(reachedC).To(BeTrue(), "Traverse did not reach node trav-c")
		})
	})

	// ===================================================================
	// Tenant Validation
	// ===================================================================

	Describe("Tenant Validation", func() {
		It("returns ErrTenantRequired for empty tenant ID", func() {
			client, err := graphdb.New(testDB)
			Expect(err).NotTo(HaveOccurred())
			ctx := context.Background()

			err = client.CreateGraph(ctx, "")
			Expect(errors.Is(err, graphdb.ErrTenantRequired)).To(BeTrue())

			err = client.CreateNode(ctx, "", graphdb.Node{
				ID: "should-fail", Label: "Requirement",
				ValidFrom: time.Now().UTC(),
			})
			Expect(errors.Is(err, graphdb.ErrTenantRequired)).To(BeTrue())
		})
	})

	// ===================================================================
	// Telemetry
	// ===================================================================

	Describe("Telemetry", func() {
		It("emits spans and metrics for graph operations", func() {
			tenantID := "graphdb-telemetry"
			setupTenant(tenantID)

			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { tp.Shutdown(context.Background()) }) //nolint:errcheck

			tracer := tp.TracerProvider().Tracer("test")
			meter := tp.MeterProvider().Meter("test")

			client, err := graphdb.New(testDB, graphdb.WithTelemetry(tracer, meter))
			Expect(err).NotTo(HaveOccurred())
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Microsecond)

			// CreateGraph
			Expect(client.CreateGraph(ctx, tenantID)).To(Succeed())

			// CreateNode x2
			Expect(client.CreateNode(ctx, tenantID, graphdb.Node{
				ID: "tel-src", Label: "Requirement", ValidFrom: now,
			})).To(Succeed())
			Expect(client.CreateNode(ctx, tenantID, graphdb.Node{
				ID: "tel-tgt", Label: "Document", ValidFrom: now,
			})).To(Succeed())

			// CreateEdge
			Expect(client.CreateEdge(ctx, tenantID, "tel-src", "tel-tgt", graphdb.Edge{
				ID: "tel-edge", Label: "DEFINED_IN", ValidFrom: now,
			})).To(Succeed())

			// QueryRelationships
			_, err = client.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{
				EdgeLabel: "DEFINED_IN",
			})
			Expect(err).NotTo(HaveOccurred())

			// Assert spans.
			spans := tp.GetSpans()
			for _, name := range []string{
				"graphdb.CreateGraph",
				"graphdb.CreateNode",
				"graphdb.CreateEdge",
				"graphdb.QueryRelationships",
			} {
				Expect(telemetrytest.FindSpan(spans, name)).NotTo(BeNil(), "expected span %q", name)
			}

			// Assert all spans carry tenant.id attribute.
			for _, s := range spans {
				_, found := telemetrytest.SpanAttribute(s, "tenant.id")
				Expect(found).To(BeTrue(), "span %q missing tenant.id attribute", s.Name())
			}

			// Assert exactly 2 CreateNode spans.
			createNodeSpans := telemetrytest.FindSpans(spans, "graphdb.CreateNode")
			Expect(createNodeSpans).To(HaveLen(2))

			// Assert metric graphdb.queries.total >= 5.
			m := telemetrytest.FindMetric(tp.GetMetrics(), "graphdb.queries.total")
			Expect(m).NotTo(BeNil(), "metric graphdb.queries.total not found")
			count, err := telemetrytest.CounterValue(m)
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(BeNumerically(">=", int64(5)))

			// Assert metric graphdb.query.duration_ms has been recorded.
			hm := telemetrytest.FindMetric(tp.GetMetrics(), "graphdb.query.duration_ms")
			Expect(hm).NotTo(BeNil(), "metric graphdb.query.duration_ms not found")
			hc, err := telemetrytest.HistogramCount(hm)
			Expect(err).NotTo(HaveOccurred())
			Expect(hc).To(BeNumerically(">=", int64(5)))
		})
	})
})
