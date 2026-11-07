// Package worker implements the LLM Worker Service that executes LLM tasks
// received via NATS.
//
// # Architecture
//
// Workers are adapters with no durable state. They subscribe to NATS work
// subjects as a queue group; NATS distributes tasks round-robin across
// workers in the same group. Each task is self-contained: the worker
// deserializes the payload, builds an LLM request, calls the gateway, and
// publishes the result. No prompt registry, database, or inter-worker
// coordination is needed.
//
// # Tasks in Flight
//
// A worker runs up to worker.max_in_flight tasks at once (default 1). Its one
// work subscription hands each task to its own goroutine once a slot is free;
// while every slot is taken the subscription's delivery goroutine waits, so
// later tasks wait in the NATS client's pending queue. Retry and abandon
// handling stay per task. Stop drains the subscriptions, keeps tasks still
// waiting for a slot from starting (counting them in worker.tasks.dropped
// with reason "worker_stopping"), and waits for running tasks until its
// context ends.
//
// # Abandoned Collections
//
// Every worker also subscribes, outside the queue group, to
// natsbus.AbandonedSubjectFilter. An analysis collection that gives up
// (timeout, cancelled job, failed dispatch) publishes one abandon naming its
// collection ID, which it also sent with each task in the X-Collection-Id
// header. The worker remembers abandons in a bounded in-memory set (at most
// worker.max_abandoned_collections, each for the WithAbandonTTL duration, fed by
// pipeline.stage_timeout) and drops a matching task before any LLM call,
// with no result and no progress tick, counting it in worker.tasks.dropped.
// The key is the collection, not the job, so a resumed job, which reuses its
// job ID with new collections, is never affected. Malformed control messages
// are dropped, logged and counted; see handleAbandoned.
//
// # Usage
//
// Obtain a WorkerConfig from the service configuration:
//
//	daemonCfg := cfg.ServiceConfig()     // populates daemonCfg.Worker.LLM = cfg.LLM
//	workerCfg := daemonCfg.Worker
//	workerCfg.QueueGroup = "llm-workers" // optional override
//
//	w := worker.New(natsClient, llmClient, workerCfg,
//	    worker.WithTelemetry(tp, mp), worker.WithLogger(logger),
//	    worker.WithAbandonTTL(cfg.Pipeline.StageTimeout))
//
//	if err := w.Start(ctx); err != nil { ... }
//	defer w.Stop(ctx)
//
// # Thread Safety
//
// Worker is safe for concurrent use. Tasks run concurrently, up to
// worker.max_in_flight; the only state they share is the mutex-guarded set of
// abandoned collections and the in-flight slots of the current Start.
//
// # Error Handling
//
// Errors are published as results with an X-Error header, matching the
// collector protocol. The worker never crashes on a bad task; it reports the
// error and moves on. See ErrInvalidMessage, ErrUnsupportedTaskType,
// ErrInvalidPayload, ErrLLMCall, ErrTenantConfig.
//
// # Audit Emission
//
// LLM audit events are published via NATSAuditEmitter, which implements
// llmclient.AuditEmitter. Service binaries must construct a NATSAuditEmitter
// and pass it to llmclient.New to enable compliance audit trails:
//
//	emitter := worker.NewNATSAuditEmitter(bus, logger)
//	llm, err := llmclient.New(cfg.LLM, llmclient.WithAuditEmitter(emitter))
//	w := worker.New(bus, llm, cfg.Worker, worker.WithAbandonTTL(cfg.Pipeline.StageTimeout))
//
// Audit events are published best-effort: publish failures are logged and
// counted via the crosscodex.worker.audit.failures.total metric, but never
// propagated to the caller.
//
// # NATS Handler Contract
//
// handleMessage always returns nil to NATS. Returning a non-nil error would
// trigger NATS redelivery, creating infinite retry loops for permanently
// invalid messages (malformed payloads, unsupported task types). Instead,
// errors are published as results with X-Error headers so the upstream
// collector can handle them. This is a deliberate design choice, not a bug.
package worker
