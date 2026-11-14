package results_test

import (
	"encoding/json"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
)

func TestAnalyzerResultsBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Analyzer Results BDD Suite")
}

// Redirect slog output to GinkgoWriter so log noise only appears on failure.
var _ = BeforeSuite(func() { DeferCleanup(testspecs.RedirectLogsToGinkgo()) })

var _ = Describe("semantic match edge identity", func() {
	It("names the label the graph writer and the mappings reader share", func() {
		Expect(results.SemanticMatchEdgeLabel).To(Equal("SEMANTIC_MATCH"))
		Expect(results.SemanticMatchEdgeIDKind).To(Equal("semantic-match"))
	})
})

var _ = Describe("synthesized mapping projection", func() {
	It("names the ID kind, stage property and values the graph writer and readers share", func() {
		Expect(results.SemanticMatchSynthesizedIDKind).To(Equal("semantic-match-synthesized"))
		Expect(results.StageProperty).To(Equal("stage"))
		Expect(results.StageAnalyzer).To(Equal("analyzer"))
		Expect(results.StageSynthesized).To(Equal("synthesized"))
		Expect(results.SynthesisStage).To(Equal("synthesis"))
	})

	It("encodes a synthesized mapping with snake_case keys", func() {
		data, err := json.Marshal(results.SynthesizedMapping{SourceID: "a", TargetID: "b", RelationshipType: "EQUIVALENT",
			Confidence: 0.9, Viability: 0.5, IsViable: true})
		Expect(err).NotTo(HaveOccurred())
		Expect(data).To(MatchJSON(`{"source_id":"a","target_id":"b","relationship_type":"EQUIVALENT","confidence":0.9,"viability":0.5,"is_viable":true}`))
	})
})

var _ = Describe("analyzer names", func() {
	It("names the analyzers whose results are persisted under their own name", func() {
		Expect(results.RequiresAnalyzer).To(Equal("requires"))
		Expect(results.RelationshipAnalyzer).To(Equal("relationship"))
	})
})

var _ = Describe("semantic match properties", func() {
	It("names the keys the relationship analyzer writes and synthesis reads", func() {
		Expect(results.SemanticMatchContributionTypeKey).To(Equal("contribution_type"))
		Expect(results.SemanticMatchUnanimousKey).To(Equal("unanimous"))
	})
})
