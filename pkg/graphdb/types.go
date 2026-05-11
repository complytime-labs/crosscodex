package graphdb

// Node represents a graph vertex.
type Node struct {
	ID         string         // Unique node identifier
	Label      string         // Node label (e.g., "Control", "Requirement")
	Properties map[string]any // Node properties
}

// Edge represents a graph relationship.
type Edge struct {
	ID         string         // Unique edge identifier
	Label      string         // Relationship type (e.g., "MAPS_TO", "DEPENDS_ON")
	Source     string         // Source node ID
	Target     string         // Target node ID
	Properties map[string]any // Edge properties
}

// Result represents the result of a graph query.
type Result struct {
	Nodes []Node // All nodes in the result
	Edges []Edge // All edges in the result
}

// TraversalQuery defines a graph traversal operation.
type TraversalQuery struct {
	StartNode  string   // Starting node ID
	Direction  string   // "outbound", "inbound", or "both"
	EdgeLabels []string // Filter by edge labels (empty = all)
	MaxDepth   int      // Maximum traversal depth (0 = unlimited)
}
