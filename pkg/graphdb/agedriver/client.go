package agedriver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"maps"
	"slices"
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
// Every span carries tenant.id: the tenant if graphdb.CheckTenant accepts it,
// invalidTenantAttr otherwise, so a malformed tenant never reaches telemetry.
func (c *ageClient) startSpan(ctx context.Context, name, tenant string) (context.Context, trace.Span) {
	tenantAttr := tenant
	if graphdb.CheckTenant(tenant) != nil {
		tenantAttr = invalidTenantAttr
	}
	opt := trace.WithAttributes(attribute.String("tenant.id", tenantAttr))
	if c.tracer != nil {
		return c.tracer.Start(ctx, name, opt)
	}
	return trace.SpanFromContext(ctx).TracerProvider().Tracer("graphdb").Start(ctx, name, opt)
}

// failSpan marks span as failed with err's message. Tenant errors are
// skipped: checkTenant already set a fixed status, and their message
// contains the rejected tenant.
func failSpan(span trace.Span, err error) {
	if errors.Is(err, graphdb.ErrTenantRequired) {
		return
	}
	span.SetStatus(codes.Error, err.Error())
}

// commit commits tx and only then marks span Ok. A failed commit marks the
// span Error and returns the error prefixed with op, which record then counts
// as a failure.
func (c *ageClient) commit(span trace.Span, tx *sql.Tx, op string) error {
	if err := tx.Commit(); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("%s: commit: %w", op, err)
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// record adds one call of op to graphdb.queries.total and its duration since
// start to graphdb.query.duration_ms. Both carry operation=op, a status
// ("ok" or "error", the status attribute internal/synthesis also uses, so
// failures are counted on the same instrument as successes) and a result:
//
//   - nil error: status=ok, result=ok
//   - ErrNodeExists or ErrEdgeExists: status=ok, result=exists
//   - ErrNodeNotFound or ErrEdgeNotFound: status=ok, result=not_found
//   - ErrNodeIDConflict: status=error, result=error, because an ID reused
//     under another label is a caller bug, not an idempotent re-run signal
//   - any other error: status=error, result=error
//
// The sentinels are domain outcomes, not faults: an idempotent materializer
// re-run meets "already exists" for every fact it rebuilds, and counting
// those as errors would turn rebuild volume into error-rate alerts. A write
// rejected because an endpoint is missing (CreateEdge, BulkCreateEdges,
// CreateRequiresEdge) is likewise status=ok, result=not_found: it is a
// caller-input outcome, not a driver fault. Dashboards for write failures
// should filter on result!=ok rather than status=error.
//
// Every public method calls record from a deferred closure right after
// startSpan, so record sees the returned error and metrics cover exactly the
// calls that have a span. A tenant rejected inside the span counts as an
// error, like a database failure: the caller asked the driver for work it
// refused. Argument, identifier and query-filter checks run before startSpan
// in every method and record neither span nor metrics; they do no I/O and
// indicate a caller bug, not graph health. Panics are not detected: the
// deferred closure still runs while a panic unwinds, but err has not been
// assigned, so the call is recorded as status=ok. The driver has no
// intentional panics, and the methods do not recover just to count one. The
// tenant is not an attribute: tenant IDs are unbounded, and the span already
// carries tenant.id.
func (c *ageClient) record(ctx context.Context, op string, start time.Time, err error) {
	status, result := outcome(err)
	attrs := metric.WithAttributes(
		attribute.String("operation", op),
		attribute.String("status", status),
		attribute.String("result", result),
	)
	if c.queryCounter != nil {
		c.queryCounter.Add(ctx, 1, attrs)
	}
	if c.queryLatency != nil {
		c.queryLatency.Record(ctx, time.Since(start).Milliseconds(), attrs)
	}
}

// outcome classifies a call's returned error into the status and result
// attributes record counts it under (the rules are in record's comment).
// markOutcome applies the same classification to the call's span.
func outcome(err error) (status, result string) {
	switch {
	case err == nil:
		return "ok", "ok"
	case errors.Is(err, graphdb.ErrNodeExists), errors.Is(err, graphdb.ErrEdgeExists):
		return "ok", "exists"
	case errors.Is(err, graphdb.ErrNodeNotFound), errors.Is(err, graphdb.ErrEdgeNotFound):
		return "ok", "not_found"
	default:
		return "error", "error"
	}
}

// markOutcome sets span's status for a non-nil err the way record counts
// it, so traces and metrics agree. A domain sentinel marks the span Ok and
// sets graphdb.result to "exists" or "not_found": the OpenTelemetry SDK
// discards an Ok status's description, so the attribute is the only place
// the outcome survives. Any other error goes through failSpan.
func markOutcome(span trace.Span, err error) {
	if status, result := outcome(err); status == "ok" {
		span.SetAttributes(attribute.String("graphdb.result", result))
		span.SetStatus(codes.Ok, "")
		return
	}
	failSpan(span, err)
}

// invalidTenantAttr is the tenant.id span attribute recorded for a tenant
// graphdb.CheckTenant rejects. The rejected value itself is caller text and
// never reaches telemetry.
const invalidTenantAttr = "invalid"

// invalidTenantStatus is the span status description for a rejected tenant,
// used instead of the error message because that names the tenant.
const invalidTenantStatus = "invalid tenant ID"

// checkTenant applies graphdb.CheckTenant and, on rejection, marks span
// failed with the fixed invalidTenantStatus.
func checkTenant(span trace.Span, tenant string) error {
	if err := graphdb.CheckTenant(tenant); err != nil {
		span.SetStatus(codes.Error, invalidTenantStatus)
		return err
	}
	return nil
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
	if err := checkTenant(trace.SpanFromContext(ctx), tenant); err != nil {
		return nil, err
	}
	// Pinned to READ COMMITTED, PostgreSQL's default, so a server or role
	// default of REPEATABLE READ cannot change driver semantics. lockNode
	// depends on it: each statement must see rows committed while it waited
	// for the lock, and REPEATABLE READ would fix the snapshot at the first
	// statement (set_config below), before the wait. Every other method is a
	// short write or read designed and tested against READ COMMITTED, so the
	// pin is applied to all of them rather than only to the node writes.
	tx, err := c.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
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

// lockNode takes a transaction-scoped advisory lock on (graph, id) so
// concurrent CreateNode and UpsertNode calls for one node serialize their
// MATCH-then-CREATE. AGE has no unique constraints, so two transactions could
// otherwise both miss the node and both create it. The lock only helps
// because beginTx pins READ COMMITTED: the MATCH that runs after the wait
// takes a fresh snapshot and sees a node the previous holder committed.
// Under REPEATABLE READ the snapshot predates the wait and the MATCH would
// miss it. pg_advisory_xact_lock needs no grant, so the restricted graph_user
// can call it, and PostgreSQL releases it at commit or rollback.
func lockNode(ctx context.Context, tx *sql.Tx, gn, id string) error {
	// The label is not part of the key: node IDs are unique across labels,
	// so writers of one ID under different labels must serialize too.
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", advisoryKey("node", gn, id)); err != nil {
		return fmt.Errorf("lock node %s: %w", id, err)
	}
	return nil
}

// advisoryKey is a 64-bit FNV-1a hash of parts, each followed by a NUL byte
// so ("ab", "c") and ("a", "bc") hash different input.
func advisoryKey(parts ...string) int64 {
	h := fnv.New64a()
	for _, part := range parts {
		_, _ = h.Write([]byte(part)) // hash.Hash.Write never returns an error
		_, _ = h.Write([]byte{0})
	}
	return int64(h.Sum64()) //nolint:gosec // G115: the bits are reinterpreted as PostgreSQL's signed bigint lock key; overflow is intended
}

// CreateGraph creates a tenant-scoped graph if it does not already exist.
// This is idempotent: calling it multiple times for the same tenant is safe,
// including concurrently, because an advisory lock on the graph name
// serializes the exists check and create_graph. Creating a graph needs
// privileges the restricted graph_user does not have; production graphs are
// created by the tenant_graph_create trigger, so for graph_user this method
// only succeeds for an existing graph.
func (c *ageClient) CreateGraph(ctx context.Context, tenant string) (err error) {
	start := time.Now()
	ctx, span := c.startSpan(ctx, "graphdb.CreateGraph", tenant)
	defer span.End()
	defer func() { c.record(ctx, "create_graph", start, err) }()

	tx, err := c.beginTx(ctx, tenant)
	if err != nil {
		failSpan(span, err)
		return err
	}
	defer func() { _ = tx.Rollback() }()

	gn := graphName(tenant)
	// Serialize first-time creation: without the lock two callers could both
	// see no graph and the second create_graph would fail. Under beginTx's
	// READ COMMITTED the exists check after the wait sees the graph the
	// previous holder committed, so every concurrent call returns nil. The
	// key's leading "graph" part keeps it in a different domain from
	// lockNode's keys, which lead with "node".
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", advisoryKey("graph", gn)); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("create graph %q: lock: %w", gn, err)
	}

	var exists bool
	err = tx.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM ag_catalog.ag_graph WHERE name = $1)", gn).Scan(&exists)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("check graph existence: %w", err)
	}
	if exists {
		return c.commit(span, tx, "create graph")
	}

	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf("SELECT ag_catalog.create_graph('%s')", escapeCypher(gn))); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("create graph %q: %w", gn, err)
	}
	return c.commit(span, tx, "create graph")
}

