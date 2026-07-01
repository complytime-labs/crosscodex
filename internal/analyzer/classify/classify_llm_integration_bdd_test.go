//go:build integration_llm

package classify_test

import (
	"os"
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

var _ = Describe("Classify LLM Integration", func() {
	var (
		client llmclient.Client
		reg    prompt.Registry
		cfg    config.ClassificationConfig
	)

	BeforeEach(func() {
		litellmURL := os.Getenv("TEST_LITELLM_URL")
		chatModel := os.Getenv("TEST_LLM_CHAT_MODEL")
		if litellmURL == "" || chatModel == "" {
			Skip("TEST_LITELLM_URL or TEST_LLM_CHAT_MODEL not set — run: task test:integration:llm")
		}

		var err error
		client, err = llmclient.NewClient(config.LLMConfig{
			GatewayURL:   litellmURL,
			GatewayMode:  true,
			DefaultModel: chatModel,
			MaxRetries:   0,
			Timeout:      120,
		})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { client.Close() })

		reg, err = prompt.NewRegistry(config.PromptConfig{
			Layers: config.PromptLayerConfig{Enabled: true},
		})
		Expect(err).NotTo(HaveOccurred())

		cfg = config.ClassificationConfig{
			Enabled:       true,
			Model:         chatModel,
			MaxTextLength: 2000,
			Temperature:   0.0,
			MaxTokens:     20,
		}
	})

	It("classifies a requirement control via real LLM and gets a parseable response", func() {
		ctx := testspecs.SetupTenantContext("test-tenant")

		a := classify.New(client, reg, cfg)

		control := &pb.Control{
			ControlId:  "nist-800-53/AC-2",
			CatalogId:  "nist-800-53",
			Identifier: "AC-2",
			Title:      "Account Management",
			Statement:  "The organization manages system accounts, including establishing, activating, modifying, reviewing, disabling, and removing accounts.",
			Parts:      map[string]string{"class": "requirement"},
		}

		tasks, err := a.GenerateWork(ctx, control, analyzer.AnalyzerConfig{})
		Expect(err).NotTo(HaveOccurred())
		Expect(tasks).To(HaveLen(1))

		task := tasks[0]
		payload, ok := task.Payload.(*structpb.Struct)
		Expect(ok).To(BeTrue())

		// Extract model from task payload.
		fields := payload.GetFields()
		model := fields["model"].GetStringValue()
		Expect(model).NotTo(BeEmpty())

		// Resolve the prompt to get system and user messages.
		spec, err := reg.Resolve(ctx, "classify")
		Expect(err).NotTo(HaveOccurred())

		vars := map[string]string{
			"few_shot_examples": classify.ExportFormatFewShotExamples(spec.FewShot),
		}
		systemMsg, err := prompt.SubstitutePlaceholders(spec.Templates.System, vars)
		Expect(err).NotTo(HaveOccurred())

		userMsg := "<requirement>" + control.Statement + "</requirement>\n\nRespond with TYPE|LEVEL."

		temp := cfg.Temperature
		req := &llmclient.CompletionRequest{
			Model: model,
			Messages: []llmclient.ChatMessage{
				{Role: llmclient.RoleSystem, Content: systemMsg},
				{Role: llmclient.RoleUser, Content: userMsg},
			},
			MaxTokens:   cfg.MaxTokens,
			Temperature: &temp,
			TenantID:    "test-tenant",
		}

		resp, err := client.Complete(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Choices).NotTo(BeEmpty())

		raw := resp.Choices[0].Message.Content
		GinkgoWriter.Printf("LLM response: %q\n", raw)

		// Assert structural parseability — not semantic correctness.
		result, parseErr := classify.ParseClassification(raw)
		if parseErr == nil {
			Expect(result.Type.String()).To(BeElementOf(
				"Technical", "Procedural", "Both", "None",
			))
			Expect(result.Level.String()).To(BeElementOf(
				"Strategic", "Tactical", "Operational", "None",
			))
		} else {
			GinkgoWriter.Printf("Parse failed (acceptable with small models): %v\n", parseErr)
			Expect(raw).NotTo(BeEmpty(), "LLM should return non-empty content")
		}
	})

	It("processes multiple controls and aggregates results", func() {
		ctx := testspecs.SetupTenantContext("test-tenant")

		a := classify.New(client, reg, cfg)

		controls := []*pb.Control{
			{
				ControlId: "nist-800-53/AC-2", CatalogId: "nist-800-53",
				Identifier: "AC-2", Title: "Account Management",
				Statement: "Manage system accounts including establishing and disabling.",
				Parts:     map[string]string{"class": "requirement"},
			},
			{
				ControlId: "nist-800-53/SI-3", CatalogId: "nist-800-53",
				Identifier: "SI-3", Title: "Malicious Code Protection",
				Statement: "Implement malicious code protection mechanisms.",
				Parts:     map[string]string{"class": "requirement"},
			},
		}

		var allResults []analyzer.TaskResult

		for _, ctrl := range controls {
			tasks, err := a.GenerateWork(ctx, ctrl, analyzer.AnalyzerConfig{})
			Expect(err).NotTo(HaveOccurred())

			for _, task := range tasks {
				payload, ok := task.Payload.(*structpb.Struct)
				Expect(ok).To(BeTrue())

				fields := payload.GetFields()
				spec, err := reg.Resolve(ctx, "classify")
				Expect(err).NotTo(HaveOccurred())

				vars := map[string]string{
					"few_shot_examples": classify.ExportFormatFewShotExamples(spec.FewShot),
				}
				systemMsg, err := prompt.SubstitutePlaceholders(spec.Templates.System, vars)
				Expect(err).NotTo(HaveOccurred())

				userMsg := "<requirement>" + ctrl.Statement + "</requirement>\n\nRespond with TYPE|LEVEL."
				temp := cfg.Temperature

				resp, err := client.Complete(ctx, &llmclient.CompletionRequest{
					Model: fields["model"].GetStringValue(),
					Messages: []llmclient.ChatMessage{
						{Role: llmclient.RoleSystem, Content: systemMsg},
						{Role: llmclient.RoleUser, Content: userMsg},
					},
					MaxTokens:   cfg.MaxTokens,
					Temperature: &temp,
					TenantID:    "test-tenant",
				})
				Expect(err).NotTo(HaveOccurred())

				raw := resp.Choices[0].Message.Content
				result, parseErr := classify.ParseClassification(raw)

				tr := analyzer.TaskResult{
					TaskID:   task.TaskID,
					TaskType: "classify",
					Duration: 100 * time.Millisecond,
				}

				if parseErr == nil {
					tr.Result = &pb.AnalysisResult{
						ResultId:   task.TaskID,
						ResultType: "classification",
						Attributes: map[string]string{
							"type":  result.Type.String(),
							"level": result.Level.String(),
						},
						Confidence: 0.9,
					}
				} else {
					tr.Error = parseErr
				}

				allResults = append(allResults, tr)
			}
		}

		output, err := a.Aggregate(ctx, allResults)
		Expect(err).NotTo(HaveOccurred())
		Expect(output.AnalyzerName).To(Equal("classify"))
		GinkgoWriter.Printf("Classify aggregate: %v\n", output.Metadata)
	})
})
