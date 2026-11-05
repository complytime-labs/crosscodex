package oscal_test

import (
	"context"
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/oscal"
)

var _ = Describe("Withdrawn controls", func() {
	parseControl := func(controlJSON string) *oscal.ControlItem {
		doc := fmt.Sprintf(`{"catalog": {"uuid": "00000000-0000-0000-0000-000000000000",
			"metadata": {"title": "T", "version": "1", "oscal-version": "1.1.3", "last-modified": "2026-06-11T00:00:00Z"},
			"controls": [%s]}}`, controlJSON)
		items, err := oscal.NewParser("").Parse(context.Background(), strings.NewReader(doc))
		Expect(err).NotTo(HaveOccurred())
		return findItem(items, "x-1")
	}

	// Values are the shapes seen in NIST SP 800-53 r4 ("Withdrawn", "#id" hrefs),
	// SP 800-171 r3 ("withdrawn", underscore rels, bare hrefs) and r5 (moved-to).
	DescribeTable("preserves the status and its targets",
		func(controlJSON string, wantProps map[string]string) {
			item := parseControl(controlJSON)
			Expect(item).NotTo(BeNil())
			Expect(item.Props).To(Equal(wantProps))
		},
		Entry("r4 capitalised status with two incorporated-into links",
			`{"id": "x-1", "title": "T", "props": [{"name": "status", "value": "Withdrawn"}],
			  "links": [{"href": "#ac-6", "rel": "incorporated-into"}, {"href": "#sc-28", "rel": "incorporated-into"}]}`,
			map[string]string{"status": "withdrawn", "incorporated-into": "ac-6,sc-28"}),
		Entry("171r3 lowercase status with underscore rel and bare href",
			`{"id": "x-1", "title": "T", "props": [{"name": "status", "value": "withdrawn"}],
			  "links": [{"href": "03.01.12", "rel": "incorporated_into"}]}`,
			map[string]string{"status": "withdrawn", "incorporated-into": "03.01.12"}),
		Entry("moved-to link",
			`{"id": "x-1", "title": "T", "props": [{"name": "status", "value": "withdrawn"}],
			  "links": [{"href": "#ac-2", "rel": "moved-to"}]}`,
			map[string]string{"status": "withdrawn", "moved-to": "ac-2"}),
		Entry("withdrawn without links",
			`{"id": "x-1", "title": "T", "props": [{"name": "status", "value": "withdrawn"}]}`,
			map[string]string{"status": "withdrawn"}),
	)

	DescribeTable("leaves other controls unmarked",
		func(controlJSON string) {
			item := parseControl(controlJSON)
			Expect(item).NotTo(BeNil())
			Expect(item.Props).NotTo(HaveKey("status"))
			Expect(item.Props).NotTo(HaveKey("incorporated-into"))
		},
		Entry("no props",
			`{"id": "x-1", "title": "T", "parts": [{"id": "x-1_smt", "name": "statement", "prose": "Do it."}]}`),
		Entry("a different status value",
			`{"id": "x-1", "title": "T", "props": [{"name": "status", "value": "draft"}]}`),
		Entry("incorporated-into link on a live control",
			`{"id": "x-1", "title": "T", "links": [{"href": "#ac-6", "rel": "incorporated-into"}]}`),
	)
})
