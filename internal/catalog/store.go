package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/jobconfig"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// ErrCatalogNotFound, ErrControlNotFound and ErrDocumentNotFound are wrapped by
// Store lookups for a row that does not exist or that row-level security hides
// from the current tenant. ErrCatalogInOtherDocument is wrapped when a version
// import names a document but the content already belongs to a different one.
// Callers classify with errors.Is, never by message text.
var (
	ErrCatalogNotFound        = errors.New("catalog not found")
	ErrControlNotFound        = errors.New("control not found")
	ErrDocumentNotFound       = errors.New("document not found")
	ErrCatalogInOtherDocument = errors.New("catalog belongs to another document")
)

// Store interface for catalog and control persistence.
type Store interface {
	GetCatalog(ctx context.Context, catalogID string) (*CatalogRecord, error)
	ListCatalogs(ctx context.Context, opts ListOptions) ([]CatalogRecord, PageInfo, error)
	UpsertCatalogWithTree(ctx context.Context, catalog CatalogRecord, containers []ContainerRecord, controls []ControlRecord) error
	// GetDocumentCurrent returns the current version of the context tenant's
	// document slug, or an error wrapping ErrDocumentNotFound.
	GetDocumentCurrent(ctx context.Context, slug string) (*CatalogRecord, error)
	// ListControlsForCatalog returns every control of a catalog of the context
	// tenant, ordered by control ID; empty when the catalog has none or is not
	// the tenant's.
	ListControlsForCatalog(ctx context.Context, catalogID string) ([]ControlRecord, error)
	// UpsertCatalogVersion is UpsertCatalogWithTree for a catalog that carries
	// a DocumentSlug and VersionLabel: it also records the catalog as the
	// document's newest version. See PGStore.UpsertCatalogVersion.
	UpsertCatalogVersion(ctx context.Context, catalog CatalogRecord, containers []ContainerRecord, controls []ControlRecord) error
	// ListDocumentVersions returns every version of the context tenant's
	// document slug, oldest first, and the ID of its current version, or an
	// error wrapping ErrDocumentNotFound.
	ListDocumentVersions(ctx context.Context, slug string) ([]CatalogRecord, string, error)
	// JobsUsingCatalogs returns, for each of catalogIDs that any of the
	// context tenant's jobs uses, those jobs and their stored status sorted by
	// job ID, read from each job's stored config with
	// jobconfig.ExtractCatalogs, and the sorted IDs of the jobs whose stored
	// config is NULL or cannot be read (so what they use is unknown).
	JobsUsingCatalogs(ctx context.Context, catalogIDs []string) (uses map[string][]JobUse, unreadable []string, err error)
	GetControl(ctx context.Context, controlID string) (*ControlRecord, error)
	SearchControls(ctx context.Context, query SearchQuery) ([]ControlRecord, PageInfo, error)
}

// CatalogRecord represents a catalog row in the catalogs table.
type CatalogRecord struct {
	CatalogID        string
	TenantID         string
	Name             string
	Version          string
	SourceType       string
	ObjectPath       string
	CreatedAt        time.Time
	SourceURI        string
	ContentHash      string
	ContentSize      int64
	Format           string
	OutputHash       string
	ExtractorName    string
	ExtractorVersion string
	ControlCount     int64
	// Document lineage, all "" for a catalog stored without a document.
	// SupersedesCatalogID is "" for a document's first version. LinkMethod is
	// how the version was tied to its document ("declared" for an explicit slug).
	DocumentSlug        string
	VersionLabel        string
	SupersedesCatalogID string
	LinkMethod          string
}

// ControlRecord represents a control row in the controls table.
type ControlRecord struct {
	TenantID   string
	ControlID  string
	CatalogID  string
	Identifier string
	Title      string
	Statement  string
	Class      string
	ParentID   string
	GroupID    string
	// ContainerKey names the innermost catalog_containers row of the same
	// catalog that holds this item; "" (stored as NULL) when it is ungrouped.
	ContainerKey string
	Props        map[string]string
	CreatedAt    time.Time
}

// ContainerRecord represents a grouping node in the catalog_containers table.
// Key is the parser's position key, unique within the catalog; ParentKey is ""
// for a top-level container (stored as NULL); Identifier is "" when the source
// group has no ID; Label is "" when the source group publishes none; Prose is ""
// when the group has no text of its own.
type ContainerRecord struct {
	TenantID   string
	CatalogID  string
	Key        string
	Identifier string
	Title      string
	Label      string
	Prose      string
	ParentKey  string
}

