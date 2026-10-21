package graphdb

import (
	"errors"
	"fmt"
)

var (
	ErrNodeNotFound      = errors.New("node not found")
	ErrNodeExists        = errors.New("node already exists")
	ErrNodeIDConflict    = errors.New("node ID already used under another label")
	ErrLabelKindConflict = errors.New("label already used for the other element kind")
	ErrEdgeNotFound      = errors.New("edge not found")
	ErrInvalidCypher     = errors.New("invalid openCypher query")
	ErrGraphNotFound     = errors.New("graph not found")
	ErrTenantRequired    = errors.New("tenant ID required")
	ErrEdgeExists        = errors.New("edge already exists")
	ErrReadOnlyViolation = errors.New("read-only transaction violation")
	ErrNotSupported      = errors.New("operation not supported by this graph driver")
)

// NodeIDConflictError is returned (wrapping ErrNodeIDConflict) when CreateNode
// or UpsertNode is given an ID that a node under another label already has.
// It deliberately does not wrap ErrNodeExists: graph writers treat that as
// success, and a conflict must never be mistaken for an idempotent re-write.
type NodeIDConflictError struct {
	ID          string
	Label       string
	StoredLabel string
}

func (e *NodeIDConflictError) Error() string {
	return fmt.Sprintf("%s: node ID %q belongs to a node labeled %s, so it cannot also identify a node labeled %s. "+
		"Node IDs are unique across labels in a tenant's graph so that edge endpoints resolve to one node. "+
		"Give the %s node an ID no other node uses (for example graphdb.DerivedID with a kind tag naming the label), or write the existing %s node instead",
		ErrNodeIDConflict, e.ID, e.StoredLabel, e.Label, e.Label, e.StoredLabel)
}

func (e *NodeIDConflictError) Unwrap() error { return ErrNodeIDConflict }

// LabelKindConflictError is returned (wrapping ErrLabelKindConflict) when a
// write uses a label that already names the other element kind in the
// tenant's graph: a node label for an edge, or an edge label for a node. Kind
// is the kind the write asked for and StoredKind the kind the label already
// has, each "node" or "edge". Apache AGE stores each label as one table of one
// kind and keeps label names unique per graph across kinds, so it cannot hold
// both; every driver rejects the reuse so the contract is the portable
// intersection. It deliberately does not wrap ErrNodeExists or ErrEdgeExists:
// graph writers treat those as success, and a conflict is not a re-write.
type LabelKindConflictError struct {
	Label      string
	Kind       string
	StoredKind string
}

func (e *LabelKindConflictError) Error() string {
	article := "a"
	if e.StoredKind == "edge" {
		article = "an"
	}
	return fmt.Sprintf("%s: cannot write %s label %q: %q is already %s %s label. "+
		"A label names either nodes or edges in a tenant's graph, never both; use a different label name",
		ErrLabelKindConflict, e.Kind, e.Label, e.Label, article, e.StoredKind)
}

func (e *LabelKindConflictError) Unwrap() error { return ErrLabelKindConflict }

// BulkEdgeError is returned by BulkCreateEdges when one edge of the batch
// fails: Index is the zero-based position of the first failing edge and Err
// the error CreateEdge would return for it. Callers that need the index use
// errors.As rather than parsing the message. It wraps Err, so errors.Is still
// finds ErrNodeNotFound, ErrEdgeExists or ErrInvalidCypher, and errors.As
// still finds a *ReservedPropertyError.
type BulkEdgeError struct {
	Index int
	Err   error
}

func (e *BulkEdgeError) Error() string {
	return fmt.Sprintf("bulk create edges [%d]: %v", e.Index, e.Err)
}

func (e *BulkEdgeError) Unwrap() error { return e.Err }
