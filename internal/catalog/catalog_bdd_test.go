//go:build !integration

package catalog_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	crosscodexv1 "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/catalog"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
	"github.com/complytime-labs/crosscodex/pkg/oscal"
	"github.com/complytime-labs/crosscodex/pkg/storage/storagetest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
	"github.com/complytime-labs/crosscodex/pkg/vectordb"
)

func TestCatalogBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Catalog BDD Suite")
}

var _ = BeforeSuite(func() { DeferCleanup(testspecs.RedirectLogsToGinkgo()) })

// ---------------------------------------------------------------------------
// Compile-time interface check
// ---------------------------------------------------------------------------

var _ catalog.Store = (*catalog.PGStore)(nil)

// ---------------------------------------------------------------------------
// Mock types for service unit testing
// ---------------------------------------------------------------------------

type mockParser struct {
	items []oscal.ControlItem
	err   error
}

func (m *mockParser) Parse(_ context.Context, _ io.Reader) ([]oscal.ControlItem, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.items, nil
}

func (m *mockParser) FindControl(items []oscal.ControlItem, controlID string) (*oscal.ControlItem, error) {
	for i := range items {
		if items[i].ID == controlID {
			return &items[i], nil
		}
	}
	return nil, oscal.ErrControlNotFound
}

type mockStore struct {
	catalogRecords   map[string]*catalog.CatalogRecord
	controlRecords   map[string]*catalog.ControlRecord
	upsertCatalogFn  func(context.Context, catalog.CatalogRecord) error
	upsertControlsFn func(context.Context, []catalog.ControlRecord) error
}

func newMockStore() *mockStore {
	return &mockStore{
		catalogRecords: make(map[string]*catalog.CatalogRecord),
		controlRecords: make(map[string]*catalog.ControlRecord),
	}
}

func (m *mockStore) UpsertCatalog(ctx context.Context, cat catalog.CatalogRecord) error {
	if m.upsertCatalogFn != nil {
		return m.upsertCatalogFn(ctx, cat)
	}
	m.catalogRecords[cat.CatalogID] = &cat
	return nil
}

func (m *mockStore) GetCatalog(_ context.Context, catalogID string) (*catalog.CatalogRecord, error) {
	rec, ok := m.catalogRecords[catalogID]
	if !ok {
		return nil, nil
	}
	return rec, nil
}

func (m *mockStore) ListCatalogs(_ context.Context, _ catalog.ListOptions) ([]catalog.CatalogRecord, catalog.PageInfo, error) {
	return nil, catalog.PageInfo{}, nil
}

func (m *mockStore) UpsertControls(ctx context.Context, controls []catalog.ControlRecord) error {
	if m.upsertControlsFn != nil {
		return m.upsertControlsFn(ctx, controls)
	}
	for _, ctrl := range controls {
		m.controlRecords[ctrl.ControlID] = &ctrl
	}
	return nil
}

func (m *mockStore) GetControl(_ context.Context, controlID string) (*catalog.ControlRecord, error) {
	rec, ok := m.controlRecords[controlID]
	if !ok {
		return nil, nil
	}
	return rec, nil
}

func (m *mockStore) SearchControls(_ context.Context, _ catalog.SearchQuery) ([]catalog.ControlRecord, catalog.PageInfo, error) {
	return nil, catalog.PageInfo{}, nil
}

// ===========================================================================
// Store Specs (from store_test.go)
// ===========================================================================

var _ = Describe("ListOptions", func() {
	DescribeTable("EffectiveLimit",
		func(limit, expected int) {
			opts := catalog.ListOptions{Limit: limit}
			Expect(opts.EffectiveLimit()).To(Equal(expected))
		},
		Entry("zero returns default 50", 0, 50),
		Entry("negative returns default 50", -10, 50),
		Entry("within range returns same", 100, 100),
		Entry("max at 1000 returns 1000", 1000, 1000),
		Entry("above 1000 caps at 1000", 5000, 1000),
		Entry("just above 1000 caps at 1000", 1001, 1000),
	)
})

const storeTenant = "store-unit"

var storeCtx = func() context.Context {
	ctx, err := tenant.WithTenant(context.Background(), storeTenant)
	if err != nil {
		panic(err)
	}
	return ctx
}()

