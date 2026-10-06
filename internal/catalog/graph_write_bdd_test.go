package catalog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	connect "connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/graphdb/memdriver"
	"github.com/complytime-labs/crosscodex/pkg/oscal"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// countingGraph wraps a memdriver graph and counts the nodes UpsertNode
// reports as created, so a spec can tell an update from a duplicate. A
// non-nil upsertErr or edgeErr is returned by UpsertNode or CreateEdge
// instead of writing, so a spec can make the graph write fail.
type countingGraph struct {
	graphdb.GraphDB
	created   int
	upsertErr error
	edgeErr   error
}

func (c *countingGraph) UpsertNode(ctx context.Context, tenantID string, n graphdb.Node) (bool, error) {
	if c.upsertErr != nil {
		return false, c.upsertErr
	}
	created, err := c.GraphDB.UpsertNode(ctx, tenantID, n)
	if created {
		c.created++
	}
	return created, err
}

func (c *countingGraph) CreateEdge(ctx context.Context, tenantID, sourceID, targetID string, e graphdb.Edge) error {
	if c.edgeErr != nil {
		return c.edgeErr
	}
	return c.GraphDB.CreateEdge(ctx, tenantID, sourceID, targetID, e)
}

// revisingParser returns the wrapped parser's items, with every title and
// statement rewritten after the first call. It stands in for a parser change
// that extracts different text from the same document bytes, which is the
// re-import that keeps the catalog ID and control IDs.
type revisingParser struct {
	oscal.Parser
	calls int
}

