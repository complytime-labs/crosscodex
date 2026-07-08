package artifacts_test

import (
	"context"
	"fmt"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/structpb"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/analyzer/artifacts"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/prompt"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// contextKey is a custom type for context keys to avoid collisions.
type contextKey string

const testContextKey contextKey = "test"

func TestArtifactsAnalyzer(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "ArtifactsAnalyzer Suite")
}

var _ = Describe("ArtifactsAnalyzer", func() {
	var (
		a   *artifacts.ArtifactsAnalyzer
		cfg config.ArtifactsConfig
	)

	BeforeEach(func() {
		cfg = config.ArtifactsConfig{
			Enabled:             true,
			Models:              []string{"model-a", "model-b"},
			SamplesPerModel:     1,
			SamplingTemperature: 0.3,
			MaxTokens:           500,
			MaxTextChars:        1500,
			FuzzyThreshold:      0.6,
		}
	})

	Describe("interface compliance", func() {
		It("implements analyzer.Analyzer[*pb.Control]", func() {
			// Compile-time check is in artifacts.go; this verifies at runtime.
			var iface analyzer.Analyzer[*pb.Control]
			a = artifacts.New(nil, nil, cfg)
			iface = a
			Expect(iface).NotTo(BeNil())
		})
	})

	Describe("Name", func() {
		It("returns 'artifacts'", func() {
			a = artifacts.New(nil, nil, cfg)
			Expect(a.Name()).To(Equal("artifacts"))
		})
	})

	Describe("DependsOn", func() {
		It("returns empty slice (no dependencies)", func() {
			a = artifacts.New(nil, nil, cfg)
			Expect(a.DependsOn()).To(BeEmpty())
		})
	})

	Describe("ResultSchema", func() {
		It("returns an AnalysisResult proto", func() {
			a = artifacts.New(nil, nil, cfg)
			schema := a.ResultSchema()
			Expect(schema).NotTo(BeNil())
			_, ok := schema.(*pb.AnalysisResult)
			Expect(ok).To(BeTrue())
		})
	})

	Describe("GenerateWork", func() {
		var (
			ctx          context.Context
			control      *pb.Control
			artifactsCfg config.ArtifactsConfig
			analyzerCfg  analyzer.AnalyzerConfig
			prompts      *mockPromptRegistry
		)

		BeforeEach(func() {
			ctx = context.WithValue(context.Background(), testContextKey, "value")
			var err error
			ctx, err = tenant.WithTenant(ctx, "test-tenant")
			Expect(err).NotTo(HaveOccurred())

			control = &pb.Control{
				Identifier: "AC-1",
				Statement:  "The organization shall define access control policy.",
				Parts:      map[string]string{"class": "compliance-requirement"},
			}

			artifactsCfg = config.ArtifactsConfig{
				Enabled:             true,
				Models:              []string{"model-a"},
				SamplesPerModel:     1,
				SamplingTemperature: 0.3,
				MaxTokens:           500,
				MaxTextChars:        1500,
				FuzzyThreshold:      0.6,
			}

			analyzerCfg = analyzer.AnalyzerConfig{
				Parameters: map[string]string{
					"framework":           "NIST-800-53",
					"ancestor_title_AC-1": "Access Control Policy and Procedures",
				},
			}

			prompts = &mockPromptRegistry{
				spec: &prompt.PromptSpec{
					Name:    "artifacts",
					Version: "1.0.0",
					Templates: prompt.TemplateSet{
						System: "System: ${extraction_guidance} ${artifact_type_definitions}",
						User:   "User: ${few_shot_examples} ${output_format} ${control_id} ${framework} ${ancestor_title} ${requirement_text}",
					},
				},
			}
		})

		Context("section skipping", func() {
			It("returns a skip task when class is compliance-section", func() {
				control.Parts["class"] = "compliance-section"
				a = artifacts.New(nil, prompts, artifactsCfg)

				tasks, err := a.GenerateWork(ctx, control, analyzerCfg)
				Expect(err).NotTo(HaveOccurred())
				Expect(tasks).To(HaveLen(1))
				Expect(tasks[0].TaskID).To(Equal("artifacts-AC-1-skip"))
				Expect(tasks[0].TaskType).To(Equal("artifacts"))

				payloadStruct, ok := tasks[0].Payload.(*structpb.Struct)
				Expect(ok).To(BeTrue())
				payload := payloadStruct.AsMap()
				Expect(payload["control_id"]).To(Equal("AC-1"))
				Expect(payload["skipped"]).To(Equal("true"))
			})
		})

		Context("task count", func() {
			It("produces N*M tasks for N models and M samples", func() {
				artifactsCfg.Models = []string{"model-a", "model-b"}
				artifactsCfg.SamplesPerModel = 3
				a = artifacts.New(nil, prompts, artifactsCfg)

				tasks, err := a.GenerateWork(ctx, control, analyzerCfg)
				Expect(err).NotTo(HaveOccurred())
				Expect(tasks).To(HaveLen(2 * 3)) // 2 models * 3 samples
			})

			It("produces 1 task for 1 model and 1 sample", func() {
				artifactsCfg.Models = []string{"model-a"}
				artifactsCfg.SamplesPerModel = 1
				a = artifacts.New(nil, prompts, artifactsCfg)

				tasks, err := a.GenerateWork(ctx, control, analyzerCfg)
				Expect(err).NotTo(HaveOccurred())
				Expect(tasks).To(HaveLen(1))
			})

			It("produces 0 tasks when models is empty", func() {
				artifactsCfg.Models = []string{}
				artifactsCfg.SamplesPerModel = 3
				a = artifacts.New(nil, prompts, artifactsCfg)

				tasks, err := a.GenerateWork(ctx, control, analyzerCfg)
				Expect(err).NotTo(HaveOccurred())
				Expect(tasks).To(BeEmpty())
			})
		})

		Context("temperature logic", func() {
			It("forces temperature to 0.0 when SamplesPerModel <= 1", func() {
				artifactsCfg.Models = []string{"model-a"}
				artifactsCfg.SamplesPerModel = 1
				artifactsCfg.SamplingTemperature = 0.7
				a = artifacts.New(nil, prompts, artifactsCfg)

				tasks, err := a.GenerateWork(ctx, control, analyzerCfg)
				Expect(err).NotTo(HaveOccurred())
				Expect(tasks).To(HaveLen(1))

				payloadStruct, ok := tasks[0].Payload.(*structpb.Struct)
				Expect(ok).To(BeTrue())
				payload := payloadStruct.AsMap()
				Expect(payload["temperature"]).To(Equal(0.0))
			})

			It("uses configured temperature when SamplesPerModel > 1", func() {
				artifactsCfg.Models = []string{"model-a"}
				artifactsCfg.SamplesPerModel = 3
				artifactsCfg.SamplingTemperature = 0.7
				a = artifacts.New(nil, prompts, artifactsCfg)

				tasks, err := a.GenerateWork(ctx, control, analyzerCfg)
				Expect(err).NotTo(HaveOccurred())
				Expect(tasks).To(HaveLen(3))

				for _, task := range tasks {
					payloadStruct, ok := task.Payload.(*structpb.Struct)
					Expect(ok).To(BeTrue())
					payload := payloadStruct.AsMap()
					Expect(payload["temperature"]).To(Equal(0.7))
				}
			})
		})

		Context("payload fields", func() {
			It("includes all required fields in task payload", func() {
				artifactsCfg.Models = []string{"model-a"}
				artifactsCfg.SamplesPerModel = 2
				artifactsCfg.SamplingTemperature = 0.3
				artifactsCfg.MaxTokens = 500
				a = artifacts.New(nil, prompts, artifactsCfg)

				tasks, err := a.GenerateWork(ctx, control, analyzerCfg)
				Expect(err).NotTo(HaveOccurred())
				Expect(tasks).To(HaveLen(2))

				for idx, task := range tasks {
					payloadStruct, ok := task.Payload.(*structpb.Struct)
					Expect(ok).To(BeTrue())
					payload := payloadStruct.AsMap()

					Expect(payload["control_id"]).To(Equal("AC-1"))
					Expect(payload["model"]).To(Equal("model-a"))
					Expect(payload["sample_index"]).To(Equal(float64(idx)))
					Expect(payload["temperature"]).To(Equal(0.3))
					Expect(payload["max_tokens"]).To(Equal(float64(500)))
					Expect(payload["prompt_name"]).To(Equal("artifacts"))
					Expect(payload["prompt_version"]).To(Equal("1.0.0"))
					Expect(payload["content_hash"]).NotTo(BeEmpty())
				}
			})

			It("generates consistent content_hash for same messages", func() {
				artifactsCfg.Models = []string{"model-a", "model-b"}
				artifactsCfg.SamplesPerModel = 2
				a = artifacts.New(nil, prompts, artifactsCfg)

				tasks, err := a.GenerateWork(ctx, control, analyzerCfg)
				Expect(err).NotTo(HaveOccurred())
				Expect(tasks).To(HaveLen(4))

				// All tasks for the same control should have the same content_hash
				firstPayloadStruct, ok := tasks[0].Payload.(*structpb.Struct)
				Expect(ok).To(BeTrue())
				firstHash := firstPayloadStruct.AsMap()["content_hash"].(string)

				for _, task := range tasks {
					payloadStruct, ok := task.Payload.(*structpb.Struct)
					Expect(ok).To(BeTrue())
					payload := payloadStruct.AsMap()
					Expect(payload["content_hash"]).To(Equal(firstHash))
				}
			})
		})

		Context("task ID format", func() {
			It("generates task IDs in artifacts-{control_id}-{model}-s{sample_index} format", func() {
				artifactsCfg.Models = []string{"model-a"}
				artifactsCfg.SamplesPerModel = 3
				a = artifacts.New(nil, prompts, artifactsCfg)

				tasks, err := a.GenerateWork(ctx, control, analyzerCfg)
				Expect(err).NotTo(HaveOccurred())

				Expect(tasks[0].TaskID).To(Equal("artifacts-AC-1-model-a-s0"))
				Expect(tasks[1].TaskID).To(Equal("artifacts-AC-1-model-a-s1"))
				Expect(tasks[2].TaskID).To(Equal("artifacts-AC-1-model-a-s2"))
			})
		})

		Context("error cases", func() {
			It("returns error when tenant is missing from context", func() {
				a = artifacts.New(nil, prompts, artifactsCfg)
				_, err := a.GenerateWork(context.Background(), control, analyzerCfg)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("tenant"))
			})

			It("returns error when prompt resolution fails", func() {
				prompts.err = fmt.Errorf("prompt not found")
				a = artifacts.New(nil, prompts, artifactsCfg)

				_, err := a.GenerateWork(ctx, control, analyzerCfg)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("resolving prompt"))
			})

			It("returns error when system template substitution fails", func() {
				prompts.spec.Templates.System = "System: ${undefined_placeholder}"
				a = artifacts.New(nil, prompts, artifactsCfg)

				_, err := a.GenerateWork(ctx, control, analyzerCfg)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("substituting system template"))
			})

			It("returns error when user template substitution fails", func() {
				prompts.spec.Templates.User = "User: ${undefined_placeholder}"
				a = artifacts.New(nil, prompts, artifactsCfg)

				_, err := a.GenerateWork(ctx, control, analyzerCfg)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("substituting user template"))
			})
		})
	})

	Describe("Aggregate", func() {
		It("counts results and includes metadata", func() {
			a = artifacts.New(nil, nil, cfg)
			ctx, err := tenant.WithTenant(context.Background(), "test-tenant")
			Expect(err).NotTo(HaveOccurred())

			results := []analyzer.TaskResult{
				{TaskID: "t1", Result: &pb.AnalysisResult{}},
				{TaskID: "t2", Error: fmt.Errorf("fail")},
				{TaskID: "t3", Result: &pb.AnalysisResult{}},
			}

			output, err := a.Aggregate(ctx, results)
			Expect(err).NotTo(HaveOccurred())
			Expect(output.AnalyzerName).To(Equal("artifacts"))
			Expect(output.Metadata["total_count"]).To(Equal("3"))
			Expect(output.Metadata["error_count"]).To(Equal("1"))
			Expect(output.Data).To(BeNil())
		})
	})
})

// mockPromptRegistry is a minimal fake implementation of prompt.Registry for testing.
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
	return []string{"artifacts"}, nil
}

func (m *mockPromptRegistry) Layers(_ context.Context, _ string) ([]prompt.LayerInfo, error) {
	return nil, nil
}
