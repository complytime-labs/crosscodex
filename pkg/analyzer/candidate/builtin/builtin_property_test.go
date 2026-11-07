package builtin_test

import (
	"context"
	"strings"
	"unicode"

	. "github.com/onsi/ginkgo/v2"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/candidate"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/candidate/builtin"
)

// Suite bootstrap lives in builtin_bdd_test.go.

// drawControls draws texts of 0-6 words from a small vocabulary, joined by
// mixed separators and case, for a distinct subset of ids.
func drawControls(t *rapid.T, label string, ids []string) map[string]*candidate.ControlData {
	words := []string{"access", "control", "audit", "log", "AC-2", "encrypt", "data"}
	controls := map[string]*candidate.ControlData{}
	for _, id := range rapid.SliceOfDistinct(rapid.SampledFrom(ids), func(s string) string { return s }).Draw(t, label) {
		text := strings.Join(rapid.SliceOfN(rapid.SampledFrom(words), 0, 6).Draw(t, label+"."+id), rapid.SampledFrom([]string{" ", ", ", "; "}).Draw(t, "sep"))
		controls[id] = &candidate.ControlData{ControlID: id, Text: text}
	}
	return controls
}

// tokenSet mirrors the generator's documented tokenization: lowercase, split on
// every non-letter, non-digit rune.
func tokenSet(text string) map[string]bool {
	set := map[string]bool{}
	for _, tok := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		set[tok] = true
	}
	return set
}

var _ = Describe("Property Specifications", Ordered, func() {
	Describe("LexicalGenerator", func() {
		It("returns at most top_k targets per source, each sharing a term and never the source itself, with scores in (0, 1] led by 1.0", func() {
			gen := builtin.NewLexicalGenerator()
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				// Overlapping id pools so a source can also appear as a target.
				sources := drawControls(t, "sources", []string{"A", "B", "C"})
				targets := drawControls(t, "targets", []string{"B", "C", "D", "E", "F"})
				topK := rapid.IntRange(1, 5).Draw(t, "top_k")

				out, err := gen.Generate(context.Background(), candidate.GenerateRequest{
					SourceControls: sources, TargetControls: targets,
					Parameters: map[string]any{"top_k": topK},
				})
				if err != nil {
					t.Fatalf("generate: %v", err)
				}

				perSource := map[string]int{}
				for i, c := range out {
					perSource[c.SourceID]++
					if c.SourceID == c.TargetID {
						t.Fatalf("source %s paired with itself", c.SourceID)
					}
					if c.Score <= 0 || c.Score > 1 {
						t.Fatalf("%s->%s: score %v outside (0, 1]", c.SourceID, c.TargetID, c.Score)
					}
					shared := false
					for tok := range tokenSet(sources[c.SourceID].Text) {
						shared = shared || tokenSet(targets[c.TargetID].Text)[tok]
					}
					if !shared {
						t.Fatalf("%s->%s: no shared term", c.SourceID, c.TargetID)
					}
					if i == 0 || out[i-1].SourceID != c.SourceID {
						if c.Score != 1 {
							t.Fatalf("source %s: top score %v, want 1", c.SourceID, c.Score)
						}
						if i > 0 && out[i-1].SourceID > c.SourceID {
							t.Fatalf("source %s listed after %s", c.SourceID, out[i-1].SourceID)
						}
					} else if out[i-1].Score < c.Score {
						t.Fatalf("source %s: score %v after %v", c.SourceID, c.Score, out[i-1].Score)
					}
				}
				for source, n := range perSource {
					if n > topK {
						t.Fatalf("source %s: %d candidates exceed top_k %d", source, n, topK)
					}
				}
			})
		})
	})
})
