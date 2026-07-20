package graphdb

import "time"

type Node struct {
	ID             string
	Label          string
	Properties     map[string]any
	ValidFrom      time.Time
	ValidTo        *time.Time
	CreatedBy      string
	CreationMethod string
}

type Edge struct {
	ID                string
	Label             string
	Properties        map[string]any
	ValidFrom         time.Time
	ValidTo           *time.Time
	DeterminedBy      string
	DeterminationType string
	Confidence        float64
	Supersedes        string
}

type Relationship struct {
	Source Node
	Edge   Edge
	Target Node
}

type RelationshipQuery struct {
	SourceLabel string
	TargetLabel string
	EdgeLabel   string
	Properties  map[string]any
}

type TraversalQuery struct {
	StartNode  string
	Direction  string // "outbound", "inbound", or "both"
	EdgeLabels []string
	MaxDepth   int // 0 = unlimited
	AsOf       *time.Time
}

type Path struct {
	Nodes []Node
	Edges []Edge
}

func (p Path) ValidAt(t time.Time) bool {
	for _, n := range p.Nodes {
		if n.ValidFrom.After(t) || (n.ValidTo != nil && !n.ValidTo.After(t)) {
			return false
		}
	}
	for _, e := range p.Edges {
		if e.ValidFrom.After(t) || (e.ValidTo != nil && !e.ValidTo.After(t)) {
			return false
		}
	}
	return true
}

// EdgeWithEndpoints wraps an Edge with its source and target node IDs.
// GetEdge returns this type because source/target are structural (MATCH
// clause endpoints), not edge properties stored on the Edge struct.
type EdgeWithEndpoints struct {
	Edge
	SourceID string
	TargetID string
}

// BulkEdge groups the parameters of CreateEdge for batch use.
type BulkEdge struct {
	SourceID string
	TargetID string
	Edge     Edge
}

// QueryValue is a tagged union for values returned by ExecuteQuery.
type QueryValue struct {
	Type       QueryValueType
	NodeVal    *Node
	EdgeVal    *EdgeWithEndpoints
	ScalarVal  string
	IntegerVal int64
	FloatVal   float64
	BoolVal    bool
}

// QueryValueType tags the active field in a QueryValue.
type QueryValueType int

const (
	QueryValueNode QueryValueType = iota
	QueryValueEdge
	QueryValueScalar
	QueryValueInteger
	QueryValueFloat
	QueryValueBool
)

// QueryRow is a row of values returned by ExecuteQuery.
type QueryRow struct {
	Values []QueryValue
}

// SupersedeRequest identifies a node or edge to mark as superseded.
// Exactly one of NodeID or EdgeID must be set.
type SupersedeRequest struct {
	NodeID            string
	EdgeID            string
	SupersededAt      time.Time
	SupersededByJobID string
}
