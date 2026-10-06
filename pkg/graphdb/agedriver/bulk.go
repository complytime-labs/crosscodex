package agedriver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/complytime-labs/crosscodex/pkg/graphdb"
)

// BulkCreateEdges creates multiple edges in a single transaction: all of
// them or none.
//
// The statement count does not grow with len(edges): one or two queries check
// the whole batch (checkBulkEdgeWrites) and one CREATE runs per distinct edge
// label (createLabelEdges). The CREATE text still grows with the batch and
// AGE's cost to evaluate a literal list grows faster than linearly, so
// callers bound the batch size (the graph RPC enforces graph.max_bulk_edges).
func (c *ageClient) BulkCreateEdges(ctx context.Context, tenant string, edges []graphdb.BulkEdge) (_ []string, err error) {
	// An empty batch returns nil, nil once the tenant is valid: it reads and
	// stores nothing, so it needs no graph. A malformed tenant is still
	// rejected (fail closed).
	if len(edges) == 0 {
		if err := graphdb.CheckTenant(tenant); err != nil {
			return nil, err
		}
		return nil, nil
	}

	for i, be := range edges {
		if err := graphdb.CheckEdge(be.SourceID, be.TargetID, be.Edge); err != nil {
			return nil, &graphdb.BulkEdgeError{Index: i, Err: err}
		}
	}

	start := time.Now()
	ctx, span := c.startSpan(ctx, "graphdb.BulkCreateEdges", tenant)
	defer span.End()
	defer func() { c.record(ctx, "bulk_create_edges", start, err) }()
	span.SetAttributes(
		attribute.Int("edge.count", len(edges)),
	)

	tx, err := c.beginTx(ctx, tenant)
	if err != nil {
		failSpan(span, err)
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	gn := graphName(tenant)
	if err := checkBulkEdgeWrites(ctx, tx, tenant, gn, edges); err != nil {
		markOutcome(span, err)
		return nil, err
	}

	var labels []string
	byLabel := make(map[string][]graphdb.BulkEdge)
	for _, be := range edges {
		if _, ok := byLabel[be.Edge.Label]; !ok {
			labels = append(labels, be.Edge.Label)
		}
		byLabel[be.Edge.Label] = append(byLabel[be.Edge.Label], be)
	}
	for _, label := range labels {
		created, err := countQuery(ctx, tx, tenant, gn, createLabelEdges(label, byLabel[label]))
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
			return nil, fmt.Errorf("bulk create edges: create %s edges: %w", label, err)
		}
		if want := int64(len(byLabel[label])); created != want {
			err := fmt.Errorf("bulk create edges: created %d of %d %s edges: an endpoint node was deleted or duplicated by a concurrent writer after the endpoint check; nothing was written, retry the batch", created, want, label)
			span.SetStatus(codes.Error, err.Error())
			return nil, err
		}
	}

	if err := c.commit(span, tx, "bulk create edges"); err != nil {
		return nil, err
	}

	ids := make([]string, len(edges))
	for i, be := range edges {
		ids[i] = be.Edge.ID
	}
	return ids, nil
}

// createLabelEdges builds one Cypher statement that creates every edge in
// edges, all of which share label, and returns how many it created.
//
// Each edge becomes a row {s: source, t: target, p: {properties}} of an
// UNWIND list. The CREATE map names the union of the rows' property keys and
// reads each with r.p['key']; a row without that key yields null, which AGE
// does not store, so every edge keeps exactly its own properties. Subscript
// access is used because AGE's parser rejects reserved words such as end
// after a dot. Endpoints are matched with WHERE equality rather than
// {id: ...}, so the planner can hash-join the rows against one scan of the
// vertices instead of scanning once per row.
func createLabelEdges(label string, edges []graphdb.BulkEdge) string {
	var keys []string
	seenKey := make(map[string]bool)
	rows := make([]string, len(edges))
	for i, be := range edges {
		pairs := edgePropertyPairs(be.Edge)
		props := make([]string, len(pairs))
		for j, p := range pairs {
			props[j] = p.key + ": " + p.value
			if !seenKey[p.key] {
				seenKey[p.key] = true
				keys = append(keys, p.key)
			}
		}
		rows[i] = fmt.Sprintf("{s: '%s', t: '%s', p: {%s}}",
			escapeCypher(be.SourceID), escapeCypher(be.TargetID), strings.Join(props, ", "))
	}
	assigns := make([]string, len(keys))
	for i, k := range keys {
		assigns[i] = fmt.Sprintf("%s: r.p['%s']", k, k)
	}
	return fmt.Sprintf(
		"UNWIND [%s] AS r MATCH (s) WHERE s.id = r.s MATCH (t) WHERE t.id = r.t CREATE (s)-[e:%s {%s}]->(t) RETURN count(e)",
		strings.Join(rows, ", "), escapeCypher(label), strings.Join(assigns, ", "),
	)
}
