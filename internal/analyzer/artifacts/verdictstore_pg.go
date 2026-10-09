package artifacts

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// enqueueChunk bounds the rows one Enqueue INSERT carries.
const enqueueChunk = 1000

// PGVerdictStore is the VerdictStore over the artifact_pair_verdicts table
// (migration 008). Each method runs in one transaction on a
// db.TenantConnection, so row-level security confines it to the tenant in
// ctx. That tenant must equal the tenantID argument.
type PGVerdictStore struct {
	conn db.TenantConnection
}

var _ VerdictStore = (*PGVerdictStore)(nil)

// NewPGVerdictStore returns a store over conn.
func NewPGVerdictStore(conn db.TenantConnection) *PGVerdictStore {
	return &PGVerdictStore{conn: conn}
}

// dissentRecord is the dissent column: the panel's opinion on a pair a human
// had already decided.
type dissentRecord struct {
	Status       VerdictStatus `json:"status"`
	Confidence   float64       `json:"confidence"`
	DeterminedBy string        `json:"determined_by"`
	DecidedAt    time.Time     `json:"decided_at"`
	Evidence     PanelEvidence `json:"evidence"`
}

// inTx runs fn in one tenant-scoped transaction and commits it when fn
// succeeds. RLS keys off ctx's tenant, so a tenantID argument that differs
// from it is refused rather than silently reading another tenant's rows.
func (s *PGVerdictStore) inTx(ctx context.Context, tenantID string, fn func(tx db.Transaction) error) error {
	ctxTenant, err := tenant.FromContext(ctx)
	if err != nil {
		return fmt.Errorf("verdict store: %w", err)
	}
	if ctxTenant != tenantID {
		return fmt.Errorf("verdict store: context tenant %q does not match tenant %q", ctxTenant, tenantID)
	}
	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("verdict store: begin: %w", err)
	}
	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			return errors.Join(err, fmt.Errorf("verdict store: rollback: %w", rbErr))
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("verdict store: commit: %w", err)
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

const enqueueSQL = `
INSERT INTO artifact_pair_verdicts
    (tenant_id, pair_key, low_group_id, high_group_id, candidate_edge_id, similarity_score, context,
     determined_by, next_attempt_at, created_at)
SELECT $1, u.pair_key, u.low, u.high, u.edge, u.score, u.ctx::jsonb, $8, $9, $9
FROM UNNEST($2::text[], $3::text[], $4::text[], $5::text[], $6::float8[], $7::text[])
    AS u(pair_key, low, high, edge, score, ctx)
ON CONFLICT (tenant_id, pair_key) DO UPDATE
    SET candidate_edge_id = EXCLUDED.candidate_edge_id, similarity_score = EXCLUDED.similarity_score
    WHERE artifact_pair_verdicts.status = 'pending' AND artifact_pair_verdicts.determination_type <> 'human'`

func (s *PGVerdictStore) Enqueue(ctx context.Context, tenantID string, candidates []PairCandidate, now time.Time) error {
	if len(candidates) == 0 {
		return nil
	}
	return s.inTx(ctx, tenantID, func(tx db.Transaction) error {
		for start := 0; start < len(candidates); start += enqueueChunk {
			chunk := candidates[start:min(start+enqueueChunk, len(candidates))]
			keys, lows, highs, edges, contexts := make([]string, len(chunk)), make([]string, len(chunk)),
				make([]string, len(chunk)), make([]string, len(chunk)), make([]string, len(chunk))
			scores := make([]float64, len(chunk))
			for i, c := range chunk {
				b, err := json.Marshal(c.Context)
				if err != nil {
					return fmt.Errorf("enqueue: encode context of %s: %w", c.CandidateEdgeID, err)
				}
				keys[i], lows[i], highs[i] = PairKey(c.LowGroupID, c.HighGroupID), c.LowGroupID, c.HighGroupID
				edges[i], scores[i], contexts[i] = c.CandidateEdgeID, c.SimilarityScore, string(b)
			}
			if err := tx.Exec(ctx, enqueueSQL, tenantID, keys, lows, highs, edges, scores, contexts, reconcilerName, now); err != nil {
				return fmt.Errorf("enqueue pairs %d-%d: %w", start, start+len(chunk)-1, err)
			}
		}
		return nil
	})
}

func (s *PGVerdictStore) ClosedPairs(ctx context.Context, tenantID string) (map[string]bool, error) {
	closed := map[string]bool{}
	err := s.inTx(ctx, tenantID, func(tx db.Transaction) error {
		rows, err := tx.Query(ctx, `SELECT pair_key FROM artifact_pair_verdicts
			WHERE tenant_id = $1 AND status IN ('confirmed', 'rejected')`, tenantID)
		if err != nil {
			return fmt.Errorf("closed pairs: %w", err)
		}
		return scanRows(rows, func() error {
			var key string
			if err := rows.Scan(&key); err != nil {
				return fmt.Errorf("closed pairs: %w", err)
			}
			closed[key] = true
			return nil
		})
	})
	return closed, err
}

