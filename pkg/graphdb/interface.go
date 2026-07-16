package graphdb

import (
	"context"
	"time"
)

// GraphDB executes openCypher queries against Apache AGE.
//
// Implementations scope all queries to a tenant-specific graph
// (crosscodex_{tenant_id}). The tenant parameter in each method
// identifies which graph to target.
type GraphDB interface {
	CreateGraph(ctx context.Context, tenant string) error
	CreateNode(ctx context.Context, tenant string, node Node) error
	CreateEdge(ctx context.Context, tenant, sourceID, targetID string, edge Edge) error
	CreateRequiresEdge(ctx context.Context, tenant string, reqEdge RequiresEdge) error
	QueryRelationships(ctx context.Context, tenant string, query RelationshipQuery) ([]Relationship, error)
	Traverse(ctx context.Context, tenant string, query TraversalQuery) ([]Path, error)
	QueryAsOf(ctx context.Context, tenant string, query RelationshipQuery, asOf time.Time) ([]Relationship, error)
}
