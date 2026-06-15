// Package analyzer defines the plugin interface and DAG builder for
// compliance analysis pipelines.
//
// Analyzers implement [Analyzer] with a concrete proto.Message type
// parameter. The [Registry] stores type-erased [RegisteredAnalyzer]
// wrappers and builds an execution [DAG] from dependency declarations
// using Kahn's algorithm.
//
// # Tenant Context
//
// This package is a pure in-memory data structure with no I/O. It does
// not enforce tenant isolation directly. Callers are responsible for
// propagating tenant context via [context.Context] when invoking
// [RegisteredAnalyzer.GenerateWorkFromProto] and
// [RegisteredAnalyzer.Aggregate]. When dispatching [Task] objects
// across process boundaries (e.g., via NATS), callers must embed the
// tenant ID in the task payload or message headers before publishing.
//
// # Example
//
//	registry := analyzer.NewRegistry()
//
//	// Register analyzers — each declares its own dependencies.
//	analyzer.Register[*catalogpb.Control](registry, classifyAnalyzer)
//	analyzer.Register[*catalogpb.Control](registry, embeddingAnalyzer)
//
//	// Build the execution DAG.
//	dag, err := registry.BuildDAG(ctx)
//	if err != nil {
//	    return err // ErrCycleDetected or ErrMissingDependency
//	}
//
//	// Inspect execution levels for parallel dispatch.
//	for level, names := range dag.Levels() {
//	    fmt.Printf("Level %d: %v\n", level, names)
//	}
//
//	// Build a partial DAG for a subset of analyzers.
//	partial, err := dag.Subset("relationship")
//	// partial includes: classify, embedding, relationship
package analyzer
