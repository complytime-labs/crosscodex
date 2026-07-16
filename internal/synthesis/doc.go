// Package synthesis implements the Synthesis Service — pure computation
// for ranking, viability weighting, and quality diagnostics.
//
// # Architecture
//
// Three components form a pipeline:
//   - Ranker: transforms []SynthesisInput + classifications → []SynthesisRow with viability weights
//   - Service: applies a confidence threshold filter and a per-source mapping cap to the ranked rows
//   - Assessor: evaluates the filtered []SynthesisRow → *QualityReport with diagnostics
//   - Service: persists viability weights to the DB and computes a SHA-256 content hash of the report
//
// # Python Parity
//
// Viability formulas use two-round rounding to match Python OllamaCrosswalker
// exactly. See viability.go for the formula; parity test vectors are in
// synthesis_bdd_test.go.
//
// # Thread Safety
//
// The Service is safe for concurrent use after construction. Ranker and
// Assessor are safe for concurrent use after construction; their internal
// state is read-only after New.
//
// # Database
//
// The Service writes to the `vote_summaries` table, updating the `viability`
// column within a single transaction per Execute call.
package synthesis