// CreateNode creates a vertex in the tenant's graph.
// Returns ErrNodeExists if a node with the same label and id already exists,
// and a *graphdb.NodeIDConflictError if the id belongs to a node under
// another label.
// Apache AGE does not enforce unique constraints on node properties, so this
// method performs an explicit label-less MATCH check within the same
// transaction, under lockNode so a concurrent CreateNode or UpsertNode cannot
// slip in between.
func (c *ageClient) CreateNode(ctx context.Context, tenant string, node graphdb.Node) (err error) {
	if err := graphdb.CheckNode(node); err != nil {
		return fmt.Errorf("create node: %w", err)
	}
	start := time.Now()
	ctx, span := c.startSpan(ctx, "graphdb.CreateNode", tenant)
	defer span.End()
	defer func() { c.record(ctx, "create_node", start, err) }()

	tx, err := c.beginTx(ctx, tenant)
	if err != nil {
		failSpan(span, err)
		return err
	}
	defer func() { _ = tx.Rollback() }()

	gn := graphName(tenant)
	if err := lockNode(ctx, tx, gn, node.ID); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("create node: %w", err)
	}

	labels, err := storedNodeLabels(ctx, tx, tenant, gn, node.ID)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("create node: %w", err)
	}
	// The conflict check comes first: in a graph written before #148 the ID
	// can be held under this label and another, and reporting ErrNodeExists
	// there would let writers treat the conflict as success.
	if i := slices.IndexFunc(labels, func(l string) bool { return l != node.Label }); i >= 0 {
		conflict := &graphdb.NodeIDConflictError{ID: node.ID, Label: node.Label, StoredLabel: labels[i]}
		span.SetStatus(codes.Error, conflict.Error())
		return fmt.Errorf("create node: %w", conflict)
	}
	if len(labels) > 0 {
		existsErr := fmt.Errorf("create node %s/%s: %w", node.Label, node.ID, graphdb.ErrNodeExists)
		markOutcome(span, existsErr)
		return existsErr
	}

	props := nodeToAGProperties(node)
	cypher := fmt.Sprintf("CREATE (n:%s %s)", escapeCypher(node.Label), props)
	query := cypherSQL(gn, cypher, "v agtype")

	if _, err := tx.ExecContext(ctx, query); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("create node: %w", classifyErr(ctx, tenant, err))
	}
	return c.commit(span, tx, "create node")
}

