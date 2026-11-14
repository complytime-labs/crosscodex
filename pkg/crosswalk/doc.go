// Package crosswalk holds the pure rules for turning relationship verdicts
// into crosswalk rows: inverting a relationship across direction, composing
// two legs of a transitive path, and merging verdicts from several jobs.
//
// It is logic over the pb.RelationshipType proto enum only. It performs no
// I/O and knows nothing about catalogs, frameworks or storage; callers (the
// graph walk and CLI export) supply verdicts and render the results.
package crosswalk
