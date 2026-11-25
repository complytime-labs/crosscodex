package pipeline_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/structpb"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/analyzer/critique"
	"github.com/complytime-labs/crosscodex/internal/analyzer/relationship"
	"github.com/complytime-labs/crosscodex/internal/analyzer/requires"
	"github.com/complytime-labs/crosscodex/internal/pipeline"
	pipelineattestation "github.com/complytime-labs/crosscodex/internal/pipeline/attestation"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/llmclient/llmclienttest"
	"github.com/complytime-labs/crosscodex/pkg/natsbus/natsbustest"
	"github.com/complytime-labs/crosscodex/pkg/prompt"
	"github.com/complytime-labs/crosscodex/pkg/storage/storagetest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// onePair is the one pair the pair analyzers' candidate providers serve.
type onePair struct{ source, target string }

type relationshipPairs struct{ onePair }

func (p relationshipPairs) Candidates(context.Context, string, string) ([]relationship.CandidatePair, error) {
	return []relationship.CandidatePair{{SourceControlID: p.source, TargetControlID: p.target}}, nil
}

type requiresPairs struct{ onePair }

func (p requiresPairs) Candidates(context.Context, string, string) ([]requires.RequiresPair, error) {
	return []requires.RequiresPair{{SourceControlID: p.source, TargetControlID: p.target}}, nil
}

// fixedVerdicts serves one proposed verdict.
type fixedVerdicts []critique.ProposedVerdict

func (f fixedVerdicts) Verdicts(context.Context, string, string) ([]critique.ProposedVerdict, error) {
	return f, nil
}

// sentMessages are the messages of a GenerateWork's first task, decoded as
// the worker decodes them before sending them to the model.
func sentMessages(tasks []analyzer.Task, err error) []*pb.PromptMessage {
	Expect(err).NotTo(HaveOccurred())
	Expect(tasks).NotTo(BeEmpty())
	sent, err := llmclient.MessagesFromPayload(tasks[0].Payload.(*structpb.Struct).Fields["messages"])
	Expect(err).NotTo(HaveOccurred())
	out := make([]*pb.PromptMessage, len(sent))
	for i, m := range sent {
		out[i] = &pb.PromptMessage{Role: m.Role, Content: m.Content}
	}
	return out
}

// noStatementControls serves one control without a statement.
type noStatementControls struct{}

func (noStatementControls) Controls(_ context.Context, _, catalogID string) ([]*pb.Control, error) {
	return []*pb.Control{{ControlId: catalogID + "/blank", CatalogId: catalogID, Title: "Blank"}}, nil
}

