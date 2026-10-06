package graphdb

import (
	"context"
	"time"
)

// GraphDB executes openCypher graph queries.
//
// Implementations scope all queries to a tenant-specific graph
// identified by the tenant parameter passed to each method.
//
// Contract (every driver honors it; internal/testspecs.GraphDBContractBehavior
// enforces it, except the clauses its doc comment lists as tested per driver:
// ExecuteQuery execution, concurrent first-time CreateGraph and the
// "ambiguous node ID" error for graphs written before #148):
//
//   - Field, identifier (CheckIdentifier) and reserved-key validation run
//     before the tenant check. The tenant check is
//     CheckTenant: an empty tenant returns ErrTenantRequired, and a tenant
//     that fails tenant.ValidateTenantID (at most 52 characters, so the
//     graph name crosscodex_<tenant> fits PostgreSQL's 63-byte limit) returns
//     an error wrapping ErrTenantRequired that states the rule. Either way
//     nothing is read or stored.
//   - Labels and property keys, written or used as query filters, must pass
//     CheckIdentifier; anything else returns an error wrapping
//     ErrInvalidCypher and stores nothing.
//   - Written Properties (CreateNode, UpsertNode, CreateEdge, BulkCreateEdges) must not
//     use a key the driver writes itself from a Node or Edge field (id,
//     valid_from, valid_to, ...; the sets are ReservedNodeKeys and
//     ReservedEdgeKeys). Such a key returns a *ReservedPropertyError, which
//     wraps ErrInvalidCypher, and stores nothing. Query property filters may
//     use reserved keys.
//   - Operations against a tenant whose graph does not exist return an error
//     wrapping ErrGraphNotFound. CreateGraph is idempotent, including for
//     concurrent first-time calls for one tenant: every call returns nil.
//   - Node IDs are unique across labels within a tenant's graph. CreateNode
//     or UpsertNode given an ID that a node under another label already has
//     returns a *NodeIDConflictError, which wraps ErrNodeIDConflict (never
//     ErrNodeExists, which graph writers treat as success), and changes
//     nothing.
//   - CreateNode rejects a node whose (label, ID) pair already exists with an
//     error wrapping ErrNodeExists and keeps the stored node unchanged.
//   - UpsertNode validates exactly like CreateNode. With no node under the ID
//     it creates one exactly as CreateNode would and returns created=true.
//     With a node under the same label it replaces the stored node in place
//     and returns created=false: id and valid_from stay as stored
//     (node.ValidFrom applies only on create, because a validity start is
//     historical and moving it would hide the node from earlier AsOf reads),
//     created_by and creation_method are rewritten from the Node fields (an
//     empty string removes the key), and the caller-owned properties become
//     exactly node.Properties, so a stored key absent from node.Properties is
//     removed. valid_to and superseded_by belong to SupersedeFact: with
//     node.ValidTo nil both are kept as stored; with node.ValidTo set,
//     valid_to is overwritten and superseded_by is removed. Edges attached to
//     the node are untouched.
//   - CreateNode and UpsertNode are atomic per ID, including against each
//     other and across labels: concurrent calls for a new ID create exactly
//     one node. One call creates it (CreateNode returns nil or UpsertNode
//     returns created=true); the others behave as if it already existed, so
//     same-label CreateNode calls return ErrNodeExists, same-label UpsertNode
//     calls update it, and calls under another label return
//     ErrNodeIDConflict. agedriver takes a transaction-scoped PostgreSQL
//     advisory lock on (graph, ID) before its MATCH; memdriver holds its
//     mutex.
//   - Edge creation (CreateEdge, CreateRequiresEdge, BulkCreateEdges) requires
//     both endpoints to exist: no match returns an error wrapping
//     ErrNodeNotFound. An ID matching more than one node returns an
//     "ambiguous node ID" error; only a graph written before #148 can hold
//     one, because writers then allowed an ID under several labels.
//   - Creating an edge whose non-empty ID already exists returns an error
//     wrapping ErrEdgeExists. Empty IDs are not deduplicated.
//   - CreateRequiresEdge writes id = RequiresEdge.EdgeID() and
//     valid_from = AnalyzedAt, so it is deduplicated and visible to QueryAsOf.
//   - BulkCreateEdges is all-or-nothing: on any error nothing is persisted and
//     the returned ID slice is nil. Empty input with a valid tenant returns
//     nil, nil; a malformed tenant is rejected even when the input is empty.
//   - Temporal properties are stored with FormatTime.
//   - QueryRelationships returns edges without valid_to and does not check
//     node validity. QueryAsOf filters edges only: valid_from <= t and
//     (valid_to unset or t < valid_to).
//   - Traverse returns every path of length 1..MaxDepth (0 = unlimited) that
//     does not reuse an edge. Direction "inbound" and "both" are honored; any
//     other value means outbound. A missing start node yields no paths and no
//     error. AsOf filters paths with Path.ValidAt.
//   - SupersedeFact overwrites valid_to unconditionally and records
//     superseded_by when SupersededByJobID is set. A request that sets both
//     NodeID and EdgeID is rejected and changes nothing.
//   - Read-back Properties include the reserved keys (id, valid_from, ...).
//     Property values of type string, bool, float64/float32 and int/int64
//     round-trip (numbers as float64); any other value type, including
//     slices, is stored as its fmt %v string (so []string{"a","b"} reads
//     back as "[a b]"). The only list a driver returns is RequiresEdge
//     models, as []any.
//   - ExecuteQuery replaces whole $name tokens; a token with no matching
//     parameter is left unchanged. A driver without an openCypher engine
//     returns an error wrapping ErrNotSupported.
type GraphDB interface {
	CreateGraph(ctx context.Context, tenant string) error

	// CreateNode stores a node in the tenant's graph. A node whose label and
	// ID both match a stored node returns an error wrapping ErrNodeExists and
	// leaves the stored node unchanged; internal/graph's subscriber writers and
	// the artifacts materializer's ArtifactType nodes treat that as success,
	// while the artifacts materializer accepts an existing Artifact node only
	// when its stored content matches and otherwise returns an error wrapping
	// ErrNodeExists. An ID stored under another label
	// returns a *NodeIDConflictError (ErrNodeIDConflict), which no caller
	// treats as success.
	CreateNode(ctx context.Context, tenant string, node Node) error

	// UpsertNode creates the node when no node has its ID and returns
	// created=true; otherwise, when the stored node has the same label, it
	// replaces the stored node's properties with the node's fields and
	// Properties and returns created=false. An ID stored under another label
	// returns a *NodeIDConflictError and changes nothing. A stored property
	// missing from node.Properties is removed, so the caller's record is
	// authoritative. The stored valid_from is always kept (node.ValidFrom
	// applies only on create), and the supersede state (valid_to,
	// superseded_by) is kept unless node.ValidTo is set. Use it for
	// nodes projected from an authoritative relational row that can change,
	// such as catalog Control nodes on re-import.
	UpsertNode(ctx context.Context, tenant string, node Node) (created bool, err error)
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
	// It is all-or-nothing: on error nothing is persisted and the returned
	// ID slice is nil. On success it returns the input edge IDs in order.
	// An error caused by one edge is a *BulkEdgeError: Index is the
	// zero-based index of the first failing edge, the message begins
	// "bulk create edges [Index]: ", and it wraps the error CreateEdge would
	// return for that edge (ErrNodeNotFound, ErrEdgeExists, a
	// *ReservedPropertyError, ...).
	BulkCreateEdges(ctx context.Context, tenant string, edges []BulkEdge) ([]string, error)

	// ExecuteQuery runs a read-only openCypher query against the tenant's graph.
	// Each whole $name token with a matching parameter is replaced by the
	// escaped value as a string literal; other tokens are left unchanged.
	// A driver that executes queries must reject writes. agedriver forces the
	// transaction read-only at the SQL level (writes return
	// ErrReadOnlyViolation) and rejects a query containing its dollar-quote
	// tag ($cypher$) with ErrInvalidCypher.
	//
	// Queries must RETURN a single expression; multi-column RETURN clauses
	// produce a database error.
	//
	// The tenant parameter on every GraphDB method is the multi-cluster routing key.
	// A TenantRouter implementing GraphDB can dispatch to per-tenant driver
	// instances connected to dedicated clusters without changing callers.
	ExecuteQuery(ctx context.Context, tenant, cypher string, params map[string]string) ([]QueryRow, error)

	// SupersedeFact sets valid_to on a node or edge, marking it as superseded.
	// Returns true if the entity was found and updated, false if not found.
	SupersedeFact(ctx context.Context, tenant string, req SupersedeRequest) (bool, error)
}
