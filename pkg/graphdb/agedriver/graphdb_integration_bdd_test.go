//go:build integration

package agedriver_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/graphdb/agedriver"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

func TestGraphDBIntegrationBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "GraphDB Integration Suite")
}

// ---------------------------------------------------------------------------
// Package-level state initialised by SynchronizedBeforeSuite
// ---------------------------------------------------------------------------

var (
	suDSN string
	// graphDSN is suDSN with the graph_user credentials, so specs connect
	// as the restricted graph role rather than the superuser.
	graphDSN string
	testDB   *sql.DB
)

// suiteDSNs carries both DSNs from node 1 to every parallel node.
type suiteDSNs struct {
	Superuser string `json:"superuser"`
	GraphUser string `json:"graph_user"`
}

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

	// Set a fresh graph_user password for this run, as test/e2e/provision
	// does, so no fixed credential lives in the repository.
	pw := make([]byte, 16)
	_, err = rand.Read(pw)
	Expect(err).NotTo(HaveOccurred(), "failed to generate graph_user password")
	password := hex.EncodeToString(pw)
	adminDB, err := sql.Open("pgx", dsn)
	Expect(err).NotTo(HaveOccurred(), "failed to open admin connection")
	// ALTER ROLE takes no bound parameters. Interpolation is safe because
	// password is hex, which cannot contain a quote.
	_, err = adminDB.ExecContext(ctx, fmt.Sprintf("ALTER ROLE graph_user WITH PASSWORD '%s'", password))
	Expect(err).NotTo(HaveOccurred(), "failed to set graph_user password")
	Expect(adminDB.Close()).To(Succeed(), "failed to close admin connection")

	u, err := url.Parse(dsn)
	Expect(err).NotTo(HaveOccurred(), "failed to parse DSN")
	u.User = url.UserPassword("graph_user", password)
	data, err := json.Marshal(suiteDSNs{Superuser: dsn, GraphUser: u.String()})
	Expect(err).NotTo(HaveOccurred(), "failed to encode suite DSNs")
	return data
}, func(data []byte) {
	// Runs on all nodes: store the DSNs and open a graph_user connection.
	var dsns suiteDSNs
	Expect(json.Unmarshal(data, &dsns)).To(Succeed(), "failed to decode suite DSNs")
	suDSN, graphDSN = dsns.Superuser, dsns.GraphUser

	var err error
	testDB, err = sql.Open("pgx", graphDSN)
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
	suDB, err := db.NewPool(db.PoolConfig{DSN: suDSN})
	Expect(err).NotTo(HaveOccurred(), "open superuser pool")
	DeferCleanup(func() { suDB.Close() })

	err = db.EnsureTenant(context.Background(), suDB, tenantID, "Test Tenant "+tenantID)
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

// seedLegacyNode writes a vertex with raw Cypher as graph_user, bypassing
// CreateNode's graph-wide ID check, to reproduce a graph written before
// #148, when nodes under different labels could share an ID. label and id
// are test literals that need no escaping.
func seedLegacyNode(ctx context.Context, gn, label, id string) {
	tx, err := testDB.BeginTx(ctx, nil)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `SET LOCAL search_path = ag_catalog, "$user", public`)
	Expect(err).NotTo(HaveOccurred())
	_, err = tx.ExecContext(ctx, fmt.Sprintf(
		"SELECT * FROM ag_catalog.cypher('%s', $$ CREATE (n:%s {id: '%s', valid_from: '%s'}) $$) AS (v agtype)",
		gn, label, id, graphdb.FormatTime(time.Now().UTC())))
	Expect(err).NotTo(HaveOccurred(), "seed legacy %s node %q", label, id)
	Expect(tx.Commit()).To(Succeed())
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

			client, err := agedriver.New(testDB)
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

			client, err := agedriver.New(testDB)
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

			client, err := agedriver.New(testDB)
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

				client, err := agedriver.New(testDB)
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

				client, err := agedriver.New(testDB)
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

			client, err := agedriver.New(testDB)
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

			client, err := agedriver.New(testDB)
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

			client, err := agedriver.New(testDB)
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
	// ExecuteQuery
	// ===================================================================

	Describe("ExecuteQuery", func() {
		var (
			client   graphdb.GraphDB
			tenantID string
			ctx      context.Context
		)

		BeforeEach(func() {
			tenantID = testID("graphdb-execq")
			setupTenant(tenantID)
			DeferCleanup(func() { cleanupTenant(tenantID) })

			var err error
			client, err = agedriver.New(testDB)
			Expect(err).NotTo(HaveOccurred())
			ctx = context.Background()

			Expect(client.CreateGraph(ctx, tenantID)).To(Succeed())
		})

		It("returns query rows for a parameterless MATCH", func() {
			rows, err := client.ExecuteQuery(ctx, tenantID, "MATCH (n) RETURN n", nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(rows).To(BeEmpty())
		})

		It("substitutes named parameters and matches only the exact node, not a shared-prefix neighbor", func() {
			now := time.Now().UTC().Truncate(time.Microsecond)
			Expect(client.CreateNode(ctx, tenantID, graphdb.Node{
				ID: "q-1", Label: "Requirement", ValidFrom: now,
			})).To(Succeed())
			Expect(client.CreateNode(ctx, tenantID, graphdb.Node{
				ID: "q-10", Label: "Requirement", ValidFrom: now,
			})).To(Succeed())

			rows, err := client.ExecuteQuery(
				ctx, tenantID,
				"MATCH (n {id: $id}) RETURN n",
				map[string]string{"id": "q-1", "id2": "other"},
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(rows).To(HaveLen(1))
			Expect(rows[0].Values).To(HaveLen(1))
			Expect(rows[0].Values[0].Type).To(Equal(graphdb.QueryValueNode))
			Expect(rows[0].Values[0].NodeVal).NotTo(BeNil())
			Expect(rows[0].Values[0].NodeVal.ID).To(Equal("q-1"))
		})

		It("rejects a writing query with ErrReadOnlyViolation, an actionable message and no PostgreSQL text, and keeps the cause on the span", func() {
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = tp.Shutdown(context.Background()) })
			traced, err := agedriver.New(testDB, agedriver.WithTelemetry(tp.TracerProvider().Tracer("test"), tp.MeterProvider().Meter("test")))
			Expect(err).NotTo(HaveOccurred())

			_, err = traced.ExecuteQuery(ctx, tenantID, "CREATE (n:Requirement {id: 'w'}) RETURN n", nil)
			Expect(err).To(MatchError(graphdb.ErrReadOnlyViolation))
			Expect(err.Error()).To(Equal("execute query: read-only transaction violation: the query writes to the graph, but ExecuteQuery is read-only; write with CreateNode, CreateEdge, BulkCreateEdges or SupersedeFact instead"))

			Expect(spanExceptionMessages(tp, "graphdb.ExecuteQuery")).To(ContainElement(ContainSubstring("read-only transaction")),
				"operators find PostgreSQL's text on the span")
			_, err = client.GetNode(ctx, tenantID, "w")
			Expect(err).To(MatchError(graphdb.ErrNodeNotFound), "the write must not have happened")
		})

		It("substitutes a parameter value containing a single quote", func() {
			now := time.Now().UTC().Truncate(time.Microsecond)
			Expect(client.CreateNode(ctx, tenantID, graphdb.Node{
				ID: "o'brien", Label: "Requirement", ValidFrom: now,
			})).To(Succeed())

			rows, err := client.ExecuteQuery(
				ctx, tenantID,
				"MATCH (n {id: $id}) RETURN n",
				map[string]string{"id": "o'brien"},
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(rows).To(HaveLen(1))
			Expect(rows[0].Values).To(HaveLen(1))
			Expect(rows[0].Values[0].NodeVal).NotTo(BeNil())
			Expect(rows[0].Values[0].NodeVal.ID).To(Equal("o'brien"))
		})
	})

	// ===================================================================
	// GetEdge
	// ===================================================================

	Describe("GetEdge", func() {
		var (
			client   graphdb.GraphDB
			tenantID string
			ctx      context.Context
		)

		BeforeEach(func() {
			tenantID = testID("graphdb-getedge")
			setupTenant(tenantID)
			DeferCleanup(func() { cleanupTenant(tenantID) })

			var err error
			client, err = agedriver.New(testDB)
			Expect(err).NotTo(HaveOccurred())
			ctx = context.Background()

			Expect(client.CreateGraph(ctx, tenantID)).To(Succeed())
		})

		It("returns the edge with source and target IDs", func() {
			now := time.Now().UTC().Truncate(time.Microsecond)

			Expect(client.CreateNode(ctx, tenantID, graphdb.Node{
				ID:             "ge-src",
				Label:          "Requirement",
				ValidFrom:      now,
				CreatedBy:      "test-job",
				CreationMethod: "import",
			})).To(Succeed())
			Expect(client.CreateNode(ctx, tenantID, graphdb.Node{
				ID:             "ge-tgt",
				Label:          "Document",
				ValidFrom:      now,
				CreatedBy:      "test-job",
				CreationMethod: "import",
			})).To(Succeed())
			Expect(client.CreateEdge(ctx, tenantID, "ge-src", "ge-tgt", graphdb.Edge{
				ID:                "ge-edge-001",
				Label:             "DEFINED_IN",
				ValidFrom:         now,
				DeterminedBy:      "test-job",
				DeterminationType: "llm_initial",
				Confidence:        0.9,
			})).To(Succeed())

			result, err := client.GetEdge(ctx, tenantID, "ge-edge-001")
			Expect(err).NotTo(HaveOccurred())
			Expect(result).NotTo(BeNil())
			Expect(result.Edge.ID).To(Equal("ge-edge-001"))
			Expect(result.Edge.Label).To(Equal("DEFINED_IN"))
			Expect(result.SourceID).To(Equal("ge-src"))
			Expect(result.TargetID).To(Equal("ge-tgt"))
		})

		It("returns ErrEdgeNotFound for a nonexistent edge ID", func() {
			_, err := client.GetEdge(ctx, tenantID, "nonexistent-edge-id")
			Expect(errors.Is(err, graphdb.ErrEdgeNotFound)).To(BeTrue())
		})
	})

	// ===================================================================
	// Tenant Validation
	// ===================================================================

	Describe("Tenant Validation", func() {
		It("returns ErrTenantRequired for empty tenant ID", func() {
			client, err := agedriver.New(testDB)
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

			client, err := agedriver.New(testDB, agedriver.WithTelemetry(tracer, meter))
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

			// UpsertNode: create, then update in place.
			for _, wantCreated := range []bool{true, false} {
				created, err := client.UpsertNode(ctx, tenantID, graphdb.Node{
					ID: "tel-upsert", Label: "Requirement", ValidFrom: now,
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(created).To(Equal(wantCreated))
			}

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

			// Assert all spans carry the validated tenant.id attribute.
			for _, s := range spans {
				attr, found := telemetrytest.SpanAttribute(s, "tenant.id")
				Expect(found).To(BeTrue(), "span %q missing tenant.id attribute", s.Name())
				Expect(attr.AsString()).To(Equal(tenantID), "span %q", s.Name())
			}

			// Assert exactly 2 CreateNode spans.
			createNodeSpans := telemetrytest.FindSpans(spans, "graphdb.CreateNode")
			Expect(createNodeSpans).To(HaveLen(2))

			upsertSpans := telemetrytest.FindSpans(spans, "graphdb.UpsertNode")
			Expect(upsertSpans).To(HaveLen(2))
			for _, s := range upsertSpans {
				Expect(s.Status().Code.String()).To(Equal("Ok"))
			}

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

// ---------------------------------------------------------------------------
// First-time graph creation
// ---------------------------------------------------------------------------

// advisoryWaiters returns how many sessions in the current database are
// waiting, not yet granted, for the advisory lock with the given bigint key.
// PostgreSQL shows a bigint key's high 32 bits in classid, its low 32 bits in
// objid, and sets objsubid to 1. Any role can read pg_locks, so conn may be
// the restricted graph_user pool or the superuser one. Lock-wait specs wait
// for this count instead of inferring "blocked" from a goroutine that has not
// returned yet, which a slow scheduler would also produce. Each poll is
// bounded to one second, so a saturated pool returns an error, which
// Eventually retries, instead of blocking the poll forever.
func advisoryWaiters(ctx context.Context, conn *sql.DB, key int64) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var n int64
	err := conn.QueryRowContext(ctx, `SELECT count(*) FROM pg_locks
		WHERE locktype = 'advisory' AND NOT granted
		  AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND classid::bigint = ($1::bigint >> 32) & 4294967295
		  AND objid::bigint = $1::bigint & 4294967295
		  AND objsubid = 1`, key).Scan(&n)
	return n, err
}

var _ = Describe("CreateGraph for a missing graph", func() {
	var ctx context.Context

	BeforeEach(func() { ctx = context.Background() })

	graphCount := func(su *sql.DB, gn string) int {
		var n int
		Expect(su.QueryRowContext(ctx, "SELECT count(*) FROM ag_catalog.ag_graph WHERE name = $1", gn).Scan(&n)).To(Succeed())
		return n
	}

	It("is refused for the restricted graph_user, which relies on the tenant trigger to create graphs", func() {
		tenantID := testID("nograph")
		su, err := sql.Open("pgx", suDSN)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = su.Close() })

		client, err := agedriver.New(testDB)
		Expect(err).NotTo(HaveOccurred())
		err = client.CreateGraph(ctx, tenantID)
		Expect(err).To(MatchError(ContainSubstring("permission denied")))
		Expect(graphCount(su, agedriver.GraphName(tenantID))).To(Equal(0), "nothing was created")
	})

	It("succeeds for every one of several concurrent first-time calls by a role that may create graphs", func() {
		tenantID := testID("newgraph")
		gn := agedriver.GraphName(tenantID)
		su, err := sql.Open("pgx", suDSN)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = su.Close() })
		DeferCleanup(func() {
			if graphCount(su, gn) > 0 {
				_, err := su.ExecContext(context.Background(), "SELECT ag_catalog.drop_graph($1, true)", gn)
				Expect(err).NotTo(HaveOccurred())
			}
		})
		Expect(graphCount(su, gn)).To(Equal(0))

		client, err := agedriver.New(su)
		Expect(err).NotTo(HaveOccurred())
		errs := make([]error, 8)
		var wg sync.WaitGroup
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = client.CreateGraph(ctx, tenantID)
			}()
		}
		wg.Wait()
		for _, err := range errs {
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(graphCount(su, gn)).To(Equal(1))
	})

	It("waits for a concurrent creator holding the graph lock, then returns nil instead of failing on the duplicate", func() {
		tenantID := testID("lockgraph")
		gn := agedriver.GraphName(tenantID)
		su, err := sql.Open("pgx", suDSN)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = su.Close() })
		DeferCleanup(func() {
			if graphCount(su, gn) > 0 {
				_, err := su.ExecContext(context.Background(), "SELECT ag_catalog.drop_graph($1, true)", gn)
				Expect(err).NotTo(HaveOccurred())
			}
		})

		holder, err := su.BeginTx(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = holder.Rollback() })
		_, err = holder.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", agedriver.AdvisoryKey("graph", gn))
		Expect(err).NotTo(HaveOccurred())
		_, err = holder.ExecContext(ctx, "SELECT ag_catalog.create_graph($1)", gn)
		Expect(err).NotTo(HaveOccurred())

		client, err := agedriver.New(su)
		Expect(err).NotTo(HaveOccurred())
		done := make(chan error, 1)
		go func() { done <- client.CreateGraph(ctx, tenantID) }()
		Eventually(func() (int64, error) {
			select {
			case err := <-done:
				StopTrying("CreateGraph returned before waiting on the graph lock").Wrap(err).Now()
			default:
			}
			return advisoryWaiters(ctx, su, agedriver.AdvisoryKey("graph", gn))
		}, 10*time.Second, 20*time.Millisecond).
			Should(Equal(int64(1)), "CreateGraph never waited on the graph lock")
		Expect(done).NotTo(Receive(), "CreateGraph returned while the lock was held")

		Expect(holder.Commit()).To(Succeed())
		var got error
		Eventually(done, 10*time.Second).Should(Receive(&got))
		Expect(got).NotTo(HaveOccurred())
		Expect(graphCount(su, gn)).To(Equal(1))
	})
})

