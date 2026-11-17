package cleaning

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/jobconfig"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// PGFindingStore is the FindingStore over the edge_findings and
// edge_finding_decisions tables. Each method runs in one transaction on a
// db.TenantConnection, so row-level security confines it to the tenant in
// ctx. That tenant must equal the tenantID argument.
type PGFindingStore struct {
	conn     db.TenantConnection
	tracer   trace.Tracer          // optional, nil-safe
	ops      metric.Int64Counter   // optional, nil-safe
	duration metric.Int64Histogram // optional, nil-safe
}

var _ FindingStore = (*PGFindingStore)(nil)

// PGFindingStoreOption configures a PGFindingStore.
type PGFindingStoreOption func(*PGFindingStore)

// WithFindingStoreTelemetry enables tracing and metrics for the store.
// Either provider may be nil; nil providers are ignored. It has no
// pkg/config field: whoever builds the store passes the daemon's providers.
func WithFindingStoreTelemetry(tp trace.TracerProvider, mp metric.MeterProvider) PGFindingStoreOption {
	return func(s *PGFindingStore) {
		if tp != nil {
			s.tracer = tp.Tracer("crosscodex/pkg/cleaning")
		}
		if mp != nil {
			meter := mp.Meter("crosscodex")
			s.ops, _ = meter.Int64Counter("cleaning.findingstore.operations.total",
				metric.WithDescription("Finding store calls by operation and status (ok or error)"))
			s.duration, _ = meter.Int64Histogram("cleaning.findingstore.operation.duration_ms",
				metric.WithDescription("Finding store call duration in milliseconds by operation and status"), metric.WithUnit("ms"))
		}
	}
}

// NewPGFindingStore returns a store over conn.
func NewPGFindingStore(conn db.TenantConnection, opts ...PGFindingStoreOption) *PGFindingStore {
	s := &PGFindingStore{conn: conn}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// observe runs one public method's body in a span named after op, then
// counts and times the call by operation and status.
func (s *PGFindingStore) observe(ctx context.Context, op, tenantID string, fn func(ctx context.Context) error) error {
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "cleaning.findingstore."+op)
	defer span.End()
	span.SetAttributes(attribute.String("tenant.id", tenantID), attribute.String("operation", op))
	start := time.Now()
	err := fn(ctx)
	status := "ok"
	if err != nil {
		status = "error"
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	} else {
		span.SetStatus(codes.Ok, "")
	}
	attrs := metric.WithAttributes(attribute.String("operation", op), attribute.String("status", status))
	if s.ops != nil {
		s.ops.Add(ctx, 1, attrs)
	}
	if s.duration != nil {
		s.duration.Record(ctx, time.Since(start).Milliseconds(), attrs)
	}
	return err
}

// inTx runs fn in one tenant-scoped transaction and commits it when fn
// succeeds. RLS keys off ctx's tenant, so a tenantID argument that differs
// from it is refused rather than silently reading another tenant's rows.
func (s *PGFindingStore) inTx(ctx context.Context, tenantID string, fn func(tx db.Transaction) error) error {
	ctxTenant, err := tenant.FromContext(ctx)
	if err != nil {
		return fmt.Errorf("finding store: %w", err)
	}
	if ctxTenant != tenantID {
		return fmt.Errorf("finding store: context tenant %q does not match tenant %q", ctxTenant, tenantID)
	}
	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("finding store: begin: %w", err)
	}
	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			return errors.Join(err, fmt.Errorf("finding store: rollback: %w", rbErr))
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("finding store: commit: %w", err)
	}
	return nil
}

// scanRows calls scan for each row, then closes rows; it returns the first
// error from scan, iteration or Close.
func scanRows(rows db.Rows, scan func() error) error {
	for rows.Next() {
		if err := scan(); err != nil {
			return errors.Join(err, rows.Close())
		}
	}
	return errors.Join(rows.Err(), rows.Close())
}

