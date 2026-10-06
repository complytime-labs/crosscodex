// Package relationship implements NIST IR 8477 relationship classification
// between pairs of compliance controls using multi-sample LLM panel voting.
//
// It implements [github.com/complytime-labs/crosscodex/pkg/analyzer.Analyzer]
// parameterized on [*catalogpb.Control]. Candidate pairs are supplied via the
// [CandidateProvider] interface (wired by the pipeline orchestrator from
// embedding similarity results). Each pair is classified by multiple LLM models
// with optional self-consistency sampling, and results are aggregated via a
// deterministic consensus algorithm.
//
// Results are stored as per-pair JSON files in object storage (source of truth)
// and published as NATS events. internal/graph consumes the events and writes
// the SEMANTIC_MATCH edges. [GraphMaterializer] writes a job's SEMANTIC_MATCH
// edges from object storage instead, but has no production caller yet.
//
// This is a port of OllamaCrosswalker's Python RelationshipClassifier with one
// documented behavioral divergence: contribution type tiebreaks are deterministic
// (INTEGRAL_TO wins ties) rather than non-deterministic as in Python's max().
package relationship
