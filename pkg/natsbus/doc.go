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
package natsbus