// UpsertNode creates the node, or replaces the properties of the node with
// the same id and label, in one transaction. The update is a single
// MATCH ... SET n = {...}: AGE replaces the property map of the matched
// vertex in place, so its graph ID and attached edges survive, and every key
// absent from the new map is removed. The map is built by
// nodeUpsertProperties, which keeps the stored valid_from and carries
// SupersedeFact's valid_to and superseded_by over from the stored node unless
// node.ValidTo is set. No stored key is spliced into the query, so only
// CheckNode-validated identifiers and escaped values reach Cypher. When the
// SET matches nothing, the node is created exactly as CreateNode creates it.
// lockNode serializes it with concurrent CreateNode and UpsertNode calls for
// the same node. An id held by a node under another label returns a
// *graphdb.NodeIDConflictError and changes nothing.
func (c *ageClient) UpsertNode(ctx context.Context, tenant string, node graphdb.Node) (_ bool, err error) {
	if err := graphdb.CheckNode(node); err != nil {
		return false, fmt.Errorf("upsert node: %w", err)
	}
	start := time.Now()
	ctx, span := c.startSpan(ctx, "graphdb.UpsertNode", tenant)
	defer span.End()
	defer func() { c.record(ctx, "upsert_node", start, err) }()

	tx, err := c.beginTx(ctx, tenant)
	if err != nil {
		failSpan(span, err)
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	gn := graphName(tenant)
	if err := lockNode(ctx, tx, gn, node.ID); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return false, fmt.Errorf("upsert node: %w", err)
	}

	labels, err := storedNodeLabels(ctx, tx, tenant, gn, node.ID)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return false, fmt.Errorf("upsert node: %w", err)
	}
	if i := slices.IndexFunc(labels, func(l string) bool { return l != node.Label }); i >= 0 {
		conflict := &graphdb.NodeIDConflictError{ID: node.ID, Label: node.Label, StoredLabel: labels[i]}
		span.SetStatus(codes.Error, conflict.Error())
		return false, fmt.Errorf("upsert node: %w", conflict)
	}

	setCypher := fmt.Sprintf("MATCH (n:%s {id: '%s'}) SET n = %s RETURN n",
		escapeCypher(node.Label), escapeCypher(node.ID), nodeUpsertProperties(node))
	setQuery := cypherSQL(gn, setCypher, "v agtype")
	rows, err := tx.QueryContext(ctx, setQuery)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return false, fmt.Errorf("upsert node: update existing: %w", classifyErr(ctx, tenant, err))
	}
	updated := rows.Next()
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		span.SetStatus(codes.Error, err.Error())
		return false, fmt.Errorf("upsert node: update existing: %w", classifyErr(ctx, tenant, err))
	}
	if closeErr := rows.Close(); closeErr != nil {
		span.SetStatus(codes.Error, closeErr.Error())
		return false, fmt.Errorf("upsert node: close update: %w", closeErr)
	}
	if updated {
		return false, c.commit(span, tx, "upsert node")
	}

	cypher := fmt.Sprintf("CREATE (n:%s %s)", escapeCypher(node.Label), nodeToAGProperties(node))
	query := cypherSQL(gn, cypher, "v agtype")
	if _, err := tx.ExecContext(ctx, query); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return false, fmt.Errorf("upsert node: create: %w", classifyErr(ctx, tenant, err))
	}
	if err := c.commit(span, tx, "upsert node"); err != nil {
		return false, err
	}
	return true, nil
}

