package analyzer_test

import (
	"errors"
	"sort"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/analyzer"
)

// Suite bootstrap lives in analyzer_bdd_test.go (TestAnalyzerBDD).
// This file only registers Describe nodes; Ginkgo collects them automatically.

var _ = Describe("Property Specifications", func() {

	Describe("ValidateName properties", func() {

		Context("when given an empty string", func() {
			It("always returns ErrInvalidName", func() {
				rapid.Check(GinkgoT(), func(t *rapid.T) {
					err := analyzer.ValidateName("")
					if err == nil || !errors.Is(err, analyzer.ErrInvalidName) {
						t.Fatalf("expected ErrInvalidName for empty string, got: %v", err)
					}
				})
			})
		})

		Context("when given names longer than 64 characters", func() {
			It("always returns ErrInvalidName", func() {
				rapid.Check(GinkgoT(), func(t *rapid.T) {
					// Generate a valid prefix then pad to exceed 64 chars.
					extra := rapid.IntRange(1, 200).Draw(t, "extra")
					name := "a" + strings.Repeat("b", 64+extra-1)
					err := analyzer.ValidateName(name)
					if err == nil || !errors.Is(err, analyzer.ErrInvalidName) {
						t.Fatalf("expected ErrInvalidName for %d-char name, got: %v", len(name), err)
					}
				})
			})
		})

		Context("when given names not starting with a lowercase letter", func() {
			It("always returns ErrInvalidName", func() {
				rapid.Check(GinkgoT(), func(t *rapid.T) {
					// Generate a first character that is NOT a-z.
					badFirstChars := []rune{
						'0', '1', '5', '9', 'A', 'B', 'Z',
						'-', '_', '.', '@', '!', ' ',
					}
					badFirst := rapid.SampledFrom(badFirstChars).Draw(t, "badFirst")
					tail := rapid.StringMatching(`[a-z0-9]{0,10}`).Draw(t, "tail")
					name := string(badFirst) + tail

					err := analyzer.ValidateName(name)
					if err == nil || !errors.Is(err, analyzer.ErrInvalidName) {
						t.Fatalf("expected ErrInvalidName for name %q starting with %q, got: %v",
							name, string(badFirst), err)
					}
				})
			})
		})
	})

	Describe("kahnSort properties", func() {

		Context("when given a valid DAG", func() {
			It("places every dependency before its dependent in the topological order", func() {
				rapid.Check(GinkgoT(), func(t *rapid.T) {
					// Generate 1-15 unique sorted node names.
					count := rapid.IntRange(1, 15).Draw(t, "nodeCount")
					nameSet := make(map[string]bool)
					for len(nameSet) < count {
						n := rapid.StringMatching(`[a-z][a-z0-9]{0,7}`).Draw(t, "nodeName")
						nameSet[n] = true
					}
					names := make([]string, 0, count)
					for n := range nameSet {
						names = append(names, n)
					}
					sort.Strings(names)

					// Build a DAG: only add edges from later to earlier names (forward
					// index -> lower index) to guarantee acyclicity.
					nodes := make(map[string]analyzer.RegisteredAnalyzer, count)
					edges := make(map[string][]string, count)
					for _, n := range names {
						nodes[n] = nil // kahnSort only uses map keys
					}
					for i := 1; i < count; i++ {
						// Each node may depend on 0..i earlier nodes.
						numDeps := rapid.IntRange(0, min(i, 3)).Draw(t, "numDeps")
						var deps []string
						used := make(map[int]bool)
						for d := 0; d < numDeps; d++ {
							idx := rapid.IntRange(0, i-1).Draw(t, "depIdx")
							if !used[idx] {
								used[idx] = true
								deps = append(deps, names[idx])
							}
						}
						edges[names[i]] = deps
					}

					levels, order := analyzer.ExportKahnSort(nodes, edges)

					// Property 1: order length equals node count.
					if len(order) != count {
						t.Fatalf("order length %d != node count %d", len(order), count)
					}

					// Property 2: every dependency appears before its dependent.
					pos := make(map[string]int, len(order))
					for i, n := range order {
						pos[n] = i
					}
					for node, deps := range edges {
						for _, dep := range deps {
							if pos[dep] >= pos[node] {
								t.Fatalf("dependency %q (pos %d) not before %q (pos %d)",
									dep, pos[dep], node, pos[node])
							}
						}
					}

					// Property 3: level 0 contains only nodes with zero in-degree.
					if len(levels) > 0 {
						for _, n := range levels[0] {
							if len(edges[n]) > 0 {
								t.Fatalf("level-0 node %q has %d dependencies", n, len(edges[n]))
							}
						}
					}
				})
			})
		})

		Context("when given the same input twice", func() {
			It("produces identical output (determinism)", func() {
				rapid.Check(GinkgoT(), func(t *rapid.T) {
					count := rapid.IntRange(1, 10).Draw(t, "nodeCount")
					nameSet := make(map[string]bool)
					for len(nameSet) < count {
						n := rapid.StringMatching(`[a-z][a-z0-9]{0,5}`).Draw(t, "nodeName")
						nameSet[n] = true
					}
					names := make([]string, 0, count)
					for n := range nameSet {
						names = append(names, n)
					}
					sort.Strings(names)

					nodes := make(map[string]analyzer.RegisteredAnalyzer, count)
					edges := make(map[string][]string, count)
					for _, n := range names {
						nodes[n] = nil
					}
					for i := 1; i < count; i++ {
						numDeps := rapid.IntRange(0, min(i, 2)).Draw(t, "numDeps")
						var deps []string
						used := make(map[int]bool)
						for d := 0; d < numDeps; d++ {
							idx := rapid.IntRange(0, i-1).Draw(t, "depIdx")
							if !used[idx] {
								used[idx] = true
								deps = append(deps, names[idx])
							}
						}
						edges[names[i]] = deps
					}

					levels1, order1 := analyzer.ExportKahnSort(nodes, edges)
					levels2, order2 := analyzer.ExportKahnSort(nodes, edges)

					if len(order1) != len(order2) {
						t.Fatalf("order lengths differ: %d vs %d", len(order1), len(order2))
					}
					for i := range order1 {
						if order1[i] != order2[i] {
							t.Fatalf("order[%d] differs: %q vs %q", i, order1[i], order2[i])
						}
					}
					if len(levels1) != len(levels2) {
						t.Fatalf("level counts differ: %d vs %d", len(levels1), len(levels2))
					}
					for i := range levels1 {
						if len(levels1[i]) != len(levels2[i]) {
							t.Fatalf("level[%d] lengths differ: %d vs %d", i, len(levels1[i]), len(levels2[i]))
						}
						for j := range levels1[i] {
							if levels1[i][j] != levels2[i][j] {
								t.Fatalf("level[%d][%d] differs: %q vs %q", i, j, levels1[i][j], levels2[i][j])
							}
						}
					}
				})
			})
		})
	})

	Describe("formatCycle properties", func() {

		Context("when given an empty slice", func() {
			It("returns the fallback message", func() {
				rapid.Check(GinkgoT(), func(t *rapid.T) {
					result := analyzer.ExportFormatCycle(nil)
					if result != "dependency cycle detected" {
						t.Fatalf("expected fallback message, got: %q", result)
					}
					result2 := analyzer.ExportFormatCycle([]string{})
					if result2 != "dependency cycle detected" {
						t.Fatalf("expected fallback message for empty slice, got: %q", result2)
					}
				})
			})
		})

		Context("when given non-empty elements", func() {
			It("joins them with arrow separators", func() {
				rapid.Check(GinkgoT(), func(t *rapid.T) {
					count := rapid.IntRange(1, 10).Draw(t, "count")
					elems := make([]string, count)
					for i := range elems {
						elems[i] = rapid.StringMatching(`[a-z]{1,8}`).Draw(t, "elem")
					}

					result := analyzer.ExportFormatCycle(elems)
					expected := strings.Join(elems, " -> ")
					if result != expected {
						t.Fatalf("expected %q, got %q", expected, result)
					}
				})
			})
		})
	})
})
