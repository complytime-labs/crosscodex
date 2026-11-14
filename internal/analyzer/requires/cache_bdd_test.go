package requires_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/types"
	"google.golang.org/protobuf/types/known/structpb"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/analyzer/requires"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/prompt"
	"github.com/complytime-labs/crosscodex/pkg/prompt/prompttest"
	"github.com/complytime-labs/crosscodex/pkg/resultcache"
)

var _ = Describe("requires result reuse", func() {
	spec := &prompt.PromptSpec{Name: "requires", Version: "1.0.0", Templates: prompt.TemplateSet{
		System: "System: ${requires_guidance} ${dependency_definitions}",
		User:   "User: ${source_text} ${target_text} ${source_id} ${target_id} ${few_shot_examples}",
	}}
	newAnalyzer := func(mutate ...func(*config.RequiresConfig)) *requires.RequiresAnalyzer {
		candidates := &mockCandidateProvider{pairs: []requires.RequiresPair{{SourceControlID: "AC-1", TargetControlID: "AC-2", AggregateScore: 0.8}}}
		prompts := &prompttest.Fake{ResolveFunc: func(context.Context, string) (*prompt.PromptSpec, error) { return spec, nil }}
		cfg := config.RequiresConfig{Enabled: true, Models: []string{"m"},
			MaxSourceChars: 1500, MaxTargetChars: 800, MaxTokens: 300, SamplesPerModel: 3, SamplingTemperature: 0.3}
		for _, m := range mutate {
			m(&cfg)
		}
		return requires.New(nil, prompts, candidates, cfg)
	}
	keysFor := func(a *requires.RequiresAnalyzer, params map[string]string) []string {
		tasks, err := a.GenerateWork(testspecs.SetupTenantContext("test-tenant"), &pb.Control{},
			analyzer.AnalyzerConfig{Parameters: params})
		Expect(err).NotTo(HaveOccurred())
		Expect(tasks).NotTo(BeEmpty(), "GenerateWork produced no task to key")
		var keys []string
		for _, t := range tasks {
			req, ok := a.CacheIdentity(t)
			Expect(ok).To(BeTrue())
			keys = append(keys, resultcache.Key("requires", req.Identity, resultcache.ModelIdentity{Observed: "observed"}))
		}
		return keys
	}
	keysIn := func(jobID string) []string { return keysFor(newAnalyzer(), pairParams(jobID, "AC-1", "AC-2")) }
	editedText := func(key string) map[string]string {
		params := pairParams("job-1", "AC-1", "AC-2")
		params[key] = "an edited statement"
		return params
	}

	It("names every panel model as a completion model", func() {
		Expect(newAnalyzer().CacheModels()).To(Equal([]analyzer.ModelUse{{Alias: "m", Kind: analyzer.ModelCompletion}}))
	})

	DescribeTable("declares its panel",
		func(allow bool) {
			a := newAnalyzer(func(c *config.RequiresConfig) {
				c.Models = []string{"panel-1", "panel-2"}
				c.AllowSharedPanelModels = allow
			})
			Expect(a.Panel()).To(Equal(analyzer.Panel{Aliases: []string{"panel-1", "panel-2"}, AllowShared: allow}))
		},
		Entry("with the allowance off", false),
		Entry("with the allowance on", true),
	)

	It("gives N voters on one pair N distinct keys", func() {
		keys := keysIn("job-1")
		Expect(keys).To(HaveLen(3))
		Expect(map[string]bool{keys[0]: true, keys[1]: true, keys[2]: true}).To(HaveLen(3))
	})

	It("gives the same pair the same keys in two jobs", func() {
		Expect(keysIn("job-2")).To(Equal(keysIn("job-1")))
	})

	DescribeTable("changes every key when the request changes",
		func(a *requires.RequiresAnalyzer, params map[string]string) {
			changed := keysFor(a, params)
			Expect(changed).To(HaveLen(3), "one key per sample")
			for _, k := range keysIn("job-1") {
				Expect(changed).NotTo(ContainElement(k))
			}
		},
		Entry("the source text", newAnalyzer(), editedText("source_text_AC-1")),
		Entry("the target text", newAnalyzer(), editedText("target_text_AC-2")),
		Entry("the token limit", newAnalyzer(func(c *config.RequiresConfig) { c.MaxTokens = 301 }), pairParams("job-1", "AC-1", "AC-2")),
		Entry("the temperature", newAnalyzer(func(c *config.RequiresConfig) { c.SamplingTemperature = 0.4 }), pairParams("job-1", "AC-1", "AC-2")),
	)

	It("adds keys for a second panel model without changing the first model's", func() {
		two := keysFor(newAnalyzer(func(c *config.RequiresConfig) { c.Models = []string{"m", "m2"} }), pairParams("job-1", "AC-1", "AC-2"))
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
		Entry("a parsed decision", map[string]any{"model": "m", "response": "REQUIRES: NO\nSANITY_CHECK: PASS\nCONFIDENCE: HIGH"}, Succeed()),
		Entry("a response with no YES or NO decision", map[string]any{"model": "m", "response": "REQUIRES: MAYBE"}, MatchError(ContainSubstring("does not parse"))),
		Entry("a response with no SANITY_CHECK line", map[string]any{"model": "m", "response": "REQUIRES: NO\nCONFIDENCE: HIGH"}, MatchError(ContainSubstring("SANITY_CHECK"))),
		Entry("a result without a response", map[string]any{"model": "m"}, MatchError(ContainSubstring("no response text"))),
	)
})
