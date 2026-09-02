package agedriver

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/pkg/graphdb"
)

// ageClient implements graphdb.GraphDB using Apache AGE on PostgreSQL.
type ageClient struct {
	db           *sql.DB
	tracer       trace.Tracer
	meter        metric.Meter
	queryCounter metric.Int64Counter
	queryLatency metric.Int64Histogram
}

// New creates a GraphDB client backed by Apache AGE.
// The caller owns the *sql.DB and is responsible for closing it.
func New(db *sql.DB, opts ...Option) (graphdb.GraphDB, error) {
	c := &ageClient{db: db}
	for _, opt := range opts {
		if err := opt(c); err != nil {
			return nil, fmt.Errorf("apply graphdb option: %w", err)
		}
	}
	return c, nil
}

// startSpan begins a new trace span using the client's configured tracer,
// falling back to the context's tracer provider when no explicit tracer is set.
func (c *ageClient) startSpan(ctx context.Context, name string) (context.Context, trace.Span) {
	if c.tracer != nil {
		return c.tracer.Start(ctx, name)
	}
	return trace.SpanFromContext(ctx).TracerProvider().Tracer("graphdb").Start(ctx, name)
}

// graphName returns the AGE graph name scoped to the given tenant.
func graphName(tenant string) string {
	return "crosscodex_" + tenant
}

// beginTx starts a transaction, sets the search path for ag_catalog, and
// records the tenant via set_config for defensive assertions
// (assert_tenant_graph).
//
// This client connects as graph_user — a dedicated role that owns per-tenant
// graph schemas. graph_user has no access to relational tables; app_user has
// no access to graph schemas. See pkg/db/doc.go for the full security model.
//
// The AGE shared library must be loaded at server startup via
// shared_preload_libraries=age in postgresql.conf. We deliberately do NOT
// use LOAD 'age' here because PostgreSQL restricts the LOAD command to
// superusers. shared_preload_libraries makes the library available to all
// sessions without per-session LOAD calls.
func (c *ageClient) beginTx(ctx context.Context, tenant string) (*sql.Tx, error) {
	if tenant == "" {
		return nil, graphdb.ErrTenantRequired
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `SET search_path = ag_catalog, "$user", public`); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("set search_path: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		"SELECT set_config('app.current_tenant', $1, true)", tenant); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("set tenant config: %w", err)
	}
	return tx, nil
}

// CreateGraph creates a tenant-scoped graph if it does not already exist.
// This is idempotent: calling it multiple times for the same tenant is safe.
func (c *ageClient) CreateGraph(ctx context.Context, tenant string) error {
	if tenant == "" {
		return graphdb.ErrTenantRequired
	}
	start := time.Now()
	ctx, span := c.startSpan(ctx, "graphdb.CreateGraph")
	defer span.End()
	span.SetAttributes(attribute.String("tenant.id", tenant))

	gn := graphName(tenant)

	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `SET search_path = ag_catalog, "$user", public`); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("set search_path: %w", err)
	}

	var exists bool
	err = tx.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM ag_catalog.ag_graph WHERE name = $1)", gn).Scan(&exists)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("check graph existence: %w", err)
	}
	if exists {
		if c.queryCounter != nil {
			c.queryCounter.Add(ctx, 1)
		}
		if c.queryLatency != nil {
			c.queryLatency.Record(ctx, time.Since(start).Milliseconds())
		}
		span.SetStatus(codes.Ok, "")
		return tx.Commit()
	}

	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf("SELECT ag_catalog.create_graph('%s')", escapeCypher(gn))); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("create graph %q: %w", gn, err)
	}
	if c.queryCounter != nil {
		c.queryCounter.Add(ctx, 1)
	}
	if c.queryLatency != nil {
		c.queryLatency.Record(ctx, time.Since(start).Milliseconds())
	}
	span.SetStatus(codes.Ok, "")
	return tx.Commit()
}

