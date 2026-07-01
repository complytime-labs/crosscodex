//go:build integration

package classify_test

import (
	"context"
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/structpb"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/analyzer/classify"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/prompt"
)

var _ = Describe("Classify Infrastructure Integration", func() {
	var (
		reg prompt.Registry
		cfg config.ClassificationConfig
	)

	BeforeEach(func() {
		// Real prompt registry with embedded defaults — no mock.
		var err error
		reg, err = prompt.NewRegistry(config.PromptConfig{
			Layers: config.PromptLayerConfig{Enabled: true},
		})
		Expect(err).NotTo(HaveOccurred())

		cfg = config.ClassificationConfig{
			Enabled:       true,
			Model:         "test-model",
			MaxTextLength: 2000,
			Temperature:   0.0,
			MaxTokens:     20,
		}
	})

	Context("GenerateWork with real prompt registry", func() {
		It("produces a task with well-formed payload for a requirement control", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")

			mock := &cannedClassifyClient{response: "Technical|Operational"}
			a := classify.New(mock, reg, cfg)

			control := &pb.Control{
				ControlId:  "nist-800-53/AC-1",
				CatalogId:  "nist-800-53",
				Identifier: "AC-1",
				Title:      "Access Control Policy and Procedures",
				Statement:  "The organization develops, documents, and disseminates an access control policy.",
				Parts:      map[string]string{"class": "requirement"},
			}

			tasks, err := a.GenerateWork(ctx, control, analyzer.AnalyzerConfig{})
			Expect(err).NotTo(HaveOccurred())
			Expect(tasks).To(HaveLen(1))

			task := tasks[0]
			Expect(task.TaskID).To(Equal("classify-nist-800-53/AC-1"))
			Expect(task.TaskType).To(Equal("classify"))

			payload, ok := task.Payload.(*structpb.Struct)
			Expect(ok).To(BeTrue(), "payload should be *structpb.Struct")

			// Verify all required payload fields are present.
			fields := payload.GetFields()
			Expect(fields).To(HaveKey("control_id"))
			Expect(fields).To(HaveKey("model"))
			Expect(fields).To(HaveKey("temperature"))
			Expect(fields).To(HaveKey("max_tokens"))
			Expect(fields).To(HaveKey("prompt_name"))
			Expect(fields).To(HaveKey("prompt_version"))
			Expect(fields).To(HaveKey("content_hash"))

			Expect(fields["control_id"].GetStringValue()).To(Equal("nist-800-53/AC-1"))
			Expect(fields["model"].GetStringValue()).To(Equal("test-model"))
			Expect(fields["prompt_name"].GetStringValue()).To(Equal("classify"))
			Expect(fields["prompt_version"].GetStringValue()).To(Equal("1.0.0"))
			Expect(fields["content_hash"].GetStringValue()).NotTo(BeEmpty())
		})

		It("auto-skips section controls without calling LLM", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")

			mock := &cannedClassifyClient{response: "Technical|Operational"}
			a := classify.New(mock, reg, cfg)

			section := &pb.Control{
				ControlId:  "nist-800-53/AC",
				CatalogId:  "nist-800-53",
				Identifier: "AC",
				Title:      "Access Control",
				Statement:  "",
				Parts:      map[string]string{"class": "compliance-section"},
			}

			tasks, err := a.GenerateWork(ctx, section, analyzer.AnalyzerConfig{})
			Expect(err).NotTo(HaveOccurred())
			Expect(tasks).To(HaveLen(1))

			// Section tasks carry a pre-built AnalysisResult, not a structpb.Struct.
			result, ok := tasks[0].Payload.(*pb.AnalysisResult)
			Expect(ok).To(BeTrue(), "section payload should be *pb.AnalysisResult")
			Expect(result.Attributes["skipped"]).To(Equal("true"))
			Expect(result.Attributes["type"]).To(Equal("None"))
			Expect(result.Attributes["level"]).To(Equal("None"))

			// LLM should not have been called.
			Expect(mock.callCount).To(Equal(0))
		})
	})

	Context("GenerateWork to Aggregate round-trip", func() {
		It("processes classify tasks through the full pipeline with canned responses", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")

			mock := &cannedClassifyClient{response: "Technical|Operational"}
			a := classify.New(mock, reg, cfg)

			controls := []*pb.Control{
				{
					ControlId: "nist-800-53/AC-1", CatalogId: "nist-800-53",
					Identifier: "AC-1", Title: "Access Control Policy",
					Statement: "Develop access control policy.",
					Parts:     map[string]string{"class": "requirement"},
				},
				{
					ControlId: "nist-800-53/AC", CatalogId: "nist-800-53",
					Identifier: "AC", Title: "Access Control",
					Parts: map[string]string{"class": "compliance-section"},
				},
				{
					ControlId: "nist-800-53/SI-2", CatalogId: "nist-800-53",
					Identifier: "SI-2", Title: "Flaw Remediation",
					Statement: "Identify and remediate flaws.",
					Parts:     map[string]string{"class": "requirement"},
				},
			}

			var allResults []analyzer.TaskResult

			for _, ctrl := range controls {
				tasks, err := a.GenerateWork(ctx, ctrl, analyzer.AnalyzerConfig{})
				Expect(err).NotTo(HaveOccurred())

				for _, task := range tasks {
					// Simulate worker: sections are pre-built, requirements need LLM.
					if result, ok := task.Payload.(*pb.AnalysisResult); ok {
						// Pre-built section result — pass through.
						allResults = append(allResults, analyzer.TaskResult{
							TaskID:   task.TaskID,
							TaskType: "classify",
							Result:   result,
							Duration: time.Millisecond,
						})
					} else {
						// Requirement — simulate LLM call with canned response.
						parsed, err := classify.ParseClassification(mock.response)
						Expect(err).NotTo(HaveOccurred())

						allResults = append(allResults, analyzer.TaskResult{
							TaskID:   task.TaskID,
							TaskType: "classify",
							Result: &pb.AnalysisResult{
								ResultId:   task.TaskID,
								ResultType: "classification",
								Attributes: map[string]string{
									"type":  parsed.Type.String(),
									"level": parsed.Level.String(),
								},
								Confidence: 0.9,
							},
							Duration: 10 * time.Millisecond,
						})
					}
				}
			}

			output, err := a.Aggregate(ctx, allResults)
			Expect(err).NotTo(HaveOccurred())
			Expect(output.AnalyzerName).To(Equal("classify"))
			Expect(output.Metadata["classified_count"]).To(Equal("2"))
			Expect(output.Metadata["skipped_count"]).To(Equal("1"))
			Expect(output.Metadata["error_count"]).To(Equal("0"))
			Expect(output.Metadata["total_count"]).To(Equal("3"))
			Expect(output.Metadata["prompt_name"]).To(Equal("classify"))
		})
	})
})

// cannedClassifyClient returns a fixed response for every Complete call.
type cannedClassifyClient struct {
	response  string
	callCount int
}

func (c *cannedClassifyClient) Complete(_ context.Context, _ *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
	c.callCount++
	return &llmclient.CompletionResponse{
		ID:    "test-id",
		Model: "test-model",
		Choices: []llmclient.CompletionChoice{{
			Index:        0,
			Message:      llmclient.ChatMessage{Role: "assistant", Content: c.response},
			FinishReason: "stop",
		}},
		Usage: llmclient.TokenUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}, nil
}

func (c *cannedClassifyClient) Embed(_ context.Context, _ *llmclient.EmbeddingRequest) (*llmclient.EmbeddingResponse, error) {
	return nil, errors.New("classify does not use embeddings")
}

func (c *cannedClassifyClient) Health(_ context.Context) error { return nil }
func (c *cannedClassifyClient) Close() error                   { return nil }
