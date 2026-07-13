package relationship_test

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/analyzer/relationship"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/prompt"
	"google.golang.org/protobuf/types/known/structpb"
)

// mockCandidateProvider returns pre-configured candidate pairs.
type mockCandidateProvider struct {
	pairs []relationship.CandidatePair
	err   error
}

func (m *mockCandidateProvider) Candidates(_ context.Context, _, _ string) ([]relationship.CandidatePair, error) {
	return m.pairs, m.err
}

// mockPromptRegistry returns a pre-configured prompt spec.
type mockPromptRegistry struct {
	spec *prompt.PromptSpec
	err  error
}

func (m *mockPromptRegistry) Resolve(_ context.Context, _ string) (*prompt.PromptSpec, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.spec, nil
}

func (m *mockPromptRegistry) Render(_ context.Context, _ string, _ map[string]string) (*prompt.ResolvedPrompt, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockPromptRegistry) List(_ context.Context) ([]string, error) {
	return []string{"relationship"}, nil
}

func (m *mockPromptRegistry) Layers(_ context.Context, _ string) ([]prompt.LayerInfo, error) {
	return nil, nil
}

var _ = Describe("RelationshipAnalyzer", func() {
	var (
		a          *relationship.RelationshipAnalyzer
		ctx        context.Context
		cfg        config.RelationshipConfig
		candidates *mockCandidateProvider
		prompts    *mockPromptRegistry
	)

	defaultSpec := &prompt.PromptSpec{
		Name:    "relationship",
		Version: "1.0.0",
		Templates: prompt.TemplateSet{
			System: "System: ${classification_guidance} ${relationship_definitions}",
			User:   "User: ${source_text} ${target_text} ${source_id} ${target_id} ${source_framework} ${target_framework} ${source_type} ${source_level} ${source_ancestor} ${target_type} ${target_level} ${target_ancestor} ${few_shot_examples}",
		},
	}

	BeforeEach(func() {
		ctx = testspecs.SetupTenantContext("test-tenant")
		cfg = config.RelationshipConfig{
			Enabled:             true,
			Models:              []string{"qwen3:8b"},
			TopK:                20,
			MaxSourceChars:      1500,
			MaxTargetChars:      800,
			MaxTokens:           300,
			SamplesPerModel:     1,
			SamplingTemperature: 0.3,
			ActionableTypes:     []string{"EQUIVALENT", "SUPERSET_OF"},
		}
		candidates = &mockCandidateProvider{}
		prompts = &mockPromptRegistry{spec: defaultSpec}
		a = relationship.New(nil, prompts, candidates, cfg)
	})

	It("returns 'relationship' from Name()", func() {
		Expect(a.Name()).To(Equal("relationship"))
	})

	It("depends on 'embedding'", func() {
		Expect(a.DependsOn()).To(Equal([]string{"embedding"}))
	})

	It("returns *pb.AnalysisResult from ResultSchema()", func() {
		schema := a.ResultSchema()
		Expect(schema).To(BeAssignableToTypeOf(&pb.AnalysisResult{}))
	})

	Context("GenerateWork", func() {
		It("produces 0 tasks for 0 candidates", func() {
			candidates.pairs = nil
			control := &pb.Control{ControlId: "AC-1", Statement: "test"}
			tasks, err := a.GenerateWork(ctx, control, analyzer.AnalyzerConfig{
				Parameters: map[string]string{"job_id": "job-1"},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(tasks).To(BeEmpty())
		})

		It("rejects empty job_id", func() {
			control := &pb.Control{ControlId: "AC-1", Statement: "test"}
			_, err := a.GenerateWork(ctx, control, analyzer.AnalyzerConfig{})
			Expect(err).To(MatchError(ContainSubstring("job_id is required")))
		})

		It("produces N*M*S tasks for N candidates, M models, S samples", func() {
			candidates.pairs = []relationship.CandidatePair{
				{SourceControlID: "AC-1", TargetControlID: "IT-3.2", SimilarityScore: 87.3},
				{SourceControlID: "AC-1", TargetControlID: "IT-4.1", SimilarityScore: 72.1},
			}
			cfg.Models = []string{"qwen3:8b", "mistral:7b"}
			cfg.SamplesPerModel = 2
			a = relationship.New(nil, prompts, candidates, cfg)

			control := &pb.Control{ControlId: "AC-1", Statement: "access control policy"}
			tasks, err := a.GenerateWork(ctx, control, analyzer.AnalyzerConfig{
				Parameters: map[string]string{"job_id": "job-1"},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(tasks).To(HaveLen(2 * 2 * 2)) // 2 pairs * 2 models * 2 samples
		})

		It("generates task IDs in relationship-{src}--{tgt}-{model}-s{idx} format", func() {
			candidates.pairs = []relationship.CandidatePair{
				{SourceControlID: "AC-1", TargetControlID: "IT-3.2", SimilarityScore: 87.3},
			}
			a = relationship.New(nil, prompts, candidates, cfg)

			control := &pb.Control{ControlId: "AC-1", Statement: "test"}
			tasks, err := a.GenerateWork(ctx, control, analyzer.AnalyzerConfig{
				Parameters: map[string]string{"job_id": "job-1"},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(tasks).To(HaveLen(1))
			Expect(tasks[0].TaskID).To(Equal("relationship-AC-1--IT-3.2-qwen3:8b-s0"))
		})

		It("forces temperature to 0.0 when SamplesPerModel == 1", func() {
			candidates.pairs = []relationship.CandidatePair{
				{SourceControlID: "AC-1", TargetControlID: "IT-3.2", SimilarityScore: 87.3},
			}
			cfg.SamplesPerModel = 1
			cfg.SamplingTemperature = 0.7
			a = relationship.New(nil, prompts, candidates, cfg)

			control := &pb.Control{ControlId: "AC-1", Statement: "test"}
			tasks, err := a.GenerateWork(ctx, control, analyzer.AnalyzerConfig{
				Parameters: map[string]string{"job_id": "job-1"},
			})
			Expect(err).NotTo(HaveOccurred())
			payload, ok := tasks[0].Payload.(*structpb.Struct)
			Expect(ok).To(BeTrue())
			Expect(payload.Fields["temperature"].GetNumberValue()).To(Equal(0.0))
			Expect(payload.Fields).To(HaveKey("messages"))
			msgsList := payload.Fields["messages"].GetListValue()
			Expect(msgsList).NotTo(BeNil())
			Expect(msgsList.Values).To(HaveLen(2))
			Expect(msgsList.Values[0].GetStructValue().Fields["role"].GetStringValue()).To(Equal("system"))
			Expect(msgsList.Values[1].GetStructValue().Fields["role"].GetStringValue()).To(Equal("user"))
		})

		It("uses SamplingTemperature when SamplesPerModel > 1", func() {
			candidates.pairs = []relationship.CandidatePair{
				{SourceControlID: "AC-1", TargetControlID: "IT-3.2", SimilarityScore: 87.3},
			}
			cfg.SamplesPerModel = 3
			cfg.SamplingTemperature = 0.7
			a = relationship.New(nil, prompts, candidates, cfg)

			control := &pb.Control{ControlId: "AC-1", Statement: "test"}
			tasks, err := a.GenerateWork(ctx, control, analyzer.AnalyzerConfig{
				Parameters: map[string]string{"job_id": "job-1"},
			})
			Expect(err).NotTo(HaveOccurred())
			payload, ok := tasks[0].Payload.(*structpb.Struct)
			Expect(ok).To(BeTrue())
			Expect(payload.Fields["temperature"].GetNumberValue()).To(Equal(0.7))
			Expect(payload.Fields).To(HaveKey("messages"))
			msgsList := payload.Fields["messages"].GetListValue()
			Expect(msgsList).NotTo(BeNil())
			Expect(msgsList.Values).To(HaveLen(2))
			Expect(msgsList.Values[0].GetStructValue().Fields["role"].GetStringValue()).To(Equal("system"))
			Expect(msgsList.Values[1].GetStructValue().Fields["role"].GetStringValue()).To(Equal("user"))
		})
	})

	Context("Aggregate", func() {
		It("counts pairs and produces output metadata", func() {
			results := []analyzer.TaskResult{
				{TaskID: "relationship-AC-1--IT-3.2-qwen3:8b-s0", TaskType: "relationship", Result: &pb.AnalysisResult{
					ResultId: "relationship-AC-1--IT-3.2-qwen3:8b-s0", ResultType: "relationship",
					Attributes: map[string]string{
						"source_control_id": "AC-1", "target_control_id": "IT-3.2",
						"relationship": "SUPERSET_OF", "confidence": "HIGH",
						"parse_status": "OK",
					},
					Confidence: 0.9,
				}},
			}
			output, err := a.Aggregate(ctx, results)
			Expect(err).NotTo(HaveOccurred())
			Expect(output.AnalyzerName).To(Equal("relationship"))
			Expect(output.Metadata).To(HaveKey("total_count"))
			Expect(output.Metadata).To(HaveKey("prompt_name"))
		})
	})
})
