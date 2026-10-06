// Package graphdb defines the vendor-neutral contract for graph database access.
//
// The GraphDB interface, domain types (Node, Edge, Path, RequiresEdge, etc.),
// and sentinel errors are declared here. Application code should depend only
// on this package. The behavioral contract every driver honors is documented
// on GraphDB and enforced by internal/testspecs.GraphDBContractBehavior.
//
// Two drivers implement it:
//
//   - agedriver: Apache AGE on PostgreSQL. Wired only in
//     cmd/crosscodexd/resources.go.
//   - memdriver: in memory, for unit tests and examples. ExecuteQuery returns
//     ErrNotSupported.
//
// Construct them as:
//
//	client, err := agedriver.New(db)
//
//	client := memdriver.New()
//	err := client.CreateGraph(ctx, tenantID)
package graphdb
