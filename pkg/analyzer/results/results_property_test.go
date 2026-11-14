package results_test

import (
	"encoding/json"
	"reflect"

	. "github.com/onsi/ginkgo/v2"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
)

// Suite bootstrap lives in results_bdd_test.go.

const maxLen = 20

func genString() *rapid.Generator[string] {
	return rapid.StringN(0, maxLen, -1)
}

// genFloat draws finite floats only: JSON cannot carry NaN or Inf.
func genFloat() *rapid.Generator[float64] {
	return rapid.Float64Range(-1e12, 1e12)
}

// roundTrip marshals v and unmarshals into a fresh T. It fails t (not
// Gomega) because it runs inside rapid.Check.
func roundTrip[T any](t *rapid.T, v T) T {
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %+v: %v", v, err)
	}
	var out T
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal %s: %v", data, err)
	}
	return out
}

// expectRoundTrip fails t unless v survives a JSON round trip unchanged.
func expectRoundTrip[T any](t *rapid.T, v T) {
	if got := roundTrip(t, v); !reflect.DeepEqual(got, v) {
		t.Fatalf("round trip changed the value:\n got %+v\nwant %+v", got, v)
	}
}

func genRequires() *rapid.Generator[results.RequiresResult] {
	return rapid.Custom(func(t *rapid.T) results.RequiresResult {
		return results.RequiresResult{
			SourceID:   genString().Draw(t, "source_id"),
			TargetID:   genString().Draw(t, "target_id"),
			Confidence: genFloat().Draw(t, "confidence"),
			Unanimous:  rapid.Bool().Draw(t, "unanimous"),
			ValidVotes: rapid.IntRange(0, 1000).Draw(t, "valid_votes"),
			TotalVotes: rapid.IntRange(0, 1000).Draw(t, "total_votes"),
			Models:     rapid.SliceOfN(genString(), 1, 5).Draw(t, "models"),
			// Absent and non-empty only: omitempty encodes an empty slice as
			// absent, which decodes as nil.
			Justifications: genJustifications().Draw(t, "justifications"),
		}
	})
}

func genSemanticMatch() *rapid.Generator[results.SemanticMatchResult] {
	return rapid.Custom(func(t *rapid.T) results.SemanticMatchResult {
		return results.SemanticMatchResult{
			SourceID:         genString().Draw(t, "source_id"),
			TargetID:         genString().Draw(t, "target_id"),
			RelationshipType: genString().Draw(t, "relationship_type"),
			Confidence:       genFloat().Draw(t, "confidence"),
			Properties:       rapid.MapOfN(genString(), genString(), 1, 5).Draw(t, "properties"),
			Justifications:   genJustifications().Draw(t, "justifications"),
		}
	})
}

// genJustifications draws nil or a non-empty slice of justifications.
func genJustifications() *rapid.Generator[[]string] {
	return rapid.OneOf(rapid.Just[[]string](nil), rapid.SliceOfN(genString(), 1, 5))
}

func genCritique() *rapid.Generator[results.CritiqueResult] {
	return rapid.Custom(func(t *rapid.T) results.CritiqueResult {
		return results.CritiqueResult{
			Kind:            rapid.SampledFrom([]string{"relationship", "requires"}).Draw(t, "kind"),
			SourceControlID: genString().Draw(t, "source_control_id"),
			TargetControlID: genString().Draw(t, "target_control_id"),
			Upheld:          rapid.IntRange(0, 100).Draw(t, "upheld"),
			Rejected:        rapid.IntRange(0, 100).Draw(t, "rejected"),
			Failed:          rapid.IntRange(0, 100).Draw(t, "failed"),
			Outcome:         rapid.SampledFrom([]string{results.CritiqueUpheld, results.CritiqueSplit, results.CritiqueVetoed, results.CritiqueFailed}).Draw(t, "outcome"),
			Vetoed:          rapid.Bool().Draw(t, "vetoed"),
			Reasons:         rapid.SliceOfN(genString(), 1, 5).Draw(t, "reasons"),
		}
	})
}

