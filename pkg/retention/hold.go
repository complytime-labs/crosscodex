package retention

import (
	"context"
	"fmt"
	"time"

	"github.com/complytime-labs/crosscodex/pkg/db"
)

// HoldScope defines the matching criteria for a legal or compliance hold.
// Empty fields act as wildcards that match any candidate value.
type HoldScope struct {
	// CreatedBefore restricts the hold to candidates created before this time.
	// Nil matches candidates of any age.
	CreatedBefore *time.Time
	// TenantID restricts the hold to candidates belonging to this tenant.
	// An empty string matches candidates from any tenant.
	TenantID string
	// JobID restricts the hold to candidates whose ID equals this job ID.
	// An empty string matches candidates with any ID.
	JobID string
}

// Hold represents a legal or compliance hold that prevents retention purge
// of matching data candidates. A hold is active while ReleasedAt is nil.
type Hold struct {
	ID         string
	Name       string
	Scope      HoldScope
	CreatedAt  time.Time
	CreatedBy  string
	ExpiresAt  *time.Time
	ReleasedAt *time.Time
	ReleasedBy string
}

// ActiveAt reports whether the hold is active at the given time.
// A hold is active when it has not been released and has not expired.
func (h Hold) ActiveAt(now time.Time) bool {
	if h.ReleasedAt != nil {
		return false
	}
	return h.ExpiresAt == nil || h.ExpiresAt.After(now)
}

// Covers reports whether this hold applies to the given candidate.
// Every non-empty/non-nil scope field must match; unset fields are wildcards.
// Matching semantics:
//   - CreatedBefore set: candidate matches only if c.CreatedAt.Before(*CreatedBefore)
//   - TenantID set: must equal c.TenantID
//   - JobID set: must equal c.ID (a job-results candidate's ID is its job id)
func (h Hold) Covers(c Candidate) bool {
	s := h.Scope
	if s.CreatedBefore != nil && !c.CreatedAt.Before(*s.CreatedBefore) {
		return false
	}
	if s.TenantID != "" && c.TenantID != s.TenantID {
		return false
	}
	if s.JobID != "" && c.ID != s.JobID {
		return false
	}
	return true
}

// HoldStore persists and retrieves retention holds.
type HoldStore interface {
	Create(ctx context.Context, h Hold) (Hold, error)
	Release(ctx context.Context, name, releasedBy string) error
	List(ctx context.Context) ([]Hold, error)
	Active(ctx context.Context, now time.Time) ([]Hold, error)
}

// HoldChecker evaluates whether any hold covers a given candidate.
type HoldChecker struct {
	holds []Hold
}

// NewHoldChecker returns a HoldChecker preloaded with the given holds.
func NewHoldChecker(active []Hold) HoldChecker {
	return HoldChecker{holds: active}
}

// Held reports whether any hold in the checker covers the candidate.
func (hc HoldChecker) Held(c Candidate) bool {
	for _, h := range hc.holds {
		if h.Covers(c) {
			return true
		}
	}
	return false
}

// pgHoldStore is a Postgres-backed HoldStore.
type pgHoldStore struct {
	conn db.Connection
}

// NewPgHoldStore returns a HoldStore backed by the given db.Connection.
func NewPgHoldStore(conn db.Connection) HoldStore {
	return &pgHoldStore{conn: conn}
}

// holdCols is the ordered column list used in SELECT and RETURNING clauses.
const holdCols = `id, name, tenant_id, created_before, job_id,
    created_at, created_by, expires_at, released_at, released_by`

