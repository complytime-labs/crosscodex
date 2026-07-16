//go:build integration

package relationship_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

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

var _ = Describe("Parser to Consensus Integration", func() {
	Context("multi-model panel with unanimous SUPERSET_OF", func() {
		It("produces unanimous consensus with full confidence", func() {
			responses := []string{
				"RELATIONSHIP: SUPERSET_OF\nCONTRIBUTION_TYPE: N/A\nJUSTIFICATION: Source encompasses target.\nCONFIDENCE: HIGH",
				"RELATIONSHIP: SUPERSET_OF\nCONTRIBUTION_TYPE: N/A\nJUSTIFICATION: Broader scope in source.\nCONFIDENCE: HIGH",
				"RELATIONSHIP: SUPERSET_OF\nCONTRIBUTION_TYPE: N/A\nJUSTIFICATION: Target is a subset of source.\nCONFIDENCE: MEDIUM",
			}
			models := []string{"model-a", "model-b", "model-c"}

			votes := make(map[string]*relationship.Vote)
			for i, raw := range responses {
				v := relationship.ParseResponse(raw)
				v.Model = models[i]
				v.VoteKey = models[i]
				votes[v.VoteKey] = v
			}

			consensus := relationship.ComputeConsensus(votes)

			Expect(consensus.Relationship).To(Equal(relationship.RelSupersetOf))
			Expect(consensus.Unanimous).To(BeTrue())
			Expect(consensus.ConfidenceFraction).To(Equal(1.0))
			Expect(consensus.ValidVoteCount).To(Equal(3))
		})
	})

	Context("split vote produces plurality winner with correct confidence", func() {
		It("selects CONTRIBUTES_TO with INTEGRAL_TO from 2-of-3 majority", func() {
			responses := []string{
				"RELATIONSHIP: CONTRIBUTES_TO\nCONTRIBUTION_TYPE: INTEGRAL_TO\nJUSTIFICATION: Integral dependency.\nCONFIDENCE: HIGH",
				"RELATIONSHIP: CONTRIBUTES_TO\nCONTRIBUTION_TYPE: INTEGRAL_TO\nJUSTIFICATION: Cannot satisfy source without target.\nCONFIDENCE: HIGH",
				"RELATIONSHIP: PARTIAL\nCONTRIBUTION_TYPE: N/A\nJUSTIFICATION: Only tangential overlap.\nCONFIDENCE: MEDIUM",
			}
			models := []string{"model-a", "model-b", "model-c"}

			votes := make(map[string]*relationship.Vote)
			for i, raw := range responses {
				v := relationship.ParseResponse(raw)
				v.Model = models[i]
				v.VoteKey = models[i]
				votes[v.VoteKey] = v
			}

			consensus := relationship.ComputeConsensus(votes)

			Expect(consensus.Relationship).To(Equal(relationship.RelContributesTo))
			Expect(consensus.ContributionType).To(Equal(relationship.ContribIntegralTo))
			Expect(consensus.ConfidenceFraction).To(BeNumerically("~", 0.667, 0.001))
			Expect(consensus.Unanimous).To(BeFalse())
		})
	})

	Context("mixed valid and garbage responses", func() {
		It("excludes garbage from valid votes and selects plurality winner", func() {
			responses := []string{
				"RELATIONSHIP: EQUIVALENT\nCONTRIBUTION_TYPE: N/A\nJUSTIFICATION: Same scope.\nCONFIDENCE: HIGH",
				"RELATIONSHIP: EQUIVALENT\nCONTRIBUTION_TYPE: N/A\nJUSTIFICATION: Identical intent.\nCONFIDENCE: HIGH",
				"I don't know how to classify this relationship.",
				"RELATIONSHIP: NO_RELATIONSHIP\nCONTRIBUTION_TYPE: N/A\nJUSTIFICATION: Different domains.\nCONFIDENCE: LOW",
			}
			models := []string{"model-a", "model-b", "model-c", "model-d"}

			votes := make(map[string]*relationship.Vote)
			for i, raw := range responses {
				v := relationship.ParseResponse(raw)
				v.Model = models[i]
				v.VoteKey = models[i]
				votes[v.VoteKey] = v
			}

			consensus := relationship.ComputeConsensus(votes)

			Expect(consensus.Relationship).To(Equal(relationship.RelEquivalent))
			Expect(consensus.ValidVoteCount).To(Equal(3))
			Expect(consensus.AllVotes).To(ContainElement("PARSE_FAIL"))
		})
	})
})

