package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/complytime-labs/crosscodex/pkg/db"
)

// Store interface for catalog and control persistence.
type Store interface {
	UpsertCatalog(ctx context.Context, catalog CatalogRecord) error
	GetCatalog(ctx context.Context, catalogID string) (*CatalogRecord, error)
	ListCatalogs(ctx context.Context, opts ListOptions) ([]CatalogRecord, PageInfo, error)
	UpsertControls(ctx context.Context, controls []ControlRecord) error
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
	Props      map[string]string
	CreatedAt  time.Time
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

// UpsertCatalog inserts or updates a catalog record.
func (s *PGStore) UpsertCatalog(ctx context.Context, catalog CatalogRecord) error {
	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin upsert catalog: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op

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
	`

	if err := tx.Exec(ctx, query,
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
	); err != nil {
		return err
	}

	return tx.Commit()
}

// GetCatalog retrieves a catalog by ID.
func (s *PGStore) GetCatalog(ctx context.Context, catalogID string) (*CatalogRecord, error) {
	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin get catalog: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op

	query := `
		SELECT
			catalog_id, tenant_id, name, version, source_type, object_path, created_at,
			COALESCE(source_uri, '') as source_uri,
			COALESCE(content_hash, '') as content_hash,
			COALESCE(content_size, 0) as content_size,
			COALESCE(format, '') as format,
			COALESCE(output_hash, '') as output_hash,
			COALESCE(extractor_name, '') as extractor_name,
			COALESCE(extractor_version, '') as extractor_version
		FROM catalogs
		WHERE catalog_id = $1
	`

	var rec CatalogRecord
	err = tx.QueryRow(ctx, query, catalogID).Scan(
		&rec.CatalogID,
		&rec.TenantID,
		&rec.Name,
		&rec.Version,
		&rec.SourceType,
		&rec.ObjectPath,
		&rec.CreatedAt,
		&rec.SourceURI,
		&rec.ContentHash,
		&rec.ContentSize,
		&rec.Format,
		&rec.OutputHash,
		&rec.ExtractorName,
		&rec.ExtractorVersion,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("catalog not found: %s", catalogID)
	}
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit get catalog: %w", err)
	}
	return &rec, nil
}

// ListCatalogs retrieves catalogs with pagination.
func (s *PGStore) ListCatalogs(ctx context.Context, opts ListOptions) ([]CatalogRecord, PageInfo, error) {
	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return nil, PageInfo{}, fmt.Errorf("begin list catalogs: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op

	limit := opts.EffectiveLimit()

	// Get total count
	var totalCount int64
	if err := tx.QueryRow(ctx, "SELECT COUNT(*) FROM catalogs").Scan(&totalCount); err != nil {
		return nil, PageInfo{}, err
	}

	// Get paginated results
	query := `
		SELECT
			catalog_id, tenant_id, name, version, source_type, object_path, created_at,
			COALESCE(source_uri, '') as source_uri,
			COALESCE(content_hash, '') as content_hash,
			COALESCE(content_size, 0) as content_size,
			COALESCE(format, '') as format,
			COALESCE(output_hash, '') as output_hash,
			COALESCE(extractor_name, '') as extractor_name,
			COALESCE(extractor_version, '') as extractor_version
		FROM catalogs
		ORDER BY created_at DESC
		LIMIT $1 OFFSET $2
	`

	rows, err := tx.Query(ctx, query, limit, opts.Offset)
	if err != nil {
		return nil, PageInfo{}, err
	}
	defer rows.Close()

	var records []CatalogRecord
	for rows.Next() {
		var rec CatalogRecord
		err := rows.Scan(
			&rec.CatalogID,
			&rec.TenantID,
			&rec.Name,
			&rec.Version,
			&rec.SourceType,
			&rec.ObjectPath,
			&rec.CreatedAt,
			&rec.SourceURI,
			&rec.ContentHash,
			&rec.ContentSize,
			&rec.Format,
			&rec.OutputHash,
			&rec.ExtractorName,
			&rec.ExtractorVersion,
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

// UpsertControls inserts or updates multiple control records in a transaction.
func (s *PGStore) UpsertControls(ctx context.Context, controls []ControlRecord) error {
	if len(controls) == 0 {
		return nil
	}

	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin upsert controls: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op

	query := `
		INSERT INTO controls (
			tenant_id, control_id, catalog_id, identifier, title, statement,
			class, parent_id, group_id, props, created_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11
		)
		ON CONFLICT (tenant_id, control_id) DO UPDATE SET
			catalog_id = EXCLUDED.catalog_id,
			identifier = EXCLUDED.identifier,
			title = EXCLUDED.title,
			statement = EXCLUDED.statement,
			class = EXCLUDED.class,
			parent_id = EXCLUDED.parent_id,
			group_id = EXCLUDED.group_id,
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
			propsJSON,
			ctrl.CreatedAt,
		); err != nil {
			return fmt.Errorf("upsert control %s: %w", ctrl.ControlID, err)
		}
	}

	return tx.Commit()
}

// GetControl retrieves a control by ID.
func (s *PGStore) GetControl(ctx context.Context, controlID string) (*ControlRecord, error) {
	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin get control: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op

	query := `
		SELECT
			tenant_id, control_id, catalog_id, identifier, title, statement,
			class, parent_id, group_id, props, created_at
		FROM controls
		WHERE control_id = $1
	`

	var rec ControlRecord
	var propsJSON []byte

	err = tx.QueryRow(ctx, query, controlID).Scan(
		&rec.TenantID,
		&rec.ControlID,
		&rec.CatalogID,
		&rec.Identifier,
		&rec.Title,
		&rec.Statement,
		&rec.Class,
		&rec.ParentID,
		&rec.GroupID,
		&propsJSON,
		&rec.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("control not found: %s", controlID)
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

// SearchControls performs full-text search on controls.
func (s *PGStore) SearchControls(ctx context.Context, sq SearchQuery) ([]ControlRecord, PageInfo, error) {
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
			class, parent_id, group_id, props, created_at
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
