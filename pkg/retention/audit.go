package retention

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/complytime-labs/crosscodex/pkg/natsbus"
)

// Action constants for retention lifecycle events published to the decisions
// audit stream. Scan-level records (ActionScanStart, ActionScanFinish) have
// no associated job; the caller MUST supply a non-empty, delimiter-free
// JobID (e.g. "scan") because AuditSubject rejects an empty or
// delimiter-bearing token — the publisher does not special-case it.
const (
	ActionScanStart   = "scan_start"
	ActionScanFinish  = "scan_finish"
	ActionArchive     = "archive"
	ActionPurge       = "purge"
	ActionHoldCreate  = "hold_create"
	ActionHoldRelease = "hold_release"
)

// AuditRecord is a single retention lifecycle event. It is serialized to JSON
// and published to crosscodex.audit.{tenant_id}.decisions.{job_id}.
type AuditRecord struct {
	Action    string    `json:"action"`
	Actor     string    `json:"actor"`
	TenantID  string    `json:"tenant_id"`
	JobID     string    `json:"job_id"`
	DataClass string    `json:"data_class"`
	Outcome   string    `json:"outcome"`
	Detail    string    `json:"detail"`
	At        time.Time `json:"at"`
}

// AuditPublisher publishes retention lifecycle events to the NATS decisions
// audit stream.
type AuditPublisher interface {
	Publish(ctx context.Context, r AuditRecord) error
}

type auditPublisher struct {
	bus natsbus.Client
}

// NewAuditPublisher returns an AuditPublisher that serializes records to JSON
// and publishes them to the decisions subject for the record's tenant and job.
func NewAuditPublisher(bus natsbus.Client) AuditPublisher {
	return &auditPublisher{bus: bus}
}

// Publish builds the decisions subject, marshals r to JSON, and publishes to
// the NATS bus. An empty or delimiter-bearing r.JobID causes AuditSubject to
// return an error; the publisher wraps and returns it without calling the bus.
func (p *auditPublisher) Publish(ctx context.Context, r AuditRecord) error {
	subject, err := natsbus.AuditSubject(r.TenantID, natsbus.AuditDecisions, r.JobID)
	if err != nil {
		return fmt.Errorf("retention audit publisher: build subject: %w", err)
	}

	data, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("retention audit publisher: marshal record: %w", err)
	}

	if err := p.bus.Publish(ctx, subject, data); err != nil {
		return fmt.Errorf("retention audit publisher: publish: %w", err)
	}
	return nil
}