// ---------------------------------------------------------------------------
// Node creation lock
// ---------------------------------------------------------------------------

// nodeWriteResult is what a CreateNode or UpsertNode goroutine reports.
type nodeWriteResult struct {
	created bool
	err     error
}

var _ = Describe("Node creation lock", func() {
	const nodeID = "lock-node"
	var (
		ctx      context.Context
		tenantID string
		gn       string
		validNow time.Time
	)

	BeforeEach(func() {
		ctx = context.Background()
		tenantID = testID("lock")
		setupTenant(tenantID)
		DeferCleanup(func() { cleanupTenant(tenantID) })
		gn = agedriver.GraphName(tenantID)
		validNow = time.Now().UTC().Truncate(time.Microsecond)
	})

	// nodeKey is the advisory lock key CreateNode and UpsertNode take for nodeID.
	nodeKey := func() int64 { return agedriver.AdvisoryKey("node", gn, nodeID) }

	// holdLock opens a graph_user transaction on testDB that holds the
	// advisory lock CreateNode and UpsertNode take for nodeID.
	holdLock := func() *sql.Tx {
		tx, err := testDB.BeginTx(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = tx.Rollback() })
		_, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", nodeKey())
		Expect(err).NotTo(HaveOccurred())
		return tx
	}

	// awaitBothWaiting waits until UpsertNode and CreateNode are both queued
	// on the node lock, then checks neither has returned. A writer that
	// returns first stops the wait at once with that writer's error.
	awaitBothWaiting := func(upsert, create chan nodeWriteResult) {
		Eventually(func() (int64, error) {
			select {
			case r := <-upsert:
				StopTrying("UpsertNode returned before waiting on the node lock").Wrap(r.err).Now()
			case r := <-create:
				StopTrying("CreateNode returned before waiting on the node lock").Wrap(r.err).Now()
			default:
			}
			return advisoryWaiters(ctx, testDB, nodeKey())
		}, 10*time.Second, 20*time.Millisecond).
			Should(Equal(int64(2)), "UpsertNode and CreateNode never both waited on the node lock")
		Expect(upsert).NotTo(Receive(), "UpsertNode returned while the lock was held")
		Expect(create).NotTo(Receive(), "CreateNode returned while the lock was held")
	}

	countNodes := func() int64 {
		tx, err := testDB.BeginTx(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = tx.Rollback() }()
		_, err = tx.ExecContext(ctx, `SET search_path = ag_catalog, "$user", public`)
		Expect(err).NotTo(HaveOccurred())
		var raw string
		Expect(tx.QueryRowContext(ctx, fmt.Sprintf(
			"SELECT * FROM ag_catalog.cypher('%s', $$ MATCH (n:Control {id: '%s'}) RETURN count(n) $$) AS (c agtype)", gn, nodeID,
		)).Scan(&raw)).To(Succeed())
		var n int64
		_, err = fmt.Sscan(raw, &n)
		Expect(err).NotTo(HaveOccurred())
		return n
	}

	// startWriters runs UpsertNode and CreateNode for nodeID concurrently on
	// client and returns their result channels.
	startWriters := func(client graphdb.GraphDB) (upsert, create chan nodeWriteResult) {
		upsert, create = make(chan nodeWriteResult, 1), make(chan nodeWriteResult, 1)
		node := graphdb.Node{ID: nodeID, Label: "Control", ValidFrom: validNow}
		go func() {
			created, err := client.UpsertNode(ctx, tenantID, node)
			upsert <- nodeWriteResult{created: created, err: err}
		}()
		go func() {
			err := client.CreateNode(ctx, tenantID, node)
			create <- nodeWriteResult{created: err == nil, err: err}
		}()
		return upsert, create
	}

	It("blocks UpsertNode and CreateNode while another transaction holds the node's lock, then creates exactly one node", func() {
		client, err := agedriver.New(testDB)
		Expect(err).NotTo(HaveOccurred())
		holder := holdLock()

		upsert, create := startWriters(client)
		awaitBothWaiting(upsert, create)

		Expect(holder.Rollback()).To(Succeed())
		var up, cr nodeWriteResult
		Eventually(upsert, 10*time.Second).Should(Receive(&up))
		Eventually(create, 10*time.Second).Should(Receive(&cr))
		Expect(up.err).NotTo(HaveOccurred())
		if up.created {
			Expect(cr.err).To(MatchError(graphdb.ErrNodeExists), "UpsertNode created the node first")
		} else {
			Expect(cr.err).NotTo(HaveOccurred(), "CreateNode created the node first")
		}
		Expect(countNodes()).To(Equal(int64(1)))
	})

	It("still sees a node committed during the lock wait when the session default is REPEATABLE READ", func() {
		u, err := url.Parse(graphDSN)
		Expect(err).NotTo(HaveOccurred())
		q := u.Query()
		q.Set("default_transaction_isolation", "repeatable read")
		u.RawQuery = q.Encode()
		rrDB, err := sql.Open("pgx", u.String())
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = rrDB.Close() })
		var level string
		Expect(rrDB.QueryRowContext(ctx, "SHOW default_transaction_isolation").Scan(&level)).To(Succeed())
		Expect(level).To(Equal("repeatable read"), "the spec needs a REPEATABLE READ session default")
		rrClient, err := agedriver.New(rrDB)
		Expect(err).NotTo(HaveOccurred())

		// The holder creates the node under the lock and commits only after
		// both writers are waiting on it.
		holder := holdLock()
		_, err = holder.ExecContext(ctx, `SET search_path = ag_catalog, "$user", public`)
		Expect(err).NotTo(HaveOccurred())
		_, err = holder.ExecContext(ctx, "SELECT set_config('app.current_tenant', $1, true)", tenantID)
		Expect(err).NotTo(HaveOccurred())
		_, err = holder.ExecContext(ctx, fmt.Sprintf(
			"SELECT * FROM ag_catalog.cypher('%s', $$ CREATE (n:Control {id: '%s', valid_from: '%s'}) $$) AS (v agtype)",
			gn, nodeID, graphdb.FormatTime(validNow)))
		Expect(err).NotTo(HaveOccurred())

		upsert, create := startWriters(rrClient)
		awaitBothWaiting(upsert, create)
		Expect(holder.Commit()).To(Succeed())

		var up, cr nodeWriteResult
		Eventually(upsert, 10*time.Second).Should(Receive(&up))
		Eventually(create, 10*time.Second).Should(Receive(&cr))
		Expect(up.err).NotTo(HaveOccurred())
		Expect(up.created).To(BeFalse(), "UpsertNode must see the committed node and update it")
		Expect(cr.err).To(MatchError(graphdb.ErrNodeExists))
		Expect(countNodes()).To(Equal(int64(1)))
	})
})

