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
// The NATS subscriber is two durable consumers of the MATERIALIZE work-queue
// stream (graph-materializer and graph-barrier) that every graph role process
// shares, so each message is handled by one process. The resolution pool
// bounds concurrent I/O.
//
// # Error Handling
//
// gRPC errors use canonical status codes. A materialization event whose
// handler fails is redelivered after nats.materialize.redeliver_delay, up to
// nats.materialize.max_deliver deliveries; every graph write is idempotent,
// so a redelivery writes nothing twice. On the last delivery a failure stores
// a "failed" graph outcome for the job (natsbus.GraphOutcomeSubject) and
// terminates the event. An edge whose endpoint node is missing
// (graphdb.ErrNodeNotFound) does not stop the event's other edges: before the
// last delivery the event is retried once they are written, so a Control node
// the catalog projection has not written yet heals; on the last delivery the
// edge is skipped, which increments graph.materialize.edges_skipped.total,
// adds a span event and logs a warning. A job's barrier is answered with a
// "materialized" or "failed" outcome once none of the job's events is left in
// the stream.
package graph

import (
	pbconnect "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/complytime-labs/crosscodex/internal/gateway"
)

// Compile-time interface assertions.
var _ pbconnect.GraphServiceHandler = (*Service)(nil)
var _ gateway.GraphBackend = (*Service)(nil)
var _ gateway.CrosswalkBackend = (*Service)(nil)
