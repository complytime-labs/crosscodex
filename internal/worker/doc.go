// Package worker implements the LLM Worker Service that executes LLM tasks
// received via NATS.
//
// # Architecture
//
// Workers are stateless adapters. They subscribe to NATS work subjects as a
// queue group; NATS distributes tasks round-robin across workers in the same
// group. Each task is self-contained: the worker deserializes the payload,
// builds an LLM request, calls the gateway, and publishes the result. No
// prompt registry, database, or inter-worker coordination is needed.
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
//	    worker.WithTelemetry(tp, mp), worker.WithLogger(logger))
//
//	if err := w.Start(ctx); err != nil { ... }
//	defer w.Stop(ctx)
//
// # Thread Safety
//
// Worker is safe for concurrent use. Each NATS message handler executes
// independently with no shared mutable state between tasks.
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
//	w := worker.New(bus, llm, cfg.Worker)
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
