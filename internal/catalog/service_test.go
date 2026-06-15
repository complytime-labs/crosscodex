package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"testing"

	crosscodexv1 "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/oscal"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/vectordb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// mockParser implements oscal.Parser for testing.
type mockParser struct {
	items []oscal.ControlItem
	err   error
}

func (m *mockParser) Parse(ctx context.Context, data io.Reader) ([]oscal.ControlItem, error) {
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

// mockStructurer implements oscal.Structurer for testing.
type mockStructurer struct {
	items []oscal.ControlItem
	err   error
}

func (m *mockStructurer) Structure(ctx context.Context, doc oscal.StructuredDoc, opts oscal.StructureOptions) ([]oscal.ControlItem, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.items, nil
}

// mockStore implements Store for testing.
type mockStore struct {
	catalogRecords   map[string]*CatalogRecord
	controlRecords   map[string]*ControlRecord
	upsertCatalogFn  func(context.Context, CatalogRecord) error
	upsertControlsFn func(context.Context, []ControlRecord) error
}

func newMockStore() *mockStore {
	return &mockStore{
		catalogRecords: make(map[string]*CatalogRecord),
		controlRecords: make(map[string]*ControlRecord),
	}
}

func (m *mockStore) UpsertCatalog(ctx context.Context, catalog CatalogRecord) error {
	if m.upsertCatalogFn != nil {
		return m.upsertCatalogFn(ctx, catalog)
	}
	m.catalogRecords[catalog.CatalogID] = &catalog
	return nil
}

func (m *mockStore) GetCatalog(ctx context.Context, catalogID string) (*CatalogRecord, error) {
	rec, ok := m.catalogRecords[catalogID]
	if !ok {
		return nil, nil
	}
	return rec, nil
}

func (m *mockStore) ListCatalogs(ctx context.Context, opts ListOptions) ([]CatalogRecord, PageInfo, error) {
	return nil, PageInfo{}, nil
}

func (m *mockStore) UpsertControls(ctx context.Context, controls []ControlRecord) error {
	if m.upsertControlsFn != nil {
		return m.upsertControlsFn(ctx, controls)
	}
	for _, ctrl := range controls {
		m.controlRecords[ctrl.ControlID] = &ctrl
	}
	return nil
}

func (m *mockStore) GetControl(ctx context.Context, controlID string) (*ControlRecord, error) {
	rec, ok := m.controlRecords[controlID]
	if !ok {
		return nil, nil
	}
	return rec, nil
}

func (m *mockStore) SearchControls(ctx context.Context, query SearchQuery) ([]ControlRecord, PageInfo, error) {
	return nil, PageInfo{}, nil
}

// mockStorage implements storage.Provider for testing.
type mockStorage struct {
	data map[string][]byte
}

func newMockStorage() *mockStorage {
	return &mockStorage{
		data: make(map[string][]byte),
	}
}

func (m *mockStorage) Get(ctx context.Context, path string) (io.ReadCloser, error) {
	data, ok := m.data[path]
	if !ok {
		return nil, io.ErrUnexpectedEOF
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (m *mockStorage) Put(ctx context.Context, path string, reader io.Reader) error {
	data, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	m.data[path] = data
	return nil
}

func (m *mockStorage) Delete(ctx context.Context, path string) error {
	delete(m.data, path)
	return nil
}

func (m *mockStorage) List(ctx context.Context, prefix string) ([]storage.ObjectMetadata, error) {
	return nil, nil
}

func (m *mockStorage) Close() error {
	return nil
}

func (m *mockStorage) Exists(ctx context.Context, path string) (bool, error) {
	_, ok := m.data[path]
	return ok, nil
}

func (m *mockStorage) Stat(ctx context.Context, key string) (*storage.ObjectMetadata, error) {
	data, ok := m.data[key]
	if !ok {
		return nil, io.ErrUnexpectedEOF
	}
	return &storage.ObjectMetadata{
		Key:  key,
		Size: int64(len(data)),
	}, nil
}

// TestNewService verifies that NewService creates a service with the given options.
func TestNewService(t *testing.T) {
	parser := &mockParser{}
	store := newMockStore()
	storage := newMockStorage()

	svc := NewService(
		WithParser(parser),
		WithStore(store),
		WithStorage(storage),
	)

	if svc.parser == nil {
		t.Error("parser not set")
	}
	if svc.store == nil {
		t.Error("store not set")
	}
	if svc.storage == nil {
		t.Error("storage not set")
	}
}

// TestParseCatalog_EmptyDocumentID verifies that ParseCatalog rejects empty document_id.
func TestParseCatalog_EmptyDocumentID(t *testing.T) {
	svc := NewService(
		WithParser(&mockParser{}),
		WithStore(newMockStore()),
		WithStorage(newMockStorage()),
	)

	req := &crosscodexv1.ParseCatalogRequest{
		TenantContext: &crosscodexv1.TenantContext{TenantId: "tenant-1"},
		DocumentId:    "",
	}

	_, err := svc.ParseCatalog(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for empty document_id")
	}

	st := status.Convert(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %v", st.Code())
	}
}

// TestParseCatalog_MissingTenantContext verifies that ParseCatalog rejects missing tenant context.
func TestParseCatalog_MissingTenantContext(t *testing.T) {
	svc := NewService(
		WithParser(&mockParser{}),
		WithStore(newMockStore()),
		WithStorage(newMockStorage()),
	)

	req := &crosscodexv1.ParseCatalogRequest{
		TenantContext: nil,
		DocumentId:    "doc-1",
	}

	_, err := svc.ParseCatalog(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for missing tenant_context")
	}

	st := status.Convert(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %v", st.Code())
	}
}

// TestGetCatalog_MissingTenantContext verifies that GetCatalog rejects missing tenant context.
func TestGetCatalog_MissingTenantContext(t *testing.T) {
	svc := NewService(
		WithStore(newMockStore()),
	)

	req := &crosscodexv1.GetCatalogRequest{
		TenantContext: nil,
		CatalogId:     "cat-1",
	}

	_, err := svc.GetCatalog(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for missing tenant_context")
	}

	st := status.Convert(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %v", st.Code())
	}
}

// TestListCatalogs_MissingTenantContext verifies that ListCatalogs rejects missing tenant context.
func TestListCatalogs_MissingTenantContext(t *testing.T) {
	svc := NewService(
		WithStore(newMockStore()),
	)

	req := &crosscodexv1.ListCatalogsRequest{
		TenantContext: nil,
	}

	_, err := svc.ListCatalogs(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for missing tenant_context")
	}

	st := status.Convert(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %v", st.Code())
	}
}

// TestGetControl_MissingTenantContext verifies that GetControl rejects missing tenant context.
func TestGetControl_MissingTenantContext(t *testing.T) {
	svc := NewService(
		WithStore(newMockStore()),
	)

	req := &crosscodexv1.GetControlRequest{
		TenantContext: nil,
		ControlId:     "ctrl-1",
	}

	_, err := svc.GetControl(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for missing tenant_context")
	}

	st := status.Convert(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %v", st.Code())
	}
}

// TestSearchControls_MissingTenantContext verifies that SearchControls rejects missing tenant context.
func TestSearchControls_MissingTenantContext(t *testing.T) {
	svc := NewService(
		WithStore(newMockStore()),
	)

	req := &crosscodexv1.SearchControlsRequest{
		TenantContext: nil,
		Query:         "test",
	}

	_, err := svc.SearchControls(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for missing tenant_context")
	}

	st := status.Convert(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %v", st.Code())
	}
}

// TestIsOSCALJSON tests the OSCAL detection helper.
func TestIsOSCALJSON(t *testing.T) {
	tests := []struct {
		name string
		data string
		want bool
	}{
		{
			name: "valid OSCAL",
			data: `{"catalog":{"uuid":"abc"}}`,
			want: true,
		},
		{
			name: "not OSCAL",
			data: `{"document":{"title":"test"}}`,
			want: false,
		},
		{
			name: "invalid JSON",
			data: `{invalid`,
			want: false,
		},
		{
			name: "empty",
			data: ``,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isOSCALJSON([]byte(tt.data))
			if got != tt.want {
				t.Errorf("isOSCALJSON() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestValidateItems tests the validation helper.
func TestValidateItems(t *testing.T) {
	tests := []struct {
		name    string
		items   []oscal.ControlItem
		wantErr bool
	}{
		{
			name: "valid items",
			items: []oscal.ControlItem{
				{ID: "ac-1", ParentID: ""},
				{ID: "ac-2", ParentID: "ac-1"},
			},
			wantErr: false,
		},
		{
			name: "duplicate ID",
			items: []oscal.ControlItem{
				{ID: "ac-1", ParentID: ""},
				{ID: "ac-1", ParentID: ""},
			},
			wantErr: true,
		},
		{
			name: "self-referential parent",
			items: []oscal.ControlItem{
				{ID: "ac-1", ParentID: "ac-1"},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateItems(tt.items)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateItems() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestGenerateCatalogID tests the catalog ID generation helper.
func TestGenerateCatalogID(t *testing.T) {
	id1 := generateCatalogID("hash1", "tenant1")
	id2 := generateCatalogID("hash1", "tenant1")
	id3 := generateCatalogID("hash2", "tenant1")
	id4 := generateCatalogID("hash1", "tenant2")

	// Same inputs produce same ID
	if id1 != id2 {
		t.Error("same inputs should produce same catalog ID")
	}

	// Different content hash produces different ID
	if id1 == id3 {
		t.Error("different content hash should produce different catalog ID")
	}

	// Different tenant produces different ID
	if id1 == id4 {
		t.Error("different tenant should produce different catalog ID")
	}

	// ID should be 16 characters
	if len(id1) != 16 {
		t.Errorf("catalog ID length = %d, want 16", len(id1))
	}
}

// TestMergeResults tests the result merging helper.
func TestMergeResults(t *testing.T) {
	ftRecords := []ControlRecord{
		{ControlID: "ctrl-1", Title: "Full Text 1"},
		{ControlID: "ctrl-2", Title: "Full Text 2"},
	}

	semanticResults := []vectordb.SimilarityResult{
		{ControlID: "ctrl-1", Similarity: 0.9},
		{ControlID: "ctrl-3", Similarity: 0.8},
	}

	merged := mergeResults(ftRecords, semanticResults)

	// Should have FT results + semantic-only results (ctrl-1 deduped, ctrl-3 added)
	if len(merged) != 3 {
		t.Errorf("merged length = %d, want 3", len(merged))
	}

	// Full-text results come first
	if merged[0].ControlID != "ctrl-1" {
		t.Errorf("merged[0].ControlID = %s, want ctrl-1", merged[0].ControlID)
	}
	if merged[1].ControlID != "ctrl-2" {
		t.Errorf("merged[1].ControlID = %s, want ctrl-2", merged[1].ControlID)
	}

	// Semantic-only result appended
	if merged[2].ControlID != "ctrl-3" {
		t.Errorf("merged[2].ControlID = %s, want ctrl-3", merged[2].ControlID)
	}
}

// TestParseCatalogOSCAL tests parsing an OSCAL document.
func TestParseCatalogOSCAL(t *testing.T) {
	items := []oscal.ControlItem{
		{ID: "ac-1", Title: "Access Control Policy", Text: "Develop policy", Class: oscal.ClassRequirement},
		{ID: "ac-2", Title: "Account Management", Text: "Manage accounts", Class: oscal.ClassRequirement},
	}

	parser := &mockParser{items: items}
	store := newMockStore()
	storage := newMockStorage()

	// Create OSCAL JSON document
	oscalDoc := map[string]interface{}{
		"catalog": map[string]interface{}{
			"uuid": "test-uuid",
		},
	}
	oscalData, _ := json.Marshal(oscalDoc)
	storage.data["doc-1"] = oscalData

	svc := NewService(
		WithParser(parser),
		WithStore(store),
		WithStorage(storage),
	)

	req := &crosscodexv1.ParseCatalogRequest{
		TenantContext: &crosscodexv1.TenantContext{TenantId: "tenant-1"},
		DocumentId:    "doc-1",
		CatalogName:   "Test Catalog",
	}

	resp, err := svc.ParseCatalog(context.Background(), req)
	if err != nil {
		t.Fatalf("ParseCatalog() error = %v", err)
	}

	if resp.CatalogId == "" {
		t.Error("expected non-empty catalog_id")
	}

	if resp.Status != crosscodexv1.JobStatus_JOB_STATUS_COMPLETED {
		t.Errorf("status = %v, want JOB_STATUS_COMPLETED", resp.Status)
	}

	// Verify catalog was stored
	catalog, err := store.GetCatalog(context.Background(), resp.CatalogId)
	if err != nil {
		t.Fatalf("GetCatalog() error = %v", err)
	}

	if catalog.TenantID != "tenant-1" {
		t.Errorf("catalog.TenantID = %s, want tenant-1", catalog.TenantID)
	}

	if catalog.Name != "Test Catalog" {
		t.Errorf("catalog.Name = %s, want Test Catalog", catalog.Name)
	}

	// Verify controls were stored
	if len(store.controlRecords) != 2 {
		t.Errorf("stored controls count = %d, want 2", len(store.controlRecords))
	}
}

// TestParseCatalogNonOSCAL tests parsing a non-OSCAL document.
// Currently structuring is not implemented at the service layer, so this should return Unimplemented.
func TestParseCatalogNonOSCAL(t *testing.T) {
	items := []oscal.ControlItem{
		{ID: "req-1", Title: "Requirement 1", Text: "Must do X", Class: oscal.ClassRequirement},
	}

	structurer := &mockStructurer{items: items}
	store := newMockStore()
	storage := newMockStorage()

	// Create non-OSCAL document
	nonOSCALData := []byte(`{"document":{"title":"test"}}`)
	storage.data["doc-2"] = nonOSCALData

	svc := NewService(
		WithStructurer(structurer),
		WithStore(store),
		WithStorage(storage),
	)

	req := &crosscodexv1.ParseCatalogRequest{
		TenantContext: &crosscodexv1.TenantContext{TenantId: "tenant-1"},
		DocumentId:    "doc-2",
		CatalogName:   "Non-OSCAL Catalog",
	}

	_, err := svc.ParseCatalog(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for non-OSCAL document")
	}

	st := status.Convert(err)
	if st.Code() != codes.Unimplemented {
		t.Errorf("expected Unimplemented, got %v", st.Code())
	}
}

func TestListCatalogs_InvalidPageToken(t *testing.T) {
	svc := NewService(
		WithStore(newMockStore()),
	)

	resp, err := svc.ListCatalogs(context.Background(), &crosscodexv1.ListCatalogsRequest{
		TenantContext: &crosscodexv1.TenantContext{TenantId: "test-tenant"},
		Options: &crosscodexv1.ListOptions{
			Pagination: &crosscodexv1.Pagination{
				PageToken: "not-a-number",
			},
		},
	})
	if resp != nil {
		t.Errorf("expected nil response, got %v", resp)
	}
	if err == nil {
		t.Fatal("expected error for invalid page token")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %v", err)
	}
}

func TestSearchControls_InvalidPageToken(t *testing.T) {
	svc := NewService(
		WithStore(newMockStore()),
	)

	resp, err := svc.SearchControls(context.Background(), &crosscodexv1.SearchControlsRequest{
		TenantContext: &crosscodexv1.TenantContext{TenantId: "test-tenant"},
		Query:         "access control",
		Options: &crosscodexv1.ListOptions{
			Pagination: &crosscodexv1.Pagination{
				PageToken: "abc",
			},
		},
	})
	if resp != nil {
		t.Errorf("expected nil response, got %v", resp)
	}
	if err == nil {
		t.Fatal("expected error for invalid page token")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %v", err)
	}
}