// SearchQuery represents a control search request.
type SearchQuery struct {
	Query      string
	CatalogIDs []string
	Vector     []float32
	Limit      int
	Offset     int
}

// ListOptions represents pagination options for list operations.
type ListOptions struct {
	Limit  int
	Offset int
}

// EffectiveLimit returns the limit capped between default and maximum.
func (o ListOptions) EffectiveLimit() int {
	if o.Limit <= 0 {
		return 50
	}
	if o.Limit > 1000 {
		return 1000
	}
	return o.Limit
}

// PageInfo represents pagination metadata.
type PageInfo struct {
	NextOffset int
	TotalCount int64
}

// PGStore implements Store with PostgreSQL.
type PGStore struct {
	conn db.Connection
}

// NewPGStore creates a new PostgreSQL store.
func NewPGStore(conn db.Connection) *PGStore {
	return &PGStore{conn: conn}
}

// catalogColumns is the SELECT list catalogScanDest reads, for a query over
// the catalogs table. NULL provenance and lineage columns read as zero values.
const catalogColumns = `
		catalog_id, tenant_id, name, version, source_type, object_path, created_at,
		COALESCE(source_uri, '') as source_uri,
		COALESCE(content_hash, '') as content_hash,
		COALESCE(content_size, 0) as content_size,
		COALESCE(format, '') as format,
		COALESCE(output_hash, '') as output_hash,
		COALESCE(extractor_name, '') as extractor_name,
		COALESCE(extractor_version, '') as extractor_version,
		(SELECT COUNT(*) FROM controls c WHERE c.catalog_id = catalogs.catalog_id AND c.tenant_id = catalogs.tenant_id) as control_count,
		COALESCE(document_slug, '') as document_slug,
		COALESCE(version_label, '') as version_label,
		COALESCE(supersedes_catalog_id, '') as supersedes_catalog_id,
		COALESCE(link_method, '') as link_method
	`

// catalogScanDest returns the Scan destinations for catalogColumns.
func catalogScanDest(rec *CatalogRecord) []any {
	return []any{
		&rec.CatalogID, &rec.TenantID, &rec.Name, &rec.Version, &rec.SourceType,
		&rec.ObjectPath, &rec.CreatedAt, &rec.SourceURI, &rec.ContentHash,
		&rec.ContentSize, &rec.Format, &rec.OutputHash, &rec.ExtractorName,
		&rec.ExtractorVersion, &rec.ControlCount, &rec.DocumentSlug,
		&rec.VersionLabel, &rec.SupersedesCatalogID, &rec.LinkMethod,
	}
}

// upsertCatalogTx writes catalog inside tx. The caller has checked the record's
// tenant against the request's.
func upsertCatalogTx(ctx context.Context, tx db.Transaction, catalog CatalogRecord) error {
	query := `
		INSERT INTO catalogs (
			catalog_id, tenant_id, name, version, source_type, object_path, created_at,
			source_uri, content_hash, content_size, format, output_hash,
			extractor_name, extractor_version
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14
		)
		ON CONFLICT (catalog_id) DO UPDATE SET
			name = EXCLUDED.name,
			version = EXCLUDED.version,
			source_type = EXCLUDED.source_type,
			object_path = EXCLUDED.object_path,
			source_uri = EXCLUDED.source_uri,
			content_hash = EXCLUDED.content_hash,
			content_size = EXCLUDED.content_size,
			format = EXCLUDED.format,
			output_hash = EXCLUDED.output_hash,
			extractor_name = EXCLUDED.extractor_name,
			extractor_version = EXCLUDED.extractor_version
		WHERE catalogs.tenant_id = EXCLUDED.tenant_id
		RETURNING catalog_id
	`

	var upserted string
	err := tx.QueryRow(ctx, query,
		catalog.CatalogID,
		catalog.TenantID,
		catalog.Name,
		catalog.Version,
		catalog.SourceType,
		catalog.ObjectPath,
		catalog.CreatedAt,
		catalog.SourceURI,
		catalog.ContentHash,
		catalog.ContentSize,
		catalog.Format,
		catalog.OutputHash,
		catalog.ExtractorName,
		catalog.ExtractorVersion,
	).Scan(&upserted)
	// A catalog ID owned by another tenant is refused two ways. On a
	// connection RLS does not apply to (owner), the WHERE above skips the
	// update and RETURNING yields no row. Under the restricted application
	// role, PostgreSQL checks the UPDATE policy's USING expression against
	// the conflicting row and raises 42501 instead. Both leave the data
	// unchanged. Catalog IDs hash the content with the tenant, so this is a
	// collision or a caller-built ID, never a re-import. The 42501 cause stays
	// in the chain because a missing GRANT or an unset app.current_tenant
	// raises 42501 here too.
	const anotherTenant = "upsert catalog %s: the catalog ID belongs to another tenant; nothing was written. Catalog IDs are derived from content and tenant, so report this collision to the operator"
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf(anotherTenant, catalog.CatalogID)
	}
	if db.IsPgErrorCode(err, "42501") {
		return fmt.Errorf(anotherTenant+": %w", catalog.CatalogID, err)
	}
	if err != nil {
		return fmt.Errorf("upsert catalog %s: %w", catalog.CatalogID, err)
	}
	return nil
}

