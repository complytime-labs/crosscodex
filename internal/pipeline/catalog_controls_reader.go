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
// Parts["ancestor_title"] set from the parent control's title (empty when no
// parent). Props (arbitrary control metadata) are merged into Parts first so
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
		SELECT control_id, identifier, COALESCE(title, ''), COALESCE(statement, ''), COALESCE(class, ''), COALESCE(parent_id, ''), props
		FROM controls
		WHERE tenant_id = $1 AND catalog_id = $2
		ORDER BY control_id
	`, tenantID, catalogID)
	if err != nil {
		return nil, fmt.Errorf("pgCatalogControlsReader.Controls: querying controls: %w", err)
	}
	defer rows.Close()

	type row struct {
		controlID, identifier, title, statement, class, parentID string
		propsJSON                                                []byte
	}
	var raw []row
	titleByID := make(map[string]string)
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.controlID, &r.identifier, &r.title, &r.statement, &r.class, &r.parentID, &r.propsJSON); err != nil {
			return nil, fmt.Errorf("pgCatalogControlsReader.Controls: scanning row: %w", err)
		}
		titleByID[r.controlID] = r.title
		raw = append(raw, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pgCatalogControlsReader.Controls: rows error: %w", err)
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
		if ancestorTitle, ok := titleByID[r.parentID]; ok {
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
