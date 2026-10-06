// Package requires implements prerequisite requirement analysis between pairs
// of compliance controls using candidate generators and consensus voting.
//
// It implements [github.com/complytime-labs/crosscodex/pkg/analyzer.Analyzer]
// parameterized on [*catalogpb.Control]. Candidate pairs are supplied via the
// [CandidateProvider] interface (wired by the pipeline orchestrator from
// prerequisite detection generators). Each pair's prerequisite relationship is
// classified and results are aggregated.
//
// Results are stored as per-pair JSON files in object storage (source of truth)
// and published as NATS events. internal/graph consumes the events and writes
// the REQUIRES edges.
package requires
