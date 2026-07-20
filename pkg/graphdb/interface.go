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

	// GetNode retrieves a single node by ID from the tenant's graph.
	// Returns ErrNodeNotFound if no node matches.
	GetNode(ctx context.Context, tenant, nodeID string) (*Node, error)

	// GetEdge retrieves a single edge by ID, including source/target node IDs.
	// Returns ErrEdgeNotFound if no edge matches.
	GetEdge(ctx context.Context, tenant, edgeID string) (*EdgeWithEndpoints, error)

	// BulkCreateEdges creates multiple edges in a single transaction.
	// Returns the list of created edge IDs. Stops on first error.
	BulkCreateEdges(ctx context.Context, tenant string, edges []BulkEdge) ([]string, error)

	// ExecuteQuery runs a read-only openCypher query against the tenant's graph.
	// Parameters are substituted via escapeCypher — AGE does not support $param binding.
	// The transaction is forced read-only at the SQL level.
	//
	// The AS clause declares a single output column — queries must RETURN a single
	// expression. Multi-column returns produce a PostgreSQL error.
	//
	// The tenant parameter on every GraphDB method is the multi-cluster routing key.
	// A TenantRouter implementing GraphDB can dispatch to per-tenant ageClient
	// instances connected to dedicated AGE clusters without changing callers.
	ExecuteQuery(ctx context.Context, tenant, cypher string, params map[string]string) ([]QueryRow, error)

	// SupersedeFact sets valid_to on a node or edge, marking it as superseded.
	// Returns true if the entity was found and updated, false if not found.
	SupersedeFact(ctx context.Context, tenant string, req SupersedeRequest) (bool, error)
}
