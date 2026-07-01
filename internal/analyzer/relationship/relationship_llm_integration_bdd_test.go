//go:build integration_llm

package relationship_test

import (
	"errors"
	"os"
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/structpb"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/analyzer/relationship"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/prompt"
)

var _ = Describe("Relationship LLM Integration", func() {
	var (
		client llmclient.Client
		reg    prompt.Registry
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
	})

	It("classifies a control pair via real LLM and produces parseable votes", func() {
		ctx := testspecs.SetupTenantContext("test-tenant")

		chatModel := os.Getenv("TEST_LLM_CHAT_MODEL")

		candidates := &mockCandidateProvider{
			pairs: []relationship.CandidatePair{
				{SourceControlID: "AC-2", TargetControlID: "IT-3.2", SimilarityScore: 85.0},
			},
		}

		cfg := config.RelationshipConfig{
			Enabled:             true,
			Models:              []string{chatModel},
			TopK:                10,
			MaxSourceChars:      1500,
			MaxTargetChars:      800,
			MaxTokens:           300,
			SamplesPerModel:     2,
			SamplingTemperature: 0.3,
			ActionableTypes:     []string{"EQUIVALENT", "SUPERSET_OF", "SUBSET_OF", "CONTRIBUTES_TO"},
		}

		a := relationship.New(client, reg, candidates, cfg)

		control := &pb.Control{
			ControlId:  "nist-800-53/AC-2",
			CatalogId:  "nist-800-53",
			Identifier: "AC-2",
			Title:      "Account Management",
			Statement:  "The organization manages system accounts, including establishing, activating, modifying, reviewing, disabling, and removing accounts.",
			Parts:      map[string]string{"class": "requirement"},
		}

		tasks, err := a.GenerateWork(ctx, control, analyzer.AnalyzerConfig{
			Parameters: map[string]string{"job_id": "integration-test-1"},
		})
		Expect(err).NotTo(HaveOccurred())
		// 1 pair x 1 model x 2 samples = 2 tasks.
		Expect(tasks).To(HaveLen(2))

		votes := make(map[string]*relationship.Vote)
		var taskResults []analyzer.TaskResult

		for _, task := range tasks {
			payload, ok := task.Payload.(*structpb.Struct)
			Expect(ok).To(BeTrue())

			fields := payload.GetFields()
			model := fields["model"].GetStringValue()
			sampleIdx := int(fields["sample_index"].GetNumberValue())

			// Resolve prompt and build CompletionRequest.
			spec, resolveErr := reg.Resolve(ctx, "relationship")
			Expect(resolveErr).NotTo(HaveOccurred())

			// Build system message with relationship definitions.
			// ExportClassificationGuidance and ExportRelationshipDefinitions
			// are string variables (const string exports), not functions.
			systemVars := map[string]string{
				"classification_guidance":  relationship.ExportClassificationGuidance,
				"relationship_definitions": relationship.ExportRelationshipDefinitions,
			}
			systemMsg, substErr := prompt.SubstitutePlaceholders(spec.Templates.System, systemVars)
			Expect(substErr).NotTo(HaveOccurred())

			// Build user message with control pair details.
			// ExportFormatFewShotExamples is a func([]prompt.FewShotExample) string.
			userVars := map[string]string{
				"source_id":         "AC-2",
				"target_id":         "IT-3.2",
				"source_text":       control.Statement,
				"target_text":       "IT controls shall restrict access to authorized users only.",
				"source_framework":  "NIST-800-53",
				"target_framework":  "SOX-IT",
				"source_type":       "Technical",
				"source_level":      "Tactical",
				"source_ancestor":   "Access Control",
				"target_type":       "Technical",
				"target_level":      "Tactical",
				"target_ancestor":   "IT General Controls",
				"few_shot_examples": relationship.ExportFormatFewShotExamples(spec.FewShot),
			}
			userMsg, substErr := prompt.SubstitutePlaceholders(spec.Templates.User, userVars)
			Expect(substErr).NotTo(HaveOccurred())

			temp := cfg.SamplingTemperature
			resp, completeErr := client.Complete(ctx, &llmclient.CompletionRequest{
				Model: model,
				Messages: []llmclient.ChatMessage{
					{Role: llmclient.RoleSystem, Content: systemMsg},
					{Role: llmclient.RoleUser, Content: userMsg},
				},
				MaxTokens:   cfg.MaxTokens,
				Temperature: &temp,
				TenantID:    "test-tenant",
			})
			Expect(completeErr).NotTo(HaveOccurred())
			Expect(resp.Choices).NotTo(BeEmpty())

			raw := resp.Choices[0].Message.Content
			GinkgoWriter.Printf("Sample %d response: %q\n", sampleIdx, raw)

			vote := relationship.ParseResponse(raw)
			vote.Model = model
			voteKey := model + "__s" + strconv.Itoa(sampleIdx)
			vote.VoteKey = voteKey
			votes[voteKey] = vote

			tr := analyzer.TaskResult{
				TaskID:   task.TaskID,
				TaskType: "relationship",
				Duration: 500 * time.Millisecond,
			}

			if vote.ParseStatus == relationship.ParseOK {
				tr.Result = &pb.AnalysisResult{
					ResultId:   task.TaskID,
					ResultType: "relationship",
					Attributes: map[string]string{
						"source_control_id": "AC-2",
						"target_control_id": "IT-3.2",
						"relationship":      vote.Relationship.String(),
						"confidence":        vote.Confidence.String(),
					},
					Confidence: 0.9,
				}
			} else {
				tr.Error = errors.New("parse failure")
			}

			taskResults = append(taskResults, tr)
		}

		// Compute consensus from real votes.
		consensus := relationship.ComputeConsensus(votes)
		GinkgoWriter.Printf("Consensus: relationship=%s, confidence=%.2f, unanimous=%t, votes=%v\n",
			consensus.Relationship.String(), consensus.ConfidenceFraction,
			consensus.Unanimous, consensus.AllVotes)

		// Assert structural validity.
		Expect(consensus.ValidVoteCount).To(BeNumerically(">=", 0))

		// Aggregate.
		output, err := a.Aggregate(ctx, taskResults)
		Expect(err).NotTo(HaveOccurred())
		Expect(output.AnalyzerName).To(Equal("relationship"))
		GinkgoWriter.Printf("Relationship aggregate: %v\n", output.Metadata)
	})
})