var _ = Describe("PGStore", func() {
	Describe("UpsertCatalog", func() {
		It("returns error when Begin fails", func() {
			beginErr := errors.New("connection refused")
			conn := &dbtest.Conn{
				BeginFunc: func(context.Context) (db.Transaction, error) {
					return nil, beginErr
				},
			}
			store := catalog.NewPGStore(conn)

			err := store.UpsertCatalog(storeCtx, catalog.CatalogRecord{CatalogID: "test", TenantID: storeTenant})
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, beginErr)).To(BeTrue())
		})

		It("commits on success", func() {
			// RETURNING catalog_id yields the row: the catalog is this tenant's.
			tx := &dbtest.Tx{
				QueryRowFunc: func(context.Context, string, ...any) db.Row { return dbtest.Row(nil, "cat-1") },
			}
			conn := &dbtest.Conn{Tx: tx}
			store := catalog.NewPGStore(conn)

			err := store.UpsertCatalog(storeCtx, catalog.CatalogRecord{CatalogID: "cat-1", TenantID: storeTenant})
			Expect(err).NotTo(HaveOccurred())
			Expect(tx.Commits()).To(Equal(1))
		})

		It("does not commit when the upsert fails", func() {
			upsertErr := errors.New("unique violation")
			tx := &dbtest.Tx{
				QueryRowFunc: func(context.Context, string, ...any) db.Row { return dbtest.Row(upsertErr) },
			}
			conn := &dbtest.Conn{Tx: tx}
			store := catalog.NewPGStore(conn)

			err := store.UpsertCatalog(storeCtx, catalog.CatalogRecord{CatalogID: "cat-1", TenantID: storeTenant})
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, upsertErr)).To(BeTrue())
			Expect(tx.Commits()).To(Equal(0))
		})

		It("refuses and does not commit when the catalog ID belongs to another tenant", func() {
			// ON CONFLICT ... WHERE catalogs.tenant_id = EXCLUDED.tenant_id
			// updates nothing for another tenant's row, so RETURNING is empty.
			tx := &dbtest.Tx{
				QueryRowFunc: func(context.Context, string, ...any) db.Row { return dbtest.Row(sql.ErrNoRows) },
			}
			conn := &dbtest.Conn{Tx: tx}
			store := catalog.NewPGStore(conn)

			err := store.UpsertCatalog(storeCtx, catalog.CatalogRecord{CatalogID: "cat-1", TenantID: storeTenant})
			Expect(err).To(MatchError(ContainSubstring("belongs to another tenant")))
			Expect(err).To(MatchError(ContainSubstring("nothing was written")))
			Expect(tx.Commits()).To(Equal(0))
		})
	})

	Describe("tenant scoping", func() {
		// Begin must never run: a store call without a tenant, or with a
		// record for another tenant, is refused before touching the database.
		noBegin := func() *dbtest.Conn {
			return &dbtest.Conn{BeginFunc: func(context.Context) (db.Transaction, error) {
				Fail("Begin must not be called")
				return nil, nil
			}}
		}

		DescribeTable("refuses every method when the context has no tenant",
			func(call func(catalog.Store, context.Context) error) {
				err := call(catalog.NewPGStore(noBegin()), context.Background())
				Expect(errors.Is(err, tenant.ErrNoTenant)).To(BeTrue(), "got %v", err)
			},
			Entry("UpsertCatalog", func(s catalog.Store, ctx context.Context) error {
				return s.UpsertCatalog(ctx, catalog.CatalogRecord{CatalogID: "c", TenantID: storeTenant})
			}),
			Entry("GetCatalog", func(s catalog.Store, ctx context.Context) error {
				_, err := s.GetCatalog(ctx, "c")
				return err
			}),
			Entry("ListCatalogs", func(s catalog.Store, ctx context.Context) error {
				_, _, err := s.ListCatalogs(ctx, catalog.ListOptions{})
				return err
			}),
			Entry("UpsertControls", func(s catalog.Store, ctx context.Context) error {
				return s.UpsertControls(ctx, []catalog.ControlRecord{{TenantID: storeTenant, ControlID: "c1"}})
			}),
			Entry("GetControl", func(s catalog.Store, ctx context.Context) error {
				_, err := s.GetControl(ctx, "c1")
				return err
			}),
			Entry("SearchControls", func(s catalog.Store, ctx context.Context) error {
				_, _, err := s.SearchControls(ctx, catalog.SearchQuery{Query: "q"})
				return err
			}),
		)

		DescribeTable("refuses a record whose tenant differs from the context tenant",
			func(call func(catalog.Store, context.Context) error) {
				err := call(catalog.NewPGStore(noBegin()), storeCtx)
				Expect(err).To(MatchError(ContainSubstring(`does not match the request tenant "store-unit"`)))
			},
			Entry("UpsertCatalog", func(s catalog.Store, ctx context.Context) error {
				return s.UpsertCatalog(ctx, catalog.CatalogRecord{CatalogID: "c", TenantID: "other-tenant"})
			}),
			Entry("UpsertCatalog with an empty tenant", func(s catalog.Store, ctx context.Context) error {
				return s.UpsertCatalog(ctx, catalog.CatalogRecord{CatalogID: "c"})
			}),
			Entry("UpsertControls when any record differs", func(s catalog.Store, ctx context.Context) error {
				return s.UpsertControls(ctx, []catalog.ControlRecord{
					{TenantID: storeTenant, ControlID: "c1"},
					{TenantID: "other-tenant", ControlID: "c2"},
				})
			}),
		)
	})

	Describe("UpsertControls", func() {
		It("succeeds without calling Begin for empty slice", func() {
			conn := &dbtest.Conn{
				BeginFunc: func(context.Context) (db.Transaction, error) {
					Fail("Begin should not be called for empty controls slice")
					return nil, nil
				},
			}
			store := catalog.NewPGStore(conn)

			Expect(store.UpsertControls(storeCtx, nil)).To(Succeed())
			Expect(store.UpsertControls(storeCtx, []catalog.ControlRecord{})).To(Succeed())
		})

		It("returns error when Begin fails", func() {
			beginErr := errors.New("pool exhausted")
			conn := &dbtest.Conn{
				BeginFunc: func(context.Context) (db.Transaction, error) { return nil, beginErr },
			}
			store := catalog.NewPGStore(conn)

			err := store.UpsertControls(storeCtx, []catalog.ControlRecord{{TenantID: storeTenant, ControlID: "c1"}})
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, beginErr)).To(BeTrue())
		})

		It("commits on success", func() {
			// The ownership check finds the catalog, then the upsert runs.
			tx := &dbtest.Tx{
				QueryRowFunc: func(context.Context, string, ...any) db.Row { return dbtest.Row(nil, 1) },
				ExecFunc:     func(context.Context, string, ...any) error { return nil },
			}
			conn := &dbtest.Conn{Tx: tx}
			store := catalog.NewPGStore(conn)

			ctrl := catalog.ControlRecord{
				TenantID:  storeTenant,
				ControlID: "ctrl-1",
				CatalogID: "cat-1",
				Title:     "Test Control",
			}
			err := store.UpsertControls(storeCtx, []catalog.ControlRecord{ctrl})
			Expect(err).NotTo(HaveOccurred())
			Expect(tx.Commits()).To(Equal(1))
		})

		It("refuses the batch without writing when a catalog is not the tenant's", func() {
			tx := &dbtest.Tx{
				QueryRowFunc: func(context.Context, string, ...any) db.Row { return dbtest.Row(sql.ErrNoRows) },
			}
			conn := &dbtest.Conn{Tx: tx}
			store := catalog.NewPGStore(conn)

			err := store.UpsertControls(storeCtx, []catalog.ControlRecord{{TenantID: storeTenant, ControlID: "c1", CatalogID: "foreign"}})
			Expect(err).To(MatchError(ContainSubstring(`catalog "foreign" is not a catalog of tenant "store-unit"`)))
			Expect(tx.Execs()).To(BeEmpty())
			Expect(tx.Commits()).To(Equal(0))
		})
	})
})

