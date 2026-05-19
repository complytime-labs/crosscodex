//go:build integration

package graphdb_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
)

var (
	suDSN  string
	testDB *sql.DB
)

func TestMain(m *testing.M) {
	ctx := context.Background()

	suDSN = os.Getenv("TEST_DATABASE_DSN")
	if suDSN == "" {
		fmt.Fprintln(os.Stderr, "TEST_DATABASE_DSN not set — run: task dev:test-integration")
		os.Exit(1)
	}

	// Run migrations.
	migrator, err := db.NewMigrator(suDSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create migrator: %v\n", err)
		os.Exit(1)
	}
	if err := migrator.Up(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "failed to run migrations: %v\n", err)
		os.Exit(1)
	}
	if err := migrator.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to close migrator: %v\n", err)
		os.Exit(1)
	}

	// Set up graph_user — a dedicated role that owns per-tenant graph schemas.
	// This is NOT app_user: graph operations require schema ownership for AGE's
	// internal DDL, which is too much privilege for the relational RLS role.
	// See pkg/db/doc.go and migration 009 for the full security model.
	adminDB, err := sql.Open("pgx", suDSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to open admin connection: %v\n", err)
		os.Exit(1)
	}
	if _, err := adminDB.ExecContext(ctx, "ALTER ROLE graph_user WITH PASSWORD 'graphpass'"); err != nil {
		fmt.Fprintf(os.Stderr, "failed to set graph_user password: %v\n", err)
		os.Exit(1)
	}
	if err := adminDB.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to close admin connection: %v\n", err)
		os.Exit(1)
	}

	// Open as graph_user.
	u, err := url.Parse(suDSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to parse DSN: %v\n", err)
		os.Exit(1)
	}
	u.User = url.UserPassword("graph_user", "graphpass")

	testDB, err = sql.Open("pgx", u.String())
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to open graph_user connection: %v\n", err)
		os.Exit(1)
	}
	testDB.SetMaxOpenConns(5)
	defer testDB.Close()

	os.Exit(m.Run())
}

func setupTenant(t *testing.T, tenantID string) {
	t.Helper()
	suDB, err := sql.Open("pgx", suDSN)
	if err != nil {
		t.Fatalf("open superuser conn: %v", err)
	}
	defer suDB.Close()

	_, err = suDB.ExecContext(context.Background(),
		"INSERT INTO tenants (tenant_id, display_name) VALUES ($1, $2) ON CONFLICT DO NOTHING",
		tenantID, "Test Tenant "+tenantID)
	if err != nil {
		t.Fatalf("setupTenant(%q): %v", tenantID, err)
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestIntegration_CreateGraph_Idempotent(t *testing.T) {
	tenantID := "graphdb-idempotent"
	setupTenant(t, tenantID)

	client := graphdb.New(testDB)
	ctx := context.Background()

	// Graph already exists via the tenant-insert trigger.
	// Calling CreateGraph again must not error.
	if err := client.CreateGraph(ctx, tenantID); err != nil {
		t.Fatalf("second CreateGraph: %v", err)
	}
	// A third call must also succeed.
	if err := client.CreateGraph(ctx, tenantID); err != nil {
		t.Fatalf("third CreateGraph: %v", err)
	}
}

func TestIntegration_CreateNode(t *testing.T) {
	tenantID := "graphdb-create-node"
	setupTenant(t, tenantID)

	client := graphdb.New(testDB)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Microsecond)
	node := graphdb.Node{
		ID:             "req-001",
		Label:          "Requirement",
		ValidFrom:      now,
		CreatedBy:      "test-job",
		CreationMethod: "import",
		Properties:     map[string]any{"framework": "NIST-800-53"},
	}
	if err := client.CreateNode(ctx, tenantID, node); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}

	// Verify graph is queryable (no edges yet, so expect empty results but no error).
	results, err := client.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{
		SourceLabel: "Requirement",
	})
	if err != nil {
		t.Fatalf("QueryRelationships after CreateNode: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 relationships (no edges yet), got %d", len(results))
	}
}