var _ = Describe("RenderPrompt", func() {
	const (
		ownTenant, otherTenant = "test-tenant", "other-tenant"
		r3Control              = "r3-cat/SR-03.01.01.c.01"
		ssdfControl            = "ssdf-cat/PO.1.1"
		recordedR3             = "NIST SP 800-171 Rev 3 (as recorded)"
	)

	var (
		ctx       context.Context
		store     *fakeStore
		llm       *llmclienttest.Fake
		prompts   prompt.Registry
		analysis  config.AnalysisConfig
		catalogs  pipeline.CatalogControlsReader
		catalogNm catalogNamesByTenant
	)

	BeforeEach(func() {
		var err error
		ctx, err = tenant.WithTenant(context.Background(), ownTenant)
		Expect(err).NotTo(HaveOccurred())
		prompts, err = prompt.NewRegistry(config.PromptConfig{Layers: config.PromptLayerConfig{Enabled: true}})
		Expect(err).NotTo(HaveOccurred())

		// Fails the spec if an analyzer ever calls the model.
		llm = &llmclienttest.Fake{
			CompleteFunc: func(context.Context, *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
				Fail("RenderPrompt called the LLM's Complete")
				return nil, nil
			},
			EmbedFunc: func(context.Context, *llmclient.EmbeddingRequest) (*llmclient.EmbeddingResponse, error) {
				Fail("RenderPrompt called the LLM's Embed")
				return nil, nil
			},
		}
		DeferCleanup(func() { Expect(llm.CompleteRequests()).To(BeEmpty()) })

		analysis = config.AnalysisConfig{
			Relationship: config.RelationshipConfig{Enabled: true, Models: []string{"m1"}, MaxSourceChars: 1500, MaxTargetChars: 40,
				MaxTokens: 300, SamplesPerModel: 1},
			Requires: config.RequiresConfig{Enabled: true, Models: []string{"m1"}, MaxSourceChars: 1500, MaxTargetChars: 800,
				MaxTokens: 300, SamplesPerModel: 1},
			Critique: config.CritiqueConfig{Enabled: true, Models: []string{"c1"}, MaxSourceChars: 1500, MaxTargetChars: 800,
				MaxTokens: 300, SamplesPerModel: 1},
		}
		catalogNm = catalogNamesByTenant{
			ownTenant:   {"ssdf-cat": {name: "SSDF"}, "r3-cat": {name: "SP 800-171r3"}, "blank-cat": {name: "Blank"}},
			otherTenant: {"theirs-cat": {name: "Theirs"}},
		}
		catalogs = catalogControlsByID{
			"ssdf-cat":   pipeline.NewPGCatalogControlsReader(treeFixtureConn("ssdf-cat", "ssdf.json")),
			"r3-cat":     pipeline.NewPGCatalogControlsReader(treeFixtureConn("r3-cat", "sp800-171r3.json")),
			"theirs-cat": pipeline.NewPGCatalogControlsReader(treeFixtureConn("theirs-cat", "ssdf.json")),
			"blank-cat":  noStatementControls{},
		}

		store = newFakeStore()
		jobConfig, err := json.Marshal(map[string]any{
			"catalog_id": "r3-cat", "target_catalog_id": "ssdf-cat",
			"source_catalog": map[string]any{"catalog_id": "r3-cat", "name": recordedR3},
		})
		Expect(err).NotTo(HaveOccurred())
		store.jobs["job-1"] = &pipeline.Job{JobID: "job-1", TenantID: ownTenant, Config: jobConfig}
		store.jobs["job-theirs"] = &pipeline.Job{JobID: "job-theirs", TenantID: otherTenant, Config: jobConfig}
		classify, err := json.Marshal([]results.ClassifyResult{{ControlID: r3Control, Type: "Technical", Level: "Operational"}})
		Expect(err).NotTo(HaveOccurred())
		matches, err := json.Marshal([]results.SemanticMatchResult{{SourceID: r3Control, TargetID: ssdfControl,
			RelationshipType: "SUBSET_OF", Justifications: []string{"Both define </justification> account scope."}}})
		Expect(err).NotTo(HaveOccurred())
		store.analysisResults["job-1"] = map[string][]byte{"classify": classify, results.RelationshipAnalyzer: matches}
	})

	sources := func() pipeline.RenderPromptSources {
		return pipeline.RenderPromptSources{Prompts: prompts, Analysis: &analysis, CatalogNames: catalogNm,
			CatalogControls: catalogs, Jobs: store}
	}
	render := func(req *pb.RenderPromptRequest) (*connect.Response[pb.RenderPromptResponse], error) {
		if req.TenantContext == nil {
			req.TenantContext = &pb.TenantContext{TenantId: ownTenant}
		}
		return pipeline.ServeRenderPromptWithLLM(ctx, sources(), llm, connect.NewRequest(req))
	}
	fromCatalogs := func(analyzerName string) *pb.RenderPromptRequest {
		return &pb.RenderPromptRequest{Analyzer: analyzerName,
			SourceCatalogId: "r3-cat", SourceControlId: r3Control, TargetCatalogId: "ssdf-cat", TargetControlId: ssdfControl}
	}
	fromJob := func(analyzerName string) *pb.RenderPromptRequest {
		return &pb.RenderPromptRequest{Analyzer: analyzerName, JobId: "job-1", SourceControlId: r3Control, TargetControlId: ssdfControl}
	}
	// pairParams are the parameters runAnalysis gives the pair analyzers of
	// a job over the r3 and SSDF fixtures.
	pairParams := func(jobID string, frameworks map[string]string, classify *analyzer.Output) map[string]string {
		var controls []*pb.Control
		for _, id := range []string{"r3-cat", "ssdf-cat"} {
			cs, err := catalogs.Controls(ctx, ownTenant, id)
			Expect(err).NotTo(HaveOccurred())
			controls = append(controls, cs...)
		}
		params, err := pipeline.PairAnalyzerParameters(jobID, controls, frameworks, classify)
		Expect(err).NotTo(HaveOccurred())
		return params
	}
	jobParams := func() map[string]string {
		return pairParams("job-1", map[string]string{"r3-cat": recordedR3, "ssdf-cat": "SSDF"},
			&analyzer.Output{AnalyzerName: "classify", ResultData: store.analysisResults["job-1"]["classify"]})
	}
	catalogParams := func() map[string]string {
		return pairParams("any-job", map[string]string{"r3-cat": "SP 800-171r3", "ssdf-cat": "SSDF"}, nil)
	}

	Describe("hardening parity: the messages are the ones the analyzer's GenerateWork sends", func() {
		pair := onePair{source: r3Control, target: ssdfControl}

		It("renders the relationship prompt for a pair of two catalogs", func() {
			resp, err := render(fromCatalogs("relationship"))
			Expect(err).NotTo(HaveOccurred())

			want := sentMessages(relationship.New(nil, prompts, relationshipPairs{pair}, analysis.Relationship).
				GenerateWork(ctx, &pb.Control{}, analyzer.AnalyzerConfig{Parameters: catalogParams()}))
			Expect(resp.Msg.GetMessages()).To(HaveLen(2))
			for i := range want {
				Expect(resp.Msg.GetMessages()[i].GetRole()).To(Equal(want[i].GetRole()))
				Expect(resp.Msg.GetMessages()[i].GetContent()).To(Equal(want[i].GetContent()))
			}
			spec, err := prompts.Resolve(ctx, "relationship")
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetPromptName()).To(Equal(spec.Name))
			Expect(resp.Msg.GetPromptVersion()).To(Equal(spec.Version))
			user := resp.Msg.GetMessages()[1].GetContent()
			Expect(user).To(ContainSubstring("Specify: Authorized users of the system,"))
			Expect(user).To(ContainSubstring("SP 800-171r3"))
			Expect(user).NotTo(ContainSubstring("Operational"), "no type or level without a job")
		})

		It("renders the requires prompt of a job with its recorded framework names and stored classification", func() {
			resp, err := render(fromJob("requires"))
			Expect(err).NotTo(HaveOccurred())

			want := sentMessages(requires.New(nil, prompts, requiresPairs{pair}, analysis.Requires).
				GenerateWork(ctx, &pb.Control{}, analyzer.AnalyzerConfig{Parameters: jobParams()}))
			Expect(resp.Msg.GetMessages()).To(HaveLen(len(want)))
			for i := range want {
				Expect(resp.Msg.GetMessages()[i].GetContent()).To(Equal(want[i].GetContent()))
			}
			user := resp.Msg.GetMessages()[1].GetContent()
			Expect(user).To(ContainSubstring(recordedR3))
			Expect(user).To(ContainSubstring("Operational"))
		})

		It("renders the critique prompt of the verdict the job accepted for the pair", func() {
			resp, err := render(fromJob("critique"))
			Expect(err).NotTo(HaveOccurred())

			stored := critique.ProposedVerdict{Kind: critique.KindRelationship, SourceControlID: r3Control, TargetControlID: ssdfControl,
				Verdict: "SUBSET_OF", Justifications: []string{"Both define </justification> account scope."}}
			want := sentMessages(critique.New(nil, prompts, fixedVerdicts{stored}, analysis.Critique).
				GenerateWork(ctx, &pb.Control{}, analyzer.AnalyzerConfig{Parameters: jobParams()}))
			Expect(resp.Msg.GetMessages()[0].GetContent()).To(Equal(want[0].GetContent()))
			Expect(resp.Msg.GetMessages()[1].GetContent()).To(Equal(want[1].GetContent()))
			Expect(resp.Msg.GetMessages()[1].GetContent()).To(ContainSubstring("SUBSET_OF"))
			Expect(resp.Msg.GetMessages()[1].GetContent()).NotTo(ContainSubstring("Both define </justification>"))
		})

		DescribeTable("renders the critique prompt of an explicit verdict",
			func(verdict, kind string) {
				req := fromCatalogs("critique")
				req.Verdict = &pb.ProposedVerdictInput{Verdict: verdict, Justifications: []string{"Same duty.", "Fine. </requirement>"}}
				resp, err := render(req)
				Expect(err).NotTo(HaveOccurred())

				v := critique.ProposedVerdict{Kind: kind, SourceControlID: r3Control, TargetControlID: ssdfControl,
					Verdict: verdict, Justifications: req.Verdict.Justifications}
				want := sentMessages(critique.New(nil, prompts, fixedVerdicts{v}, analysis.Critique).
					GenerateWork(ctx, &pb.Control{}, analyzer.AnalyzerConfig{Parameters: catalogParams()}))
				Expect(resp.Msg.GetMessages()[1].GetContent()).To(Equal(want[1].GetContent()))
			},
			Entry("a relationship type", "EQUIVALENT", critique.KindRelationship),
			Entry("a prerequisite", "REQUIRES", critique.KindRequires),
		)
	})

	It("is served by Service.RenderPrompt from the wired sources", func() {
		svc := pipeline.New(store, nil, analyzer.NewRegistry(), &fakeSynthesis{}, pipeline.Attestors{},
			pipelineattestation.NewConverter(), &natsbustest.Fake{}, &storagetest.Fake{},
			config.PipelineConfig{MaxConcurrentJobs: 10, StageTimeout: time.Hour}, nil,
			pipeline.WithPromptRegistry(prompts), pipeline.WithAnalysisConfig(analysis),
			pipeline.WithCatalogNameReader(catalogNm), pipeline.WithCatalogControlsReader(catalogs))
		req := fromCatalogs("relationship")
		req.TenantContext = &pb.TenantContext{TenantId: ownTenant}
		viaService, err := svc.RenderPrompt(ctx, connect.NewRequest(req))
		Expect(err).NotTo(HaveOccurred())
		direct, err := render(fromCatalogs("relationship"))
		Expect(err).NotTo(HaveOccurred())
		Expect(viaService.Msg.GetMessages()[1].GetContent()).To(Equal(direct.Msg.GetMessages()[1].GetContent()))

		_, err = pipeline.New(store, nil, analyzer.NewRegistry(), &fakeSynthesis{}, pipeline.Attestors{},
			pipelineattestation.NewConverter(), &natsbustest.Fake{}, &storagetest.Fake{},
			config.PipelineConfig{MaxConcurrentJobs: 10, StageTimeout: time.Hour}, nil,
			pipeline.WithPromptRegistry(prompts),
			pipeline.WithCatalogNameReader(catalogNm), pipeline.WithCatalogControlsReader(catalogs)).
			RenderPrompt(ctx, connect.NewRequest(req))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnavailable))
		Expect(err.Error()).To(ContainSubstring("pipeline.WithAnalysisConfig"))
	})

	Describe("one NotFound for everything not the tenant's", func() {
		It("answers another tenant's control, an unknown control, an unknown job and another tenant's job alike", func() {
			theirs := fromCatalogs("relationship")
			theirs.SourceCatalogId, theirs.SourceControlId = "theirs-cat", "theirs-cat/PO.1.1"
			unknownControl := fromCatalogs("relationship")
			unknownControl.SourceControlId = "r3-cat/NOPE"
			unknownCatalog := fromCatalogs("relationship")
			unknownCatalog.TargetCatalogId = "nope-cat"
			unknownJob := fromJob("critique")
			unknownJob.JobId = "job-nope"
			theirJob := fromJob("critique")
			theirJob.JobId = "job-theirs"
			wrongEnd := fromJob("relationship")
			wrongEnd.SourceControlId = ssdfControl // in the job, but not in its source catalog

			var errs []error
			for _, req := range []*pb.RenderPromptRequest{theirs, unknownControl, unknownCatalog, unknownJob, theirJob, wrongEnd} {
				resp, err := render(req)
				Expect(resp).To(BeNil())
				errs = append(errs, err)
			}
			for _, err := range errs {
				Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
				Expect(err.Error()).To(Equal(errs[0].Error()))
			}
			Expect(errs[0].Error()).To(ContainSubstring("crosscodex catalog controls"))
			Expect(errs[0].Error()).NotTo(ContainSubstring("theirs"))
			Expect(errs[0].Error()).NotTo(ContainSubstring(otherTenant))
		})
	})

	Describe("tenant context", func() {
		It("rejects a request without tenant_context as InvalidArgument", func() {
			_, err := pipeline.ServeRenderPromptWithLLM(ctx, sources(), llm, connect.NewRequest(fromCatalogs("relationship")))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(err.Error()).To(ContainSubstring("missing tenant context"))
		})

		It("denies a tenant_context naming another tenant without naming either", func() {
			req := fromCatalogs("relationship")
			req.TenantContext = &pb.TenantContext{TenantId: otherTenant}
			_, err := render(req)
			Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied))
			Expect(err.Error()).NotTo(ContainSubstring(otherTenant))
		})
	})

	Describe("refused requests", func() {
		DescribeTable("refuses as InvalidArgument with what to do instead",
			func(mutate func(*pb.RenderPromptRequest), want string) {
				req := fromCatalogs("critique")
				req.Verdict = &pb.ProposedVerdictInput{Verdict: "EQUIVALENT"}
				mutate(req)
				resp, err := render(req)
				Expect(resp).To(BeNil())
				Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
				Expect(err.Error()).To(ContainSubstring(want))
			},
			Entry("critique with both a job and a verdict", func(r *pb.RenderPromptRequest) { r.JobId = "job-1" }, "give either --job <job-id>"),
			Entry("critique with neither a job nor a verdict", func(r *pb.RenderPromptRequest) { r.Verdict = nil }, "or --verdict <verdict>"),
			Entry("a verdict for another analyzer", func(r *pb.RenderPromptRequest) { r.Analyzer = "relationship" },
				"applies only to the critique analyzer"),
			Entry("an unknown analyzer", func(r *pb.RenderPromptRequest) { r.Analyzer = "classify" }, "relationship, requires, critique"),
			Entry("a verdict critique does not review", func(r *pb.RenderPromptRequest) { r.Verdict.Verdict = "NO_RELATIONSHIP" },
				"give REQUIRES for a prerequisite"),
			Entry("too many justifications", func(r *pb.RenderPromptRequest) {
				r.Verdict.Justifications = make([]string, 65)
			}, "Give at most 64"),
			Entry("an over-size justification", func(r *pb.RenderPromptRequest) {
				r.Verdict.Justifications = []string{"ok", strings.Repeat("x", 2001)}
			}, "justification 2 is 2001 bytes"),
			Entry("an over-size control ID", func(r *pb.RenderPromptRequest) { r.TargetControlId = strings.Repeat("a", 257) },
				"target_control_id is 257 bytes"),
			Entry("no source control", func(r *pb.RenderPromptRequest) { r.SourceControlId = "" }, "source_control_id is required"),
			Entry("no target catalog without a job", func(r *pb.RenderPromptRequest) { r.TargetCatalogId = "" },
				"target_catalog_id is required without job_id"),
		)

		It("refuses a catalog that is not one of the job's", func() {
			req := fromJob("relationship")
			req.TargetCatalogId = "blank-cat"
			_, err := render(req)
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(err.Error()).To(ContainSubstring("r3-cat and ssdf-cat"))
		})

		It("refuses a control without a statement as FailedPrecondition", func() {
			req := fromCatalogs("relationship")
			req.TargetCatalogId, req.TargetControlId = "blank-cat", "blank-cat/blank"
			_, err := render(req)
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
			Expect(err.Error()).To(ContainSubstring("give the control a statement in the catalog and re-import it"))
		})

		It("refuses critique of a pair the job accepted no verdict for", func() {
			req := fromJob("critique")
			req.TargetControlId = "ssdf-cat/PO.1"
			_, err := render(req)
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
			Expect(err.Error()).To(ContainSubstring("accepted no relationship or requires verdict"))
		})

		It("refuses critique of a pair the job accepted two verdicts for, naming both ways to render them", func() {
			prereqs, err := json.Marshal([]results.RequiresResult{{SourceID: r3Control, TargetID: ssdfControl}})
			Expect(err).NotTo(HaveOccurred())
			store.analysisResults["job-1"][results.RequiresAnalyzer] = prereqs
			_, err = render(fromJob("critique"))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
			Expect(err.Error()).To(ContainSubstring("--verdict SUBSET_OF or --verdict REQUIRES"))
		})

		It("refuses critique of a pair whose stored relationship verdict is listed twice, as the critique stage does", func() {
			twice, err := json.Marshal([]results.SemanticMatchResult{
				{SourceID: r3Control, TargetID: ssdfControl, RelationshipType: "SUBSET_OF"},
				{SourceID: r3Control, TargetID: ssdfControl, RelationshipType: "EQUIVALENT"},
			})
			Expect(err).NotTo(HaveOccurred())
			store.analysisResults["job-1"][results.RelationshipAnalyzer] = twice
			_, err = render(fromJob("critique"))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
			Expect(err.Error()).To(ContainSubstring("stored results are corrupt"))
		})

		It("withholds a store error from the caller", func() {
			store.getJobErr = errors.New("dial tcp 10.0.3.7:5432: secret_admin")
			_, err := render(fromJob("relationship"))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnavailable))
			Expect(err.Error()).NotTo(ContainSubstring("secret_admin"))
			Expect(err.Error()).To(ContainSubstring("Retry the request"))
		})
	})
})