// ===========================================================================
// Service Specs (from service_test.go)
// ===========================================================================

var _ = Describe("Service", func() {
	Describe("NewService", func() {
		It("creates a service with the given options", func() {
			parser := &mockParser{}
			store := newMockStore()
			stg := &storagetest.Fake{}

			svc := catalog.NewService(
				catalog.WithParser(parser),
				catalog.WithStore(store),
				catalog.WithStorage(stg),
			)
			Expect(svc).NotTo(BeNil())
			Expect(catalog.ExportServiceHasParser(svc)).To(BeTrue())
			Expect(catalog.ExportServiceHasStore(svc)).To(BeTrue())
			Expect(catalog.ExportServiceHasStorage(svc)).To(BeTrue())
		})
	})

	Describe("ParseCatalog", func() {
		var (
			svc *catalog.Service
			stg *storagetest.Fake
			ms  *mockStore
		)

		BeforeEach(func() {
			ms = newMockStore()
			stg = &storagetest.Fake{}
		})

		Context("validation", func() {
			BeforeEach(func() {
				svc = catalog.NewService(
					catalog.WithParser(&mockParser{}),
					catalog.WithStore(ms),
					catalog.WithStorage(stg),
				)
			})

			It("rejects empty document_id", func() {
				req := &crosscodexv1.ParseCatalogRequest{
					TenantContext: &crosscodexv1.TenantContext{TenantId: "tenant-1"},
					DocumentId:    "",
				}

				_, err := svc.ParseCatalog(context.Background(), connect.NewRequest(req))
				Expect(err).To(HaveOccurred())
				connectErr := err.(*connect.Error)
				Expect(connectErr.Code()).To(Equal(connect.CodeInvalidArgument))
			})

			It("rejects missing tenant context", func() {
				req := &crosscodexv1.ParseCatalogRequest{
					TenantContext: nil,
					DocumentId:    "doc-1",
				}

				_, err := svc.ParseCatalog(context.Background(), connect.NewRequest(req))
				Expect(err).To(HaveOccurred())
				connectErr := err.(*connect.Error)
				Expect(connectErr.Code()).To(Equal(connect.CodeInvalidArgument))
			})
		})

		Context("with OSCAL document", func() {
			It("parses and stores the catalog and controls", func() {
				items := []oscal.ControlItem{
					{ID: "ac-1", Title: "Access Control Policy", Text: "Develop policy", Class: oscal.ClassRequirement},
					{ID: "ac-2", Title: "Account Management", Text: "Manage accounts", Class: oscal.ClassRequirement},
				}

				parser := &mockParser{items: items}
				svc = catalog.NewService(
					catalog.WithParser(parser),
					catalog.WithStore(ms),
					catalog.WithStorage(stg),
				)

				oscalDoc := map[string]interface{}{
					"catalog": map[string]interface{}{
						"uuid": "test-uuid",
					},
				}
				oscalData, _ := json.Marshal(oscalDoc)
				Expect(stg.Put(context.Background(), "doc-1", bytes.NewReader(oscalData))).To(Succeed())

				req := &crosscodexv1.ParseCatalogRequest{
					TenantContext: &crosscodexv1.TenantContext{TenantId: "tenant-1"},
					DocumentId:    "doc-1",
					CatalogName:   "Test Catalog",
				}

				resp, err := svc.ParseCatalog(context.Background(), connect.NewRequest(req))
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.Msg.CatalogId).NotTo(BeEmpty())
				Expect(resp.Msg.Status).To(Equal(crosscodexv1.JobStatus_JOB_STATUS_COMPLETED))

				cat, err := ms.GetCatalog(context.Background(), resp.Msg.CatalogId)
				Expect(err).NotTo(HaveOccurred())
				Expect(cat.TenantID).To(Equal("tenant-1"))
				Expect(cat.Name).To(Equal("Test Catalog"))
				Expect(ms.controlRecords).To(HaveLen(2))
			})
		})

		Context("with non-OSCAL document", func() {
			DescribeTable("rejects it as an unsupported format, advises a fix and stores nothing",
				func(format crosscodexv1.CatalogFormat, content, advice, wrongAdvice string) {
					svc = catalog.NewService(
						catalog.WithParser(&mockParser{}),
						catalog.WithStore(ms),
						catalog.WithStorage(stg),
					)

					Expect(stg.Put(context.Background(), "doc-2", strings.NewReader(content))).To(Succeed())

					req := &crosscodexv1.ParseCatalogRequest{
						TenantContext: &crosscodexv1.TenantContext{TenantId: "tenant-1"},
						DocumentId:    "doc-2",
						CatalogName:   "Non-OSCAL Catalog",
						Format:        format,
					}

					_, err := svc.ParseCatalog(context.Background(), connect.NewRequest(req))
					Expect(err).To(HaveOccurred())
					Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
					Expect(err.Error()).To(ContainSubstring("unsupported catalog format"))
					Expect(err.Error()).To(ContainSubstring("Only OSCAL JSON catalogs can be imported"))
					Expect(err.Error()).To(ContainSubstring(advice))
					Expect(err.Error()).NotTo(ContainSubstring(wrongAdvice))
					Expect(err.Error()).NotTo(ContainSubstring("structurer"))
					Expect(ms.catalogRecords).To(BeEmpty())
					Expect(ms.controlRecords).To(BeEmpty())
				},
				Entry("JSON without the OSCAL root key",
					crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED,
					`{"document":{"title":"test"}}`, `no top-level "catalog" object`, "not valid JSON"),
				Entry("a JSON array",
					crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED,
					`[{"catalog":{}}]`, `no top-level "catalog" object`, "not valid JSON"),
				Entry("plain text",
					crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED,
					"AC-1 Access Control Policy\nDevelop a policy.", "not valid JSON", "no top-level"),
				Entry("truncated OSCAL JSON",
					crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED,
					`{"catalog":{"uuid":"x","metadata":{`, "fix the JSON syntax", "no top-level"),
				Entry("a declared Gemara catalog",
					crosscodexv1.CatalogFormat_CATALOG_FORMAT_GEMARA,
					"AC-1 Access Control Policy", "Gemara catalogs cannot be imported yet", "not valid JSON"),
			)
		})
	})

	Describe("GetCatalog", func() {
		It("rejects missing tenant context", func() {
			svc := catalog.NewService(catalog.WithStore(newMockStore()))
			req := &crosscodexv1.GetCatalogRequest{
				TenantContext: nil,
				CatalogId:     "cat-1",
			}

			_, err := svc.GetCatalog(context.Background(), connect.NewRequest(req))
			Expect(err).To(HaveOccurred())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		})
	})

	Describe("ListCatalogs", func() {
		var svc *catalog.Service

		BeforeEach(func() {
			svc = catalog.NewService(catalog.WithStore(newMockStore()))
		})

		It("rejects missing tenant context", func() {
			req := &crosscodexv1.ListCatalogsRequest{TenantContext: nil}

			_, err := svc.ListCatalogs(context.Background(), connect.NewRequest(req))
			Expect(err).To(HaveOccurred())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		})

		It("rejects invalid page token", func() {
			resp, err := svc.ListCatalogs(context.Background(), connect.NewRequest(&crosscodexv1.ListCatalogsRequest{
				TenantContext: &crosscodexv1.TenantContext{TenantId: "test-tenant"},
				Options: &crosscodexv1.ListOptions{
					Pagination: &crosscodexv1.Pagination{
						PageToken: "not-a-number",
					},
				},
			}))
			Expect(resp).To(BeNil())
			Expect(err).To(HaveOccurred())
			connectErr, ok := err.(*connect.Error)
			Expect(ok).To(BeTrue())
			Expect(connectErr.Code()).To(Equal(connect.CodeInvalidArgument))
		})
	})

	Describe("GetControl", func() {
		It("rejects missing tenant context", func() {
			svc := catalog.NewService(catalog.WithStore(newMockStore()))
			req := &crosscodexv1.GetControlRequest{
				TenantContext: nil,
				ControlId:     "ctrl-1",
			}

			_, err := svc.GetControl(context.Background(), connect.NewRequest(req))
			Expect(err).To(HaveOccurred())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		})
	})

	Describe("SearchControls", func() {
		var svc *catalog.Service

		BeforeEach(func() {
			svc = catalog.NewService(catalog.WithStore(newMockStore()))
		})

		It("rejects missing tenant context", func() {
			req := &crosscodexv1.SearchControlsRequest{
				TenantContext: nil,
				Query:         "test",
			}

			_, err := svc.SearchControls(context.Background(), connect.NewRequest(req))
			Expect(err).To(HaveOccurred())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		})

		It("rejects invalid page token", func() {
			resp, err := svc.SearchControls(context.Background(), connect.NewRequest(&crosscodexv1.SearchControlsRequest{
				TenantContext: &crosscodexv1.TenantContext{TenantId: "test-tenant"},
				Query:         "access control",
				Options: &crosscodexv1.ListOptions{
					Pagination: &crosscodexv1.Pagination{
						PageToken: "abc",
					},
				},
			}))
			Expect(resp).To(BeNil())
			Expect(err).To(HaveOccurred())
			connectErr, ok := err.(*connect.Error)
			Expect(ok).To(BeTrue())
			Expect(connectErr.Code()).To(Equal(connect.CodeInvalidArgument))
		})
	})
})

