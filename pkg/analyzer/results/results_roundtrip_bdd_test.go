package results_test

import (
	"encoding/json"
	"reflect"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
)

var _ = Describe("Result Round-Trips", func() {
	It("round-trips RequiresResult through JSON", func() {
		want := []results.RequiresResult{{
			SourceID: "AC-1", TargetID: "AC-2", Confidence: 0.8,
			Unanimous: true, ValidVotes: 3, TotalVotes: 3, Models: []string{"llama3.2:3b"},
		}}
		data, err := json.Marshal(want)
		Expect(err).NotTo(HaveOccurred())

		var got []results.RequiresResult
		Expect(json.Unmarshal(data, &got)).To(Succeed())
		Expect(reflect.DeepEqual(got[0], want[0])).To(BeTrue(),
			"round-trip mismatch: got %+v, want %+v", got[0], want[0])
	})

	It("round-trips SemanticMatchResult through JSON", func() {
		want := []results.SemanticMatchResult{{
			SourceID:         "AC-1",
			TargetID:         "AC-2",
			RelationshipType: "SUPPORTS",
			Confidence:       0.9,
			Properties:       map[string]string{"note": "derived"},
		}}
		data, err := json.Marshal(want)
		Expect(err).NotTo(HaveOccurred())

		var got []results.SemanticMatchResult
		Expect(json.Unmarshal(data, &got)).To(Succeed())
		Expect(reflect.DeepEqual(got[0], want[0])).To(BeTrue(),
			"round-trip mismatch: got %+v, want %+v", got[0], want[0])
	})

	It("round-trips ArtifactResult through JSON", func() {
		want := []results.ArtifactResult{{
			ControlID: "AC-1",
			Artifacts: []results.Artifact{{
				Name:       "Security Log",
				Type:       "log",
				Frequency:  "daily",
				OwnerRole:  "admin",
				Confidence: 0.7,
			}},
		}}
		data, err := json.Marshal(want)
		Expect(err).NotTo(HaveOccurred())

		var got []results.ArtifactResult
		Expect(json.Unmarshal(data, &got)).To(Succeed())
		Expect(got[0].ControlID).To(Equal(want[0].ControlID))
		Expect(got[0].Artifacts[0]).To(Equal(want[0].Artifacts[0]))
	})

	It("round-trips ClassifyResult through JSON", func() {
		want := []results.ClassifyResult{{
			ControlID: "AC-1",
			Type:      "technical",
			Level:     "high",
		}}
		data, err := json.Marshal(want)
		Expect(err).NotTo(HaveOccurred())

		var got []results.ClassifyResult
		Expect(json.Unmarshal(data, &got)).To(Succeed())
		Expect(got[0]).To(Equal(want[0]))
	})

	It("round-trips EmbedResult through JSON", func() {
		want := []results.EmbedResult{{
			ControlID: "AC-1",
		}}
		data, err := json.Marshal(want)
		Expect(err).NotTo(HaveOccurred())

		var got []results.EmbedResult
		Expect(json.Unmarshal(data, &got)).To(Succeed())
		Expect(got[0]).To(Equal(want[0]))
	})
})
