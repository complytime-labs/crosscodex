// Package graph implements the Graph Service that provides gRPC access to the
// compliance relationship graph and materializes analysis results via NATS
// events.
//
// # Architecture
//
// CQRS split: event-driven writes via NATS subscriber consume pipeline stage
// completion events and materialize nodes/edges into per-tenant Apache AGE
// graphs via pkg/graphdb. Synchronous reads via gRPC RPCs serve queries
// directly. A ResourceResolver abstraction decouples the subscriber from
// storage backends.
//
// # Usage
//
//	svc := graph.New(graphDB, vectorDB, bus,
//	    graph.WithTelemetry(tp, mp),
//	    graph.WithLogger(logger),
//	    graph.WithResolver(pgResolver),
//	)
//	pb.RegisterGraphServiceServer(grpcServer, svc)
//	svc.Start(ctx) // begins NATS subscription
//	defer svc.Stop(ctx)
//
// # Thread Safety
//
// Service is safe for concurrent use. gRPC handlers are inherently concurrent.
// The NATS subscriber uses a queue group for single-delivery. The resolution
// pool bounds concurrent I/O.
//
// # Error Handling
//
// gRPC errors use canonical status codes. The NATS subscriber is a core-NATS
// queue subscription, so a handler error is logged and recorded on the
// natsbus.process span but the event is not redelivered. Because of that, an
// edge whose endpoint node is missing (graphdb.ErrNodeNotFound) is skipped
// rather than failing the event, so later edges in the event are still
// written; each skip increments graph.materialize.edges_skipped.total, adds a
// span event and logs a warning. Other driver errors still fail the event.
package graph

import (
	pbconnect "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/complytime-labs/crosscodex/internal/gateway"
)

// Compile-time interface assertions.
var _ pbconnect.GraphServiceHandler = (*Service)(nil)
var _ gateway.GraphBackend = (*Service)(nil)
