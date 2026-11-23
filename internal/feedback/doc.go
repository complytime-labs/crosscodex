// Package feedback records reviewers' feedback on a job's mappings: votes,
// and determinations that a mapping is valid or invalid (issue #230).
//
// A mapping is one synthesized vote_summaries row of a completed job, the
// relationship analyzer's verdict on a (source, target) control pair. Its ID
// is MappingID(job, source, target), the SEMANTIC_MATCH edge ID the job wrote
// (graphdb.DerivedID(results.SemanticMatchEdgeIDKind, ...)). The ID names the
// verdict, a stable key that never moves to another edge (see the identity
// rule in docs/dev/feedback-cleaning-results-spec.md).
//
// Store is the authoritative, tenant-scoped record, kept in three tables of
// migration 003_mapping_feedback:
//
//   - mapping_votes: append-only votes. A reviewer's latest vote on a mapping
//     is the one counted (LatestVotes).
//   - mapping_determinations: append-only valid / invalid / stale calls, each
//     with its canonical request and signed attestation. Only re-projection
//     records stale ones (MarkStale), as DecidedByReproject.
//   - mapping_reviews: one mutable row per decided mapping, naming its
//     current determination and the graph edges that determination changed.
//     GraphApplied is false from the moment a determination is recorded
//     until its edge changes are written (MarkProjected), so the graph, a
//     projection of these rows, can always be brought back in line.
//
// PGStore implements Store over PostgreSQL as the restricted application
// role; row-level security confines every call to the tenant in ctx. Votes
// and determinations are refused at write time unless the mapping is one of
// a completed job visible to ctx, and unless they pass Vote.Validate or
// Determination.Validate. A determination also needs an active registered
// reviewer and the review state its caller read, checked under a row lock.
//
// Service serves SubmitVote, GetReviewQueue and RecordDetermination over a
// Store, and is the role gate. A determination changes the graph only
// through pkg/cleaning's RetireEdges and RestoreEdges, after it is signed
// and recorded; the review's GraphApplied flag tracks that change, and
// outstanding changes are applied before the next determination. Reproject
// replays the reviews into a rebuilt graph, as cleaning's Reproject does for
// findings. Without WithDeterminations (the embedded daemon) only votes and
// the queue are served.
package feedback
