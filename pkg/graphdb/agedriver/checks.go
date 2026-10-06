package agedriver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/complytime-labs/crosscodex/pkg/graphdb"
)

// countQuery runs a Cypher statement that returns a single count(...) value.
func countQuery(ctx context.Context, tx *sql.Tx, tenant, gn, cypher string) (int64, error) {
	query := cypherSQL(gn, cypher, "c agtype")
	var raw string
	if err := tx.QueryRowContext(ctx, query).Scan(&raw); err != nil {
		return 0, classifyErr(ctx, tenant, err)
	}
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse count %q: %w", raw, err)
	}
	return n, nil
}

// storedNodeLabels returns the sorted labels of every node whose id property
// is id. CreateNode and UpsertNode call it under lockNode. Node IDs are
// unique across labels, so it returns at most one label, except in a graph
// written before #148; sorting keeps the label a conflict error names
// deterministic there.
func storedNodeLabels(ctx context.Context, tx *sql.Tx, tenant, gn, id string) ([]string, error) {
	query := cypherSQL(gn, fmt.Sprintf("MATCH (n {id: '%s'}) RETURN n", escapeCypher(id)), "v agtype")
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("check existing: %w", classifyErr(ctx, tenant, err))
	}
	var labels []string
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("check existing: scan: %w", err)
		}
		n, err := parseAGVertex(raw)
		if err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("check existing: %w", err)
		}
		labels = append(labels, n.Label)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("check existing: %w", classifyErr(ctx, tenant, err))
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("check existing: close: %w", err)
	}
	slices.Sort(labels)
	return labels, nil
}

// checkEdgeWrite enforces the edge-creation contract inside the caller's
// transaction, before the CREATE: each endpoint ID must match exactly one
// node, and a non-empty edge ID must be unused.
//
// Edge writes take no lock, unlike node writes, which serialize on lockNode.
// AGE has no unique constraints on property values, so two concurrent
// writers of one non-empty edge ID can both pass this check and both CREATE,
// leaving two edges with that ID. That window is accepted: the graph is a
// materialized view that a rebuild replaces (see "Graph Data Model" in
// docs/dev/design-principles.md).
func checkEdgeWrite(ctx context.Context, tx *sql.Tx, tenant, gn, sourceID, targetID, edgeID string) error {
	endpoints := []struct{ role, id string }{{"source", sourceID}, {"target", targetID}}
	for _, ep := range endpoints {
		n, err := countQuery(ctx, tx, tenant, gn, fmt.Sprintf("MATCH (n {id: '%s'}) RETURN count(n)", escapeCypher(ep.id)))
		if err != nil {
			return fmt.Errorf("check %s node: %w", ep.role, err)
		}
		if err := endpointErr(ep.role, ep.id, n); err != nil {
			return err
		}
	}
	if edgeID == "" {
		return nil
	}
	n, err := countQuery(ctx, tx, tenant, gn, fmt.Sprintf("MATCH ()-[e {id: '%s'}]->() RETURN count(e)", escapeCypher(edgeID)))
	if err != nil {
		return fmt.Errorf("check edge id: %w", err)
	}
	if n > 0 {
		return fmt.Errorf("edge %q: %w", edgeID, graphdb.ErrEdgeExists)
	}
	return nil
}

// endpointErr reports an edge endpoint whose ID matched n nodes; exactly one
// match is required.
func endpointErr(role, id string, n int64) error {
	switch {
	case n == 0:
		return fmt.Errorf("%s: node %q: %w", role, id, graphdb.ErrNodeNotFound)
	case n > 1:
		return fmt.Errorf("%s: ambiguous node ID %q: %d nodes share it. Node IDs have been unique per graph since #148, so this graph was written before then; rebuild the tenant's graph (see \"Upgrade note (#148)\" in docs/dev/design-principles.md)", role, id, n)
	}
	return nil
}

