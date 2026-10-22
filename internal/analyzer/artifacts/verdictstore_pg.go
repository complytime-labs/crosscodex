package artifacts

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// enqueueChunk bounds the rows one Enqueue INSERT carries. A chunk binds nine
// parameters whatever its size (six UNNEST arrays and three scalars), so no
// bind-parameter limit applies and the value only bounds statement size. It
// is a constant rather than config because Enqueue runs inside reconcile,
// which does not depend on analysis.artifact_adjudication.
const enqueueChunk = 1000

// PGVerdictStore is the VerdictStore over the artifact_pair_verdicts table.
// Each method runs in one transaction on a
// db.TenantConnection, so row-level security confines it to the tenant in
// ctx. That tenant must equal the tenantID argument.
type PGVerdictStore struct {
	conn     db.TenantConnection
	tracer   trace.Tracer          // optional, nil-safe
	ops      metric.Int64Counter   // optional, nil-safe
	duration metric.Int64Histogram // optional, nil-safe
}

var _ VerdictStore = (*PGVerdictStore)(nil)

// PGVerdictStoreOption configures a PGVerdictStore.
type PGVerdictStoreOption func(*PGVerdictStore)

// WithVerdictStoreTelemetry enables tracing and metrics for the store.
// Either provider may be nil; nil providers are ignored.
func WithVerdictStoreTelemetry(tp trace.TracerProvider, mp metric.MeterProvider) PGVerdictStoreOption {
	return func(s *PGVerdictStore) {
		if tp != nil {
			s.tracer = tp.Tracer("crosscodex/internal/analyzer/artifacts")
		}
		if mp != nil {
			meter := mp.Meter("crosscodex")
			s.ops, _ = meter.Int64Counter("artifacts.verdictstore.operations.total",
				metric.WithDescription("Verdict store calls by operation and status (ok or error)"))
			s.duration, _ = meter.Int64Histogram("artifacts.verdictstore.operation.duration_ms",
				metric.WithDescription("Verdict store call duration in milliseconds by operation and status"), metric.WithUnit("ms"))
		}
	}
}

// NewPGVerdictStore returns a store over conn.
func NewPGVerdictStore(conn db.TenantConnection, opts ...PGVerdictStoreOption) *PGVerdictStore {
	s := &PGVerdictStore{conn: conn}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// observe runs one public method's body in a span named after op, then
// counts and times the call by operation and status.
func (s *PGVerdictStore) observe(ctx context.Context, op, tenantID string, fn func(ctx context.Context) error) error {
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "artifacts.verdictstore."+op)
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

// dissentRecord is the dissent column: a panel opinion on a human-owned pair,
// either given after a human decided it (RecordDissent) or the panel verdict
// a human overrode (DecideHuman). Evidence is raw JSON so DecideHuman can
// copy stored evidence it may no longer be able to decode. DecidedAt is
// stored at microsecond precision, so ReviewQueue's comparison with the
// decided_at column is exact.
type dissentRecord struct {
	Status       VerdictStatus   `json:"status"`
	Confidence   float64         `json:"confidence"`
	DeterminedBy string          `json:"determined_by"`
	DecidedAt    time.Time       `json:"decided_at"`
	Evidence     json.RawMessage `json:"evidence"`
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
	return s.observe(ctx, "Enqueue", tenantID, func(ctx context.Context) error {
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
					key := PairKey(c.LowGroupID, c.HighGroupID)
					b, err := json.Marshal(c.Context)
					if err != nil {
						return fmt.Errorf("enqueue: encode context of pair %s: %w", PairRef(key), err)
					}
					keys[i], lows[i], highs[i] = key, c.LowGroupID, c.HighGroupID
					edges[i], scores[i], contexts[i] = c.CandidateEdgeID, c.SimilarityScore, string(b)
				}
				if err := tx.Exec(ctx, enqueueSQL, tenantID, keys, lows, highs, edges, scores, contexts, reconcilerName, now); err != nil {
					return fmt.Errorf("enqueue pairs %d-%d: %w", start, start+len(chunk)-1, err)
				}
			}
			return nil
		})
	})
}

