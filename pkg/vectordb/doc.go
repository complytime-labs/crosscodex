// Package vectordb provides pgvector similarity search operations.
//
// Manages vector embeddings for semantic search across compliance artifacts.
//
// Example usage:
//
//	index, err := vectordb.NewIndex(dbConn, "embeddings")
//	if err != nil {
//	    return err
//	}
//
//	err = index.Insert(ctx, "doc-123", embedding, map[string]any{
//	    "type": "control",
//	    "framework": "NIST-800-53",
//	})
//
//	matches, err := index.Search(ctx, queryEmbedding, 10)
package vectordb