// GetCatalog retrieves a catalog of the context tenant by ID.
func (s *PGStore) GetCatalog(ctx context.Context, catalogID string) (*CatalogRecord, error) {
	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("get catalog: %w", err)
	}

	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin get catalog: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op

	query := `
		SELECT ` + catalogColumns + `
		FROM catalogs
		WHERE catalog_id = $1 AND tenant_id = $2
	`

	var rec CatalogRecord
	err = tx.QueryRow(ctx, query, catalogID, tenantID).Scan(
		catalogScanDest(&rec)...,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrCatalogNotFound, catalogID)
	}
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit get catalog: %w", err)
	}
	return &rec, nil
}

// ListCatalogs retrieves the context tenant's catalogs with pagination.
func (s *PGStore) ListCatalogs(ctx context.Context, opts ListOptions) ([]CatalogRecord, PageInfo, error) {
	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		return nil, PageInfo{}, fmt.Errorf("list catalogs: %w", err)
	}

	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return nil, PageInfo{}, fmt.Errorf("begin list catalogs: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op

	limit := opts.EffectiveLimit()

	// Get total count
	var totalCount int64
	if err := tx.QueryRow(ctx, "SELECT COUNT(*) FROM catalogs WHERE tenant_id = $1", tenantID).Scan(&totalCount); err != nil {
		return nil, PageInfo{}, err
	}

	// Get paginated results
	query := `
		SELECT ` + catalogColumns + `
		FROM catalogs
		WHERE tenant_id = $1
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`

	rows, err := tx.Query(ctx, query, tenantID, limit, opts.Offset)
	if err != nil {
		return nil, PageInfo{}, err
	}
	defer rows.Close()

	var records []CatalogRecord
	for rows.Next() {
		var rec CatalogRecord
		err := rows.Scan(
			catalogScanDest(&rec)...,
		)
		if err != nil {
			return nil, PageInfo{}, err
		}
		records = append(records, rec)
	}

	if err := rows.Err(); err != nil {
		return nil, PageInfo{}, err
	}

	if err := tx.Commit(); err != nil {
		return nil, PageInfo{}, fmt.Errorf("commit list catalogs: %w", err)
	}

	nextOffset := opts.Offset + len(records)
	pageInfo := PageInfo{
		NextOffset: nextOffset,
		TotalCount: totalCount,
	}

	return records, pageInfo, nil
}

// Refusals of a catalog that is not the request tenant's, naming the first
// offending record: control ID or container key, catalog ID, tenant ID.
const (
	errNotTenantCatalog          = "upsert control %s: catalog %q is not a catalog of tenant %q, so no control in this batch was written. Set the record's catalog ID to the catalog this batch writes, or to one of the tenant's catalog IDs"
	errNotTenantCatalogContainer = "upsert container %s: catalog %q is not a catalog of tenant %q, so no container or control in this batch was written. Set the record's catalog ID to the catalog this batch writes, or to one of the tenant's catalog IDs"
)

// UpsertCatalogWithTree writes a catalog row, its containers and its controls
// for the context tenant in one transaction, so a failed import stores nothing:
// no catalog row without its controls, and a failed re-import leaves the
// earlier import as it was. It refuses records for another tenant, or a
// catalog ID of another tenant, without writing anything.
func (s *PGStore) UpsertCatalogWithTree(ctx context.Context, catalog CatalogRecord, containers []ContainerRecord, controls []ControlRecord) error {
	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		return fmt.Errorf("upsert catalog: %w", err)
	}
	if catalog.TenantID != tenantID {
		return fmt.Errorf("upsert catalog %s: record tenant %q does not match the request tenant %q; refusing to write across tenants. Build the record with the request's tenant",
			catalog.CatalogID, catalog.TenantID, tenantID)
	}
	if err := checkTreeTenants(tenantID, containers, controls); err != nil {
		return err
	}

	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin upsert catalog %s: %w", catalog.CatalogID, err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op

	if err := upsertCatalogTx(ctx, tx, catalog); err != nil {
		return err
	}
	if err := upsertTreeTx(ctx, tx, tenantID, containers, controls); err != nil {
		return err
	}
	return tx.Commit()
}