func TestIntegration_CreateEdge(t *testing.T) {
	tenantID := "graphdb-create-edge"
	setupTenant(t, tenantID)

	client := graphdb.New(testDB)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	// Create source node with full temporal and provenance attributes.
	if err := client.CreateNode(ctx, tenantID, graphdb.Node{
		ID:             "src-001",
		Label:          "Requirement",
		ValidFrom:      now,
		CreatedBy:      "import-job-7",
		CreationMethod: "oscal-import",
		Properties:     map[string]any{"framework": "NIST-800-53"},
	}); err != nil {
		t.Fatalf("CreateNode source: %v", err)
	}

	// Create target node.
	if err := client.CreateNode(ctx, tenantID, graphdb.Node{
		ID:             "tgt-001",
		Label:          "Document",
		ValidFrom:      now,
		CreatedBy:      "catalog-loader",
		CreationMethod: "bulk-import",
	}); err != nil {
		t.Fatalf("CreateNode target: %v", err)
	}

	// Create edge with full temporal and provenance attributes.
	if err := client.CreateEdge(ctx, tenantID, graphdb.Edge{
		ID:                "edge-001",
		Label:             "DEFINED_IN",
		Source:            "src-001",
		Target:            "tgt-001",
		ValidFrom:         now,
		DeterminedBy:      "job-1",
		DeterminationType: "llm_initial",
		Confidence:        0.85,
	}); err != nil {
		t.Fatalf("CreateEdge: %v", err)
	}

	// Query back and verify all temporal/provenance attributes round-trip.
	results, err := client.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{
		EdgeLabel: "DEFINED_IN",
	})
	if err != nil {
		t.Fatalf("QueryRelationships: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 relationship, got %d", len(results))
	}

	rel := results[0]

	// Edge assertions.
	if rel.Edge.Label != "DEFINED_IN" {
		t.Errorf("edge label = %q, want DEFINED_IN", rel.Edge.Label)
	}
	if rel.Edge.ID != "edge-001" {
		t.Errorf("edge id = %q, want edge-001", rel.Edge.ID)
	}
	if rel.Edge.DeterminedBy != "job-1" {
		t.Errorf("determined_by = %q, want job-1", rel.Edge.DeterminedBy)
	}
	if rel.Edge.DeterminationType != "llm_initial" {
		t.Errorf("determination_type = %q, want llm_initial", rel.Edge.DeterminationType)
	}
	if rel.Edge.Confidence != 0.85 {
		t.Errorf("confidence = %g, want 0.85", rel.Edge.Confidence)
	}
	if rel.Edge.ValidFrom.IsZero() {
		t.Error("edge valid_from is zero, want non-zero")
	}
	if rel.Edge.ValidTo != nil {
		t.Errorf("edge valid_to = %v, want nil (current)", rel.Edge.ValidTo)
	}
	if rel.Edge.Source != "src-001" {
		t.Errorf("edge source = %q, want src-001", rel.Edge.Source)
	}
	if rel.Edge.Target != "tgt-001" {
		t.Errorf("edge target = %q, want tgt-001", rel.Edge.Target)
	}

	// Source node assertions.
	if rel.Source.Label != "Requirement" {
		t.Errorf("source label = %q, want Requirement", rel.Source.Label)
	}
	if rel.Source.ID != "src-001" {
		t.Errorf("source id = %q, want src-001", rel.Source.ID)
	}
	if rel.Source.CreatedBy != "import-job-7" {
		t.Errorf("source created_by = %q, want import-job-7", rel.Source.CreatedBy)
	}
	if rel.Source.CreationMethod != "oscal-import" {
		t.Errorf("source creation_method = %q, want oscal-import", rel.Source.CreationMethod)
	}
	if rel.Source.ValidFrom.IsZero() {
		t.Error("source valid_from is zero, want non-zero")
	}
	if v, ok := rel.Source.Properties["framework"]; !ok || v != "NIST-800-53" {
		t.Errorf("source properties[framework] = %v, want NIST-800-53", v)
	}

	// Target node assertions.
	if rel.Target.Label != "Document" {
		t.Errorf("target label = %q, want Document", rel.Target.Label)
	}
	if rel.Target.ID != "tgt-001" {
		t.Errorf("target id = %q, want tgt-001", rel.Target.ID)
	}
	if rel.Target.CreatedBy != "catalog-loader" {
		t.Errorf("target created_by = %q, want catalog-loader", rel.Target.CreatedBy)
	}
	if rel.Target.CreationMethod != "bulk-import" {
		t.Errorf("target creation_method = %q, want bulk-import", rel.Target.CreationMethod)
	}
}

