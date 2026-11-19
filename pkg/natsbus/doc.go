// Package natsbus provides a dual-mode NATS messaging client supporting
// both embedded (in-process) and external NATS servers.
//
// A single [Client] instance serves all tenants. Tenant ID is extracted
// from context via [pkg/tenant.FromContext] on each operation.
//
// Every published message carries mandatory provenance headers
// (X-Trace-Id, X-Span-Id, X-Tenant-Id, X-Timestamp, X-Content-SHA256)
// for downstream in-toto attestation by [pkg/attestation].
//
// Three JetStream audit streams (AUDIT_LLM, AUDIT_DECISIONS,
// AUDIT_EVENTS) are created on startup for compliance record retention.
// [WithExistingStreams] instead attaches to streams another process created,
// leaving them unchanged.
//
// Work distribution uses core NATS queue groups via [Client.QueueSubscribe].
//
// # Embedded Mode
//
// When NATSConfig.URL is empty, the client starts an in-process NATS
// server with JetStream enabled. JetStream data is stored in
// $XDG_STATE_HOME/crosscodex/nats/ (or the configured store_dir).
//
// # External Mode
//
// When NATSConfig.URL is set, the client connects to an external NATS
// server. TLS is configured via [WithTLSConfig].
//
// Example usage:
//
//	client, err := natsbus.New(cfg.NATS)
//	if err != nil {
//	    return err
//	}
//	defer client.Close()
//
//	subject, err := natsbus.WorkSubject(tenantID, natsbus.TaskClassify, jobID)
//	if err != nil {
//	    return err
//	}
//
//	err = client.Publish(ctx, subject, payload)
//
// Publish is core NATS: fire-and-forget, lost if no subscriber or the server
// is down. For messages that must not be lost, use PublishDurable, which waits
// for a JetStream stream to acknowledge storage and returns an error wrapping
// ErrNotStored when none does (including when no stream captures the subject).
//
// # Durable consumers
//
// [Client.ConsumeDurable] runs a durable JetStream pull consumer with explicit
// acknowledgements. The handler's result decides the outcome: nil acks, an
// error wrapping [ErrPermanent] terminates the message (removing it from a
// work-queue stream), and any other error redelivers it after
// [ConsumerConfig].RedeliverDelay, up to MaxDeliver (-1 is unlimited). Provenance
// and content-hash checks apply as for Subscribe, but a failing message is
// terminated rather than dropped. [Client.SubjectMessageCount] reports how many
// messages a stream holds on a subject, which in a work queue means work not yet
// acknowledged. natsbustest.Fake mirrors both.
package natsbus