// CreateNode creates a vertex in the tenant's graph.
// Returns ErrNodeExists if a node with the same label and id already exists.
// Apache AGE does not enforce unique constraints on node properties, so this
// method performs an explicit MATCH check within the same transaction.
func (c *ageClient) CreateNode(ctx context.Context, tenant string, node graphdb.Node) error {
	if node.ID == "" {
		return fmt.Errorf("create node: id is required")
	}
	if node.Label == "" {
		return fmt.Errorf("create node: label is required")
	}
	if node.ValidFrom.IsZero() {
		return fmt.Errorf("create node: valid_from is required")
	}
	start := time.Now()
	ctx, span := c.startSpan(ctx, "graphdb.CreateNode")
	defer span.End()
	span.SetAttributes(attribute.String("tenant.id", tenant))

	tx, err := c.beginTx(ctx, tenant)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	defer func() { _ = tx.Rollback() }()

	gn := graphName(tenant)

	matchCypher := fmt.Sprintf("MATCH (n:%s {id: '%s'}) RETURN n",
		escapeCypher(node.Label), escapeCypher(node.ID))
	matchQuery := fmt.Sprintf(
		"SELECT * FROM ag_catalog.cypher('%s', "+cypherDollarTag+" %s "+cypherDollarTag+") AS (v agtype)",
		escapeCypher(gn), matchCypher,
	)
	rows, err := tx.QueryContext(ctx, matchQuery)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("create node: check existing: %w", err)
	}
	exists := rows.Next()
	if closeErr := rows.Close(); closeErr != nil {
		span.SetStatus(codes.Error, closeErr.Error())
		return fmt.Errorf("create node: close check: %w", closeErr)
	}
	if exists {
		span.SetStatus(codes.Ok, "node exists")
		return fmt.Errorf("create node %s/%s: %w", node.Label, node.ID, graphdb.ErrNodeExists)
	}

	props := nodeToAGProperties(node)
	cypher := fmt.Sprintf("CREATE (n:%s %s)", escapeCypher(node.Label), props)
	query := fmt.Sprintf(
		"SELECT * FROM ag_catalog.cypher('%s', "+cypherDollarTag+" %s "+cypherDollarTag+") AS (v agtype)",
		escapeCypher(gn), cypher,
	)

	if _, err := tx.ExecContext(ctx, query); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("create node: %w", err)
	}
	if c.queryCounter != nil {
		c.queryCounter.Add(ctx, 1)
	}
	if c.queryLatency != nil {
		c.queryLatency.Record(ctx, time.Since(start).Milliseconds())
	}
	span.SetStatus(codes.Ok, "")
	return tx.Commit()
}

// CreateEdge creates a directed edge between two existing nodes in the tenant's graph.
// Source and target node IDs are explicit parameters — they identify the structural
// endpoints of the edge and are NOT stored as edge properties. Edge properties carry
// only domain-level metadata (confidence, determination_type, etc.).
func (c *ageClient) CreateEdge(ctx context.Context, tenant, sourceID, targetID string, edge graphdb.Edge) error {
	if edge.Label == "" {
		return fmt.Errorf("create edge: label is required")
	}
	if sourceID == "" || targetID == "" {
		return fmt.Errorf("create edge: source and target are required")
	}
	if edge.ValidFrom.IsZero() {
		return fmt.Errorf("create edge: valid_from is required")
	}
	start := time.Now()
	ctx, span := c.startSpan(ctx, "graphdb.CreateEdge")
	defer span.End()
	span.SetAttributes(attribute.String("tenant.id", tenant))

	tx, err := c.beginTx(ctx, tenant)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	defer func() { _ = tx.Rollback() }()

	gn := graphName(tenant)
	props := edgeToAGProperties(edge)
	cypher := fmt.Sprintf(
		"MATCH (s {id: '%s'}), (t {id: '%s'}) CREATE (s)-[e:%s %s]->(t)",
		escapeCypher(sourceID),
		escapeCypher(targetID),
		escapeCypher(edge.Label),
		props,
	)
	query := fmt.Sprintf(
		"SELECT * FROM ag_catalog.cypher('%s', "+cypherDollarTag+" %s "+cypherDollarTag+") AS (v agtype)",
		escapeCypher(gn), cypher,
	)

	if _, err := tx.ExecContext(ctx, query); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("create edge: %w", err)
	}
	if c.queryCounter != nil {
		c.queryCounter.Add(ctx, 1)
	}
	if c.queryLatency != nil {
		c.queryLatency.Record(ctx, time.Since(start).Milliseconds())
	}
	span.SetStatus(codes.Ok, "")
	return tx.Commit()
}

