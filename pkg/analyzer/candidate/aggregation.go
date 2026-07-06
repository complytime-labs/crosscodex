package candidate

import "fmt"

// aggregate combines candidates from multiple generators using the specified strategy
func aggregate(candidateSets [][]Candidate, strategy AggregationStrategy, cfg *generateConfig) ([]Candidate, error) {
	switch strategy {
	case StrategyUnion:
		return aggregateUnion(candidateSets)
	case StrategyWeightedUnion:
		return aggregateWeightedUnion(candidateSets, cfg.minScore)
	default:
		return nil, fmt.Errorf("unknown aggregation strategy: %q", strategy)
	}
}

// aggregateUnion returns all unique candidates from any generator
func aggregateUnion(candidateSets [][]Candidate) ([]Candidate, error) {
	// Use map to deduplicate by (SourceID, TargetID) pair
	seen := make(map[string]Candidate)

	for _, candidates := range candidateSets {
		for _, c := range candidates {
			key := pairKey(c.SourceID, c.TargetID)
			if _, exists := seen[key]; !exists {
				seen[key] = c
			}
		}
	}

	// Convert map to slice
	result := make([]Candidate, 0, len(seen))
	for _, c := range seen {
		result = append(result, c)
	}

	return result, nil
}

// aggregateWeightedUnion combines scores from multiple generators and filters by threshold
func aggregateWeightedUnion(candidateSets [][]Candidate, minScore float64) ([]Candidate, error) {
	// Group candidates by (SourceID, TargetID) pair
	type pairData struct {
		sourceID      string
		targetID      string
		weightedSum   float64
		totalWeight   float64 // sum(weight)
		contributions []Candidate
	}

	pairs := make(map[string]*pairData)

	for _, candidates := range candidateSets {
		for _, c := range candidates {
			key := pairKey(c.SourceID, c.TargetID)

			if _, exists := pairs[key]; !exists {
				pairs[key] = &pairData{
					sourceID:      c.SourceID,
					targetID:      c.TargetID,
					contributions: []Candidate{},
				}
			}

			pd := pairs[key]
			pd.weightedSum += c.Weight * c.Score
			pd.totalWeight += c.Weight
			pd.contributions = append(pd.contributions, c)
		}
	}

	// Filter by normalized score threshold and build result
	var result []Candidate

	for _, pd := range pairs {
		if pd.totalWeight == 0 {
			continue
		}

		// Normalize: weighted average
		normalizedScore := pd.weightedSum / pd.totalWeight

		if normalizedScore >= minScore {
			// Build aggregated candidate
			// Use first contribution as base, but update score
			aggCandidate := pd.contributions[0]
			aggCandidate.Score = normalizedScore
			// For metadata, we could merge all contributions, but for now keep it simple
			result = append(result, aggCandidate)
		}
	}

	return result, nil
}

// pairKey creates a unique key for a (source, target) pair
func pairKey(sourceID, targetID string) string {
	return sourceID + ":" + targetID
}