// checkBulkEdgeWrites enforces the checkEdgeWrite contract for a whole batch
// with one or two queries, so the cost does not grow with the batch size:
// one resolves every distinct endpoint ID and one looks up every non-empty
// edge ID. It then walks the batch in order and reports the first failure
// with the same wording checkEdgeWrite uses, prefixed with the edge's index.
// A non-empty edge ID repeated within the batch is reported at its second
// occurrence, as if the earlier edge had already been created.
//
// Neither query has a property index to use (see "How bulk writes work" and
// "Why there are no indexes" in docs/dev/design-principles.md), so each costs
// a fixed number of sequential scans. Like checkEdgeWrite it takes no lock,
// so the same concurrent-duplicate window applies.
func checkBulkEdgeWrites(ctx context.Context, tx *sql.Tx, tenant, gn string, edges []graphdb.BulkEdge) error {
	var nodeIDs, edgeIDs []string
	seenNode := make(map[string]bool)
	seenEdge := make(map[string]bool)
	for _, be := range edges {
		for _, id := range []string{be.SourceID, be.TargetID} {
			if !seenNode[id] {
				seenNode[id] = true
				nodeIDs = append(nodeIDs, id)
			}
		}
		if id := be.Edge.ID; id != "" && !seenEdge[id] {
			seenEdge[id] = true
			edgeIDs = append(edgeIDs, id)
		}
	}

	nodeCounts, err := idCounts(ctx, tx, tenant, gn,
		fmt.Sprintf("MATCH (n) WHERE n.id IN %s RETURN n.id, count(*)", cypherStringList(nodeIDs)))
	if err != nil {
		return fmt.Errorf("bulk create edges: check endpoint nodes: %w", err)
	}
	edgeCounts := map[string]int64{}
	if len(edgeIDs) > 0 {
		edgeCounts, err = idCounts(ctx, tx, tenant, gn,
			fmt.Sprintf("MATCH ()-[e]->() WHERE e.id IN %s RETURN e.id, count(*)", cypherStringList(edgeIDs)))
		if err != nil {
			return fmt.Errorf("bulk create edges: check edge ids: %w", err)
		}
	}

	// The queries return stored IDs, which escapeCypher has stripped of the
	// dollar-quote tag, so lookups strip it too. This matches checkEdgeWrite,
	// whose {id: '...'} match compares the stripped value.
	batchEdge := make(map[string]bool, len(edgeIDs))
	for i, be := range edges {
		endpoints := []struct{ role, id string }{{"source", be.SourceID}, {"target", be.TargetID}}
		for _, ep := range endpoints {
			if err := endpointErr(ep.role, ep.id, nodeCounts[stripDollarTag(ep.id)]); err != nil {
				return &graphdb.BulkEdgeError{Index: i, Err: err}
			}
		}
		id := be.Edge.ID
		if id == "" {
			continue
		}
		key := stripDollarTag(id)
		if edgeCounts[key] > 0 || batchEdge[key] {
			return &graphdb.BulkEdgeError{Index: i, Err: fmt.Errorf("edge %q: %w", id, graphdb.ErrEdgeExists)}
		}
		batchEdge[key] = true
	}
	return nil
}

// idCounts runs a Cypher statement returning (string id, count) rows and
// returns the counts keyed by ID. IDs absent from the result have count 0.
func idCounts(ctx context.Context, tx *sql.Tx, tenant, gn, cypher string) (map[string]int64, error) {
	query := cypherSQL(gn, cypher, "id agtype, c agtype")
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, classifyErr(ctx, tenant, err)
	}
	defer func() { _ = rows.Close() }()

	counts := make(map[string]int64)
	for rows.Next() {
		var idRaw, cRaw string
		if err := rows.Scan(&idRaw, &cRaw); err != nil {
			return nil, fmt.Errorf("scan id count row: %w", err)
		}
		// AGE renders an agtype string as a JSON string literal.
		var id string
		if err := json.Unmarshal([]byte(idRaw), &id); err != nil {
			return nil, fmt.Errorf("parse id %q: %w", idRaw, err)
		}
		n, err := strconv.ParseInt(strings.TrimSpace(cRaw), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse count %q: %w", cRaw, err)
		}
		counts[id] += n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate id count rows: %w", classifyErr(ctx, tenant, err))
	}
	return counts, nil
}

// cypherStringList renders ids as a Cypher list of escaped string literals.
func cypherStringList(ids []string) string {
	quoted := make([]string, len(ids))
	for i, id := range ids {
		quoted[i] = "'" + escapeCypher(id) + "'"
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}
