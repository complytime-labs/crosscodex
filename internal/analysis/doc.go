// Package analysis implements the Analysis Engine service that orchestrates
// analyzer execution via NATS.
//
// # Architecture
//
// The Engine composes two focused components behind interfaces:
//
//   - Dispatcher — publishes analyzer tasks to NATS work subjects. Owns
//     serialization and header injection. Knows nothing about results.
//   - Collector — subscribes to NATS result subjects, collects results,
//     handles per-task retry via the Dispatcher, and enforces timeout.
//     Knows nothing about DAGs or analyzers.
//
// This separation provides independent testability (mock each component's
// direct dependencies only) and a future distribution seam (swap NATS
// implementations without touching DAG logic).
//
// # Usage
//
//	engine := analysis.NewWithNATS(registry, natsClient, cfg, taskTypes,
//	    analysis.WithTelemetry(tp, mp),
//	    analysis.WithStageReporter(analysis.NewNATSStageReporter(natsClient)),
//	)
//
//	result, err := engine.Execute(ctx, analysis.ExecutionRequest{
//	    JobID:         jobID,
//	    AnalyzerNames: []string{"classify", "embedding"},
//	    Input:         control,
//	})
//
// # Thread Safety
//
// Engine is safe for sequential use. Concurrent Execute calls require
// external synchronization — the Engine itself holds no mutable shared
// state, but the underlying DAG construction is not goroutine-safe.
//
// # Error Handling
//
// Three layers: per-task retry (exponential backoff with jitter), per-analyzer
// failure (marks stage failed, skips dependents), and partial results
// (aggregates successes, flags incomplete). See ErrTaskTimeout,
// ErrRetryExhausted, ErrAnalyzerFailed, ErrDependencyFailed.
package analysis
