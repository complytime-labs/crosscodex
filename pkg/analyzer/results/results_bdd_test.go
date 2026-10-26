package results_test

import (
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