// CreateEdge creates a directed edge between two existing nodes in the tenant's graph.
// Source and target node IDs are explicit parameters — they identify the structural
// endpoints of the edge and are NOT stored as edge properties. Edge properties carry
// only domain-level metadata (confidence, determination_type, etc.).
// Both endpoints must resolve to exactly one node (ErrNodeNotFound otherwise),
// and a non-empty edge.ID must be unused (ErrEdgeExists).
func (c *ageClient) CreateEdge(ctx context.Context, tenant, sourceID, targetID string, edge graphdb.Edge) (err error) {
	if err := graphdb.CheckEdge(sourceID, targetID, edge); err != nil {
		return fmt.Errorf("create edge: %w", err)
	}
	start := time.Now()
	ctx, span := c.startSpan(ctx, "graphdb.CreateEdge", tenant)
	defer span.End()
	defer func() { c.record(ctx, "create_edge", start, err) }()

	tx, err := c.beginTx(ctx, tenant)
	if err != nil {
		failSpan(span, err)
		return err
	}
	defer func() { _ = tx.Rollback() }()

	gn := graphName(tenant)
	if err := checkEdgeWrite(ctx, tx, tenant, gn, sourceID, targetID, edge.ID); err != nil {
		markOutcome(span, err)
		return fmt.Errorf("create edge: %w", err)
	}
	props := edgeToAGProperties(edge)
	cypher := fmt.Sprintf(
		"MATCH (s {id: '%s'}), (t {id: '%s'}) CREATE (s)-[e:%s %s]->(t)",
		escapeCypher(sourceID),
		escapeCypher(targetID),
		escapeCypher(edge.Label),
		props,
	)
	query := cypherSQL(gn, cypher, "v agtype")

	if _, err := tx.ExecContext(ctx, query); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("create edge: %w", classifyErr(ctx, tenant, err))
	}
	return c.commit(span, tx, "create edge")
}

