package pipeline

import (
	"context"
	"io"
	"testing"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/prompt"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/vectordb"
)

// fakeLLMClient is a minimal no-op llmclient.Client for constructor wiring
// tests that never invoke Complete/Embed.
type fakeLLMClient struct{}

func (f *fakeLLMClient) Complete(ctx context.Context, req *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
	return nil, nil
}

func (f *fakeLLMClient) Embed(ctx context.Context, req *llmclient.EmbeddingRequest) (*llmclient.EmbeddingResponse, error) {
	return nil, nil
}

func (f *fakeLLMClient) Health(ctx context.Context) error { return nil }

func (f *fakeLLMClient) Close() error { return nil }

// fakeVectorDB is a minimal no-op vectordb.VectorDB for constructor wiring
// tests that never invoke embedding storage or search.
type fakeVectorDB struct{}

func (f *fakeVectorDB) StoreEmbedding(ctx context.Context, tenant string, embedding vectordb.Embedding) error {
	return nil
}

func (f *fakeVectorDB) StoreBatch(ctx context.Context, tenant string, embeddings []vectordb.Embedding) error {
	return nil
}

func (f *fakeVectorDB) FindSimilar(ctx context.Context, tenant string, query vectordb.FindSimilarQuery) ([]vectordb.SimilarityResult, error) {
	return nil, nil
}

func (f *fakeVectorDB) DeleteByModel(ctx context.Context, tenant, catalogID, model string) error {
	return nil
}

// fakeStorage is a minimal no-op storage.Provider for constructor wiring
// tests that never read or write objects.
type fakeStorage struct{}

func (f *fakeStorage) Get(ctx context.Context, key string) (io.ReadCloser, error) { return nil, nil }

func (f *fakeStorage) Put(ctx context.Context, key string, data io.Reader) error { return nil }

func (f *fakeStorage) Delete(ctx context.Context, key string) error { return nil }

func (f *fakeStorage) List(ctx context.Context, prefix string) ([]storage.ObjectMetadata, error) {
	return nil, nil
}

func (f *fakeStorage) Exists(ctx context.Context, key string) (bool, error) { return false, nil }

func (f *fakeStorage) Stat(ctx context.Context, key string) (*storage.ObjectMetadata, error) {
	return nil, nil
}

func (f *fakeStorage) Close() error { return nil }

// fakePromptRegistry is a minimal no-op prompt.Registry for constructor
// wiring tests that never resolve or render prompts.
type fakePromptRegistry struct{}

func (f *fakePromptRegistry) Resolve(ctx context.Context, name string) (*prompt.PromptSpec, error) {
	return nil, nil
}

func (f *fakePromptRegistry) Render(ctx context.Context, name string, vars map[string]string) (*prompt.ResolvedPrompt, error) {
	return nil, nil
}

func (f *fakePromptRegistry) List(ctx context.Context) ([]string, error) { return nil, nil }

func (f *fakePromptRegistry) Layers(ctx context.Context, name string) ([]prompt.LayerInfo, error) {
	return nil, nil
}

// fakeTenantConn is a minimal no-op db.TenantConnection for constructor
// wiring tests that never execute queries.
type fakeTenantConn struct{}

func (f *fakeTenantConn) Begin(ctx context.Context) (db.Transaction, error) { return nil, nil }

func (f *fakeTenantConn) Query(ctx context.Context, query string, args ...any) (db.Rows, error) {
	return nil, nil
}

func (f *fakeTenantConn) QueryRow(ctx context.Context, query string, args ...any) db.Row {
	return nil
}

func (f *fakeTenantConn) Exec(ctx context.Context, query string, args ...any) error { return nil }

func (f *fakeTenantConn) Close() error { return nil }

func TestNewProductionRegistry_RegistersAllFiveAnalyzers(t *testing.T) {
	cfg := config.AnalysisConfig{
		Classification: config.ClassificationConfig{Model: "m", MaxTextLength: 100},
		Embedding:      config.EmbeddingConfig{Models: []string{"m"}, MaxChars: 100, BatchSize: 1},
		Relationship:   config.RelationshipConfig{Models: []string{"m"}, SamplesPerModel: 1, MaxSourceChars: 100, MaxTargetChars: 100, MaxTokens: 100},
		Requires:       config.RequiresConfig{Models: []string{"m"}, SamplesPerModel: 1, ConsensusThreshold: 0.5, MaxSourceChars: 100, MaxTargetChars: 100, MaxTokens: 100},
		Artifacts:      config.ArtifactsConfig{Models: []string{"m"}, SamplesPerModel: 1, MaxTokens: 100, MaxTextChars: 100, FuzzyThreshold: 0.6},
	}

	reg, err := NewProductionRegistry(&fakeLLMClient{}, &fakeVectorDB{}, &fakeStorage{}, &fakePromptRegistry{}, &fakeTenantConn{}, cfg)
	if err != nil {
		t.Fatalf("NewProductionRegistry() error = %v", err)
	}

	for _, name := range []string{"classify", "embedding", "artifacts", "requires", "relationship"} {
		if _, err := reg.Get(name); err != nil {
			t.Errorf("registry missing analyzer %q: %v", name, err)
		}
	}

	if _, err := reg.BuildDAG(context.Background()); err != nil {
		t.Errorf("BuildDAG() error = %v", err)
	}
}
