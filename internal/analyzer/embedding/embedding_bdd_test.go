package embedding_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"unicode/utf8"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/analyzer/embedding"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
	"github.com/complytime-labs/crosscodex/pkg/vectordb"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestEmbeddingBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Embedding Analyzer BDD Suite")
}

var restoreLogs func()

var _ = BeforeSuite(func() {
	restoreLogs = testspecs.RedirectLogsToGinkgo()
})

var _ = AfterSuite(func() {
	restoreLogs()
})

// --- Mock LLM client for embedding tests ---
type mockLLMClient struct {
	embedFunc func(ctx context.Context, req *llmclient.EmbeddingRequest) (*llmclient.EmbeddingResponse, error)
}

func (m *mockLLMClient) Complete(_ context.Context, _ *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *mockLLMClient) Embed(ctx context.Context, req *llmclient.EmbeddingRequest) (*llmclient.EmbeddingResponse, error) {
	if m.embedFunc != nil {
		return m.embedFunc(ctx, req)
	}
	data := make([]llmclient.EmbeddingData, len(req.Input))
	for i := range req.Input {
		data[i] = llmclient.EmbeddingData{
			Index:     i,
			Embedding: []float32{0.1, 0.2, 0.3},
		}
	}
	return &llmclient.EmbeddingResponse{Data: data, Model: req.Model}, nil
}

func (m *mockLLMClient) Health(_ context.Context) error { return nil }
func (m *mockLLMClient) Close() error                   { return nil }

// --- Mock VectorDB for embedding tests ---
type mockVectorDB struct {
	storedBatches [][]vectordb.Embedding
	storedTenants []string
}

func (m *mockVectorDB) StoreEmbedding(_ context.Context, _ string, _ vectordb.Embedding) error {
	return nil
}

func (m *mockVectorDB) StoreBatch(_ context.Context, tenantID string, batch []vectordb.Embedding) error {
	m.storedBatches = append(m.storedBatches, batch)
	m.storedTenants = append(m.storedTenants, tenantID)
	return nil
}

func (m *mockVectorDB) FindSimilar(_ context.Context, _ string, _ vectordb.FindSimilarQuery) ([]vectordb.SimilarityResult, error) {
	return nil, nil
}

func (m *mockVectorDB) DeleteByModel(_ context.Context, _, _, _ string) error {
	return nil
}

// --- Mock storage for embedding tests ---
type mockStorage struct {
	stored map[string][]byte
}

func newMockStorage() *mockStorage {
	return &mockStorage{stored: make(map[string][]byte)}
}

func (m *mockStorage) Get(_ context.Context, _ string) (io.ReadCloser, error) {
	return nil, errors.New("not implemented")
}

func (m *mockStorage) Put(_ context.Context, key string, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.stored[key] = data
	return nil
}

func (m *mockStorage) Delete(_ context.Context, _ string) error { return nil }

func (m *mockStorage) List(_ context.Context, _ string) ([]storage.ObjectMetadata, error) {
	return nil, nil
}

func (m *mockStorage) Exists(_ context.Context, _ string) (bool, error) { return false, nil }

func (m *mockStorage) Stat(_ context.Context, _ string) (*storage.ObjectMetadata, error) {
	return nil, errors.New("not implemented")
}

func (m *mockStorage) Close() error { return nil }

