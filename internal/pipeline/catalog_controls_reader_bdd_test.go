package pipeline_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/pipeline"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

var _ = Describe("PGCatalogControlsReader", func() {
	It("returns controls ordered by ID with ancestor title and class", func() {
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
		Expect(err).NotTo(HaveOccurred())

		controls, err := reader.Controls(ctx, "tenant-a", "cat-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(controls).To(HaveLen(3))

		Expect(controls[0].GetControlId()).To(Equal("AC-1"))
		Expect(controls[0].GetIdentifier()).To(Equal("ac-1"))
		Expect(controls[1].GetControlId()).To(Equal("AC-1_smt.a"))
		Expect(controls[2].GetControlId()).To(Equal("SECTION-1"))

		Expect(controls[1].GetParts()["ancestor_title"]).To(Equal("Access Control"))
		Expect(controls[2].GetParts()["class"]).To(Equal("compliance-section"))
	})

	It("validates the tenant ID", func() {
		reader := pipeline.NewPGCatalogControlsReader(&mockTenantConnection{})
		_, err := reader.Controls(context.Background(), "BAD TENANT", "cat-1")
		Expect(err).To(HaveOccurred(), "expected error for invalid tenant ID")
	})
})