// checkTreeTenants refuses any container or control record that is not the
// request tenant's, so the caller writes nothing for a mixed batch.
func checkTreeTenants(tenantID string, containers []ContainerRecord, controls []ControlRecord) error {
	for _, c := range containers {
		if c.TenantID != tenantID {
			return fmt.Errorf("upsert container %s: record tenant %q does not match the request tenant %q; refusing to write across tenants, so no container or control in this batch was written. Build every record with the request's tenant",
				c.Key, c.TenantID, tenantID)
		}
	}
	for _, ctrl := range controls {
		if ctrl.TenantID != tenantID {
			return fmt.Errorf("upsert control %s: record tenant %q does not match the request tenant %q; refusing to write across tenants, so no control in this batch was written. Build every record with the request's tenant",
				ctrl.ControlID, ctrl.TenantID, tenantID)
		}
	}
	return nil
}

// upsertTreeTx writes containers, then controls, inside tx. The caller has
// checked every record's tenant.
func upsertTreeTx(ctx context.Context, tx db.Transaction, tenantID string, containers []ContainerRecord, controls []ControlRecord) error {
	// The (catalog_id, tenant_id) foreign key to catalogs
	// refuses another tenant's catalog; checking first names the catalog
	// and tells the caller what to do instead.
	checked := make(map[string]bool)
	checkCatalog := func(noun, catalogID, recordID, refusal string) error {
		if checked[catalogID] {
			return nil
		}
		var owned int
		err := tx.QueryRow(ctx, "SELECT 1 FROM catalogs WHERE catalog_id = $1 AND tenant_id = $2",
			catalogID, tenantID).Scan(&owned)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf(refusal, recordID, catalogID, tenantID)
		}
		if err != nil {
			return fmt.Errorf("upsert %s %s: check catalog %s: %w", noun, recordID, catalogID, err)
		}
		checked[catalogID] = true
		return nil
	}
	for _, c := range containers {
		if err := checkCatalog("container", c.CatalogID, c.Key, errNotTenantCatalogContainer); err != nil {
			return err
		}
	}
	for _, ctrl := range controls {
		if err := checkCatalog("control", ctrl.CatalogID, ctrl.ControlID, errNotTenantCatalog); err != nil {
			return err
		}
	}

	// Containers go first: controls.container_key refers to them by key.
	containerQuery := `
		INSERT INTO catalog_containers (
			tenant_id, catalog_id, container_key, identifier, title, label, prose, parent_key
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8
		)
		ON CONFLICT (tenant_id, catalog_id, container_key) DO UPDATE SET
			identifier = EXCLUDED.identifier,
			title = EXCLUDED.title,
			label = EXCLUDED.label,
			prose = EXCLUDED.prose,
			parent_key = EXCLUDED.parent_key
	`
	for _, c := range containers {
		if err := tx.Exec(ctx, containerQuery,
			c.TenantID, c.CatalogID, c.Key, c.Identifier, c.Title, c.Label, c.Prose, nullIfEmpty(c.ParentKey),
		); err != nil {
			// A foreign-key violation past the check above means the
			// catalog was deleted concurrently; tenants are never deleted.
			if db.IsPgErrorCode(err, "23503") {
				return fmt.Errorf(errNotTenantCatalogContainer+": %w",
					c.Key, c.CatalogID, tenantID, err)
			}
			return fmt.Errorf("upsert container %s: %w", c.Key, err)
		}
	}

	query := `
		INSERT INTO controls (
			tenant_id, control_id, catalog_id, identifier, title, statement,
			class, parent_id, group_id, container_key, props, created_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12
		)
		ON CONFLICT (tenant_id, control_id) DO UPDATE SET
			catalog_id = EXCLUDED.catalog_id,
			identifier = EXCLUDED.identifier,
			title = EXCLUDED.title,
			statement = EXCLUDED.statement,
			class = EXCLUDED.class,
			parent_id = EXCLUDED.parent_id,
			group_id = EXCLUDED.group_id,
			container_key = EXCLUDED.container_key,
			props = EXCLUDED.props
	`

	for _, ctrl := range controls {
		propsJSON, err := json.Marshal(ctrl.Props)
		if err != nil {
			return fmt.Errorf("marshal props for control %s: %w", ctrl.ControlID, err)
		}

		if err := tx.Exec(ctx, query,
			ctrl.TenantID,
			ctrl.ControlID,
			ctrl.CatalogID,
			ctrl.Identifier,
			ctrl.Title,
			ctrl.Statement,
			ctrl.Class,
			ctrl.ParentID,
			ctrl.GroupID,
			nullIfEmpty(ctrl.ContainerKey),
			propsJSON,
			ctrl.CreatedAt,
		); err != nil {
			// A foreign-key violation past the check above means the
			// catalog was deleted concurrently; tenants are never deleted.
			if db.IsPgErrorCode(err, "23503") {
				return fmt.Errorf(errNotTenantCatalog+": %w",
					ctrl.ControlID, ctrl.CatalogID, tenantID, err)
			}
			return fmt.Errorf("upsert control %s: %w", ctrl.ControlID, err)
		}
	}
	return nil
}

