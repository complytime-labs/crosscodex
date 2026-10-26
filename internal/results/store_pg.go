package results

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	analyzerresults "github.com/complytime-labs/crosscodex/pkg/analyzer/results"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// jobStatusCompleted mirrors pipeline.JobStatusCompleted
// (internal/pipeline/types.go); internal packages do not import each other.
const jobStatusCompleted = "completed"

// PGStore is the Reader over vote_summaries and analysis_results. Each call
// runs in one transaction on a db.TenantConnection, so row-level security
// confines it to the tenant in ctx, which Filter.TenantID must equal.
type PGStore struct {
	conn     db.TenantConnection
	tracer   trace.Tracer          // optional, nil-safe
	ops      metric.Int64Counter   // optional, nil-safe
	duration metric.Int64Histogram // optional, nil-safe
	// analyzerBytes is the size of the last result_data decoded by an
	// analyzer-stage read; optional, nil-safe.
	analyzerBytes metric.Float64Gauge
}

var _ Reader = (*PGStore)(nil)

// StoreOption configures a PGStore.
type StoreOption func(*PGStore)

// WithStoreTelemetry enables tracing and metrics for the store.
// Either provider may be nil; nil providers are ignored.
func WithStoreTelemetry(tp trace.TracerProvider, mp metric.MeterProvider) StoreOption {
	return func(s *PGStore) {
		if tp != nil {
			s.tracer = tp.Tracer("crosscodex/internal/results")
		}
		if mp != nil {
			meter := mp.Meter("crosscodex")
			s.ops, _ = meter.Int64Counter("results.store.operations.total",
				metric.WithDescription("Results store calls by operation and status (ok or error)"))
			s.duration, _ = meter.Int64Histogram("results.store.operation.duration_ms",
				metric.WithDescription("Results store call duration in milliseconds by operation and status"), metric.WithUnit("ms"))
			s.analyzerBytes, _ = meter.Float64Gauge("results.store.analyzer_result_bytes",
				metric.WithDescription("Size in bytes of the relationship analyzer result_data decoded for the last analyzer-stage read"), metric.WithUnit("By"))
		}
	}
}

// NewPGStore returns a store over conn.
func NewPGStore(conn db.TenantConnection, opts ...StoreOption) *PGStore {
	s := &PGStore{conn: conn}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// observe runs one public method's body in a span named after op, then
// counts and times the call by operation and status.
func (s *PGStore) observe(ctx context.Context, op, tenantID string, fn func(ctx context.Context) error) error {
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "results.store."+op)
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
// succeeds. RLS keys off ctx's tenant, so a tenantID that differs from it is
// refused rather than silently reading another tenant's rows.
func (s *PGStore) inTx(ctx context.Context, tenantID string, fn func(tx db.Transaction) error) error {
	ctxTenant, err := tenant.FromContext(ctx)
	if err != nil {
		return fmt.Errorf("results store: %w", err)
	}
	if ctxTenant != tenantID {
		return fmt.Errorf("results store: context tenant %q does not match filter tenant %q", ctxTenant, tenantID)
	}
	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("results store: begin: %w", err)
	}
	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			return errors.Join(err, fmt.Errorf("results store: rollback: %w", rbErr))
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("results store: commit: %w", err)
	}
	return nil
}

