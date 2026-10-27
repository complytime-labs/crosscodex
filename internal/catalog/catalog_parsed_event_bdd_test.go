package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"

	connect "connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/natsbus/natsbustest"
	"github.com/complytime-labs/crosscodex/pkg/oscal"
	"github.com/complytime-labs/crosscodex/pkg/storage/storagetest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// callLog records store writes and bus publishes in the order they happen,
// so a spec can assert the publish follows the control upsert.
type callLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *callLog) add(call string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, call)
}

func (l *callLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}

// editingParser returns the wrapped parser's items passed through edit, when
// set, so a spec can make the parser yield a catalog with no controls.
type editingParser struct {
	oscal.Parser
	edit func([]oscal.ControlItem) []oscal.ControlItem
}

func (p *editingParser) Parse(ctx context.Context, r io.Reader) ([]oscal.ControlItem, error) {
	items, err := p.Parser.Parse(ctx, r)
	if err != nil || p.edit == nil {
		return items, err
	}
	return p.edit(items), nil
}

// memStore is a minimal in-memory catalog Store that logs its writes.
type memStore struct {
	log      *callLog
	catalogs []CatalogRecord
	controls []ControlRecord
}

func (m *memStore) UpsertCatalog(_ context.Context, c CatalogRecord) error {
	m.catalogs = append(m.catalogs, c)
	m.log.add("upsert catalog")
	return nil
}
func (m *memStore) GetCatalog(context.Context, string) (*CatalogRecord, error) {
	return nil, nil
}
func (m *memStore) ListCatalogs(context.Context, ListOptions) ([]CatalogRecord, PageInfo, error) {
	return nil, PageInfo{}, nil
}
func (m *memStore) UpsertControls(_ context.Context, c []ControlRecord) error {
	m.controls = append(m.controls, c...)
	m.log.add("upsert controls")
	return nil
}
func (m *memStore) GetControl(context.Context, string) (*ControlRecord, error) { return nil, nil }
func (m *memStore) SearchControls(context.Context, SearchQuery) ([]ControlRecord, PageInfo, error) {
	return nil, PageInfo{}, nil
}

