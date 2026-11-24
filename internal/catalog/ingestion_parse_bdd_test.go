//go:build !integration

package catalog_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	crosscodexv1 "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/catalog"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/oscal"
	"github.com/complytime-labs/crosscodex/pkg/storage/storagetest"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
)

// fixedStructurer returns the same items and tier for every document.
type fixedStructurer struct {
	items []oscal.ControlItem
	tier  oscal.StructureTier
	err   error
	calls int
}

func (f *fixedStructurer) Structure(ctx context.Context, doc oscal.StructuredDoc, opts oscal.StructureOptions) ([]oscal.ControlItem, error) {
	items, _, err := f.StructureWithTier(ctx, doc, opts)
	return items, err
}

func (f *fixedStructurer) StructureWithTier(context.Context, oscal.StructuredDoc, oscal.StructureOptions) ([]oscal.ControlItem, oscal.StructureTier, error) {
	f.calls++
	if f.err != nil {
		return nil, "", f.err
	}
	return f.items, f.tier, nil
}

// defaultStructuring loads the shipped catalog.structuring defaults, with an
// empty user config and a project directory holding none.
func defaultStructuring() config.StructuringConfig {
	GinkgoHelper()
	cfg, err := config.NewLoader().Load(context.Background(),
		config.WithUserConfigContent([]byte{}), config.WithProjectDir(GinkgoT().TempDir()))
	Expect(err).NotTo(HaveOccurred())
	return cfg.Catalog.Structuring
}

// outcome is one data point's attributes of crosscodex.catalog.structured.total.
type outcome struct{ tier, result string }

// structuredOutcomes sums crosscodex.catalog.structured.total by tier and result.
func structuredOutcomes(tp *telemetrytest.TestProvider) map[outcome]int64 {
	GinkgoHelper()
	m := telemetrytest.FindMetric(tp.GetMetrics(), "crosscodex.catalog.structured.total")
	Expect(m).NotTo(BeNil())
	sum, ok := m.Data.(metricdata.Sum[int64])
	Expect(ok).To(BeTrue())
	out := map[outcome]int64{}
	for _, dp := range sum.DataPoints {
		tier, _ := dp.Attributes.Value("tier")
		result, _ := dp.Attributes.Value("result")
		out[outcome{tier.AsString(), result.AsString()}] += dp.Value
	}
	return out
}