func genArtifact() *rapid.Generator[results.Artifact] {
	return rapid.Custom(func(t *rapid.T) results.Artifact {
		return results.Artifact{
			Name:       genString().Draw(t, "name"),
			Type:       genString().Draw(t, "type"),
			Frequency:  genString().Draw(t, "frequency"),
			OwnerRole:  genString().Draw(t, "owner_role"),
			Confidence: genFloat().Draw(t, "confidence"),
		}
	})
}

var _ = Describe("Property Specifications", Ordered, func() {

	Describe("JSON round-trip of persisted result shapes", func() {
		It("preserves RequiresResult", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				in := genRequires().Draw(t, "requires")
				expectRoundTrip(t, in)
			})
		})

		It("preserves SemanticMatchResult", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				in := genSemanticMatch().Draw(t, "semantic_match")
				expectRoundTrip(t, in)
			})
		})

		It("preserves CritiqueResult", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				in := genCritique().Draw(t, "critique")
				expectRoundTrip(t, in)
			})
		})

		It("preserves Artifact", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				in := genArtifact().Draw(t, "artifact")
				expectRoundTrip(t, in)
			})
		})

		It("preserves ArtifactResult", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				in := results.ArtifactResult{
					ControlID: genString().Draw(t, "control_id"),
					Artifacts: rapid.SliceOfN(genArtifact(), 1, 5).Draw(t, "artifacts"),
				}
				expectRoundTrip(t, in)
			})
		})

		It("preserves ClassifyResult", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				in := results.ClassifyResult{
					ControlID: genString().Draw(t, "control_id"),
					Type:      genString().Draw(t, "type"),
					Level:     genString().Draw(t, "level"),
				}
				expectRoundTrip(t, in)
			})
		})

		It("preserves EmbedResult", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				in := results.EmbedResult{ControlID: genString().Draw(t, "control_id")}
				expectRoundTrip(t, in)
			})
		})

		It("preserves the order of a []SemanticMatchResult", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				in := rapid.SliceOfN(genSemanticMatch(), 1, 8).Draw(t, "matches")
				expectRoundTrip(t, in)
			})
		})
	})

	Describe("critique verdicts", func() {
		It("vetoes exactly the vetoed results of the applied kind and notes only split and failed ones", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				crit := rapid.SliceOfNDistinct(genCritique(), 0, 8, func(c results.CritiqueResult) [3]string {
					return [3]string{c.Kind, c.SourceControlID, c.TargetControlID}
				}).Draw(t, "critiques")
				kind := rapid.SampledFrom([]string{"relationship", "requires"}).Draw(t, "kind")
				v := results.Apply(kind, crit)
				again := results.Apply(kind, crit)
				for _, c := range crit {
					got, ok := v.Result(c.SourceControlID, c.TargetControlID)
					if c.Kind != kind {
						if ok && got.Kind != kind {
							t.Fatalf("Apply(%q) returned a %q result for %s -> %s", kind, got.Kind, c.SourceControlID, c.TargetControlID)
						}
						continue
					}
					if !ok || !reflect.DeepEqual(got, c) {
						t.Fatalf("Apply(%q).Result(%q, %q) = %+v, %v; want %+v", kind, c.SourceControlID, c.TargetControlID, got, ok, c)
					}
					if v.Vetoed(c.SourceControlID, c.TargetControlID) != c.Vetoed {
						t.Fatalf("Vetoed(%q, %q) = %v; want %v", c.SourceControlID, c.TargetControlID, !c.Vetoed, c.Vetoed)
					}
					noted := v.Note(c.SourceControlID, c.TargetControlID) != ""
					if want := c.Outcome == results.CritiqueSplit || c.Outcome == results.CritiqueFailed; noted != want {
						t.Fatalf("outcome %q: noted = %v; want %v", c.Outcome, noted, want)
					}
					if again.Vetoed(c.SourceControlID, c.TargetControlID) != c.Vetoed {
						t.Fatalf("applying the same critique again changed the decision for %s -> %s", c.SourceControlID, c.TargetControlID)
					}
				}
			})
		})
	})
})