// ===========================================================================
// Helper function specs (from service_test.go, using export bridges)
// ===========================================================================

var _ = Describe("isOSCALJSON", func() {
	DescribeTable("detects OSCAL documents",
		func(data string, want bool) {
			Expect(catalog.ExportIsOSCALJSON([]byte(data))).To(Equal(want))
		},
		Entry("valid OSCAL", `{"catalog":{"uuid":"abc"}}`, true),
		Entry("not OSCAL", `{"document":{"title":"test"}}`, false),
		Entry("invalid JSON", `{invalid`, false),
		Entry("empty", ``, false),
	)
})

var _ = Describe("validateItems", func() {
	DescribeTable("validates control items",
		func(items []oscal.ControlItem, shouldErr bool) {
			err := catalog.ExportValidateItems(items)
			if shouldErr {
				Expect(err).To(HaveOccurred())
			} else {
				Expect(err).NotTo(HaveOccurred())
			}
		},
		Entry("valid items",
			[]oscal.ControlItem{
				{ID: "ac-1", ParentID: ""},
				{ID: "ac-2", ParentID: "ac-1"},
			},
			false,
		),
		Entry("duplicate ID",
			[]oscal.ControlItem{
				{ID: "ac-1", ParentID: ""},
				{ID: "ac-1", ParentID: ""},
			},
			true,
		),
		Entry("self-referential parent",
			[]oscal.ControlItem{
				{ID: "ac-1", ParentID: "ac-1"},
			},
			true,
		),
	)
})

