// Package embedding implements a compliance control embedding analyzer.
//
// It generates vector embeddings for each control via
// [github.com/complytime-labs/crosscodex/pkg/llmclient.Client] and builds
// per-model cosine similarity matrices. Storage and vector database
// dependencies ([github.com/complytime-labs/crosscodex/pkg/vectordb.VectorDB]
// and [github.com/complytime-labs/crosscodex/pkg/storage.Provider]) are
// injected for use during task execution, which is handled by the pipeline
// orchestrator — this package produces work tasks and aggregates results
// but does not perform I/O against those backends directly.
//
// The analyzer implements [github.com/complytime-labs/crosscodex/pkg/analyzer.Analyzer]
// parameterized on [*catalogpb.Control]. It depends on the "classify" analyzer,
// running after classification in the analyzer DAG.
//
// Cosine similarity is computed using gonum/floats for numerical stability
// over high-dimensional vectors. Input float32 vectors (LLM wire type) are
// promoted to float64 for the computation, then demoted back for storage.
package embedding
