//go:build integration_llm

package embedding_test

import (
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/structpb"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/analyzer/embedding"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/storage"
)

var _ = Describe("Embedding LLM Integration", func() {
	var (
		client llmclient.Client
	)

	BeforeEach(func() {
		litellmURL := os.Getenv("TEST_LITELLM_URL")
		embedModel := os.Getenv("TEST_LLM_EMBED_MODEL")
		if litellmURL == "" || embedModel == "" {
			Skip("TEST_LITELLM_URL or TEST_LLM_EMBED_MODEL not set — run: task test:integration:llm")
		}

		var err error
		client, err = llmclient.NewClient(config.LLMConfig{
			GatewayURL:     litellmURL,
			GatewayMode:    true,
			EmbeddingModel: embedModel,
			MaxRetries:     0,
			Timeout:        120,
		})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { client.Close() })
	})

	It("generates real embeddings from Ollama and validates vector dimensions", func() {
		ctx := testspecs.SetupTenantContext("test-tenant")

		embedModel := os.Getenv("TEST_LLM_EMBED_MODEL")

		store, err := storage.NewLocal(GinkgoT().TempDir(), "test-tenant")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = store.Close() })

		embCfg := config.EmbeddingConfig{
			Enabled:   true,
			Models:    []string{embedModel},
			MaxChars:  2000,
			BatchSize: 10,
		}
		relCfg := config.RelationshipConfig{TopK: 5}

		// Use nil vectordb — GenerateWork does not call it.
		a := embedding.New(client, nil, store, embCfg, relCfg)

		controls := []*pb.Control{
			{
				ControlId:  "nist-800-53/AC-2",
				CatalogId:  "nist-800-53",
				Identifier: "AC-2",
				Title:      "Account Management",
				Statement:  "Manage system accounts including establishing, activating, and removing.",
				Parts:      map[string]string{"class": "requirement"},
			},
			{
				ControlId:  "nist-800-53/SI-3",
				CatalogId:  "nist-800-53",
				Identifier: "SI-3",
				Title:      "Malicious Code Protection",
				Statement:  "Implement malicious code protection at entry and exit points.",
				Parts:      map[string]string{"class": "requirement"},
			},
		}

		var dimensions []int
		var allResults []analyzer.TaskResult

		for _, ctrl := range controls {
			tasks, err := a.GenerateWork(ctx, ctrl, analyzer.AnalyzerConfig{})
			Expect(err).NotTo(HaveOccurred())

			for _, task := range tasks {
				payload, ok := task.Payload.(*structpb.Struct)
				if !ok {
					continue // Skip section pre-built results.
				}

				fields := payload.GetFields()
				text := fields["text"].GetStringValue()
				Expect(text).NotTo(BeEmpty(), "prepared text should not be empty")

				// Call real embedding API.
				resp, err := client.Embed(ctx, &llmclient.EmbeddingRequest{
					Model:    embedModel,
					Input:    []string{text},
					TenantID: "test-tenant",
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.Data).To(HaveLen(1))

				vec := resp.Data[0].Embedding
				Expect(vec).NotTo(BeEmpty(), "embedding vector should have dimensions")
				dimensions = append(dimensions, len(vec))

				GinkgoWriter.Printf("Control %s: %d dimensions\n",
					fields["control_id"].GetStringValue(), len(vec))

				allResults = append(allResults, analyzer.TaskResult{
					TaskID:   task.TaskID,
					TaskType: "embedding",
					Result: &pb.AnalysisResult{
						ResultId:   task.TaskID,
						ResultType: "embedding",
						Attributes: map[string]string{
							"control_id": fields["control_id"].GetStringValue(),
							"model":      embedModel,
						},
						Confidence: 1.0,
					},
					Duration: 100 * time.Millisecond,
				})
			}
		}

		// All vectors should have the same dimensionality.
		Expect(dimensions).To(HaveLen(2))
		Expect(dimensions[0]).To(Equal(dimensions[1]),
			"all embeddings from the same model should have identical dimensions")
		Expect(dimensions[0]).To(BeNumerically(">", 0))

		// Aggregate.
		output, err := a.Aggregate(ctx, allResults)
		Expect(err).NotTo(HaveOccurred())
		Expect(output.AnalyzerName).To(Equal("embedding"))
		GinkgoWriter.Printf("Embedding aggregate: %v\n", output.Metadata)
	})
})