// QueryRelationships finds currently-valid relationships matching the query filters.
func (c *ageClient) QueryRelationships(ctx context.Context, tenant string, query graphdb.RelationshipQuery) ([]graphdb.Relationship, error) {
	start := time.Now()
	ctx, span := c.startSpan(ctx, "graphdb.QueryRelationships")
	defer span.End()
	span.SetAttributes(attribute.String("tenant.id", tenant))

	results, err := c.queryRelationshipsInternal(ctx, tenant, query, nil)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	if c.queryCounter != nil {
		c.queryCounter.Add(ctx, 1)
	}
	if c.queryLatency != nil {
		c.queryLatency.Record(ctx, time.Since(start).Milliseconds())
	}
	span.SetStatus(codes.Ok, "")
	return results, nil
}

// QueryAsOf finds relationships that were valid at the given point in time.
func (c *ageClient) QueryAsOf(ctx context.Context, tenant string, query graphdb.RelationshipQuery, asOf time.Time) ([]graphdb.Relationship, error) {
	start := time.Now()
	ctx, span := c.startSpan(ctx, "graphdb.QueryAsOf")
	defer span.End()
	span.SetAttributes(attribute.String("tenant.id", tenant))

	results, err := c.queryRelationshipsInternal(ctx, tenant, query, &asOf)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	if c.queryCounter != nil {
		c.queryCounter.Add(ctx, 1)
	}
	if c.queryLatency != nil {
		c.queryLatency.Record(ctx, time.Since(start).Milliseconds())
	}
	span.SetStatus(codes.Ok, "")
	return results, nil
}