var _ = Describe("ParseCatalog with ingestion output", func() {
	const (
		tenantID = "tenant-1"
		docID    = "documents/d-1/structured.json"
	)
	var (
		ms  *mockStore
		stg *storagetest.Fake
	)

	BeforeEach(func() {
		ms = newMockStore()
		stg = &storagetest.Fake{}
	})

	newService := func(opts ...catalog.ServiceOption) *catalog.Service {
		return catalog.NewService(append([]catalog.ServiceOption{
			catalog.WithParser(oscal.NewParser("")), catalog.WithStore(ms), catalog.WithStorage(stg),
			catalog.WithStructuring(defaultStructuring()),
		}, opts...)...)
	}
	parse := func(svc *catalog.Service, data []byte, format crosscodexv1.CatalogFormat) (*crosscodexv1.ParseCatalogResponse, error) {
		Expect(stg.Put(context.Background(), docID, bytes.NewReader(data))).To(Succeed())
		resp, err := svc.ParseCatalog(context.Background(), connect.NewRequest(&crosscodexv1.ParseCatalogRequest{
			TenantContext: &crosscodexv1.TenantContext{TenantId: tenantID},
			DocumentId:    docID,
			Format:        format,
		}))
		if err != nil {
			return nil, err
		}
		return resp.Msg, nil
	}
	// seed stores one unrelated catalog, so "unchanged" is checked against
	// existing data rather than an empty store.
	seed := func() {
		Expect(ms.UpsertCatalogWithTree(context.Background(),
			catalog.CatalogRecord{CatalogID: "seed", TenantID: tenantID, Name: "Seed"},
			[]catalog.ContainerRecord{{TenantID: tenantID, CatalogID: "seed", Key: "g1"}},
			[]catalog.ControlRecord{{TenantID: tenantID, CatalogID: "seed", ControlID: "seed/x", Identifier: "x"}},
		)).To(Succeed())
	}
	snapshot := func() (map[string]catalog.CatalogRecord, map[string]catalog.ControlRecord, []catalog.ContainerRecord) {
		cats := map[string]catalog.CatalogRecord{}
		for k, v := range ms.catalogRecords {
			cats[k] = *v
		}
		ctrls := map[string]catalog.ControlRecord{}
		for k, v := range ms.controlRecords {
			ctrls[k] = *v
		}
		return cats, ctrls, append([]catalog.ContainerRecord(nil), ms.containerRecords...)
	}
	expectUnchanged := func(cats map[string]catalog.CatalogRecord, ctrls map[string]catalog.ControlRecord, conts []catalog.ContainerRecord) {
		GinkgoHelper()
		gotCats, gotCtrls, gotConts := snapshot()
		Expect(gotCats).To(Equal(cats))
		Expect(gotCtrls).To(Equal(ctrls))
		Expect(gotConts).To(Equal(conts))
	}

	DescribeTable("structures a real converted document under the default config and records its provenance",
		func(fixture string) {
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { Expect(tp.Shutdown(context.Background())).To(Succeed()) })
			data, _ := readIngestionFixture(fixture)
			var declared struct {
				SchemaVersion string `json:"schema_version"`
			}
			Expect(json.Unmarshal(data, &declared)).To(Succeed())

			resp, err := parse(newService(catalog.WithStructurer(oscal.NewStructurer(nil, nil)),
				catalog.WithMeter(tp.MeterProvider().Meter("test"))),
				data, crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Status).To(Equal(crosscodexv1.JobStatus_JOB_STATUS_COMPLETED))
			// Both conversions have headings, so the heading tier wins.
			Expect(structuredOutcomes(tp)).To(Equal(map[outcome]int64{{"heading", "structured"}: 1}))

			cat := ms.catalogRecords[resp.CatalogId]
			Expect(cat).NotTo(BeNil())
			Expect(cat.Format).To(Equal(catalog.FormatIngestion))
			Expect(cat.ObjectPath).To(Equal(docID))
			Expect(cat.SourceURI).To(Equal(docID))
			Expect(cat.ExtractorName).To(Equal(catalog.IngestionExtractorName))
			Expect(cat.ExtractorVersion).To(Equal(declared.SchemaVersion))
			Expect(cat.ContentHash).NotTo(BeEmpty())
			Expect(ms.controlRecords).NotTo(BeEmpty())
			for _, ctrl := range ms.controlRecords {
				Expect(ctrl.CatalogID).To(Equal(resp.CatalogId))
				Expect(ctrl.Title).NotTo(BeEmpty(), "control %s", ctrl.Identifier)
			}
			// No tier derives groups from headings yet, so real imports have
			// no containers.
			Expect(ms.containerRecords).To(BeEmpty())
		},
		Entry("a PDF conversion", "nist-sp800-171r3-excerpt.json"),
		Entry("an HTML conversion", "eurlex-nis2-32022L2555.json"),
	)

	It("stores Containers when items carry GroupID", func() {
		data, _ := readIngestionFixture("eurlex-nis2-32022L2555.json")
		st := &fixedStructurer{tier: oscal.StructureTierHeading, items: []oscal.ControlItem{
			{ID: "part-1", Title: "Part one", Class: oscal.ClassSection},
			{ID: "item-1", Title: "Item one", Text: "It shall.", Class: oscal.ClassRequirement, GroupID: "part-1"},
			{ID: "item-1.a", Text: "And this.", Class: oscal.ClassRequirement, ParentID: "item-1"},
		}}

		resp, err := parse(newService(catalog.WithStructurer(st)), data, crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED)
		Expect(err).NotTo(HaveOccurred())

		Expect(ms.containerRecords).To(ConsistOf(catalog.ContainerRecord{
			TenantID: tenantID, CatalogID: resp.CatalogId, Key: "g1", Identifier: "part-1", Title: "Part one",
		}))
		Expect(ms.controlRecords).To(HaveLen(2))
		Expect(ms.controlRecords[resp.CatalogId+"/item-1"].ContainerKey).To(Equal("g1"))
		Expect(ms.controlRecords[resp.CatalogId+"/item-1.a"].ContainerKey).To(Equal("g1"))
		Expect(ms.controlRecords[resp.CatalogId+"/item-1.a"].ParentID).To(Equal("item-1"))
	})

	It("counts and accepts a result from an LLM tier", func() {
		tp, err := telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(tp.Shutdown(context.Background())).To(Succeed()) })
		data, _ := readIngestionFixture("eurlex-nis2-32022L2555.json")
		st := &fixedStructurer{tier: oscal.StructureTierLLMExtract, items: []oscal.ControlItem{
			{ID: "r1", Title: "One", Text: "It shall.", Class: oscal.ClassRequirement},
		}}

		_, err = parse(newService(catalog.WithStructurer(st), catalog.WithMeter(tp.MeterProvider().Meter("test"))),
			data, crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED)
		Expect(err).NotTo(HaveOccurred())
		Expect(ms.controlRecords).To(HaveLen(1))
		Expect(structuredOutcomes(tp)).To(Equal(map[outcome]int64{{"llm-extract", "structured"}: 1}))
	})

	Context("when structuring reaches the fallback tier", func() {
		It("refuses the import, names the tier and the config, and stores nothing", func() {
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { Expect(tp.Shutdown(context.Background())).To(Succeed()) })
			seed()
			cats, ctrls, conts := snapshot()
			// No heading, table or numbering, and no Completer for the LLM
			// tiers: only paragraph splitting can produce items.
			data := ingestionDoc("1.0",
				map[string]any{"content": "Operators keep records of what they do."},
				map[string]any{"content": "Records are kept for a reasonable time."},
			)

			_, err = parse(newService(catalog.WithStructurer(oscal.NewStructurer(nil, nil)),
				catalog.WithMeter(tp.MeterProvider().Meter("test"))),
				data, crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED)

			Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
			Expect(err).To(MatchError(And(
				ContainSubstring(`the "fallback" tier`),
				ContainSubstring("catalog.structuring.section_pattern"),
				ContainSubstring("Nothing was stored"),
			)))
			expectUnchanged(cats, ctrls, conts)
			Expect(structuredOutcomes(tp)).To(Equal(map[outcome]int64{{"fallback", "refused"}: 1}))
		})
	})

	DescribeTable("fails closed without leaking detail, and counts it, when the converted document cannot be read",
		func(data []byte, hidden string) {
			seed()
			cats, ctrls, conts := snapshot()
			st := &fixedStructurer{tier: oscal.StructureTierHeading, items: []oscal.ControlItem{{ID: "x", Text: "y"}}}
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { Expect(tp.Shutdown(context.Background())).To(Succeed()) })

			_, err = parse(newService(catalog.WithStructurer(st), catalog.WithMeter(tp.MeterProvider().Meter("test"))),
				data, crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED)

			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInternal))
			Expect(structuredOutcomes(tp)).To(Equal(map[outcome]int64{{"none", "invalid_output"}: 1}))
			Expect(err).To(MatchError(ContainSubstring("the server logged the cause")))
			Expect(err).NotTo(MatchError(ContainSubstring(hidden)))
			Expect(st.calls).To(BeZero())
			expectUnchanged(cats, ctrls, conts)
		},
		Entry("an unsupported schema major", ingestionDoc("2.0", map[string]any{}), "schema_version"),
		Entry("output breaking the schema", ingestionDoc("1.0", map[string]any{"content": "  "}), "elements[0]"),
	)

	DescribeTable("rejects structured items that cannot form a tree, naming the fix, and stores nothing",
		func(items []oscal.ControlItem, want ...string) {
			seed()
			cats, ctrls, conts := snapshot()
			data, _ := readIngestionFixture("nist-sp800-171r3-excerpt.json")
			st := &fixedStructurer{tier: oscal.StructureTierHeading, items: items}

			_, err := parse(newService(catalog.WithStructurer(st)), data, crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED)

			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
			for _, w := range want {
				Expect(err).To(MatchError(ContainSubstring(w)))
			}
			expectUnchanged(cats, ctrls, conts)
		},
		Entry("duplicate IDs (BuildTree)", []oscal.ControlItem{{ID: "a", Text: "x"}, {ID: "a", Text: "y"}},
			`share the ID "a"`, "catalog.structuring.section_pattern"),
		Entry("an ID with a slash (the OSCAL tree validation)", []oscal.ControlItem{{ID: "a/b", Text: "x"}},
			"validation failed", `invalid item ID "a/b"`),
	)

	It("returns an internal error, counts it and stores nothing when the structurer fails", func() {
		seed()
		cats, ctrls, conts := snapshot()
		data, _ := readIngestionFixture("nist-sp800-171r3-excerpt.json")
		st := &fixedStructurer{err: errors.New("pattern detail /etc/secret")}
		tp, err := telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(tp.Shutdown(context.Background())).To(Succeed()) })

		_, err = parse(newService(catalog.WithStructurer(st), catalog.WithMeter(tp.MeterProvider().Meter("test"))),
			data, crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED)

		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInternal))
		Expect(structuredOutcomes(tp)).To(Equal(map[outcome]int64{{"none", "failed"}: 1}))
		Expect(err).NotTo(MatchError(ContainSubstring("/etc/secret")))
		expectUnchanged(cats, ctrls, conts)
	})

	It("refuses converted documents when no structurer is configured, and stores nothing", func() {
		seed()
		cats, ctrls, conts := snapshot()
		data, _ := readIngestionFixture("nist-sp800-171r3-excerpt.json")

		_, err := parse(newService(), data, crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED)

		Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		Expect(err).To(MatchError(ContainSubstring("document structurer not configured")))
		expectUnchanged(cats, ctrls, conts)
	})

	DescribeTable("keeps the unsupported-format error for other documents",
		func(data []byte, format crosscodexv1.CatalogFormat, advice string) {
			st := &fixedStructurer{tier: oscal.StructureTierHeading, items: []oscal.ControlItem{{ID: "x", Text: "y"}}}

			_, err := parse(newService(catalog.WithStructurer(st)), data, format)

			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(err).To(MatchError(ContainSubstring(catalog.UnsupportedFormatError(format, data).Error())))
			Expect(err).To(MatchError(ContainSubstring(advice)))
			Expect(st.calls).To(BeZero())
			Expect(ms.catalogRecords).To(BeEmpty())
		},
		Entry("plain text", []byte("Some requirement text.\nAnother line."),
			crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED, "not valid JSON"),
		Entry("JSON with only one of the ingestion keys", []byte(`{"schema_version":"1.0"}`),
			crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED, `no top-level "catalog" object`),
		Entry("ingestion output declared Gemara", ingestionDoc("1.0", map[string]any{}),
			crosscodexv1.CatalogFormat_CATALOG_FORMAT_GEMARA, "Gemara catalogs cannot be imported yet"),
		Entry("ingestion output declared OSCAL", ingestionDoc("1.0", map[string]any{}),
			crosscodexv1.CatalogFormat_CATALOG_FORMAT_OSCAL, `no top-level "catalog" object`),
	)
})
