package relationship

import (
	"math"
	"sort"
)

// ComputeConsensus aggregates votes into a single relationship determination
// using plurality voting with priority tiebreak. This is a direct port of
// Python _compute_consensus() with one documented divergence: contribution
// type ties are broken deterministically (INTEGRAL_TO wins).
//
// This is a pure function — it takes no context and creates no spans.
// The calling pipeline orchestrator should create a wrapping span
// (e.g., "relationship.ComputeConsensus") for trace coverage.
func ComputeConsensus(votes map[string]*Vote) Consensus {
	c := Consensus{
		Relationship: RelNoRelationship,
	}

	if len(votes) == 0 {
		return c
	}

	// Collect all vote names (including parse errors) for audit trail.
	allVotes := make([]string, 0, len(votes))
	// Count valid votes by relationship type.
	counts := make(map[RelationshipType]int)
	validTotal := 0

	// Sort keys for deterministic AllVotes ordering.
	keys := make([]string, 0, len(votes))
	for k := range votes {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		v := votes[k]
		if v == nil {
			allVotes = append(allVotes, "NIL_VOTE")
			continue
		}
		switch v.ParseStatus {
		case ParseFail:
			allVotes = append(allVotes, "PARSE_FAIL")
			continue
		case ParseError:
			allVotes = append(allVotes, "PARSE_ERROR")
			continue
		}
		allVotes = append(allVotes, v.Relationship.String())
		counts[v.Relationship]++
		validTotal++
	}

	c.AllVotes = allVotes
	c.ValidVoteCount = validTotal

	// No valid votes: return NO_RELATIONSHIP with confidence 0.
	if validTotal == 0 {
		return c
	}

	// Find plurality winner. On tie, pick highest priority (lowest index).
	maxCount := 0
	for _, count := range counts {
		if count > maxCount {
			maxCount = count
		}
	}

	// Among types with maxCount, pick the one with highest priority.
	winner := RelNoRelationship
	for _, rt := range RelationshipPriority() {
		if counts[rt] == maxCount {
			winner = rt
			break
		}
	}

	c.Relationship = winner
	c.ConfidenceFraction = math.Round(float64(maxCount)/float64(validTotal)*1000) / 1000
	c.Unanimous = validTotal > 0 && maxCount == validTotal

	// Secondary consensus for CONTRIBUTES_TO: contribution type.
	if winner == RelContributesTo {
		integralCount := 0
		exampleCount := 0
		for _, v := range votes {
			if v == nil || v.ParseStatus != ParseOK || v.Relationship != RelContributesTo {
				continue
			}
			switch v.ContributionType {
			case ContribIntegralTo:
				integralCount++
			case ContribExampleOf:
				exampleCount++
			}
		}
		// Deterministic tiebreak: INTEGRAL_TO wins ties.
		if integralCount >= exampleCount && integralCount > 0 {
			c.ContributionType = ContribIntegralTo
		} else if exampleCount > 0 {
			c.ContributionType = ContribExampleOf
		}
	}

	return c
}
