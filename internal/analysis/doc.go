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
//     handles per-task retry via the Dispatcher, and fails the stage when
//     neither a result nor any worker progress arrives within the timeout.
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
//
// # Stall Detection
//
// CollectRequest.Timeout (analysis.engine.task_timeout) is an idle timeout
// over the whole worker pool, not over one stage. Concurrent stages, jobs
// and daemons share one queue group of workers, so a stage can receive no
// result of its own for a long time while the pool is busy with another's
// tasks. Every worker publishes a tenant-free tick on
// natsbus.WorkerProgressSubject after any task result it publishes, success
// or error (including an error that will be retried); it sends no tick when
// the result publish fails or the task is dropped before any result (a
// message without a valid tenant, say). The Collector resets the idle timer
// on any task result or worker progress, so a stage fails with
// ErrTaskTimeout only once the pool has been silent for Timeout. Ticks never
// count as results.
//
// Because any busy pool resets the idle timer, it cannot catch a lost task
// (work runs on core NATS: no redelivery after a worker crash or a dropped
// message). CollectRequest.StageTimeout (pipeline.stage_timeout, set with
// WithStageTimeout) does: it caps the whole collection, nothing resets it,
// and past it the stage fails with ErrStageTimeout, keeping the results
// received so far. A tick, forged or legitimate, can delay a stage's failure
// at most until that cap. A collection without a positive StageTimeout is
// refused with ErrNoStageTimeout.
//
// # Abandoned Collections
//
// A collection that ends without every expected result (ErrTaskTimeout,
// ErrStageTimeout, a cancelled context, which is how CancelJob reaches a
// running stage, or a failed Dispatch) leaves tasks queued on the worker
// pool whose results nobody will read, and each one is a billed LLM call.
// The collection then publishes exactly one tenant-scoped control message on
// natsbus.AbandonedSubject and stops its pending retries, and every worker
// drops the collection's tasks before calling the LLM.
//
// The abandon is keyed by CollectionHandle.CollectionID, a random UUID that
// PrepareCollect creates for each collection and the Dispatcher sends with
// every task (the X-Collection-Id header), not by the job: a resumed or
// retried job reuses its job ID, so a job-scoped abandon could drop the
// tasks of a legitimate later run. A failed abandon publish is logged and
// counted in analysis.collections.abandoned and never changes the stage's
// error.
package analysis
