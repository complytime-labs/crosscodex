package pipeline

import (
	"context"
	"encoding/json"
	"fmt"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// CatalogControlsReader reads every control in a catalog (sections included)
// as *pb.Control, for feeding analysis.ExecutionRequest.Controls. Distinct
// from ControlsReader (candidate_generation.go), which excludes sections and
// returns a different shape for candidate generation.
type CatalogControlsReader interface {
	Controls(ctx context.Context, tenantID, catalogID string) ([]*pb.Control, error)
}

type pgCatalogControlsReader struct{ db db.TenantConnection }

// NewPGCatalogControlsReader constructs a CatalogControlsReader backed by Postgres.
func NewPGCatalogControlsReader(conn db.TenantConnection) CatalogControlsReader {
	return &pgCatalogControlsReader{db: conn}
}

// Controls reads all of a catalog's controls, ordered by control_id for
// determinism, with Parts["class"] set from the class column and
// Parts["ancestor_title"] set from the parent control's title (absent when no
// parent). parent_id is stored raw by import while control_id is
// "<catalog_id>/<identifier>", so the parent is looked up as
// catalogID + "/" + parent_id. A top-level control (empty parent_id) takes
// the title of its container (controls.container_key); containers are never
// emitted. Props (arbitrary control metadata) are merged into Parts first so
// class/ancestor_title always win on key collision.
func (r *pgCatalogControlsReader) Controls(ctx context.Context, tenantID, catalogID string) ([]*pb.Control, error) {
	if err := tenant.ValidateTenantID(tenantID); err != nil {
		return nil, fmt.Errorf("pgCatalogControlsReader.Controls: %w", err)
	}

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("pgCatalogControlsReader.Controls: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.Query(ctx, `
		SELECT control_id, identifier, COALESCE(title, ''), COALESCE(statement, ''), COALESCE(class, ''), COALESCE(parent_id, ''), COALESCE(container_key, ''), props
		FROM controls
		WHERE tenant_id = $1 AND catalog_id = $2
		ORDER BY control_id
	`, tenantID, catalogID)
	if err != nil {
		return nil, fmt.Errorf("pgCatalogControlsReader.Controls: querying controls: %w", err)
	}
	defer rows.Close()

	type row struct {
		controlID, identifier, title, statement, class, parentID, containerKey string
		propsJSON                                                              []byte
	}
	var raw []row
	titleByID := make(map[string]string)
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.controlID, &r.identifier, &r.title, &r.statement, &r.class, &r.parentID, &r.containerKey, &r.propsJSON); err != nil {
			return nil, fmt.Errorf("pgCatalogControlsReader.Controls: scanning row: %w", err)
		}
		titleByID[r.controlID] = r.title
		raw = append(raw, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pgCatalogControlsReader.Controls: rows error: %w", err)
	}
	rows.Close()

	containerTitles, err := readContainerTitles(ctx, tx, tenantID, catalogID)
	if err != nil {
		return nil, fmt.Errorf("pgCatalogControlsReader.Controls: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("pgCatalogControlsReader.Controls: commit failed: %w", err)
	}

	controls := make([]*pb.Control, len(raw))
	for i, r := range raw {
		parts := make(map[string]string)
		if len(r.propsJSON) > 0 {
			if err := json.Unmarshal(r.propsJSON, &parts); err != nil {
				return nil, fmt.Errorf("pgCatalogControlsReader.Controls: unmarshaling props for %s: %w", r.controlID, err)
			}
		}
		parts["class"] = r.class
		if ancestorTitle, ok := ancestorTitleOf(catalogID, r.parentID, r.containerKey, titleByID, containerTitles); ok {
			parts["ancestor_title"] = ancestorTitle
		}
		controls[i] = &pb.Control{
			ControlId:  r.controlID,
			CatalogId:  catalogID,
			Identifier: r.identifier,
			Title:      r.title,
			Statement:  r.statement,
			Parts:      parts,
		}
	}
	return controls, nil
}

// readContainerTitles maps container_key to title for a catalog, within the
// caller's tenant-scoped transaction. Containers are context for ancestor
// lookup only; they never become controls.
func readContainerTitles(ctx context.Context, tx db.Transaction, tenantID, catalogID string) (map[string]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT container_key, title
		FROM catalog_containers
		WHERE tenant_id = $1 AND catalog_id = $2
	`, tenantID, catalogID)
	if err != nil {
		return nil, fmt.Errorf("querying containers: %w", err)
	}
	defer rows.Close()

	titles := make(map[string]string)
	for rows.Next() {
		var key, title string
		if err := rows.Scan(&key, &title); err != nil {
			return nil, fmt.Errorf("scanning container row: %w", err)
		}
		titles[key] = title
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("container rows error: %w", err)
	}
	return titles, nil
}

// ancestorTitleOf resolves a control's ancestor title: its parent item's title
// when parentID is set, else its container's title when containerKey is set.
// ok is false when neither is known. parent_id is stored raw while control_id
// is "<catalog_id>/<identifier>", so a parent is looked up as
// catalogID + "/" + parentID.
func ancestorTitleOf(catalogID, parentID, containerKey string, titleByID, containerTitles map[string]string) (title string, ok bool) {
	if parentID != "" {
		title, ok = titleByID[catalogID+"/"+parentID]
		return title, ok
	}
	if containerKey != "" {
		title, ok = containerTitles[containerKey]
		return title, ok
	}
	return "", false
}