var _ = Describe("Text Preparation", func() {
	Describe("cleanForEmbedding", func() {
		It("removes OSCAL template placeholders", func() {
			input := "The organization {{ insert: param, ac-1_prm_1 }} shall define policy."
			result := embedding.ExportCleanForEmbedding(input)
			Expect(result).NotTo(ContainSubstring("{{"))
			Expect(result).NotTo(ContainSubstring("}}"))
			Expect(result).To(ContainSubstring("The organization"))
			Expect(result).To(ContainSubstring("shall define policy."))
		})

		It("removes nested OSCAL template placeholders", func() {
			input := "Enforce {{ insert: param, ac-2_prm_1, selection }} controls."
			result := embedding.ExportCleanForEmbedding(input)
			Expect(result).NotTo(ContainSubstring("{{"))
			Expect(result).To(ContainSubstring("Enforce"))
			Expect(result).To(ContainSubstring("controls."))
		})

		It("removes VerDate PDF artifacts", func() {
			input := "Some text\nVerDate Sep 11 2014 10:47 Jan 20, 2023\nMore text"
			result := embedding.ExportCleanForEmbedding(input)
			Expect(result).NotTo(ContainSubstring("VerDate"))
			Expect(result).To(ContainSubstring("Some text"))
			Expect(result).To(ContainSubstring("More text"))
		})

		It("removes Jkt PDF artifacts", func() {
			input := "Text before\nJkt 099006 PO 00000\nText after"
			result := embedding.ExportCleanForEmbedding(input)
			Expect(result).NotTo(ContainSubstring("Jkt"))
		})

		It("removes PO PDF artifacts", func() {
			input := "Text before\nPO 00000 Frm 00042\nText after"
			result := embedding.ExportCleanForEmbedding(input)
			Expect(result).NotTo(ContainSubstring("PO 00000"))
		})

		It("removes Frm PDF artifacts", func() {
			input := "Text before\nFrm 00042 Fmt 6633\nText after"
			result := embedding.ExportCleanForEmbedding(input)
			Expect(result).NotTo(ContainSubstring("Frm"))
		})

		It("removes Fmt PDF artifacts", func() {
			input := "Text before\nFmt 6633 Sfmt 6633\nText after"
			result := embedding.ExportCleanForEmbedding(input)
			Expect(result).NotTo(ContainSubstring("Fmt"))
		})

		It("removes Sfmt PDF artifacts", func() {
			input := "Text before\nSfmt 6633\nText after"
			result := embedding.ExportCleanForEmbedding(input)
			Expect(result).NotTo(ContainSubstring("Sfmt"))
		})

		It("removes G:\\COMP paths", func() {
			input := "Text before\nG:\\COMP\\PUBL\\Title47.xml\nText after"
			result := embedding.ExportCleanForEmbedding(input)
			Expect(result).NotTo(ContainSubstring("G:\\COMP"))
		})

		It("removes markdown table separators", func() {
			input := "Header\n|---|---|\nData"
			result := embedding.ExportCleanForEmbedding(input)
			Expect(result).NotTo(ContainSubstring("|---|"))
		})

		It("collapses multiple newlines to double newline", func() {
			input := "First\n\n\n\n\nSecond"
			result := embedding.ExportCleanForEmbedding(input)
			Expect(result).To(Equal("First\n\nSecond"))
		})

		It("collapses multiple spaces to single space", func() {
			input := "First    Second"
			result := embedding.ExportCleanForEmbedding(input)
			Expect(result).To(Equal("First Second"))
		})

		It("trims leading and trailing whitespace", func() {
			input := "  \n text \n  "
			result := embedding.ExportCleanForEmbedding(input)
			Expect(result).To(Equal("text"))
		})

		It("passes through clean text unchanged", func() {
			input := "The organization defines access control policy requirements."
			result := embedding.ExportCleanForEmbedding(input)
			Expect(result).To(Equal(input))
		})

		It("handles empty input", func() {
			Expect(embedding.ExportCleanForEmbedding("")).To(Equal(""))
		})

		It("handles combined artifacts", func() {
			input := "Control {{ insert: param }} text\nVerDate Sep 2014 10:47\n\n\n\nMore text  here"
			result := embedding.ExportCleanForEmbedding(input)
			Expect(result).NotTo(ContainSubstring("{{"))
			Expect(result).NotTo(ContainSubstring("VerDate"))
			Expect(result).To(ContainSubstring("Control"))
			Expect(result).To(ContainSubstring("text"))
			Expect(result).To(ContainSubstring("More text here"))
		})
	})

	Describe("prepareText", func() {
		It("prepends ancestor title when provided", func() {
			result := embedding.ExportPrepareText("Enforce access controls.", "ACCESS CONTROL", 0)
			Expect(result).To(HavePrefix("[ACCESS CONTROL] "))
			Expect(result).To(ContainSubstring("Enforce access controls."))
		})

		It("does not prepend when ancestor title is empty", func() {
			result := embedding.ExportPrepareText("Enforce access controls.", "", 0)
			Expect(result).NotTo(HavePrefix("["))
			Expect(result).To(Equal("Enforce access controls."))
		})

		It("cleans OSCAL artifacts in the text", func() {
			result := embedding.ExportPrepareText("{{ insert: param }} defines policy.", "", 0)
			Expect(result).NotTo(ContainSubstring("{{"))
			Expect(result).To(ContainSubstring("defines policy."))
		})

		It("truncates to maxChars runes", func() {
			longText := "abcdefghijklmnopqrstuvwxyz"
			result := embedding.ExportPrepareText(longText, "", 10)
			Expect(utf8.RuneCountInString(result)).To(Equal(10))
		})

		It("does not truncate when maxChars is 0", func() {
			text := "short text"
			result := embedding.ExportPrepareText(text, "", 0)
			Expect(result).To(Equal("short text"))
		})

		It("handles multi-byte runes correctly during truncation", func() {
			text := "\u00e9\u00e9\u00e9\u00e9\u00e9"
			result := embedding.ExportPrepareText(text, "", 3)
			Expect(utf8.RuneCountInString(result)).To(Equal(3))
			Expect(utf8.ValidString(result)).To(BeTrue())
		})

		It("handles ancestor + cleaning + truncation together", func() {
			result := embedding.ExportPrepareText("text {{ insert: param }}", "ROOT", 10)
			Expect(utf8.RuneCountInString(result)).To(BeNumerically("<=", 10))
			Expect(result).To(HavePrefix("[ROOT] "))
		})

		It("handles empty statement", func() {
			result := embedding.ExportPrepareText("", "ROOT", 0)
			Expect(result).To(Equal("[ROOT]"))
		})

		It("handles empty statement and empty ancestor", func() {
			result := embedding.ExportPrepareText("", "", 0)
			Expect(result).To(Equal(""))
		})
	})
})