// QueryRelationships finds currently-valid relationships matching the query filters.
func (c *ageClient) QueryRelationships(ctx context.Context, tenant string, query graphdb.RelationshipQuery) (_ []graphdb.Relationship, err error) {
	if err := graphdb.CheckRelationshipQuery(query); err != nil {
		return nil, fmt.Errorf("query relationships: %w", err)
	}
	start := time.Now()
	ctx, span := c.startSpan(ctx, "graphdb.QueryRelationships", tenant)
	defer span.End()
	defer func() { c.record(ctx, "query_relationships", start, err) }()

	results, err := c.queryRelationshipsInternal(ctx, tenant, query, nil)
	if err != nil {
		failSpan(span, err)
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return results, nil
}

// QueryAsOf finds relationships that were valid at the given point in time.
func (c *ageClient) QueryAsOf(ctx context.Context, tenant string, query graphdb.RelationshipQuery, asOf time.Time) (_ []graphdb.Relationship, err error) {
	if err := graphdb.CheckRelationshipQuery(query); err != nil {
		return nil, fmt.Errorf("query relationships: %w", err)
	}
	start := time.Now()
	ctx, span := c.startSpan(ctx, "graphdb.QueryAsOf", tenant)
	defer span.End()
	defer func() { c.record(ctx, "query_as_of", start, err) }()

	results, err := c.queryRelationshipsInternal(ctx, tenant, query, &asOf)
	if err != nil {
		failSpan(span, err)
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return results, nil
}

// queryRelationshipsInternal implements relationship queries with optional
// temporal filtering. When asOf is nil it returns currently-valid edges
// (valid_to IS NULL). When asOf is set it returns edges valid at that instant.
// Callers validate q with graphdb.CheckRelationshipQuery before starting their span.
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
		ts := escapeCypher(graphdb.FormatTime(*asOf))
		conditions = append(conditions,
			fmt.Sprintf("e.valid_from <= '%s'", ts),
			fmt.Sprintf("(e.valid_to IS NULL OR e.valid_to > '%s')", ts),
		)
	} else {
		conditions = append(conditions, "e.valid_to IS NULL")
	}
	for _, k := range slices.Sorted(maps.Keys(q.Properties)) {
		conditions = append(conditions, fmt.Sprintf("e.%s = %s", escapeCypher(k), cypherValue(q.Properties[k])))
	}

	whereClause := ""
	if len(conditions) > 0 {
		whereClause = " WHERE " + strings.Join(conditions, " AND ")
	}

	cypher := fmt.Sprintf("MATCH (%s)-[%s]->(%s)%s RETURN s, e, t",
		sourcePattern, edgePattern, targetPattern, whereClause)
	sqlQuery := cypherSQL(gn, cypher, "s agtype, e agtype, t agtype")

	rows, err := tx.QueryContext(ctx, sqlQuery)
	if err != nil {
		return nil, fmt.Errorf("query relationships: %w", classifyErr(ctx, tenant, err))
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
		return nil, fmt.Errorf("iterate relationship rows: %w", classifyErr(ctx, tenant, err))
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("query relationships: commit: %w", err)
	}
	return results, nil
}

