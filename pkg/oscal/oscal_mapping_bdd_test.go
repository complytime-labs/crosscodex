package oscal_test

import (
	"encoding/json"
	"math"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/oscal"
)

// Fixture catalogs use two real control-ID encodings: "ac-2" (SP 800-53) and
// "03.01.01" (SP 800-171 r3).
func exportFixture() oscal.MappingExport {
	return oscal.MappingExport{
		JobID:       "job-1",
		Stage:       "synthesized",
		CompletedAt: time.Date(2026, 11, 22, 10, 30, 0, 0, time.FixedZone("x", 3600)),
		Source:      oscal.ExportCatalog{ID: "cat-src", Name: "SP 800-53", Version: "r5", VersionDisplay: "Rev 5"},
		Target:      oscal.ExportCatalog{ID: "cat-tgt", Name: "SP 800-171", Version: "r3"},
		Mappings: []oscal.ExportMapping{
			{MappingID: "m-2", SourceControlID: "cat-src/ac-2", TargetControlID: "cat-tgt/03.01.01", Relationship: "EQUIVALENT", Confidence: 0.9},
			{MappingID: "m-1", SourceControlID: "cat-src/ac-2", TargetControlID: "cat-tgt/03.01.02", Relationship: "SUBSET_OF", Confidence: 0.5},
			{MappingID: "m-3", SourceControlID: "cat-src/ac-1", TargetControlID: "cat-tgt/03.01.01", Relationship: "PARTIAL", Confidence: 0.25},
		},
	}
}

func buildDoc(in oscal.MappingExport) (map[string]any, []byte) {
	out, err := oscal.BuildMappingCollection(in)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	var top map[string]any
	ExpectWithOffset(1, json.Unmarshal(out, &top)).To(Succeed())
	return top["mapping-collection"].(map[string]any), out
}

func asMaps(v any) []map[string]any {
	var res []map[string]any
	for _, e := range v.([]any) {
		res = append(res, e.(map[string]any))
	}
	return res
}

// propValues returns the "value" of each prop with the given name, in order.
func propValues(v any, name string) []string {
	var res []string
	if v == nil {
		return res
	}
	for _, p := range asMaps(v) {
		if p["name"] == name {
			res = append(res, p["value"].(string))
		}
	}
	return res
}

