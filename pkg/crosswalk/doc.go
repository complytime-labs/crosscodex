// Package crosswalk holds the pure rules for turning relationship verdicts
// into crosswalk rows: inverting a relationship across direction, composing
// two legs of a transitive path, and merging verdicts from several jobs.
//
// Apart from LoadPair, which reads verdict edges through a graphdb.GraphDB,
// it is logic over the pb.RelationshipType proto enum only: it performs no I/O
// and knows nothing about frameworks or storage; callers (the graph walk and
// CLI export) supply verdicts and render the results.
package crosswalk
