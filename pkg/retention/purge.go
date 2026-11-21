package retention

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/storage"
)

// ErrCatalogIsCurrent is wrapped when a purge names a catalog that is its
// document's current version (catalog_documents.current_catalog_id). The purge
// refuses it and changes nothing: deleting it would leave the document without
// a current version, and choosing a new one is not retention's decision.
var ErrCatalogIsCurrent = errors.New("catalog is its document's current version")

// Purger deletes expired data after the Archiver has verified a copy.
type Purger interface {
	Purge(ctx context.Context, c Candidate) error
	// PurgeBatch deletes every StorePostgres candidate of one tenant in a
	// single transaction: all of them are deleted or, on any error, none.
	// It refuses an empty batch, candidates of more than one tenant, and any
	// StoreObject candidate before it begins the transaction.
	PurgeBatch(ctx context.Context, cs []Candidate) error
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
// llm_result_cache is purge-only the same way (derived and regenerable), for
// both classes.
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
			// The job's cached LLM results (derived, regenerable: purge-only)
			// and its append-only cache audit record (archived first, see
			// dbAggregateQueries). Purging them with the producer keeps
			// every surviving hit verifiable against a write that exists.
			{"llm_result_cache", "job_id"},
			{"job_cache_entries", "job_id"},
			{"job_cache_stats", "job_id"},
		},
	},
	ClassCatalogs: {
		parentTable: "catalogs", parentPK: "catalog_id",
		children: []struct{ table, fkCol string }{
			{"classifications", "catalog_id"},
			{"embeddings", "catalog_id"},
			{"controls", "catalog_id"},
			{"catalog_containers", "catalog_id"},
			// Cached results carry either catalog's text, so either purges
			// them. Purge-only, like embeddings.
			{"llm_result_cache", "source_catalog_id"},
			{"llm_result_cache", "target_catalog_id"},
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
	return p.purgeDBTx(ctx, c.TenantID, []Candidate{c})
}

// PurgeBatch implements Purger.
func (p *purger) PurgeBatch(ctx context.Context, cs []Candidate) error {
	if len(cs) == 0 {
		return errors.New("purge batch is empty; pass at least one candidate")
	}
	tenantID := cs[0].TenantID
	for _, c := range cs {
		if c.Store != StorePostgres {
			return fmt.Errorf("purge batch: candidate %s is in store %s; a batch deletes database rows in one transaction, so purge object candidates one at a time with Purge", c.ID, c.Store)
		}
		if c.TenantID != tenantID {
			return fmt.Errorf("purge batch: candidate %s belongs to tenant %q, not %q; a batch runs under one tenant's row-level security, so split it per tenant", c.ID, c.TenantID, tenantID)
		}
	}
	return p.purgeDBTx(ctx, tenantID, cs)
}

// purgeDBTx deletes every candidate's aggregate in one transaction scoped to
// tenantID, committing only when all of them are deleted.
func (p *purger) purgeDBTx(ctx context.Context, tenantID string, cs []Candidate) error {
	tx, err := p.conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin purge transaction: %w", err)
	}
	// Rollback after a successful Commit is a harmless no-op (pgx returns
	// ErrTxClosed, which we discard). The guard also covers the Commit-failure
	// path so callers never observe a half-applied transaction.
	defer func() { _ = tx.Rollback() }()

	if err := tx.Exec(ctx, "SELECT set_config('app.current_tenant', $1, true)", tenantID); err != nil {
		return fmt.Errorf("set tenant for purge: %w", err)
	}

	for _, c := range cs {
		agg, ok := dbAggregates[c.Class]
		if !ok {
			return fmt.Errorf("unsupported purge class: %s", c.Class)
		}
		if c.Class == ClassCatalogs {
			if err := unlinkCatalogVersion(ctx, tx, c.ID); err != nil {
				return err
			}
		}
		// Delete every FK child before the parent so no child outlives the FK
		// to a deleted parent. The children reference only the parent (not
		// one another), so their relative order is free; the parent MUST be
		// last.
		for _, child := range agg.children {
			if err := tx.Exec(ctx, "DELETE FROM "+child.table+" WHERE "+child.fkCol+" = $1", c.ID); err != nil {
				return fmt.Errorf("purge %s %s: %w", child.table, c.ID, err)
			}
		}
		if err := tx.Exec(ctx, "DELETE FROM "+agg.parentTable+" WHERE "+agg.parentPK+" = $1", c.ID); err != nil {
			return fmt.Errorf("purge %s %s: %w", agg.parentTable, c.ID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit purge: %w", err)
	}
	return nil
}

// unlinkCatalogVersion takes catalogID out of its document's version chain
// inside tx, so the lineage foreign keys (ON DELETE NO ACTION, migration 002)
// do not block deleting it. A document's current version is refused with
// ErrCatalogIsCurrent. Otherwise the version that supersedes catalogID is
// re-pointed to catalogID's own predecessor (NULL when catalogID was the
// first version), which keeps the chain intact. A catalog outside any
// document matches no row and is unaffected.
//
// purge_user holds exactly the privileges this needs: SELECT on
// catalog_documents and UPDATE on catalogs.supersedes_catalog_id.
func unlinkCatalogVersion(ctx context.Context, tx db.Transaction, catalogID string) error {
	var slug string
	err := tx.QueryRow(ctx,
		"SELECT document_slug FROM catalog_documents WHERE current_catalog_id = $1", catalogID).Scan(&slug)
	switch {
	case err == nil:
		return fmt.Errorf("%w: catalog %s is the current version of document %q, so it was not purged and nothing was deleted. To remove it, import a newer version of the document first so this one is superseded, then purge or retire it",
			ErrCatalogIsCurrent, catalogID, slug)
	case errors.Is(err, sql.ErrNoRows):
	default:
		return fmt.Errorf("purge catalog %s: check whether it is a document's current version: %w", catalogID, err)
	}
	if err := tx.Exec(ctx,
		`UPDATE catalogs SET supersedes_catalog_id = (SELECT supersedes_catalog_id FROM catalogs WHERE catalog_id = $1) WHERE supersedes_catalog_id = $1`,
		catalogID); err != nil {
		return fmt.Errorf("purge catalog %s: re-point the version that supersedes it: %w", catalogID, err)
	}
	return nil
}