func (p *revisingParser) Parse(ctx context.Context, r io.Reader) ([]oscal.ControlItem, error) {
	items, err := p.Parser.Parse(ctx, r)
	p.calls++
	if err != nil || p.calls == 1 {
		return items, err
	}
	for i := range items {
		items[i].Title += " (revised)"
		items[i].Text += " revised"
	}
	return items, nil
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

var _ = Describe("ParseCatalog graph writes", func() {
	const (
		tenantID = "e2e"
		oscalDoc = `{"catalog":{"uuid":"11111111-1111-1111-1111-111111111111",` +
			`"metadata":{"title":"t","last-modified":"2026-08-22T00:00:00Z","version":"1","oscal-version":"1.1.2"},` +
			`"groups":[{"id":"ac","class":"family","title":"Access Control","controls":[` +
			`{"id":"ac-2","title":"Account Management","parts":[{"id":"ac-2_smt","name":"statement","prose":"p:",` +
			`"parts":[{"id":"ac-2_smt.a","name":"item","prose":"x"},{"id":"ac-2_smt.b","name":"item","prose":"x"}]}]}` +
			`]}]}}`
	)

	var (
		ctx    context.Context
		graph  *countingGraph
		parser *revisingParser
		svc    *Service
		logBuf *bytes.Buffer
	)

	BeforeEach(func() {
		var err error
		ctx, err = tenant.WithTenant(context.Background(), tenantID)
		Expect(err).NotTo(HaveOccurred())
		graph = &countingGraph{GraphDB: memdriver.New()}
		Expect(graph.CreateGraph(ctx, tenantID)).To(Succeed())
		st := &memStorage{}
		// Pre-store the document so ParseCatalog's storage.Get returns it.
		Expect(st.Put(ctx, "doc", bytes.NewReader([]byte(oscalDoc)))).To(Succeed())
		parser = &revisingParser{Parser: oscal.NewParser("")}
		logBuf = &bytes.Buffer{}
		svc = NewService(
			WithParser(parser),
			WithStore(&memStore{}),
			WithStorage(st),
			WithGraphDB(graph),
			WithLogger(slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))),
		)
	})

	ingest := func() string {
		resp, err := svc.ParseCatalog(ctx, connect.NewRequest(&pb.ParseCatalogRequest{
			TenantContext: &pb.TenantContext{TenantId: tenantID},
			DocumentId:    "doc",
			Format:        pb.CatalogFormat_CATALOG_FORMAT_OSCAL,
			CatalogName:   "e2e",
		}))
		Expect(err).NotTo(HaveOccurred())
		return resp.Msg.GetCatalogId()
	}

	parentEdges := func() []graphdb.Relationship {
		rels, err := graph.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{EdgeLabel: "PARENT_OF"})
		Expect(err).NotTo(HaveOccurred())
		return rels
	}

	It("writes Control nodes and PARENT_OF edges with ValidFrom set", func() {
		catalogID := ingest()

		Expect(graph.created).To(Equal(3), "Control nodes")
		rels := parentEdges()
		Expect(rels).To(HaveLen(2), "PARENT_OF edges")
		for _, r := range rels {
			Expect(r.Source.ID).To(Equal(catalogID + "/ac-2"))
			Expect(r.Edge.ID).To(Equal(graphdb.DerivedID("parent-of", r.Source.ID, r.Target.ID)))
			Expect(r.Edge.ValidFrom.IsZero()).To(BeFalse(), "edge %s has zero ValidFrom", r.Edge.ID)
			for _, n := range []graphdb.Node{r.Source, r.Target} {
				Expect(n.Label).To(Equal("Control"))
				Expect(n.ValidFrom.IsZero()).To(BeFalse(), "node %s has zero ValidFrom", n.ID)
			}
		}
	})

	It("refreshes Control nodes in place when the same document is re-imported with new text", func() {
		catalogID := ingest()
		first, err := graph.GetNode(ctx, tenantID, catalogID+"/ac-2")
		Expect(err).NotTo(HaveOccurred())
		Expect(first.Properties).To(HaveKeyWithValue("title", "Account Management"))

		Expect(ingest()).To(Equal(catalogID), "same document bytes keep the catalog ID")

		Expect(graph.created).To(Equal(3), "re-import must update the 3 nodes, not add new ones")
		got, err := graph.GetNode(ctx, tenantID, catalogID+"/ac-2")
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Properties).To(HaveKeyWithValue("title", "Account Management (revised)"))
		Expect(got.Properties).To(HaveKeyWithValue("statement", HaveSuffix(" revised")))
		Expect(got.Properties["statement"]).NotTo(Equal(first.Properties["statement"]))
		Expect(got.ValidFrom.Equal(first.ValidFrom)).To(BeTrue(), "re-import keeps the first import's valid_from")

		rels := parentEdges()
		Expect(rels).To(HaveLen(2), "PARENT_OF edges survive the update")
		for _, r := range rels {
			Expect(r.Source.Properties).To(HaveKeyWithValue("title", "Account Management (revised)"))
			Expect(r.Target.Properties).To(HaveKeyWithValue("title", HaveSuffix(" (revised)")))
		}
	})

	It("re-ingesting the same catalog keeps one PARENT_OF edge per pair and logs no edge warning", func() {
		catalogID := ingest()
		Expect(ingest()).To(Equal(catalogID))

		Expect(logBuf.String()).NotTo(ContainSubstring("create graph edge failed"),
			"an existing PARENT_OF edge is the expected re-import outcome, not a warning")
		rels := parentEdges()
		Expect(rels).To(HaveLen(2), "PARENT_OF edges after two ingests")
		pairs := map[string]bool{}
		for _, r := range rels {
			Expect(r.Edge.ID).To(Equal(graphdb.DerivedID("parent-of", r.Source.ID, r.Target.ID)))
			pairs[r.Source.ID+" -> "+r.Target.ID] = true
		}
		Expect(pairs).To(HaveLen(2), "one edge per parent/child pair")
	})

	DescribeTable("logs a failed PARENT_OF write unless the edge already exists, and still ingests",
		func(edgeErr error, wantWarnings int) {
			graph.edgeErr = edgeErr
			ingest()

			Expect(strings.Count(logBuf.String(), "create graph edge failed")).To(Equal(wantWarnings))
			if wantWarnings > 0 {
				Expect(logBuf.String()).To(ContainSubstring("from=ac-2"))
				Expect(logBuf.String()).To(ContainSubstring(edgeErr.Error()))
			}
			Expect(parentEdges()).To(BeEmpty(), "the injected error stops every edge write")
			Expect(graph.created).To(Equal(3), "a failed edge must not stop the node upserts")
		},
		Entry("a driver error", errors.New("injected edge failure"), 2),
		Entry("a missing endpoint, proving ErrNodeNotFound is logged and not filtered like ErrEdgeExists", fmt.Errorf("create edge: source: %w", graphdb.ErrNodeNotFound), 2),
		Entry("an existing edge, which is a re-import and not a failure", fmt.Errorf("create edge: %w", graphdb.ErrEdgeExists), 0),
	)

	It("logs every failed Control node upsert and still ingests", func() {
		graph.upsertErr = errors.New("injected upsert failure")
		catalogID := ingest()

		Expect(strings.Count(logBuf.String(), "upsert graph node failed")).To(Equal(3), "one warning per Control item")
		Expect(logBuf.String()).To(ContainSubstring("control_id=ac-2"))
		Expect(logBuf.String()).To(ContainSubstring("injected upsert failure"))
		_, err := graph.GetNode(ctx, tenantID, catalogID+"/ac-2")
		Expect(err).To(MatchError(graphdb.ErrNodeNotFound), "a failed upsert must not store the node")
	})
})
