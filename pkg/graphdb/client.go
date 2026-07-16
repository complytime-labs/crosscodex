package graphdb

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
)

// ageClient implements GraphDB using Apache AGE on PostgreSQL.
type ageClient struct {
	db           *sql.DB
	tracer       trace.Tracer
	meter        metric.Meter
	queryCounter metric.Int64Counter
	queryLatency metric.Int64Histogram
}

// New creates a GraphDB client backed by Apache AGE.
// The caller owns the *sql.DB and is responsible for closing it.
func New(db *sql.DB, opts ...Option) (GraphDB, error) {
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
		return nil, ErrTenantRequired
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

// cypherDollarTag is the PostgreSQL dollar-quote tag wrapping Cypher queries
// in ag_catalog.cypher() calls. A tagged dollar-quote prevents content
// containing bare $$ from escaping the SQL string boundary. escapeCypher
// strips this tag from any content as a defense-in-depth measure.
const cypherDollarTag = "$cypher$"

// escapeCypher escapes backslashes and single quotes for Cypher string literals
// and strips the dollar-quote tag to prevent SQL injection via AGE's
// dollar-quoted Cypher embedding.
func escapeCypher(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	s = strings.ReplaceAll(s, cypherDollarTag, "")
	return s
}

// nodeToAGProperties serializes a Node's fields into a Cypher property map
// string such as {id: 'x', valid_from: '2024-01-01T00:00:00Z'}.
func nodeToAGProperties(n Node) string {
	var pairs []string
	pairs = append(pairs, fmt.Sprintf("id: '%s'", escapeCypher(n.ID)))
	pairs = append(pairs, fmt.Sprintf("valid_from: '%s'", escapeCypher(n.ValidFrom.Format(time.RFC3339Nano))))
	if n.ValidTo != nil {
		pairs = append(pairs, fmt.Sprintf("valid_to: '%s'", escapeCypher(n.ValidTo.Format(time.RFC3339Nano))))
	}
	if n.CreatedBy != "" {
		pairs = append(pairs, fmt.Sprintf("created_by: '%s'", escapeCypher(n.CreatedBy)))
	}
	if n.CreationMethod != "" {
		pairs = append(pairs, fmt.Sprintf("creation_method: '%s'", escapeCypher(n.CreationMethod)))
	}
	for k, v := range n.Properties {
		pairs = append(pairs, fmt.Sprintf("%s: %s", escapeCypher(k), cypherValue(v)))
	}
	return "{" + strings.Join(pairs, ", ") + "}"
}

// edgeToAGProperties serializes an Edge's fields into a Cypher property map.
func edgeToAGProperties(e Edge) string {
	var pairs []string
	pairs = append(pairs, fmt.Sprintf("id: '%s'", escapeCypher(e.ID)))
	pairs = append(pairs, fmt.Sprintf("valid_from: '%s'", escapeCypher(e.ValidFrom.Format(time.RFC3339Nano))))
	if e.ValidTo != nil {
		pairs = append(pairs, fmt.Sprintf("valid_to: '%s'", escapeCypher(e.ValidTo.Format(time.RFC3339Nano))))
	}
	if e.DeterminedBy != "" {
		pairs = append(pairs, fmt.Sprintf("determined_by: '%s'", escapeCypher(e.DeterminedBy)))
	}
	if e.DeterminationType != "" {
		pairs = append(pairs, fmt.Sprintf("determination_type: '%s'", escapeCypher(e.DeterminationType)))
	}
	if e.Confidence != 0 {
		pairs = append(pairs, fmt.Sprintf("confidence: %g", e.Confidence))
	}
	if e.Supersedes != "" {
		pairs = append(pairs, fmt.Sprintf("supersedes: '%s'", escapeCypher(e.Supersedes)))
	}
	for k, v := range e.Properties {
		pairs = append(pairs, fmt.Sprintf("%s: %s", escapeCypher(k), cypherValue(v)))
	}
	return "{" + strings.Join(pairs, ", ") + "}"
}

// cypherValue formats a Go value as a Cypher literal.
func cypherValue(v any) string {
	switch val := v.(type) {
	case string:
		return fmt.Sprintf("'%s'", escapeCypher(val))
	case float64:
		return fmt.Sprintf("%g", val)
	case float32:
		return fmt.Sprintf("%g", val)
	case int:
		return fmt.Sprintf("%d", val)
	case int64:
		return fmt.Sprintf("%d", val)
	case bool:
		if val {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprintf("'%s'", escapeCypher(fmt.Sprintf("%v", val)))
	}
}

// CreateGraph creates a tenant-scoped graph if it does not already exist.
// This is idempotent: calling it multiple times for the same tenant is safe.
func (c *ageClient) CreateGraph(ctx context.Context, tenant string) error {
	if tenant == "" {
		return ErrTenantRequired
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
func (c *ageClient) CreateNode(ctx context.Context, tenant string, node Node) error {
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
		return fmt.Errorf("create node %s/%s: %w", node.Label, node.ID, ErrNodeExists)
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
func (c *ageClient) CreateEdge(ctx context.Context, tenant, sourceID, targetID string, edge Edge) error {
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

// CreateRequiresEdge creates a REQUIRES edge from the tenant's requires_consensus data.
// The method transforms RequiresEdge consensus metadata into a graph edge with
// full provenance (models, confidence, vote counts). This is the pipeline entry point
// for materializing consensus results into the graph.
func (c *ageClient) CreateRequiresEdge(ctx context.Context, tenant string, reqEdge RequiresEdge) error {
	if reqEdge.SourceID == "" {
		return fmt.Errorf("create requires edge: source_id is required")
	}
	if reqEdge.TargetID == "" {
		return fmt.Errorf("create requires edge: target_id is required")
	}
	if reqEdge.AnalyzedAt.IsZero() {
		return fmt.Errorf("create requires edge: analyzed_at is required")
	}
	start := time.Now()
	ctx, span := c.startSpan(ctx, "graphdb.CreateRequiresEdge")
	defer span.End()
	span.SetAttributes(
		attribute.String("tenant.id", tenant),
		attribute.String("source.id", reqEdge.SourceID),
		attribute.String("target.id", reqEdge.TargetID),
	)

	tx, err := c.beginTx(ctx, tenant)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	defer func() { _ = tx.Rollback() }()

	gn := graphName(tenant)

	// Build properties map for REQUIRES edge. Source/target node IDs are
	// structural topology (the MATCH clause endpoints), not data properties.
	// Tenant ID is the graph partition key (graph name), not an edge property.
	var pairs []string
	pairs = append(pairs, fmt.Sprintf("confidence: %g", reqEdge.Confidence))
	pairs = append(pairs, fmt.Sprintf("unanimous: %v", reqEdge.Unanimous))
	pairs = append(pairs, fmt.Sprintf("valid_votes: %d", reqEdge.ValidVotes))
	pairs = append(pairs, fmt.Sprintf("total_votes: %d", reqEdge.TotalVotes))
	pairs = append(pairs, fmt.Sprintf("vote_weight: %g", reqEdge.VoteWeight))

	// Models array: model names joined in Cypher array syntax.
	if len(reqEdge.Models) > 0 {
		models := ""
		for i, m := range reqEdge.Models {
			if i > 0 {
				models += ", "
			}
			models += "'" + escapeCypher(m) + "'"
		}
		pairs = append(pairs, fmt.Sprintf("models: [%s]", models))
	}

	pairs = append(pairs, fmt.Sprintf("samples_per_model: %d", reqEdge.SamplesPerModel))
	if reqEdge.PromptVersion != "" {
		pairs = append(pairs, fmt.Sprintf("prompt_version: '%s'", escapeCypher(reqEdge.PromptVersion)))
	}
	pairs = append(pairs, fmt.Sprintf("analyzed_at: '%s'", escapeCypher(reqEdge.AnalyzedAt.Format(time.RFC3339Nano))))
	pairs = append(pairs, fmt.Sprintf("job_id: '%s'", escapeCypher(reqEdge.JobID)))

	props := "{" + strings.Join(pairs, ", ") + "}"

	cypher := fmt.Sprintf(
		"MATCH (s {id: '%s'}), (t {id: '%s'}) CREATE (s)-[e:REQUIRES %s]->(t)",
		escapeCypher(reqEdge.SourceID),
		escapeCypher(reqEdge.TargetID),
		props,
	)
	query := fmt.Sprintf(
		"SELECT * FROM ag_catalog.cypher('%s', "+cypherDollarTag+" %s "+cypherDollarTag+") AS (v agtype)",
		escapeCypher(gn), cypher,
	)

	if _, err := tx.ExecContext(ctx, query); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("create requires edge: %w", err)
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
func (c *ageClient) QueryRelationships(ctx context.Context, tenant string, query RelationshipQuery) ([]Relationship, error) {
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
func (c *ageClient) QueryAsOf(ctx context.Context, tenant string, query RelationshipQuery, asOf time.Time) ([]Relationship, error) {
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
	q RelationshipQuery,
	asOf *time.Time,
) ([]Relationship, error) {
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

	var results []Relationship
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
		results = append(results, Relationship{
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
func (c *ageClient) Traverse(ctx context.Context, tenant string, query TraversalQuery) ([]Path, error) {
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

	// Edge label filter: either a specific set of labels or any edge.
	edgePattern := "e"
	if len(query.EdgeLabels) > 0 {
		escaped := make([]string, len(query.EdgeLabels))
		for i, l := range query.EdgeLabels {
			escaped[i] = escapeCypher(l)
		}
		edgePattern = "e:" + strings.Join(escaped, "|")
	}

	// Depth bound.
	depthSuffix := "*1.."
	if query.MaxDepth > 0 {
		depthSuffix = fmt.Sprintf("*1..%d", query.MaxDepth)
	}

	// Direction: outbound ->, inbound <-, both -.
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
	default: // "outbound" or unspecified
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

	var results []Path
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
	if c.queryCounter != nil {
		c.queryCounter.Add(ctx, 1)
	}
	if c.queryLatency != nil {
		c.queryLatency.Record(ctx, time.Since(start).Milliseconds())
	}
	span.SetStatus(codes.Ok, "")
	return results, tx.Commit()
}
