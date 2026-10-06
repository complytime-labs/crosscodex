package agedriver

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/pkg/graphdb"
)

// cypherDollarTag is the PostgreSQL dollar-quote tag wrapping Cypher queries
// in ag_catalog.cypher() calls. A tagged dollar-quote prevents content
// containing bare $$ from escaping the SQL string boundary. escapeCypher
// strips this tag from any content as a defense-in-depth measure.
const cypherDollarTag = "$cypher$"

// cypherSQL wraps a Cypher query for graph gn in the ag_catalog.cypher() SQL
// call, dollar-quoted with cypherDollarTag. cols is the AS column list, such
// as "v agtype". The graph name is escaped here; cypher must already be built
// from escaped values and checked identifiers, and cols must be a constant.
func cypherSQL(gn, cypher, cols string) string {
	return fmt.Sprintf(
		"SELECT * FROM ag_catalog.cypher('%s', "+cypherDollarTag+" %s "+cypherDollarTag+") AS (%s)",
		escapeCypher(gn), cypher, cols,
	)
}

// escapeCypher escapes backslashes and single quotes for Cypher string literals
// and strips the dollar-quote tag to prevent SQL injection via AGE's
// dollar-quoted Cypher embedding. The strip repeats until no tag remains: one
// pass over "$cyp$cypher$her$" leaves "$cypher$" behind. Each pass shortens
// the string, so the loop terminates.
func escapeCypher(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	return stripDollarTag(s)
}

// stripDollarTag removes cypherDollarTag until none remains. It is also the
// value AGE stores and returns for a string written through escapeCypher:
// escaping adds only backslashes, which are not part of the tag, so stripping
// before or after escaping removes the same characters.
func stripDollarTag(s string) string {
	for strings.Contains(s, cypherDollarTag) {
		s = strings.ReplaceAll(s, cypherDollarTag, "")
	}
	return s
}

// nodeToAGProperties serializes a Node's fields into a Cypher property map
// string such as {id: 'x', valid_from: '2024-01-01T00:00:00Z'}.
func nodeToAGProperties(n graphdb.Node) string {
	return joinPropertyPairs(nodePropertyPairs(n))
}

// nodeUpsertProperties is the property map UpsertNode assigns to an existing
// node bound to n with SET n = {...}: the node as CreateNode writes it, except
// that valid_from is read from the stored node (a validity start is
// historical, and moving it would hide the node from earlier AsOf reads),
// plus, when ValidTo is unset, SupersedeFact's valid_to and superseded_by read
// from the stored node. AGE evaluates n.<key> before the assignment and drops
// a key whose value is null, so an absent key stays absent.
func nodeUpsertProperties(n graphdb.Node) string {
	pairs := nodePropertyPairs(n)
	for i := range pairs {
		if pairs[i].key == "valid_from" {
			pairs[i].value = "n.valid_from"
		}
	}
	if n.ValidTo == nil {
		pairs = append(pairs,
			propertyPair{"valid_to", "n.valid_to"},
			propertyPair{"superseded_by", "n.superseded_by"},
		)
	}
	return joinPropertyPairs(pairs)
}

// nodePropertyPairs lists the properties a node is stored with, in the order
// nodeToAGProperties writes them: the Node fields, then Properties sorted by
// key, so the generated Cypher is deterministic.
func nodePropertyPairs(n graphdb.Node) []propertyPair {
	pairs := []propertyPair{
		{"id", fmt.Sprintf("'%s'", escapeCypher(n.ID))},
		{"valid_from", fmt.Sprintf("'%s'", escapeCypher(graphdb.FormatTime(n.ValidFrom)))},
	}
	if n.ValidTo != nil {
		pairs = append(pairs, propertyPair{"valid_to", fmt.Sprintf("'%s'", escapeCypher(graphdb.FormatTime(*n.ValidTo)))})
	}
	if n.CreatedBy != "" {
		pairs = append(pairs, propertyPair{"created_by", fmt.Sprintf("'%s'", escapeCypher(n.CreatedBy))})
	}
	if n.CreationMethod != "" {
		pairs = append(pairs, propertyPair{"creation_method", fmt.Sprintf("'%s'", escapeCypher(n.CreationMethod))})
	}
	for _, k := range slices.Sorted(maps.Keys(n.Properties)) {
		pairs = append(pairs, propertyPair{escapeCypher(k), cypherValue(n.Properties[k])})
	}
	return pairs
}

