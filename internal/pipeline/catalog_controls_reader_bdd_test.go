package pipeline_test

import (
	"context"
	"errors"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/pipeline"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

var _ = Describe("PGCatalogControlsReader", func() {
	It("returns controls ordered by ID with context and class", func() {
		// Query column order matches pgCatalogControlsReader.Controls's SELECT:
		// control_id, identifier, title, statement, class, parent_id, container_key, props.
		mockTx := &dbtest.Tx{
			QueryFunc: func(ctx context.Context, query string, args ...any) (db.Rows, error) {
				if strings.Contains(query, "catalog_containers") {
					return dbtest.Rows(nil), nil
				}
				return dbtest.Rows(nil,
					[]any{"cat-1/AC-1", "AC-1", "Access Control", "stmt-1", "", "", "", []byte(`{}`)},
					[]any{"cat-1/AC-1_smt.a", "AC-1_smt.a", "", "stmt-1a", "", "AC-1", "", []byte(`{}`)},
					[]any{"cat-1/SECTION-1", "SECTION-1", "Overview", "", "compliance-section", "", "", []byte(`{}`)},
				), nil
			},
		}
		mockDB := &dbtest.Conn{Tx: mockTx}
		reader := pipeline.NewPGCatalogControlsReader(mockDB)

		ctx, err := tenant.WithTenant(context.Background(), "tenant-a")
		Expect(err).NotTo(HaveOccurred())

		controls, err := reader.Controls(ctx, "tenant-a", "cat-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(controls).To(HaveLen(3))

		Expect(controls[0].GetControlId()).To(Equal("cat-1/AC-1"))
		Expect(controls[0].GetIdentifier()).To(Equal("AC-1"))
		Expect(controls[1].GetControlId()).To(Equal("cat-1/AC-1_smt.a"))
		Expect(controls[2].GetControlId()).To(Equal("cat-1/SECTION-1"))

		Expect(controls[1].GetParts()["context"]).To(Equal("Access Control (AC-1)"))
		Expect(controls[2].GetParts()["class"]).To(Equal("compliance-section"))
	})

	It("gives no context to a row whose parent_id matches no control", func() {
		mockTx := &dbtest.Tx{
			QueryFunc: func(ctx context.Context, query string, args ...any) (db.Rows, error) {
				if strings.Contains(query, "catalog_containers") {
					return dbtest.Rows(nil), nil
				}
				return dbtest.Rows(nil,
					[]any{"cat-1/AC-1", "AC-1", "Access Control", "stmt", "", "", "", []byte(`{}`)},
					[]any{"cat-1/X-1", "X-1", "", "stmt", "", "cat-1/AC-1", "", []byte(`{}`)},
				), nil
			},
		}
		reader := pipeline.NewPGCatalogControlsReader(&dbtest.Conn{Tx: mockTx})
		ctx, err := tenant.WithTenant(context.Background(), "tenant-a")
		Expect(err).NotTo(HaveOccurred())

		controls, err := reader.Controls(ctx, "tenant-a", "cat-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(controls[0].GetParts()).NotTo(HaveKey("context"), "a root has no ancestor")
		Expect(controls[1].GetParts()).NotTo(HaveKey("context"), "parent_id is stored raw, so a catalog-prefixed value is unknown")
	})

	It("continues a control's context into its container and never emits a container", func() {
		var containerArgs []any
		mockTx := &dbtest.Tx{
			QueryFunc: func(ctx context.Context, query string, args ...any) (db.Rows, error) {
				if strings.Contains(query, "catalog_containers") {
					containerArgs = args
					// Columns: container_key, parent_key, title, identifier, label.
					return dbtest.Rows(nil, []any{"g1", "", "Access Control", "", ""}), nil
				}
				return dbtest.Rows(nil,
					[]any{"cat-1/AC-1", "AC-1", "Policy", "stmt", "", "", "g1", []byte(`{}`)},
					[]any{"cat-1/AC-1.a", "AC-1.a", "", "stmt-a", "", "AC-1", "g1", []byte(`{}`)},
					[]any{"cat-1/X-1", "X-1", "", "stmt", "", "", "g9", []byte(`{}`)},
					[]any{"cat-1/Y-1", "Y-1", "", "stmt", "", "", "", []byte(`{}`)},
				), nil
			},
		}
		reader := pipeline.NewPGCatalogControlsReader(&dbtest.Conn{Tx: mockTx})
		ctx, err := tenant.WithTenant(context.Background(), "tenant-a")
		Expect(err).NotTo(HaveOccurred())

		controls, err := reader.Controls(ctx, "tenant-a", "cat-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(containerArgs).To(Equal([]any{"tenant-a", "cat-1"}), "containers are read tenant- and catalog-scoped")
		Expect(controls).To(HaveLen(4), "only controls are emitted, never containers")
		Expect(controls[0].GetParts()["context"]).To(Equal("Access Control"))
		Expect(controls[1].GetParts()["context"]).To(Equal("Access Control > Policy (AC-1)"), "a part's context runs through its parent item")
		Expect(controls[2].GetParts()).NotTo(HaveKey("context"), "an unknown container key yields no ancestor")
		Expect(controls[3].GetParts()).NotTo(HaveKey("context"))
	})

	It("returns an error when the containers query fails", func() {
		mockTx := &dbtest.Tx{
			QueryFunc: func(ctx context.Context, query string, args ...any) (db.Rows, error) {
				if strings.Contains(query, "catalog_containers") {
					return nil, errors.New("boom")
				}
				return dbtest.Rows(nil), nil
			},
		}
		reader := pipeline.NewPGCatalogControlsReader(&dbtest.Conn{Tx: mockTx})
		ctx, err := tenant.WithTenant(context.Background(), "tenant-a")
		Expect(err).NotTo(HaveOccurred())
		_, err = reader.Controls(ctx, "tenant-a", "cat-1")
		Expect(err).To(MatchError(ContainSubstring("querying containers")))
	})

	It("validates the tenant ID", func() {
		conn := &dbtest.Conn{}
		reader := pipeline.NewPGCatalogControlsReader(conn)
		_, err := reader.Controls(context.Background(), "BAD TENANT", "cat-1")
		Expect(err).To(HaveOccurred(), "expected error for invalid tenant ID")
		Expect(conn.Begins()).To(BeZero(), "the tenant must be refused before any transaction starts")
	})
})
