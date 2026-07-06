package requires_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/analyzer/requires"
)

var _ = Describe("RequiresPair", func() {
	It("constructs with all fields", func() {
		provenance := []requires.CandidateProvenance{
			{
				GeneratorName: "generator1",
				Score:         0.85,
				Weight:        0.5,
				Metadata:      map[string]string{"key": "value"},
			},
		}
		pair := requires.RequiresPair{
			SourceControlID: "SC001",
			TargetControlID: "SC002",
			AggregateScore:  0.85,
			Provenance:      provenance,
		}

		Expect(pair.SourceControlID).To(Equal("SC001"))
		Expect(pair.TargetControlID).To(Equal("SC002"))
		Expect(pair.AggregateScore).To(Equal(0.85))
		Expect(pair.Provenance).To(HaveLen(1))
		Expect(pair.Provenance[0].GeneratorName).To(Equal("generator1"))
		Expect(pair.Provenance[0].Score).To(Equal(0.85))
		Expect(pair.Provenance[0].Weight).To(Equal(0.5))
		Expect(pair.Provenance[0].Metadata).To(HaveKeyWithValue("key", "value"))
	})

	It("supports multiple provenance entries", func() {
		pair := requires.RequiresPair{
			SourceControlID: "SC001",
			TargetControlID: "SC002",
			AggregateScore:  0.75,
			Provenance: []requires.CandidateProvenance{
				{
					GeneratorName: "gen1",
					Score:         0.80,
					Weight:        0.6,
					Metadata:      map[string]string{"method": "cosine"},
				},
				{
					GeneratorName: "gen2",
					Score:         0.70,
					Weight:        0.4,
					Metadata:      map[string]string{"method": "euclidean"},
				},
			},
		}

		Expect(pair.Provenance).To(HaveLen(2))
		Expect(pair.Provenance[0].GeneratorName).To(Equal("gen1"))
		Expect(pair.Provenance[1].GeneratorName).To(Equal("gen2"))
	})

	It("allows empty metadata", func() {
		pair := requires.RequiresPair{
			SourceControlID: "SC001",
			TargetControlID: "SC002",
			AggregateScore:  0.5,
			Provenance: []requires.CandidateProvenance{
				{
					GeneratorName: "gen1",
					Score:         0.5,
					Weight:        1.0,
					Metadata:      map[string]string{},
				},
			},
		}

		Expect(pair.Provenance[0].Metadata).To(Equal(map[string]string{}))
	})

	It("allows nil metadata", func() {
		pair := requires.RequiresPair{
			SourceControlID: "SC001",
			TargetControlID: "SC002",
			AggregateScore:  0.5,
			Provenance: []requires.CandidateProvenance{
				{
					GeneratorName: "gen1",
					Score:         0.5,
					Weight:        1.0,
					Metadata:      nil,
				},
			},
		}

		Expect(pair.Provenance[0].Metadata).To(BeNil())
	})
})

var _ = Describe("CandidateProvenance", func() {
	It("constructs with all fields", func() {
		prov := requires.CandidateProvenance{
			GeneratorName: "embedding_analyzer",
			Score:         0.92,
			Weight:        0.75,
			Metadata: map[string]string{
				"model":    "sentence-transformers",
				"distance": "cosine",
			},
		}

		Expect(prov.GeneratorName).To(Equal("embedding_analyzer"))
		Expect(prov.Score).To(Equal(0.92))
		Expect(prov.Weight).To(Equal(0.75))
		Expect(prov.Metadata).To(HaveLen(2))
		Expect(prov.Metadata["model"]).To(Equal("sentence-transformers"))
		Expect(prov.Metadata["distance"]).To(Equal("cosine"))
	})

	It("allows zero scores", func() {
		prov := requires.CandidateProvenance{
			GeneratorName: "gen",
			Score:         0.0,
			Weight:        1.0,
			Metadata:      map[string]string{},
		}

		Expect(prov.Score).To(Equal(0.0))
	})

	It("allows maximum scores", func() {
		prov := requires.CandidateProvenance{
			GeneratorName: "gen",
			Score:         1.0,
			Weight:        1.0,
			Metadata:      map[string]string{},
		}

		Expect(prov.Score).To(Equal(1.0))
	})
})
