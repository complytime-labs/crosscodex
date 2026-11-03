package classify_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/types"
	"google.golang.org/protobuf/types/known/structpb"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/analyzer/classify"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/prompt"
	"github.com/complytime-labs/crosscodex/pkg/resultcache"
)

var _ = Describe("classify result reuse", func() {
	var a *classify.ClassifyAnalyzer
	var newAnalyzer func(mutate ...func(*config.ClassificationConfig)) *classify.ClassifyAnalyzer

	BeforeEach(func() {
		prompts, err := prompt.NewRegistry(config.PromptConfig{Layers: config.PromptLayerConfig{Enabled: true}})
		Expect(err).NotTo(HaveOccurred())
		newAnalyzer = func(mutate ...func(*config.ClassificationConfig)) *classify.ClassifyAnalyzer {
			cfg := config.ClassificationConfig{Enabled: true, MaxTextLength: 2000, MaxTokens: 20}
			for _, m := range mutate {
				m(&cfg)
			}
			return classify.New(nil, prompts, cfg)
		}
		a = newAnalyzer()
	})

	control := func() *pb.Control {
		return &pb.Control{ControlId: "nist/AC-1", Identifier: "AC-1", Statement: "Define access control policy.",
			Parts: map[string]string{"class": "compliance-requirement"}}
	}

	It("names the tenant-default alias as a completion model", func() {
		Expect(a.CacheModels()).To(Equal([]analyzer.ModelUse{{Alias: "", Kind: analyzer.ModelCompletion}}))
	})

	It("gives the same control the same key in two jobs", func() {
		keyIn := func(jobID string) string {
			tasks, err := a.GenerateWork(testspecs.SetupTenantContext("test-tenant"), control(),
				analyzer.AnalyzerConfig{Parameters: map[string]string{"job_id": jobID}})
			Expect(err).NotTo(HaveOccurred())
			Expect(tasks).To(HaveLen(1))
			req, ok := a.CacheIdentity(tasks[0])
			Expect(ok).To(BeTrue())
			Expect(req.SourceControlID).To(Equal("nist/AC-1"))
			Expect(req.TargetControlID).To(Equal("nist/AC-1"))
			return resultcache.Key("classify", req.Identity, resultcache.ModelIdentity{Observed: "qwen"})
		}
		Expect(keyIn("job-1")).To(Equal(keyIn("job-2")))
	})

	It("never caches a section's pre-built task", func() {
		section := &pb.Control{ControlId: "nist/AC", Identifier: "AC", Statement: "ACCESS CONTROL", Parts: map[string]string{"class": "compliance-section"}}
		tasks, err := a.GenerateWork(testspecs.SetupTenantContext("test-tenant"), section, analyzer.AnalyzerConfig{})
		Expect(err).NotTo(HaveOccurred())
		Expect(tasks).To(HaveLen(1))
		for _, t := range tasks {
			_, ok := a.CacheIdentity(t)
			Expect(ok).To(BeFalse())
		}
	})

	DescribeTable("changes the key when the request changes",
		func(mutate func(*config.ClassificationConfig), edit func(*pb.Control)) {
			keyOf := func(a *classify.ClassifyAnalyzer, c *pb.Control) string {
				tasks, err := a.GenerateWork(testspecs.SetupTenantContext("test-tenant"), c, analyzer.AnalyzerConfig{})
				Expect(err).NotTo(HaveOccurred())
				Expect(tasks).To(HaveLen(1))
				req, ok := a.CacheIdentity(tasks[0])
				Expect(ok).To(BeTrue())
				return resultcache.Key("classify", req.Identity, resultcache.ModelIdentity{Observed: "qwen"})
			}
			changed := control()
			edit(changed)
			Expect(keyOf(newAnalyzer(mutate), changed)).NotTo(Equal(keyOf(a, control())))
		},
		Entry("the control text", func(*config.ClassificationConfig) {}, func(c *pb.Control) { c.Statement = "A different statement." }),
		Entry("the token limit", func(c *config.ClassificationConfig) { c.MaxTokens = 21 }, func(*pb.Control) {}),
		Entry("the temperature", func(c *config.ClassificationConfig) { c.Temperature = 0.4 }, func(*pb.Control) {}),
		Entry("the model", func(c *config.ClassificationConfig) { c.Model = "other" }, func(*pb.Control) {}),
	)

	// A result Aggregate would discard must not be stored: every later job
	// would replay the same failure instead of asking the model again.
	DescribeTable("stores only a result Aggregate would use",
		func(fields map[string]any, want types.GomegaMatcher) {
			result, err := structpb.NewStruct(fields)
			Expect(err).NotTo(HaveOccurred())
			Expect(newAnalyzer().CacheableResult(result)).To(want)
		},
		Entry("a parsed classification", map[string]any{"model": "m", "response": "Technical|Operational"}, Succeed()),
		Entry("a response with no text", map[string]any{"model": "m", "response": "  .\n"}, MatchError(ContainSubstring("does not parse"))),
		Entry("a result without a response", map[string]any{"model": "m"}, MatchError(ContainSubstring("no response text"))),
	)
})
