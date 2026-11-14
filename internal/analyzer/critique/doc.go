// Package critique implements the critique stage. An independent model panel
// re-reads every verdict the relationship and requires panels accepted and
// answers UPHOLD or REJECT. A verdict is vetoed when the share of REJECT votes
// among the valid votes reaches analysis.critique.consensus_threshold.
//
// The analyzer only decides. It stores one results.CritiqueResult per
// reviewed verdict, which records every vote's reason. Applying the vetoes
// downstream (mappings, REQUIRES edges, exports) is the pipeline's job.
//
// The proposed verdicts arrive through VerdictProvider, which the pipeline
// implements from the relationship and requires results it has stored. The
// analyzers never import each other.
package critique