func TestIntegration_QueryAsOf(t *testing.T) {
	tenantID := "graphdb-query-asof"
	setupTenant(t, tenantID)

	client := graphdb.New(testDB)
	ctx := context.Background()

	baseTime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	// Create two nodes.
	if err := client.CreateNode(ctx, tenantID, graphdb.Node{
		ID:        "asof-src",
		Label:     "Requirement",
		ValidFrom: baseTime,
	}); err != nil {
		t.Fatalf("CreateNode source: %v", err)
	}
	if err := client.CreateNode(ctx, tenantID, graphdb.Node{
		ID:        "asof-tgt",
		Label:     "Document",
		ValidFrom: baseTime,
	}); err != nil {
		t.Fatalf("CreateNode target: %v", err)
	}

	// Create old edge: valid 2025-01-01 to 2025-06-01.
	oldValidTo := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	if err := client.CreateEdge(ctx, tenantID, graphdb.Edge{
		ID:                "asof-edge-old",
		Label:             "MAPS_TO",
		Source:            "asof-src",
		Target:            "asof-tgt",
		ValidFrom:         baseTime,
		ValidTo:           &oldValidTo,
		DeterminedBy:      "job-old",
		DeterminationType: "llm_initial",
		Confidence:        0.7,
	}); err != nil {
		t.Fatalf("CreateEdge old: %v", err)
	}

	// Create new edge: valid from 2025-06-01, no valid_to (current), supersedes old.
	newValidFrom := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	if err := client.CreateEdge(ctx, tenantID, graphdb.Edge{
		ID:                "asof-edge-new",
		Label:             "MAPS_TO",
		Source:            "asof-src",
		Target:            "asof-tgt",
		ValidFrom:         newValidFrom,
		DeterminedBy:      "job-new",
		DeterminationType: "human_feedback",
		Confidence:        0.95,
		Supersedes:        "asof-edge-old",
	}); err != nil {
		t.Fatalf("CreateEdge new: %v", err)
	}

	q := graphdb.RelationshipQuery{EdgeLabel: "MAPS_TO"}

	// Before any edge existed (2024-12-01): 0 results.
	beforeAll := time.Date(2024, 12, 1, 0, 0, 0, 0, time.UTC)
	results, err := client.QueryAsOf(ctx, tenantID, q, beforeAll)
	if err != nil {
		t.Fatalf("QueryAsOf before all: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("QueryAsOf(2024-12-01): expected 0 results, got %d", len(results))
	}

	// During old edge validity (2025-03-01): 1 result, llm_initial.
	duringOld := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)
	results, err = client.QueryAsOf(ctx, tenantID, q, duringOld)
	if err != nil {
		t.Fatalf("QueryAsOf during old: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("QueryAsOf(2025-03-01): expected 1 result, got %d", len(results))
	}
	if results[0].Edge.DeterminationType != "llm_initial" {
		t.Errorf("during old: determination_type = %q, want llm_initial", results[0].Edge.DeterminationType)
	}

	// After supersession (2025-12-01): 1 result, human_feedback with supersedes set.
	afterSupersede := time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC)
	results, err = client.QueryAsOf(ctx, tenantID, q, afterSupersede)
	if err != nil {
		t.Fatalf("QueryAsOf after supersession: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("QueryAsOf(2025-12-01): expected 1 result, got %d", len(results))
	}
	if results[0].Edge.DeterminationType != "human_feedback" {
		t.Errorf("after supersession: determination_type = %q, want human_feedback", results[0].Edge.DeterminationType)
	}
	if results[0].Edge.Supersedes != "asof-edge-old" {
		t.Errorf("after supersession: supersedes = %q, want asof-edge-old", results[0].Edge.Supersedes)
	}
}

func TestIntegration_TemporalCurrentState(t *testing.T) {
	tenantID := "graphdb-temporal-current"
	setupTenant(t, tenantID)

	client := graphdb.New(testDB)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Microsecond)

	// Create two nodes.
	if err := client.CreateNode(ctx, tenantID, graphdb.Node{
		ID:        "tc-src",
		Label:     "Requirement",
		ValidFrom: now,
	}); err != nil {
		t.Fatalf("CreateNode source: %v", err)
	}
	if err := client.CreateNode(ctx, tenantID, graphdb.Node{
		ID:        "tc-tgt",
		Label:     "Document",
		ValidFrom: now,
	}); err != nil {
		t.Fatalf("CreateNode target: %v", err)
	}

	// Closed edge (valid_to set).
	closedEnd := now.Add(-1 * time.Hour)
	closedStart := closedEnd.Add(-24 * time.Hour)
	if err := client.CreateEdge(ctx, tenantID, graphdb.Edge{
		ID:                "tc-edge-closed",
		Label:             "REFERENCES",
		Source:            "tc-src",
		Target:            "tc-tgt",
		ValidFrom:         closedStart,
		ValidTo:           &closedEnd,
		DeterminedBy:      "job-old",
		DeterminationType: "llm_initial",
		Confidence:        0.6,
	}); err != nil {
		t.Fatalf("CreateEdge closed: %v", err)
	}

	// Current edge (valid_to nil, supersedes closed).
	if err := client.CreateEdge(ctx, tenantID, graphdb.Edge{
		ID:                "tc-edge-current",
		Label:             "REFERENCES",
		Source:            "tc-src",
		Target:            "tc-tgt",
		ValidFrom:         now,
		DeterminedBy:      "job-new",
		DeterminationType: "human_feedback",
		Confidence:        0.95,
		Supersedes:        "tc-edge-closed",
	}); err != nil {
		t.Fatalf("CreateEdge current: %v", err)
	}

	// QueryRelationships returns only current edges (valid_to IS NULL).
	results, err := client.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{
		EdgeLabel: "REFERENCES",
	})
	if err != nil {
		t.Fatalf("QueryRelationships: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 current relationship, got %d", len(results))
	}
	if results[0].Edge.ID != "tc-edge-current" {
		t.Errorf("expected current edge tc-edge-current, got %q", results[0].Edge.ID)
	}
}