var _ = Describe("Full Pipeline Flow", func() {
	It("candidates through graph edges", func() {
		ctx := testspecs.SetupTenantContext("test-tenant")

		// Setup candidates.
		candidates := &mockCandidateProvider{
			pairs: []relationship.CandidatePair{
				{SourceControlID: "AC-1", TargetControlID: "IT-3.2", SimilarityScore: 87.3},
				{SourceControlID: "AC-2", TargetControlID: "IT-4.1", SimilarityScore: 72.1},
			},
		}

		// Setup prompt registry with realistic templates matching relationship.yaml.
		prompts := &mockPromptRegistry{
			spec: &prompt.PromptSpec{
				Name:    "relationship",
				Version: "1.0.0",
				Templates: prompt.TemplateSet{
					System: `You are a compliance analyst classifying the relationship between two
requirements from different frameworks.

${classification_guidance}

${relationship_definitions}

Respond ONLY in this exact format:
RELATIONSHIP: <EQUIVALENT|SUPERSET_OF|SUBSET_OF|CONTRIBUTES_TO|COMPLEMENTS|PARTIAL|CONFLICTS_WITH|NO_RELATIONSHIP>
CONTRIBUTION_TYPE: <INTEGRAL_TO|EXAMPLE_OF|N/A>  (only when RELATIONSHIP is CONTRIBUTES_TO; otherwise N/A)
JUSTIFICATION: <one to two sentences explaining your reasoning, including subject domains identified>
CONFIDENCE: <HIGH|MEDIUM|LOW>
`,
					User: `SOURCE FRAMEWORK: ${source_framework}
TARGET FRAMEWORK: ${target_framework}

${few_shot_examples}

SOURCE REQUIREMENT:
ID: ${source_id} | Framework: ${source_framework} | Type: ${source_type} | Level: ${source_level} | Control Domain: "${source_ancestor}"
<requirement>
${source_text}
</requirement>

TARGET REQUIREMENT:
ID: ${target_id} | Framework: ${target_framework} | Type: ${target_type} | Level: ${target_level} | Control Domain: "${target_ancestor}"
<requirement>
${target_text}
</requirement>

Classify the NIST IR 8477 relationship FROM the source TO the target.
`,
				},
			},
		}

		// Config: 2 models, 1 sample each.
		cfg := config.RelationshipConfig{
			Enabled:             true,
			Models:              []string{"qwen3:8b", "mistral:7b"},
			TopK:                20,
			MaxSourceChars:      1500,
			MaxTargetChars:      800,
			MaxTokens:           300,
			SamplesPerModel:     1,
			SamplingTemperature: 0.0,
			ActionableTypes:     []string{"EQUIVALENT", "SUPERSET_OF"},
		}

		a := relationship.New(nil, prompts, candidates, cfg)

		// Step 1: GenerateWork — should produce 4 tasks (2 pairs x 2 models x 1 sample).
		control := &pb.Control{ControlId: "AC-1", Statement: "access control policy"}
		tasks, err := a.GenerateWork(ctx, control, analyzer.AnalyzerConfig{
			Parameters: map[string]string{"job_id": "job-1"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(tasks).To(HaveLen(4))

		// Step 2-4: Parse simulated LLM responses and group votes by pair.
		pairVotes := make(map[string]map[string]*relationship.Vote)
		var taskResults []analyzer.TaskResult

		for _, task := range tasks {
			payload, ok := task.Payload.(*structpb.Struct)
			Expect(ok).To(BeTrue())

			srcID := payload.Fields["source_control_id"].GetStringValue()
			tgtID := payload.Fields["target_control_id"].GetStringValue()
			model := payload.Fields["model"].GetStringValue()

			// Simulate LLM response — all respond SUPERSET_OF.
			rawResponse := fmt.Sprintf(
				"RELATIONSHIP: SUPERSET_OF\nCONTRIBUTION_TYPE: N/A\nJUSTIFICATION: Source %s encompasses target %s.\nCONFIDENCE: HIGH",
				srcID, tgtID,
			)

			vote := relationship.ParseResponse(rawResponse)
			vote.Model = model
			vote.VoteKey = model

			pairKey := srcID + "--" + tgtID
			if pairVotes[pairKey] == nil {
				pairVotes[pairKey] = make(map[string]*relationship.Vote)
			}
			pairVotes[pairKey][vote.VoteKey] = vote

			// Build TaskResult for Aggregate.
			taskResults = append(taskResults, analyzer.TaskResult{
				TaskID:   task.TaskID,
				TaskType: "relationship",
				Result: &pb.AnalysisResult{
					ResultId:   task.TaskID,
					ResultType: "relationship",
					Attributes: map[string]string{
						"source_control_id": srcID,
						"target_control_id": tgtID,
						"relationship":      vote.Relationship.String(),
						"confidence":        vote.Confidence.String(),
						"parse_status":      vote.ParseStatus.String(),
					},
					Confidence: 0.9,
				},
			})
		}

		// Step 5: Compute consensus and store pair results.
		store := newMockStorage()
		for pairKey, votes := range pairVotes {
			consensus := relationship.ComputeConsensus(votes)
			parts := strings.SplitN(pairKey, "--", 2)
			Expect(parts).To(HaveLen(2))

			pair := relationship.PairResult{
				SourceControlID: parts[0],
				TargetControlID: parts[1],
				Votes:           votes,
				Consensus:       consensus,
				SimilarityScore: 87.3,
			}

			data, err := json.Marshal(pair)
			Expect(err).NotTo(HaveOccurred())

			storageKey := fmt.Sprintf("test-tenant/analysis/relationship/job-1/%s.json", pairKey)
			store.data[storageKey] = data
		}

		// Step 7-8: Create GraphMaterializer and materialize.
		graph := &mockGraphDB{}
		mat := relationship.NewGraphMaterializer(graph, store, config.RelationshipConfig{})
		err = mat.Materialize(ctx, "test-tenant", "job-1")
		Expect(err).NotTo(HaveOccurred())

		// Step 9: Assert graph edges.
		Expect(graph.captured).To(HaveLen(2))
		for _, c := range graph.captured {
			Expect(c.Edge.Label).To(Equal("SEMANTIC_MATCH"))
			Expect(c.Edge.DeterminationType).To(Equal("llm_panel"))
			Expect(c.Edge.Properties["relationship_type"]).To(Equal("SUPERSET_OF"))
		}

		// Verify both pairs are represented.
		edgePairs := make(map[string]bool)
		for _, c := range graph.captured {
			edgePairs[c.SourceID+"--"+c.TargetID] = true
		}
		Expect(edgePairs).To(HaveKey("AC-1--IT-3.2"))
		Expect(edgePairs).To(HaveKey("AC-2--IT-4.1"))

		// Step 10: Call Aggregate and verify metadata.
		output, err := a.Aggregate(ctx, taskResults)
		Expect(err).NotTo(HaveOccurred())
		Expect(output.AnalyzerName).To(Equal("relationship"))
		Expect(output.Metadata["total_count"]).To(Equal("4"))
		Expect(output.Metadata["error_count"]).To(Equal("0"))
		Expect(output.Metadata["prompt_name"]).To(Equal("relationship"))
	})
})

var _ = Describe("Aggregate with Realistic Data", func() {
	It("processes multi-pair multi-model results with errors", func() {
		ctx := testspecs.SetupTenantContext("test-tenant")

		candidates := &mockCandidateProvider{}
		prompts := &mockPromptRegistry{
			spec: &prompt.PromptSpec{
				Name:    "relationship",
				Version: "1.0.0",
				Templates: prompt.TemplateSet{
					System: "System: ${classification_guidance} ${relationship_definitions}",
					User:   "User: ${source_text} ${target_text} ${source_id} ${target_id} ${source_framework} ${target_framework} ${source_type} ${source_level} ${source_ancestor} ${target_type} ${target_level} ${target_ancestor} ${few_shot_examples}",
				},
			},
		}

		cfg := config.RelationshipConfig{
			Enabled:         true,
			Models:          []string{"qwen3:8b", "mistral:7b"},
			SamplesPerModel: 1,
		}
		a := relationship.New(nil, prompts, candidates, cfg)

		// Build 6 results: 2 pairs x 3 models, 2 with errors.
		results := []analyzer.TaskResult{
			{
				TaskID:   "relationship-AC-1--IT-3.2-qwen3:8b-s0",
				TaskType: "relationship",
				Result: &pb.AnalysisResult{
					ResultId: "relationship-AC-1--IT-3.2-qwen3:8b-s0", ResultType: "relationship",
					Attributes: map[string]string{
						"source_control_id": "AC-1", "target_control_id": "IT-3.2",
						"relationship": "SUPERSET_OF", "confidence": "HIGH", "parse_status": "OK",
					},
					Confidence: 0.9,
				},
			},
			{
				TaskID:   "relationship-AC-1--IT-3.2-mistral:7b-s0",
				TaskType: "relationship",
				Result: &pb.AnalysisResult{
					ResultId: "relationship-AC-1--IT-3.2-mistral:7b-s0", ResultType: "relationship",
					Attributes: map[string]string{
						"source_control_id": "AC-1", "target_control_id": "IT-3.2",
						"relationship": "EQUIVALENT", "confidence": "MEDIUM", "parse_status": "OK",
					},
					Confidence: 0.8,
				},
			},
			{
				TaskID:   "relationship-AC-1--IT-3.2-llama3:8b-s0",
				TaskType: "relationship",
				Error:    fmt.Errorf("LLM timeout after 30s"),
			},
			{
				TaskID:   "relationship-AC-2--IT-4.1-qwen3:8b-s0",
				TaskType: "relationship",
				Result: &pb.AnalysisResult{
					ResultId: "relationship-AC-2--IT-4.1-qwen3:8b-s0", ResultType: "relationship",
					Attributes: map[string]string{
						"source_control_id": "AC-2", "target_control_id": "IT-4.1",
						"relationship": "NO_RELATIONSHIP", "confidence": "HIGH", "parse_status": "OK",
					},
					Confidence: 0.95,
				},
			},
			{
				TaskID:   "relationship-AC-2--IT-4.1-mistral:7b-s0",
				TaskType: "relationship",
				Error:    fmt.Errorf("connection refused"),
			},
			{
				TaskID:   "relationship-AC-2--IT-4.1-llama3:8b-s0",
				TaskType: "relationship",
				Result: &pb.AnalysisResult{
					ResultId: "relationship-AC-2--IT-4.1-llama3:8b-s0", ResultType: "relationship",
					Attributes: map[string]string{
						"source_control_id": "AC-2", "target_control_id": "IT-4.1",
						"relationship": "NO_RELATIONSHIP", "confidence": "MEDIUM", "parse_status": "OK",
					},
					Confidence: 0.7,
				},
			},
		}

		output, err := a.Aggregate(ctx, results)
		Expect(err).NotTo(HaveOccurred())
		Expect(output.AnalyzerName).To(Equal("relationship"))
		Expect(output.Metadata["total_count"]).To(Equal("6"))
		Expect(output.Metadata["error_count"]).To(Equal("2"))
		Expect(output.Metadata["prompt_name"]).To(Equal("relationship"))
	})
})

var _ = Describe("Prompt Template Validation", func() {
	var (
		reg prompt.Registry
		ctx context.Context
	)

	BeforeEach(func() {
		ctx = testspecs.SetupTenantContext("test-tenant")
		var err error
		reg, err = prompt.NewRegistry(config.PromptConfig{
			Layers: config.PromptLayerConfig{Enabled: true},
		})
		Expect(err).NotTo(HaveOccurred())
	})

	Context("real relationship.yaml templates accept all required placeholders", func() {
		It("renders system and user templates without errors", func() {
			spec, err := reg.Resolve(ctx, "relationship")
			Expect(err).NotTo(HaveOccurred())

			vars := map[string]string{
				"classification_guidance":  "Focus on semantic intent...",
				"relationship_definitions": "Relationship Definitions: EQUIVALENT...",
				"few_shot_examples":        "Example 1: ...",
				"source_id":                "AC-1",
				"target_id":                "IT-3.2",
				"source_text":              "Implement access control policy.",
				"target_text":              "Restrict system access to authorised users.",
				"source_framework":         "SOX-IT",
				"target_framework":         "NIST-800-53",
				"source_type":              "Technical",
				"source_level":             "Tactical",
				"source_ancestor":          "Access Control",
				"target_type":              "Technical",
				"target_level":             "Tactical",
				"target_ancestor":          "Access Control",
			}

			systemRendered, err := prompt.SubstitutePlaceholders(spec.Templates.System, vars)
			Expect(err).NotTo(HaveOccurred())

			userRendered, err := prompt.SubstitutePlaceholders(spec.Templates.User, vars)
			Expect(err).NotTo(HaveOccurred())

			Expect(systemRendered).To(ContainSubstring("Focus on semantic intent..."))
			Expect(userRendered).To(ContainSubstring("AC-1"))
			Expect(userRendered).To(ContainSubstring("IT-3.2"))
		})
	})

	Context("real relationship.yaml has 8 few-shot examples", func() {
		It("contains exactly 8 examples", func() {
			spec, err := reg.Resolve(ctx, "relationship")
			Expect(err).NotTo(HaveOccurred())
			Expect(spec.FewShot).To(HaveLen(8))
		})
	})

	Context("few-shot examples cover relationship types", func() {
		It("covers all actionable relationship types (NO_RELATIONSHIP intentionally omitted)", func() {
			spec, err := reg.Resolve(ctx, "relationship")
			Expect(err).NotTo(HaveOccurred())

			typeSet := make(map[string]bool)
			for _, example := range spec.FewShot {
				vote := relationship.ParseResponse(example.Output)
				if vote.ParseStatus == relationship.ParseOK {
					typeSet[vote.Relationship.String()] = true
				}
			}

			// The few-shot examples cover 7 of 8 types. NO_RELATIONSHIP is
			// intentionally omitted — it is the default/fallback and does not
			// need a positive example to teach the model.
			expectedTypes := []string{
				"EQUIVALENT", "SUPERSET_OF", "SUBSET_OF", "CONTRIBUTES_TO",
				"COMPLEMENTS", "PARTIAL", "CONFLICTS_WITH",
			}
			for _, rt := range expectedTypes {
				Expect(typeSet).To(HaveKey(rt),
					fmt.Sprintf("missing relationship type in few-shot examples: %s", rt))
			}
			Expect(typeSet).To(HaveLen(7))
		})
	})
})
