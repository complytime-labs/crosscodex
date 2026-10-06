package graphdb_test

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

var _ = Describe("Property Specifications", Ordered, func() {
	// Any int64 of Unix nanoseconds is a year in 1677..2262, so every draw
	// formats as a four-digit year and TimeLayout stays fixed-width.
	drawTime := func(t *rapid.T, label string) time.Time {
		return time.Unix(0, rapid.Int64().Draw(t, label)).UTC()
	}

	Context("FormatTime — ordering", func() {
		It("orders formatted strings exactly as the times they encode", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				a := drawTime(t, "a")
				b := drawTime(t, "b")
				fa, fb := graphdb.FormatTime(a), graphdb.FormatTime(b)
				Expect(fa < fb).To(Equal(a.Before(b)), "a=%s b=%s", fa, fb)
				Expect(fa == fb).To(Equal(a.Equal(b)), "a=%s b=%s", fa, fb)
			})
		})
	})

	Context("FormatTime — roundtrip", func() {
		It("parses back to the same instant", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				a := drawTime(t, "a")
				parsed, err := time.Parse(time.RFC3339Nano, graphdb.FormatTime(a))
				Expect(err).NotTo(HaveOccurred())
				Expect(parsed.Equal(a)).To(BeTrue())
			})
		})
	})

	Context("CheckIdentifier — acceptance", func() {
		// isIdentifier restates the rule rune by rune so the property does
		// not share an implementation with the code under test.
		isIdentifier := func(s string) bool {
			if s == "" {
				return false
			}
			for i, r := range s {
				switch {
				case r == '_', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
				case r >= '0' && r <= '9' && i > 0:
				default:
					return false
				}
			}
			return true
		}

		It("accepts a string exactly when it is an ASCII identifier", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				s := rapid.String().Draw(t, "s")
				err := graphdb.CheckIdentifier("label", s)
				if isIdentifier(s) {
					Expect(err).NotTo(HaveOccurred())
				} else {
					Expect(err).To(MatchError(graphdb.ErrInvalidCypher))
				}
			})
		})

		It("accepts every generated identifier", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				s := rapid.StringMatching(`[A-Za-z_][A-Za-z0-9_]{0,40}`).Draw(t, "s")
				Expect(graphdb.CheckIdentifier("label", s)).To(Succeed())
			})
		})
	})

	Context("DerivedID — injectivity", func() {
		// The alphabet is mostly separator and escape characters so that
		// underscore-joining without escaping would collide often.
		part := rapid.StringMatching(`[_%25Fa]{0,6}`)

		It("gives distinct IDs to distinct non-empty part lists", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				a := rapid.SliceOfN(part, 1, 4).Draw(t, "a")
				b := rapid.SliceOfN(part, 1, 4).Draw(t, "b")
				if slices.Equal(a, b) {
					Expect(graphdb.DerivedID(a...)).To(Equal(graphdb.DerivedID(b...)))
					return
				}
				Expect(graphdb.DerivedID(a...)).NotTo(Equal(graphdb.DerivedID(b...)), "a=%q b=%q", a, b)
			})
		})
	})

	Context("CheckTenant — acceptance", func() {
		It("accepts a tenant exactly when tenant.ValidateTenantID does, and every accepted graph name fits 63 bytes", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				id := rapid.OneOf(
					rapid.String(),
					rapid.StringMatching(`[a-z][a-z0-9-]{0,60}[a-z0-9]`),
				).Draw(t, "id")
				err := graphdb.CheckTenant(id)
				if tenant.ValidateTenantID(id) != nil {
					Expect(err).To(MatchError(graphdb.ErrTenantRequired))
					return
				}
				Expect(err).NotTo(HaveOccurred())
				Expect(len("crosscodex_"+id)).To(BeNumerically("<=", 63), "id=%q", id)
			})
		})
	})

	Context("CheckNodeProperties / CheckEdgeProperties — reserved keys", func() {
		key := rapid.StringMatching(`[a-z_]{1,20}`)

		DescribeTable("accepts identifier keys exactly when none is reserved",
			func(check func(map[string]any) error, reserved []string) {
				rapid.Check(GinkgoT(), func(t *rapid.T) {
					keys := rapid.SliceOfN(rapid.OneOf(key, rapid.SampledFrom(reserved)), 0, 5).Draw(t, "keys")
					props := make(map[string]any, len(keys))
					hasReserved := false
					for _, k := range keys {
						props[k] = "v"
						hasReserved = hasReserved || slices.Contains(reserved, k)
					}
					err := check(props)
					if hasReserved {
						Expect(err).To(MatchError(graphdb.ErrInvalidCypher), "props=%v", props)
						Expect(err.Error()).To(ContainSubstring("is reserved"))
					} else {
						Expect(err).NotTo(HaveOccurred(), "props=%v", props)
					}
				})
			},
			Entry("node", graphdb.CheckNodeProperties, slices.Sorted(maps.Keys(graphdb.ReservedNodeKeys()))),
			Entry("edge", graphdb.CheckEdgeProperties, slices.Sorted(maps.Keys(graphdb.ReservedEdgeKeys()))),
		)
	})

	Context("CheckNode — first defect", func() {
		It("accepts a node exactly when it has no defect, and otherwise reports the first one in check order", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				id := rapid.SampledFrom([]string{"n1", ""}).Draw(t, "id")
				label := rapid.SampledFrom([]string{"Control", "", "a b"}).Draw(t, "label")
				key := rapid.SampledFrom([]string{"title", "a b", "valid_to"}).Draw(t, "key")
				validFrom := rapid.SampledFrom([]time.Time{time.Unix(0, 1), {}}).Draw(t, "validFrom")

				err := graphdb.CheckNode(graphdb.Node{ID: id, Label: label, Properties: map[string]any{key: "v"}, ValidFrom: validFrom})

				var want string
				invalidCypher := false
				switch {
				case id == "":
					want = "id is required"
				case label == "":
					want = "label is required"
				case label == "a b":
					want, invalidCypher = `label "a b" must be an identifier`, true
				case key == "a b":
					want, invalidCypher = `property key "a b"`, true
				case key == "valid_to":
					want, invalidCypher = `property key "valid_to" is reserved`, true
				case validFrom.IsZero():
					want = "valid_from is required"
				}
				if want == "" {
					Expect(err).NotTo(HaveOccurred())
					return
				}
				Expect(err).To(MatchError(ContainSubstring(want)))
				Expect(errors.Is(err, graphdb.ErrInvalidCypher)).To(Equal(invalidCypher))
			})
		})
	})

	Context("CheckEdge — first defect", func() {
		It("accepts an edge exactly when it has no defect, and otherwise reports the first one in check order", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				label := rapid.SampledFrom([]string{"MAPS", "", "a b"}).Draw(t, "label")
				key := rapid.SampledFrom([]string{"kind", "a b", "confidence"}).Draw(t, "key")
				src := rapid.SampledFrom([]string{"a", ""}).Draw(t, "src")
				tgt := rapid.SampledFrom([]string{"b", ""}).Draw(t, "tgt")
				validFrom := rapid.SampledFrom([]time.Time{time.Unix(0, 1), {}}).Draw(t, "validFrom")

				err := graphdb.CheckEdge(src, tgt, graphdb.Edge{Label: label, Properties: map[string]any{key: "v"}, ValidFrom: validFrom})

				var want string
				invalidCypher := false
				switch {
				case label == "":
					want = "label is required"
				case label == "a b":
					want, invalidCypher = `label "a b" must be an identifier`, true
				case key == "a b":
					want, invalidCypher = `property key "a b"`, true
				case key == "confidence":
					want, invalidCypher = `property key "confidence" is reserved`, true
				case src == "" || tgt == "":
					want = "source and target are required"
				case validFrom.IsZero():
					want = "valid_from is required"
				}
				if want == "" {
					Expect(err).NotTo(HaveOccurred())
					return
				}
				Expect(err).To(MatchError(ContainSubstring(want)))
				Expect(errors.Is(err, graphdb.ErrInvalidCypher)).To(Equal(invalidCypher))
			})
		})
	})

	Context("CheckSupersedeRequest — first defect", func() {
		It("accepts a request exactly when it has no defect, and otherwise reports the first one in check order", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				nodeID := rapid.SampledFrom([]string{"", "a"}).Draw(t, "nodeID")
				edgeID := rapid.SampledFrom([]string{"", "e1"}).Draw(t, "edgeID")
				at := rapid.SampledFrom([]time.Time{time.Unix(0, 1), {}}).Draw(t, "supersededAt")

				err := graphdb.CheckSupersedeRequest(graphdb.SupersedeRequest{NodeID: nodeID, EdgeID: edgeID, SupersededAt: at})

				var want string
				switch {
				case nodeID == "" && edgeID == "":
					want = "node_id or edge_id is required"
				case nodeID != "" && edgeID != "":
					want = "set node_id or edge_id, not both"
				case at.IsZero():
					want = "superseded_at is required"
				}
				if want == "" {
					Expect(err).NotTo(HaveOccurred())
					return
				}
				Expect(err).To(MatchError(want))
			})
		})
	})

	Context("CheckRequiresEdge — first defect", func() {
		It("accepts an edge exactly when it has no defect, and otherwise reports the first one in check order", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				src := rapid.SampledFrom([]string{"", "ac-2"}).Draw(t, "src")
				tgt := rapid.SampledFrom([]string{"", "ac-1"}).Draw(t, "tgt")
				at := rapid.SampledFrom([]time.Time{time.Unix(0, 1), {}}).Draw(t, "analyzedAt")

				err := graphdb.CheckRequiresEdge(graphdb.RequiresEdge{SourceID: src, TargetID: tgt, AnalyzedAt: at})

				var want string
				switch {
				case src == "":
					want = "source_id is required"
				case tgt == "":
					want = "target_id is required"
				case at.IsZero():
					want = "analyzed_at is required"
				}
				if want == "" {
					Expect(err).NotTo(HaveOccurred())
					return
				}
				Expect(err).To(MatchError(want))
			})
		})
	})

	Context("BulkEdgeError — message and unwrapping", func() {
		It("formats any index and message as the drivers always have, and unwraps to the cause", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				index := rapid.IntRange(0, 1_000_000).Draw(t, "index")
				inner := errors.New(rapid.String().Draw(t, "msg"))
				be := &graphdb.BulkEdgeError{Index: index, Err: inner}
				Expect(be.Error()).To(Equal(fmt.Sprintf("bulk create edges [%d]: %s", index, inner.Error())))
				Expect(errors.Is(be, inner)).To(BeTrue())
				Expect(errors.Unwrap(be)).To(BeIdenticalTo(inner))
			})
		})
	})
})