var _ = Describe("Cosine Similarity", func() {
	Describe("cosineSimilarity", func() {
		It("returns 1.0 for identical vectors", func() {
			v := []float32{1.0, 2.0, 3.0}
			result := embedding.ExportCosineSimilarity(v, v)
			Expect(result).To(BeNumerically("~", 1.0, 1e-6))
		})

		It("returns 0.0 for orthogonal vectors", func() {
			a := []float32{1.0, 0.0}
			b := []float32{0.0, 1.0}
			result := embedding.ExportCosineSimilarity(a, b)
			Expect(result).To(BeNumerically("~", 0.0, 1e-6))
		})

		It("returns -1.0 for opposite vectors", func() {
			a := []float32{1.0, 0.0}
			b := []float32{-1.0, 0.0}
			result := embedding.ExportCosineSimilarity(a, b)
			Expect(result).To(BeNumerically("~", -1.0, 1e-6))
		})

		It("returns 0.0 for zero vector (fail-safe)", func() {
			zero := []float32{0.0, 0.0, 0.0}
			normal := []float32{1.0, 2.0, 3.0}
			Expect(embedding.ExportCosineSimilarity(zero, normal)).To(BeNumerically("~", 0.0, 1e-6))
			Expect(embedding.ExportCosineSimilarity(normal, zero)).To(BeNumerically("~", 0.0, 1e-6))
		})

		It("returns 0.0 for both zero vectors", func() {
			zero := []float32{0.0, 0.0}
			Expect(embedding.ExportCosineSimilarity(zero, zero)).To(BeNumerically("~", 0.0, 1e-6))
		})

		It("is symmetric", func() {
			a := []float32{1.0, 2.0, 3.0}
			b := []float32{4.0, 5.0, 6.0}
			Expect(embedding.ExportCosineSimilarity(a, b)).To(
				BeNumerically("~", embedding.ExportCosineSimilarity(b, a), 1e-6))
		})

		It("handles high-dimensional vectors", func() {
			dim := 768
			a := make([]float32, dim)
			b := make([]float32, dim)
			for i := range dim {
				a[i] = float32(i) * 0.01
				b[i] = float32(i) * 0.02
			}
			// Parallel vectors should have similarity ~1.0
			result := embedding.ExportCosineSimilarity(a, b)
			Expect(result).To(BeNumerically("~", 1.0, 1e-4))
		})

		// Parity test: known vectors with pre-computed scikit-learn values.
		// Python: cosine_similarity([[1,2,3]], [[4,5,6]])[0][0] = 0.9746318...
		It("matches scikit-learn reference for known vectors", func() {
			a := []float32{1.0, 2.0, 3.0}
			b := []float32{4.0, 5.0, 6.0}
			result := embedding.ExportCosineSimilarity(a, b)
			Expect(result).To(BeNumerically("~", 0.9746318, 1e-5))
		})

		It("returns 0.0 for mismatched dimensions (fail-safe)", func() {
			a := []float32{1.0, 2.0}
			b := []float32{1.0}
			Expect(embedding.ExportCosineSimilarity(a, b)).To(BeNumerically("~", 0.0, 1e-6))
		})

		It("returns 0.0 for empty slices (fail-safe)", func() {
			Expect(embedding.ExportCosineSimilarity([]float32{}, []float32{})).To(BeNumerically("~", 0.0, 1e-6))
		})

		It("returns 0.0 for nil slices (fail-safe)", func() {
			Expect(embedding.ExportCosineSimilarity(nil, nil)).To(BeNumerically("~", 0.0, 1e-6))
		})

		// Parity test 2: orthogonal-ish vectors
		// Python: cosine_similarity([[1,0,1]], [[0,1,0]])[0][0] = 0.0
		It("matches scikit-learn for orthogonal vectors", func() {
			a := []float32{1.0, 0.0, 1.0}
			b := []float32{0.0, 1.0, 0.0}
			result := embedding.ExportCosineSimilarity(a, b)
			Expect(result).To(BeNumerically("~", 0.0, 1e-6))
		})
	})

	Describe("buildSimilarityMatrix", func() {
		It("produces correct 2x2 matrix for known vectors", func() {
			embeddings := map[string][]float32{
				"a": {1.0, 0.0},
				"b": {0.0, 1.0},
			}
			ids := []string{"a", "b"}
			matrix := embedding.ExportBuildSimilarityMatrix(embeddings, ids)

			Expect(matrix.IDs).To(Equal(ids))
			Expect(matrix.Values).To(HaveLen(2))
			Expect(matrix.Values[0]).To(HaveLen(2))

			// Diagonal should be 100 (self-similarity scaled)
			Expect(matrix.Values[0][0]).To(BeNumerically("~", 100.0, 0.01))
			Expect(matrix.Values[1][1]).To(BeNumerically("~", 100.0, 0.01))

			// Off-diagonal should be 0 (orthogonal)
			Expect(matrix.Values[0][1]).To(BeNumerically("~", 0.0, 0.01))
			Expect(matrix.Values[1][0]).To(BeNumerically("~", 0.0, 0.01))
		})

		It("produces symmetric matrix", func() {
			embeddings := map[string][]float32{
				"a": {1.0, 2.0, 3.0},
				"b": {4.0, 5.0, 6.0},
				"c": {1.0, 0.0, 1.0},
			}
			ids := []string{"a", "b", "c"}
			matrix := embedding.ExportBuildSimilarityMatrix(embeddings, ids)

			for i := range ids {
				for j := range ids {
					Expect(matrix.Values[i][j]).To(
						BeNumerically("~", matrix.Values[j][i], 0.01),
						"matrix[%d][%d] != matrix[%d][%d]", i, j, j, i)
				}
			}
		})

		It("handles single-element input", func() {
			embeddings := map[string][]float32{
				"a": {1.0, 2.0},
			}
			ids := []string{"a"}
			matrix := embedding.ExportBuildSimilarityMatrix(embeddings, ids)

			Expect(matrix.IDs).To(Equal(ids))
			Expect(matrix.Values).To(HaveLen(1))
			Expect(matrix.Values[0][0]).To(BeNumerically("~", 100.0, 0.01))
		})

		It("handles empty input", func() {
			matrix := embedding.ExportBuildSimilarityMatrix(map[string][]float32{}, []string{})
			Expect(matrix.IDs).To(BeEmpty())
			Expect(matrix.Values).To(BeEmpty())
		})
	})

	Describe("topKPairs", func() {
		It("returns top-K pairs sorted by similarity descending", func() {
			matrix := &embedding.SimilarityMatrix{
				IDs: []string{"a", "b", "c"},
				Values: [][]float32{
					{100.0, 80.0, 30.0},
					{80.0, 100.0, 50.0},
					{30.0, 50.0, 100.0},
				},
			}
			pairs := embedding.ExportTopKPairs(matrix, 2)
			Expect(pairs).To(HaveLen(2))
			// Highest off-diagonal: a-b (80), then b-c (50)
			Expect(pairs[0].Similarity).To(BeNumerically("~", 80.0, 0.01))
			Expect(pairs[1].Similarity).To(BeNumerically("~", 50.0, 0.01))
		})

		It("excludes self-similarity (diagonal)", func() {
			matrix := &embedding.SimilarityMatrix{
				IDs:    []string{"a", "b"},
				Values: [][]float32{{100.0, 50.0}, {50.0, 100.0}},
			}
			pairs := embedding.ExportTopKPairs(matrix, 10)
			for _, p := range pairs {
				Expect(p.SourceID).NotTo(Equal(p.TargetID))
			}
		})

		It("deduplicates symmetric pairs (a,b) and (b,a)", func() {
			matrix := &embedding.SimilarityMatrix{
				IDs:    []string{"a", "b"},
				Values: [][]float32{{100.0, 50.0}, {50.0, 100.0}},
			}
			pairs := embedding.ExportTopKPairs(matrix, 10)
			// Only one of (a,b) or (b,a) should appear
			Expect(pairs).To(HaveLen(1))
		})

		It("returns empty for k=0", func() {
			matrix := &embedding.SimilarityMatrix{
				IDs:    []string{"a", "b"},
				Values: [][]float32{{100.0, 50.0}, {50.0, 100.0}},
			}
			Expect(embedding.ExportTopKPairs(matrix, 0)).To(BeEmpty())
		})

		It("returns all pairs when k exceeds count", func() {
			matrix := &embedding.SimilarityMatrix{
				IDs:    []string{"a", "b", "c"},
				Values: [][]float32{{100, 80, 30}, {80, 100, 50}, {30, 50, 100}},
			}
			// 3 unique off-diagonal pairs: (a,b), (a,c), (b,c)
			pairs := embedding.ExportTopKPairs(matrix, 100)
			Expect(pairs).To(HaveLen(3))
		})

		It("handles empty matrix", func() {
			matrix := &embedding.SimilarityMatrix{}
			Expect(embedding.ExportTopKPairs(matrix, 5)).To(BeEmpty())
		})
	})

	Describe("writeMatrixCSV", func() {
		It("produces CSV with header and index column", func() {
			matrix := &embedding.SimilarityMatrix{
				IDs: []string{"a", "b"},
				Values: [][]float32{
					{100.0, 50.5},
					{50.5, 100.0},
				},
			}
			var buf strings.Builder
			err := embedding.ExportWriteMatrixCSV(matrix, &buf)
			Expect(err).NotTo(HaveOccurred())

			lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
			Expect(lines).To(HaveLen(3)) // header + 2 rows

			// Header: ,a,b
			Expect(lines[0]).To(Equal(",a,b"))
			// Row 0: a,100.00,50.50
			Expect(lines[1]).To(HavePrefix("a,"))
			Expect(lines[1]).To(ContainSubstring("100.00"))
			Expect(lines[1]).To(ContainSubstring("50.50"))
		})

		It("handles empty matrix", func() {
			matrix := &embedding.SimilarityMatrix{}
			var buf strings.Builder
			err := embedding.ExportWriteMatrixCSV(matrix, &buf)
			Expect(err).NotTo(HaveOccurred())
			// Only header line (empty)
			Expect(strings.TrimSpace(buf.String())).To(Equal(""))
		})
	})
})