func TestIntegration_EdgeVersioning(t *testing.T) {
	tenantID := "graphdb-edge-version"
	setupTenant(t, tenantID)

	client := graphdb.New(testDB)
	ctx := context.Background()

	baseTime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	// Create nodes.
	if err := client.CreateNode(ctx, tenantID, graphdb.Node{
		ID:        "ev-src",
		Label:     "Control",
		ValidFrom: baseTime,
	}); err != nil {
		t.Fatalf("CreateNode source: %v", err)
	}
	if err := client.CreateNode(ctx, tenantID, graphdb.Node{
		ID:        "ev-tgt",
		Label:     "Evidence",
		ValidFrom: baseTime,
	}); err != nil {
		t.Fatalf("CreateNode target: %v", err)
	}

	// Create initial LLM edge (will be closed).
	closedEnd := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	if err := client.CreateEdge(ctx, tenantID, graphdb.Edge{
		ID:                "ev-edge-v1",
		Label:             "SATISFIED_BY",
		Source:            "ev-src",
		Target:            "ev-tgt",
		ValidFrom:         baseTime,
		ValidTo:           &closedEnd,
		DeterminedBy:      "llm-job",
		DeterminationType: "llm_initial",
		Confidence:        0.7,
	}); err != nil {
		t.Fatalf("CreateEdge v1: %v", err)
	}

	// Create superseding human_feedback edge.
	if err := client.CreateEdge(ctx, tenantID, graphdb.Edge{
		ID:                "ev-edge-v2",
		Label:             "SATISFIED_BY",
		Source:            "ev-src",
		Target:            "ev-tgt",
		ValidFrom:         closedEnd,
		DeterminedBy:      "human-reviewer",
		DeterminationType: "human_feedback",
		Confidence:        0.99,
		Supersedes:        "ev-edge-v1",
	}); err != nil {
		t.Fatalf("CreateEdge v2: %v", err)
	}

	// QueryRelationships (current state) finds only the superseding edge.
	results, err := client.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{
		EdgeLabel: "SATISFIED_BY",
	})
	if err != nil {
		t.Fatalf("QueryRelationships current: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 current relationship, got %d", len(results))
	}
	if results[0].Edge.ID != "ev-edge-v2" {
		t.Errorf("current edge = %q, want ev-edge-v2", results[0].Edge.ID)
	}
	if results[0].Edge.DeterminationType != "human_feedback" {
		t.Errorf("determination_type = %q, want human_feedback", results[0].Edge.DeterminationType)
	}

	// QueryAsOf at old time finds the original.
	oldTime := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)
	results, err = client.QueryAsOf(ctx, tenantID, graphdb.RelationshipQuery{
		EdgeLabel: "SATISFIED_BY",
	}, oldTime)
	if err != nil {
		t.Fatalf("QueryAsOf old: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("QueryAsOf: expected 1 result, got %d", len(results))
	}
	if results[0].Edge.ID != "ev-edge-v1" {
		t.Errorf("historical edge = %q, want ev-edge-v1", results[0].Edge.ID)
	}
	if results[0].Edge.DeterminationType != "llm_initial" {
		t.Errorf("historical determination_type = %q, want llm_initial", results[0].Edge.DeterminationType)
	}
}