// Traverse performs a variable-length path traversal starting from a given node.
func (c *ageClient) Traverse(ctx context.Context, tenant string, query graphdb.TraversalQuery) (_ []graphdb.Path, err error) {
	for _, l := range query.EdgeLabels {
		if err := graphdb.CheckIdentifier("edge label", l); err != nil {
			return nil, fmt.Errorf("traverse: %w", err)
		}
	}
	start := time.Now()
	ctx, span := c.startSpan(ctx, "graphdb.Traverse", tenant)
	defer span.End()
	defer func() { c.record(ctx, "traverse", start, err) }()

	tx, err := c.beginTx(ctx, tenant)
	if err != nil {
		failSpan(span, err)
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
	sqlQuery := cypherSQL(gn, cypher, "p agtype")

	rows, err := tx.QueryContext(ctx, sqlQuery)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("traverse: %w", classifyErr(ctx, tenant, err))
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
		return nil, fmt.Errorf("iterate path rows: %w", classifyErr(ctx, tenant, err))
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
	if err := c.commit(span, tx, "traverse"); err != nil {
		return nil, err
	}
	return results, nil
}

// GetNode retrieves a single node by ID from the tenant's graph.
func (c *ageClient) GetNode(ctx context.Context, tenant, nodeID string) (_ *graphdb.Node, err error) {
	if nodeID == "" {
		return nil, fmt.Errorf("get node: node_id is required")
	}
	start := time.Now()
	ctx, span := c.startSpan(ctx, "graphdb.GetNode", tenant)
	defer span.End()
	defer func() { c.record(ctx, "get_node", start, err) }()

	tx, err := c.beginTx(ctx, tenant)
	if err != nil {
		failSpan(span, err)
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	gn := graphName(tenant)
	cypher := fmt.Sprintf("MATCH (n {id: '%s'}) RETURN n", escapeCypher(nodeID))
	query := cypherSQL(gn, cypher, "v agtype")

	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("get node: %w", classifyErr(ctx, tenant, err))
	}
	defer func() { _ = rows.Close() }()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			span.SetStatus(codes.Error, err.Error())
			return nil, fmt.Errorf("get node: %w", classifyErr(ctx, tenant, err))
		}
		notFound := fmt.Errorf("get node %s: %w", nodeID, graphdb.ErrNodeNotFound)
		markOutcome(span, notFound)
		return nil, notFound
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

	if err := c.commit(span, tx, "get node"); err != nil {
		return nil, err
	}
	return &node, nil
}

