package agedriver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// labelKind is ag_label.kind: 'v' for a vertex label, 'e' for an edge label.
type labelKind byte

const (
	vertexLabel labelKind = 'v'
	edgeLabel   labelKind = 'e'
)

func (k labelKind) String() string {
	if k == edgeLabel {
		return "edge"
	}
	return "vertex"
}

// propertyIDExpr is the expression AGE 1.5 compiles n.id to. An index on it
// serves WHERE n.id = '...' and n.id IN [...]; the map form {id: '...'}
// compiles to properties @> ... instead and cannot use it, which is why every
// driver ID lookup uses WHERE.
const propertyIDExpr = `ag_catalog.agtype_access_operator(VARIADIC ARRAY[properties, '"id"'::ag_catalog.agtype])`

// labelIndexName names one of a label's indexes: cc_<kind>_<first 16 hex
// characters of sha256(label)>_<col>. It is at most 27 bytes and only
// [a-z0-9_], so it never needs quoting, and the hash makes it unique per
// label within a graph schema.
func labelIndexName(kind labelKind, label, col string) string {
	sum := sha256.Sum256([]byte(label))
	return fmt.Sprintf("cc_%c_%s_%s", byte(kind), hex.EncodeToString(sum[:8]), col)
}

// labelIndexDDL returns the CREATE INDEX statements for a label's table:
// the graphid id and the id property for both kinds, plus start_id and
// end_id for edges, which serve traversal joins. relation is
// ag_label.relation::text, which PostgreSQL schema-qualifies and quotes as
// needed, so it is spliced as-is.
func labelIndexDDL(kind labelKind, label, relation string) []string {
	type index struct{ col, expr string }
	indexes := []index{{"gid", "id"}, {"pid", "(" + propertyIDExpr + ")"}}
	if kind == edgeLabel {
		indexes = append(indexes, index{"sid", "start_id"}, index{"eid", "end_id"})
	}
	stmts := make([]string, len(indexes))
	for i, ix := range indexes {
		stmts[i] = fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s ON %s (%s)",
			labelIndexName(kind, label, ix.col), relation, ix.expr)
	}
	return stmts
}

// labelKey identifies a label that ensureLabel has created and indexed.
type labelKey struct {
	graph string
	kind  labelKind
	label string
}

// ensureLabel makes sure label exists in the tenant's graph as a label of
// kind, with the indexes labelIndexDDL lists. Every write method calls it
// before its own transaction, so a label table never gains rows before its
// indexes exist. AGE would otherwise create the table lazily inside the
// write, without indexes, where DDL cannot run safely.
//
// A cache hit costs nothing and records nothing. A miss runs indexLabel in its
// own transaction and records ensure_label. On failure nothing is cached and
// the caller must not write: a label that cannot be indexed refuses writes
// instead of degrading to sequential scans, so grant and catalog problems
// surface at once. Reads never call it; an unindexed label table is scanned
// sequentially and stays correct.
func (c *ageClient) ensureLabel(ctx context.Context, tenant string, kind labelKind, label string) (err error) {
	key := labelKey{graph: graphName(tenant), kind: kind, label: label}
	if _, ok := c.labels.Load(key); ok {
		return nil
	}
	start := time.Now()
	defer func() { c.record(ctx, "ensure_label", start, err) }()
	if err := c.indexLabel(ctx, tenant, key); err != nil {
		return fmt.Errorf("ensure label %q: %w", label, err)
	}
	c.labels.Store(key, struct{}{})
	return nil
}

// indexLabel creates key's label if it is missing and its indexes if they
// are missing, in one transaction. A plain CREATE INDEX takes a SHARE lock,
// so on a populated legacy label table writers to that label wait for the
// build (readers do not); CONCURRENTLY is not used because a failed build
// would leave an INVALID index behind.
func (c *ageClient) indexLabel(ctx context.Context, tenant string, key labelKey) error {
	tx, err := c.beginTx(ctx, tenant)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Serialize first writers of one label name of either kind, across
	// processes too: without the lock two could both miss the label and the
	// second create_vlabel or create_elabel would fail with AGE's raw
	// "already exists" instead of the kind-mismatch error below. The kind is
	// not part of the key because AGE label names are unique per graph
	// across kinds. Under beginTx's READ COMMITTED the lookup after the wait
	// sees the label the previous holder committed. The leading "label" part
	// keeps the key apart from lockNode's "node" and CreateGraph's "graph"
	// keys.
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)",
		advisoryKey("label", key.graph, key.label)); err != nil {
		return fmt.Errorf("lock: %w", err)
	}

	lookup := func() (labelKind, string, error) {
		var kind, relation string
		err := tx.QueryRowContext(ctx,
			`SELECT l.kind::text, l.relation::text
			   FROM ag_catalog.ag_label l
			   JOIN ag_catalog.ag_graph g ON g.graphid = l.graph
			  WHERE g.name = $1 AND l.name = $2`,
			key.graph, key.label).Scan(&kind, &relation)
		if err != nil {
			return 0, "", err
		}
		return labelKind(kind[0]), relation, nil
	}
	stored, relation, err := lookup()
	if errors.Is(err, sql.ErrNoRows) {
		fn := "create_vlabel"
		if key.kind == edgeLabel {
			fn = "create_elabel"
		}
		if _, err := tx.ExecContext(ctx, "SELECT ag_catalog."+fn+"($1, $2)", key.graph, key.label); err != nil {
			return fmt.Errorf("create label: %w", classifyErr(ctx, tenant, err))
		}
		stored, relation, err = lookup()
	}
	if err != nil {
		return fmt.Errorf("look up label: %w", err)
	}
	if stored != key.kind {
		article := "a"
		if stored == edgeLabel {
			article = "an"
		}
		return fmt.Errorf("cannot write %s label %q for tenant %q: %q is already %s %s label. Labels are per-graph and single-kind; use a different label name",
			key.kind, key.label, tenant, key.label, article, stored)
	}

	for _, stmt := range labelIndexDDL(key.kind, key.label, relation) {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("create index: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// forgetGraphLabels drops every cached label of graph gn. CreateGraph calls
// it after creating a graph, because a graph of that name that existed before
// (dropped since) left entries its labels no longer back.
func (c *ageClient) forgetGraphLabels(gn string) {
	c.labels.Range(func(k, _ any) bool {
		if k.(labelKey).graph == gn {
			c.labels.Delete(k)
		}
		return true
	})
}