// queryRelationshipsInternal implements relationship queries with optional
// temporal filtering. When asOf is nil it returns currently-valid edges
// (valid_to IS NULL). When asOf is set it returns edges valid at that instant.
func (c *ageClient) queryRelationshipsInternal(
	ctx context.Context,
	tenant string,
	q graphdb.RelationshipQuery,
	asOf *time.Time,
) ([]graphdb.Relationship, error) {
	tx, err := c.beginTx(ctx, tenant)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	gn := graphName(tenant)

	sourcePattern := "s"
	if q.SourceLabel != "" {
		sourcePattern = fmt.Sprintf("s:%s", escapeCypher(q.SourceLabel))
	}
	targetPattern := "t"
	if q.TargetLabel != "" {
		targetPattern = fmt.Sprintf("t:%s", escapeCypher(q.TargetLabel))
	}
	edgePattern := "e"
	if q.EdgeLabel != "" {
		edgePattern = fmt.Sprintf("e:%s", escapeCypher(q.EdgeLabel))
	}

	var conditions []string
	if asOf != nil {
		ts := escapeCypher(asOf.Format(time.RFC3339Nano))
		conditions = append(conditions,
			fmt.Sprintf("e.valid_from <= '%s'", ts),
			fmt.Sprintf("(e.valid_to IS NULL OR e.valid_to > '%s')", ts),
		)
	} else {
		conditions = append(conditions, "e.valid_to IS NULL")
	}
	for k, v := range q.Properties {
		conditions = append(conditions, fmt.Sprintf("e.%s = %s", escapeCypher(k), cypherValue(v)))
	}

	whereClause := ""
	if len(conditions) > 0 {
		whereClause = " WHERE " + strings.Join(conditions, " AND ")
	}

	cypher := fmt.Sprintf("MATCH (%s)-[%s]->(%s)%s RETURN s, e, t",
		sourcePattern, edgePattern, targetPattern, whereClause)
	sqlQuery := fmt.Sprintf(
		"SELECT * FROM ag_catalog.cypher('%s', "+cypherDollarTag+" %s "+cypherDollarTag+") AS (s agtype, e agtype, t agtype)",
		escapeCypher(gn), cypher,
	)

	rows, err := tx.QueryContext(ctx, sqlQuery)
	if err != nil {
		return nil, fmt.Errorf("query relationships: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var results []graphdb.Relationship
	for rows.Next() {
		var sRaw, eRaw, tRaw string
		if err := rows.Scan(&sRaw, &eRaw, &tRaw); err != nil {
			return nil, fmt.Errorf("scan relationship row: %w", err)
		}
		source, err := parseAGVertex(sRaw)
		if err != nil {
			return nil, fmt.Errorf("parse source vertex: %w", err)
		}
		edge, err := parseAGEdge(eRaw)
		if err != nil {
			return nil, fmt.Errorf("parse edge: %w", err)
		}
		target, err := parseAGVertex(tRaw)
		if err != nil {
			return nil, fmt.Errorf("parse target vertex: %w", err)
		}
		results = append(results, graphdb.Relationship{
			Source: source,
			Edge:   edge,
			Target: target,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate relationship rows: %w", err)
	}
	return results, tx.Commit()
}

// Traverse performs a variable-length path traversal starting from a given node.
func (c *ageClient) Traverse(ctx context.Context, tenant string, query graphdb.TraversalQuery) ([]graphdb.Path, error) {
	start := time.Now()
	ctx, span := c.startSpan(ctx, "graphdb.Traverse")
	defer span.End()
	span.SetAttributes(attribute.String("tenant.id", tenant))

	tx, err := c.beginTx(ctx, tenant)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	gn := graphName(tenant)

	edgePattern := "e"
	if len(query.EdgeLabels) > 0 {
		escaped := make([]string, len(query.EdgeLabels))
		for i, l := range query.EdgeLabels {
			escaped[i] = escapeCypher(l)
		}
		edgePattern = "e:" + strings.Join(escaped, "|")
	}

	depthSuffix := "*1.."
	if query.MaxDepth > 0 {
		depthSuffix = fmt.Sprintf("*1..%d", query.MaxDepth)
	}

	var matchPattern string
	switch query.Direction {
	case "inbound":
		matchPattern = fmt.Sprintf(
			"MATCH p = (start_node {id: '%s'})<-[%s%s]-(end_node)",
			escapeCypher(query.StartNode), edgePattern, depthSuffix,
		)
	case "both":
		matchPattern = fmt.Sprintf(
			"MATCH p = (start_node {id: '%s'})-[%s%s]-(end_node)",
			escapeCypher(query.StartNode), edgePattern, depthSuffix,
		)
	default:
		matchPattern = fmt.Sprintf(
			"MATCH p = (start_node {id: '%s'})-[%s%s]->(end_node)",
			escapeCypher(query.StartNode), edgePattern, depthSuffix,
		)
	}

	cypher := matchPattern + " RETURN p"
	sqlQuery := fmt.Sprintf(
		"SELECT * FROM ag_catalog.cypher('%s', "+cypherDollarTag+" %s "+cypherDollarTag+") AS (p agtype)",
		escapeCypher(gn), cypher,
	)

	rows, err := tx.QueryContext(ctx, sqlQuery)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("traverse: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var results []graphdb.Path
	for rows.Next() {
		var pRaw string
		if err := rows.Scan(&pRaw); err != nil {
			span.SetStatus(codes.Error, err.Error())
			return nil, fmt.Errorf("scan path row: %w", err)
		}
		path, err := parseAGPath(pRaw)
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
			return nil, fmt.Errorf("parse path: %w", err)
		}
		results = append(results, path)
	}
	if err := rows.Err(); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("iterate path rows: %w", err)
	}
	if query.AsOf != nil {
		filtered := results[:0]
		for _, p := range results {
			if p.ValidAt(*query.AsOf) {
				filtered = append(filtered, p)
			}
		}
		results = filtered
	}
	if c.queryCounter != nil {
		c.queryCounter.Add(ctx, 1)
	}
	if c.queryLatency != nil {
		c.queryLatency.Record(ctx, time.Since(start).Milliseconds())
	}
	span.SetStatus(codes.Ok, "")
	return results, tx.Commit()
}

// GetNode retrieves a single node by ID from the tenant's graph.
func (c *ageClient) GetNode(ctx context.Context, tenant, nodeID string) (*graphdb.Node, error) {
	if nodeID == "" {
		return nil, fmt.Errorf("get node: node_id is required")
	}
	start := time.Now()
	ctx, span := c.startSpan(ctx, "graphdb.GetNode")
	defer span.End()
	span.SetAttributes(attribute.String("tenant.id", tenant))

	tx, err := c.beginTx(ctx, tenant)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	gn := graphName(tenant)
	cypher := fmt.Sprintf("MATCH (n {id: '%s'}) RETURN n", escapeCypher(nodeID))
	query := fmt.Sprintf(
		"SELECT * FROM ag_catalog.cypher('%s', "+cypherDollarTag+" %s "+cypherDollarTag+") AS (v agtype)",
		escapeCypher(gn), cypher,
	)

	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("get node: %w", err)
	}
	defer func() { _ = rows.Close() }()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			span.SetStatus(codes.Error, err.Error())
			return nil, fmt.Errorf("get node: %w", err)
		}
		span.SetStatus(codes.Error, graphdb.ErrNodeNotFound.Error())
		return nil, fmt.Errorf("get node %s: %w", nodeID, graphdb.ErrNodeNotFound)
	}

	var raw string
	if err := rows.Scan(&raw); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("get node: scan: %w", err)
	}
	if closeErr := rows.Close(); closeErr != nil {
		span.SetStatus(codes.Error, closeErr.Error())
		return nil, fmt.Errorf("get node: close: %w", closeErr)
	}

	node, err := parseAGVertex(raw)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("get node: parse: %w", err)
	}

	if c.queryCounter != nil {
		c.queryCounter.Add(ctx, 1)
	}
	if c.queryLatency != nil {
		c.queryLatency.Record(ctx, time.Since(start).Milliseconds())
	}
	span.SetStatus(codes.Ok, "")
	return &node, tx.Commit()
}

