package graphdb

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"

	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// identifierPattern is the identifier grammar every driver accepts for labels
// and property keys. agedriver splices both into Cypher text unquoted, where
// escaping cannot help, so anything wider than a plain identifier would let a
// caller rewrite the query.
var identifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// CheckIdentifier returns an error wrapping ErrInvalidCypher unless s is an
// identifier. kind names the field in the message, for example "label".
func CheckIdentifier(kind, s string) error {
	if identifierPattern.MatchString(s) {
		return nil
	}
	return fmt.Errorf("%w: %s %q must be an identifier: letters, digits and underscores, not starting with a digit. Rename it, for example by replacing spaces and dashes with underscores",
		ErrInvalidCypher, kind, s)
}

// CheckRelationshipQuery applies CheckIdentifier to every label q sets and
// CheckPropertyKeys to its property filter.
func CheckRelationshipQuery(q RelationshipQuery) error {
	for _, l := range [][2]string{{"source label", q.SourceLabel}, {"target label", q.TargetLabel}, {"edge label", q.EdgeLabel}} {
		if l[1] == "" {
			continue
		}
		if err := CheckIdentifier(l[0], l[1]); err != nil {
			return err
		}
	}
	return CheckPropertyKeys(q.Properties)
}

// CheckPropertyKeys applies CheckIdentifier to every key in props, in sorted
// order so the reported key is deterministic.
func CheckPropertyKeys(props map[string]any) error {
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := CheckIdentifier("property key", k); err != nil {
			return err
		}
	}
	return nil
}

// supersedeField names where SupersedeFact takes the superseded_by value from.
const supersedeField = "SupersedeRequest.SupersededByJobID (via SupersedeFact)"

// reservedNodeKeys maps each property key a driver writes for a node itself
// to the field it writes it from. Both drivers write these after, or merged
// with, caller Properties, so a caller-supplied copy would silently override
// (or be overridden by) the field and the drivers would disagree.
var reservedNodeKeys = map[string]string{
	"id":              "Node.ID",
	"valid_from":      "Node.ValidFrom",
	"valid_to":        "Node.ValidTo",
	"created_by":      "Node.CreatedBy",
	"creation_method": "Node.CreationMethod",
	"superseded_by":   supersedeField,
}

// reservedEdgeKeys is reservedNodeKeys for edges. A caller "id" property in
// particular would replace Edge.ID after the ErrEdgeExists check ran.
var reservedEdgeKeys = map[string]string{
	"id":                 "Edge.ID",
	"valid_from":         "Edge.ValidFrom",
	"valid_to":           "Edge.ValidTo",
	"determined_by":      "Edge.DeterminedBy",
	"determination_type": "Edge.DeterminationType",
	"confidence":         "Edge.Confidence",
	"supersedes":         "Edge.Supersedes",
	"superseded_by":      supersedeField,
}

// CheckNodeProperties applies CheckPropertyKeys to the Properties of a node
// being written and also rejects, with an error wrapping ErrInvalidCypher,
// any key the driver writes itself from a Node field (id, valid_from, ...).
func CheckNodeProperties(props map[string]any) error {
	return checkWrittenProperties(props, reservedNodeKeys)
}

// CheckEdgeProperties is CheckNodeProperties for the Properties of an edge
// being written (CreateEdge, BulkCreateEdges).
func CheckEdgeProperties(props map[string]any) error {
	return checkWrittenProperties(props, reservedEdgeKeys)
}

// CheckNode validates a node being written (CreateNode, UpsertNode): ID,
// Label and ValidFrom are required, the label must pass CheckIdentifier and
// Properties must pass CheckNodeProperties. Errors are unprefixed so each
// driver method can name itself.
func CheckNode(n Node) error {
	if n.ID == "" {
		return errors.New("id is required")
	}
	if n.Label == "" {
		return errors.New("label is required")
	}
	if err := CheckIdentifier("label", n.Label); err != nil {
		return err
	}
	if err := CheckNodeProperties(n.Properties); err != nil {
		return err
	}
	if n.ValidFrom.IsZero() {
		return errors.New("valid_from is required")
	}
	return nil
}

