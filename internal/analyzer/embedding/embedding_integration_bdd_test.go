//go:build integration

package embedding_test

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"os"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/analyzer/embedding"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/vectordb"
	"google.golang.org/protobuf/types/known/structpb"
)

var _ = Describe("Embedding Infrastructure Integration", Ordered, func() {
	var (
		sqlDB   *sql.DB
		vectors *vectordb.PgVectorStore
		store   storage.Provider
	)

	BeforeAll(func() {
		dsn := os.Getenv("TEST_DATABASE_DSN")
		if dsn == "" {
			Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
		}

		ctx := context.Background()

		// Run migrations so the embeddings table and pgvector extension exist.
		migrator, err := db.NewMigrator(dsn)
		Expect(err).NotTo(HaveOccurred(), "failed to create migrator")
		Expect(migrator.Up(ctx)).To(Succeed(), "failed to run migrations")
		Expect(migrator.Close()).To(Succeed(), "failed to close migrator")

		// Insert FK parent rows required by the embeddings table.
		adminDB, err := sql.Open("pgx", dsn)
		Expect(err).NotTo(HaveOccurred(), "failed to open admin connection")
		defer adminDB.Close()

		_, err = adminDB.ExecContext(ctx,
			`INSERT INTO tenants (tenant_id, display_name) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			"test-tenant", "Integration Test Tenant")
		Expect(err).NotTo(HaveOccurred(), "failed to insert tenant")

		_, err = adminDB.ExecContext(ctx,
			`INSERT INTO catalogs (catalog_id, tenant_id, name, version, source_type, object_path)
			 VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING`,
			"nist-800-53", "test-tenant", "NIST 800-53", "rev5", "oscal", "test-fixture")
		Expect(err).NotTo(HaveOccurred(), "failed to insert catalog")

		// Open connection for the test suite.
		sqlDB, err = sql.Open("pgx", dsn)
		Expect(err).NotTo(HaveOccurred(), "failed to open test connection")

		vectors, err = vectordb.NewPgVectorStore(sqlDB)
		Expect(err).NotTo(HaveOccurred(), "failed to create PgVectorStore")
	})

	BeforeEach(func() {
		dsn := os.Getenv("TEST_DATABASE_DSN")
		if dsn == "" {
			Skip("TEST_DATABASE_DSN not set")
		}

		var err error
		store, err = storage.NewLocal(GinkgoT().TempDir(), "test-tenant")
		Expect(err).NotTo(HaveOccurred(), "failed to create local storage")
	})

	AfterEach(func() {
		if store != nil {
			_ = store.Close()
		}
	})

	AfterAll(func() {
		if sqlDB != nil {
			// Clean up test embeddings before closing.
			_, _ = sqlDB.Exec(`DELETE FROM embeddings WHERE tenant_id = $1`, "test-tenant")
			sqlDB.Close()
		}
	})

	It("generates embedding tasks and stores vectors in real pgvector", func() {
		ctx := testspecs.SetupTenantContext("test-tenant")

		// Vectors must be 2000-dimensional to match the embeddings table schema
		// (vector(2000)). Seed values occupy leading positions; rest are zeros.
		dim := pgvectorDim
		mockVectors := map[string][]float32{
			"nist-800-53/AC-1": normalizeVec(testVec(1, 0, 0, 0, 0, 0, 0, 0)),
			"nist-800-53/AC-2": normalizeVec(testVec(0.9, 0.1, 0, 0, 0, 0, 0, 0)),
			"nist-800-53/SI-3": normalizeVec(testVec(0, 0, 0, 0, 1, 0, 0, 0)),
		}

		mock := &cannedEmbedClient{
			vectors: mockVectors,
			dim:     dim,
		}

		embCfg := config.EmbeddingConfig{
			Enabled:   true,
			Models:    []string{"test-embed"},
			MaxChars:  2000,
			BatchSize: 10,
		}
		relCfg := config.RelationshipConfig{TopK: 5}

		a := embedding.New(mock, vectors, store, embCfg, relCfg)

		controls := []*pb.Control{
			{
				ControlId: "nist-800-53/AC-1", CatalogId: "nist-800-53",
				Identifier: "AC-1", Title: "Access Control Policy",
				Statement: "Develop access control policy.",
				Parts:     map[string]string{"class": "requirement"},
			},
			{
				ControlId: "nist-800-53/AC-2", CatalogId: "nist-800-53",
				Identifier: "AC-2", Title: "Account Management",
				Statement: "Manage system accounts.",
				Parts:     map[string]string{"class": "requirement"},
			},
			{
				ControlId: "nist-800-53/SI-3", CatalogId: "nist-800-53",
				Identifier: "SI-3", Title: "Malicious Code Protection",
				Statement: "Implement malicious code protection.",
				Parts:     map[string]string{"class": "requirement"},
			},
		}

		var allResults []analyzer.TaskResult
		for _, ctrl := range controls {
			tasks, err := a.GenerateWork(ctx, ctrl, analyzer.AnalyzerConfig{})
			Expect(err).NotTo(HaveOccurred())

			for _, task := range tasks {
				payload, ok := task.Payload.(*structpb.Struct)
				if !ok {
					// Section — pre-built result, no embedding needed.
					allResults = append(allResults, analyzer.TaskResult{
						TaskID: task.TaskID, TaskType: "embedding",
						Result: task.Payload.(*pb.AnalysisResult), Duration: time.Millisecond,
					})
					continue
				}

				fields := payload.GetFields()
				controlID := fields["control_id"].GetStringValue()

				// Simulate worker: look up the canned vector for this control.
				vec, exists := mockVectors[controlID]
				if !exists {
					vec = make([]float32, dim)
				}

				// Store in real pgvector.
				err = vectors.StoreEmbedding(ctx, "test-tenant", vectordb.Embedding{
					CatalogID: "nist-800-53",
					ControlID: controlID,
					Model:     "test-embed",
					Vector:    vec,
				})
				Expect(err).NotTo(HaveOccurred(), "failed to store embedding for %s", controlID)

				allResults = append(allResults, analyzer.TaskResult{
					TaskID:   task.TaskID,
					TaskType: "embedding",
					Result: &pb.AnalysisResult{
						ResultId:   task.TaskID,
						ResultType: "embedding",
						Attributes: map[string]string{
							"control_id": controlID,
							"model":      "test-embed",
							"dimensions": "8",
						},
						Confidence: 1.0,
					},
					Duration: 5 * time.Millisecond,
				})
			}
		}

		// Verify vectors were stored by querying pgvector for similarity.
		results, err := vectors.FindSimilar(ctx, "test-tenant", vectordb.FindSimilarQuery{
			CatalogID: "nist-800-53",
			Model:     "test-embed",
			Vector:    mockVectors["nist-800-53/AC-1"],
			Limit:     3,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(results).To(HaveLen(3))

		// AC-1 should be most similar to itself, then AC-2, then SI-3.
		Expect(results[0].ControlID).To(Equal("nist-800-53/AC-1"))
		Expect(results[1].ControlID).To(Equal("nist-800-53/AC-2"))

		// Aggregate results.
		output, err := a.Aggregate(ctx, allResults)
		Expect(err).NotTo(HaveOccurred())
		Expect(output.AnalyzerName).To(Equal("embedding"))
		Expect(output.Metadata["embedded_count"]).To(Equal("3"))
		Expect(output.Metadata["error_count"]).To(Equal("0"))
	})
})

// cannedEmbedClient returns pre-defined embedding vectors keyed by control ID.
// It matches control IDs by searching for them within the prepared text,
// since prepareText() prepends ancestor context like "[Title] statement".
type cannedEmbedClient struct {
	vectors map[string][]float32
	dim     int
}

func (c *cannedEmbedClient) Complete(_ context.Context, _ *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
	return nil, errors.New("embedding client does not support completions")
}

func (c *cannedEmbedClient) Embed(_ context.Context, req *llmclient.EmbeddingRequest) (*llmclient.EmbeddingResponse, error) {
	var data []llmclient.EmbeddingData
	for i, text := range req.Input {
		vec := make([]float32, c.dim)
		for id, v := range c.vectors {
			if strings.Contains(text, id) {
				vec = v
				break
			}
		}
		data = append(data, llmclient.EmbeddingData{Index: i, Embedding: vec})
	}
	return &llmclient.EmbeddingResponse{
		Data:  data,
		Model: "test-embed",
		Usage: llmclient.EmbeddingUsage{PromptTokens: len(req.Input) * 10, TotalTokens: len(req.Input) * 10},
	}, nil
}

func (c *cannedEmbedClient) Health(_ context.Context) error { return nil }
func (c *cannedEmbedClient) Close() error                   { return nil }

// pgvectorDim matches the embeddings table column: vector(2000).
const pgvectorDim = 2000

// testVec returns a 2000-dimensional vector with the given seed values
// in the leading positions and zeros elsewhere.
func testVec(seed ...float32) []float32 {
	v := make([]float32, pgvectorDim)
	copy(v, seed)
	return v
}

// normalizeVec returns a unit-length copy of the input vector.
func normalizeVec(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	norm := math.Sqrt(sum)
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(float64(x) / norm)
	}
	return out
}