// nullIfEmpty maps "" to SQL NULL for the nullable key columns.
func nullIfEmpty(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// GetDocumentCurrent returns the current version of the context tenant's
// document slug.
func (s *PGStore) GetDocumentCurrent(ctx context.Context, slug string) (*CatalogRecord, error) {
	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("get document current: %w", err)
	}

	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin get document current: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op

	query := `
		SELECT ` + catalogColumns + `
		FROM catalogs
		WHERE tenant_id = $1 AND catalog_id = (
			SELECT current_catalog_id FROM catalog_documents
			WHERE tenant_id = $1 AND document_slug = $2
		)
	`
	var rec CatalogRecord
	err = tx.QueryRow(ctx, query, tenantID, slug).Scan(catalogScanDest(&rec)...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrDocumentNotFound, slug)
	}
	if err != nil {
		return nil, fmt.Errorf("get document current %s: %w", slug, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit get document current: %w", err)
	}
	return &rec, nil
}

// UpsertCatalogVersion writes catalog, its containers and its controls like
// UpsertCatalogWithTree and records the catalog as the newest version of the
// document named by catalog.DocumentSlug, in one transaction.
//
//   - A new version supersedes the document's current one and becomes current.
//     The first version of a slug creates the document. Content stored earlier
//     without a document is attached to it, not duplicated.
//   - Content already in this document is refreshed but keeps its place: current
//     does not move and its lineage and label are unchanged.
//   - Content already in another document is refused with ErrCatalogInOtherDocument
//     and nothing is written; a catalog belongs to one document.
//
// Concurrent imports under one slug serialise on the catalog_documents row, so
// the versions form one chain with exactly one current. The row is created
// with ON CONFLICT DO NOTHING (the loser waits for the winner's commit) and
// then locked with FOR UPDATE before current is read.
//
// The slug is not validated here; the service validates it before calling.
func (s *PGStore) UpsertCatalogVersion(ctx context.Context, catalog CatalogRecord, containers []ContainerRecord, controls []ControlRecord) error {
	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		return fmt.Errorf("upsert catalog version: %w", err)
	}
	if catalog.DocumentSlug == "" {
		return fmt.Errorf("upsert catalog version %s: the record has no document slug. Set DocumentSlug, or use UpsertCatalogWithTree for a catalog outside any document", catalog.CatalogID)
	}
	if catalog.TenantID != tenantID {
		return fmt.Errorf("upsert catalog version %s: record tenant %q does not match the request tenant %q; refusing to write across tenants. Build the record with the request's tenant",
			catalog.CatalogID, catalog.TenantID, tenantID)
	}
	if err := checkTreeTenants(tenantID, containers, controls); err != nil {
		return err
	}

	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin upsert catalog version %s: %w", catalog.CatalogID, err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op

	// The upsert leaves the lineage columns alone and, for a row that exists,
	// locks it, so the document_slug read below cannot change under us.
	if err := upsertCatalogTx(ctx, tx, catalog); err != nil {
		return err
	}
	var existingSlug string
	err = tx.QueryRow(ctx,
		"SELECT COALESCE(document_slug, '') FROM catalogs WHERE catalog_id = $1 AND tenant_id = $2",
		catalog.CatalogID, tenantID).Scan(&existingSlug)
	if err != nil {
		return fmt.Errorf("upsert catalog version %s: read document: %w", catalog.CatalogID, err)
	}
	if existingSlug != "" && existingSlug != catalog.DocumentSlug {
		return fmt.Errorf("%w: catalog %s is already version of document %q, so it was not added to %q and nothing was written. A catalog belongs to one document; import it under %q, or import changed content under %q",
			ErrCatalogInOtherDocument, catalog.CatalogID, existingSlug, catalog.DocumentSlug, existingSlug, catalog.DocumentSlug)
	}

	if existingSlug == "" {
		if err := linkVersionTx(ctx, tx, tenantID, catalog); err != nil {
			return err
		}
	}
	if err := upsertTreeTx(ctx, tx, tenantID, containers, controls); err != nil {
		return err
	}
	return tx.Commit()
}