// synthesizedSQL pages vote_summaries by (job_id, source_id, target_id).
// Mappings are the relationship contribution only: requires rows are a
// different signal outside the relationship enum (see
// synthesis.ConsensusRequires), and a pair both analyzers reported has one
// row per contribution, so filtering on analyzer_name = $11 (analyzerresults.RelationshipAnalyzer)
// also keeps (job_id, source_id, target_id) unique for the keyset. A named
// job is checked for completion by requireCompleted; the all-jobs path ($2 empty) keeps only rows of completed
// jobs, so in-flight jobs' provisional rows never surface. Real rows sort after the empty cursor
// ("","","") because job_id is never empty.
const synthesizedSQL = `
SELECT job_id, source_id, target_id, consensus, confidence, viability
FROM vote_summaries
WHERE tenant_id = $1
  AND analyzer_name = $11
  AND ($2::text = '' OR job_id = $2)
  AND ($2::text <> '' OR EXISTS (
        SELECT 1 FROM jobs j
        WHERE j.job_id = vote_summaries.job_id AND j.tenant_id = $1 AND j.status = 'completed'))
  AND ($3::text = '' OR source_id = $3 OR target_id = $3)
  AND confidence >= $4::float8
  AND ($5::bool OR viability > 0)
  AND consensus = ANY($6::text[])
  AND (job_id, source_id, target_id) > ($7::text, $8::text, $9::text)
ORDER BY job_id, source_id, target_id
LIMIT $10`

// ListMappings returns one page of mappings for f.
func (s *PGStore) ListMappings(ctx context.Context, f Filter) (Page, error) {
	var page Page
	err := s.observe(ctx, "ListMappings", f.TenantID, func(ctx context.Context) error {
		return s.inTx(ctx, f.TenantID, func(tx db.Transaction) error {
			if f.JobID != "" {
				if err := requireCompleted(ctx, tx, f); err != nil {
					return err
				}
			}
			var rows []Mapping
			var err error
			switch f.Stage {
			case StageSynthesized:
				rows, err = listSynthesized(ctx, tx, f)
			case StageAnalyzer:
				rows, err = s.listAnalyzer(ctx, tx, f)
			default:
				err = fmt.Errorf("list mappings: unknown stage %d", f.Stage)
			}
			if err != nil {
				return err
			}
			page = paginate(rows, f.PageSize)
			return nil
		})
	})
	if err != nil {
		return Page{}, err
	}
	return page, nil
}

// requireCompleted fails unless f.JobID names a completed job of the tenant.
func requireCompleted(ctx context.Context, tx db.Transaction, f Filter) error {
	var status string
	err := tx.QueryRow(ctx, `SELECT status FROM jobs WHERE job_id = $1 AND tenant_id = $2`, f.JobID, f.TenantID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("list mappings: %w", ErrJobNotFound)
	}
	if err != nil {
		return fmt.Errorf("list mappings: read job %s: %w", f.JobID, err)
	}
	if status != jobStatusCompleted {
		return &JobNotCompletedError{JobID: f.JobID, Status: status}
	}
	return nil
}

// paginate trims rows fetched with one extra row to size and sets Next to
// the last kept row when the extra row proves another page exists.
func paginate(rows []Mapping, size int) Page {
	if len(rows) <= size {
		return Page{Mappings: rows}
	}
	rows = rows[:size]
	last := rows[size-1]
	return Page{Mappings: rows, Next: &Cursor{JobID: last.JobID, SourceID: last.SourceID, TargetID: last.TargetID}}
}

func listSynthesized(ctx context.Context, tx db.Transaction, f Filter) ([]Mapping, error) {
	after := Cursor{}
	if f.After != nil {
		after = *f.After
	}
	rs, err := tx.Query(ctx, synthesizedSQL, f.TenantID, f.JobID, f.ControlID, f.MinConfidence, f.IncludeNonViable,
		f.RelationshipTypes, after.JobID, after.SourceID, after.TargetID, f.PageSize+1, analyzerresults.RelationshipAnalyzer)
	if err != nil {
		return nil, fmt.Errorf("list mappings: query vote summaries: %w", err)
	}
	var out []Mapping
	for rs.Next() {
		m := Mapping{HasViability: true}
		if err := rs.Scan(&m.JobID, &m.SourceID, &m.TargetID, &m.RelationshipType, &m.Confidence, &m.Viability); err != nil {
			return nil, errors.Join(fmt.Errorf("list mappings: scan vote summary: %w", err), rs.Close())
		}
		out = append(out, m)
	}
	if err := errors.Join(rs.Err(), rs.Close()); err != nil {
		return nil, fmt.Errorf("list mappings: read vote summaries: %w", err)
	}
	return out, nil
}