var _ = Describe("generateCatalogID", func() {
	It("produces deterministic IDs", func() {
		id1 := catalog.ExportGenerateCatalogID("hash1", "tenant1")
		id2 := catalog.ExportGenerateCatalogID("hash1", "tenant1")
		Expect(id1).To(Equal(id2))
	})

	It("produces different IDs for different content hashes", func() {
		id1 := catalog.ExportGenerateCatalogID("hash1", "tenant1")
		id3 := catalog.ExportGenerateCatalogID("hash2", "tenant1")
		Expect(id1).NotTo(Equal(id3))
	})

	It("produces different IDs for different tenants", func() {
		id1 := catalog.ExportGenerateCatalogID("hash1", "tenant1")
		id4 := catalog.ExportGenerateCatalogID("hash1", "tenant2")
		Expect(id1).NotTo(Equal(id4))
	})

	It("produces 16-character IDs", func() {
		id := catalog.ExportGenerateCatalogID("hash1", "tenant1")
		Expect(id).To(HaveLen(16))
	})
})

var _ = Describe("mergeResults", func() {
	It("merges full-text and semantic results with deduplication", func() {
		ftRecords := []catalog.ExportControlRecord{
			{ControlID: "ctrl-1", Title: "Full Text 1"},
			{ControlID: "ctrl-2", Title: "Full Text 2"},
		}

		semanticResults := []vectordb.SimilarityResult{
			{ControlID: "ctrl-1", Similarity: 0.9},
			{ControlID: "ctrl-3", Similarity: 0.8},
		}

		merged := catalog.ExportMergeResults(ftRecords, semanticResults)

		Expect(merged).To(HaveLen(3))

		// Full-text results come first
		Expect(merged[0].ControlID).To(Equal("ctrl-1"))
		Expect(merged[1].ControlID).To(Equal("ctrl-2"))

		// Semantic-only result appended
		Expect(merged[2].ControlID).To(Equal("ctrl-3"))
	})
})