// CheckEdge validates an edge being written (CreateEdge, BulkCreateEdges):
// Label, both endpoint IDs and ValidFrom are required, the label must pass
// CheckIdentifier and Properties must pass CheckEdgeProperties. Edge.ID is
// optional. Errors are unprefixed so each driver method can name itself.
func CheckEdge(sourceID, targetID string, e Edge) error {
	if e.Label == "" {
		return errors.New("label is required")
	}
	if err := CheckIdentifier("label", e.Label); err != nil {
		return err
	}
	if err := CheckEdgeProperties(e.Properties); err != nil {
		return err
	}
	if sourceID == "" || targetID == "" {
		return errors.New("source and target are required")
	}
	if e.ValidFrom.IsZero() {
		return errors.New("valid_from is required")
	}
	return nil
}

// CheckSupersedeRequest validates a SupersedeFact request: exactly one of
// NodeID and EdgeID is set, and SupersededAt is set. Errors are unprefixed so
// each driver method can name itself.
func CheckSupersedeRequest(req SupersedeRequest) error {
	if req.NodeID == "" && req.EdgeID == "" {
		return errors.New("node_id or edge_id is required")
	}
	if req.NodeID != "" && req.EdgeID != "" {
		return errors.New("set node_id or edge_id, not both")
	}
	if req.SupersededAt.IsZero() {
		return errors.New("superseded_at is required")
	}
	return nil
}

// CheckRequiresEdge validates a REQUIRES edge being written
// (CreateRequiresEdge): SourceID, TargetID and AnalyzedAt are required.
// Errors are unprefixed so each driver method can name itself.
func CheckRequiresEdge(e RequiresEdge) error {
	if e.SourceID == "" {
		return errors.New("source_id is required")
	}
	if e.TargetID == "" {
		return errors.New("target_id is required")
	}
	if e.AnalyzedAt.IsZero() {
		return errors.New("analyzed_at is required")
	}
	return nil
}

// ReservedNodeKeys returns a copy of the node property keys a driver writes
// itself, mapped to the Go field each is written from.
func ReservedNodeKeys() map[string]string { return maps.Clone(reservedNodeKeys) }

// ReservedEdgeKeys is ReservedNodeKeys for edges.
func ReservedEdgeKeys() map[string]string { return maps.Clone(reservedEdgeKeys) }

// ReservedPropertyError is returned (wrapping ErrInvalidCypher) when written
// Properties use a key the driver writes itself. Field names the Go field the
// key is written from, as in ReservedNodeKeys, so an RPC layer can name its
// own field instead.
type ReservedPropertyError struct {
	Key   string
	Field string
}

func (e *ReservedPropertyError) Error() string {
	return fmt.Sprintf("%s: property key %q is reserved because the driver writes it from %s. Set that field instead of Properties[%q], or rename the property",
		ErrInvalidCypher, e.Key, e.Field, e.Key)
}

func (e *ReservedPropertyError) Unwrap() error { return ErrInvalidCypher }

func checkWrittenProperties(props map[string]any, reserved map[string]string) error {
	if err := CheckPropertyKeys(props); err != nil {
		return err
	}
	for _, k := range slices.Sorted(maps.Keys(props)) {
		if field, ok := reserved[k]; ok {
			return &ReservedPropertyError{Key: k, Field: field}
		}
	}
	return nil
}

// CheckTenant returns nil when id is a valid tenant ID
// (tenant.ValidateTenantID). Drivers call it themselves instead of trusting
// callers, because agedriver splices the tenant into SQL as part of the graph
// name crosscodex_<tenant>, and PostgreSQL truncates a longer graph name to
// 63 bytes so two tenants could share a graph. An empty id returns
// ErrTenantRequired itself; a malformed one returns an error matching both
// ErrTenantRequired and tenant.ErrInvalidTenant whose message states the
// rule once.
func CheckTenant(id string) error {
	if id == "" {
		return ErrTenantRequired
	}
	if err := tenant.ValidateTenantID(id); err != nil {
		return &invalidTenantError{cause: err}
	}
	return nil
}

// invalidTenantError matches both ErrTenantRequired and the
// tenant.ValidateTenantID error, and reads exactly as the latter.
type invalidTenantError struct{ cause error }

func (e *invalidTenantError) Error() string { return e.cause.Error() }

func (e *invalidTenantError) Unwrap() []error { return []error{ErrTenantRequired, e.cause} }
