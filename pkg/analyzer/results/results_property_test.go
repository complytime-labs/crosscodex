package results_test

import (
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
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

// roundTrip marshals v and unmarshals into a fresh T.
func roundTrip[T any](v T) T {
	data, err := json.Marshal(v)
	Expect(err).NotTo(HaveOccurred())
	var out T
	Expect(json.Unmarshal(data, &out)).To(Succeed())
	return out
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
				Expect(roundTrip(in)).To(Equal(in))
			})
		})

		It("preserves SemanticMatchResult", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				in := genSemanticMatch().Draw(t, "semantic_match")
				Expect(roundTrip(in)).To(Equal(in))
			})
		})

		It("preserves Artifact", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				in := genArtifact().Draw(t, "artifact")
				Expect(roundTrip(in)).To(Equal(in))
			})
		})

		It("preserves ArtifactResult", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				in := results.ArtifactResult{
					ControlID: genString().Draw(t, "control_id"),
					Artifacts: rapid.SliceOfN(genArtifact(), 1, 5).Draw(t, "artifacts"),
				}
				Expect(roundTrip(in)).To(Equal(in))
			})
		})

		It("preserves ClassifyResult", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				in := results.ClassifyResult{
					ControlID: genString().Draw(t, "control_id"),
					Type:      genString().Draw(t, "type"),
					Level:     genString().Draw(t, "level"),
				}
				Expect(roundTrip(in)).To(Equal(in))
			})
		})

		It("preserves EmbedResult", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				in := results.EmbedResult{ControlID: genString().Draw(t, "control_id")}
				Expect(roundTrip(in)).To(Equal(in))
			})
		})

		It("preserves the order of a []SemanticMatchResult", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				in := rapid.SliceOfN(genSemanticMatch(), 1, 8).Draw(t, "matches")
				Expect(roundTrip(in)).To(Equal(in))
			})
		})
	})
})
