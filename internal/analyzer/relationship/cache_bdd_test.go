package relationship_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/types"
	"google.golang.org/protobuf/types/known/structpb"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/analyzer/relationship"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/prompt"
	"github.com/complytime-labs/crosscodex/pkg/resultcache"
)

var _ = Describe("relationship result reuse", func() {
	spec := &prompt.PromptSpec{Name: "relationship", Version: "1.0.0", Templates: prompt.TemplateSet{
		System: "System: ${classification_guidance} ${relationship_definitions}",
		User:   "User: ${source_text} ${target_text} ${source_id} ${target_id} ${source_framework} ${target_framework} ${few_shot_examples}",
	}}
	newAnalyzer := func(mutate ...func(*config.RelationshipConfig)) *relationship.RelationshipAnalyzer {
		candidates := &mockCandidateProvider{pairs: []relationship.CandidatePair{{SourceControlID: "AC-1", TargetControlID: "IT-3", SimilarityScore: 87}}}
		cfg := config.RelationshipConfig{Enabled: true, Models: []string{"m"},
			MaxSourceChars: 1500, MaxTargetChars: 800, MaxTokens: 300, SamplesPerModel: 3, SamplingTemperature: 0.3}
		for _, m := range mutate {
			m(&cfg)
		}
		return relationship.New(nil, specRegistry(spec), candidates, cfg)
	}
	keysFor := func(a *relationship.RelationshipAnalyzer, params map[string]string) []string {
		tasks, err := a.GenerateWork(testspecs.SetupTenantContext("test-tenant"), &pb.Control{},
			analyzer.AnalyzerConfig{Parameters: params})
		Expect(err).NotTo(HaveOccurred())
		Expect(tasks).NotTo(BeEmpty(), "GenerateWork produced no task to key")
		var keys []string
		for _, t := range tasks {
			req, ok := a.CacheIdentity(t)
			Expect(ok).To(BeTrue())
			Expect(req.SourceControlID).To(Equal("AC-1"))
			Expect(req.TargetControlID).To(Equal("IT-3"))
			keys = append(keys, resultcache.Key("relationship", req.Identity, resultcache.ModelIdentity{Observed: "observed"}))
		}
		return keys
	}
	keysIn := func(jobID string) []string { return keysFor(newAnalyzer(), pairParams(jobID, "AC-1", "IT-3")) }
	editedText := func(key string) map[string]string {
		params := pairParams("job-1", "AC-1", "IT-3")
		params[key] = "an edited statement"
		return params
	}

	It("names every panel model as a completion model", func() {
		Expect(newAnalyzer().CacheModels()).To(Equal([]analyzer.ModelUse{{Alias: "m", Kind: analyzer.ModelCompletion}}))
	})

	It("gives N voters on one pair N distinct keys", func() {
		keys := keysIn("job-1")
		Expect(keys).To(HaveLen(3))
		Expect(map[string]bool{keys[0]: true, keys[1]: true, keys[2]: true}).To(HaveLen(3))
	})

	It("gives the same pair the same keys in two jobs", func() {
		Expect(keysIn("job-2")).To(Equal(keysIn("job-1")))
	})

	DescribeTable("changes every key when the request changes",
		func(a *relationship.RelationshipAnalyzer, params map[string]string) {
			changed := keysFor(a, params)
			Expect(changed).To(HaveLen(3), "one key per sample")
			for _, k := range keysIn("job-1") {
				Expect(changed).NotTo(ContainElement(k))
			}
		},
		Entry("the source text", newAnalyzer(), editedText("source_text_AC-1")),
		Entry("the target text", newAnalyzer(), editedText("target_text_IT-3")),
		Entry("the token limit", newAnalyzer(func(c *config.RelationshipConfig) { c.MaxTokens = 301 }), pairParams("job-1", "AC-1", "IT-3")),
		Entry("the temperature", newAnalyzer(func(c *config.RelationshipConfig) { c.SamplingTemperature = 0.4 }), pairParams("job-1", "AC-1", "IT-3")),
	)

	It("adds keys for a second panel model without changing the first model's", func() {
		two := keysFor(newAnalyzer(func(c *config.RelationshipConfig) { c.Models = []string{"m", "m2"} }), pairParams("job-1", "AC-1", "IT-3"))
		Expect(two).To(HaveLen(6))
		Expect(map[string]bool{two[0]: true, two[1]: true, two[2]: true, two[3]: true, two[4]: true, two[5]: true}).To(HaveLen(6))
		Expect(two[:3]).To(Equal(keysIn("job-1")))
	})

	// A result Aggregate would discard must not be stored: every later job
	// would replay the same failure instead of asking the model again.
	DescribeTable("stores only a result Aggregate would use",
		func(fields map[string]any, want types.GomegaMatcher) {
			result, err := structpb.NewStruct(fields)
			Expect(err).NotTo(HaveOccurred())
			Expect(newAnalyzer().CacheableResult(result)).To(want)
		},
		Entry("a parsed relationship", map[string]any{"model": "m", "response": "RELATIONSHIP: SUPERSET_OF\nCONFIDENCE: HIGH"}, Succeed()),
		Entry("a response naming no known relationship", map[string]any{"model": "m", "response": "I am not sure."}, MatchError(ContainSubstring("does not parse"))),
		Entry("a result without a response", map[string]any{"model": "m"}, MatchError(ContainSubstring("no response text"))),
	)
})