// GetEdge retrieves a single edge by ID, including source/target node IDs.
func (c *ageClient) GetEdge(ctx context.Context, tenant, edgeID string) (_ *graphdb.EdgeWithEndpoints, err error) {
	if edgeID == "" {
		return nil, fmt.Errorf("get edge: edge_id is required")
	}
	start := time.Now()
	ctx, span := c.startSpan(ctx, "graphdb.GetEdge", tenant)
	defer span.End()
	defer func() { c.record(ctx, "get_edge", start, err) }()

	tx, err := c.beginTx(ctx, tenant)
	if err != nil {
		failSpan(span, err)
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	gn := graphName(tenant)
	cypher := fmt.Sprintf("MATCH (s)-[e {id: '%s'}]->(t) RETURN s, e, t", escapeCypher(edgeID))
	query := cypherSQL(gn, cypher, "s agtype, e agtype, t agtype")

	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("get edge: %w", classifyErr(ctx, tenant, err))
	}
	defer func() { _ = rows.Close() }()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			span.SetStatus(codes.Error, err.Error())
			return nil, fmt.Errorf("get edge: %w", classifyErr(ctx, tenant, err))
		}
		notFound := fmt.Errorf("get edge %s: %w", edgeID, graphdb.ErrEdgeNotFound)
		markOutcome(span, notFound)
		return nil, notFound
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

	if err := c.commit(span, tx, "get edge"); err != nil {
		return nil, err
	}
	return &graphdb.EdgeWithEndpoints{
		Edge:     edge,
		SourceID: source.ID,
		TargetID: target.ID,
	}, nil
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
// Parameters are substituted textually (including inside Cypher string
// literals) for whole `$name` tokens matching [A-Za-z_][A-Za-z0-9_]*; numeric
// tokens like $1 and non-ASCII names are left as-is.
func (c *ageClient) ExecuteQuery(ctx context.Context, tenant, cypher string, params map[string]string) (_ []graphdb.QueryRow, err error) {
	if cypher == "" {
		return nil, fmt.Errorf("execute query: cypher is required")
	}
	// The query text is caller-written, so it is rejected rather than
	// stripped: silently rewriting it would change what the caller asked for,
	// and a single strip pass can reassemble the tag from its halves.
	// Substituted values cannot carry the tag (escapeCypher removes it, and
	// the surrounding quotes stop it straddling a boundary), so checking the
	// substituted text checks the caller's own text.
	resolved := substituteParams(cypher, params)
	if strings.Contains(resolved, cypherDollarTag) {
		return nil, fmt.Errorf("execute query: %w: the query contains %q, which would end the SQL dollar-quote wrapping it. Remove it from the query",
			graphdb.ErrInvalidCypher, cypherDollarTag)
	}
	start := time.Now()
	ctx, span := c.startSpan(ctx, "graphdb.ExecuteQuery", tenant)
	defer span.End()
	defer func() { c.record(ctx, "execute_query", start, err) }()

	tx, err := c.beginTx(ctx, tenant)
	if err != nil {
		failSpan(span, err)
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, "SET TRANSACTION READ ONLY"); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("execute query: set read-only: %w", err)
	}

	gn := graphName(tenant)
	sqlQuery := cypherSQL(gn, resolved, "v agtype")

	rows, err := tx.QueryContext(ctx, sqlQuery)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		if strings.Contains(err.Error(), "cannot execute") && strings.Contains(err.Error(), "read-only") {
			// err is PostgreSQL's text (SQLSTATE 25006): it goes on the
			// span for operators and stays out of the returned error, which
			// reaches RPC clients through the PERMISSION_DENIED message.
			span.RecordError(err)
			return nil, fmt.Errorf("execute query: %w: the query writes to the graph, but ExecuteQuery is read-only; write with CreateNode, CreateEdge, BulkCreateEdges or SupersedeFact instead", graphdb.ErrReadOnlyViolation)
		}
		return nil, fmt.Errorf("execute query: %w", classifyErr(ctx, tenant, err))
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
		return nil, fmt.Errorf("execute query: iterate: %w", classifyErr(ctx, tenant, err))
	}

	if err := c.commit(span, tx, "execute query"); err != nil {
		return nil, err
	}
	return result, nil
}

// SupersedeFact sets valid_to on a node or edge, marking it as superseded.
func (c *ageClient) SupersedeFact(ctx context.Context, tenant string, req graphdb.SupersedeRequest) (_ bool, err error) {
	if err := graphdb.CheckSupersedeRequest(req); err != nil {
		return false, fmt.Errorf("supersede fact: %w", err)
	}
	start := time.Now()
	ctx, span := c.startSpan(ctx, "graphdb.SupersedeFact", tenant)
	defer span.End()
	defer func() { c.record(ctx, "supersede_fact", start, err) }()

	tx, err := c.beginTx(ctx, tenant)
	if err != nil {
		failSpan(span, err)
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	gn := graphName(tenant)
	ts := escapeCypher(graphdb.FormatTime(req.SupersededAt))

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

	query := cypherSQL(gn, cypher, "v agtype")

	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return false, fmt.Errorf("supersede fact: %w", classifyErr(ctx, tenant, err))
	}
	updated := rows.Next()
	if closeErr := rows.Close(); closeErr != nil {
		span.SetStatus(codes.Error, closeErr.Error())
		return false, fmt.Errorf("supersede fact: close: %w", closeErr)
	}
	if err := rows.Err(); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return false, fmt.Errorf("supersede fact: %w", classifyErr(ctx, tenant, err))
	}

	if err := c.commit(span, tx, "supersede fact"); err != nil {
		return false, err
	}
	return updated, nil
}