// scanHold reads a single Hold from any value that supports Scan.
// Accepts db.Row (from QueryRow) and db.Rows (from Query) since both expose Scan.
func scanHold(s interface{ Scan(...any) error }) (Hold, error) {
	var h Hold
	var tenantID *string
	var createdBefore *time.Time
	var jobID *string
	var expiresAt *time.Time
	var releasedAt *time.Time
	var releasedBy *string

	err := s.Scan(
		&h.ID,
		&h.Name,
		&tenantID,
		&createdBefore,
		&jobID,
		&h.CreatedAt,
		&h.CreatedBy,
		&expiresAt,
		&releasedAt,
		&releasedBy,
	)
	if err != nil {
		return Hold{}, err
	}
	if tenantID != nil {
		h.Scope.TenantID = *tenantID
	}
	h.Scope.CreatedBefore = createdBefore
	if jobID != nil {
		h.Scope.JobID = *jobID
	}
	h.ExpiresAt = expiresAt
	h.ReleasedAt = releasedAt
	if releasedBy != nil {
		h.ReleasedBy = *releasedBy
	}
	return h, nil
}

// scanHolds drains a Rows result into a slice of Hold values.
func scanHolds(rows db.Rows) ([]Hold, error) {
	defer rows.Close() //nolint:errcheck
	var holds []Hold
	for rows.Next() {
		h, err := scanHold(rows)
		if err != nil {
			return nil, err
		}
		holds = append(holds, h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return holds, nil
}

// Create inserts a new hold and returns it with the database-assigned ID.
// The insert runs inside a tenant-scoped transaction so RLS applies.
func (s *pgHoldStore) Create(ctx context.Context, h Hold) (Hold, error) {
	var tenantID *string
	if h.Scope.TenantID != "" {
		tenantID = &h.Scope.TenantID
	}
	var jobID *string
	if h.Scope.JobID != "" {
		jobID = &h.Scope.JobID
	}

	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return Hold{}, fmt.Errorf("hold store: begin create: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	row := tx.QueryRow(ctx, `
		INSERT INTO public.retention_holds
			(name, tenant_id, created_before, job_id, created_by, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING `+holdCols,
		h.Name,
		tenantID,
		h.Scope.CreatedBefore,
		jobID,
		h.CreatedBy,
		h.ExpiresAt,
	)
	created, err := scanHold(row)
	if err != nil {
		return Hold{}, fmt.Errorf("hold store: create: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Hold{}, fmt.Errorf("hold store: commit create: %w", err)
	}
	return created, nil
}

// Release marks the named active hold as released. It is idempotent when the
// hold is already released (the WHERE released_at IS NULL predicate excludes it).
// The update runs inside a tenant-scoped transaction so RLS applies.
func (s *pgHoldStore) Release(ctx context.Context, name, releasedBy string) error {
	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("hold store: begin release: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := tx.Exec(ctx, `
		UPDATE public.retention_holds
		SET released_at = now(), released_by = $2
		WHERE name = $1 AND released_at IS NULL`,
		name, releasedBy,
	); err != nil {
		return fmt.Errorf("hold store: release: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("hold store: commit release: %w", err)
	}
	return nil
}

// List returns all holds (active and released) ordered by creation time descending.
// The query runs inside a tenant-scoped transaction so RLS applies.
func (s *pgHoldStore) List(ctx context.Context) ([]Hold, error) {
	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("hold store: begin list: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.Query(ctx, `
		SELECT `+holdCols+`
		FROM public.retention_holds
		ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("hold store: list: %w", err)
	}
	holds, err := scanHolds(rows)
	if err != nil {
		return nil, fmt.Errorf("hold store: scan list: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("hold store: commit list: %w", err)
	}
	return holds, nil
}

// Active returns holds that are neither released nor expired as of now.
// The query runs inside a tenant-scoped transaction so RLS applies.
func (s *pgHoldStore) Active(ctx context.Context, now time.Time) ([]Hold, error) {
	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("hold store: begin active: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.Query(ctx, `
		SELECT `+holdCols+`
		FROM public.retention_holds
		WHERE released_at IS NULL
		  AND (expires_at IS NULL OR expires_at > $1)
		ORDER BY created_at DESC`,
		now,
	)
	if err != nil {
		return nil, fmt.Errorf("hold store: active: %w", err)
	}
	holds, err := scanHolds(rows)
	if err != nil {
		return nil, fmt.Errorf("hold store: scan active: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("hold store: commit active: %w", err)
	}
	return holds, nil
}
