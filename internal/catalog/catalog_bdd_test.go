//go:build !integration

package catalog_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	crosscodexv1 "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/catalog"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/oscal"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/vectordb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
// Mock types implementing db interfaces for store unit testing
// ---------------------------------------------------------------------------

type mockConn struct {
	beginFn func(ctx context.Context) (db.Transaction, error)
}

func (m *mockConn) Begin(ctx context.Context) (db.Transaction, error) { return m.beginFn(ctx) }
func (m *mockConn) Query(_ context.Context, _ string, _ ...any) (db.Rows, error) {
	return nil, errors.New("mockConn.Query not implemented")
}
func (m *mockConn) QueryRow(_ context.Context, _ string, _ ...any) db.Row {
	return &mockRow{scanFn: func(...any) error { return errors.New("mockConn.QueryRow not implemented") }}
}
func (m *mockConn) Exec(_ context.Context, _ string, _ ...any) error {
	return errors.New("mockConn.Exec not implemented")
}
func (m *mockConn) Close() error { return nil }

type mockTx struct {
	committed  bool
	rolledBack bool
	execFn     func(ctx context.Context, query string, args ...any) error
	queryFn    func(ctx context.Context, query string, args ...any) (db.Rows, error)
	queryRowFn func(ctx context.Context, query string, args ...any) db.Row
}

func (m *mockTx) Commit() error   { m.committed = true; return nil }
func (m *mockTx) Rollback() error { m.rolledBack = true; return nil }
func (m *mockTx) Exec(ctx context.Context, query string, args ...any) error {
	if m.execFn != nil {
		return m.execFn(ctx, query, args...)
	}
	return nil
}
func (m *mockTx) Query(ctx context.Context, query string, args ...any) (db.Rows, error) {
	if m.queryFn != nil {
		return m.queryFn(ctx, query, args...)
	}
	return &emptyRows{}, nil
}
func (m *mockTx) QueryRow(ctx context.Context, query string, args ...any) db.Row {
	if m.queryRowFn != nil {
		return m.queryRowFn(ctx, query, args...)
	}
	return &mockRow{scanFn: func(...any) error { return nil }}
}

type mockRow struct {
	scanFn func(dest ...any) error
}

func (m *mockRow) Scan(dest ...any) error { return m.scanFn(dest...) }

type emptyRows struct{}

func (e *emptyRows) Next() bool          { return false }
func (e *emptyRows) Scan(_ ...any) error { return nil }
func (e *emptyRows) Close() error        { return nil }
func (e *emptyRows) Err() error          { return nil }

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

type mockStructurer struct {
	items []oscal.ControlItem
	err   error
}

