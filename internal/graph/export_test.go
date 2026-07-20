package graph

import (
	"context"

	"github.com/complytime-labs/crosscodex/pkg/authn"
)

// Export unexported functions for testing.

var ExportNodeToProto = nodeToProto
var ExportEdgeToProto = edgeToProto
var ExportProtoToNode = protoToNode
var ExportProtoToEdge = protoToEdge
var ExportPathToTraverseResponse = pathToTraverseResponse
var ExportQueryRowsToProto = queryRowsToProto
var ExportSimilarityResultToProto = similarityResultToProto
var ExportExtractTenant = (*Service).extractTenant
var ExportHandleEvent = (*Service).handleEvent

// ExportContextWithIdentity injects an authn.Identity into a context
// using the shared authn.WithIdentity helper, allowing external _test packages
// to set up authenticated contexts without the gRPC interceptor.
func ExportContextWithIdentity(ctx context.Context, id *authn.Identity) context.Context {
	return authn.WithIdentity(ctx, id)
}

// Re-export types for test convenience.
type (
	ExportResourceRef      = ResourceRef
	ExportResolverRegistry = ResolverRegistry
)
