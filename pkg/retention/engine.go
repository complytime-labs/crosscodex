package retention

import (
	"context"
	"fmt"
	"time"
)

// ClassCount aggregates retention action tallies for a single DataClass.
type ClassCount struct {
	Scanned, Held, Archived, Purged int
}

// Report summarises the outcome of a single Engine.Scan invocation.
type Report struct {
	Scanned  int
	Held     int
	Archived int
	Purged   int
	PerClass map[DataClass]ClassCount
	Errors   []string
}

// ScanOptions controls Engine.Scan behaviour.
type ScanOptions struct {
	DryRun bool
	Now    time.Time
}

// Engine orchestrates the retention lifecycle: collect → hold-filter →
// archive → purge → audit.
type Engine struct {
	collectors []Collector
	holds      HoldStore
	arch       Archiver
	purger     Purger
	audit      AuditPublisher
	policy     Policy
}

// NewEngine wires together the retention engine components.
func NewEngine(collectors []Collector, holds HoldStore, arch Archiver, purger Purger, audit AuditPublisher, policy Policy) *Engine {
	return &Engine{
		collectors: collectors,
		holds:      holds,
		arch:       arch,
		purger:     purger,
		audit:      audit,
		policy:     policy,
	}
}

// auditJobID returns a delimiter-free routing token for the NATS subject.
// ClassJobResults candidates use the job ID directly (it is already
// delimiter-free); all other candidates route via the literal "retention"
// token. The candidate's true identity is carried in AuditRecord.Detail.
func auditJobID(c Candidate) string {
	if c.Class == ClassJobResults {
		return c.ID
	}
	return "retention"
}

// auditDetail returns the best available candidate identity string for the
// Detail field of an AuditRecord.
func auditDetail(c Candidate) string {
	if c.ObjectKey != "" {
		return c.ObjectKey
	}
	return c.ID
}

// publishAudit emits an audit record. A publish failure is non-fatal: the
// error message is appended to rep.Errors and the caller continues normally.
func (e *Engine) publishAudit(ctx context.Context, rep *Report, c Candidate, action, outcome string, now time.Time) {
	r := AuditRecord{
		Action:    action,
		Actor:     "retention-engine",
		TenantID:  c.TenantID,
		JobID:     auditJobID(c),
		DataClass: string(c.Class),
		Outcome:   outcome,
		Detail:    auditDetail(c),
		At:        now,
	}
	if err := e.audit.Publish(ctx, r); err != nil {
		rep.Errors = append(rep.Errors, fmt.Errorf("retention engine: audit publish: %w", err).Error())
	}
}

// Scan runs one retention sweep. It loads active holds, collects expired
// candidates from all collectors, filters held candidates, and (unless
// DryRun) archives then purges each remaining candidate, emitting an audit
// record for each completed action.
//
// Archive-before-purge: a candidate whose archive fails is NOT purged; the
// error is appended to Report.Errors and processing continues with the next
// candidate. Audit-publish failures are similarly non-fatal.
//
// Report.Scanned is the total count returned by collectors (collectors
// already pre-filter by expiry — the engine does not re-check Policy.Expired).
func (e *Engine) Scan(ctx context.Context, opts ScanOptions) (Report, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	rep := Report{PerClass: make(map[DataClass]ClassCount)}

	// Load active legal/compliance holds and build a fast checker.
	active, err := e.holds.Active(ctx, now)
	if err != nil {
		return rep, fmt.Errorf("retention engine: load active holds: %w", err)
	}
	hc := NewHoldChecker(active)

	// Collect expired candidates from each injected collector.
	var candidates []Candidate
	for _, col := range e.collectors {
		cs, err := col.Collect(ctx, e.policy, now)
		if err != nil {
			return rep, fmt.Errorf("retention engine: collect: %w", err)
		}
		candidates = append(candidates, cs...)
	}

	for _, c := range candidates {
		cc := rep.PerClass[c.Class]
		cc.Scanned++
		rep.Scanned++

		if hc.Held(c) {
			cc.Held++
			rep.Held++
			rep.PerClass[c.Class] = cc
			continue
		}

		if opts.DryRun {
			rep.PerClass[c.Class] = cc
			continue
		}

		// Archive first; skip purge if archive fails.
		if err := e.arch.Archive(ctx, c); err != nil {
			rep.Errors = append(rep.Errors, fmt.Errorf("retention engine: archive %s: %w", auditDetail(c), err).Error())
			rep.PerClass[c.Class] = cc
			continue
		}
		cc.Archived++
		rep.Archived++
		e.publishAudit(ctx, &rep, c, ActionArchive, "ok", now)

		// Purge only after a verified archive.
		if err := e.purger.Purge(ctx, c); err != nil {
			rep.Errors = append(rep.Errors, fmt.Errorf("retention engine: purge %s: %w", auditDetail(c), err).Error())
			rep.PerClass[c.Class] = cc
			continue
		}
		cc.Purged++
		rep.Purged++
		e.publishAudit(ctx, &rep, c, ActionPurge, "ok", now)

		rep.PerClass[c.Class] = cc
	}

	return rep, nil
}