func (m *mockStructurer) Structure(_ context.Context, _ oscal.StructuredDoc, _ oscal.StructureOptions) ([]oscal.ControlItem, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.items, nil
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

type mockStorage struct {
	data map[string][]byte
}

func newMockStorage() *mockStorage {
	return &mockStorage{
		data: make(map[string][]byte),
	}
}

func (m *mockStorage) Get(_ context.Context, path string) (io.ReadCloser, error) {
	data, ok := m.data[path]
	if !ok {
		return nil, io.ErrUnexpectedEOF
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (m *mockStorage) Put(_ context.Context, path string, reader io.Reader) error {
	data, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	m.data[path] = data
	return nil
}

func (m *mockStorage) Delete(_ context.Context, path string) error {
	delete(m.data, path)
	return nil
}

func (m *mockStorage) List(_ context.Context, _ string) ([]storage.ObjectMetadata, error) {
	return nil, nil
}

func (m *mockStorage) Close() error { return nil }

func (m *mockStorage) Exists(_ context.Context, path string) (bool, error) {
	_, ok := m.data[path]
	return ok, nil
}

func (m *mockStorage) Stat(_ context.Context, key string) (*storage.ObjectMetadata, error) {
	data, ok := m.data[key]
	if !ok {
		return nil, io.ErrUnexpectedEOF
	}
	return &storage.ObjectMetadata{
		Key:  key,
		Size: int64(len(data)),
	}, nil
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

var _ = Describe("PGStore", func() {
	Describe("UpsertCatalog", func() {
		It("returns error when Begin fails", func() {
			beginErr := errors.New("connection refused")
			conn := &mockConn{
				beginFn: func(context.Context) (db.Transaction, error) {
					return nil, beginErr
				},
			}
			store := catalog.NewPGStore(conn)

			err := store.UpsertCatalog(context.Background(), catalog.CatalogRecord{CatalogID: "test"})
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, beginErr)).To(BeTrue())
		})

		It("commits on success", func() {
			tx := &mockTx{}
			conn := &mockConn{
				beginFn: func(context.Context) (db.Transaction, error) { return tx, nil },
			}
			store := catalog.NewPGStore(conn)

			err := store.UpsertCatalog(context.Background(), catalog.CatalogRecord{CatalogID: "cat-1"})
			Expect(err).NotTo(HaveOccurred())
			Expect(tx.committed).To(BeTrue())
		})

		It("does not commit when Exec fails", func() {
			execErr := errors.New("unique violation")
			tx := &mockTx{
				execFn: func(context.Context, string, ...any) error { return execErr },
			}
			conn := &mockConn{
				beginFn: func(context.Context) (db.Transaction, error) { return tx, nil },
			}
			store := catalog.NewPGStore(conn)

			err := store.UpsertCatalog(context.Background(), catalog.CatalogRecord{CatalogID: "cat-1"})
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, execErr)).To(BeTrue())
			Expect(tx.committed).To(BeFalse())
		})
	})

	Describe("UpsertControls", func() {
		It("succeeds without calling Begin for empty slice", func() {
			conn := &mockConn{
				beginFn: func(context.Context) (db.Transaction, error) {
					Fail("Begin should not be called for empty controls slice")
					return nil, nil
				},
			}
			store := catalog.NewPGStore(conn)

			Expect(store.UpsertControls(context.Background(), nil)).To(Succeed())
			Expect(store.UpsertControls(context.Background(), []catalog.ControlRecord{})).To(Succeed())
		})

		It("returns error when Begin fails", func() {
			beginErr := errors.New("pool exhausted")
			conn := &mockConn{
				beginFn: func(context.Context) (db.Transaction, error) { return nil, beginErr },
			}
			store := catalog.NewPGStore(conn)

			err := store.UpsertControls(context.Background(), []catalog.ControlRecord{{ControlID: "c1"}})
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, beginErr)).To(BeTrue())
		})

		It("commits on success", func() {
			tx := &mockTx{}
			conn := &mockConn{
				beginFn: func(context.Context) (db.Transaction, error) { return tx, nil },
			}
			store := catalog.NewPGStore(conn)

			ctrl := catalog.ControlRecord{
				ControlID: "ctrl-1",
				CatalogID: "cat-1",
				Title:     "Test Control",
			}
			err := store.UpsertControls(context.Background(), []catalog.ControlRecord{ctrl})
			Expect(err).NotTo(HaveOccurred())
			Expect(tx.committed).To(BeTrue())
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
			stg := newMockStorage()

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
			stg *mockStorage
			ms  *mockStore
		)

		BeforeEach(func() {
			ms = newMockStore()
			stg = newMockStorage()
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

				_, err := svc.ParseCatalog(context.Background(), req)
				Expect(err).To(HaveOccurred())
				st := status.Convert(err)
				Expect(st.Code()).To(Equal(codes.InvalidArgument))
			})

			It("rejects missing tenant context", func() {
				req := &crosscodexv1.ParseCatalogRequest{
					TenantContext: nil,
					DocumentId:    "doc-1",
				}

				_, err := svc.ParseCatalog(context.Background(), req)
				Expect(err).To(HaveOccurred())
				st := status.Convert(err)
				Expect(st.Code()).To(Equal(codes.InvalidArgument))
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
				stg.data["doc-1"] = oscalData

				req := &crosscodexv1.ParseCatalogRequest{
					TenantContext: &crosscodexv1.TenantContext{TenantId: "tenant-1"},
					DocumentId:    "doc-1",
					CatalogName:   "Test Catalog",
				}

				resp, err := svc.ParseCatalog(context.Background(), req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.CatalogId).NotTo(BeEmpty())
				Expect(resp.Status).To(Equal(crosscodexv1.JobStatus_JOB_STATUS_COMPLETED))

				cat, err := ms.GetCatalog(context.Background(), resp.CatalogId)
				Expect(err).NotTo(HaveOccurred())
				Expect(cat.TenantID).To(Equal("tenant-1"))
				Expect(cat.Name).To(Equal("Test Catalog"))
				Expect(ms.controlRecords).To(HaveLen(2))
			})
		})

		Context("with non-OSCAL document", func() {
			It("returns Unimplemented", func() {
				items := []oscal.ControlItem{
					{ID: "req-1", Title: "Requirement 1", Text: "Must do X", Class: oscal.ClassRequirement},
				}

				svc = catalog.NewService(
					catalog.WithStructurer(&mockStructurer{items: items}),
					catalog.WithStore(ms),
					catalog.WithStorage(stg),
				)

				stg.data["doc-2"] = []byte(`{"document":{"title":"test"}}`)

				req := &crosscodexv1.ParseCatalogRequest{
					TenantContext: &crosscodexv1.TenantContext{TenantId: "tenant-1"},
					DocumentId:    "doc-2",
					CatalogName:   "Non-OSCAL Catalog",
				}

				_, err := svc.ParseCatalog(context.Background(), req)
				Expect(err).To(HaveOccurred())
				st := status.Convert(err)
				Expect(st.Code()).To(Equal(codes.Unimplemented))
			})
		})
	})

	Describe("GetCatalog", func() {
		It("rejects missing tenant context", func() {
			svc := catalog.NewService(catalog.WithStore(newMockStore()))
			req := &crosscodexv1.GetCatalogRequest{
				TenantContext: nil,
				CatalogId:     "cat-1",
			}

			_, err := svc.GetCatalog(context.Background(), req)
			Expect(err).To(HaveOccurred())
			Expect(status.Convert(err).Code()).To(Equal(codes.InvalidArgument))
		})
	})

	Describe("ListCatalogs", func() {
		var svc *catalog.Service

		BeforeEach(func() {
			svc = catalog.NewService(catalog.WithStore(newMockStore()))
		})

		It("rejects missing tenant context", func() {
			req := &crosscodexv1.ListCatalogsRequest{TenantContext: nil}

			_, err := svc.ListCatalogs(context.Background(), req)
			Expect(err).To(HaveOccurred())
			Expect(status.Convert(err).Code()).To(Equal(codes.InvalidArgument))
		})

		It("rejects invalid page token", func() {
			resp, err := svc.ListCatalogs(context.Background(), &crosscodexv1.ListCatalogsRequest{
				TenantContext: &crosscodexv1.TenantContext{TenantId: "test-tenant"},
				Options: &crosscodexv1.ListOptions{
					Pagination: &crosscodexv1.Pagination{
						PageToken: "not-a-number",
					},
				},
			})
			Expect(resp).To(BeNil())
			Expect(err).To(HaveOccurred())
			st, ok := status.FromError(err)
			Expect(ok).To(BeTrue())
			Expect(st.Code()).To(Equal(codes.InvalidArgument))
		})
	})

	Describe("GetControl", func() {
		It("rejects missing tenant context", func() {
			svc := catalog.NewService(catalog.WithStore(newMockStore()))
			req := &crosscodexv1.GetControlRequest{
				TenantContext: nil,
				ControlId:     "ctrl-1",
			}

			_, err := svc.GetControl(context.Background(), req)
			Expect(err).To(HaveOccurred())
			Expect(status.Convert(err).Code()).To(Equal(codes.InvalidArgument))
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

			_, err := svc.SearchControls(context.Background(), req)
			Expect(err).To(HaveOccurred())
			Expect(status.Convert(err).Code()).To(Equal(codes.InvalidArgument))
		})

		It("rejects invalid page token", func() {
			resp, err := svc.SearchControls(context.Background(), &crosscodexv1.SearchControlsRequest{
				TenantContext: &crosscodexv1.TenantContext{TenantId: "test-tenant"},
				Query:         "access control",
				Options: &crosscodexv1.ListOptions{
					Pagination: &crosscodexv1.Pagination{
						PageToken: "abc",
					},
				},
			})
			Expect(resp).To(BeNil())
			Expect(err).To(HaveOccurred())
			st, ok := status.FromError(err)
			Expect(ok).To(BeTrue())
			Expect(st.Code()).To(Equal(codes.InvalidArgument))
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
