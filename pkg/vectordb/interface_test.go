package vectordb

import (
	"context"
	"testing"
)

func TestVectorDBInterface(t *testing.T) {
	// Test that VectorDB interface has expected methods
	var _ VectorDB = (*testVectorDB)(nil)
}

// testVectorDB implements VectorDB for compile-time interface check
type testVectorDB struct{}

func (db *testVectorDB) StoreEmbedding(ctx context.Context, tenant string, embedding Embedding) error {
	return nil
}

func (db *testVectorDB) StoreBatch(ctx context.Context, tenant string, embeddings []Embedding) error {
	return nil
}

func (db *testVectorDB) FindSimilar(ctx context.Context, tenant string, query FindSimilarQuery) ([]SimilarityResult, error) {
	return nil, nil
}

func (db *testVectorDB) DeleteByModel(ctx context.Context, tenant, catalogID, model string) error {
	return nil
}