func (s *PGVerdictStore) ClosedPairs(ctx context.Context, tenantID string) (map[string]bool, error) {
	closed := map[string]bool{}
	err := s.observe(ctx, "ClosedPairs", tenantID, func(ctx context.Context) error {
		return s.inTx(ctx, tenantID, func(tx db.Transaction) error {
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
	var out []LeasedPair
	err := s.observe(ctx, "Lease", tenantID, func(ctx context.Context) error {
		if n <= 0 || leaseFor <= 0 {
			return fmt.Errorf("lease: n (%d) and leaseFor (%s) must be positive", n, leaseFor)
		}
		return s.inTx(ctx, tenantID, func(tx db.Transaction) error {
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
					return fmt.Errorf("lease: decode context of pair %s: %w", PairRef(p.Key), err)
				}
				out = append(out, p)
				return nil
			})
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
	var d Decision
	err := s.observe(ctx, "Decide", tenantID, func(ctx context.Context) error {
		switch v.Status {
		case VerdictConfirmed, VerdictRejected, VerdictUndecided:
		default:
			return fmt.Errorf("decide pair %s: status %q is not a panel verdict", PairRef(key), v.Status)
		}
		evidence, err := json.Marshal(v.Evidence)
		if err != nil {
			return fmt.Errorf("decide pair %s: encode evidence: %w", PairRef(key), err)
		}
		return s.inTx(ctx, tenantID, func(tx db.Transaction) error {
			err := tx.QueryRow(ctx, decideSQL, tenantID, key, string(v.Status), adjudicatorName, v.Confidence, string(evidence), v.DecidedAt).Scan(&d.CandidateEdgeID)
			if err == nil {
				d.Outcome = DecideApplied
				return nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("decide pair %s: %w", PairRef(key), err)
			}
			var determination string
			err = tx.QueryRow(ctx, `SELECT determination_type FROM artifact_pair_verdicts
			WHERE tenant_id = $1 AND pair_key = $2`, tenantID, key).Scan(&determination)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				return fmt.Errorf("decide pair %s: pair is not queued", PairRef(key))
			case err != nil:
				return fmt.Errorf("decide pair %s: %w", PairRef(key), err)
			case determination == DeterminationHuman:
				d.Outcome = DecideHumanOwned
			default:
				d.Outcome = DecideStale
			}
			return nil
		})
	})
	if err != nil {
		return Decision{}, err
	}
	return d, nil
}

func (s *PGVerdictStore) RecordDissent(ctx context.Context, tenantID, key string, v Verdict) error {
	return s.observe(ctx, "RecordDissent", tenantID, func(ctx context.Context) error {
		ev, err := json.Marshal(v.Evidence)
		if err != nil {
			return fmt.Errorf("record dissent pair %s: encode evidence: %w", PairRef(key), err)
		}
		b, err := json.Marshal(dissentRecord{Status: v.Status, Confidence: v.Confidence, DeterminedBy: adjudicatorName,
			DecidedAt: v.DecidedAt.Truncate(time.Microsecond), Evidence: ev})
		if err != nil {
			return fmt.Errorf("record dissent pair %s: encode: %w", PairRef(key), err)
		}
		return s.inTx(ctx, tenantID, func(tx db.Transaction) error {
			if err := tx.Exec(ctx, `UPDATE artifact_pair_verdicts SET dissent = $3::jsonb
			WHERE tenant_id = $1 AND pair_key = $2 AND determination_type = 'human'`, tenantID, key, string(b)); err != nil {
				return fmt.Errorf("record dissent pair %s: %w", PairRef(key), err)
			}
			return nil
		})
	})
}

// pairRefSQL computes PairRef in SQL: the first 16 hex characters of
// sha256(pair_key). pair_key is TEXT in a UTF-8 database, so convert_to
// yields the bytes Go hashes.
const pairRefSQL = `left(encode(sha256(convert_to(pair_key, 'UTF8')), 'hex'), 16)`

// disputedSQL is true for a human verdict a panel contradicted afterwards: a
// confirmed or rejected dissent, different from the human status, decided
// after the human verdict. The panel verdict a human overrode is dissent too,
// but it predates the human verdict, so it never makes a pair disputed.
const disputedSQL = `(determination_type = 'human' AND dissent->>'status' IN ('confirmed', 'rejected')
           AND dissent->>'status' <> status AND (dissent->>'decided_at')::timestamptz > decided_at)`

// pairByRefColumnsSQL is what DecideHuman and PairDetail read of a pair found
// by ref; pairRow.dest scans it. The last column is determined_by as shown:
// a hidden reviewer's pseudonym on a human verdict, determined_by otherwise.
// It is a scalar subquery so selectByRefSQL's FOR UPDATE still locks only
// the verdict row.
const pairByRefColumnsSQL = `pair_key, status, determination_type, determined_by, confidence, evidence, decided_at,
       projection_claimed_until, COALESCE(` + disputedSQL + `, false), similarity_score, dissent,
       COALESCE((SELECT r.pseudonym FROM reviewers r
                 WHERE r.tenant_id = artifact_pair_verdicts.tenant_id AND r.reviewer_id = artifact_pair_verdicts.determined_by
                   AND r.hidden_at IS NOT NULL AND artifact_pair_verdicts.determination_type = 'human'), determined_by)`

// selectByRefSQL locks the tenant's pairs with a ref. LIMIT 2 is enough to
// tell one match from several.
const selectByRefSQL = `
SELECT ` + pairByRefColumnsSQL + `
FROM artifact_pair_verdicts
WHERE tenant_id = $1 AND ` + pairRefSQL + ` = $2
LIMIT 2
FOR UPDATE`

// pairRow is one pairByRefColumnsSQL row.
type pairRow struct {
	key, status, determination, determinedBy string
	displayBy                                string // not part of state: hiding a reviewer keeps tokens valid
	confidence                               sql.NullFloat64
	evidence, dissent                        []byte
	decidedAt, claimedUntil                  sql.NullTime
	disputed                                 bool
	score                                    float64
}

// dest returns scan destinations in pairByRefColumnsSQL order.
func (c *pairRow) dest() []any {
	return []any{&c.key, &c.status, &c.determination, &c.determinedBy, &c.confidence, &c.evidence, &c.decidedAt,
		&c.claimedUntil, &c.disputed, &c.score, &c.dissent, &c.displayBy}
}

// state is the part of c an expect token covers.
func (c *pairRow) state() pairState {
	p := pairState{Status: c.status, DeterminationType: c.determination, DeterminedBy: c.determinedBy,
		SimilarityScore: c.score, Evidence: c.evidence, Dissent: c.dissent}
	if c.confidence.Valid {
		v := c.confidence.Float64
		p.Confidence = &v
	}
	if c.decidedAt.Valid {
		p.DecidedAt = c.decidedAt.Time
	}
	return p
}

// decideHumanSQL makes a pair human-owned. $5 is the dissent to store, or
// NULL to keep the stored one. The projection claim is kept: its holder is
// writing the previous verdict, and the new one must reach the graph after
// those writes.
const decideHumanSQL = `
UPDATE artifact_pair_verdicts
SET status = $3, determination_type = 'human', determined_by = $4, confidence = 1, evidence = NULL,
    dissent = COALESCE($5::jsonb, dissent), decided_at = $6, graph_applied = false,
    leased_until = NULL, last_error = NULL
WHERE tenant_id = $1 AND pair_key = $2`

func (s *PGVerdictStore) DecideHuman(ctx context.Context, tenantID, ref string, h HumanVerdict) (HumanDecision, error) {
	var d HumanDecision
	err := s.observe(ctx, "DecideHuman", tenantID, func(ctx context.Context) error {
		if err := validateHumanVerdict(ref, h); err != nil {
			return err
		}
		return s.inTx(ctx, tenantID, func(tx db.Transaction) error {
			// Locks the reviewer row FOR SHARE until commit, so the reviewer
			// cannot be deleted while this verdict is recorded.
			if err := tx.Exec(ctx, "SELECT public.require_active_reviewer($1)", h.Reviewer); err != nil {
				switch {
				case db.IsPgErrorCode(err, "P0002"):
					return fmt.Errorf("reviewer %q is not registered for tenant %q; nothing was recorded. Register them with 'crosscodexd admin reviewer add --tenant %s --reviewer %s --by <operator> --reason <text>': %w",
						h.Reviewer, tenantID, tenantID, h.Reviewer, ErrReviewerNotRegistered)
				case db.IsPgErrorCode(err, "55000"):
					// The ID is left out: the message reaches span status, and a
					// deleted reviewer may be hidden.
					return fmt.Errorf("the reviewer passed with --reviewer was deleted; nothing was recorded. Restore them with 'crosscodexd admin reviewer restore --tenant %s --reviewer <reviewer> --by <operator> --reason <text>' (run 'crosscodexd admin reviewer unhide --tenant %s --reviewer <reviewer> --by <operator> --reason <text>' first if they are hidden) or decide as another reviewer: %w",
						tenantID, tenantID, ErrReviewerDeleted)
				}
				return fmt.Errorf("decide human %s: check reviewer: %w", ref, err)
			}
			var found []pairRow
			rows, err := tx.Query(ctx, selectByRefSQL, tenantID, ref)
			if err != nil {
				return fmt.Errorf("decide human %s: %w", ref, err)
			}
			// scanRows closes rows before the UPDATE below runs on tx.
			if err := scanRows(rows, func() error {
				var c pairRow
				if err := rows.Scan(c.dest()...); err != nil {
					return fmt.Errorf("decide human %s: %w", ref, err)
				}
				found = append(found, c)
				return nil
			}); err != nil {
				return err
			}
			switch len(found) {
			case 0:
				return fmt.Errorf("decide human %s: %w", ref, ErrPairNotFound)
			case 1:
			default:
				return fmt.Errorf("decide human %s: %w", ref, ErrPairRefAmbiguous)
			}
			c := found[0]
			// The reviewer decided from the state their token was taken from.
			// A pair that already holds this undisputed human verdict needs no
			// write, so it falls through to HumanUnchanged below whatever the
			// token says.
			unchanged := c.determination == DeterminationHuman && VerdictStatus(c.status) == h.Status && !c.disputed
			if !unchanged && c.state().token() != h.Expect {
				changed := &PairChangedError{Ref: ref, Status: VerdictStatus(c.status), DeterminationType: c.determination,
					DeterminedBy: c.determinedBy, DisplayBy: c.displayBy}
				if c.decidedAt.Valid {
					changed.DecidedAt = c.decidedAt.Time
				}
				return changed
			}
			d.Key = c.key
			if c.claimedUntil.Valid && c.claimedUntil.Time.After(h.DecidedAt) {
				d.ClaimedUntil = c.claimedUntil.Time
			}
			d.Outcome = HumanApplied
			if c.determination == DeterminationHuman && VerdictStatus(c.status) == h.Status {
				if !c.disputed {
					d.Outcome = HumanUnchanged
					return nil
				}
				// The reviewer stands by the verdict a panel contradicted
				// afterwards. Recording it again at h.DecidedAt makes the
				// stored dissent older than the human verdict, so the pair
				// leaves the review queue.
				d.Outcome = HumanReaffirmed
			}
			var dissent any // nil keeps the stored dissent
			if c.determination == DeterminationLLMPanel {
				// Evidence is copied, not decoded, so evidence written in an
				// older shape never blocks a human verdict.
				ev := json.RawMessage(c.evidence)
				if c.evidence == nil {
					if ev, err = json.Marshal(PanelEvidence{}); err != nil {
						return fmt.Errorf("decide human %s: encode empty evidence: %w", ref, err)
					}
				}
				b, err := json.Marshal(dissentRecord{Status: VerdictStatus(c.status), Confidence: c.confidence.Float64,
					DeterminedBy: c.determinedBy, DecidedAt: c.decidedAt.Time, Evidence: ev})
				if err != nil {
					return fmt.Errorf("decide human %s: encode dissent: %w", ref, err)
				}
				dissent = string(b)
			}
			if err := tx.Exec(ctx, decideHumanSQL, tenantID, c.key, string(h.Status), h.Reviewer, dissent, h.DecidedAt); err != nil {
				return fmt.Errorf("decide human %s: %w", ref, err)
			}
			return nil
		})
	})
	if err != nil {
		return HumanDecision{}, err
	}
	return d, nil
}

// pairDetailSQL reads the tenant's pairs with a ref, as selectByRefSQL does,
// without locking them.
const pairDetailSQL = `
SELECT ` + pairByRefColumnsSQL + `, context, low_group_id, high_group_id, COALESCE(last_error, '')
FROM artifact_pair_verdicts
WHERE tenant_id = $1 AND ` + pairRefSQL + ` = $2
LIMIT 2`

// relatedPairsSQL lists up to $5 pairs other than $2 that share group $3 or
// $4, with how many there are: the window count is taken before LIMIT.
const relatedPairsSQL = `
SELECT ` + pairRefSQL + `, status, determination_type,
       COALESCE(context->>'low_name', ''), COALESCE(context->>'high_name', ''), count(*) OVER ()
FROM artifact_pair_verdicts
WHERE tenant_id = $1 AND pair_key <> $2
  AND (low_group_id IN ($3, $4) OR high_group_id IN ($3, $4))
ORDER BY decided_at DESC NULLS LAST, pair_key COLLATE "C"
LIMIT $5`

func (s *PGVerdictStore) PairDetail(ctx context.Context, tenantID, ref string, relatedLimit int) (PairDetail, error) {
	var d PairDetail
	err := s.observe(ctx, "PairDetail", tenantID, func(ctx context.Context) error {
		if err := ValidatePairRef(ref); err != nil {
			return err
		}
		if relatedLimit <= 0 {
			return fmt.Errorf("pair detail %s: relatedLimit (%d) must be positive", ref, relatedLimit)
		}
		return s.inTx(ctx, tenantID, func(tx db.Transaction) error {
			type detailRow struct {
				pairRow
				context              []byte
				low, high, lastError string
			}
			var found []detailRow
			rows, err := tx.Query(ctx, pairDetailSQL, tenantID, ref)
			if err != nil {
				return fmt.Errorf("pair detail %s: %w", ref, err)
			}
			if err := scanRows(rows, func() error {
				var f detailRow
				if err := rows.Scan(append(f.dest(), &f.context, &f.low, &f.high, &f.lastError)...); err != nil {
					return fmt.Errorf("pair detail %s: %w", ref, err)
				}
				found = append(found, f)
				return nil
			}); err != nil {
				return err
			}
			switch len(found) {
			case 0:
				return fmt.Errorf("pair detail %s: %w", ref, ErrPairNotFound)
			case 1:
			default:
				return fmt.Errorf("pair detail %s: %w", ref, ErrPairRefAmbiguous)
			}
			f := found[0]
			d = PairDetail{
				ReviewItem: ReviewItem{Ref: ref, Key: f.key, Status: VerdictStatus(f.status),
					DeterminationType: f.determination, SimilarityScore: f.score},
				DeterminedBy: f.determinedBy, DisplayBy: f.displayBy, LastError: f.lastError, Disputed: f.disputed, Expect: f.state().token(),
			}
			if err := json.Unmarshal(f.context, &d.Context); err != nil {
				return fmt.Errorf("pair detail %s: decode context: %w", ref, err)
			}
			if f.confidence.Valid {
				v := f.confidence.Float64
				d.Confidence = &v
			}
			if f.decidedAt.Valid {
				d.DecidedAt = f.decidedAt.Time
			}
			// Judged as scanReviewItems judges it, so the listing and the
			// detail agree on which pairs are unreadable.
			if f.determination == DeterminationLLMPanel {
				var ev PanelEvidence
				err := errors.New("stored evidence is missing")
				if f.evidence != nil {
					err = json.Unmarshal(f.evidence, &ev)
				}
				if err != nil {
					d.EvidenceErr, d.VotesUnreadable = fmt.Errorf("decode evidence: %w", err), true
				} else {
					split := ev.Split()
					d.Evidence, d.Votes = &ev, &split
				}
			}
			if f.dissent != nil {
				d.Dissent = decodeDissent(f.dissent)
				d.DissentStatus = d.Dissent.Status
			}
			d.Related, d.RelatedTotal, err = relatedPairs(ctx, tx, tenantID, ref, f.key, f.low, f.high, relatedLimit)
			return err
		})
	})
	if err != nil {
		return PairDetail{}, err
	}
	return d, nil
}

// decodeDissent decodes a stored dissent column. A record or evidence that
// cannot be decoded is reported in EvidenceErr rather than as an error, so
// PairDetail still returns the expect token; a record that cannot be decoded
// keeps its status when the status alone can be read.
func decodeDissent(raw []byte) *PanelDissent {
	var rec dissentRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		var partial struct {
			Status VerdictStatus `json:"status"`
		}
		if json.Unmarshal(raw, &partial) != nil {
			partial.Status = ""
		}
		return &PanelDissent{Status: partial.Status, EvidenceErr: fmt.Errorf("decode dissent: %w", err)}
	}
	pd := &PanelDissent{Status: rec.Status, Confidence: rec.Confidence, DeterminedBy: rec.DeterminedBy, DecidedAt: rec.DecidedAt}
	var ev PanelEvidence
	if err := json.Unmarshal(rec.Evidence, &ev); err != nil {
		pd.EvidenceErr = fmt.Errorf("decode dissent evidence: %w", err)
	} else {
		pd.Evidence = &ev
	}
	return pd
}

// relatedPairs runs relatedPairsSQL for the pair with key, whose groups are
// low and high, and returns up to limit related pairs with how many there are.
// ref prefixes errors.
func relatedPairs(ctx context.Context, tx db.Transaction, tenantID, ref, key, low, high string, limit int) ([]RelatedPair, int, error) {
	var (
		out   []RelatedPair
		total int
	)
	rows, err := tx.Query(ctx, relatedPairsSQL, tenantID, key, low, high, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("pair detail %s: related pairs: %w", ref, err)
	}
	err = scanRows(rows, func() error {
		var (
			r      RelatedPair
			status string
		)
		if err := rows.Scan(&r.Ref, &status, &r.DeterminationType, &r.LowName, &r.HighName, &total); err != nil {
			return fmt.Errorf("pair detail %s: related pairs: %w", ref, err)
		}
		r.Status = VerdictStatus(status)
		out = append(out, r)
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

const reviewQueueSQL = `
SELECT ` + reviewItemColumnsSQL + `
FROM artifact_pair_verdicts
WHERE tenant_id = $1
  AND (status IN ('undecided', 'abandoned') OR ` + disputedSQL + `)
ORDER BY created_at, pair_key
LIMIT $2`

// reviewItemColumnsSQL selects the columns scanReviewItems reads. Evidence is
// selected only for llm_panel rows, the only ones whose current verdict has
// votes. The split is computed in Go rather than SQL so this path and
// PairDetail judge "unreadable" the same way.
const reviewItemColumnsSQL = pairRefSQL + `, pair_key, status, determination_type, similarity_score, context,
       COALESCE(dissent->>'status', ''), CASE WHEN determination_type = 'llm_panel' THEN evidence END`

// scanReviewItems reads ReviewQueue's and SearchPairs' rows; op prefixes its
// errors. Evidence that cannot be decoded marks the item VotesUnreadable
// rather than failing the listing, so one bad row cannot hide the queue;
// PairDetail reports the decode error.
func scanReviewItems(ctx context.Context, rows db.Rows, tenantID, op string) ([]ReviewItem, error) {
	var out []ReviewItem
	err := scanRows(rows, func() error {
		var (
			it                    ReviewItem
			status, dissentStatus string
			raw, evidence         []byte
		)
		if err := rows.Scan(&it.Ref, &it.Key, &status, &it.DeterminationType, &it.SimilarityScore, &raw, &dissentStatus, &evidence); err != nil {
			return fmt.Errorf("%s: %w", op, err)
		}
		if err := json.Unmarshal(raw, &it.Context); err != nil {
			return fmt.Errorf("%s: decode context of pair %s: %w", op, PairRef(it.Key), err)
		}
		it.Status, it.DissentStatus = VerdictStatus(status), VerdictStatus(dissentStatus)
		var evErr error
		switch {
		case evidence != nil:
			var ev PanelEvidence
			if evErr = json.Unmarshal(evidence, &ev); evErr == nil {
				s := ev.Split()
				it.Votes = &s
			}
		case it.DeterminationType == DeterminationLLMPanel:
			evErr = errors.New("stored evidence is missing")
		}
		if evErr != nil {
			it.VotesUnreadable = true
			slog.WarnContext(ctx, "artifact review item: stored evidence cannot be decoded",
				"tenant.id", tenantID, "op", op, "pair.ref", PairRef(it.Key), "error", evErr)
		}
		out = append(out, it)
		return nil
	})
	return out, err
}

func (s *PGVerdictStore) ReviewQueue(ctx context.Context, tenantID string, n int) ([]ReviewItem, error) {
	var out []ReviewItem
	err := s.observe(ctx, "ReviewQueue", tenantID, func(ctx context.Context) error {
		if n <= 0 {
			return fmt.Errorf("review queue: n (%d) must be positive", n)
		}
		return s.inTx(ctx, tenantID, func(tx db.Transaction) error {
			rows, err := tx.Query(ctx, reviewQueueSQL, tenantID, n)
			if err != nil {
				return fmt.Errorf("review queue: %w", err)
			}
			out, err = scanReviewItems(ctx, rows, tenantID, "review queue")
			return err
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// escapeLike backslash-escapes \, % and _ so s matches only itself inside an
// ILIKE pattern with ESCAPE '\'. The search text comes from an operator, and
// an unescaped % or _ would widen the match.
var escapeLike = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace

// searchPairsSQL matches $2, an escaped '%text%' pattern, against both names.
// ILIKE folds case by the database's locale; the ESCAPE clause is explicit so
// matching never depends on the default escape character.
const searchPairsSQL = `
SELECT ` + reviewItemColumnsSQL + `
FROM artifact_pair_verdicts
WHERE tenant_id = $1
  AND ((context->>'low_name') ILIKE $2 ESCAPE '\' OR (context->>'high_name') ILIKE $2 ESCAPE '\')
ORDER BY created_at, pair_key
LIMIT $3`

func (s *PGVerdictStore) SearchPairs(ctx context.Context, tenantID, name string, n int) ([]ReviewItem, error) {
	var out []ReviewItem
	err := s.observe(ctx, "SearchPairs", tenantID, func(ctx context.Context) error {
		if n <= 0 {
			return fmt.Errorf("search pairs: n (%d) must be positive", n)
		}
		if err := ValidateSearchName(name); err != nil {
			return err
		}
		return s.inTx(ctx, tenantID, func(tx db.Transaction) error {
			rows, err := tx.Query(ctx, searchPairsSQL, tenantID, "%"+escapeLike(name)+"%", n)
			if err != nil {
				return fmt.Errorf("search pairs: %w", err)
			}
			out, err = scanReviewItems(ctx, rows, tenantID, "search pairs")
			return err
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

const recordFailureSQL = `
UPDATE artifact_pair_verdicts
SET attempts = attempts + 1, last_error = $3, leased_until = NULL, next_attempt_at = $4,
    status = CASE WHEN $5::boolean THEN 'abandoned' ELSE status END,
    decided_at = CASE WHEN $5::boolean THEN $6::timestamptz ELSE decided_at END
WHERE tenant_id = $1 AND pair_key = $2 AND status = 'pending' AND determination_type <> 'human'
  AND leased_until = $7`

func (s *PGVerdictStore) RecordFailure(ctx context.Context, tenantID, key string, f Failure) error {
	return s.observe(ctx, "RecordFailure", tenantID, func(ctx context.Context) error {
		return s.inTx(ctx, tenantID, func(tx db.Transaction) error {
			if err := tx.Exec(ctx, recordFailureSQL, tenantID, key, f.Err, f.NextAttemptAt, f.Abandon, f.At, f.LeasedUntil); err != nil {
				return fmt.Errorf("record failure pair %s: %w", PairRef(key), err)
			}
			return nil
		})
	})
}

func (s *PGVerdictStore) Unprojected(ctx context.Context, tenantID string, n int, now time.Time) ([]Projection, error) {
	var out []Projection
	err := s.observe(ctx, "Unprojected", tenantID, func(ctx context.Context) error {
		if n <= 0 {
			return fmt.Errorf("unprojected: n (%d) must be positive", n)
		}
		return s.inTx(ctx, tenantID, func(tx db.Transaction) error {
			rows, err := tx.Query(ctx, `
			SELECT pair_key, low_group_id, high_group_id, candidate_edge_id, status, confidence, evidence, decided_at,
			       determination_type, determined_by
			FROM artifact_pair_verdicts
			WHERE tenant_id = $1 AND status IN ('confirmed', 'rejected') AND NOT graph_applied
			  AND (projection_claimed_until IS NULL OR projection_claimed_until <= $3)
			ORDER BY decided_at, pair_key
			LIMIT $2`, tenantID, n, now)
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
				if err := rows.Scan(&p.Key, &p.LowGroupID, &p.HighGroupID, &p.CandidateEdgeID, &status, &confidence, &raw, &decidedAt,
					&p.DeterminationType, &p.DeterminedBy); err != nil {
					return fmt.Errorf("unprojected: %w", err)
				}
				panel := p.DeterminationType == DeterminationLLMPanel
				if !confidence.Valid || !decidedAt.Valid || (panel && raw == nil) {
					return fmt.Errorf("unprojected: pair %s is decided but lacks confidence, evidence or decided_at", PairRef(p.Key))
				}
				if panel {
					if err := json.Unmarshal(raw, &p.Evidence); err != nil {
						return fmt.Errorf("unprojected: decode evidence of pair %s: %w", PairRef(p.Key), err)
					}
				}
				p.Status, p.Confidence, p.DecidedAt = VerdictStatus(status), confidence.Float64, decidedAt.Time
				out = append(out, p)
				return nil
			})
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// claimProjectionSQL claims a row for projecting the verdict $3, $4, $5
// until $6, if it still holds that verdict, is not projected and has no live
// claim at $7. Both sides of the decided_at comparison are microseconds,
// since pgx truncates $5.
const claimProjectionSQL = `
UPDATE artifact_pair_verdicts
SET projection_claimed_until = $6
WHERE tenant_id = $1 AND pair_key = $2 AND determination_type = $3 AND status = $4 AND decided_at = $5
  AND NOT graph_applied AND (projection_claimed_until IS NULL OR projection_claimed_until <= $7)
RETURNING 1`

// claimHoldsSQL tells ClaimBusy from ClaimReplaced when claimProjectionSQL
// matched nothing: whether the row still holds the verdict.
const claimHoldsSQL = `
SELECT determination_type = $3 AND status = $4 AND decided_at IS NOT DISTINCT FROM $5
FROM artifact_pair_verdicts WHERE tenant_id = $1 AND pair_key = $2`

func (s *PGVerdictStore) ClaimProjection(ctx context.Context, tenantID string, p Projection, until, now time.Time) (ClaimOutcome, error) {
	var out ClaimOutcome
	err := s.observe(ctx, "ClaimProjection", tenantID, func(ctx context.Context) error {
		if !until.After(now) {
			return fmt.Errorf("claim projection pair %s: until (%s) must be after now (%s)", PairRef(p.Key), until, now)
		}
		return s.inTx(ctx, tenantID, func(tx db.Transaction) error {
			var one int
			err := tx.QueryRow(ctx, claimProjectionSQL, tenantID, p.Key, p.DeterminationType, string(p.Status), p.DecidedAt, until, now).Scan(&one)
			if err == nil {
				out = ClaimAcquired
				return nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("claim projection pair %s: %w", PairRef(p.Key), err)
			}
			var holds bool
			err = tx.QueryRow(ctx, claimHoldsSQL, tenantID, p.Key, p.DeterminationType, string(p.Status), p.DecidedAt).Scan(&holds)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				return fmt.Errorf("claim projection pair %s: pair is not queued", PairRef(p.Key))
			case err != nil:
				return fmt.Errorf("claim projection pair %s: %w", PairRef(p.Key), err)
			case holds:
				out = ClaimBusy
			default:
				out = ClaimReplaced
			}
			return nil
		})
	})
	if err != nil {
		return 0, err
	}
	return out, nil
}

// markProjectedSQL releases the caller's claim ($6) and is the projection
// fence: graph_applied becomes true only if the row still holds the verdict
// identified by $3, $4 and $5. A row under another claim, or none, is not
// touched. Both sides of each timestamp comparison are microseconds, since
// pgx truncates $5 and $6.
const markProjectedSQL = `
UPDATE artifact_pair_verdicts
SET graph_applied = (determination_type = $3 AND status = $4 AND decided_at = $5), projection_claimed_until = NULL
WHERE tenant_id = $1 AND pair_key = $2 AND projection_claimed_until = $6
RETURNING graph_applied`

func (s *PGVerdictStore) MarkProjected(ctx context.Context, tenantID string, p Projection, claimedUntil time.Time) (bool, error) {
	var applied bool
	err := s.observe(ctx, "MarkProjected", tenantID, func(ctx context.Context) error {
		return s.inTx(ctx, tenantID, func(tx db.Transaction) error {
			err := tx.QueryRow(ctx, markProjectedSQL, tenantID, p.Key, p.DeterminationType, string(p.Status), p.DecidedAt, claimedUntil).Scan(&applied)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("mark projected pair %s: %w", PairRef(p.Key), err)
			}
			return nil
		})
	})
	if err != nil {
		return false, err
	}
	return applied, nil
}

func (s *PGVerdictStore) ReleaseClaim(ctx context.Context, tenantID, key string, claimedUntil time.Time) error {
	return s.observe(ctx, "ReleaseClaim", tenantID, func(ctx context.Context) error {
		return s.inTx(ctx, tenantID, func(tx db.Transaction) error {
			if err := tx.Exec(ctx, `UPDATE artifact_pair_verdicts SET projection_claimed_until = NULL
			WHERE tenant_id = $1 AND pair_key = $2 AND projection_claimed_until = $3`, tenantID, key, claimedUntil); err != nil {
				return fmt.Errorf("release projection claim pair %s: %w", PairRef(key), err)
			}
			return nil
		})
	})
}

// resetProjectionSQL re-queues every projected verdict, llm_panel and human
// alike. The determination types are listed explicitly so a future type is
// not reset by accident.
const resetProjectionSQL = `
WITH reset AS (
    UPDATE artifact_pair_verdicts SET graph_applied = false
    WHERE tenant_id = $1 AND determination_type IN ('llm_panel', 'human') AND status IN ('confirmed', 'rejected')
      AND graph_applied
    RETURNING 1
)
SELECT count(*) FROM reset`

func (s *PGVerdictStore) ResetProjection(ctx context.Context, tenantID string) (int, error) {
	var n int
	err := s.observe(ctx, "ResetProjection", tenantID, func(ctx context.Context) error {
		return s.inTx(ctx, tenantID, func(tx db.Transaction) error {
			if err := tx.QueryRow(ctx, resetProjectionSQL, tenantID).Scan(&n); err != nil {
				return fmt.Errorf("reset projection: %w", err)
			}
			return nil
		})
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

func (s *PGVerdictStore) Counts(ctx context.Context, tenantID string, now time.Time) (QueueCounts, error) {
	var c QueueCounts
	err := s.observe(ctx, "Counts", tenantID, func(ctx context.Context) error {
		return s.inTx(ctx, tenantID, func(tx db.Transaction) error {
			if err := tx.QueryRow(ctx, `
			SELECT
			  count(*) FILTER (WHERE status = 'pending' AND determination_type <> 'human'),
			  count(*) FILTER (WHERE status = 'pending' AND determination_type <> 'human' AND next_attempt_at <= $2
			                   AND (leased_until IS NULL OR leased_until < $2)),
			  count(*) FILTER (WHERE status IN ('confirmed', 'rejected') AND NOT graph_applied)
			FROM artifact_pair_verdicts
			WHERE tenant_id = $1`, tenantID, now).Scan(&c.Pending, &c.Due, &c.Unprojected); err != nil {
				return fmt.Errorf("counts: %w", err)
			}
			return nil
		})
	})
	return c, err
}
