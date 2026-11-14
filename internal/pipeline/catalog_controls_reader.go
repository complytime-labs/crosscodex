package pipeline

import (
	"context"
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
// determinism, composed by composeControls: Parts carries the props, "class"
// and "context", and Title and Statement are the analysis input. Containers
// supply context but are never emitted.
func (r *pgCatalogControlsReader) Controls(ctx context.Context, tenantID, catalogID string) ([]*pb.Control, error) {
	if err := tenant.ValidateTenantID(tenantID); err != nil {
		return nil, fmt.Errorf("pgCatalogControlsReader.Controls: %w", err)
	}

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("pgCatalogControlsReader.Controls: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, containers, err := readCatalog(ctx, tx, tenantID, catalogID)
	if err != nil {
		return nil, fmt.Errorf("pgCatalogControlsReader.Controls: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("pgCatalogControlsReader.Controls: commit failed: %w", err)
	}
	return composeControls(catalogID, rows, containers), nil
}