// ---------------------------------------------------------------------------
// Shared GraphDB contract
// ---------------------------------------------------------------------------

var _ = Describe("GraphDB contract (agedriver)", testspecs.GraphDBContractBehavior(func() (graphdb.GraphDB, string, string) {
	tenantID := testID("contract")
	setupTenant(tenantID)
	DeferCleanup(func() { cleanupTenant(tenantID) })

	otherID := testID("contract-other")
	setupTenant(otherID)
	DeferCleanup(func() { cleanupTenant(otherID) })

	client, err := agedriver.New(testDB)
	Expect(err).NotTo(HaveOccurred())
	return client, tenantID, otherID
}))

// ---------------------------------------------------------------------------
// Graphs written before #148
// ---------------------------------------------------------------------------

var _ = Describe("Graphs written before #148 (agedriver)", func() {
	const ambiguousDup = `ambiguous node ID "dup": 2 nodes share it. Node IDs have been unique per graph since #148, so this graph was written before then; rebuild the tenant's graph (see "Upgrade note (#148)" in docs/dev/design-principles.md)`

	var (
		ctx      context.Context
		tenantID string
		client   graphdb.GraphDB
		now      time.Time
	)

	BeforeEach(func() {
		ctx = context.Background()
		tenantID = testID("legacy")
		setupTenant(tenantID)
		DeferCleanup(func() { cleanupTenant(tenantID) })
		var err error
		client, err = agedriver.New(testDB)
		Expect(err).NotTo(HaveOccurred())
		now = time.Now().UTC().Truncate(time.Microsecond)
		for _, id := range []string{"dup", "b"} {
			Expect(client.CreateNode(ctx, tenantID, graphdb.Node{ID: id, Label: "Control", ValidFrom: now})).To(Succeed())
		}
		seedLegacyNode(ctx, agedriver.GraphName(tenantID), "Artifact", "dup")
	})

	DescribeTable("rejects an edge whose endpoint ID several nodes share, names the rebuild, and stores nothing",
		func(write func() error, role string) {
			err := write()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(role + ": " + ambiguousDup))
			rels, err := client.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{})
			Expect(err).NotTo(HaveOccurred())
			Expect(rels).To(BeEmpty())
		},
		Entry("CreateEdge", func() error {
			return client.CreateEdge(ctx, tenantID, "dup", "b", graphdb.Edge{ID: "e1", Label: "MAPS", ValidFrom: now})
		}, "source"),
		Entry("CreateRequiresEdge", func() error {
			return client.CreateRequiresEdge(ctx, tenantID, graphdb.RequiresEdge{SourceID: "b", TargetID: "dup", AnalyzedAt: now, JobID: "job-1"})
		}, "target"),
	)

	// Even with a same-label node present, the other label's node makes the
	// write a conflict, never ErrNodeExists, which graph writers treat as
	// success.
	DescribeTable("rejects a node write on an ID another label also holds and changes neither node",
		func(write func(graphdb.Node) error) {
			err := write(graphdb.Node{ID: "dup", Label: "Control", ValidFrom: now, Properties: map[string]any{"title": "new"}})
			Expect(err).To(MatchError(graphdb.ErrNodeIDConflict))
			Expect(errors.Is(err, graphdb.ErrNodeExists)).To(BeFalse(), "graph writers treat ErrNodeExists as success")
			var conflict *graphdb.NodeIDConflictError
			Expect(errors.As(err, &conflict)).To(BeTrue())
			Expect(*conflict).To(Equal(graphdb.NodeIDConflictError{ID: "dup", Label: "Control", StoredLabel: "Artifact"}))

			rows, err := client.ExecuteQuery(ctx, tenantID, "MATCH (n {id: 'dup'}) RETURN n", nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(rows).To(HaveLen(2))
			for _, row := range rows {
				Expect(row.Values).To(HaveLen(1))
				Expect(row.Values[0].NodeVal).NotTo(BeNil())
				Expect(row.Values[0].NodeVal.Properties).NotTo(HaveKey("title"))
			}
		},
		Entry("CreateNode", func(n graphdb.Node) error { return client.CreateNode(ctx, tenantID, n) }),
		Entry("UpsertNode", func(n graphdb.Node) error {
			_, err := client.UpsertNode(ctx, tenantID, n)
			return err
		}),
	)
})

