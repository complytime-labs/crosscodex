package agedriver_test

import (
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/graphdb/agedriver"
)

var _ = Describe("Property Specifications", func() {

	Context("escapeCypher — injection prevention", func() {
		It("never produces output with unescaped single quotes", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				input := rapid.String().Draw(t, "input")
				result := agedriver.EscapeCypher(input)

				// Walk the result: every single quote must be preceded by a backslash.
				for i := 0; i < len(result); i++ {
					if result[i] == '\'' {
						Expect(i).To(BeNumerically(">", 0),
							"single quote at position 0 is unescaped")
						Expect(result[i-1]).To(Equal(byte('\\')),
							"single quote at position %d is not preceded by backslash in %q", i, result)
					}
				}
			})
		})
	})

	Context("nodeToAGProperties — format compliance", func() {
		It("always produces output enclosed in curly braces", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				node := graphdb.Node{
					ID:    rapid.StringMatching(`[a-zA-Z0-9_-]+`).Draw(t, "id"),
					Label: rapid.StringMatching(`[A-Z][a-zA-Z]*`).Draw(t, "label"),
					ValidFrom: time.Date(
						rapid.IntRange(2000, 2030).Draw(t, "year"),
						time.Month(rapid.IntRange(1, 12).Draw(t, "month")),
						rapid.IntRange(1, 28).Draw(t, "day"),
						0, 0, 0, 0, time.UTC,
					),
					Properties: drawStringMap(t),
				}

				result := agedriver.NodeToAGProperties(node)
				Expect(result).To(HavePrefix("{"), "output must start with {")
				Expect(result).To(HaveSuffix("}"), "output must end with }")
				if node.ID != "" {
					Expect(result).To(ContainSubstring(node.ID))
				}
				if !node.ValidFrom.IsZero() {
					Expect(result).To(ContainSubstring(node.ValidFrom.Format(time.RFC3339Nano)))
				}
			})
		})
	})

	Context("graphName — format compliance", func() {
		It("always produces crosscodex_ prefix followed by the tenant", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				tenant := rapid.StringMatching(`[a-z0-9-]{3,64}`).Draw(t, "tenant")
				result := agedriver.GraphName(tenant)

				Expect(result).To(Equal("crosscodex_" + tenant))
				Expect(strings.HasPrefix(result, "crosscodex_")).To(BeTrue(),
					"result %q must have crosscodex_ prefix", result)
			})
		})
	})

	Context("edgeToAGProperties — format compliance", func() {
		It("always produces output enclosed in curly braces", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				edge := graphdb.Edge{
					ID:    rapid.StringMatching(`[a-zA-Z0-9_-]+`).Draw(t, "id"),
					Label: rapid.StringMatching(`[A-Z][a-zA-Z_]*`).Draw(t, "label"),
					ValidFrom: time.Date(
						rapid.IntRange(2000, 2030).Draw(t, "year"),
						time.Month(rapid.IntRange(1, 12).Draw(t, "month")),
						rapid.IntRange(1, 28).Draw(t, "day"),
						0, 0, 0, 0, time.UTC,
					),
					Properties: drawStringMap(t),
				}

				result := agedriver.EdgeToAGProperties(edge)
				Expect(result).To(HavePrefix("{"), "output must start with {")
				Expect(result).To(HaveSuffix("}"), "output must end with }")
				if edge.ID != "" {
					Expect(result).To(ContainSubstring(edge.ID))
				}
				if !edge.ValidFrom.IsZero() {
					Expect(result).To(ContainSubstring(edge.ValidFrom.Format(time.RFC3339Nano)))
				}
			})
		})
	})
})

// drawStringMap generates a small map[string]any with string values for property tests.
func drawStringMap(t *rapid.T) map[string]any {
	n := rapid.IntRange(0, 5).Draw(t, "prop_count")
	if n == 0 {
		return nil
	}
	m := make(map[string]any, n)
	for i := 0; i < n; i++ {
		key := rapid.StringMatching(`[a-z][a-z0-9_]{0,15}`).Draw(t, "prop_key")
		val := rapid.String().Draw(t, "prop_val")
		m[key] = val
	}
	return m
}