var _ = Describe("BuildMappingCollection", func() {
	DescribeTable("relationship mapping",
		func(crosscodex, want string, oscalNS bool) {
			in := exportFixture()
			in.Mappings = in.Mappings[:1]
			in.Mappings[0].Relationship = crosscodex
			doc, _ := buildDoc(in)
			m := asMaps(asMaps(doc["mappings"])[0]["maps"])[0]
			Expect(m["relationship"]).To(Equal(want))
			if oscalNS {
				Expect(m).NotTo(HaveKey("ns"))
			} else {
				Expect(m["ns"]).To(Equal(oscal.ExportComplianceMapperNS))
			}
		},
		Entry("EQUIVALENT", "EQUIVALENT", "equivalent-to", true),
		Entry("SUPERSET_OF", "SUPERSET_OF", "superset-of", true),
		Entry("SUBSET_OF", "SUBSET_OF", "subset-of", true),
		Entry("NO_RELATIONSHIP", "NO_RELATIONSHIP", "no-relationship", true),
		Entry("CONTRIBUTES_TO", "CONTRIBUTES_TO", "contributes-to", false),
		Entry("COMPLEMENTS", "COMPLEMENTS", "complements", false),
		Entry("PARTIAL", "PARTIAL", "partial", false),
		Entry("CONFLICTS_WITH", "CONFLICTS_WITH", "conflicts-with", false),
	)

	Describe("directions", func() {
		reverseRow := oscal.ExportMapping{MappingID: "m-r", SourceControlID: "cat-tgt/03.01.01", TargetControlID: "cat-src/ac-2", Relationship: "SUPERSET_OF", Confidence: 0.7}

		It("emits one mapping for forward-only input", func() {
			doc, _ := buildDoc(exportFixture())
			Expect(doc["mappings"]).To(HaveLen(1))
		})

		It("emits a second mapping with swapped resources and unchanged relationships for reverse rows", func() {
			in := exportFixture()
			in.Mappings = append(in.Mappings, reverseRow)
			doc, _ := buildDoc(in)
			ms := asMaps(doc["mappings"])
			Expect(ms).To(HaveLen(2))
			fwd, rev := ms[0], ms[1]
			Expect(rev["source-resource"]).To(Equal(fwd["target-resource"]))
			Expect(rev["target-resource"]).To(Equal(fwd["source-resource"]))
			Expect(rev["uuid"]).NotTo(Equal(fwd["uuid"]))
			m := asMaps(rev["maps"])[0]
			Expect(m["relationship"]).To(Equal("superset-of"))
			Expect(asMaps(m["sources"])[0]["id-ref"]).To(Equal("03.01.01"))
			Expect(asMaps(m["targets"])[0]["id-ref"]).To(Equal("ac-2"))
		})

		It("emits only the reverse mapping for reverse-only input", func() {
			in := exportFixture()
			in.Mappings = []oscal.ExportMapping{reverseRow}
			doc, _ := buildDoc(in)
			ms := asMaps(doc["mappings"])
			Expect(ms).To(HaveLen(1))
			resources := asMaps(doc["back-matter"].(map[string]any)["resources"])
			Expect(ms[0]["source-resource"].(map[string]any)["href"]).To(Equal("#" + resources[1]["uuid"].(string)))
		})
	})

	It("strips catalog prefixes and sorts maps by source, target, mapping id", func() {
		in := exportFixture()
		in.Mappings = append(in.Mappings, oscal.ExportMapping{MappingID: "m-0", SourceControlID: "cat-src/ac-2", TargetControlID: "cat-tgt/03.01.02", Relationship: "EQUIVALENT", Confidence: 1})
		doc, _ := buildDoc(in)
		var got []string
		for _, m := range asMaps(asMaps(doc["mappings"])[0]["maps"]) {
			got = append(got, asMaps(m["sources"])[0]["id-ref"].(string)+">"+asMaps(m["targets"])[0]["id-ref"].(string)+"#"+propValues(m["props"], "mapping-id")[0])
		}
		Expect(got).To(Equal([]string{"ac-1>03.01.01#m-3", "ac-2>03.01.01#m-2", "ac-2>03.01.02#m-0", "ac-2>03.01.02#m-1"}))
	})

	Describe("confidence", func() {
		It("renders confidence 0 explicitly rather than dropping it", func() {
			in := exportFixture()
			in.Mappings[0].Confidence = 0
			_, raw := buildDoc(in)
			Expect(string(raw)).To(ContainSubstring(`"percentage": 0`))
		})

		It("rounds to four decimal places", func() {
			in := exportFixture()
			in.Mappings = in.Mappings[:1]
			in.Mappings[0].Confidence = 0.123456
			doc, _ := buildDoc(in)
			m := asMaps(asMaps(doc["mappings"])[0]["maps"])[0]
			Expect(m["confidence-score"]).To(Equal(map[string]any{"percentage": 0.1235}))
		})
	})

	It("keeps quotes and markup in a hostile catalog name, flattens its line break, and still validates", func() {
		in := exportFixture()
		in.Source.Name = "Q\"<script>\n"
		doc, _ := buildDoc(in)
		resources := asMaps(doc["back-matter"].(map[string]any)["resources"])
		Expect(resources[0]["title"]).To(Equal("Q\"<script> r5"))
		Expect(doc["metadata"].(map[string]any)["title"]).To(Equal("CrossCodex mapping: Q\"<script> to SP 800-171"))
	})

	It("records props, provenance and metadata per the spec", func() {
		in := exportFixture()
		in.Filters = []oscal.Prop{{Name: "min-confidence", Value: "0.5"}, {Name: "relationship", Value: "EQUIVALENT"}}
		in.Models = []oscal.ExportModel{{Alias: "judge", Observed: "m-a", Revision: "7"}, {Alias: "panel", Observed: "m-b"}}
		in.Mappings[0].CleaningState = "restored"
		in.Mappings[0].CritiqueOutcome = "upheld"
		doc, _ := buildDoc(in)

		prov := doc["provenance"].(map[string]any)
		Expect(prov["method"]).To(Equal("automation"))
		Expect(prov["matching-rationale"]).To(Equal("semantic"))
		Expect(prov["status"]).To(Equal("draft"))
		Expect(prov["mapping-description"]).To(Equal("CrossCodex job job-1: SP 800-53 r5 to SP 800-171 r3, synthesized stage. Unreviewed machine output."))
		var names []string
		for _, p := range asMaps(prov["props"]) {
			names = append(names, p["name"].(string))
			Expect(p["ns"]).To(Equal(oscal.ExportComplianceMapperNS))
		}
		Expect(names).To(Equal([]string{"job-id", "stage", "filter-min-confidence", "filter-relationship", "model", "model"}))
		Expect(propValues(prov["props"], "job-id")).To(Equal([]string{"job-1"}))
		Expect(propValues(prov["props"], "filter-min-confidence")).To(Equal([]string{"0.5"}))
		Expect(propValues(prov["props"], "model")).To(Equal([]string{"judge=m-a@7", "panel=m-b"}))

		meta := doc["metadata"].(map[string]any)
		Expect(meta["title"]).To(Equal("CrossCodex mapping: SP 800-53 to SP 800-171"))
		Expect(meta["last-modified"]).To(Equal("2026-11-22T09:30:00Z"))
		Expect(meta["version"]).To(Equal("job-1"))
		Expect(meta["oscal-version"]).To(Equal("1.2.3"))

		maps := asMaps(asMaps(doc["mappings"])[0]["maps"])
		var m2, m3 map[string]any
		for _, m := range maps {
			switch propValues(m["props"], "mapping-id")[0] {
			case "m-2":
				m2 = m
			case "m-3":
				m3 = m
			}
		}
		var m2Names []string
		for _, p := range asMaps(m2["props"]) {
			m2Names = append(m2Names, p["name"].(string))
		}
		Expect(m2Names).To(Equal([]string{"mapping-id", "cleaning-state", "critique-outcome"}))
		Expect(asMaps(m3["props"])).To(HaveLen(1))

		resources := asMaps(doc["back-matter"].(map[string]any)["resources"])
		Expect(resources).To(HaveLen(2))
		Expect(propValues(resources[0]["props"], "catalog-id")).To(Equal([]string{"cat-src"}))
		Expect(propValues(resources[0]["props"], "version-display")).To(Equal([]string{"Rev 5"}))
		Expect(propValues(resources[1]["props"], "version-display")).To(BeEmpty())
		Expect(resources[1]["title"]).To(Equal("SP 800-171 r3"))
		Expect(resources[0]["title"]).To(Equal("SP 800-53 r5"))
	})

	It("supports a single-catalog job with one mapping and one shared resource", func() {
		in := exportFixture()
		in.Target = in.Source
		in.Mappings = []oscal.ExportMapping{
			{MappingID: "m-1", SourceControlID: "cat-src/ac-1", TargetControlID: "cat-src/ac-2", Relationship: "SUBSET_OF", Confidence: 0.5},
			{MappingID: "m-2", SourceControlID: "cat-src/ac-2", TargetControlID: "cat-src/ac-1", Relationship: "SUPERSET_OF", Confidence: 0.5},
		}
		doc, _ := buildDoc(in)
		ms := asMaps(doc["mappings"])
		Expect(ms).To(HaveLen(1))
		Expect(asMaps(ms[0]["maps"])).To(HaveLen(2))
		Expect(asMaps(doc["back-matter"].(map[string]any)["resources"])).To(HaveLen(1))
		Expect(ms[0]["source-resource"]).To(Equal(ms[0]["target-resource"]))
	})

	It("is deterministic and changes the document uuid when a row changes", func() {
		first, rawFirst := buildDoc(exportFixture())
		again, rawAgain := buildDoc(exportFixture())
		Expect(rawAgain).To(Equal(rawFirst))
		Expect(again["uuid"]).To(Equal(first["uuid"]))

		in := exportFixture()
		in.Mappings[0].Confidence = 0.8
		changed, _ := buildDoc(in)
		Expect(changed["uuid"]).NotTo(Equal(first["uuid"]))
	})

	DescribeTable("refuses",
		func(mutate func(*oscal.MappingExport), substrings ...string) {
			in := exportFixture()
			mutate(&in)
			out, err := oscal.BuildMappingCollection(in)
			Expect(err).To(MatchError(oscal.ErrMappingExport))
			for _, s := range substrings {
				Expect(err.Error()).To(ContainSubstring(s))
			}
			Expect(out).To(BeNil())
		},
		Entry("an unknown relationship, naming the mapping",
			func(in *oscal.MappingExport) { in.Mappings[1].Relationship = "FOO" }, "m-1", "FOO"),
		Entry("a control ID with neither catalog prefix, naming the mapping and both prefixes",
			func(in *oscal.MappingExport) { in.Mappings[2].SourceControlID = "other/ac-1" }, "m-3", "cat-src/", "cat-tgt/"),
		Entry("a row whose both IDs belong to the same catalog",
			func(in *oscal.MappingExport) { in.Mappings[0].TargetControlID = "cat-src/ac-3" }, "m-2"),
		Entry("confidence above 1, naming the mapping",
			func(in *oscal.MappingExport) { in.Mappings[0].Confidence = 1.5 }, "m-2"),
		Entry("negative confidence, naming the mapping",
			func(in *oscal.MappingExport) { in.Mappings[0].Confidence = -0.1 }, "m-2"),
		Entry("no mappings, with the actionable message",
			func(in *oscal.MappingExport) { in.Mappings = nil },
			"job job-1 has no mappings to export with these filters (retired rows are excluded from OSCAL exports); widen the filters or export --format json to see every row"),
		Entry("a filter value the schema rejects, with the remedy",
			func(in *oscal.MappingExport) { in.Filters = []oscal.Prop{{Name: "x", Value: " x"}} },
			"schema validation", "a recorded value (a catalog version, model name or filter)", "Export --format json instead"),
		Entry("a zero completion time, which has no valid last-modified",
			func(in *oscal.MappingExport) { in.CompletedAt = time.Time{} }, "completion"),
	)

	It("refuses NaN confidence", func() {
		in := exportFixture()
		in.Mappings[0].Confidence = math.NaN()
		out, err := oscal.BuildMappingCollection(in)
		Expect(err).To(MatchError(oscal.ErrMappingExport))
		Expect(err.Error()).To(ContainSubstring("m-2"))
		Expect(out).To(BeNil())
	})
})