func TestIntegration_TenantIsolation(t *testing.T) {
	tenantA := "graphdb-iso-a"
	tenantB := "graphdb-iso-b"
	setupTenant(t, tenantA)
	setupTenant(t, tenantB)

	client := graphdb.New(testDB)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	// Create node and edge in tenant A.
	if err := client.CreateNode(ctx, tenantA, graphdb.Node{
		ID:        "iso-src",
		Label:     "Requirement",
		ValidFrom: now,
	}); err != nil {
		t.Fatalf("CreateNode A source: %v", err)
	}
	if err := client.CreateNode(ctx, tenantA, graphdb.Node{
		ID:        "iso-tgt",
		Label:     "Document",
		ValidFrom: now,
	}); err != nil {
		t.Fatalf("CreateNode A target: %v", err)
	}
	if err := client.CreateEdge(ctx, tenantA, graphdb.Edge{
		ID:                "iso-edge",
		Label:             "DEFINED_IN",
		Source:            "iso-src",
		Target:            "iso-tgt",
		ValidFrom:         now,
		DeterminedBy:      "job-a",
		DeterminationType: "llm_initial",
		Confidence:        0.8,
	}); err != nil {
		t.Fatalf("CreateEdge A: %v", err)
	}

	// Query from tenant B: should see 0 results.
	resultsB, err := client.QueryRelationships(ctx, tenantB, graphdb.RelationshipQuery{
		EdgeLabel: "DEFINED_IN",
	})
	if err != nil {
		t.Fatalf("QueryRelationships B: %v", err)
	}
	if len(resultsB) != 0 {
		t.Errorf("tenant B sees %d relationships from tenant A, want 0", len(resultsB))
	}

	// Query from tenant A: should see 1 result.
	resultsA, err := client.QueryRelationships(ctx, tenantA, graphdb.RelationshipQuery{
		EdgeLabel: "DEFINED_IN",
	})
	if err != nil {
		t.Fatalf("QueryRelationships A: %v", err)
	}
	if len(resultsA) != 1 {
		t.Errorf("tenant A sees %d relationships, want 1", len(resultsA))
	}
}

func TestIntegration_Traverse(t *testing.T) {
	tenantID := "graphdb-traverse"
	setupTenant(t, tenantID)

	client := graphdb.New(testDB)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	// Create chain: A -> B -> C via PARENT_OF edges.
	for _, n := range []graphdb.Node{
		{ID: "trav-a", Label: "Category", ValidFrom: now},
		{ID: "trav-b", Label: "Category", ValidFrom: now},
		{ID: "trav-c", Label: "Category", ValidFrom: now},
	} {
		if err := client.CreateNode(ctx, tenantID, n); err != nil {
			t.Fatalf("CreateNode %s: %v", n.ID, err)
		}
	}

	if err := client.CreateEdge(ctx, tenantID, graphdb.Edge{
		ID:        "trav-ab",
		Label:     "PARENT_OF",
		Source:    "trav-a",
		Target:    "trav-b",
		ValidFrom: now,
	}); err != nil {
		t.Fatalf("CreateEdge A->B: %v", err)
	}
	if err := client.CreateEdge(ctx, tenantID, graphdb.Edge{
		ID:        "trav-bc",
		Label:     "PARENT_OF",
		Source:    "trav-b",
		Target:    "trav-c",
		ValidFrom: now,
	}); err != nil {
		t.Fatalf("CreateEdge B->C: %v", err)
	}

	// Traverse outbound from A with MaxDepth=2.
	paths, err := client.Traverse(ctx, tenantID, graphdb.TraversalQuery{
		StartNode:  "trav-a",
		Direction:  "outbound",
		EdgeLabels: []string{"PARENT_OF"},
		MaxDepth:   2,
	})
	if err != nil {
		t.Fatalf("Traverse: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("Traverse returned 0 paths, expected at least 1")
	}

	// Verify node C was reached in at least one path.
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
	if !reachedC {
		t.Error("Traverse did not reach node trav-c")
	}
}

func TestIntegration_TenantRequired(t *testing.T) {
	client := graphdb.New(testDB)
	ctx := context.Background()

	// CreateGraph("") must return ErrTenantRequired.
	err := client.CreateGraph(ctx, "")
	if !errors.Is(err, graphdb.ErrTenantRequired) {
		t.Errorf("CreateGraph empty tenant: got %v, want ErrTenantRequired", err)
	}

	// CreateNode("", ...) must return ErrTenantRequired.
	err = client.CreateNode(ctx, "", graphdb.Node{
		ID:        "should-fail",
		Label:     "Requirement",
		ValidFrom: time.Now().UTC(),
	})
	if !errors.Is(err, graphdb.ErrTenantRequired) {
		t.Errorf("CreateNode empty tenant: got %v, want ErrTenantRequired", err)
	}
}