// GetEdge retrieves a single edge by ID, including source/target node IDs.
func (c *ageClient) GetEdge(ctx context.Context, tenant, edgeID string) (*graphdb.EdgeWithEndpoints, error) {
	if edgeID == "" {
		return nil, fmt.Errorf("get edge: edge_id is required")
	}
	start := time.Now()
	ctx, span := c.startSpan(ctx, "graphdb.GetEdge")
	defer span.End()
	span.SetAttributes(attribute.String("tenant.id", tenant))

	tx, err := c.beginTx(ctx, tenant)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	gn := graphName(tenant)
	cypher := fmt.Sprintf("MATCH (s)-[e {id: '%s'}]->(t) RETURN s, e, t", escapeCypher(edgeID))
	query := fmt.Sprintf(
		"SELECT * FROM ag_catalog.cypher('%s', "+cypherDollarTag+" %s "+cypherDollarTag+") AS (s agtype, e agtype, t agtype)",
		escapeCypher(gn), cypher,
	)

	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("get edge: %w", err)
	}
	defer func() { _ = rows.Close() }()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			span.SetStatus(codes.Error, err.Error())
			return nil, fmt.Errorf("get edge: %w", err)
		}
		span.SetStatus(codes.Error, graphdb.ErrEdgeNotFound.Error())
		return nil, fmt.Errorf("get edge %s: %w", edgeID, graphdb.ErrEdgeNotFound)
	}

	var sRaw, eRaw, tRaw string
	if err := rows.Scan(&sRaw, &eRaw, &tRaw); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("get edge: scan: %w", err)
	}
	if closeErr := rows.Close(); closeErr != nil {
		span.SetStatus(codes.Error, closeErr.Error())
		return nil, fmt.Errorf("get edge: close: %w", closeErr)
	}

	source, err := parseAGVertex(sRaw)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("get edge: parse source: %w", err)
	}
	edge, err := parseAGEdge(eRaw)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("get edge: parse edge: %w", err)
	}
	target, err := parseAGVertex(tRaw)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("get edge: parse target: %w", err)
	}

	if c.queryCounter != nil {
		c.queryCounter.Add(ctx, 1)
	}
	if c.queryLatency != nil {
		c.queryLatency.Record(ctx, time.Since(start).Milliseconds())
	}
	span.SetStatus(codes.Ok, "")
	return &graphdb.EdgeWithEndpoints{
		Edge:     edge,
		SourceID: source.ID,
		TargetID: target.ID,
	}, tx.Commit()
}

