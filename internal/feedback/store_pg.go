package feedback

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	analyzerresults "github.com/complytime-labs/crosscodex/pkg/analyzer/results"
	"github.com/complytime-labs/crosscodex/pkg/cleaning"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// PGStore is the Store over the mapping_votes, mapping_determinations and
// mapping_reviews tables, reading mappings from vote_summaries. Each method
// runs in one transaction on a db.TenantConnection, so row-level security
// confines it to the tenant in ctx, which must equal the tenantID argument.
// The connection's role is the restricted application role, so that a
// compromised daemon can only append votes and determinations, never
// rewrite or remove them (see migration 003_mapping_feedback).
type PGStore struct {
	conn     db.TenantConnection
	tracer   trace.Tracer          // optional, nil-safe
	ops      metric.Int64Counter   // optional, nil-safe
	duration metric.Int64Histogram // optional, nil-safe
}

var _ Store = (*PGStore)(nil)

// StoreOption configures a PGStore.
type StoreOption func(*PGStore)

// WithStoreTelemetry enables tracing and metrics for the store. Either
// provider may be nil; nil providers are ignored. It has no pkg/config
// field: whoever builds the store passes the daemon's providers.
func WithStoreTelemetry(tp trace.TracerProvider, mp metric.MeterProvider) StoreOption {
	return func(s *PGStore) {
		if tp != nil {
			s.tracer = tp.Tracer("crosscodex/internal/feedback")
		}
		if mp != nil {
			meter := mp.Meter("crosscodex")
			var err error
			if s.ops, err = meter.Int64Counter("feedback.store.operations.total",
				metric.WithDescription("Feedback store calls by operation and status (ok or error)")); err != nil {
				s.ops = nil
				slog.Warn("feedback: feedback.store.operations.total is not recorded", "error", err)
			}
			if s.duration, err = meter.Int64Histogram("feedback.store.operation.duration_ms",
				metric.WithDescription("Feedback store call duration in milliseconds by operation and status"), metric.WithUnit("ms")); err != nil {
				s.duration = nil
				slog.Warn("feedback: feedback.store.operation.duration_ms is not recorded", "error", err)
			}
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
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "feedback.store."+op)
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
func (s *PGStore) inTx(ctx context.Context, tenantID string, fn func(tx db.Transaction) error) error {
	ctxTenant, err := tenant.FromContext(ctx)
	if err != nil {
		return fmt.Errorf("feedback store: %w", err)
	}
	if ctxTenant != tenantID {
		return fmt.Errorf("feedback store: context tenant %q does not match tenant %q", ctxTenant, tenantID)
	}
	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("feedback store: begin: %w", err)
	}
	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			return errors.Join(err, fmt.Errorf("feedback store: rollback: %w", rbErr))
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("feedback store: commit: %w", err)
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

// encodeJSON returns v's JSON as a string for a ::jsonb parameter. Callers
// pass string slices and string maps, which always encode.
func encodeJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("feedback: encode a string slice or map: %v", err))
	}
	return string(b)
}

// edgeIDsJSON encodes edge IDs for mapping_reviews.edge_ids, none as [] so
// the mapping_cleaning_states view's "changed no edges" test holds.
func edgeIDsJSON(ids []string) string {
	if len(ids) == 0 {
		return "[]"
	}
	return encodeJSON(ids)
}

// requireCompleted fails unless jobID names a completed job of the tenant.
// Callers pass a ctx without a user (tenant.WithoutUser): the jobs policy
// would otherwise hide other users' jobs, and reviewers review jobs other
// users created.
func requireCompleted(ctx context.Context, tx db.Transaction, op, tenantID, jobID string) error {
	var status string
	err := tx.QueryRow(ctx, `SELECT status FROM jobs WHERE job_id = $1 AND tenant_id = $2`, jobID, tenantID).Scan(&status)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%s: job %s: %w", op, jobID, ErrJobNotFound)
	case err != nil:
		return fmt.Errorf("%s: read job %s: %w", op, jobID, err)
	case status != jobStatusCompleted:
		return &JobNotCompletedError{JobID: jobID, Status: status}
	}
	return nil
}