var _ = Describe("EmbeddingAnalyzer", func() {
	var (
		a          *embedding.EmbeddingAnalyzer
		mockClient *mockLLMClient
		mockVec    *mockVectorDB
		mockStore  *mockStorage
		cfg        config.EmbeddingConfig
		relCfg     config.RelationshipConfig
		ctx        context.Context
	)

	BeforeEach(func() {
		ctx = testspecs.SetupTenantContext("test-tenant")

		mockClient = &mockLLMClient{}
		mockVec = &mockVectorDB{}
		mockStore = newMockStorage()

		cfg = config.EmbeddingConfig{
			Enabled:   true,
			Models:    []string{"test-model"},
			MaxChars:  1500,
			BatchSize: 50,
		}

		relCfg = config.RelationshipConfig{TopK: 20}

		a = embedding.New(mockClient, mockVec, mockStore, cfg, relCfg)
	})

	Describe("Name", func() {
		It("returns 'embedding'", func() {
			Expect(a.Name()).To(Equal("embedding"))
		})
	})

	Describe("DependsOn", func() {
		It("returns classify as upstream dependency", func() {
			deps := a.DependsOn()
			Expect(deps).To(Equal([]string{"classify"}))
		})
	})

	Describe("ResultSchema", func() {
		It("returns *AnalysisResult", func() {
			schema := a.ResultSchema()
			Expect(schema).NotTo(BeNil())
			_, ok := schema.(*pb.AnalysisResult)
			Expect(ok).To(BeTrue())
		})
	})

	Describe("GenerateWork", func() {
		Context("with a compliance requirement", func() {
			It("produces one task per model", func() {
				control := &pb.Control{
					ControlId:  "nist-800-53/AC-1",
					Identifier: "AC-1",
					Statement:  "The organization shall define access control policy.",
					Parts:      map[string]string{"class": "compliance-requirement"},
				}
				tasks, err := a.GenerateWork(ctx, control, analyzer.AnalyzerConfig{})
				Expect(err).NotTo(HaveOccurred())
				Expect(tasks).To(HaveLen(1)) // one model
				Expect(tasks[0].TaskType).To(Equal("embedding"))
				Expect(tasks[0].TaskID).To(ContainSubstring("AC-1"))
				Expect(tasks[0].TaskID).To(ContainSubstring("test-model"))

				payload, ok := tasks[0].Payload.(*structpb.Struct)
				Expect(ok).To(BeTrue(), "payload should be *structpb.Struct")
				Expect(payload.Fields["control_id"].GetStringValue()).To(Equal("nist-800-53/AC-1"))
				Expect(payload.Fields["model"].GetStringValue()).To(Equal("test-model"))
				Expect(payload.Fields["text"].GetStringValue()).NotTo(BeEmpty())
				Expect(payload.Fields["batch_size"].GetNumberValue()).To(Equal(float64(50)))
				Expect(payload.Fields["content_hash"].GetStringValue()).NotTo(BeEmpty())
			})

			It("produces multiple tasks for multiple models", func() {
				multiModelA := embedding.New(mockClient, mockVec, mockStore,
					config.EmbeddingConfig{
						Enabled:   true,
						Models:    []string{"model-a", "model-b"},
						MaxChars:  1500,
						BatchSize: 50,
					}, relCfg)

				control := &pb.Control{
					ControlId:  "nist-800-53/AC-1",
					Identifier: "AC-1",
					Statement:  "Access control policy.",
					Parts:      map[string]string{"class": "compliance-requirement"},
				}
				tasks, err := multiModelA.GenerateWork(ctx, control, analyzer.AnalyzerConfig{})
				Expect(err).NotTo(HaveOccurred())
				Expect(tasks).To(HaveLen(2))
			})
		})

		Context("with a compliance section", func() {
			It("produces a pre-built skip result (no embedding)", func() {
				control := &pb.Control{
					ControlId:  "nist-800-53/AC",
					Identifier: "AC",
					Statement:  "ACCESS CONTROL",
					Parts:      map[string]string{"class": "compliance-section"},
				}
				tasks, err := a.GenerateWork(ctx, control, analyzer.AnalyzerConfig{})
				Expect(err).NotTo(HaveOccurred())
				Expect(tasks).To(HaveLen(1))

				result, ok := tasks[0].Payload.(*pb.AnalysisResult)
				Expect(ok).To(BeTrue())
				Expect(result.Attributes["skipped"]).To(Equal("true"))
			})
		})

		Context("without tenant context", func() {
			It("returns an error", func() {
				control := &pb.Control{
					ControlId: "nist-800-53/AC-1",
					Statement: "text",
					Parts:     map[string]string{"class": "compliance-requirement"},
				}
				_, err := a.GenerateWork(context.Background(), control, analyzer.AnalyzerConfig{})
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("embedding.GenerateWork"))
			})
		})
	})

	Describe("Aggregate", func() {
		It("counts embedded, skipped, and errors", func() {
			results := []analyzer.TaskResult{
				{
					TaskID:   "embedding-ctrl-1-test-model",
					TaskType: "embedding",
					Result: &pb.AnalysisResult{
						ResultId: "embedding-ctrl-1-test-model",
						Attributes: map[string]string{
							"control_id": "ctrl-1",
							"model":      "test-model",
						},
					},
				},
				{
					TaskID:   "embedding-ctrl-2-test-model",
					TaskType: "embedding",
					Result: &pb.AnalysisResult{
						ResultId:   "embedding-ctrl-2-test-model",
						Attributes: map[string]string{"skipped": "true"},
					},
				},
				{
					TaskID:   "embedding-ctrl-3-test-model",
					TaskType: "embedding",
					Error:    errors.New("llm timeout"),
				},
			}

			output, err := a.Aggregate(ctx, results)
			Expect(err).NotTo(HaveOccurred())
			Expect(output.AnalyzerName).To(Equal("embedding"))
			Expect(output.Metadata["total_count"]).To(Equal("3"))
			Expect(output.Metadata["skipped_count"]).To(Equal("1"))
			Expect(output.Metadata["error_count"]).To(Equal("1"))
			Expect(output.Metadata["embedded_count"]).To(Equal("1"))
		})

		Context("with empty results", func() {
			It("returns zero counts", func() {
				output, err := a.Aggregate(ctx, []analyzer.TaskResult{})
				Expect(err).NotTo(HaveOccurred())
				Expect(output.AnalyzerName).To(Equal("embedding"))
				Expect(output.Metadata["total_count"]).To(Equal("0"))
				Expect(output.Metadata["embedded_count"]).To(Equal("0"))
				Expect(output.Metadata["skipped_count"]).To(Equal("0"))
				Expect(output.Metadata["error_count"]).To(Equal("0"))
			})
		})

		Context("with all errors", func() {
			It("counts all as errors", func() {
				results := []analyzer.TaskResult{
					{TaskID: "t1", Error: errors.New("fail")},
					{TaskID: "t2", Error: errors.New("fail")},
				}
				output, err := a.Aggregate(ctx, results)
				Expect(err).NotTo(HaveOccurred())
				Expect(output.Metadata["total_count"]).To(Equal("2"))
				Expect(output.Metadata["embedded_count"]).To(Equal("0"))
				Expect(output.Metadata["error_count"]).To(Equal("2"))
			})
		})

		Context("with wrong result type", func() {
			It("counts as error", func() {
				results := []analyzer.TaskResult{
					{TaskID: "t1", Result: &structpb.Struct{}},
				}
				output, err := a.Aggregate(ctx, results)
				Expect(err).NotTo(HaveOccurred())
				Expect(output.Metadata["total_count"]).To(Equal("1"))
				Expect(output.Metadata["error_count"]).To(Equal("1"))
			})
		})

		Context("persisting embedding vectors", func() {
			var embedCfg config.EmbeddingConfig

			BeforeEach(func() {
				embedCfg = config.EmbeddingConfig{Models: []string{"modelA"}}
			})

			buildEmbeddingPayload := func(vector []float32) *structpb.Struct {
				values := make([]interface{}, len(vector))
				for i, v := range vector {
					values[i] = float64(v)
				}
				payload, err := structpb.NewStruct(map[string]interface{}{
					"embeddings": []interface{}{values},
					"model":      "modelA",
					"dimensions": float64(len(vector)),
				})
				Expect(err).NotTo(HaveOccurred())
				return payload
			}

			It("stores a batch of embeddings extracted from successful TaskResults", func() {
				a = embedding.New(mockClient, mockVec, mockStore, embedCfg, relCfg)

				aggCtx, err := tenant.WithTenant(context.Background(), "tenant-a")
				Expect(err).NotTo(HaveOccurred())
				aggCtx = analyzer.WithCatalogID(aggCtx, "cat-1")

				results := []analyzer.TaskResult{
					{
						TaskID:   "embedding-AC-1-modelA",
						TaskType: "embedding",
						Result:   buildEmbeddingPayload([]float32{0.1, 0.2, 0.3}),
					},
					{
						TaskID:   "embedding-AC-2-modelA",
						TaskType: "embedding",
						Result:   buildEmbeddingPayload([]float32{0.4, 0.5, 0.6}),
					},
				}

				_, err = a.Aggregate(aggCtx, results)
				Expect(err).NotTo(HaveOccurred())

				Expect(mockVec.storedBatches).To(HaveLen(1))
				Expect(mockVec.storedTenants).To(Equal([]string{"tenant-a"}))
				Expect(mockVec.storedBatches[0]).To(ConsistOf(
					vectordb.Embedding{CatalogID: "cat-1", ControlID: "AC-1", Model: "modelA", Vector: []float32{0.1, 0.2, 0.3}},
					vectordb.Embedding{CatalogID: "cat-1", ControlID: "AC-2", Model: "modelA", Vector: []float32{0.4, 0.5, 0.6}},
				))
			})

			It("skips a TaskResult with a task-level error", func() {
				a = embedding.New(mockClient, mockVec, mockStore, embedCfg, relCfg)

				aggCtx, err := tenant.WithTenant(context.Background(), "tenant-a")
				Expect(err).NotTo(HaveOccurred())
				aggCtx = analyzer.WithCatalogID(aggCtx, "cat-1")

				results := []analyzer.TaskResult{
					{
						TaskID:   "embedding-AC-1-modelA",
						TaskType: "embedding",
						Result:   buildEmbeddingPayload([]float32{0.1, 0.2, 0.3}),
					},
					{
						TaskID:   "embedding-AC-2-modelA",
						TaskType: "embedding",
						Error:    errors.New("llm timeout"),
					},
				}

				_, err = a.Aggregate(aggCtx, results)
				Expect(err).NotTo(HaveOccurred())

				Expect(mockVec.storedBatches).To(HaveLen(1))
				Expect(mockVec.storedBatches[0]).To(ConsistOf(
					vectordb.Embedding{CatalogID: "cat-1", ControlID: "AC-1", Model: "modelA", Vector: []float32{0.1, 0.2, 0.3}},
				))
			})

			It("skips persistence without error when there is no vector store", func() {
				a = embedding.New(mockClient, nil /* vectors */, mockStore, embedCfg, relCfg)

				aggCtx, err := tenant.WithTenant(context.Background(), "tenant-a")
				Expect(err).NotTo(HaveOccurred())
				aggCtx = analyzer.WithCatalogID(aggCtx, "cat-1")

				results := []analyzer.TaskResult{
					{
						TaskID:   "embedding-AC-1-modelA",
						TaskType: "embedding",
						Result:   buildEmbeddingPayload([]float32{0.1, 0.2, 0.3}),
					},
				}

				Expect(func() {
					_, err = a.Aggregate(aggCtx, results)
				}).NotTo(Panic())
				Expect(err).NotTo(HaveOccurred())
			})

			It("errors if CatalogID is missing from context and there are embeddings to store", func() {
				a = embedding.New(mockClient, mockVec, mockStore, embedCfg, relCfg)

				aggCtx, err := tenant.WithTenant(context.Background(), "tenant-a")
				Expect(err).NotTo(HaveOccurred())
				// No analyzer.WithCatalogID call — catalog ID is absent from context.

				results := []analyzer.TaskResult{
					{
						TaskID:   "embedding-AC-1-modelA",
						TaskType: "embedding",
						Result:   buildEmbeddingPayload([]float32{0.1, 0.2, 0.3}),
					},
				}

				_, err = a.Aggregate(aggCtx, results)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("catalog ID required"))
				Expect(mockVec.storedBatches).To(BeEmpty())
			})
		})
	})

	Describe("telemetry integration", func() {
		It("creates spans on GenerateWork", func() {
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = tp.Shutdown(ctx) }()

			a = embedding.New(mockClient, mockVec, mockStore, cfg, relCfg,
				embedding.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()))

			control := &pb.Control{
				ControlId: "nist-800-53/AC-1",
				Statement: "Test requirement.",
				Parts:     map[string]string{"class": "compliance-requirement"},
			}
			_, err = a.GenerateWork(ctx, control, analyzer.AnalyzerConfig{})
			Expect(err).NotTo(HaveOccurred())

			spans := tp.GetSpans()
			span := telemetrytest.FindSpan(spans, "embedding.GenerateWork")
			Expect(span).NotTo(BeNil())

			tenantAttr, ok := telemetrytest.SpanAttribute(span, "tenant.id")
			Expect(ok).To(BeTrue())
			Expect(tenantAttr.AsString()).To(Equal("test-tenant"))

			controlAttr, ok := telemetrytest.SpanAttribute(span, "control.id")
			Expect(ok).To(BeTrue())
			Expect(controlAttr.AsString()).To(Equal("nist-800-53/AC-1"))
		})

		It("creates spans on Aggregate", func() {
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = tp.Shutdown(ctx) }()

			a = embedding.New(mockClient, mockVec, mockStore, cfg, relCfg,
				embedding.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()))

			_, err = a.Aggregate(ctx, []analyzer.TaskResult{})
			Expect(err).NotTo(HaveOccurred())

			spans := tp.GetSpans()
			span := telemetrytest.FindSpan(spans, "embedding.Aggregate")
			Expect(span).NotTo(BeNil())
		})

		It("records skipped attribute for section controls", func() {
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = tp.Shutdown(ctx) }()

			a = embedding.New(mockClient, mockVec, mockStore, cfg, relCfg,
				embedding.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()))

			control := &pb.Control{
				ControlId: "nist-800-53/AC",
				Statement: "ACCESS CONTROL",
				Parts:     map[string]string{"class": "compliance-section"},
			}
			_, err = a.GenerateWork(ctx, control, analyzer.AnalyzerConfig{})
			Expect(err).NotTo(HaveOccurred())

			spans := tp.GetSpans()
			span := telemetrytest.FindSpan(spans, "embedding.GenerateWork")
			Expect(span).NotTo(BeNil())

			skippedAttr, ok := telemetrytest.SpanAttribute(span, "embedding.skipped")
			Expect(ok).To(BeTrue())
			Expect(skippedAttr.AsBool()).To(BeTrue())
		})
	})

	Describe("metrics integration", func() {
		It("records skipped counter for section controls", func() {
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = tp.Shutdown(ctx) }()

			a = embedding.New(mockClient, mockVec, mockStore, cfg, relCfg,
				embedding.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()))

			control := &pb.Control{
				ControlId: "nist-800-53/AC",
				Statement: "ACCESS CONTROL",
				Parts:     map[string]string{"class": "compliance-section"},
			}
			_, err = a.GenerateWork(ctx, control, analyzer.AnalyzerConfig{})
			Expect(err).NotTo(HaveOccurred())

			rm := tp.GetMetrics()
			counterMetric := telemetrytest.FindMetric(rm, "embedding.operations.total")
			Expect(counterMetric).NotTo(BeNil(), "expected embedding.operations.total metric")
			count, err := telemetrytest.CounterValue(counterMetric)
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(Equal(int64(1)))
		})

		It("records pending counter for requirement controls", func() {
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = tp.Shutdown(ctx) }()

			a = embedding.New(mockClient, mockVec, mockStore, cfg, relCfg,
				embedding.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()))

			control := &pb.Control{
				ControlId: "nist-800-53/AC-1",
				Statement: "The organization shall define access control policy.",
				Parts:     map[string]string{"class": "compliance-requirement"},
			}
			_, err = a.GenerateWork(ctx, control, analyzer.AnalyzerConfig{})
			Expect(err).NotTo(HaveOccurred())

			rm := tp.GetMetrics()
			counterMetric := telemetrytest.FindMetric(rm, "embedding.operations.total")
			Expect(counterMetric).NotTo(BeNil(), "expected embedding.operations.total metric")
			count, err := telemetrytest.CounterValue(counterMetric)
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(Equal(int64(1)))
		})

		It("records embedded and error counters in Aggregate", func() {
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = tp.Shutdown(ctx) }()

			a = embedding.New(mockClient, mockVec, mockStore, cfg, relCfg,
				embedding.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()))

			results := []analyzer.TaskResult{
				{
					TaskID:   "task-1",
					TaskType: "embedding",
					Result: &pb.AnalysisResult{
						ResultId: "task-1",
						Attributes: map[string]string{
							"control_id": "AC-1",
							"model":      "test-model",
						},
					},
				},
				{
					TaskID:   "task-2",
					TaskType: "embedding",
					Result: &pb.AnalysisResult{
						ResultId:   "task-2",
						Attributes: map[string]string{"skipped": "true"},
					},
				},
				{
					TaskID:   "task-3",
					TaskType: "embedding",
					Error:    errors.New("LLM timeout"),
				},
			}

			_, err = a.Aggregate(ctx, results)
			Expect(err).NotTo(HaveOccurred())

			rm := tp.GetMetrics()
			counterMetric := telemetrytest.FindMetric(rm, "embedding.operations.total")
			Expect(counterMetric).NotTo(BeNil(), "expected embedding.operations.total metric")

			// Total should be 2: 1 embedded + 1 error (skipped is not counted in Aggregate)
			count, err := telemetrytest.CounterValue(counterMetric)
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(Equal(int64(2)))
		})

		It("does not panic without telemetry", func() {
			a = embedding.New(mockClient, mockVec, mockStore, cfg, relCfg) // no WithTelemetry

			control := &pb.Control{
				ControlId: "nist-800-53/AC-1",
				Statement: "Test requirement.",
				Parts:     map[string]string{"class": "compliance-requirement"},
			}
			// Should not panic with nil instruments.
			_, err := a.GenerateWork(ctx, control, analyzer.AnalyzerConfig{})
			Expect(err).NotTo(HaveOccurred())

			_, err = a.Aggregate(ctx, []analyzer.TaskResult{})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

// TestAggregate_ProducesResultData verifies that Aggregate builds
// ResultData from both TaskID shapes GenerateWork emits: the per-model
// path ("embedding-<controlID>-<model>") and the section auto-skip path
// ("embedding-<controlID>", no model suffix). A control embedded by
// multiple configured models must appear exactly once (deduplicated), and
// the output must be sorted by control ID rather than reflecting
// map-iteration or input order.
func TestAggregate_ProducesResultData(t *testing.T) {
	cfg := config.EmbeddingConfig{Models: []string{"nomic-embed-text", "mxbai-embed-large"}}
	a := embedding.New(nil, nil, nil, cfg, config.RelationshipConfig{})

	out, err := a.Aggregate(context.Background(), []analyzer.TaskResult{
		{TaskID: "embedding-AC-2-nomic-embed-text", TaskType: "embedding"},
		{TaskID: "embedding-AC-2-mxbai-embed-large", TaskType: "embedding"},
		{TaskID: "embedding-AC-1-nomic-embed-text", TaskType: "embedding"},
		{TaskID: "embedding-AC-3", TaskType: "embedding"}, // section auto-skip, no model suffix
	})
	if err != nil {
		t.Fatalf("Aggregate error: %v", err)
	}

	var parsed []results.EmbedResult
	if err := json.Unmarshal(out.ResultData, &parsed); err != nil {
		t.Fatalf("unmarshal ResultData: %v", err)
	}

	want := []results.EmbedResult{
		{ControlID: "AC-1"},
		{ControlID: "AC-2"},
		{ControlID: "AC-3"},
	}
	if len(parsed) != len(want) {
		t.Fatalf("expected %d controls, got %d: %+v", len(want), len(parsed), parsed)
	}
	for i, w := range want {
		if parsed[i] != w {
			t.Fatalf("ResultData[%d] = %+v, want %+v (full: %+v)", i, parsed[i], w, parsed)
		}
	}
}

// TestAggregate_ResultDataOmitsErrorsAndForeignTaskIDs verifies that
// TaskResults with a non-nil Error, and TaskIDs that don't carry this
// analyzer's prefix, are excluded from ResultData rather than producing a
// garbage entry or crashing Aggregate.
func TestAggregate_ResultDataOmitsErrorsAndForeignTaskIDs(t *testing.T) {
	cfg := config.EmbeddingConfig{Models: []string{"nomic-embed-text"}}
	a := embedding.New(nil, nil, nil, cfg, config.RelationshipConfig{})

	out, err := a.Aggregate(context.Background(), []analyzer.TaskResult{
		{TaskID: "embedding-AC-4-nomic-embed-text", TaskType: "embedding", Error: errors.New("llm timeout")},
		{TaskID: "classify-AC-5", TaskType: "classify"},
	})
	if err != nil {
		t.Fatalf("Aggregate error: %v", err)
	}

	var parsed []results.EmbedResult
	if err := json.Unmarshal(out.ResultData, &parsed); err != nil {
		t.Fatalf("unmarshal ResultData: %v", err)
	}
	if len(parsed) != 0 {
		t.Fatalf("expected no controls in ResultData, got %+v", parsed)
	}
}
