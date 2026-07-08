// Package artifacts implements observable artifact extraction from compliance
// controls using multi-sample LLM panel voting with fuzzy deduplication.
//
// It implements [github.com/complytime-labs/crosscodex/pkg/analyzer.Analyzer]
// parameterized on [*catalogpb.Control]. Each control is independently analyzed
// by multiple LLM models, and results are aggregated via fuzzy token-set
// matching to deduplicate semantically equivalent artifacts across model votes.
//
// Results are stored as per-control JSON files in object storage (source of
// truth) and materialized as graph nodes and edges by [GraphMaterializer].
//
// This is a port of OllamaCrosswalker's Python ArtifactExtractor.
package artifacts