// BulkCreateEdges creates multiple edges in a single transaction.
func (c *ageClient) BulkCreateEdges(ctx context.Context, tenant string, edges []graphdb.BulkEdge) ([]string, error) {
	if len(edges) == 0 {
		return nil, nil
	}

	for i, be := range edges {
		if be.Edge.Label == "" {
			return nil, fmt.Errorf("bulk create edges [%d]: label is required", i)
		}
		if be.SourceID == "" || be.TargetID == "" {
			return nil, fmt.Errorf("bulk create edges [%d]: source and target are required", i)
		}
		if be.Edge.ValidFrom.IsZero() {
			return nil, fmt.Errorf("bulk create edges [%d]: valid_from is required", i)
		}
	}

	start := time.Now()
	ctx, span := c.startSpan(ctx, "graphdb.BulkCreateEdges")
	defer span.End()
	span.SetAttributes(
		attribute.String("tenant.id", tenant),
		attribute.Int("edge.count", len(edges)),
	)

	tx, err := c.beginTx(ctx, tenant)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	gn := graphName(tenant)
	ids := make([]string, 0, len(edges))

	for i, be := range edges {
		props := edgeToAGProperties(be.Edge)
		cypher := fmt.Sprintf(
			"MATCH (s {id: '%s'}), (t {id: '%s'}) CREATE (s)-[e:%s %s]->(t) RETURN e",
			escapeCypher(be.SourceID),
			escapeCypher(be.TargetID),
			escapeCypher(be.Edge.Label),
			props,
		)
		query := fmt.Sprintf(
			"SELECT * FROM ag_catalog.cypher('%s', "+cypherDollarTag+" %s "+cypherDollarTag+") AS (e agtype)",
			escapeCypher(gn), cypher,
		)

		if _, err := tx.ExecContext(ctx, query); err != nil {
			span.SetStatus(codes.Error, err.Error())
			return ids, fmt.Errorf("bulk create edges [%d]: %w", i, err)
		}
		ids = append(ids, be.Edge.ID)
	}

	if c.queryCounter != nil {
		c.queryCounter.Add(ctx, 1)
	}
	if c.queryLatency != nil {
		c.queryLatency.Record(ctx, time.Since(start).Milliseconds())
	}
	span.SetStatus(codes.Ok, "")
	return ids, tx.Commit()
}

// parseQueryValue inspects AGE type suffixes and returns a tagged QueryValue.
func parseQueryValue(raw string) graphdb.QueryValue {
	switch {
	case strings.HasSuffix(raw, "::vertex"):
		node, err := parseAGVertex(raw)
		if err != nil {
			return graphdb.QueryValue{Type: graphdb.QueryValueScalar, ScalarVal: raw}
		}
		return graphdb.QueryValue{Type: graphdb.QueryValueNode, NodeVal: &node}
	case strings.HasSuffix(raw, "::edge"):
		edge, err := parseAGEdge(raw)
		if err != nil {
			return graphdb.QueryValue{Type: graphdb.QueryValueScalar, ScalarVal: raw}
		}
		return graphdb.QueryValue{Type: graphdb.QueryValueEdge, EdgeVal: &graphdb.EdgeWithEndpoints{Edge: edge}}
	default:
		return graphdb.QueryValue{Type: graphdb.QueryValueScalar, ScalarVal: raw}
	}
}

