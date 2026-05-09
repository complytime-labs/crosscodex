package graphdb

import "context"

// Client executes openCypher queries against Apache AGE.
//
// Implementations must handle tenant isolation by scoping all queries
// to the appropriate graph partition.
type Client interface {
	// Execute runs an openCypher query with parameters.
	Execute(ctx context.Context, cypher string, params map[string]any) (*Result, error)

	// Traverse performs a breadth-first or depth-first traversal.
	Traverse(ctx context.Context, query TraversalQuery) ([]Node, []Edge, error)

	// CreateNode creates a new node with the specified label and properties.
	CreateNode(ctx context.Context, label string, properties map[string]any) (*Node, error)

	// CreateEdge creates a new relationship between two nodes.
	CreateEdge(ctx context.Context, source, target, label string, properties map[string]any) (*Edge, error)

	// DeleteNode removes a node and all connected edges.
	DeleteNode(ctx context.Context, nodeID string) error

	// DeleteEdge removes a relationship.
	DeleteEdge(ctx context.Context, edgeID string) error
}
