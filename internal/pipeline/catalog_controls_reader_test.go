package pipeline_test

import (
	"context"
	"testing"

	"github.com/complytime-labs/crosscodex/internal/pipeline"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

func TestPGCatalogControlsReader_ReturnsControlsOrderedByID_WithAncestorTitleAndClass(t *testing.T) {
	// Query column order matches pgCatalogControlsReader.Controls's SELECT:
	// control_id, identifier, title, statement, class, parent_id, props.
	mockTx := &mockTransaction{
		queryFunc: func(ctx context.Context, query string, args ...any) (db.Rows, error) {
			return &mockRows{
				rows: [][]any{
					{"AC-1", "ac-1", "Access Control", "stmt-1", "", "", []byte(`{}`)},
					{"AC-1_smt.a", "ac-1.a", "", "stmt-1a", "", "AC-1", []byte(`{}`)},
					{"SECTION-1", "sec-1", "Overview", "", "compliance-section", "", []byte(`{}`)},
				},
				cursor: 0,
			}, nil
		},
	}
	mockDB := &mockTenantConnection{beginFunc: func(ctx context.Context) (db.Transaction, error) { return mockTx, nil }}
	reader := pipeline.NewPGCatalogControlsReader(mockDB)

	ctx, err := tenant.WithTenant(context.Background(), "tenant-a")
	if err != nil {
		t.Fatalf("tenant.WithTenant: %v", err)
	}

	controls, err := reader.Controls(ctx, "tenant-a", "cat-1")
	if err != nil {
		t.Fatalf("Controls: %v", err)
	}
	if len(controls) != 3 {
		t.Fatalf("expected 3 controls, got %d", len(controls))
	}

	if got := controls[0].GetControlId(); got != "AC-1" {
		t.Errorf("controls[0].ControlId = %q, want AC-1", got)
	}
	if got := controls[0].GetIdentifier(); got != "ac-1" {
		t.Errorf("controls[0].Identifier = %q, want ac-1", got)
	}
	if got := controls[1].GetControlId(); got != "AC-1_smt.a" {
		t.Errorf("controls[1].ControlId = %q, want AC-1_smt.a", got)
	}
	if got := controls[2].GetControlId(); got != "SECTION-1" {
		t.Errorf("controls[2].ControlId = %q, want SECTION-1", got)
	}

	if got := controls[1].GetParts()["ancestor_title"]; got != "Access Control" {
		t.Errorf("controls[1].Parts[ancestor_title] = %q, want %q", got, "Access Control")
	}
	if got := controls[2].GetParts()["class"]; got != "compliance-section" {
		t.Errorf("controls[2].Parts[class] = %q, want compliance-section", got)
	}
}

func TestPGCatalogControlsReader_ValidatesTenantID(t *testing.T) {
	reader := pipeline.NewPGCatalogControlsReader(&mockTenantConnection{})
	_, err := reader.Controls(context.Background(), "BAD TENANT", "cat-1")
	if err == nil {
		t.Fatal("expected error for invalid tenant ID")
	}
}