var _ = Describe("ParseCatalog parsed event", func() {
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
		log    *callLog
		store  *memStore
		st     *storagetest.Fake
		bus    *natsbustest.Fake
		failOn string
		parser *editingParser
	)

	BeforeEach(func() {
		var err error
		ctx, err = tenant.WithTenant(context.Background(), tenantID)
		Expect(err).NotTo(HaveOccurred())
		log = &callLog{}
		store = &memStore{log: log}
		st = &storagetest.Fake{}
		// Pre-store the document so ParseCatalog's storage.Get returns it.
		Expect(st.Put(ctx, "doc", bytes.NewReader([]byte(oscalDoc)))).To(Succeed())
		// Publishing to a subject starting with failOn fails; every other
		// publish is logged in order with the store writes. The Fake's other
		// methods keep their defaults (PublishWithHeaders, Subscribe,
		// QueueSubscribe and Close succeed), so the AfterEach below asserts
		// the service never subscribed.
		failOn = ""
		bus = &natsbustest.Fake{PublishFunc: func(_ context.Context, subject string, _ []byte) error {
			if failOn != "" && strings.HasPrefix(subject, failOn) {
				return errors.New("nats: connection closed")
			}
			log.add("publish " + subject)
			return nil
		}}
		parser = &editingParser{Parser: oscal.NewParser("")}
	})

	AfterEach(func() {
		Expect(bus.Subscriptions()).To(BeEmpty(), "ParseCatalog must only publish, never subscribe")
	})

	newService := func(opts ...ServiceOption) *Service {
		base := []ServiceOption{
			WithParser(parser),
			WithStorage(st),
			WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		}
		return NewService(append(base, opts...)...)
	}

	publishedSubjects := func() []string {
		var out []string
		for _, p := range bus.Published() {
			out = append(out, p.Subject)
		}
		return out
	}

	parse := func(svc *Service) (*connect.Response[pb.ParseCatalogResponse], error) {
		return svc.ParseCatalog(ctx, connect.NewRequest(&pb.ParseCatalogRequest{
			TenantContext: &pb.TenantContext{TenantId: tenantID},
			DocumentId:    "doc",
			Format:        pb.CatalogFormat_CATALOG_FORMAT_OSCAL,
			CatalogName:   "e2e",
		}))
	}

	It("publishes the catalog parsed event with the catalog ID and control count, and still publishes the audit event", func() {
		resp, err := parse(newService(WithStore(store), WithBus(bus)))
		Expect(err).NotTo(HaveOccurred())
		catalogID := resp.Msg.GetCatalogId()

		subject, err := natsbus.CatalogParsedSubject(tenantID, catalogID)
		Expect(err).NotTo(HaveOccurred())
		Expect(publishedSubjects()).To(ConsistOf(subject, "audit.catalog.parsed"))

		var payload struct {
			CatalogID    string `json:"catalog_id"`
			ControlCount int    `json:"control_count"`
		}
		at := slices.IndexFunc(bus.Published(), func(p natsbustest.Publication) bool { return p.Subject == subject })
		Expect(at).To(BeNumerically(">=", 0))
		Expect(json.Unmarshal(bus.Published()[at].Data, &payload)).To(Succeed())
		Expect(payload.CatalogID).To(Equal(catalogID))
		Expect(payload.ControlCount).To(Equal(len(store.controls)))
		Expect(payload.ControlCount).To(Equal(3))
	})

	It("publishes the parsed event only after the controls are stored", func() {
		resp, err := parse(newService(WithStore(store), WithBus(bus)))
		Expect(err).NotTo(HaveOccurred())
		subject, err := natsbus.CatalogParsedSubject(tenantID, resp.Msg.GetCatalogId())
		Expect(err).NotTo(HaveOccurred())

		calls := log.snapshot()
		upsertAt := slices.Index(calls, "upsert controls")
		publishAt := slices.Index(calls, "publish "+subject)
		Expect(upsertAt).To(BeNumerically(">=", 0), "controls were stored: %v", calls)
		Expect(publishAt).To(BeNumerically(">", upsertAt), "publish must follow the control upsert: %v", calls)
	})

	It("fails the import with a retryable, actionable error when the parsed event cannot be published, leaving the stored rows in place", func() {
		failOn = "crosscodex.catalog."
		resp, err := parse(newService(WithStore(store), WithBus(bus)))

		Expect(resp).To(BeNil())
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnavailable))
		Expect(store.catalogs).To(HaveLen(1))
		catalogID := store.catalogs[0].CatalogID
		Expect(err.Error()).To(ContainSubstring(catalogID))
		Expect(err.Error()).To(ContainSubstring("could not be queued"))
		Expect(err.Error()).To(ContainSubstring("Re-import the same document"))
		Expect(err.Error()).NotTo(ContainSubstring("nats:"), "bus internals are logged, not returned")

		// The rows are upserted, not rolled back: re-importing the same
		// document upserts them again and republishes, so keeping them is safe.
		Expect(store.controls).To(HaveLen(3))
		for _, c := range store.controls {
			Expect(c.CatalogID).To(Equal(catalogID))
		}
		Expect(publishedSubjects()).To(ConsistOf(HavePrefix("crosscodex.catalog.")), "only the failed parsed-event attempt reached the bus, not the audit event")
		Expect(log.snapshot()).NotTo(ContainElement(HavePrefix("publish ")), "no publish succeeded")
	})

	It("succeeds without publishing when no bus is configured", func() {
		resp, err := parse(newService(WithStore(store)))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.GetCatalogId()).NotTo(BeEmpty())
		Expect(store.controls).To(HaveLen(3))
	})

	It("does not publish the parsed event when no store is configured, because no rows exist to project", func() {
		_, err := parse(newService(WithBus(bus)))
		Expect(err).NotTo(HaveOccurred())
		Expect(publishedSubjects()).To(ConsistOf("audit.catalog.parsed"))
	})

	It("does not publish the parsed event for a catalog with no controls, because the graph role rejects an empty projection", func() {
		parser.edit = func([]oscal.ControlItem) []oscal.ControlItem { return nil }
		_, err := parse(newService(WithStore(store), WithBus(bus)))
		Expect(err).NotTo(HaveOccurred())
		Expect(store.controls).To(BeEmpty())
		Expect(publishedSubjects()).To(ConsistOf("audit.catalog.parsed"))
	})
})
