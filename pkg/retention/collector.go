package retention

import (
	"context"
	"fmt"
	"time"

	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/storage"
)

// Collector produces Candidates from a backing store for retention evaluation.
type Collector interface {
	Collect(ctx context.Context, p Policy, now time.Time) ([]Candidate, error)
}

// dbCollector collects completed jobs from Postgres.
type dbCollector struct {
	conn db.Connection
}

// NewDBCollector returns a Collector that scans completed jobs from the given
// Postgres connection. Only jobs whose retention has expired are returned.
func NewDBCollector(conn db.Connection) Collector {
	return &dbCollector{conn: conn}
}

// Collect queries all completed jobs and returns those whose retention has expired.
func (c *dbCollector) Collect(ctx context.Context, p Policy, now time.Time) ([]Candidate, error) {
	// conn is a TenantConnection (its Query is rejected outside a transaction),
	// so the scan MUST run inside Begin. The request tenant carried in ctx scopes
	// the jobs read via RLS.
	tx, err := c.conn.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("db collector: begin: %w", err)
	}
	// Rollback after a successful Commit is a harmless no-op (pgx returns
	// ErrTxClosed, which we discard); the guard also covers the error paths.
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.Query(ctx, `SELECT job_id, tenant_id, created_at FROM jobs WHERE status='completed'`)
	if err != nil {
		return nil, fmt.Errorf("db collector: query: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var candidates []Candidate
	for rows.Next() {
		var jobID, tenantID string
		var createdAt time.Time
		if err := rows.Scan(&jobID, &tenantID, &createdAt); err != nil {
			return nil, err
		}
		if !p.Expired(ClassJobResults, tenantID, createdAt, now) {
			continue
		}
		candidates = append(candidates, Candidate{
			Store:     StorePostgres,
			Class:     ClassJobResults,
			ID:        jobID,
			TenantID:  tenantID,
			CreatedAt: createdAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close() //nolint:errcheck  // release before Commit
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("db collector: commit: %w", err)
	}
	return candidates, nil
}

// catalogCollector collects catalogs from Postgres. Every catalog is a
// candidate (there is no status filter as with jobs); expiry is decided per row
// by the policy against created_at.
type catalogCollector struct {
	conn db.Connection
}

// NewCatalogCollector returns a Collector that scans catalogs from the given
// Postgres connection. Only catalogs whose retention has expired are returned;
// their classifications, controls, and embeddings are purged as dependents.
func NewCatalogCollector(conn db.Connection) Collector {
	return &catalogCollector{conn: conn}
}

// Collect queries all catalogs and returns those whose retention has expired.
func (c *catalogCollector) Collect(ctx context.Context, p Policy, now time.Time) ([]Candidate, error) {
	// conn is a TenantConnection (its Query is rejected outside a transaction),
	// so the scan MUST run inside Begin. The request tenant carried in ctx scopes
	// the catalogs read via RLS.
	tx, err := c.conn.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("catalog collector: begin: %w", err)
	}
	// Rollback after a successful Commit is a harmless no-op (pgx returns
	// ErrTxClosed, which we discard); the guard also covers the error paths.
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.Query(ctx, `SELECT catalog_id, tenant_id, created_at FROM catalogs`)
	if err != nil {
		return nil, fmt.Errorf("catalog collector: query: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var candidates []Candidate
	for rows.Next() {
		var catalogID, tenantID string
		var createdAt time.Time
		if err := rows.Scan(&catalogID, &tenantID, &createdAt); err != nil {
			return nil, err
		}
		if !p.Expired(ClassCatalogs, tenantID, createdAt, now) {
			continue
		}
		candidates = append(candidates, Candidate{
			Store:     StorePostgres,
			Class:     ClassCatalogs,
			ID:        catalogID,
			TenantID:  tenantID,
			CreatedAt: createdAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close() //nolint:errcheck  // release before Commit
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("catalog collector: commit: %w", err)
	}
	return candidates, nil
}

// objectCollector collects objects of a given DataClass from an object store.
type objectCollector struct {
	prov     storage.Provider
	class    DataClass
	tenantID string
}

// NewObjectCollector returns a Collector that scans objects of the given class
// from the provider. The provider must already be scoped to the tenant's store;
// tenantID is used only for policy lookup and Candidate stamping.
func NewObjectCollector(prov storage.Provider, class DataClass, tenantID string) Collector {
	return &objectCollector{prov: prov, class: class, tenantID: tenantID}
}

// classPrefix returns the object key prefix for the given DataClass.
func classPrefix(class DataClass) string {
	switch class {
	case ClassAttestation:
		return "attestation/"
	default:
		return string(class) + "/"
	}
}

// Collect lists all objects for the collector's class and returns those whose
// retention has expired. Size and LastModified come from List metadata directly —
// no per-object Stat call is made.
func (c *objectCollector) Collect(ctx context.Context, p Policy, now time.Time) ([]Candidate, error) {
	prefix := classPrefix(c.class)
	objects, err := c.prov.List(ctx, prefix)
	if err != nil {
		return nil, err
	}

	var candidates []Candidate
	for _, meta := range objects {
		createdAt := time.Unix(meta.LastModified, 0).UTC()
		if !p.Expired(c.class, c.tenantID, createdAt, now) {
			continue
		}
		candidates = append(candidates, Candidate{
			Store:     StoreObject,
			Class:     c.class,
			ID:        meta.Key,
			TenantID:  c.tenantID,
			CreatedAt: createdAt,
			Size:      meta.Size,
			ObjectKey: meta.Key,
		})
	}
	return candidates, nil
}
