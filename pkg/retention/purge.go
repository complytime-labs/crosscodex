package retention

import (
	"context"
	"fmt"

	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/storage"
)

// Purger deletes expired data after the Archiver has verified a copy.
type Purger interface {
	Purge(ctx context.Context, c Candidate) error
}

// dbAggregate describes a DB entity that is archived and purged as a parent row
// plus its foreign-key children, all keyed by the parent id (Candidate.ID, bound
// as $1). It is the single source of truth shared by the archive path (whose SQL
// twin lives in dbAggregateQueries) and the purge path below, so the archived set
// and the deleted set can never drift. Every table and column name is a
// compile-time constant; the candidate id is ONLY ever a bound $1.
type dbAggregate struct {
	parentTable string // e.g. "jobs"
	parentPK    string // e.g. "job_id"
	// children are deleted in this order (all before the parent); each is deleted
	// WHERE <fkCol> = $1. For the job aggregate the set is the parent's FK children
	// plus requires_candidates, which has no FK to jobs but is job-scoped by its
	// job_id column and so is purged with the job.
	children []struct{ table, fkCol string }
}

// dbAggregates lists the DB classes purged as a parent row with its FK children.
// Every StorePostgres class supported for deletion lives here; a class absent
// from the map is an error.
//
// The catalog aggregate deletes embeddings as a catalog dependent even though
// the archive doc (dbAggregateQueries in archive.go) deliberately omits it:
// embeddings are derived/regenerable, so they are purge-only. This archive⊆purge
// asymmetry is intentional — do not add embeddings to the archive to "match".
var dbAggregates = map[DataClass]dbAggregate{
	ClassJobResults: {
		parentTable: "jobs", parentPK: "job_id",
		children: []struct{ table, fkCol string }{
			{"job_stages", "job_id"},
			{"vote_summaries", "job_id"},
			{"analysis_results", "job_id"},
			{"relationship_candidates", "job_id"},
			// requires_candidates has no FK to jobs; it is associated by the job_id
			// column only, but is job-scoped data purged with the job.
			{"requires_candidates", "job_id"},
		},
	},
	ClassCatalogs: {
		parentTable: "catalogs", parentPK: "catalog_id",
		children: []struct{ table, fkCol string }{
			{"classifications", "catalog_id"},
			{"embeddings", "catalog_id"},
			{"controls", "catalog_id"},
		},
	},
}

type purger struct {
	conn    db.Connection
	primary storage.Provider
}

// NewPurger returns a Purger backed by conn and primary. The connection MUST
// authenticate as purge_user (member of retention_purge); the delete-path
// immutability triggers enforce this at the DB layer. purge_user is NOT
// BYPASSRLS by design (enforced in the DB schema), so setting
// app.current_tenant via set_config is REQUIRED before every DELETE —
// without it the tenant_isolation policy matches zero rows.
func NewPurger(conn db.Connection, primary storage.Provider) Purger {
	return &purger{conn: conn, primary: primary}
}

// Purge deletes the data identified by c from the appropriate backend.
// For StorePostgres, both the RLS tenant context and the DELETE run inside a
// single transaction; the transaction is rolled back on any error.
func (p *purger) Purge(ctx context.Context, c Candidate) error {
	switch c.Store {
	case StoreObject:
		if err := p.primary.Delete(ctx, c.ObjectKey); err != nil {
			return fmt.Errorf("delete object %s: %w", c.ObjectKey, err)
		}
		return nil
	case StorePostgres:
		return p.purgeDB(ctx, c)
	default:
		return fmt.Errorf("unsupported store: %s", c.Store)
	}
}

func (p *purger) purgeDB(ctx context.Context, c Candidate) error {
	tx, err := p.conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin purge transaction: %w", err)
	}
	// Rollback after a successful Commit is a harmless no-op (pgx returns
	// ErrTxClosed, which we discard). The guard also covers the Commit-failure
	// path so callers never observe a half-applied transaction.
	defer func() { _ = tx.Rollback() }()

	agg, ok := dbAggregates[c.Class]
	if !ok {
		return fmt.Errorf("unsupported purge class: %s", c.Class)
	}

	if err := tx.Exec(ctx, "SELECT set_config('app.current_tenant', $1, true)", c.TenantID); err != nil {
		return fmt.Errorf("set tenant for purge: %w", err)
	}

	// Delete every FK child before the parent so no child outlives the FK to a
	// deleted parent. The children reference only the parent (not one another), so
	// their relative order is free; the parent MUST be last.
	for _, child := range agg.children {
		if err := tx.Exec(ctx, "DELETE FROM "+child.table+" WHERE "+child.fkCol+" = $1", c.ID); err != nil {
			return fmt.Errorf("purge %s %s: %w", child.table, c.ID, err)
		}
	}
	if err := tx.Exec(ctx, "DELETE FROM "+agg.parentTable+" WHERE "+agg.parentPK+" = $1", c.ID); err != nil {
		return fmt.Errorf("purge %s %s: %w", agg.parentTable, c.ID, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit purge: %w", err)
	}
	return nil
}
