package catalog

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	connect "connectrpc.com/connect"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/oscal"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// captureGraph records CreateNode/CreateEdge calls; every other GraphDB method
// is a no-op so it satisfies the interface.
type captureGraph struct {
	nodes []graphdb.Node
	edges []graphdb.Edge
}

func (c *captureGraph) CreateGraph(context.Context, string) error { return nil }
func (c *captureGraph) CreateNode(_ context.Context, _ string, n graphdb.Node) error {
	c.nodes = append(c.nodes, n)
	return nil
}
func (c *captureGraph) CreateEdge(_ context.Context, _, _, _ string, e graphdb.Edge) error {
	c.edges = append(c.edges, e)
	return nil
}
func (c *captureGraph) CreateRequiresEdge(context.Context, string, graphdb.RequiresEdge) error {
	return nil
}
func (c *captureGraph) QueryRelationships(context.Context, string, graphdb.RelationshipQuery) ([]graphdb.Relationship, error) {
	return nil, nil
}
func (c *captureGraph) Traverse(context.Context, string, graphdb.TraversalQuery) ([]graphdb.Path, error) {
	return nil, nil
}
func (c *captureGraph) QueryAsOf(context.Context, string, graphdb.RelationshipQuery, time.Time) ([]graphdb.Relationship, error) {
	return nil, nil
}
func (c *captureGraph) GetNode(context.Context, string, string) (*graphdb.Node, error) {
	return nil, nil
}
func (c *captureGraph) GetEdge(context.Context, string, string) (*graphdb.EdgeWithEndpoints, error) {
	return nil, nil
}
func (c *captureGraph) BulkCreateEdges(context.Context, string, []graphdb.BulkEdge) ([]string, error) {
	return nil, nil
}
func (c *captureGraph) ExecuteQuery(context.Context, string, string, map[string]string) ([]graphdb.QueryRow, error) {
	return nil, nil
}
func (c *captureGraph) SupersedeFact(context.Context, string, graphdb.SupersedeRequest) (bool, error) {
	return false, nil
}

// memStore is a minimal in-memory catalog Store.
type memStore struct{ controls []ControlRecord }

func (m *memStore) UpsertCatalog(context.Context, CatalogRecord) error { return nil }
func (m *memStore) GetCatalog(context.Context, string) (*CatalogRecord, error) {
	return nil, nil
}
func (m *memStore) ListCatalogs(context.Context, ListOptions) ([]CatalogRecord, PageInfo, error) {
	return nil, PageInfo{}, nil
}
func (m *memStore) UpsertControls(_ context.Context, c []ControlRecord) error {
	m.controls = append(m.controls, c...)
	return nil
}
func (m *memStore) GetControl(context.Context, string) (*ControlRecord, error) { return nil, nil }
func (m *memStore) SearchControls(context.Context, SearchQuery) ([]ControlRecord, PageInfo, error) {
	return nil, PageInfo{}, nil
}

// memStorage is a minimal storage.Provider backed by a byte slice.
type memStorage struct{ data []byte }

func (m *memStorage) Put(_ context.Context, _ string, r io.Reader) error {
	b, err := io.ReadAll(r)
	m.data = b
	return err
}
func (m *memStorage) Get(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(m.data)), nil
}
func (m *memStorage) Delete(context.Context, string) error         { return nil }
func (m *memStorage) Exists(context.Context, string) (bool, error) { return true, nil }
func (m *memStorage) List(context.Context, string) ([]storage.ObjectMetadata, error) {
	return nil, nil
}
func (m *memStorage) Stat(context.Context, string) (*storage.ObjectMetadata, error) {
	return nil, nil
}
func (m *memStorage) Close() error { return nil }

var _ storage.Provider = (*memStorage)(nil)

func TestParseCatalogGraphWritesSetValidFrom(t *testing.T) {
	const oscalDoc = `{"catalog":{"uuid":"11111111-1111-1111-1111-111111111111",` +
		`"metadata":{"title":"t","last-modified":"2026-08-22T00:00:00Z","version":"1","oscal-version":"1.1.2"},` +
		`"groups":[{"id":"ac","class":"family","title":"Access Control","controls":[` +
		`{"id":"ac-2","title":"Account Management","parts":[{"id":"ac-2_smt","name":"statement","prose":"p:",` +
		`"parts":[{"id":"ac-2_smt.a","name":"item","prose":"x"},{"id":"ac-2_smt.b","name":"item","prose":"x"}]}]}` +
		`]}]}}`

	graph := &captureGraph{}
	st := &memStorage{}
	// Pre-store the document so ParseCatalog's storage.Get returns it.
	if err := st.Put(context.Background(), "doc", bytes.NewReader([]byte(oscalDoc))); err != nil {
		t.Fatal(err)
	}
	svc := NewService(
		WithParser(oscal.NewParser("")),
		WithStore(&memStore{}),
		WithStorage(st),
		WithGraphDB(graph),
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)

	ctx, err := tenant.WithTenant(context.Background(), "e2e")
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.ParseCatalog(ctx, connect.NewRequest(&pb.ParseCatalogRequest{
		TenantContext: &pb.TenantContext{TenantId: "e2e"},
		DocumentId:    "doc",
		Format:        pb.CatalogFormat_CATALOG_FORMAT_OSCAL,
		CatalogName:   "e2e",
	}))
	if err != nil {
		t.Fatalf("ParseCatalog: %v", err)
	}

	if len(graph.nodes) != 3 {
		t.Fatalf("Control nodes: got %d, want 3", len(graph.nodes))
	}
	for _, n := range graph.nodes {
		if n.ValidFrom.IsZero() {
			t.Errorf("node %s has zero ValidFrom", n.ID)
		}
	}
	if len(graph.edges) != 2 {
		t.Fatalf("PARENT_OF edges: got %d, want 2", len(graph.edges))
	}
	for _, e := range graph.edges {
		if e.Label != "PARENT_OF" {
			t.Errorf("edge %s label: got %q, want PARENT_OF", e.ID, e.Label)
		}
		if e.ValidFrom.IsZero() {
			t.Errorf("edge %s has zero ValidFrom", e.ID)
		}
	}
}
