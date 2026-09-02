package agedriver

import (
	"fmt"
	"strings"
	"time"

	"github.com/complytime-labs/crosscodex/pkg/graphdb"
)

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
func nodeToAGProperties(n graphdb.Node) string {
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
func edgeToAGProperties(e graphdb.Edge) string {
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