// linkVersionTx makes catalog, which belongs to no document yet, the newest
// version of catalog.DocumentSlug inside tx.
func linkVersionTx(ctx context.Context, tx db.Transaction, tenantID string, catalog CatalogRecord) error {
	// Create the document if absent. Under a concurrent first import the
	// second insert waits for the first transaction, then does nothing.
	var created string
	err := tx.QueryRow(ctx, `
		INSERT INTO catalog_documents (tenant_id, document_slug, current_catalog_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (tenant_id, document_slug) DO NOTHING
		RETURNING document_slug
	`, tenantID, catalog.DocumentSlug, catalog.CatalogID).Scan(&created)
	var supersedes any
	switch {
	case err == nil:
		// First version: nothing to supersede, current already points here.
	case errors.Is(err, sql.ErrNoRows):
		var current string
		err = tx.QueryRow(ctx,
			"SELECT current_catalog_id FROM catalog_documents WHERE tenant_id = $1 AND document_slug = $2 FOR UPDATE",
			tenantID, catalog.DocumentSlug).Scan(&current)
		if err != nil {
			return fmt.Errorf("upsert catalog version %s: lock document %q: %w", catalog.CatalogID, catalog.DocumentSlug, err)
		}
		supersedes = current
		if err := tx.Exec(ctx,
			"UPDATE catalog_documents SET current_catalog_id = $3 WHERE tenant_id = $1 AND document_slug = $2",
			tenantID, catalog.DocumentSlug, catalog.CatalogID); err != nil {
			return fmt.Errorf("upsert catalog version %s: move current of document %q: %w", catalog.CatalogID, catalog.DocumentSlug, err)
		}
	default:
		return fmt.Errorf("upsert catalog version %s: create document %q: %w", catalog.CatalogID, catalog.DocumentSlug, err)
	}

	if err := tx.Exec(ctx, `
		UPDATE catalogs
		SET document_slug = $3, version_label = $4, supersedes_catalog_id = $5, link_method = 'declared'
		WHERE catalog_id = $1 AND tenant_id = $2
	`, catalog.CatalogID, tenantID, catalog.DocumentSlug, nullIfEmpty(catalog.VersionLabel), supersedes); err != nil {
		return fmt.Errorf("upsert catalog version %s: record lineage: %w", catalog.CatalogID, err)
	}
	return nil
}

