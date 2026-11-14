//go:build integration

package relationship_test

import (
	"bytes"
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
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/prompt"
	"github.com/complytime-labs/crosscodex/pkg/storage/storagetest"
	"google.golang.org/protobuf/types/known/structpb"
)

var _ = Describe("Parser to Consensus Integration", func() {
	Context("multi-model panel with unanimous SUPERSET_OF", func() {
		It("produces unanimous consensus with full confidence", func() {
			responses := []string{
				"RELATIONSHIP: SUPERSET_OF\nCONTRIBUTION_TYPE: N/A\nJUSTIFICATION: Source encompasses target.\nSANITY_CHECK: PASS\nCONFIDENCE: HIGH",
				"RELATIONSHIP: SUPERSET_OF\nCONTRIBUTION_TYPE: N/A\nJUSTIFICATION: Broader scope in source.\nSANITY_CHECK: PASS\nCONFIDENCE: HIGH",
				"RELATIONSHIP: SUPERSET_OF\nCONTRIBUTION_TYPE: N/A\nJUSTIFICATION: Target is a subset of source.\nSANITY_CHECK: PASS\nCONFIDENCE: MEDIUM",
			}
			models := []string{"model-a", "model-b", "model-c"}

			votes := make(map[string]*relationship.Vote)
			for i, raw := range responses {
				v := relationship.ParseResponse(raw)
				v.Model = models[i]
				v.VoteKey = models[i]
				votes[v.VoteKey] = v
			}

			consensus := relationship.ComputeConsensus(votes, defaultConfidenceWeights)

			Expect(consensus.Relationship).To(Equal(relationship.RelSupersetOf))
			Expect(consensus.Unanimous).To(BeTrue())
			Expect(consensus.AgreementFraction).To(Equal(1.0))
			// (HIGH 1.0 + HIGH 1.0 + MEDIUM 0.67) / 3 under the default weights.
			Expect(consensus.ConfidenceFraction).To(Equal(0.89))
			Expect(consensus.ValidVoteCount).To(Equal(3))
		})
	})

	Context("split vote produces plurality winner with correct confidence", func() {
		It("selects CONTRIBUTES_TO with INTEGRAL_TO from 2-of-3 majority", func() {
			responses := []string{
				"RELATIONSHIP: CONTRIBUTES_TO\nCONTRIBUTION_TYPE: INTEGRAL_TO\nJUSTIFICATION: Integral dependency.\nSANITY_CHECK: PASS\nCONFIDENCE: HIGH",
				"RELATIONSHIP: CONTRIBUTES_TO\nCONTRIBUTION_TYPE: INTEGRAL_TO\nJUSTIFICATION: Cannot satisfy source without target.\nSANITY_CHECK: PASS\nCONFIDENCE: HIGH",
				"RELATIONSHIP: PARTIAL\nCONTRIBUTION_TYPE: N/A\nJUSTIFICATION: Only tangential overlap.\nSANITY_CHECK: PASS\nCONFIDENCE: MEDIUM",
			}
			models := []string{"model-a", "model-b", "model-c"}

			votes := make(map[string]*relationship.Vote)
			for i, raw := range responses {
				v := relationship.ParseResponse(raw)
				v.Model = models[i]
				v.VoteKey = models[i]
				votes[v.VoteKey] = v
			}

			consensus := relationship.ComputeConsensus(votes, defaultConfidenceWeights)

			Expect(consensus.Relationship).To(Equal(relationship.RelContributesTo))
			Expect(consensus.ContributionType).To(Equal(relationship.ContribIntegralTo))
			Expect(consensus.ConfidenceFraction).To(BeNumerically("~", 0.667, 0.001))
			Expect(consensus.Unanimous).To(BeFalse())
		})
	})

	Context("mixed valid and garbage responses", func() {
		It("excludes garbage from valid votes and selects plurality winner", func() {
			responses := []string{
				"RELATIONSHIP: EQUIVALENT\nCONTRIBUTION_TYPE: N/A\nJUSTIFICATION: Same scope.\nSANITY_CHECK: PASS\nCONFIDENCE: HIGH",
				"RELATIONSHIP: EQUIVALENT\nCONTRIBUTION_TYPE: N/A\nJUSTIFICATION: Identical intent.\nSANITY_CHECK: PASS\nCONFIDENCE: HIGH",
				"I don't know how to classify this relationship.",
				"RELATIONSHIP: NO_RELATIONSHIP\nCONTRIBUTION_TYPE: N/A\nJUSTIFICATION: Different domains.\nSANITY_CHECK: PASS\nCONFIDENCE: LOW",
			}
			models := []string{"model-a", "model-b", "model-c", "model-d"}

			votes := make(map[string]*relationship.Vote)
			for i, raw := range responses {
				v := relationship.ParseResponse(raw)
				v.Model = models[i]
				v.VoteKey = models[i]
				votes[v.VoteKey] = v
			}

			consensus := relationship.ComputeConsensus(votes, defaultConfidenceWeights)

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

		// The shipped relationship prompt, so this spec cannot drift from
		// relationship.yaml.
		prompts, err := prompt.NewRegistry(config.PromptConfig{Layers: config.PromptLayerConfig{Enabled: true}})
		Expect(err).NotTo(HaveOccurred())

		// Config: 2 models, 1 sample each.
		cfg := config.RelationshipConfig{
			Enabled:             true,
			Models:              []string{"qwen3:8b", "mistral:7b"},
			MaxSourceChars:      1500,
			MaxTargetChars:      800,
			MaxTokens:           300,
			SamplesPerModel:     1,
			SamplingTemperature: 0.0,
			ActionableTypes:     []string{"EQUIVALENT", "SUPERSET_OF"},
		}

		a := relationship.New(nil, prompts, candidates, cfg)

		// Step 1: GenerateWork — should produce 4 tasks (2 pairs x 2 models x 1 sample).
		tasks, err := a.GenerateWork(ctx, &pb.Control{}, analyzer.AnalyzerConfig{
			Parameters: pairParams("job-1", "AC-1", "AC-2", "IT-3.2", "IT-4.1"),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(tasks).To(HaveLen(4))
		user := tasks[0].Payload.(*structpb.Struct).Fields["messages"].GetListValue().GetValues()[1].GetStructValue().Fields["content"].GetStringValue()
		Expect(user).To(ContainSubstring("<source_metadata>\nID: AC-1\n"))

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
				"RELATIONSHIP: SUPERSET_OF\nCONTRIBUTION_TYPE: N/A\nJUSTIFICATION: Source %s encompasses target %s.\nSANITY_CHECK: PASS\nCONFIDENCE: HIGH",
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
		store := &storagetest.Fake{}
		for pairKey, votes := range pairVotes {
			consensus := relationship.ComputeConsensus(votes, defaultConfidenceWeights)
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
			Expect(store.Put(ctx, storageKey, bytes.NewReader(data))).To(Succeed())
		}

		// Step 7-8: Create GraphMaterializer and materialize.
		graph := newControlGraph(ctx, "test-tenant", "AC-1", "IT-3.2", "AC-2", "IT-4.1")
		mat := relationship.NewGraphMaterializer(graph, store, config.RelationshipConfig{})
		err = mat.Materialize(ctx, "test-tenant", "job-1")
		Expect(err).NotTo(HaveOccurred())

		// Step 9: Assert graph edges.
		rels, err := graph.QueryRelationships(ctx, "test-tenant", graphdb.RelationshipQuery{EdgeLabel: "SEMANTIC_MATCH"})
		Expect(err).NotTo(HaveOccurred())
		Expect(rels).To(HaveLen(2))
		for _, r := range rels {
			Expect(r.Edge.Label).To(Equal("SEMANTIC_MATCH"))
			Expect(r.Edge.DeterminationType).To(Equal("llm_panel"))
			Expect(r.Edge.Properties["relationship_type"]).To(Equal("SUPERSET_OF"))
		}

		// Verify both pairs are represented.
		edgePairs := make(map[string]bool)
		for _, r := range rels {
			edgePairs[r.Source.ID+"--"+r.Target.ID] = true
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
		prompts := specRegistry(&prompt.PromptSpec{
			Name:    "relationship",
			Version: "1.0.0",
			Templates: prompt.TemplateSet{
				System: "System: ${classification_guidance} ${relationship_definitions}",
				User:   "User: ${source_text} ${target_text} ${source_id} ${target_id} ${source_framework} ${target_framework} ${source_type} ${source_level} ${source_context} ${source_title} ${target_type} ${target_level} ${target_context} ${target_title} ${few_shot_examples}",
			},
		})

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
				"source_context":           "Access Control",
				"source_title":             "T-source",
				"target_type":              "Technical",
				"target_level":             "Tactical",
				"target_context":           "Access Control",
				"target_title":             "T-target",
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