const leaseSQL = `
UPDATE artifact_pair_verdicts AS v
SET leased_until = $3
FROM (
    SELECT pair_key FROM artifact_pair_verdicts
    WHERE tenant_id = $1 AND status = 'pending' AND determination_type <> 'human'
      AND next_attempt_at <= $2 AND (leased_until IS NULL OR leased_until < $2)
    ORDER BY next_attempt_at, pair_key
    LIMIT $4
    FOR UPDATE SKIP LOCKED
) AS due
WHERE v.tenant_id = $1 AND v.pair_key = due.pair_key
RETURNING v.pair_key, v.low_group_id, v.high_group_id, v.candidate_edge_id, v.similarity_score, v.context, v.attempts,
    v.leased_until`

func (s *PGVerdictStore) Lease(ctx context.Context, tenantID string, n int, leaseFor time.Duration, now time.Time) ([]LeasedPair, error) {
	if n <= 0 || leaseFor <= 0 {
		return nil, fmt.Errorf("lease: n (%d) and leaseFor (%s) must be positive", n, leaseFor)
	}
	var out []LeasedPair
	err := s.inTx(ctx, tenantID, func(tx db.Transaction) error {
		rows, err := tx.Query(ctx, leaseSQL, tenantID, now, now.Add(leaseFor), n)
		if err != nil {
			return fmt.Errorf("lease: %w", err)
		}
		return scanRows(rows, func() error {
			var (
				p   LeasedPair
				raw []byte
			)
			if err := rows.Scan(&p.Key, &p.LowGroupID, &p.HighGroupID, &p.CandidateEdgeID, &p.SimilarityScore, &raw, &p.Attempts, &p.LeasedUntil); err != nil {
				return fmt.Errorf("lease: %w", err)
			}
			if err := json.Unmarshal(raw, &p.Context); err != nil {
				return fmt.Errorf("lease: decode context of %s: %w", p.Key, err)
			}
			out = append(out, p)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

const decideSQL = `
UPDATE artifact_pair_verdicts
SET status = $3, determination_type = 'llm_panel', determined_by = $4, confidence = $5,
    evidence = $6::jsonb, decided_at = $7, leased_until = NULL, graph_applied = false, last_error = NULL
WHERE tenant_id = $1 AND pair_key = $2 AND status = 'pending' AND determination_type <> 'human'
RETURNING candidate_edge_id`

func (s *PGVerdictStore) Decide(ctx context.Context, tenantID, key string, v Verdict) (Decision, error) {
	switch v.Status {
	case VerdictConfirmed, VerdictRejected, VerdictUndecided:
	default:
		return Decision{}, fmt.Errorf("decide %s: status %q is not a panel verdict", key, v.Status)
	}
	evidence, err := json.Marshal(v.Evidence)
	if err != nil {
		return Decision{}, fmt.Errorf("decide %s: encode evidence: %w", key, err)
	}
	var d Decision
	err = s.inTx(ctx, tenantID, func(tx db.Transaction) error {
		err := tx.QueryRow(ctx, decideSQL, tenantID, key, string(v.Status), adjudicatorName, v.Confidence, string(evidence), v.DecidedAt).Scan(&d.CandidateEdgeID)
		if err == nil {
			d.Outcome = DecideApplied
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("decide %s: %w", key, err)
		}
		var determination string
		err = tx.QueryRow(ctx, `SELECT determination_type FROM artifact_pair_verdicts
			WHERE tenant_id = $1 AND pair_key = $2`, tenantID, key).Scan(&determination)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("decide %s: pair is not queued", key)
		case err != nil:
			return fmt.Errorf("decide %s: %w", key, err)
		case determination == DeterminationHuman:
			d.Outcome = DecideHumanOwned
		default:
			d.Outcome = DecideStale
		}
		return nil
	})
	if err != nil {
		return Decision{}, err
	}
	return d, nil
}

func (s *PGVerdictStore) RecordDissent(ctx context.Context, tenantID, key string, v Verdict) error {
	b, err := json.Marshal(dissentRecord{Status: v.Status, Confidence: v.Confidence, DeterminedBy: adjudicatorName,
		DecidedAt: v.DecidedAt, Evidence: v.Evidence})
	if err != nil {
		return fmt.Errorf("record dissent %s: encode: %w", key, err)
	}
	return s.inTx(ctx, tenantID, func(tx db.Transaction) error {
		if err := tx.Exec(ctx, `UPDATE artifact_pair_verdicts SET dissent = $3::jsonb
			WHERE tenant_id = $1 AND pair_key = $2 AND determination_type = 'human'`, tenantID, key, string(b)); err != nil {
			return fmt.Errorf("record dissent %s: %w", key, err)
		}
		return nil
	})
}

const recordFailureSQL = `
UPDATE artifact_pair_verdicts
SET attempts = attempts + 1, last_error = $3, leased_until = NULL, next_attempt_at = $4,
    status = CASE WHEN $5::boolean THEN 'abandoned' ELSE status END,
    decided_at = CASE WHEN $5::boolean THEN $6::timestamptz ELSE decided_at END
WHERE tenant_id = $1 AND pair_key = $2 AND status = 'pending' AND determination_type <> 'human'
  AND leased_until = $7`

func (s *PGVerdictStore) RecordFailure(ctx context.Context, tenantID, key string, f Failure) error {
	return s.inTx(ctx, tenantID, func(tx db.Transaction) error {
		if err := tx.Exec(ctx, recordFailureSQL, tenantID, key, f.Err, f.NextAttemptAt, f.Abandon, f.At, f.LeasedUntil); err != nil {
			return fmt.Errorf("record failure %s: %w", key, err)
		}
		return nil
	})
}

func (s *PGVerdictStore) Unprojected(ctx context.Context, tenantID string, n int) ([]Projection, error) {
	if n <= 0 {
		return nil, fmt.Errorf("unprojected: n (%d) must be positive", n)
	}
	var out []Projection
	err := s.inTx(ctx, tenantID, func(tx db.Transaction) error {
		rows, err := tx.Query(ctx, `
			SELECT pair_key, low_group_id, high_group_id, candidate_edge_id, status, confidence, evidence, decided_at
			FROM artifact_pair_verdicts
			WHERE tenant_id = $1 AND status IN ('confirmed', 'rejected') AND NOT graph_applied
			  AND determination_type <> 'human'
			ORDER BY decided_at, pair_key
			LIMIT $2`, tenantID, n)
		if err != nil {
			return fmt.Errorf("unprojected: %w", err)
		}
		return scanRows(rows, func() error {
			var (
				p          Projection
				status     string
				confidence sql.NullFloat64
				raw        []byte
				decidedAt  sql.NullTime
			)
			if err := rows.Scan(&p.Key, &p.LowGroupID, &p.HighGroupID, &p.CandidateEdgeID, &status, &confidence, &raw, &decidedAt); err != nil {
				return fmt.Errorf("unprojected: %w", err)
			}
			if !confidence.Valid || !decidedAt.Valid || raw == nil {
				return fmt.Errorf("unprojected: %s is decided but lacks confidence, evidence or decided_at", p.Key)
			}
			if err := json.Unmarshal(raw, &p.Evidence); err != nil {
				return fmt.Errorf("unprojected: decode evidence of %s: %w", p.Key, err)
			}
			p.Status, p.Confidence, p.DecidedAt = VerdictStatus(status), confidence.Float64, decidedAt.Time
			out = append(out, p)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PGVerdictStore) MarkProjected(ctx context.Context, tenantID, key string) error {
	return s.inTx(ctx, tenantID, func(tx db.Transaction) error {
		if err := tx.Exec(ctx, `UPDATE artifact_pair_verdicts SET graph_applied = true
			WHERE tenant_id = $1 AND pair_key = $2 AND status IN ('confirmed', 'rejected')
			  AND determination_type <> 'human'`, tenantID, key); err != nil {
			return fmt.Errorf("mark projected %s: %w", key, err)
		}
		return nil
	})
}

func (s *PGVerdictStore) Counts(ctx context.Context, tenantID string, now time.Time) (QueueCounts, error) {
	var c QueueCounts
	err := s.inTx(ctx, tenantID, func(tx db.Transaction) error {
		if err := tx.QueryRow(ctx, `
			SELECT
			  count(*) FILTER (WHERE status = 'pending'),
			  count(*) FILTER (WHERE status = 'pending' AND next_attempt_at <= $2
			                   AND (leased_until IS NULL OR leased_until < $2)),
			  count(*) FILTER (WHERE status IN ('confirmed', 'rejected') AND NOT graph_applied)
			FROM artifact_pair_verdicts
			WHERE tenant_id = $1 AND determination_type <> 'human'`, tenantID, now).Scan(&c.Pending, &c.Due, &c.Unprojected); err != nil {
			return fmt.Errorf("counts: %w", err)
		}
		return nil
	})
	return c, err
}