// ListDocumentVersions returns the versions of the context tenant's document
// slug, oldest first (ties by catalog ID), and its current version's ID.
func (s *PGStore) ListDocumentVersions(ctx context.Context, slug string) ([]CatalogRecord, string, error) {
	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("list document versions: %w", err)
	}

	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("begin list document versions: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op

	var current string
	err = tx.QueryRow(ctx,
		"SELECT current_catalog_id FROM catalog_documents WHERE tenant_id = $1 AND document_slug = $2",
		tenantID, slug).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", fmt.Errorf("%w: %s", ErrDocumentNotFound, slug)
	}
	if err != nil {
		return nil, "", fmt.Errorf("read document %s: %w", slug, err)
	}

	rows, err := tx.Query(ctx, `
		SELECT `+catalogColumns+`
		FROM catalogs
		WHERE tenant_id = $1 AND document_slug = $2
		ORDER BY created_at, catalog_id
	`, tenantID, slug)
	if err != nil {
		return nil, "", fmt.Errorf("list versions of document %s: %w", slug, err)
	}
	defer rows.Close()
	var versions []CatalogRecord
	for rows.Next() {
		var rec CatalogRecord
		if err := rows.Scan(catalogScanDest(&rec)...); err != nil {
			return nil, "", fmt.Errorf("scan version of document %s: %w", slug, err)
		}
		versions = append(versions, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("list versions of document %s: %w", slug, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, "", fmt.Errorf("commit list document versions: %w", err)
	}
	return versions, current, nil
}

// StoredJob is a jobs row as JobsUsingCatalogs reads it. A NULL config is nil,
// which jobconfig.ExtractCatalogs cannot read.
type StoredJob struct {
	Config []byte
	Status string
}

// JobUse is a job that uses a catalog, with its stored jobs.status.
type JobUse struct {
	JobID  string
	Status string
}

// terminalJobStatuses are the jobs.status values after which a job no longer
// reads its catalogs. They mirror pipeline.JobStatusCompleted, Failed and
// Cancelled, which this package may not import (AGENTS.md import rules). Any
// other status, including one this list does not know, counts as in flight.
var terminalJobStatuses = map[string]bool{"completed": true, "failed": true, "cancelled": true}

// JobsUsingCatalogs reads every stored job config of the context tenant and
// returns the jobs that use each of catalogIDs (see jobsUsingCatalogs). It
// reads all of the tenant's jobs, because which catalogs a job uses is
// decided by jobconfig.ExtractCatalogs, not by a SQL filter.
func (s *PGStore) JobsUsingCatalogs(ctx context.Context, catalogIDs []string) (map[string][]JobUse, []string, error) {
	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("list jobs using catalogs: %w", err)
	}

	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("begin list jobs using catalogs: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op

	rows, err := tx.Query(ctx,
		"SELECT job_id, status, config FROM jobs WHERE tenant_id = $1", tenantID)
	if err != nil {
		return nil, nil, fmt.Errorf("list job configs: %w", err)
	}
	defer rows.Close()
	jobs := map[string]StoredJob{}
	for rows.Next() {
		var jobID string
		var job StoredJob
		if err := rows.Scan(&jobID, &job.Status, &job.Config); err != nil {
			return nil, nil, fmt.Errorf("scan job config: %w", err)
		}
		jobs[jobID] = job
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("list job configs: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit list jobs using catalogs: %w", err)
	}
	uses, unreadable := jobsUsingCatalogs(jobs, catalogIDs)
	return uses, unreadable, nil
}

// jobsUsingCatalogs maps each of catalogIDs that a job in jobs (job ID to
// stored row) uses, as source or target, to those jobs sorted by ID, and
// returns the sorted IDs of the jobs whose config jobconfig.ExtractCatalogs
// cannot read.
func jobsUsingCatalogs(jobs map[string]StoredJob, catalogIDs []string) (uses map[string][]JobUse, unreadable []string) {
	wanted := map[string]bool{}
	for _, id := range catalogIDs {
		wanted[id] = true
	}
	jobIDs := make([]string, 0, len(jobs))
	for id := range jobs {
		jobIDs = append(jobIDs, id)
	}
	slices.Sort(jobIDs)
	uses = map[string][]JobUse{}
	for _, jobID := range jobIDs {
		cats, ok := jobconfig.ExtractCatalogs(jobs[jobID].Config)
		if !ok {
			unreadable = append(unreadable, jobID)
			continue
		}
		for _, catalogID := range cats.CatalogIDs() {
			if wanted[catalogID] {
				uses[catalogID] = append(uses[catalogID], JobUse{JobID: jobID, Status: jobs[jobID].Status})
			}
		}
	}
	return uses, unreadable
}

// ListControlsForCatalog returns the controls of a catalog of the context
// tenant, ordered by control ID.
func (s *PGStore) ListControlsForCatalog(ctx context.Context, catalogID string) ([]ControlRecord, error) {
	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("list controls for catalog: %w", err)
	}

	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin list controls for catalog: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op

	rows, err := tx.Query(ctx, `
		SELECT tenant_id, control_id, catalog_id, identifier, title, statement,
			class, parent_id, group_id, COALESCE(container_key, ''), props, created_at
		FROM controls
		WHERE catalog_id = $1 AND tenant_id = $2
		ORDER BY control_id
	`, catalogID, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list controls for catalog %s: %w", catalogID, err)
	}
	defer rows.Close()

	var records []ControlRecord
	for rows.Next() {
		var rec ControlRecord
		var propsJSON []byte
		if err := rows.Scan(&rec.TenantID, &rec.ControlID, &rec.CatalogID, &rec.Identifier,
			&rec.Title, &rec.Statement, &rec.Class, &rec.ParentID, &rec.GroupID,
			&rec.ContainerKey, &propsJSON, &rec.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan control of catalog %s: %w", catalogID, err)
		}
		if len(propsJSON) > 0 {
			if err := json.Unmarshal(propsJSON, &rec.Props); err != nil {
				return nil, fmt.Errorf("unmarshal props for control %s: %w", rec.ControlID, err)
			}
		}
		records = append(records, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list controls for catalog %s: %w", catalogID, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit list controls for catalog: %w", err)
	}
	return records, nil
}