// joinPropertyPairs renders pairs as a Cypher map literal.
func joinPropertyPairs(pairs []propertyPair) string {
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = p.key + ": " + p.value
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// edgeToAGProperties serializes an Edge's fields into a Cypher property map.
func edgeToAGProperties(e graphdb.Edge) string {
	return joinPropertyPairs(edgePropertyPairs(e))
}

// propertyPair is one escaped Cypher map entry: key is safe to splice as a
// map key and value is a Cypher literal.
type propertyPair struct{ key, value string }

// edgePropertyPairs lists the properties an edge is stored with, in the order
// edgeToAGProperties writes them.
func edgePropertyPairs(e graphdb.Edge) []propertyPair {
	pairs := []propertyPair{
		{"id", fmt.Sprintf("'%s'", escapeCypher(e.ID))},
		{"valid_from", fmt.Sprintf("'%s'", escapeCypher(graphdb.FormatTime(e.ValidFrom)))},
	}
	if e.ValidTo != nil {
		pairs = append(pairs, propertyPair{"valid_to", fmt.Sprintf("'%s'", escapeCypher(graphdb.FormatTime(*e.ValidTo)))})
	}
	if e.DeterminedBy != "" {
		pairs = append(pairs, propertyPair{"determined_by", fmt.Sprintf("'%s'", escapeCypher(e.DeterminedBy))})
	}
	if e.DeterminationType != "" {
		pairs = append(pairs, propertyPair{"determination_type", fmt.Sprintf("'%s'", escapeCypher(e.DeterminationType))})
	}
	if e.Confidence != 0 {
		pairs = append(pairs, propertyPair{"confidence", fmt.Sprintf("%g", e.Confidence)})
	}
	if e.Supersedes != "" {
		pairs = append(pairs, propertyPair{"supersedes", fmt.Sprintf("'%s'", escapeCypher(e.Supersedes))})
	}
	for _, k := range slices.Sorted(maps.Keys(e.Properties)) {
		pairs = append(pairs, propertyPair{escapeCypher(k), cypherValue(e.Properties[k])})
	}
	return pairs
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

// paramToken matches a $name parameter reference in an openCypher query.
var paramToken = regexp.MustCompile(`\$[A-Za-z_][A-Za-z0-9_]*`)

// substituteParams replaces each whole $name token that has a matching
// parameter with the escaped value as a Cypher string literal. Tokens with
// no matching parameter are left unchanged, so $id never rewrites part of
// $id2, and inserted values are never substituted again.
func substituteParams(cypher string, params map[string]string) string {
	return paramToken.ReplaceAllStringFunc(cypher, func(tok string) string {
		v, ok := params[tok[1:]]
		if !ok {
			return tok
		}
		return "'" + escapeCypher(v) + "'"
	})
}

// missingGraphPattern matches Apache AGE's error for a cypher() call naming
// a graph that does not exist. It matches the wording, not the graph name, so
// it does not depend on how AGE renders the name. graphdb.CheckTenant caps
// tenant IDs at 52 characters, so graphName(tenant) always fits PostgreSQL's
// 63-byte identifier limit and is never truncated. The quoted-name group is
// restricted to non-quote characters so a message like `graph "g" exists but
// label "foo" does not exist` is not misclassified as a missing-graph error.
//
// The match is on text because AGE reports a missing graph as SQLSTATE 3F000
// (invalid_schema_name), which PostgreSQL also raises for any other missing
// schema, so the code alone cannot identify it. Reading the code would also
// tie this package, which talks to PostgreSQL only through database/sql, to
// the pgx driver. graphdb_integration_bdd_test.go pins both the message and
// the code against a real AGE, so an AGE upgrade that changes either fails
// there first.
var missingGraphPattern = regexp.MustCompile(`graph "[^"]*" does not exist`)

// classifyErr replaces AGE's missing-graph error with one wrapping
// graphdb.ErrGraphNotFound, so callers can map it without parsing AGE
// messages. The returned error names the tenant the caller passed and drops
// AGE's text, which names the internal graph and a SQLSTATE and would reach
// RPC clients through the NOT_FOUND message. AGE's error is recorded on the
// span in ctx instead, for operators; the tenant was validated by
// graphdb.CheckTenant before any query ran, so the graph name it carries is
// safe for telemetry. Other errors, and nil, pass through unchanged.
func classifyErr(ctx context.Context, tenant string, err error) error {
	if err != nil && missingGraphPattern.MatchString(err.Error()) {
		trace.SpanFromContext(ctx).RecordError(err)
		return fmt.Errorf("graph for tenant %q does not exist; create the tenant (or call CreateGraph) before using its graph: %w", tenant, graphdb.ErrGraphNotFound)
	}
	return err
}
