//go:build !integration

package catalog_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

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
			// The CLI's content-hash command prints ContentIdentity; it must
			// equal what the daemon records.
			kind, hash, err := catalog.ContentIdentity(data)
			Expect(err).NotTo(HaveOccurred())
			Expect(kind).To(Equal(catalog.ContentKindIngestion))
			Expect(cat.ContentHash).To(Equal(hash))
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

	Context("when the output names its own controls and groups (schema 1.3 roles)", func() {
		asvsDoc := func() []byte {
			return withCatalogMeta(ingestionDoc("1.3",
				withRole(heading("e-1", 1, nil, "Encoding"), "V1", "group", "Encoding", nil, nil),
				withRole(heading("e-3", 2, "e-1", "Architecture"), "V1.1", "group", "Architecture", nil, nil),
				withRole(map[string]any{"element_id": "e-4", "content": "Verify one.", "parent_id": "e-3"},
					"V1.1.1", "control", nil, "Verify that one thing holds.", nil),
				withRole(map[string]any{"element_id": "e-5", "content": "Verify two.", "parent_id": "e-3"},
					"V1.1.2", "control", nil, "Verify that two things hold. Also, a second sentence follows here.", nil),
			), "Test Verification Standard", "5.0.0")
		}
		parseNamed := func(svc *catalog.Service, data []byte, name string) *crosscodexv1.ParseCatalogResponse {
			GinkgoHelper()
			Expect(stg.Put(context.Background(), docID, bytes.NewReader(data))).To(Succeed())
			resp, err := svc.ParseCatalog(context.Background(), connect.NewRequest(&crosscodexv1.ParseCatalogRequest{
				TenantContext: &crosscodexv1.TenantContext{TenantId: tenantID},
				DocumentId:    docID,
				CatalogName:   name,
			}))
			Expect(err).NotTo(HaveOccurred())
			return resp.Msg
		}

		It("stores the source's own IDs and groups without running the structurer", func() {
			st := &fixedStructurer{tier: oscal.StructureTierHeading, items: []oscal.ControlItem{{ID: "x", Text: "y"}}}

			resp, err := parse(newService(catalog.WithStructurer(st)), asvsDoc(), crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED)
			Expect(err).NotTo(HaveOccurred())

			Expect(st.calls).To(BeZero())
			Expect(ms.controlRecords).To(HaveLen(2))
			Expect(ms.controlRecords).To(HaveKey(resp.CatalogId + "/V1.1.1"))
			Expect(ms.controlRecords).To(HaveKey(resp.CatalogId + "/V1.1.2"))
			Expect(ms.containerRecords).To(HaveLen(2))
			var identifiers []string
			for _, c := range ms.containerRecords {
				identifiers = append(identifiers, c.Identifier)
			}
			Expect(identifiers).To(ConsistOf("V1", "V1.1"))
			Expect(ms.catalogRecords[resp.CatalogId].Format).To(Equal(catalog.FormatIngestion))
			Expect(ms.catalogRecords[resp.CatalogId].ExtractorVersion).To(Equal("1.3"))
		})

		It("names the catalog and its version from the output's metadata", func() {
			resp := parseNamed(newService(catalog.WithStructurer(oscal.NewStructurer(nil, nil))), asvsDoc(), "")

			Expect(ms.catalogRecords[resp.CatalogId].Name).To(Equal("Test Verification Standard"))
			Expect(ms.catalogRecords[resp.CatalogId].Version).To(Equal("5.0.0"))
		})

		It("lets the request's catalog_name win over the output's title", func() {
			resp := parseNamed(newService(catalog.WithStructurer(oscal.NewStructurer(nil, nil))), asvsDoc(), "Operator Name")

			Expect(ms.catalogRecords[resp.CatalogId].Name).To(Equal("Operator Name"))
			Expect(ms.catalogRecords[resp.CatalogId].Version).To(Equal("5.0.0"))
		})

		DescribeTable("labels a document version from the output's catalog_version",
			func(catalogVersion any, expectLabel func(label string) bool) {
				data := withCatalogMeta(asvsDoc(), "Test Verification Standard", catalogVersion)
				Expect(stg.Put(context.Background(), docID, bytes.NewReader(data))).To(Succeed())

				resp, err := newService(catalog.WithStructurer(oscal.NewStructurer(nil, nil))).ParseCatalog(context.Background(),
					connect.NewRequest(&crosscodexv1.ParseCatalogRequest{
						TenantContext: &crosscodexv1.TenantContext{TenantId: tenantID},
						DocumentId:    docID,
						DocumentSlug:  "test-verification-standard",
					}))
				Expect(err).NotTo(HaveOccurred())

				Expect(expectLabel(resp.Msg.VersionLabel)).To(BeTrue(), "label %q", resp.Msg.VersionLabel)
			},
			Entry("a catalog_version is the label", "5.0.0", func(l string) bool { return l == "5.0.0" }),
			Entry("a null catalog_version gets the date and hash label", nil, func(l string) bool {
				return regexp.MustCompile(`^\d{4}-\d{2}-\d{2} · [0-9a-f]{6}$`).MatchString(l)
			}),
		)

		DescribeTable("labels a document version from a real release's catalog_version",
			func(fixture, mapping, wantLabel string) {
				data, _ := readIngestionFixture(fixture)
				Expect(stg.Put(context.Background(), docID, bytes.NewReader(data))).To(Succeed())

				resp, err := newService(catalog.WithStructurer(oscal.NewStructurer(nil, nil))).ParseCatalog(context.Background(),
					connect.NewRequest(&crosscodexv1.ParseCatalogRequest{
						TenantContext:    &crosscodexv1.TenantContext{TenantId: tenantID},
						DocumentId:       docID,
						DocumentSlug:     "release-fixture",
						IngestionMapping: mapping,
					}))
				Expect(err).NotTo(HaveOccurred())

				Expect(resp.Msg.VersionLabel).To(Equal(wantLabel))
			},
			Entry("ASVS 5.0.0 release", "owasp-asvs-5.0.0-excerpt.json", "owasp-asvs-5.0.0-json", "5.0.0"),
			Entry("SP 800-53 Rev. 5 release: a label with a space is valid", "nist-sp800-53r5-excerpt.json", "nist-sp800-53r5-catalog-xlsx", "Revision 5"),
		)

		It("stores every control unchanged under filtering and decomposition", func() {
			cfg := defaultStructuring()
			cfg.FilterByKeywords, cfg.Decompose = true, true

			resp, err := parse(newService(catalog.WithStructurer(oscal.NewStructurer(nil, nil)), catalog.WithStructuring(cfg)),
				asvsDoc(), crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED)
			Expect(err).NotTo(HaveOccurred())

			Expect(ms.controlRecords).To(HaveLen(2))
			Expect(ms.controlRecords[resp.CatalogId+"/V1.1.1"].Statement).To(Equal("Verify that one thing holds."))
			Expect(ms.controlRecords[resp.CatalogId+"/V1.1.2"].Statement).To(Equal("Verify that two things hold. Also, a second sentence follows here."))
		})

		It("counts the source-ids tier and records it on the span", func() {
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { Expect(tp.Shutdown(context.Background())).To(Succeed()) })

			parseNamed(newService(catalog.WithStructurer(oscal.NewStructurer(nil, nil)),
				catalog.WithTracer(tp.TracerProvider().Tracer("catalog")),
				catalog.WithMeter(tp.MeterProvider().Meter("test"))), asvsDoc(), "")

			Expect(structuredOutcomes(tp)).To(Equal(map[outcome]int64{{"source-ids", "structured"}: 1}))
			span := telemetrytest.FindSpan(tp.GetSpans(), "catalog.ParseCatalog")
			Expect(span).NotTo(BeNil())
			tier, ok := telemetrytest.SpanAttribute(span, "structure.tier")
			Expect(ok).To(BeTrue())
			Expect(tier.AsString()).To(Equal("source-ids"))
		})

		It("fails closed without leaking detail when a group has no children, and stores nothing", func() {
			seed()
			cats, ctrls, conts := snapshot()
			data := ingestionDoc("1.3",
				withRole(heading("e-1", 1, nil, "Sensitive heading"), "SID-1", "group", "Sensitive title", nil, nil))
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { Expect(tp.Shutdown(context.Background())).To(Succeed()) })

			_, err = parse(newService(catalog.WithStructurer(oscal.NewStructurer(nil, nil)),
				catalog.WithMeter(tp.MeterProvider().Meter("test"))),
				data, crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED)

			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInternal))
			Expect(err).To(MatchError(ContainSubstring("the server logged the cause")))
			Expect(err).NotTo(MatchError(ContainSubstring("SID-1")))
			Expect(structuredOutcomes(tp)).To(Equal(map[outcome]int64{{"none", "invalid_output"}: 1}))
			expectUnchanged(cats, ctrls, conts)
		})

		It("blames the converter, not section_pattern, when the items clash, and stores nothing", func() {
			seed()
			cats, ctrls, conts := snapshot()
			data := ingestionDoc("1.3",
				withRole(map[string]any{"element_id": "e-1", "content": "One."}, "dup", "control", nil, "One.", nil),
				withRole(map[string]any{"element_id": "e-2", "content": "Two."}, "dup", "control", nil, "Two.", nil))

			_, err := parse(newService(catalog.WithStructurer(oscal.NewStructurer(nil, nil))),
				data, crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED)

			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(err).To(MatchError(And(
				ContainSubstring(`share the ID "dup"`),
				ContainSubstring("re-run the conversion"),
				ContainSubstring("Nothing was stored"),
			)))
			Expect(err).NotTo(MatchError(ContainSubstring("section_pattern")))
			Expect(err).NotTo(MatchError(ContainSubstring("decompose")))
			expectUnchanged(cats, ctrls, conts)
		})
	})

	Context("when the import names a mapping", func() {
		const (
			asvsMapping = "owasp-asvs-5.0.0-json"
			asvsSHA     = "de791a43657db03a3a728c4ec1bc14b31d400c0cfe70958fbe7d958847a0ec36"
		)
		parseMapped := func(svc *catalog.Service, documentID string, data []byte, mapping, slug string) (*crosscodexv1.ParseCatalogResponse, error) {
			GinkgoHelper()
			Expect(stg.Put(context.Background(), documentID, bytes.NewReader(data))).To(Succeed())
			resp, err := svc.ParseCatalog(context.Background(), connect.NewRequest(&crosscodexv1.ParseCatalogRequest{
				TenantContext:    &crosscodexv1.TenantContext{TenantId: tenantID},
				DocumentId:       documentID,
				IngestionMapping: mapping,
				DocumentSlug:     slug,
			}))
			if err != nil {
				return nil, err
			}
			return resp.Msg, nil
		}
		// withProcessingTime returns doc with metadata.processing_time_ms replaced.
		withProcessingTime := func(doc []byte, ms float64) []byte {
			GinkgoHelper()
			var m map[string]any
			Expect(json.Unmarshal(doc, &m)).To(Succeed())
			m["metadata"].(map[string]any)["processing_time_ms"] = ms
			out, err := json.Marshal(m)
			Expect(err).NotTo(HaveOccurred())
			return out
		}
		newSvc := func() *catalog.Service {
			return newService(catalog.WithStructurer(oscal.NewStructurer(nil, nil)))
		}

		It("keeps one catalog across conversions that differ only in timing", func() {
			data, _ := readIngestionFixture("owasp-asvs-5.0.0-excerpt.json")
			_, meta, err := catalog.AdaptIngestionOutput(data)
			Expect(err).NotTo(HaveOccurred())
			svc := newSvc()

			first, err := parseMapped(svc, "documents/d-1/structured.json", data, asvsMapping, "")
			Expect(err).NotTo(HaveOccurred())
			controls := len(ms.controlRecords)
			second, err := parseMapped(svc, "documents/d-2/structured.json", withProcessingTime(data, 1234.5), asvsMapping, "")
			Expect(err).NotTo(HaveOccurred())

			Expect(second.CatalogId).To(Equal(first.CatalogId))
			Expect(ms.catalogRecords).To(HaveLen(1))
			Expect(ms.catalogRecords[first.CatalogId].ContentHash).To(Equal(meta.ContentHash))
			Expect(ms.controlRecords).To(HaveLen(controls))
			Expect(ms.catalogRecords[first.CatalogId].ObjectPath).To(Equal("documents/d-2/structured.json"))
		})

		It("does not supersede or re-version a document re-imported after a timing-only change", func() {
			data, _ := readIngestionFixture("owasp-asvs-5.0.0-excerpt.json")
			svc := newSvc()

			first, err := parseMapped(svc, "documents/d-1/structured.json", data, asvsMapping, "asvs")
			Expect(err).NotTo(HaveOccurred())
			second, err := parseMapped(svc, "documents/d-2/structured.json", withProcessingTime(data, 1234.5), asvsMapping, "asvs")
			Expect(err).NotTo(HaveOccurred())

			Expect(second.SupersededCatalogId).To(BeEmpty())
			Expect(second.CatalogId).To(Equal(first.CatalogId))
			Expect(second.Version).To(Equal(first.Version))
		})

		It("returns the mapping sha256", func() {
			data, _ := readIngestionFixture("owasp-asvs-5.0.0-excerpt.json")

			resp, err := parseMapped(newSvc(), docID, data, asvsMapping, "")

			Expect(err).NotTo(HaveOccurred())
			Expect(resp.GetIngestionMappingSha256()).To(Equal(asvsSHA))
		})

		It("leaves the mapping sha256 empty when no mapping was requested", func() {
			data, _ := readIngestionFixture("nist-sp800-171r3-excerpt.json")

			resp, err := parseMapped(newSvc(), docID, data, "", "")

			Expect(err).NotTo(HaveOccurred())
			Expect(resp.GetIngestionMappingSha256()).To(BeEmpty())
		})

		DescribeTable("refuses a mapping the output did not use",
			func(fixture, requested, named string) {
				seed()
				cats, ctrls, conts := snapshot()
				data, _ := readIngestionFixture(fixture)

				_, err := parseMapped(newSvc(), docID, data, requested, "")

				Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
				Expect(err).To(MatchError(And(
					ContainSubstring(fmt.Sprintf("mapping %q", requested)),
					ContainSubstring(fmt.Sprintf("%q", named)),
					ContainSubstring("re-run the conversion"),
					ContainSubstring("Nothing was stored"),
				)))
				expectUnchanged(cats, ctrls, conts)
				_, getErr := stg.Get(context.Background(), docID)
				Expect(getErr).NotTo(HaveOccurred())
			},
			Entry("a mapping requested but the output names none", "nist-sp800-171r3-excerpt.json", "owasp-asvs-5.0.0-json", ""),
			Entry("no mapping requested but the output names one", "owasp-asvs-5.0.0-excerpt.json", "", "owasp-asvs-5.0.0-json"),
			Entry("a different mapping requested", "owasp-asvs-5.0.0-excerpt.json", "other-mapping", "owasp-asvs-5.0.0-json"),
		)

		It("refuses a mapping with OSCAL content, and stores nothing", func() {
			seed()
			cats, ctrls, conts := snapshot()
			data, err := os.ReadFile(filepath.Join("..", "..", "pkg", "oscal", "testdata", "minimal_catalog.json"))
			Expect(err).NotTo(HaveOccurred())

			_, err = parseMapped(newSvc(), docID, data, "x", "")

			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(err).To(MatchError(And(
				ContainSubstring("applies only to converted structured sources"),
				ContainSubstring("Nothing was stored"),
			)))
			expectUnchanged(cats, ctrls, conts)
		})
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

	Context("when the output is schema 1.4 and says how it was structured", func() {
		const (
			guessed1_4 = "eurlex-nis2-32022L2555-1.4.json"
			mapped1_4  = "owasp-asvs-5.0.0-excerpt-1.4.json"
		)
		newSvc := func(cfg config.StructuringConfig, opts ...catalog.ServiceOption) *catalog.Service {
			return newService(append([]catalog.ServiceOption{
				catalog.WithStructurer(oscal.NewStructurer(nil, nil)), catalog.WithStructuring(cfg)}, opts...)...)
		}
		parseWith := func(svc *catalog.Service, data []byte, mapping string) (*crosscodexv1.ParseCatalogResponse, error) {
			GinkgoHelper()
			Expect(stg.Put(context.Background(), docID, bytes.NewReader(data))).To(Succeed())
			resp, err := svc.ParseCatalog(context.Background(), connect.NewRequest(&crosscodexv1.ParseCatalogRequest{
				TenantContext:    &crosscodexv1.TenantContext{TenantId: tenantID},
				DocumentId:       docID,
				IngestionMapping: mapping,
			}))
			if err != nil {
				return nil, err
			}
			return resp.Msg, nil
		}
		// refusal is the message for the settings named, in the plan's order.
		refusal := func(method, names string) string {
			return fmt.Sprintf("cannot import document %q: ingestion already structured it (method %s), so these catalog.structuring settings no longer apply: %s. "+
				"Keep crosscodex-ingestion below schema 1.4 until document mappings exist (crosscodex-ingestion#38 phase 3), "+
				"or reset (or ask an operator to reset) those settings to their defaults to accept ingestion's structure. Nothing was stored", docID, method, names)
		}

		DescribeTable("refuses a guessed structure when a setting it makes moot is not at its default, and stores nothing",
			func(change func(*config.StructuringConfig), names string) {
				seed()
				cats, ctrls, conts := snapshot()
				data, _ := readIngestionFixture(guessed1_4)
				cfg := defaultStructuring()
				change(&cfg)
				tp, err := telemetrytest.NewTestProvider()
				Expect(err).NotTo(HaveOccurred())
				DeferCleanup(func() { Expect(tp.Shutdown(context.Background())).To(Succeed()) })

				_, err = parseWith(newSvc(cfg, catalog.WithTracer(tp.TracerProvider().Tracer("catalog")),
					catalog.WithMeter(tp.MeterProvider().Meter("test"))), data, "")

				Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
				var cerr *connect.Error
				Expect(errors.As(err, &cerr)).To(BeTrue())
				Expect(cerr.Message()).To(Equal(refusal("heading", names)))
				Expect(structuredOutcomes(tp)).To(Equal(map[outcome]int64{{"ingestion_heading", "refused"}: 1}))
				span := telemetrytest.FindSpan(tp.GetSpans(), "catalog.ParseCatalog")
				Expect(span).NotTo(BeNil())
				tier, ok := telemetrytest.SpanAttribute(span, "structure.tier")
				Expect(ok).To(BeTrue())
				Expect(tier.AsString()).To(Equal("ingestion_heading"))
				expectUnchanged(cats, ctrls, conts)
			},
			Entry("section_pattern", func(c *config.StructuringConfig) { c.SectionPattern = `^(\S+)\s+(.*)$` }, "section_pattern"),
			Entry("decompose", func(c *config.StructuringConfig) { c.Decompose = !c.Decompose }, "decompose"),
			Entry("min_decompose_words", func(c *config.StructuringConfig) { c.MinDecomposeWords++ }, "min_decompose_words"),
			Entry("filter_by_keywords", func(c *config.StructuringConfig) { c.FilterByKeywords = !c.FilterByKeywords }, "filter_by_keywords"),
			Entry("keywords", func(c *config.StructuringConfig) { c.Keywords = []string{"shall"} }, "keywords"),
			Entry("max_heading_repeats alone", func(c *config.StructuringConfig) { c.MaxHeadingRepeats = 5 }, "max_heading_repeats"),
			Entry("print_artifact_patterns", func(c *config.StructuringConfig) { c.PrintArtifactPatterns = []string{`^Page \d+$`} }, "print_artifact_patterns"),
			Entry("two settings, listed in the documented order", func(c *config.StructuringConfig) {
				c.PrintArtifactPatterns = []string{`^Page \d+$`}
				c.Decompose = !c.Decompose
				c.SectionPattern = `^(\S+)\s+(.*)$`
			}, "section_pattern, decompose, print_artifact_patterns"),
		)

		DescribeTable("rejects source-declared controls that cannot form a tree, with advice that fits who structured them",
			func(version, method string, wantAdvice, forbidAdvice string) {
				seed()
				cats, ctrls, conts := snapshot()
				data := ingestionDoc(version,
					withRole(map[string]any{"element_id": "e-1", "content": "One."}, "c1", "control", nil, "One.", nil),
					withRole(map[string]any{"element_id": "e-2", "content": "Two."}, "c1", "control", nil, "Two.", nil))
				if method != "" {
					data = withStructureMethod(data, method)
				}

				_, err := parseWith(newSvc(defaultStructuring()), data, "")

				Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
				Expect(err).To(MatchError(ContainSubstring(`share the ID "c1"`)))
				Expect(err).To(MatchError(ContainSubstring(wantAdvice)))
				Expect(err).NotTo(MatchError(ContainSubstring(forbidAdvice)))
				Expect(err).To(MatchError(ContainSubstring("Nothing was stored")))
				expectUnchanged(cats, ctrls, conts)
			},
			Entry("a guessed heading structure", "1.4", "heading",
				"report it as an ingestion service bug", "fix the mapping"),
			Entry("a guessed table structure", "1.4", "table",
				"crosscodex-ingestion#38 phase 3", "re-run the conversion"),
			Entry("a guessed pattern structure", "1.4", "pattern",
				"does not form a catalog tree", "fix the mapping"),
			Entry("a 1.3 source-ID output keeps the mapping advice", "1.3", "",
				"re-run the conversion or fix the mapping", "ingestion service bug"),
		)

		It("accepts a guessed structure under the default settings", func() {
			data, _ := readIngestionFixture(guessed1_4)

			_, err := parseWith(newSvc(defaultStructuring()), data, "")

			Expect(err).NotTo(HaveOccurred())
			Expect(ms.controlRecords).NotTo(BeEmpty())
		})

		It("does not refuse non-default settings for a mapped source, whose method is mapping", func() {
			data, _ := readIngestionFixture(mapped1_4)
			cfg := defaultStructuring()
			cfg.Decompose, cfg.FilterByKeywords, cfg.MaxHeadingRepeats = !cfg.Decompose, true, 5

			resp, err := parseWith(newSvc(cfg), data, "owasp-asvs-5.0.0-json")

			Expect(err).NotTo(HaveOccurred())
			Expect(resp.IngestionStructureMethod).To(Equal("mapping"))
			Expect(ms.controlRecords).To(HaveLen(43))
		})

		It("does not refuse non-default settings for a pre-1.4 document, which still runs the cascade", func() {
			data, _ := readIngestionFixture("eurlex-nis2-32022L2555.json")
			cfg := defaultStructuring()
			cfg.Decompose, cfg.MaxHeadingRepeats = !cfg.Decompose, 5

			resp, err := parseWith(newSvc(cfg), data, "")

			Expect(err).NotTo(HaveOccurred())
			Expect(resp.IngestionStructureMethod).To(BeEmpty())
			Expect(ms.controlRecords).NotTo(BeEmpty())
		})

		DescribeTable("labels the structuring tier on the metric and the span",
			func(data func() []byte, wantTier string) {
				tp, err := telemetrytest.NewTestProvider()
				Expect(err).NotTo(HaveOccurred())
				DeferCleanup(func() { Expect(tp.Shutdown(context.Background())).To(Succeed()) })

				_, err = parseWith(newSvc(defaultStructuring(),
					catalog.WithTracer(tp.TracerProvider().Tracer("catalog")),
					catalog.WithMeter(tp.MeterProvider().Meter("test"))), data(), "")

				Expect(err).NotTo(HaveOccurred())
				Expect(structuredOutcomes(tp)).To(Equal(map[outcome]int64{{wantTier, "structured"}: 1}))
				span := telemetrytest.FindSpan(tp.GetSpans(), "catalog.ParseCatalog")
				Expect(span).NotTo(BeNil())
				tier, ok := telemetrytest.SpanAttribute(span, "structure.tier")
				Expect(ok).To(BeTrue())
				Expect(tier.AsString()).To(Equal(wantTier))
			},
			Entry("a 1.4 output that carries a method", func() []byte {
				data, _ := readIngestionFixture(guessed1_4)
				return data
			}, "ingestion_heading"),
			Entry("a 1.3 output with roles keeps source-ids", func() []byte {
				return ingestionDoc("1.3",
					withRole(map[string]any{"element_id": "e-1", "content": "One."}, "c1", "control", nil, "One.", nil))
			}, "source-ids"),
		)

		DescribeTable("reports the method in the response",
			func(fixture, mapping, want string) {
				data, _ := readIngestionFixture(fixture)

				resp, err := parseWith(newSvc(defaultStructuring()), data, mapping)

				Expect(err).NotTo(HaveOccurred())
				Expect(resp.IngestionStructureMethod).To(Equal(want))
			},
			Entry("a guessed 1.4 structure", guessed1_4, "", "heading"),
			Entry("a mapped 1.4 structure", mapped1_4, "owasp-asvs-5.0.0-json", "mapping"),
			Entry("a 1.3 output, which has no method", "owasp-asvs-5.0.0-excerpt.json", "owasp-asvs-5.0.0-json", ""),
		)

		DescribeTable("reports the HTML profile the conversion applied, and only that",
			func(fixture string, wantProfile bool) {
				data, _ := readIngestionFixture(fixture)

				resp, err := parseWith(newSvc(defaultStructuring()), data, "")

				Expect(err).NotTo(HaveOccurred())
				if wantProfile {
					// The expected pair is read from the fixture itself.
					var fixtureMeta struct {
						Metadata struct {
							HTMLProfile struct {
								ID     string `json:"id"`
								SHA256 string `json:"sha256"`
							} `json:"html_profile"`
						} `json:"metadata"`
					}
					Expect(json.Unmarshal(data, &fixtureMeta)).To(Succeed())
					Expect(fixtureMeta.Metadata.HTMLProfile.ID).NotTo(BeEmpty())
					Expect(resp.IngestionHtmlProfile).To(Equal(fixtureMeta.Metadata.HTMLProfile.ID))
					Expect(resp.IngestionHtmlProfileSha256).To(Equal(fixtureMeta.Metadata.HTMLProfile.SHA256))
				} else {
					Expect(resp.IngestionHtmlProfile).To(BeEmpty())
					Expect(resp.IngestionHtmlProfileSha256).To(BeEmpty())
				}
			},
			Entry("a 1.4 HTML conversion", guessed1_4, true),
			Entry("a 1.2 HTML conversion", "eurlex-nis2-32022L2555.json", true),
			Entry("a PDF conversion, which applies no profile", "nist-sp800-171r3-excerpt-1.4.json", false),
		)

		DescribeTable("refuses an HTML profile id outside ingestion's rule, withholding the cause, and stores nothing",
			func(fixture, badID string) {
				seed()
				cats, ctrls, conts := snapshot()
				data, _ := readIngestionFixture(fixture)
				quoted, err := json.Marshal(badID)
				Expect(err).NotTo(HaveOccurred())
				data = bytes.Replace(data, []byte(`"id": "eur-lex-oj"`), append([]byte(`"id": `), quoted...), 1)

				_, err = parseWith(newSvc(defaultStructuring()), data, "")

				// ParseCatalog withholds the cause (CWE-209) and logs it; the
				// adapter spec asserts the actionable message itself.
				Expect(connect.CodeOf(err)).To(Equal(connect.CodeInternal))
				Expect(err).To(MatchError(ContainSubstring("the server logged the cause")))
				Expect(err).NotTo(MatchError(ContainSubstring("html_profile")))
				Expect(err.Error()).NotTo(ContainSubstring(badID))
				expectUnchanged(cats, ctrls, conts)
			},
			Entry("an uppercase id at schema 1.4", guessed1_4, "EUR-LEX"),
			Entry("an id with a space at schema 1.4", guessed1_4, "eur lex"),
			Entry("an id with a control character at schema 1.4", guessed1_4, "eur\u0007lex"),
			Entry("a leading hyphen at schema 1.4", guessed1_4, "-eur-lex"),
			Entry("129 bytes at schema 1.4", guessed1_4, strings.Repeat("a", 129)),
			Entry("an uppercase id below schema 1.4", "eurlex-nis2-32022L2555.json", "EUR-LEX"),
			Entry("129 bytes below schema 1.4", "eurlex-nis2-32022L2555.json", strings.Repeat("a", 129)),
		)

		It("accepts an HTML profile id of exactly 128 bytes", func() {
			data, _ := readIngestionFixture(guessed1_4)
			id := strings.Repeat("a", 128)
			data = bytes.Replace(data, []byte(`"id": "eur-lex-oj"`), []byte(`"id": "`+id+`"`), 1)

			resp, err := parseWith(newSvc(defaultStructuring()), data, "")

			Expect(err).NotTo(HaveOccurred())
			Expect(resp.IngestionHtmlProfile).To(Equal(id))
		})

		It("leaves the HTML profile empty for OSCAL", func() {
			data, err := os.ReadFile(filepath.Join("..", "..", "pkg", "oscal", "testdata", "minimal_catalog.json"))
			Expect(err).NotTo(HaveOccurred())

			resp, err := parseWith(newSvc(defaultStructuring()), data, "")

			Expect(err).NotTo(HaveOccurred())
			Expect(resp.IngestionHtmlProfile).To(BeEmpty())
			Expect(resp.IngestionHtmlProfileSha256).To(BeEmpty())
		})

		It("leaves the method empty for OSCAL", func() {
			data, err := os.ReadFile(filepath.Join("..", "..", "pkg", "oscal", "testdata", "minimal_catalog.json"))
			Expect(err).NotTo(HaveOccurred())

			resp, err := parseWith(newSvc(defaultStructuring()), data, "")

			Expect(err).NotTo(HaveOccurred())
			Expect(resp.IngestionStructureMethod).To(BeEmpty())
		})
	})

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

	It("treats ingestion-shaped JSON as any other unsupported document when no structurer is configured, and stores nothing", func() {
		seed()
		cats, ctrls, conts := snapshot()
		data, _ := readIngestionFixture("nist-sp800-171r3-excerpt.json")

		_, err := parse(newService(), data, crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED)

		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		Expect(err).To(MatchError(connect.NewError(connect.CodeInvalidArgument,
			catalog.UnsupportedFormatError(crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED, data)).Error()))
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