// GetControl retrieves a control of the context tenant by ID.
func (s *PGStore) GetControl(ctx context.Context, controlID string) (*ControlRecord, error) {
	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("get control: %w", err)
	}

	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin get control: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op

	query := `
		SELECT
			tenant_id, control_id, catalog_id, identifier, title, statement,
			class, parent_id, group_id, COALESCE(container_key, ''), props, created_at
		FROM controls
		WHERE control_id = $1 AND tenant_id = $2
	`

	var rec ControlRecord
	var propsJSON []byte

	err = tx.QueryRow(ctx, query, controlID, tenantID).Scan(
		&rec.TenantID,
		&rec.ControlID,
		&rec.CatalogID,
		&rec.Identifier,
		&rec.Title,
		&rec.Statement,
		&rec.Class,
		&rec.ParentID,
		&rec.GroupID,
		&rec.ContainerKey,
		&propsJSON,
		&rec.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrControlNotFound, controlID)
	}
	if err != nil {
		return nil, err
	}

	if len(propsJSON) > 0 {
		if err := json.Unmarshal(propsJSON, &rec.Props); err != nil {
			return nil, fmt.Errorf("unmarshal props for control %s: %w", controlID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit get control: %w", err)
	}
	return &rec, nil
}

// SearchControls performs full-text search on the context tenant's controls.
func (s *PGStore) SearchControls(ctx context.Context, sq SearchQuery) ([]ControlRecord, PageInfo, error) {
	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		return nil, PageInfo{}, fmt.Errorf("search controls: %w", err)
	}

	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return nil, PageInfo{}, fmt.Errorf("begin search controls: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op

	limit := sq.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 1000 {
		limit = 1000
	}

	// Build WHERE clause based on query parameters
	whereClause := "WHERE search_vector @@ plainto_tsquery('english', $1)"
	args := []interface{}{sq.Query}
	argIndex := 2

	if len(sq.CatalogIDs) > 0 {
		placeholders := ""
		for i, catalogID := range sq.CatalogIDs {
			if i > 0 {
				placeholders += ", "
			}
			placeholders += fmt.Sprintf("$%d", argIndex)
			args = append(args, catalogID)
			argIndex++
		}
		whereClause += fmt.Sprintf(" AND catalog_id IN (%s)", placeholders)
	}
	whereClause += fmt.Sprintf(" AND tenant_id = $%d", argIndex)
	args = append(args, tenantID)
	argIndex++

	// Get total count
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM controls %s", whereClause)
	var totalCount int64
	if err := tx.QueryRow(ctx, countQuery, args...).Scan(&totalCount); err != nil {
		return nil, PageInfo{}, err
	}

	// Get ranked results
	searchQuery := fmt.Sprintf(`
		SELECT
			tenant_id, control_id, catalog_id, identifier, title, statement,
			class, parent_id, group_id, COALESCE(container_key, ''), props, created_at
		FROM controls
		%s
		ORDER BY ts_rank_cd(search_vector, plainto_tsquery('english', $1)) DESC
		LIMIT $%d OFFSET $%d
	`, whereClause, argIndex, argIndex+1)

	args = append(args, limit, sq.Offset)

	rows, err := tx.Query(ctx, searchQuery, args...)
	if err != nil {
		return nil, PageInfo{}, err
	}
	defer rows.Close()

	var records []ControlRecord
	for rows.Next() {
		var rec ControlRecord
		var propsJSON []byte

		err := rows.Scan(
			&rec.TenantID,
			&rec.ControlID,
			&rec.CatalogID,
			&rec.Identifier,
			&rec.Title,
			&rec.Statement,
			&rec.Class,
			&rec.ParentID,
			&rec.GroupID,
			&rec.ContainerKey,
			&propsJSON,
			&rec.CreatedAt,
		)
		if err != nil {
			return nil, PageInfo{}, err
		}

		if len(propsJSON) > 0 {
			if err := json.Unmarshal(propsJSON, &rec.Props); err != nil {
				return nil, PageInfo{}, fmt.Errorf("unmarshal props for control %s: %w", rec.ControlID, err)
			}
		}

		records = append(records, rec)
	}

	if err := rows.Err(); err != nil {
		return nil, PageInfo{}, err
	}

	if err := tx.Commit(); err != nil {
		return nil, PageInfo{}, fmt.Errorf("commit search controls: %w", err)
	}

	nextOffset := sq.Offset + len(records)
	pageInfo := PageInfo{
		NextOffset: nextOffset,
		TotalCount: totalCount,
	}

	return records, pageInfo, nil
}

// Compile-time interface check.
var _ Store = (*PGStore)(nil)