// listAnalyzer decodes the relationship analyzer's result_data for f.JobID.
// The result is one JSON document, not rows, so every analyzer-stage page
// fetches and decodes the job's whole result_data: walking N pages costs N
// decodes. s.analyzerBytes reports the size of the last document decoded.
func (s *PGStore) listAnalyzer(ctx context.Context, tx db.Transaction, f Filter) ([]Mapping, error) {
	if f.JobID == "" {
		return nil, errors.New("list mappings: the analyzer stage needs a job ID; pass a job ID or request the synthesized stage")
	}
	var data []byte
	err := tx.QueryRow(ctx, `SELECT result_data FROM analysis_results
		WHERE tenant_id = $1 AND job_id = $2 AND analyzer_name = $3`, f.TenantID, f.JobID, analyzerresults.RelationshipAnalyzer).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil // the relationship analyzer may be disabled for the job
	}
	if err != nil {
		return nil, fmt.Errorf("list mappings: read %s result of job %s: %w", analyzerresults.RelationshipAnalyzer, f.JobID, err)
	}
	if s.analyzerBytes != nil {
		s.analyzerBytes.Record(ctx, float64(len(data)))
	}
	var matches []analyzerresults.SemanticMatchResult
	if err := json.Unmarshal(data, &matches); err != nil {
		return nil, fmt.Errorf("list mappings: decode %s result of job %s: %w", analyzerresults.RelationshipAnalyzer, f.JobID, err)
	}
	var out []Mapping
	for _, m := range matches {
		if m.Confidence < f.MinConfidence || !slices.Contains(f.RelationshipTypes, m.RelationshipType) {
			continue
		}
		if f.ControlID != "" && m.SourceID != f.ControlID && m.TargetID != f.ControlID {
			continue
		}
		out = append(out, Mapping{JobID: f.JobID, SourceID: m.SourceID, TargetID: m.TargetID,
			RelationshipType: m.RelationshipType, Confidence: m.Confidence})
	}
	slices.SortFunc(out, func(a, b Mapping) int { return compareKey(a, b.JobID, b.SourceID, b.TargetID) })
	for i := 1; i < len(out); i++ {
		if out[i-1].SourceID == out[i].SourceID && out[i-1].TargetID == out[i].TargetID {
			return nil, fmt.Errorf("list mappings: %s result of job %s lists the pair %s -> %s more than once; the stored result is corrupt, so re-run the job or ask an operator to inspect its analysis_results row",
				analyzerresults.RelationshipAnalyzer, f.JobID, out[i].SourceID, out[i].TargetID)
		}
	}
	if f.After != nil {
		out = slices.DeleteFunc(out, func(m Mapping) bool {
			return compareKey(m, f.After.JobID, f.After.SourceID, f.After.TargetID) <= 0
		})
	}
	if len(out) > f.PageSize+1 {
		out = out[:f.PageSize+1]
	}
	return out, nil
}

// compareKey orders m against a (job, source, target) tuple by byte order.
// The analyzer stage sorts and filters with it in Go; the synthesized stage
// orders in the database, by the column collation. Each stage's cursor is
// consistent with its own order, but the two orders may differ for IDs that
// collate differently from their bytes, so a cursor is not portable between
// stages. COLLATE "C" in the query would align them but stop the primary key
// index from serving ORDER BY.
func compareKey(m Mapping, jobID, sourceID, targetID string) int {
	if c := strings.Compare(m.JobID, jobID); c != 0 {
		return c
	}
	if c := strings.Compare(m.SourceID, sourceID); c != 0 {
		return c
	}
	return strings.Compare(m.TargetID, targetID)
}