// decodeKeys strictly decodes a stored concerns or targets column of at most
// limit bytes: a JSON array (or null) of verdict keys with no unknown fields,
// each one ParseVerdictKey would accept. Anything else wraps
// ErrInvalidVerdictKey.
func decodeKeys(b []byte, limit int) ([]VerdictKey, error) {
	if len(b) > limit {
		return nil, fmt.Errorf("%w: input exceeds %d bytes", ErrInvalidVerdictKey, limit)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var keys []VerdictKey
	if err := dec.Decode(&keys); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidVerdictKey, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: unexpected data after the keys", ErrInvalidVerdictKey)
	}
	for i, k := range keys {
		if err := k.validate(); err != nil {
			return nil, fmt.Errorf("key %d: %w", i, err)
		}
	}
	return keys, nil
}

// findingColumnsSQL is what scanFinding reads, in order.
const findingColumnsSQL = `finding_id, signal, source_catalog, target_catalog, evidence, evidence_hash,
       concerns, targets, edge_ids, status, graph_applied, updated_at`

// scanFinding reads one findingColumnsSQL row through scan. Stored JSON is
// decoded strictly: a row that does not decode is an error naming it, never
// a finding with missing parts.
func scanFinding(scan func(dest ...any) error) (StoredFinding, error) {
	var (
		f                                  StoredFinding
		signal, status                     string
		evidence, concerns, targets, edges []byte
	)
	if err := scan(&f.ID, &signal, &f.SourceCatalog, &f.TargetCatalog, &evidence, &f.EvidenceHash,
		&concerns, &targets, &edges, &status, &f.GraphApplied, &f.UpdatedAt); err != nil {
		return StoredFinding{}, err
	}
	f.Signal, f.Status, f.UpdatedAt = Signal(signal), Status(status), f.UpdatedAt.UTC()
	var err error
	if f.Evidence, err = ParseEvidence(evidence); err != nil {
		return StoredFinding{}, fmt.Errorf("finding %s: evidence: %w", f.ID, err)
	}
	if f.Concerns, err = decodeKeys(concerns, maxEvidenceBytes); err != nil {
		return StoredFinding{}, fmt.Errorf("finding %s: concerns: %w", f.ID, err)
	}
	if f.Targets, err = decodeKeys(targets, maxListBytes); err != nil {
		return StoredFinding{}, fmt.Errorf("finding %s: targets: %w", f.ID, err)
	}
	if len(edges) > maxListBytes {
		return StoredFinding{}, fmt.Errorf("finding %s: edge IDs exceed %d bytes", f.ID, maxListBytes)
	}
	if err := json.Unmarshal(edges, &f.EdgeIDs); err != nil {
		return StoredFinding{}, fmt.Errorf("finding %s: edge IDs: %w", f.ID, err)
	}
	return f, nil
}

// queryFindings runs a query selecting findingColumnsSQL and decodes every row.
func queryFindings(ctx context.Context, tx db.Transaction, op, query string, args ...any) ([]StoredFinding, error) {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	var out []StoredFinding
	err = scanRows(rows, func() error {
		f, err := scanFinding(rows.Scan)
		if err != nil {
			return fmt.Errorf("%s: %w", op, err)
		}
		out = append(out, f)
		return nil
	})
	return out, err
}

const completedJobsSQL = `
SELECT j.job_id, j.config, s.completed_at IS NOT NULL, COALESCE(s.completed_at, 'epoch'::timestamptz)
FROM jobs j
CROSS JOIN LATERAL (
    SELECT max(js.completed_at) AS completed_at FROM job_stages js
    WHERE js.tenant_id = j.tenant_id AND js.job_id = j.job_id
) s
WHERE j.tenant_id = $1 AND j.status = 'completed'
ORDER BY j.job_id`

// completedStagesSQL reads every stage of the tenant's completed jobs, with
// whether stage_record is NULL (unrecorded) as its own column: an empty but
// non-NULL record is a value that must parse, not a NULL.
const completedStagesSQL = `
SELECT js.job_id, js.stage_name, js.stage_record IS NOT NULL, COALESCE(js.stage_record, '')
FROM job_stages js JOIN jobs j ON j.job_id = js.job_id AND j.tenant_id = js.tenant_id
WHERE js.tenant_id = $1 AND j.status = 'completed'
ORDER BY js.job_id, js.stage_name`

