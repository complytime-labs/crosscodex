// Package artifacts implements observable artifact extraction from compliance
// controls using multi-sample LLM panel voting with fuzzy deduplication.
//
// It implements [github.com/complytime-labs/crosscodex/pkg/analyzer.Analyzer]
// parameterized on [*catalogpb.Control]. Each control is independently analyzed
// by multiple LLM models, and results are aggregated via fuzzy token-set
// matching to deduplicate semantically equivalent artifacts across model votes.
//
// Results are stored as per-control JSON files in object storage (source of
// truth). internal/graph consumes the published results and writes the live
// Artifact and ArtifactType nodes and DEMANDS and IS_TYPE edges.
// [GraphMaterializer] writes the same kinds of nodes and edges from object
// storage, under its own ID scheme, but has no production caller yet.
//
// This is a port of OllamaCrosswalker's Python ArtifactExtractor.
package artifacts