// ExecuteQuery runs a read-only openCypher query against the tenant's graph.
func (c *ageClient) ExecuteQuery(ctx context.Context, tenant, cypher string, params map[string]string) ([]graphdb.QueryRow, error) {
	if cypher == "" {
		return nil, fmt.Errorf("execute query: cypher is required")
	}
	start := time.Now()
	ctx, span := c.startSpan(ctx, "graphdb.ExecuteQuery")
	defer span.End()
	span.SetAttributes(attribute.String("tenant.id", tenant))

	tx, err := c.beginTx(ctx, tenant)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, "SET TRANSACTION READ ONLY"); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("execute query: set read-only: %w", err)
	}

	resolved := cypher
	for k, v := range params {
		resolved = strings.ReplaceAll(resolved, "$"+k, "'"+escapeCypher(v)+"'")
	}
	resolved = strings.ReplaceAll(resolved, cypherDollarTag, "")

	gn := graphName(tenant)
	sqlQuery := fmt.Sprintf(
		"SELECT * FROM ag_catalog.cypher('%s', "+cypherDollarTag+" %s "+cypherDollarTag+") AS (v agtype)",
		escapeCypher(gn), resolved,
	)

	rows, err := tx.QueryContext(ctx, sqlQuery)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		if strings.Contains(err.Error(), "cannot execute") && strings.Contains(err.Error(), "read-only") {
			return nil, fmt.Errorf("execute query: %w: %w", graphdb.ErrReadOnlyViolation, err)
		}
		return nil, fmt.Errorf("execute query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var result []graphdb.QueryRow
	cols, err := rows.Columns()
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("execute query: columns: %w", err)
	}

	for rows.Next() {
		values := make([]string, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			span.SetStatus(codes.Error, err.Error())
			return nil, fmt.Errorf("execute query: scan: %w", err)
		}

		row := graphdb.QueryRow{Values: make([]graphdb.QueryValue, len(cols))}
		for i, raw := range values {
			row.Values[i] = parseQueryValue(raw)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("execute query: iterate: %w", err)
	}

	if c.queryCounter != nil {
		c.queryCounter.Add(ctx, 1)
	}
	if c.queryLatency != nil {
		c.queryLatency.Record(ctx, time.Since(start).Milliseconds())
	}
	span.SetStatus(codes.Ok, "")
	return result, tx.Commit()
}

// SupersedeFact sets valid_to on a node or edge, marking it as superseded.
func (c *ageClient) SupersedeFact(ctx context.Context, tenant string, req graphdb.SupersedeRequest) (bool, error) {
	if req.NodeID == "" && req.EdgeID == "" {
		return false, fmt.Errorf("supersede fact: node_id or edge_id is required")
	}
	if req.NodeID != "" && req.EdgeID != "" {
		return false, fmt.Errorf("supersede fact: set node_id or edge_id, not both")
	}
	if req.SupersededAt.IsZero() {
		return false, fmt.Errorf("supersede fact: superseded_at is required")
	}
	start := time.Now()
	ctx, span := c.startSpan(ctx, "graphdb.SupersedeFact")
	defer span.End()
	span.SetAttributes(attribute.String("tenant.id", tenant))

	tx, err := c.beginTx(ctx, tenant)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	gn := graphName(tenant)
	ts := escapeCypher(req.SupersededAt.Format(time.RFC3339Nano))

	var cypher string
	if req.NodeID != "" {
		setClauses := fmt.Sprintf("n.valid_to = '%s'", ts)
		if req.SupersededByJobID != "" {
			setClauses += fmt.Sprintf(", n.superseded_by = '%s'", escapeCypher(req.SupersededByJobID))
		}
		cypher = fmt.Sprintf("MATCH (n {id: '%s'}) SET %s RETURN n",
			escapeCypher(req.NodeID), setClauses)
	} else {
		setClauses := fmt.Sprintf("e.valid_to = '%s'", ts)
		if req.SupersededByJobID != "" {
			setClauses += fmt.Sprintf(", e.superseded_by = '%s'", escapeCypher(req.SupersededByJobID))
		}
		cypher = fmt.Sprintf("MATCH ()-[e {id: '%s'}]->() SET %s RETURN e",
			escapeCypher(req.EdgeID), setClauses)
	}

	query := fmt.Sprintf(
		"SELECT * FROM ag_catalog.cypher('%s', "+cypherDollarTag+" %s "+cypherDollarTag+") AS (v agtype)",
		escapeCypher(gn), cypher,
	)

	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return false, fmt.Errorf("supersede fact: %w", err)
	}
	updated := rows.Next()
	if closeErr := rows.Close(); closeErr != nil {
		span.SetStatus(codes.Error, closeErr.Error())
		return false, fmt.Errorf("supersede fact: close: %w", closeErr)
	}
	if err := rows.Err(); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return false, fmt.Errorf("supersede fact: %w", err)
	}

	if c.queryCounter != nil {
		c.queryCounter.Add(ctx, 1)
	}
	if c.queryLatency != nil {
		c.queryLatency.Record(ctx, time.Since(start).Milliseconds())
	}
	span.SetStatus(codes.Ok, "")
	return updated, tx.Commit()
}