func (s *PGFindingStore) CompletedJobs(ctx context.Context, tenantID string) ([]Job, error) {
	var out []Job
	// The jobs policy also shows only the ctx user's own jobs. A cleaning
	// pass judges every verdict of the tenant, and both queries below read
	// jobs, so the user is dropped: the result is tenant-wide by contract
	// (see FindingStore.CompletedJobs), and RLS still confines it to the
	// tenant.
	ctx = tenant.WithoutUser(ctx)
	err := s.observe(ctx, "CompletedJobs", tenantID, func(ctx context.Context) error {
		return s.inTx(ctx, tenantID, func(tx db.Transaction) error {
			rows, err := tx.Query(ctx, completedJobsSQL, tenantID)
			if err != nil {
				return fmt.Errorf("completed jobs: %w", err)
			}
			byID := map[string]int{}
			if err := scanRows(rows, func() error {
				var (
					j         Job
					config    []byte
					completed bool
				)
				if err := rows.Scan(&j.ID, &config, &completed, &j.CompletedAt); err != nil {
					return fmt.Errorf("completed jobs: %w", err)
				}
				if !completed {
					return fmt.Errorf("completed jobs: job %s is completed but none of its stages has a completion time, "+
						"so its verdicts cannot be ordered; the run is refused rather than guess. Check the job's job_stages rows", j.ID)
				}
				var ok bool
				if j.Catalogs, ok = jobconfig.ExtractCatalogs(config); !ok {
					return fmt.Errorf("completed jobs: job %s has a stored config that names no catalog, "+
						"so its verdicts cannot be placed on a catalog pair; the run is refused rather than guess. Check the job's config", j.ID)
				}
				j.CompletedAt = j.CompletedAt.UTC()
				j.Stages = map[string]*StageRecord{}
				byID[j.ID] = len(out)
				out = append(out, j)
				return nil
			}); err != nil {
				return err
			}
			rows, err = tx.Query(ctx, completedStagesSQL, tenantID)
			if err != nil {
				return fmt.Errorf("completed jobs: stages: %w", err)
			}
			return scanRows(rows, func() error {
				var (
					jobID, stage, record string
					recorded             bool
				)
				if err := rows.Scan(&jobID, &stage, &recorded, &record); err != nil {
					return fmt.Errorf("completed jobs: stages: %w", err)
				}
				i, ok := byID[jobID]
				if !ok {
					// The job completed between the two queries; it is not in this snapshot.
					return nil
				}
				if !recorded {
					out[i].Stages[stage] = nil
					return nil
				}
				if record == "" {
					return fmt.Errorf("completed jobs: job %s, stage %s: the stored stage_record is empty rather than NULL, "+
						"so which prompt and models produced it is unknown; the run is refused rather than guess: %w", jobID, stage, ErrInvalidStageRecord)
				}
				r, err := ParseStageRecord(record)
				if err != nil {
					return fmt.Errorf("completed jobs: job %s, stage %s: the stored stage_record does not parse, "+
						"so which prompt and models produced it is unknown; the run is refused rather than guess: %w", jobID, stage, err)
				}
				out[i].Stages[stage] = r
				return nil
			})
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// pairSQL is Scope's unordered catalog-pair rule over the parameters $a and
// $b; checkPair refuses a one-sided pair before it runs.
func pairSQL(a, b string) string {
	return `((` + a + `::text = '' AND ` + b + `::text = '') OR (source_catalog = ` + a + ` AND target_catalog = ` + b +
		`) OR (source_catalog = ` + b + ` AND target_catalog = ` + a + `))`
}

var findingsSQL = `SELECT ` + findingColumnsSQL + ` FROM edge_findings
WHERE tenant_id = $1 AND ` + pairSQL("$2", "$3") + `
ORDER BY finding_id`

func (s *PGFindingStore) Findings(ctx context.Context, tenantID string, scope Scope) ([]StoredFinding, error) {
	var out []StoredFinding
	err := s.observe(ctx, "Findings", tenantID, func(ctx context.Context) error {
		if err := checkPair(scope.SourceCatalog, scope.TargetCatalog); err != nil {
			return fmt.Errorf("findings: %w", err)
		}
		return s.inTx(ctx, tenantID, func(tx db.Transaction) error {
			var err error
			out, err = queryFindings(ctx, tx, "findings", findingsSQL, tenantID, scope.SourceCatalog, scope.TargetCatalog)
			return err
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// listFindingsSQL is keyset paging on finding_id. Finding IDs are lowercase
// hex (ApplyRun refuses any other), so every collation orders them as Go does.
var listFindingsSQL = `SELECT ` + findingColumnsSQL + ` FROM edge_findings
WHERE tenant_id = $1 AND ($2::text = '' OR signal = $2) AND ($3::text = '' OR status = $3)
  AND ` + pairSQL("$4", "$5") + ` AND finding_id > $6
ORDER BY finding_id
LIMIT $7`

func (s *PGFindingStore) ListFindings(ctx context.Context, tenantID string, f FindingFilter) ([]StoredFinding, string, error) {
	var (
		page []StoredFinding
		next string
	)
	err := s.observe(ctx, "ListFindings", tenantID, func(ctx context.Context) error {
		if f.PageSize < 1 {
			return fmt.Errorf("list findings: page size %d is not positive; pass at least 1", f.PageSize)
		}
		if err := checkPair(f.SourceCatalog, f.TargetCatalog); err != nil {
			return fmt.Errorf("list findings: %w", err)
		}
		return s.inTx(ctx, tenantID, func(tx db.Transaction) error {
			// One row beyond the page tells whether another page follows.
			got, err := queryFindings(ctx, tx, "list findings", listFindingsSQL, tenantID, string(f.Signal), string(f.Status),
				f.SourceCatalog, f.TargetCatalog, f.PageToken, int64(f.PageSize)+1)
			if err != nil {
				return err
			}
			page = got
			if len(got) > f.PageSize {
				page = got[:f.PageSize]
				next = page[len(page)-1].ID
			}
			return nil
		})
	})
	if err != nil {
		return nil, "", err
	}
	return page, next, nil
}

const getFindingSQL = `SELECT ` + findingColumnsSQL + ` FROM edge_findings WHERE tenant_id = $1 AND finding_id = $2`

// decisionsSQL orders a finding's decisions oldest first; decision_id breaks
// ties byte-wise.
const decisionsSQL = `
SELECT decision_id, finding_id, verdict, decided_by, COALESCE(run_id, ''), rationale, request, attestation, decided_at
FROM edge_finding_decisions
WHERE tenant_id = $1 AND finding_id = $2
ORDER BY decided_at, decision_id COLLATE "C"`

// readFinding returns the finding id, locking it when query does; a missing
// row wraps ErrFindingNotFound.
func readFinding(ctx context.Context, tx db.Transaction, op, query, tenantID, id string) (StoredFinding, error) {
	f, err := scanFinding(tx.QueryRow(ctx, query, tenantID, id).Scan)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return StoredFinding{}, fmt.Errorf("%s %s: %w", op, id, ErrFindingNotFound)
	case err != nil:
		return StoredFinding{}, fmt.Errorf("%s %s: %w", op, id, err)
	}
	return f, nil
}

func (s *PGFindingStore) GetFinding(ctx context.Context, tenantID, findingID string) (StoredFinding, []Decision, error) {
	var (
		f  StoredFinding
		ds []Decision
	)
	err := s.observe(ctx, "GetFinding", tenantID, func(ctx context.Context) error {
		return s.inTx(ctx, tenantID, func(tx db.Transaction) error {
			var err error
			if f, err = readFinding(ctx, tx, "get finding", getFindingSQL, tenantID, findingID); err != nil {
				return err
			}
			rows, err := tx.Query(ctx, decisionsSQL, tenantID, findingID)
			if err != nil {
				return fmt.Errorf("get finding %s: decisions: %w", findingID, err)
			}
			return scanRows(rows, func() error {
				var d Decision
				if err := rows.Scan(&d.ID, &d.FindingID, &d.Verdict, &d.DecidedBy, &d.RunID, &d.Rationale,
					&d.Request, &d.Attestation, &d.DecidedAt); err != nil {
					return fmt.Errorf("get finding %s: decisions: %w", findingID, err)
				}
				d.DecidedAt = d.DecidedAt.UTC()
				ds = append(ds, d)
				return nil
			})
		})
	})
	if err != nil {
		return StoredFinding{}, nil, err
	}
	return f, ds, nil
}

// The finding columns a run writes, in the order upsertArgs returns them
// after tenant_id and finding_id.
const insertFindingSQL = `
INSERT INTO edge_findings
    (tenant_id, finding_id, signal, source_catalog, target_catalog, evidence, evidence_hash,
     concerns, targets, edge_ids, status, graph_applied)
VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8::jsonb, $9::jsonb, $10::jsonb, $11, $12)`

const updateFindingSQL = `
UPDATE edge_findings
SET signal = $3, source_catalog = $4, target_catalog = $5, evidence = $6::jsonb, evidence_hash = $7,
    concerns = $8::jsonb, targets = $9::jsonb, edge_ids = $10::jsonb, status = $11, graph_applied = $12,
    updated_at = now()
WHERE tenant_id = $1 AND finding_id = $2`

// insertDecisionSQL leaves run_id NULL for a reviewer's decision and stamps
// decided_at with the transaction time when $10 is NULL.
const insertDecisionSQL = `
INSERT INTO edge_finding_decisions
    (tenant_id, decision_id, finding_id, verdict, decided_by, run_id, rationale, request, attestation, decided_at)
VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $7, $8, $9, COALESCE($10::timestamptz, now()))`

const lockFindingSQL = getFindingSQL + ` FOR UPDATE`

// encodeJSON returns v's JSON as a string for a ::jsonb parameter. Callers
// pass values StoredFinding.Validate has already encoded, so it cannot fail.
func encodeJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("cleaning: encode a validated value: %v", err))
	}
	return string(b)
}

// staleRun is the refusal of an upsert whose expected state no longer holds.
func staleRun(id string) error {
	return fmt.Errorf("finding %s changed since the preview; nothing was written. Preview the run again and apply its new plan: %w",
		id, ErrStateChanged)
}

// writeUpsert writes one validated upsert if its expected state holds: a
// new finding is inserted, and another run that inserted it first makes the
// INSERT fail with a unique violation; a stored one is locked, compared and
// updated.
func writeUpsert(ctx context.Context, tx db.Transaction, tenantID string, u RunUpsert) error {
	f := u.Finding
	args := []any{tenantID, f.ID, string(f.Signal), f.SourceCatalog, f.TargetCatalog, encodeJSON(f.Evidence), f.EvidenceHash,
		encodeJSON(f.Concerns), encodeJSON(f.Targets), encodeJSON(f.EdgeIDs), string(f.Status), f.GraphApplied}
	if u.ExpectState == "" {
		if err := tx.Exec(ctx, insertFindingSQL, args...); err != nil {
			if db.IsPgErrorCode(err, "23505") {
				return staleRun(f.ID)
			}
			return fmt.Errorf("finding %s: %w", f.ID, err)
		}
		return nil
	}
	current, err := readFinding(ctx, tx, "finding", lockFindingSQL, tenantID, f.ID)
	switch {
	case errors.Is(err, ErrFindingNotFound):
		return staleRun(f.ID)
	case err != nil:
		return err
	case StateToken(current) != u.ExpectState:
		return staleRun(f.ID)
	}
	if err := tx.Exec(ctx, updateFindingSQL, args...); err != nil {
		return fmt.Errorf("finding %s: %w", f.ID, err)
	}
	return nil
}

// insertDecision records a validated decision. The table's foreign key and
// primary key refuse a decision on a finding not stored and a decision
// already recorded; those refusals get messages saying what to do.
func insertDecision(ctx context.Context, tx db.Transaction, tenantID string, d Decision) error {
	var decidedAt any
	if !d.DecidedAt.IsZero() {
		decidedAt = d.DecidedAt
	}
	err := tx.Exec(ctx, insertDecisionSQL, tenantID, d.ID, d.FindingID, d.Verdict, d.DecidedBy, d.RunID, d.Rationale,
		d.Request, d.Attestation, decidedAt)
	switch {
	case err == nil:
		return nil
	case db.IsPgErrorCode(err, "23503"):
		return fmt.Errorf("decision %s refers to finding %s, which is not stored; upsert the finding in the same run; nothing was written",
			d.ID, d.FindingID)
	case db.IsPgErrorCode(err, "23505"):
		return fmt.Errorf("decision %s is already recorded: a decision ID is the hash of its request, so this request was already applied; nothing was written", d.ID)
	}
	return fmt.Errorf("decision %s on finding %s: %w", d.ID, d.FindingID, err)
}

func (s *PGFindingStore) ApplyRun(ctx context.Context, tenantID string, w RunWrite) error {
	return s.observe(ctx, "ApplyRun", tenantID, func(ctx context.Context) error {
		for _, u := range w.Upserts {
			if err := u.Finding.Validate(); err != nil {
				return fmt.Errorf("apply run: %w", err)
			}
		}
		for _, d := range w.Decisions {
			if d.DecidedBy != DecidedByPolicy {
				return fmt.Errorf("apply run: decision %s is decided by %q: ApplyRun records only policy decisions; record a reviewer's decision with Decide; nothing was written",
					d.ID, d.DecidedBy)
			}
			if err := d.Validate(); err != nil {
				return fmt.Errorf("apply run: %w", err)
			}
		}
		return s.inTx(ctx, tenantID, func(tx db.Transaction) error {
			for _, u := range w.Upserts {
				if err := writeUpsert(ctx, tx, tenantID, u); err != nil {
					return fmt.Errorf("apply run: %w", err)
				}
			}
			for _, d := range w.Decisions {
				if err := insertDecision(ctx, tx, tenantID, d); err != nil {
					return fmt.Errorf("apply run: %w", err)
				}
			}
			return nil
		})
	})
}

const decideUpdateSQL = `
UPDATE edge_findings
SET status = $3, targets = $4::jsonb, edge_ids = $5::jsonb, graph_applied = false, updated_at = now()
WHERE tenant_id = $1 AND finding_id = $2`

func (s *PGFindingStore) Decide(ctx context.Context, tenantID string, d Decision, expectState string, next StoredFinding) error {
	return s.observe(ctx, "Decide", tenantID, func(ctx context.Context) error {
		if next.ID != d.FindingID {
			return fmt.Errorf("decide finding %s: next is finding %s; pass the decided finding's next state", d.FindingID, next.ID)
		}
		if d.DecidedBy == DecidedByPolicy {
			return fmt.Errorf("decide finding %s: policy decisions are recorded only by ApplyRun; decide as a registered reviewer", d.FindingID)
		}
		if err := d.Validate(); err != nil {
			return fmt.Errorf("decide finding %s: %w", d.FindingID, err)
		}
		if err := next.Validate(); err != nil {
			return fmt.Errorf("decide finding %s: %w", d.FindingID, err)
		}
		return s.inTx(ctx, tenantID, func(tx db.Transaction) error {
			// Locks the reviewer row FOR SHARE until commit, so the reviewer
			// cannot be deleted while this decision is recorded.
			if err := tx.Exec(ctx, "SELECT public.require_active_reviewer($1)", d.DecidedBy); err != nil {
				switch {
				case db.IsPgErrorCode(err, "P0002"):
					return fmt.Errorf("reviewer %q is not registered for tenant %q; nothing was recorded. Register them with 'crosscodexd admin reviewer add --tenant %s --reviewer %s --by <operator> --reason <text>': %w",
						d.DecidedBy, tenantID, tenantID, d.DecidedBy, ErrReviewerNotRegistered)
				case db.IsPgErrorCode(err, "55000"):
					// The ID is left out: the message reaches span status, and a
					// deleted reviewer may be hidden.
					return fmt.Errorf("the deciding reviewer was deleted; nothing was recorded. Restore them with 'crosscodexd admin reviewer restore --tenant %s --reviewer <reviewer> --by <operator> --reason <text>' (run 'crosscodexd admin reviewer unhide --tenant %s --reviewer <reviewer> --by <operator> --reason <text>' first if they are hidden) or decide as another reviewer: %w",
						tenantID, tenantID, ErrReviewerDeleted)
				}
				return fmt.Errorf("decide finding %s: check reviewer: %w", d.FindingID, err)
			}
			current, err := readFinding(ctx, tx, "decide finding", lockFindingSQL, tenantID, d.FindingID)
			if err != nil {
				return err
			}
			if StateToken(current) != expectState {
				return fmt.Errorf("finding %s changed since it was read (it is now %s); nothing was recorded. "+
					"Reload the finding and decide again with its current state token: %w", d.FindingID, current.Status, ErrStateChanged)
			}
			if err := insertDecision(ctx, tx, tenantID, d); err != nil {
				return fmt.Errorf("decide finding %s: %w", d.FindingID, err)
			}
			if err := tx.Exec(ctx, decideUpdateSQL, tenantID, d.FindingID, string(next.Status),
				encodeJSON(next.Targets), encodeJSON(next.EdgeIDs)); err != nil {
				return fmt.Errorf("decide finding %s: %w", d.FindingID, err)
			}
			return nil
		})
	})
}

var unprojectedSQL = `SELECT ` + findingColumnsSQL + ` FROM edge_findings
WHERE tenant_id = $1 AND NOT graph_applied
ORDER BY finding_id`

func (s *PGFindingStore) Unprojected(ctx context.Context, tenantID string) ([]StoredFinding, error) {
	var out []StoredFinding
	err := s.observe(ctx, "Unprojected", tenantID, func(ctx context.Context) error {
		return s.inTx(ctx, tenantID, func(tx db.Transaction) error {
			var err error
			out, err = queryFindings(ctx, tx, "unprojected findings", unprojectedSQL, tenantID)
			return err
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

const markProjectedSQL = `
UPDATE edge_findings SET graph_applied = true, edge_ids = $3::jsonb, updated_at = now()
WHERE tenant_id = $1 AND finding_id = $2
RETURNING finding_id`

func (s *PGFindingStore) MarkProjected(ctx context.Context, tenantID, findingID string, edgeIDs []string) error {
	return s.observe(ctx, "MarkProjected", tenantID, func(ctx context.Context) error {
		// The same bound StoredFinding.Validate applies, so the row reads back.
		edges := encodeJSON(edgeIDs)
		if len(edges) > storedListLimit {
			return fmt.Errorf("mark finding %s projected: %w: the edge_ids column would hold %d bytes of JSON, over the %d-byte limit within which a stored finding reads back, so nothing was written",
				findingID, ErrFindingTooLarge, len(edges), storedListLimit)
		}
		return s.inTx(ctx, tenantID, func(tx db.Transaction) error {
			var id string
			err := tx.QueryRow(ctx, markProjectedSQL, tenantID, findingID, edges).Scan(&id)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				return fmt.Errorf("mark finding %s projected: %w", findingID, ErrFindingNotFound)
			case err != nil:
				return fmt.Errorf("mark finding %s projected: %w", findingID, err)
			}
			return nil
		})
	})
}

func (s *PGFindingStore) PendingCount(ctx context.Context, tenantID string) (int64, error) {
	var n int64
	err := s.observe(ctx, "PendingCount", tenantID, func(ctx context.Context) error {
		return s.inTx(ctx, tenantID, func(tx db.Transaction) error {
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM edge_findings WHERE tenant_id = $1 AND status = 'pending'`,
				tenantID).Scan(&n); err != nil {
				return fmt.Errorf("pending findings: %w", err)
			}
			return nil
		})
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}