// mappingExistsSQL tests for the relationship analyzer's vote_summaries row
// of one pair: a mapping is that contribution only (see internal/results'
// synthesizedSQL).
const mappingExistsSQL = `
SELECT EXISTS (
    SELECT 1 FROM vote_summaries
    WHERE tenant_id = $1 AND job_id = $2 AND source_id = $3 AND target_id = $4 AND analyzer_name = $5)`

// requireMapping fails unless (jobID, sourceID, targetID) is a mapping of a
// completed job of the tenant; see requireCompleted for ctx.
func requireMapping(ctx context.Context, tx db.Transaction, op, tenantID, mappingID, jobID, sourceID, targetID string) error {
	if err := requireCompleted(ctx, tx, op, tenantID, jobID); err != nil {
		return err
	}
	var exists bool
	if err := tx.QueryRow(ctx, mappingExistsSQL, tenantID, jobID, sourceID, targetID,
		analyzerresults.RelationshipAnalyzer).Scan(&exists); err != nil {
		return fmt.Errorf("%s: read mapping %s: %w", op, mappingID, err)
	}
	if !exists {
		return fmt.Errorf("%s: %w", op, mappingNotFound(mappingID, jobID))
	}
	return nil
}

// jobMappingsSQL reads a job's synthesized mappings; COLLATE "C" sorts
// byte-wise whatever the database collation.
const jobMappingsSQL = `
SELECT source_id, target_id, consensus, confidence, viability
FROM vote_summaries
WHERE tenant_id = $1 AND job_id = $2 AND analyzer_name = $3
ORDER BY source_id COLLATE "C", target_id COLLATE "C"`

