package agedriver_test

import (
	"regexp"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/graphdb/agedriver"
)

var _ = Describe("Property Specifications", Ordered, func() {

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
					Expect(result).To(ContainSubstring(graphdb.FormatTime(node.ValidFrom)))
				}
			})
		})
	})

	Context("nodeUpsertProperties — replacement map", func() {
		It("is the CreateNode map with stored-value references only for valid_from and, without ValidTo, the supersede keys; every caller value is quoted", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				props := drawStringMap(t)
				for k := range graphdb.ReservedNodeKeys() {
					delete(props, k) // CheckNode rejects these before serialization
				}
				node := graphdb.Node{
					ID:             rapid.String().Draw(t, "id"),
					Label:          "Control",
					ValidFrom:      time.Unix(rapid.Int64Range(0, 4e9).Draw(t, "valid_from"), 0).UTC(),
					CreatedBy:      rapid.String().Draw(t, "created_by"),
					CreationMethod: rapid.String().Draw(t, "creation_method"),
					Properties:     props,
				}
				wantRefs := []string{"n.valid_from"}
				suffix := ", valid_to: n.valid_to, superseded_by: n.superseded_by}"
				if rapid.Bool().Draw(t, "has_valid_to") {
					validTo := node.ValidFrom.Add(time.Hour)
					node.ValidTo = &validTo
					suffix = "}"
				} else {
					wantRefs = append(wantRefs, "n.valid_to", "n.superseded_by")
				}

				got := agedriver.NodeUpsertProperties(node)
				created := agedriver.NodeToAGProperties(node)
				literal := "valid_from: '" + graphdb.FormatTime(node.ValidFrom) + "'"
				Expect(strings.Count(created, literal)).To(BeNumerically(">=", 1))
				want := strings.Replace(created, literal, "valid_from: n.valid_from", 1)
				Expect(got).To(Equal(strings.TrimSuffix(want, "}") + suffix))

				outside := unquotedText(got)
				Expect(cypherRef.FindAllString(outside, -1)).To(Equal(wantRefs), "n.<key> references outside string literals in %q", got)
				Expect(outside).To(MatchRegexp(`^[{}A-Za-z0-9_:,. ]*$`), "only keys, references and punctuation may appear unquoted in %q", got)
				for k, v := range props {
					Expect(got).To(ContainSubstring(k + ": '" + agedriver.EscapeCypher(v.(string)) + "'"))
				}
			})
		})
	})

	Context("graphName — format compliance", func() {
		It("always produces crosscodex_ prefix followed by the tenant, within PostgreSQL's 63-byte identifier limit", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				tenant := rapid.StringMatching(`[a-z][a-z0-9-]{1,50}[a-z0-9]`).Draw(t, "tenant")
				result := agedriver.GraphName(tenant)

				Expect(result).To(Equal("crosscodex_" + tenant))
				Expect(strings.HasPrefix(result, "crosscodex_")).To(BeTrue(),
					"result %q must have crosscodex_ prefix", result)
				Expect(len(result)).To(BeNumerically("<=", 63),
					"graph name %q exceeds 63 bytes and would be truncated", result)
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
					Expect(result).To(ContainSubstring(graphdb.FormatTime(edge.ValidFrom)))
				}
			})
		})
	})
})

// cypherRef matches a reference to a stored node property.
var cypherRef = regexp.MustCompile(`\bn\.[A-Za-z_][A-Za-z0-9_]*`)

// unquotedText returns s with every single-quoted Cypher string literal
// removed, honoring backslash escapes inside literals.
func unquotedText(s string) string {
	var b strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case inQuote && c == '\\':
			i++
		case c == '\'':
			inQuote = !inQuote
		case !inQuote:
			b.WriteByte(c)
		}
	}
	return b.String()
}

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
