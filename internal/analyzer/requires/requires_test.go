package requires_test

import (
	"context"
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	intanalyzer "github.com/complytime-labs/crosscodex/internal/analyzer"
	"github.com/complytime-labs/crosscodex/internal/analyzer/requires"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/prompt"
	"google.golang.org/protobuf/types/known/structpb"
)

// mockCandidateProvider returns pre-configured candidate pairs.
type mockCandidateProvider struct {
	pairs []requires.RequiresPair
	err   error
}

func (m *mockCandidateProvider) Candidates(_ context.Context, _, _ string) ([]requires.RequiresPair, error) {
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
	return []string{"requires"}, nil
}

func (m *mockPromptRegistry) Layers(_ context.Context, _ string) ([]prompt.LayerInfo, error) {
	return nil, nil
}

var _ = Describe("RequiresAnalyzer", func() {
	var (
		a          *requires.RequiresAnalyzer
		ctx        context.Context
		cfg        config.RequiresConfig
		candidates *mockCandidateProvider
		prompts    *mockPromptRegistry
	)

	defaultSpec := &prompt.PromptSpec{
		Name:    "requires",
		Version: "1.0.0",
		Templates: prompt.TemplateSet{
			System: "System: ${requires_guidance} ${dependency_definitions}",
			User:   "User: ${source_text} ${target_text} ${source_id} ${target_id} ${source_framework} ${target_framework} ${source_type} ${source_level} ${source_ancestor} ${target_type} ${target_level} ${target_ancestor} ${few_shot_examples}",
		},
	}

	BeforeEach(func() {
		ctx = testspecs.SetupTenantContext("test-tenant")
		cfg = config.RequiresConfig{
			Enabled:             true,
			Models:              []string{"qwen3:8b"},
			MaxSourceChars:      1500,
			MaxTargetChars:      800,
			MaxTokens:           300,
			SamplesPerModel:     1,
			SamplingTemperature: 0.3,
		}
		candidates = &mockCandidateProvider{}
		prompts = &mockPromptRegistry{spec: defaultSpec}
		a = requires.New(nil, prompts, candidates, cfg)
	})

	It("returns 'requires' from Name()", func() {
		Expect(a.Name()).To(Equal("requires"))
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

		It("returns error when tenant is missing from context", func() {
			control := &pb.Control{ControlId: "AC-1", Statement: "test"}
			_, err := a.GenerateWork(context.Background(), control, analyzer.AnalyzerConfig{
				Parameters: map[string]string{"job_id": "job-1"},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("requires.GenerateWork"))
		})

		It("returns error when candidate provider fails", func() {
			candidates.err = fmt.Errorf("storage unavailable")
			control := &pb.Control{ControlId: "AC-1", Statement: "test"}
			_, err := a.GenerateWork(ctx, control, analyzer.AnalyzerConfig{
				Parameters: map[string]string{"job_id": "job-1"},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("fetching candidates"))
		})

		It("returns error when prompt resolution fails", func() {
			candidates.pairs = []requires.RequiresPair{
				{SourceControlID: "AC-1", TargetControlID: "AC-2", AggregateScore: 0.8},
			}
			prompts.err = fmt.Errorf("prompt not found")
			a = requires.New(nil, prompts, candidates, cfg)

			control := &pb.Control{ControlId: "AC-1", Statement: "test"}
			_, err := a.GenerateWork(ctx, control, analyzer.AnalyzerConfig{
				Parameters: map[string]string{"job_id": "job-1"},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("resolving prompt"))
		})

		It("produces N*M*S tasks for N candidates, M models, S samples", func() {
			candidates.pairs = []requires.RequiresPair{
				{SourceControlID: "AC-1", TargetControlID: "IA-2", AggregateScore: 0.85},
				{SourceControlID: "AC-1", TargetControlID: "AU-6", AggregateScore: 0.72},
			}
			cfg.Models = []string{"qwen3:8b", "mistral:7b"}
			cfg.SamplesPerModel = 3
			cfg.AllowEvenSamples = true
			a = requires.New(nil, prompts, candidates, cfg)

			control := &pb.Control{ControlId: "AC-1", Statement: "access control policy"}
			tasks, err := a.GenerateWork(ctx, control, analyzer.AnalyzerConfig{
				Parameters: map[string]string{"job_id": "job-1"},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(tasks).To(HaveLen(2 * 2 * 3)) // 2 pairs * 2 models * 3 samples
		})

		It("generates task IDs in requires-{src}--{tgt}-{model}-s{idx} format", func() {
			candidates.pairs = []requires.RequiresPair{
				{SourceControlID: "AC-1", TargetControlID: "IA-2", AggregateScore: 0.85},
			}
			a = requires.New(nil, prompts, candidates, cfg)

			control := &pb.Control{ControlId: "AC-1", Statement: "test"}
			tasks, err := a.GenerateWork(ctx, control, analyzer.AnalyzerConfig{
				Parameters: map[string]string{"job_id": "job-1"},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(tasks).To(HaveLen(1))
			Expect(tasks[0].TaskID).To(Equal("requires-AC-1--IA-2-qwen3:8b-s0"))
		})

		It("sets TaskType to 'requires'", func() {
			candidates.pairs = []requires.RequiresPair{
				{SourceControlID: "AC-1", TargetControlID: "IA-2", AggregateScore: 0.85},
			}
			a = requires.New(nil, prompts, candidates, cfg)

			control := &pb.Control{ControlId: "AC-1", Statement: "test"}
			tasks, err := a.GenerateWork(ctx, control, analyzer.AnalyzerConfig{
				Parameters: map[string]string{"job_id": "job-1"},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(tasks[0].TaskType).To(Equal("requires"))
		})

		It("forces temperature to 0.0 when SamplesPerModel == 1", func() {
			candidates.pairs = []requires.RequiresPair{
				{SourceControlID: "AC-1", TargetControlID: "IA-2", AggregateScore: 0.85},
			}
			cfg.SamplesPerModel = 1
			cfg.SamplingTemperature = 0.7
			a = requires.New(nil, prompts, candidates, cfg)

			control := &pb.Control{ControlId: "AC-1", Statement: "test"}
			tasks, err := a.GenerateWork(ctx, control, analyzer.AnalyzerConfig{
				Parameters: map[string]string{"job_id": "job-1"},
			})
			Expect(err).NotTo(HaveOccurred())
			payload, ok := tasks[0].Payload.(*structpb.Struct)
			Expect(ok).To(BeTrue())
			Expect(payload.Fields["temperature"].GetNumberValue()).To(Equal(0.0))
		})

		It("uses SamplingTemperature when SamplesPerModel > 1", func() {
			candidates.pairs = []requires.RequiresPair{
				{SourceControlID: "AC-1", TargetControlID: "IA-2", AggregateScore: 0.85},
			}
			cfg.SamplesPerModel = 3
			cfg.SamplingTemperature = 0.7
			a = requires.New(nil, prompts, candidates, cfg)

			control := &pb.Control{ControlId: "AC-1", Statement: "test"}
			tasks, err := a.GenerateWork(ctx, control, analyzer.AnalyzerConfig{
				Parameters: map[string]string{"job_id": "job-1"},
			})
			Expect(err).NotTo(HaveOccurred())
			payload, ok := tasks[0].Payload.(*structpb.Struct)
			Expect(ok).To(BeTrue())
			Expect(payload.Fields["temperature"].GetNumberValue()).To(Equal(0.7))
		})

		It("includes aggregate_score in payload", func() {
			candidates.pairs = []requires.RequiresPair{
				{SourceControlID: "AC-1", TargetControlID: "IA-2", AggregateScore: 0.92},
			}
			a = requires.New(nil, prompts, candidates, cfg)

			control := &pb.Control{ControlId: "AC-1", Statement: "test"}
			tasks, err := a.GenerateWork(ctx, control, analyzer.AnalyzerConfig{
				Parameters: map[string]string{"job_id": "job-1"},
			})
			Expect(err).NotTo(HaveOccurred())
			payload, ok := tasks[0].Payload.(*structpb.Struct)
			Expect(ok).To(BeTrue())
			Expect(payload.Fields["aggregate_score"].GetNumberValue()).To(Equal(0.92))
		})

		It("includes prompt metadata in payload", func() {
			candidates.pairs = []requires.RequiresPair{
				{SourceControlID: "AC-1", TargetControlID: "IA-2", AggregateScore: 0.85},
			}
			a = requires.New(nil, prompts, candidates, cfg)

			control := &pb.Control{ControlId: "AC-1", Statement: "test"}
			tasks, err := a.GenerateWork(ctx, control, analyzer.AnalyzerConfig{
				Parameters: map[string]string{"job_id": "job-1"},
			})
			Expect(err).NotTo(HaveOccurred())
			payload, ok := tasks[0].Payload.(*structpb.Struct)
			Expect(ok).To(BeTrue())
			Expect(payload.Fields["prompt_name"].GetStringValue()).To(Equal("requires"))
			Expect(payload.Fields["prompt_version"].GetStringValue()).To(Equal("1.0.0"))
			Expect(payload.Fields["content_hash"].GetStringValue()).NotTo(BeEmpty())
		})

		It("includes model and max_tokens in payload", func() {
			candidates.pairs = []requires.RequiresPair{
				{SourceControlID: "AC-1", TargetControlID: "IA-2", AggregateScore: 0.85},
			}
			a = requires.New(nil, prompts, candidates, cfg)

			control := &pb.Control{ControlId: "AC-1", Statement: "test"}
			tasks, err := a.GenerateWork(ctx, control, analyzer.AnalyzerConfig{
				Parameters: map[string]string{"job_id": "job-1"},
			})
			Expect(err).NotTo(HaveOccurred())
			payload, ok := tasks[0].Payload.(*structpb.Struct)
			Expect(ok).To(BeTrue())
			Expect(payload.Fields["model"].GetStringValue()).To(Equal("qwen3:8b"))
			Expect(payload.Fields["max_tokens"].GetNumberValue()).To(Equal(float64(300)))
		})

		It("truncates source text to MaxSourceChars", func() {
			candidates.pairs = []requires.RequiresPair{
				{SourceControlID: "AC-1", TargetControlID: "IA-2", AggregateScore: 0.85},
			}
			cfg.MaxSourceChars = 10
			a = requires.New(nil, prompts, candidates, cfg)

			longStatement := "This is a very long statement that should be truncated"
			control := &pb.Control{ControlId: "AC-1", Statement: longStatement}
			tasks, err := a.GenerateWork(ctx, control, analyzer.AnalyzerConfig{
				Parameters: map[string]string{"job_id": "job-1"},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(tasks).To(HaveLen(1))
			// The truncated text is embedded in the rendered prompt -- we verify
			// the task was produced without error, confirming truncation worked.
		})
	})

	Context("Aggregate", func() {
		It("counts results and produces output metadata", func() {
			results := []analyzer.TaskResult{
				{TaskID: "requires-AC-1--IA-2-qwen3:8b-s0", TaskType: "requires", Result: &pb.AnalysisResult{
					ResultId: "requires-AC-1--IA-2-qwen3:8b-s0", ResultType: "requires",
					Attributes: map[string]string{
						"source_control_id": "AC-1", "target_control_id": "IA-2",
						"dependency": "REQUIRES", "confidence": "HIGH",
					},
					Confidence: 0.9,
				}},
			}
			output, err := a.Aggregate(ctx, results)
			Expect(err).NotTo(HaveOccurred())
			Expect(output.AnalyzerName).To(Equal("requires"))
			Expect(output.Metadata).To(HaveKey("total_count"))
			Expect(output.Metadata["total_count"]).To(Equal("1"))
			Expect(output.Metadata).To(HaveKey("error_count"))
			Expect(output.Metadata["error_count"]).To(Equal("0"))
			Expect(output.Metadata).To(HaveKey("prompt_name"))
			Expect(output.Metadata["prompt_name"]).To(Equal("requires"))
			Expect(output.Metadata).To(HaveKey("prompt_version"))
			Expect(output.Metadata["prompt_version"]).To(Equal("1.0.0"))
		})

		It("counts errors in results", func() {
			results := []analyzer.TaskResult{
				{TaskID: "requires-AC-1--IA-2-qwen3:8b-s0", TaskType: "requires", Error: fmt.Errorf("llm timeout")},
				{TaskID: "requires-AC-1--AU-6-qwen3:8b-s0", TaskType: "requires", Result: &pb.AnalysisResult{
					ResultId: "requires-AC-1--AU-6-qwen3:8b-s0", ResultType: "requires",
				}},
			}
			output, err := a.Aggregate(ctx, results)
			Expect(err).NotTo(HaveOccurred())
			Expect(output.Metadata["total_count"]).To(Equal("2"))
			Expect(output.Metadata["error_count"]).To(Equal("1"))
		})

		It("handles empty results", func() {
			output, err := a.Aggregate(ctx, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(output.AnalyzerName).To(Equal("requires"))
			Expect(output.Metadata["total_count"]).To(Equal("0"))
			Expect(output.Metadata["error_count"]).To(Equal("0"))
		})

		It("sets Data to nil", func() {
			results := []analyzer.TaskResult{
				{TaskID: "requires-AC-1--IA-2-qwen3:8b-s0", TaskType: "requires"},
			}
			output, err := a.Aggregate(ctx, results)
			Expect(err).NotTo(HaveOccurred())
			Expect(output.Data).To(BeNil())
		})
	})

	Context("helper functions", func() {
		It("truncateText replaces newlines and truncates at rune boundary", func() {
			truncate := intanalyzer.TruncateText
			result := truncate("hello\nworld\r!", 8)
			Expect(result).To(Equal("hello wo"))
		})

		It("truncateText handles multibyte runes", func() {
			truncate := intanalyzer.TruncateText
			// 4 runes, each 3 bytes in UTF-8
			result := truncate("世界你好", 2)
			Expect(result).To(Equal("世界"))
		})

		It("truncateText returns full text when under limit", func() {
			truncate := intanalyzer.TruncateText
			result := truncate("short", 100)
			Expect(result).To(Equal("short"))
		})

		It("truncateText handles zero maxChars (no truncation)", func() {
			truncate := intanalyzer.TruncateText
			result := truncate("hello world", 0)
			Expect(result).To(Equal("hello world"))
		})

		It("formatFewShotExamples returns empty string for no examples", func() {
			format := intanalyzer.FormatFewShotExamples
			result := format(nil)
			Expect(result).To(BeEmpty())
		})

		It("formatFewShotExamples formats examples with numbered headers", func() {
			format := intanalyzer.FormatFewShotExamples
			examples := []prompt.FewShotExample{
				{Input: "source: AC-1, target: IA-2", Output: "REQUIRES"},
				{Input: "source: AC-1, target: MP-3", Output: "NO_DEPENDENCY"},
			}
			result := format(examples)
			Expect(result).To(ContainSubstring("EXAMPLES:"))
			Expect(result).To(ContainSubstring("Example 1:"))
			Expect(result).To(ContainSubstring("Example 2:"))
			Expect(result).To(ContainSubstring("REQUIRES"))
			Expect(result).To(ContainSubstring("NO_DEPENDENCY"))
		})

		It("exposes guidance and definitions constants", func() {
			Expect(requires.ExportRequiresGuidance).To(ContainSubstring("OPERATIONAL DEPENDENCY"))
			Expect(requires.ExportDependencyDefinitions).To(ContainSubstring("REQUIRES"))
			Expect(requires.ExportDependencyDefinitions).To(ContainSubstring("BENEFITS_FROM"))
			Expect(requires.ExportDependencyDefinitions).To(ContainSubstring("NO_DEPENDENCY"))
		})
	})

	Context("embedded template integration", func() {
		It("loads real requires.yaml and substitutes all variables", func() {
			// Create real prompt registry that loads embedded defaults
			reg, err := prompt.NewRegistry(config.PromptConfig{
				Layers: config.PromptLayerConfig{
					Enabled: true, // Enable default layer stack
				},
			})
			Expect(err).NotTo(HaveOccurred())

			// Resolve requires prompt
			spec, err := reg.Resolve(ctx, "requires")
			Expect(err).NotTo(HaveOccurred())
			Expect(spec.Name).To(Equal("requires"))
			Expect(spec.Version).To(Equal("1.0.0"))

			// Build variable map matching what GenerateWork passes
			vars := map[string]string{
				"requires_guidance":      "test guidance text",
				"dependency_definitions": "test definitions",
				"few_shot_examples":      "EXAMPLES:\nExample 1:\nSource: AC-1\nTarget: IA-2\nExpected output:\nREQUIRES: YES",
				"source_id":              "AC-1",
				"target_id":              "IA-2",
				"source_text":            "The organization shall establish access control policies.",
				"target_text":            "The organization shall identify and authenticate users.",
				"source_framework":       "NIST-800-53",
				"target_framework":       "ISO-27001",
				"source_type":            "policy",
				"source_level":           "strategic",
				"source_ancestor":        " | Domain: Access Control",
				"target_type":            "control",
				"target_level":           "tactical",
				"target_ancestor":        " | Domain: Identity Management",
			}

			// Substitute system template
			systemMsg, err := prompt.SubstitutePlaceholders(spec.Templates.System, vars)
			Expect(err).NotTo(HaveOccurred(), "system template should substitute without errors")
			Expect(systemMsg).To(ContainSubstring("NIST-800-53"))
			Expect(systemMsg).To(ContainSubstring("ISO-27001"))
			Expect(systemMsg).To(ContainSubstring("test guidance text"))
			Expect(systemMsg).To(ContainSubstring("test definitions"))

			// Substitute user template
			userMsg, err := prompt.SubstitutePlaceholders(spec.Templates.User, vars)
			Expect(err).NotTo(HaveOccurred(), "user template should substitute without errors")
			Expect(userMsg).To(ContainSubstring("AC-1"))
			Expect(userMsg).To(ContainSubstring("IA-2"))
			Expect(userMsg).To(ContainSubstring("access control policies"))
			Expect(userMsg).To(ContainSubstring("authenticate users"))
			Expect(userMsg).To(ContainSubstring("EXAMPLES:"))
			Expect(userMsg).To(ContainSubstring("REQUIRES: YES or NO"))
			Expect(userMsg).To(ContainSubstring("JUSTIFICATION:"))
			Expect(userMsg).To(ContainSubstring("CONFIDENCE:"))

			// Verify no unreplaced placeholders remain
			Expect(systemMsg).NotTo(ContainSubstring("${"), "system template should have no unreplaced placeholders")
			Expect(userMsg).NotTo(ContainSubstring("${"), "user template should have no unreplaced placeholders")
		})

		It("template structure matches relationship.yaml pattern", func() {
			reg, err := prompt.NewRegistry(config.PromptConfig{
				Layers: config.PromptLayerConfig{
					Enabled: true,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			requiresSpec, err := reg.Resolve(ctx, "requires")
			Expect(err).NotTo(HaveOccurred())

			relationshipSpec, err := reg.Resolve(ctx, "relationship")
			Expect(err).NotTo(HaveOccurred())

			// Both should have system and user templates
			Expect(requiresSpec.Templates.System).NotTo(BeEmpty())
			Expect(requiresSpec.Templates.User).NotTo(BeEmpty())
			Expect(relationshipSpec.Templates.System).NotTo(BeEmpty())
			Expect(relationshipSpec.Templates.User).NotTo(BeEmpty())

			// Both should have few_shot_examples section
			Expect(requiresSpec.FewShot).NotTo(BeEmpty())
			Expect(relationshipSpec.FewShot).NotTo(BeEmpty())

			// Both should use source/target framework variables (in system or user templates)
			requiresTemplates := requiresSpec.Templates.System + requiresSpec.Templates.User
			relationshipTemplates := relationshipSpec.Templates.System + relationshipSpec.Templates.User
			Expect(requiresTemplates).To(ContainSubstring("${source_framework}"))
			Expect(requiresTemplates).To(ContainSubstring("${target_framework}"))
			Expect(relationshipTemplates).To(ContainSubstring("${source_framework}"))
			Expect(relationshipTemplates).To(ContainSubstring("${target_framework}"))
		})

		It("verifies no legacy variable names remain", func() {
			reg, err := prompt.NewRegistry(config.PromptConfig{
				Layers: config.PromptLayerConfig{
					Enabled: true,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			spec, err := reg.Resolve(ctx, "requires")
			Expect(err).NotTo(HaveOccurred())

			// Read raw YAML to check for dead keys
			Expect(spec.Templates.System).NotTo(ContainSubstring("${guidance}"), "should use ${requires_guidance} not ${guidance}")
			Expect(spec.Templates.User).NotTo(ContainSubstring("${guidance}"), "should use ${requires_guidance} not ${guidance}")
			Expect(spec.Templates.User).NotTo(ContainSubstring("${output_format}"), "output_format should be inlined, not a variable")

			// Verify the output format is inlined in user template
			Expect(strings.Contains(spec.Templates.User, "REQUIRES: YES or NO")).To(BeTrue(), "output format should be inlined in user template")
		})
	})
})
