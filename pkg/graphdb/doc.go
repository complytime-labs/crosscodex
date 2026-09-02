// Package graphdb defines the vendor-neutral contract for graph database access.
//
// The GraphDB interface, domain types (Node, Edge, Path, RequiresEdge, etc.),
// and sentinel errors are declared here. Application code should depend only
// on this package.
//
// To construct a live client backed by Apache AGE, import the agedriver sub-package:
//
//	import "github.com/complytime-labs/crosscodex/pkg/graphdb/agedriver"
//
//	client, err := agedriver.New(db)
package graphdb