var _ = Describe("AGE missing-graph error", func() {
	It("matches the message classifyErr recognizes", func() {
		ctx := context.Background()
		tx, err := testDB.BeginTx(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = tx.Rollback() })

		_, err = tx.ExecContext(ctx, `SET search_path = ag_catalog, "$user", public`)
		Expect(err).NotTo(HaveOccurred())

		_, err = tx.QueryContext(ctx,
			"SELECT * FROM ag_catalog.cypher('crosscodex_never_created', $$ MATCH (n) RETURN n $$) AS (v agtype)")
		Expect(err).To(MatchError(MatchRegexp(`graph "crosscodex_never_created" does not exist`)))
		var pgErr *pgconn.PgError
		Expect(errors.As(err, &pgErr)).To(BeTrue(), "database/sql over pgx returns AGE's error as a *pgconn.PgError")
		// classifyErr matches the message, not this code; pinning both makes
		// an AGE upgrade that changes either fail here first.
		Expect(pgErr.Code).To(Equal("3F000"))
		Expect(agedriver.ClassifyErr(ctx, "never_created", err)).To(MatchError(graphdb.ErrGraphNotFound))
	})

	It("classifies the missing-graph error for the longest allowed tenant ID, whose graph name fills PostgreSQL's 63-byte identifier limit exactly", func() {
		// "crosscodex_" (11 bytes) + a 52-byte tenant ID = 63 bytes, the
		// longest identifier PostgreSQL keeps untruncated. Tenant IDs are
		// capped at 52 characters for exactly this reason, so AGE must
		// report the full graph name and classifyErr must still recognize it.
		tenantID := "t" + strings.Repeat("a", 50) + "1"
		Expect(tenantID).To(HaveLen(52))
		Expect(tenant.ValidateTenantID(tenantID)).To(Succeed())

		tp, err := telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = tp.Shutdown(context.Background()) })
		client, err := agedriver.New(testDB, agedriver.WithTelemetry(tp.TracerProvider().Tracer("test"), tp.MeterProvider().Meter("test")))
		Expect(err).NotTo(HaveOccurred())

		_, err = client.GetNode(context.Background(), tenantID, "any-id")
		Expect(err).To(MatchError(graphdb.ErrGraphNotFound))
		Expect(err.Error()).To(ContainSubstring(tenantID))
		Expect(err.Error()).NotTo(ContainSubstring("crosscodex_"), "the internal graph name must not reach the caller")
		Expect(err.Error()).NotTo(ContainSubstring("SQLSTATE"))
		Expect(spanExceptionMessages(tp, "graphdb.GetNode")).To(ContainElement(ContainSubstring(`graph "crosscodex_`+tenantID+`" does not exist`)),
			"AGE must report the untruncated 63-byte graph name, and the span must keep it for operators")
	})
})

// spanExceptionMessages returns the exception.message of every error event
// recorded on the spans named name.
func spanExceptionMessages(tp *telemetrytest.TestProvider, name string) []string {
	var msgs []string
	for _, s := range telemetrytest.FindSpans(tp.GetSpans(), name) {
		for _, ev := range s.Events() {
			for _, kv := range ev.Attributes {
				if kv.Key == "exception.message" {
					msgs = append(msgs, kv.Value.AsString())
				}
			}
		}
	}
	return msgs
}