func (s *PGStore) JobMappings(ctx context.Context, tenantID, jobID string) ([]Mapping, error) {
	var out []Mapping
	// Tenant-wide by contract (see Store.JobMappings): the jobs policy would
	// show a ctx carrying a user only that user's jobs.
	ctx = tenant.WithoutUser(ctx)
	err := s.observe(ctx, "JobMappings", tenantID, func(ctx context.Context) error {
		return s.inTx(ctx, tenantID, func(tx db.Transaction) error {
			if err := requireCompleted(ctx, tx, "job mappings", tenantID, jobID); err != nil {
				return err
			}
			rows, err := tx.Query(ctx, jobMappingsSQL, tenantID, jobID, analyzerresults.RelationshipAnalyzer)
			if err != nil {
				return fmt.Errorf("job mappings: query vote summaries of job %s: %w", jobID, err)
			}
			return scanRows(rows, func() error {
				m := Mapping{JobID: jobID}
				if err := rows.Scan(&m.SourceID, &m.TargetID, &m.RelationshipType, &m.Confidence, &m.Viability); err != nil {
					return fmt.Errorf("job mappings: scan vote summary of job %s: %w", jobID, err)
				}
				m.ID = MappingID(jobID, m.SourceID, m.TargetID)
				out = append(out, m)
				return nil
			})
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

const insertVoteSQL = `
INSERT INTO mapping_votes
    (tenant_id, vote_id, mapping_id, job_id, source_id, target_id, voter, vote_type, suggested_type, rationale, evidence, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9, ''), $10, $11::jsonb, $12)`

func (s *PGStore) RecordVote(ctx context.Context, tenantID string, v Vote) error {
	// The mapping check reads jobs tenant-wide, as JobMappings does.
	ctx = tenant.WithoutUser(ctx)
	return s.observe(ctx, "RecordVote", tenantID, func(ctx context.Context) error {
		if err := v.Validate(); err != nil {
			return err
		}
		evidence := v.Evidence
		if evidence == nil {
			evidence = map[string]string{}
		}
		return s.inTx(ctx, tenantID, func(tx db.Transaction) error {
			if err := requireMapping(ctx, tx, "record vote", tenantID, v.MappingID, v.JobID, v.SourceID, v.TargetID); err != nil {
				return err
			}
			err := tx.Exec(ctx, insertVoteSQL, tenantID, v.ID, v.MappingID, v.JobID, v.SourceID, v.TargetID, v.Voter,
				string(v.Type), v.SuggestedType, v.Rationale, encodeJSON(evidence), v.CreatedAt)
			switch {
			case db.IsPgErrorCode(err, "23505"):
				return fmt.Errorf("vote %s is already recorded; record a new vote with a new ID; nothing was recorded", v.ID)
			case err != nil:
				return fmt.Errorf("record vote %s: %w", v.ID, err)
			}
			return nil
		})
	})
}

// latestVotesSQL keeps each (mapping, voter)'s latest vote: DISTINCT ON
// keeps the first row of each group in ORDER BY order.
const latestVotesSQL = `
SELECT DISTINCT ON (mapping_id COLLATE "C", voter COLLATE "C")
       vote_id, mapping_id, job_id, source_id, target_id, voter, vote_type, COALESCE(suggested_type, ''),
       rationale, evidence, created_at
FROM mapping_votes
WHERE tenant_id = $1 AND job_id = $2
ORDER BY mapping_id COLLATE "C", voter COLLATE "C", created_at DESC, vote_id COLLATE "C" DESC`

func (s *PGStore) LatestVotes(ctx context.Context, tenantID, jobID string) ([]Vote, error) {
	var out []Vote
	err := s.observe(ctx, "LatestVotes", tenantID, func(ctx context.Context) error {
		return s.inTx(ctx, tenantID, func(tx db.Transaction) error {
			rows, err := tx.Query(ctx, latestVotesSQL, tenantID, jobID)
			if err != nil {
				return fmt.Errorf("latest votes of job %s: %w", jobID, err)
			}
			return scanRows(rows, func() error {
				var (
					v        Vote
					voteType string
					evidence []byte
				)
				if err := rows.Scan(&v.ID, &v.MappingID, &v.JobID, &v.SourceID, &v.TargetID, &v.Voter, &voteType,
					&v.SuggestedType, &v.Rationale, &evidence, &v.CreatedAt); err != nil {
					return fmt.Errorf("latest votes of job %s: %w", jobID, err)
				}
				if err := json.Unmarshal(evidence, &v.Evidence); err != nil {
					return fmt.Errorf("latest votes of job %s: vote %s: evidence: %w", jobID, v.ID, err)
				}
				if len(v.Evidence) == 0 {
					v.Evidence = nil
				}
				v.Type, v.CreatedAt = VoteType(voteType), v.CreatedAt.UTC()
				out = append(out, v)
				return nil
			})
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// reviewColumnsSQL is what scanReview reads, in order.
const reviewColumnsSQL = `mapping_id, job_id, source_id, target_id, status, determination_id, decided_at, edge_ids, graph_applied`

const getReviewSQL = `SELECT ` + reviewColumnsSQL + ` FROM mapping_reviews WHERE tenant_id = $1 AND mapping_id = $2`

const lockReviewSQL = getReviewSQL + ` FOR UPDATE`

// scanReview reads one reviewColumnsSQL row through scan.
func scanReview(scan func(dest ...any) error) (Review, error) {
	var (
		r      Review
		status string
		edges  []byte
	)
	if err := scan(&r.MappingID, &r.JobID, &r.SourceID, &r.TargetID, &status, &r.DeterminationID, &r.DecidedAt,
		&edges, &r.GraphApplied); err != nil {
		return Review{}, err
	}
	if err := json.Unmarshal(edges, &r.EdgeIDs); err != nil {
		return Review{}, fmt.Errorf("review of mapping %s: edge IDs: %w", r.MappingID, err)
	}
	if len(r.EdgeIDs) == 0 {
		r.EdgeIDs = nil
	}
	r.Status, r.DecidedAt = ReviewStatus(status), r.DecidedAt.UTC()
	return r, nil
}

// readReview returns the mapping's review, locking it when query does, and
// false when it has none.
func readReview(ctx context.Context, tx db.Transaction, op, query, tenantID, mappingID string) (Review, bool, error) {
	r, err := scanReview(tx.QueryRow(ctx, query, tenantID, mappingID).Scan)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Review{}, false, nil
	case err != nil:
		return Review{}, false, fmt.Errorf("%s %s: %w", op, mappingID, err)
	}
	return r, true, nil
}

// queryReviews runs a query selecting reviewColumnsSQL and decodes every row.
func queryReviews(ctx context.Context, tx db.Transaction, op, query string, args ...any) ([]Review, error) {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	var out []Review
	err = scanRows(rows, func() error {
		r, err := scanReview(rows.Scan)
		if err != nil {
			return fmt.Errorf("%s: %w", op, err)
		}
		out = append(out, r)
		return nil
	})
	return out, err
}

func (s *PGStore) Review(ctx context.Context, tenantID, mappingID string) (Review, bool, error) {
	var (
		r     Review
		found bool
	)
	err := s.observe(ctx, "Review", tenantID, func(ctx context.Context) error {
		return s.inTx(ctx, tenantID, func(tx db.Transaction) error {
			var err error
			r, found, err = readReview(ctx, tx, "review of mapping", getReviewSQL, tenantID, mappingID)
			return err
		})
	})
	if err != nil {
		return Review{}, false, err
	}
	return r, found, nil
}

const insertDeterminationSQL = `
INSERT INTO mapping_determinations
    (tenant_id, determination_id, mapping_id, job_id, source_id, target_id, outcome, decided_by, rationale,
     request, attestation, decided_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`

// insertDetermination records a validated determination; a determination ID
// already stored is refused with a message saying why.
func insertDetermination(ctx context.Context, tx db.Transaction, tenantID string, d Determination) error {
	err := tx.Exec(ctx, insertDeterminationSQL, tenantID, d.ID, d.MappingID, d.JobID, d.SourceID, d.TargetID,
		string(d.Outcome), d.DecidedBy, d.Rationale, d.Request, d.Attestation, d.DecidedAt)
	switch {
	case err == nil:
		return nil
	case db.IsPgErrorCode(err, "23505"):
		return fmt.Errorf("determination %s is already recorded: a determination ID is the hash of its request, so this request was already applied; nothing was recorded", d.ID)
	}
	return fmt.Errorf("determination %s on mapping %s: %w", d.ID, d.MappingID, err)
}

// reviewerCheck maps public.require_active_reviewer's refusals to the
// cleaning sentinels, as pkg/cleaning's store does for decisions.
func reviewerCheck(err error, tenantID, reviewer string) error {
	switch {
	case db.IsPgErrorCode(err, "P0002"):
		return fmt.Errorf("reviewer %q is not registered for tenant %q; nothing was recorded. Register them with 'crosscodexd admin reviewer add --tenant %s --reviewer %s --by <operator> --reason <text>': %w",
			reviewer, tenantID, tenantID, reviewer, cleaning.ErrReviewerNotRegistered)
	case db.IsPgErrorCode(err, "55000"):
		// The ID is left out: the message reaches span status, and a deleted
		// reviewer may be hidden.
		return fmt.Errorf("the deciding reviewer was deleted; nothing was recorded. Restore them with 'crosscodexd admin reviewer restore --tenant %s --reviewer <reviewer> --by <operator> --reason <text>' (run 'crosscodexd admin reviewer unhide --tenant %s --reviewer <reviewer> --by <operator> --reason <text>' first if they are hidden) or decide as another reviewer: %w",
			tenantID, tenantID, cleaning.ErrReviewerDeleted)
	}
	return fmt.Errorf("check reviewer: %w", err)
}

const insertReviewSQL = `
INSERT INTO mapping_reviews
    (tenant_id, mapping_id, job_id, source_id, target_id, status, determination_id, decided_at, edge_ids, graph_applied)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, false)`

const updateReviewSQL = `
UPDATE mapping_reviews
SET status = $3, determination_id = $4, decided_at = $5, edge_ids = $6::jsonb, graph_applied = false, updated_at = now()
WHERE tenant_id = $1 AND mapping_id = $2`

func (s *PGStore) Determine(ctx context.Context, tenantID string, d Determination, expectPrev string, next Review) error {
	// The mapping check reads jobs tenant-wide, as JobMappings does.
	ctx = tenant.WithoutUser(ctx)
	return s.observe(ctx, "Determine", tenantID, func(ctx context.Context) error {
		if err := checkDetermine(d, next); err != nil {
			return err
		}
		if err := checkEdgeIDs("determine mapping", d.MappingID, next.EdgeIDs); err != nil {
			return err
		}
		return s.inTx(ctx, tenantID, func(tx db.Transaction) error {
			// Locks the reviewer row FOR SHARE until commit, so the reviewer
			// cannot be deleted while this determination is recorded.
			if err := tx.Exec(ctx, "SELECT public.require_active_reviewer($1)", d.DecidedBy); err != nil {
				return fmt.Errorf("determine mapping %s: %w", d.MappingID, reviewerCheck(err, tenantID, d.DecidedBy))
			}
			if err := requireMapping(ctx, tx, "determine mapping", tenantID, d.MappingID, d.JobID, d.SourceID, d.TargetID); err != nil {
				return err
			}
			current, found, err := readReview(ctx, tx, "determine mapping", lockReviewSQL, tenantID, d.MappingID)
			if err != nil {
				return err
			}
			if current.DeterminationID != expectPrev {
				return reviewChanged(d.MappingID, current.DeterminationID)
			}
			if err := insertDetermination(ctx, tx, tenantID, d); err != nil {
				return fmt.Errorf("determine mapping %s: %w", d.MappingID, err)
			}
			edges := edgeIDsJSON(next.EdgeIDs)
			if found {
				err = tx.Exec(ctx, updateReviewSQL, tenantID, d.MappingID, string(next.Status), d.ID, d.DecidedAt, edges)
			} else {
				err = tx.Exec(ctx, insertReviewSQL, tenantID, d.MappingID, d.JobID, d.SourceID, d.TargetID,
					string(next.Status), d.ID, d.DecidedAt, edges)
			}
			switch {
			case !found && db.IsPgErrorCode(err, "23505"):
				// Another determination inserted the first review since the
				// lock found none.
				return reviewChanged(d.MappingID, "another determination")
			case err != nil:
				return fmt.Errorf("determine mapping %s: write review: %w", d.MappingID, err)
			}
			return nil
		})
	})
}

const markStaleSQL = `
UPDATE mapping_reviews
SET status = 'stale', determination_id = $3, decided_at = $4, graph_applied = true, updated_at = now()
WHERE tenant_id = $1 AND mapping_id = $2`

func (s *PGStore) MarkStale(ctx context.Context, tenantID string, d Determination, expectPrev string) error {
	return s.observe(ctx, "MarkStale", tenantID, func(ctx context.Context) error {
		if err := checkStale(d); err != nil {
			return err
		}
		return s.inTx(ctx, tenantID, func(tx db.Transaction) error {
			current, found, err := readReview(ctx, tx, "mark mapping stale", lockReviewSQL, tenantID, d.MappingID)
			switch {
			case err != nil:
				return err
			case !found:
				return fmt.Errorf("mark mapping %s stale: %w", d.MappingID, ErrReviewNotFound)
			case current.DeterminationID != expectPrev:
				return reviewChanged(d.MappingID, current.DeterminationID)
			}
			if err := insertDetermination(ctx, tx, tenantID, d); err != nil {
				return fmt.Errorf("mark mapping %s stale: %w", d.MappingID, err)
			}
			if err := tx.Exec(ctx, markStaleSQL, tenantID, d.MappingID, d.ID, d.DecidedAt); err != nil {
				return fmt.Errorf("mark mapping %s stale: %w", d.MappingID, err)
			}
			return nil
		})
	})
}

const markReprojectedSQL = `UPDATE mapping_reviews SET edge_ids = $3::jsonb WHERE tenant_id = $1 AND mapping_id = $2`

func (s *PGStore) MarkReprojected(ctx context.Context, tenantID, mappingID, expectDetermination string, edgeIDs []string) error {
	return s.observe(ctx, "MarkReprojected", tenantID, func(ctx context.Context) error {
		if err := checkEdgeIDs("re-project mapping", mappingID, edgeIDs); err != nil {
			return err
		}
		return s.inTx(ctx, tenantID, func(tx db.Transaction) error {
			current, found, err := readReview(ctx, tx, "re-project mapping", lockReviewSQL, tenantID, mappingID)
			switch {
			case err != nil:
				return err
			case !found:
				return fmt.Errorf("re-project mapping %s: %w", mappingID, ErrReviewNotFound)
			case current.DeterminationID != expectDetermination:
				return reviewChanged(mappingID, current.DeterminationID)
			case !current.GraphApplied:
				return unprojected(mappingID)
			}
			if err := tx.Exec(ctx, markReprojectedSQL, tenantID, mappingID, edgeIDsJSON(edgeIDs)); err != nil {
				return fmt.Errorf("re-project mapping %s: %w", mappingID, err)
			}
			return nil
		})
	})
}

var unprojectedSQL = `SELECT ` + reviewColumnsSQL + ` FROM mapping_reviews
WHERE tenant_id = $1 AND NOT graph_applied
ORDER BY mapping_id COLLATE "C"`

func (s *PGStore) Unprojected(ctx context.Context, tenantID string) ([]Review, error) {
	var out []Review
	err := s.observe(ctx, "Unprojected", tenantID, func(ctx context.Context) error {
		return s.inTx(ctx, tenantID, func(tx db.Transaction) error {
			var err error
			out, err = queryReviews(ctx, tx, "unprojected reviews", unprojectedSQL, tenantID)
			return err
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

var reviewsSQL = `SELECT ` + reviewColumnsSQL + ` FROM mapping_reviews
WHERE tenant_id = $1
ORDER BY mapping_id COLLATE "C"`

func (s *PGStore) Reviews(ctx context.Context, tenantID string) ([]Review, error) {
	var out []Review
	err := s.observe(ctx, "Reviews", tenantID, func(ctx context.Context) error {
		return s.inTx(ctx, tenantID, func(tx db.Transaction) error {
			var err error
			out, err = queryReviews(ctx, tx, "reviews", reviewsSQL, tenantID)
			return err
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

const markProjectedSQL = `
UPDATE mapping_reviews SET graph_applied = true, edge_ids = $3::jsonb, updated_at = now()
WHERE tenant_id = $1 AND mapping_id = $2`

func (s *PGStore) MarkProjected(ctx context.Context, tenantID, mappingID, expectDetermination string, edgeIDs []string) error {
	return s.observe(ctx, "MarkProjected", tenantID, func(ctx context.Context) error {
		if err := checkEdgeIDs("mark mapping projected", mappingID, edgeIDs); err != nil {
			return err
		}
		return s.inTx(ctx, tenantID, func(tx db.Transaction) error {
			current, found, err := readReview(ctx, tx, "mark mapping projected", lockReviewSQL, tenantID, mappingID)
			switch {
			case err != nil:
				return err
			case !found:
				return fmt.Errorf("mark mapping %s projected: %w", mappingID, ErrReviewNotFound)
			case current.DeterminationID != expectDetermination:
				// A later determination is recorded; its own projection
				// still has to run, so this one must not mark it applied.
				return reviewChanged(mappingID, current.DeterminationID)
			}
			if err := tx.Exec(ctx, markProjectedSQL, tenantID, mappingID, edgeIDsJSON(edgeIDs)); err != nil {
				return fmt.Errorf("mark mapping %s projected: %w", mappingID, err)
			}
			return nil
		})
	})
}
