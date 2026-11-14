package artifacts_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/types"
	"google.golang.org/protobuf/types/known/structpb"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/analyzer/artifacts"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/prompt"
	"github.com/complytime-labs/crosscodex/pkg/prompt/prompttest"
	"github.com/complytime-labs/crosscodex/pkg/resultcache"
)

var _ = Describe("artifacts result reuse", func() {
	spec := &prompt.PromptSpec{Name: "artifacts", Version: "1.0.0", Templates: prompt.TemplateSet{
		System: "System: ${extraction_guidance} ${artifact_type_definitions}",
		User:   "User: ${few_shot_examples} ${output_format} ${control_id} ${framework} ${context} ${title} ${requirement_text}",
	}}
	newAnalyzer := func(mutate ...func(*config.ArtifactsConfig)) *artifacts.ArtifactsAnalyzer {
		prompts := &prompttest.Fake{ResolveFunc: func(context.Context, string) (*prompt.PromptSpec, error) { return spec, nil }}
		cfg := config.ArtifactsConfig{Enabled: true, Models: []string{"model-a"}, SamplesPerModel: 3,
			SamplingTemperature: 0.3, MaxTokens: 500, MaxTextChars: 1500, FuzzyThreshold: 0.6}
		for _, m := range mutate {
			m(&cfg)
		}
		return artifacts.New(nil, prompts, cfg)
	}
	control := &pb.Control{Identifier: "AC-1", Statement: "Define access control policy.", Parts: map[string]string{"class": "compliance-requirement"}}

	keysFor := func(a *artifacts.ArtifactsAnalyzer, control *pb.Control, jobID string) []string {
		tasks, err := a.GenerateWork(testspecs.SetupTenantContext("test-tenant"), control,
			analyzer.AnalyzerConfig{Parameters: map[string]string{"job_id": jobID}})
		Expect(err).NotTo(HaveOccurred())
		Expect(tasks).NotTo(BeEmpty(), "GenerateWork produced no task to key")
		var keys []string
		for _, t := range tasks {
			req, ok := a.CacheIdentity(t)
			Expect(ok).To(BeTrue())
			keys = append(keys, resultcache.Key("artifacts", req.Identity, resultcache.ModelIdentity{Observed: "observed"}))
		}
		return keys
	}
	keysIn := func(jobID string) []string { return keysFor(newAnalyzer(), control, jobID) }

	It("names every panel model as a completion model", func() {
		Expect(newAnalyzer().CacheModels()).To(Equal([]analyzer.ModelUse{{Alias: "model-a", Kind: analyzer.ModelCompletion}}))
	})

	It("gives N samples of one control N distinct keys", func() {
		keys := keysIn("job-1")
		Expect(keys).To(HaveLen(3))
		Expect(map[string]bool{keys[0]: true, keys[1]: true, keys[2]: true}).To(HaveLen(3))
	})

	It("gives the same control the same keys in two jobs", func() {
		Expect(keysIn("job-2")).To(Equal(keysIn("job-1")))
	})

	DescribeTable("changes every key when the request changes",
		func(a *artifacts.ArtifactsAnalyzer, c *pb.Control) {
			changed := keysFor(a, c, "job-1")
			Expect(changed).To(HaveLen(3), "one key per sample")
			for _, k := range keysFor(newAnalyzer(), control, "job-1") {
				Expect(changed).NotTo(ContainElement(k))
			}
		},
		Entry("the control text", newAnalyzer(), &pb.Control{Identifier: "AC-1", Statement: "A different statement.", Parts: map[string]string{"class": "compliance-requirement"}}),
		Entry("the token limit", newAnalyzer(func(c *config.ArtifactsConfig) { c.MaxTokens = 501 }), control),
		Entry("the temperature", newAnalyzer(func(c *config.ArtifactsConfig) { c.SamplingTemperature = 0.4 }), control),
	)

	It("adds keys for a second panel model without changing the first model's", func() {
		two := keysFor(newAnalyzer(func(c *config.ArtifactsConfig) { c.Models = []string{"model-a", "model-b"} }), control, "job-1")
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
		Entry("a response that says there are no artifacts", map[string]any{"model": "m", "response": "ARTIFACTS: NONE"}, Succeed()),
		Entry("a response with no parseable artifact block", map[string]any{"model": "m", "response": "Here are some thoughts."}, MatchError(ContainSubstring("does not parse"))),
		Entry("a result without a response", map[string]any{"model": "m"}, MatchError(ContainSubstring("no response text"))),
	)
})
