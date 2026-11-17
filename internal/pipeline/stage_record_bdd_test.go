package pipeline_test

import (
	"context"
	"errors"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/pipeline"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/analyzertest"
	"github.com/complytime-labs/crosscodex/pkg/cleaning"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/prompt"
	"github.com/complytime-labs/crosscodex/pkg/prompt/prompttest"
	"github.com/complytime-labs/crosscodex/pkg/storage/storagetest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// promptedPanelStub is a panel analyzer that names the prompt it renders.
type promptedPanelStub struct {
	*analyzertest.Stub[*pb.Control]
	panel  analyzer.Panel
	prompt string
}

func (s *promptedPanelStub) Panel() analyzer.Panel { return s.panel }
func (s *promptedPanelStub) PromptName() string    { return s.prompt }

var _ = Describe("Stage record", func() {
	var (
		ctx     context.Context
		reg     *analyzer.Registry
		prompts *prompttest.Fake
		// resolvedTenant is the tenant the prompt registry saw in the ctx
		// of its last Resolve call.
		resolvedTenant string
	)

	BeforeEach(func() {
		var err error
		ctx, err = tenant.WithTenant(context.Background(), "test-tenant")
		Expect(err).NotTo(HaveOccurred())
		reg = analyzer.NewRegistry()
		resolvedTenant = ""
		prompts = &prompttest.Fake{ResolveFunc: func(ctx context.Context, name string) (*prompt.PromptSpec, error) {
			resolvedTenant, _ = tenant.FromContext(ctx)
			return &prompt.PromptSpec{Name: name, Version: "4"}, nil
		}}
	})

	registerPanel := func(stage, promptName string, aliases ...string) {
		Expect(analyzer.Register[*pb.Control](reg, &promptedPanelStub{
			Stub:   &analyzertest.Stub[*pb.Control]{AnalyzerName: stage},
			panel:  analyzer.Panel{Aliases: aliases},
			prompt: promptName,
		})).To(Succeed())
	}
	service := func(opts ...pipeline.Option) *pipeline.Service {
		return pipeline.New(newFakeStore(), nil, reg, nil, pipeline.Attestors{}, nil, nil, &storagetest.Fake{},
			config.PipelineConfig{}, nil, opts...)
	}
	withRevisions := func(revisions map[string]string) pipeline.Option {
		return pipeline.WithResultCache(nil, config.ResultCacheConfig{}, config.LLMConfig{ModelRevisions: revisions}, nil)
	}
	output := func(metadata map[string]string) analyzer.Output { return analyzer.Output{Metadata: metadata} }

	Describe("stageRecordFor", func() {
		It("records the prompt and each panel alias's revision, empty when unset", func() {
			registerPanel("relationship", "relationship", "a", "b")
			svc := service(withRevisions(map[string]string{"a": "r1"}))

			got, err := svc.StageRecordFor("relationship", output(map[string]string{"prompt_name": "relationship", "prompt_version": "3"}))

			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(MatchJSON(`{"prompt_name":"relationship","prompt_version":"3","models":{"a":"r1","b":""}}`))
		})

		It("records an empty model set for a stage that declares no panel", func() {
			Expect(analyzer.Register[*pb.Control](reg, &analyzertest.Stub[*pb.Control]{AnalyzerName: "classify"})).To(Succeed())

			got, err := service().StageRecordFor("classify", output(map[string]string{"prompt_name": "classify", "prompt_version": "1"}))

			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(MatchJSON(`{"prompt_name":"classify","prompt_version":"1","models":{}}`))
		})

		It("records an unresolved prompt version as empty, as the analyzer reports it", func() {
			got, err := service().StageRecordFor("classify", output(map[string]string{"prompt_name": "classify", "prompt_version": ""}))

			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(MatchJSON(`{"prompt_name":"classify","prompt_version":"","models":{}}`))
		})

		DescribeTable("yields no record when the stage rendered no prompt",
			func(metadata map[string]string) {
				got, err := service().StageRecordFor("embedding", output(metadata))
				Expect(err).NotTo(HaveOccurred())
				Expect(got).To(BeEmpty())
			},
			Entry("no metadata", nil),
			Entry("metadata without prompt_name", map[string]string{"total_count": "3"}),
			Entry("a blank prompt_name", map[string]string{"prompt_name": ""}),
		)

		It("refuses a revision too long to store, naming the alias, rather than storing a record that cannot be read back", func() {
			registerPanel("relationship", "relationship", "a")
			svc := service(withRevisions(map[string]string{"a": strings.Repeat("r", 300)}))

			got, err := svc.StageRecordFor("relationship", output(map[string]string{"prompt_name": "relationship"}))

			Expect(err).To(MatchError(cleaning.ErrInvalidStageRecord))
			Expect(err.Error()).To(ContainSubstring(`"a"`))
			Expect(got).To(BeEmpty())
		})
	})

	Describe("CurrentStageRecord", func() {
		It("resolves the stage's prompt in the tenant's layers and pairs it with the panel's revisions", func() {
			registerPanel("relationship", "relationship", "a", "b")
			svc := service(pipeline.WithPromptRegistry(prompts), withRevisions(map[string]string{"b": "r2"}))

			rec, ok, err := svc.CurrentStageRecord(ctx, "test-tenant", "relationship")

			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeTrue())
			Expect(rec).To(Equal(cleaning.StageRecord{
				PromptName: "relationship", PromptVersion: "4", Models: map[string]string{"a": "", "b": "r2"},
			}))
			Expect(resolvedTenant).To(Equal("test-tenant"))
		})

		It("uses the tenant argument, not whatever tenant the ctx carries", func() {
			registerPanel("relationship", "relationship", "a")
			svc := service(pipeline.WithPromptRegistry(prompts))

			_, _, err := svc.CurrentStageRecord(ctx, "other-tenant", "relationship")

			Expect(err).NotTo(HaveOccurred())
			Expect(resolvedTenant).To(Equal("other-tenant"))
		})

		DescribeTable("reports false, without error, for a stage that has no prompt record",
			func(register func(), stage string) {
				register()
				_, ok, err := service(pipeline.WithPromptRegistry(prompts)).CurrentStageRecord(ctx, "test-tenant", stage)
				Expect(err).NotTo(HaveOccurred())
				Expect(ok).To(BeFalse())
			},
			Entry("an unregistered stage", func() {}, "relationship"),
			Entry("a registered analyzer that renders no prompt", func() {
				Expect(analyzer.Register[*pb.Control](reg, &analyzertest.Stub[*pb.Control]{AnalyzerName: "embedding"})).To(Succeed())
			}, "embedding"),
		)

		It("fails closed, naming the option, when no prompt registry is wired", func() {
			registerPanel("relationship", "relationship", "a")

			_, ok, err := service().CurrentStageRecord(ctx, "test-tenant", "relationship")

			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("WithPromptRegistry"))
			Expect(ok).To(BeFalse())
		})

		It("returns the prompt registry's error rather than guessing a version", func() {
			registerPanel("relationship", "relationship", "a")
			boom := errors.New("layer unreadable")
			prompts.ResolveFunc = func(context.Context, string) (*prompt.PromptSpec, error) { return nil, boom }

			_, ok, err := service(pipeline.WithPromptRegistry(prompts)).CurrentStageRecord(ctx, "test-tenant", "relationship")

			Expect(err).To(MatchError(boom))
			Expect(ok).To(BeFalse())
		})

		It("rejects an invalid tenant before resolving any prompt", func() {
			registerPanel("relationship", "relationship", "a")

			_, ok, err := service(pipeline.WithPromptRegistry(prompts)).CurrentStageRecord(ctx, "", "relationship")

			Expect(err).To(MatchError(tenant.ErrInvalidTenant))
			Expect(ok).To(BeFalse())
			Expect(resolvedTenant).To(BeEmpty())
		})
	})

	Describe("DBStageReporter", func() {
		var (
			store    *fakeStore
			reporter *pipeline.DBStageReporter
		)

		BeforeEach(func() {
			store = newFakeStore()
			reporter = pipeline.NewDBStageReporter(&fakeNATSReporter{}, store)
			Expect(store.CreateJob(ctx, &pipeline.Job{JobID: "job-1", TenantID: "test-tenant"})).To(Succeed())
			Expect(store.CreateStages(ctx, "job-1", []string{"relationship"})).To(Succeed())
		})

		stored := func() string {
			stages, err := store.GetStages(ctx, "job-1")
			Expect(err).NotTo(HaveOccurred())
			return stages[0].StageRecord
		}

		It("stores the record with the completion when wired to a service", func() {
			registerPanel("relationship", "relationship", "a")
			service(pipeline.WithStageReporter(reporter), withRevisions(map[string]string{"a": "r1"}))

			Expect(reporter.ReportStageCompleted(ctx, "relationship", "job-1", &analyzer.Output{
				Metadata: map[string]string{"prompt_name": "relationship", "prompt_version": "3"},
			})).To(Succeed())

			Expect(stored()).To(MatchJSON(`{"prompt_name":"relationship","prompt_version":"3","models":{"a":"r1"}}`))
		})

		It("stores no record for a stage that rendered no prompt", func() {
			service(pipeline.WithStageReporter(reporter))

			Expect(reporter.ReportStageCompleted(ctx, "relationship", "job-1", &analyzer.Output{})).To(Succeed())

			Expect(stored()).To(BeEmpty())
		})

		It("fails the stage and leaves it uncompleted when the record cannot be built", func() {
			registerPanel("relationship", "relationship", "a")
			service(pipeline.WithStageReporter(reporter), withRevisions(map[string]string{"a": strings.Repeat("r", 300)}))

			err := reporter.ReportStageCompleted(ctx, "relationship", "job-1", &analyzer.Output{
				Metadata: map[string]string{"prompt_name": "relationship"},
			})

			Expect(err).To(MatchError(cleaning.ErrInvalidStageRecord))
			stages, _ := store.GetStages(ctx, "job-1")
			Expect(stages[0].Status).NotTo(Equal(pipeline.StageStatusCompleted))
			Expect(stored()).To(BeEmpty())
		})
	})
})
