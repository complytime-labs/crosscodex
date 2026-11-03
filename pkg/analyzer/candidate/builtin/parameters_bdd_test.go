package builtin_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/candidate"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/candidate/builtin"
)

// Generator parameters arrive as interface{} values whose Go type depends on
// the decoder: YAML yields int for 75 and float64 for 72.5, JSON yields
// float64 for every number. These specs pin that every numeric shape of a
// valid value is honoured and that an unusable value is an error, never a
// silent fallback to the default.
var _ = Describe("Generator numeric parameters", func() {
	controls := map[string]*candidate.ControlData{
		"AC-1": {ControlID: "AC-1", Text: "Establish a policy", Level: "Strategic"},
		"AC-2": {ControlID: "AC-2", Text: "Review logs", Level: "Operational"},
		"AC-3": {ControlID: "AC-3", Text: "Rotate keys", Level: "Operational"},
	}
	matrix := &SimilarityMatrix{
		IDs: []string{"AC-1", "AC-2", "AC-3"},
		Values: [][]float32{
			{100, 80, 60},
			{80, 100, 70},
			{60, 70, 100},
		},
	}
	request := func(params map[string]interface{}) candidate.GenerateRequest {
		return candidate.GenerateRequest{
			TenantID: "test-tenant", JobID: "test-job",
			SourceControls: controls, TargetControls: controls,
			EmbeddingMatrix: matrix,
			Parameters:      params,
		}
	}

	DescribeTable("semantic honours top_k of every integer shape",
		func(topK interface{}) {
			candidates, err := builtin.NewSemanticGenerator().Generate(context.Background(), request(map[string]interface{}{"top_k": topK}))
			Expect(err).NotTo(HaveOccurred())
			Expect(candidates).To(HaveLen(3), "one candidate per source")
		},
		Entry("int", 1),
		Entry("int64", int64(1)),
		Entry("integral float64", 1.0),
	)

	DescribeTable("semantic honours min_similarity of every numeric shape",
		func(minSimilarity interface{}) {
			candidates, err := builtin.NewSemanticGenerator().Generate(context.Background(), request(map[string]interface{}{"min_similarity": minSimilarity}))
			Expect(err).NotTo(HaveOccurred())
			Expect(candidates).To(HaveLen(2), "only the AC-1/AC-2 pair (80) clears 75")
		},
		Entry("int", 75),
		Entry("int64", int64(75)),
		Entry("float64", 75.0),
	)

	DescribeTable("every generator honours weight of every numeric shape",
		func(gen candidate.Generator, weight interface{}, want float64) {
			candidates, err := gen.Generate(context.Background(), request(map[string]interface{}{"weight": weight}))
			Expect(err).NotTo(HaveOccurred())
			Expect(candidates).NotTo(BeEmpty())
			for _, c := range candidates {
				Expect(c.Weight).To(Equal(want))
			}
		},
		Entry("semantic float64", builtin.NewSemanticGenerator(), 0.25, 0.25),
		Entry("semantic int", builtin.NewSemanticGenerator(), 0, 0.0),
		Entry("keyword float64", builtin.NewKeywordGenerator(), 0.25, 0.25),
		Entry("keyword int64", builtin.NewKeywordGenerator(), int64(0), 0.0),
		Entry("level float64", builtin.NewLevelGenerator(), 0.25, 0.25),
		Entry("level int", builtin.NewLevelGenerator(), 0, 0.0),
	)

	DescribeTable("rejects an unusable parameter instead of using the default",
		func(gen candidate.Generator, params map[string]interface{}, msg string) {
			candidates, err := gen.Generate(context.Background(), request(params))
			Expect(err).To(MatchError(ContainSubstring(msg)))
			Expect(candidates).To(BeNil())
		},
		Entry("semantic string top_k", builtin.NewSemanticGenerator(),
			map[string]interface{}{"top_k": "3"}, `semantic: parameter top_k "3" (string) must be an integer`),
		Entry("semantic fractional top_k", builtin.NewSemanticGenerator(),
			map[string]interface{}{"top_k": 1.5}, `semantic: parameter top_k 1.5 (float64) must be an integer`),
		Entry("semantic string min_similarity", builtin.NewSemanticGenerator(),
			map[string]interface{}{"min_similarity": "high"}, `semantic: parameter min_similarity "high" (string) must be a number`),
		Entry("keyword string weight", builtin.NewKeywordGenerator(),
			map[string]interface{}{"weight": "heavy"}, `keyword: parameter weight "heavy" (string) must be a number`),
		Entry("level string weight", builtin.NewLevelGenerator(),
			map[string]interface{}{"weight": "heavy"}, `level: parameter weight "heavy" (string) must be a number`),
		Entry("semantic zero top_k", builtin.NewSemanticGenerator(),
			map[string]interface{}{"top_k": 0}, `semantic: parameter top_k 0 (int) must be a positive integer`),
		Entry("semantic negative top_k", builtin.NewSemanticGenerator(),
			map[string]interface{}{"top_k": -2}, `semantic: parameter top_k -2 (int) must be a positive integer`),
		Entry("keyword non-list keywords", builtin.NewKeywordGenerator(),
			map[string]interface{}{"keywords": 5}, `keyword: parameter keywords 5 (int) must be a non-empty list of non-empty strings`),
		Entry("keyword empty keywords", builtin.NewKeywordGenerator(),
			map[string]interface{}{"keywords": []interface{}{}}, `keyword: parameter keywords [] ([]interface {}) must be a non-empty list of non-empty strings`),
		Entry("keyword non-string keyword", builtin.NewKeywordGenerator(),
			map[string]interface{}{"keywords": []interface{}{"policy", 7}}, `keyword: parameter keywords [policy 7] ([]interface {}) must be a non-empty list of non-empty strings`),
		Entry("keyword blank keyword", builtin.NewKeywordGenerator(),
			map[string]interface{}{"keywords": []string{"policy", " "}}, `keyword: parameter keywords [policy  ] ([]string) must be a non-empty list of non-empty strings`),
	)

	DescribeTable("keyword honours keywords of either list shape",
		func(keywords interface{}) {
			candidates, err := builtin.NewKeywordGenerator().Generate(context.Background(), request(map[string]interface{}{"keywords": keywords}))
			Expect(err).NotTo(HaveOccurred())
			Expect(candidates).To(HaveLen(2), "only AC-2 contains the keyword; AC-2 is never its own candidate")
			for _, c := range candidates {
				Expect(c.TargetID).To(Equal("AC-2"))
			}
		},
		Entry("YAML/JSON []interface{}", []interface{}{"logs"}),
		Entry("Go []string", []string{"logs"}),
	)
})
