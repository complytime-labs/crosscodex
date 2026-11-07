package candidate

import (
	"fmt"
	"sort"
	"strings"
)

// aggregate combines candidates from multiple generators using the specified strategy
func aggregate(candidateSets [][]Candidate, strategy AggregationStrategy, cfg *generateConfig) ([]Candidate, error) {
	switch strategy {
	case StrategyUnion:
		return aggregateUnion(candidateSets)
	case StrategyWeightedUnion:
		return aggregateWeightedUnion(candidateSets, cfg.minScore)
	case StrategyRankFusion:
		return aggregateRankFusion(candidateSets, cfg.maxPerSource)
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

// rrfK is the reciprocal rank fusion smoothing constant from Cormack et al. (2009).
const rrfK = 60.0

// aggregateRankFusion scores each (source, target) pair as sum(weight / (rrfK + rank)),
// where rank is the pair's fractional position within one generator's candidates for
// that source, then keeps the top maxPerSource pairs per source. Output is ordered by
// source ID, fused score descending, target ID.
func aggregateRankFusion(candidateSets [][]Candidate, maxPerSource int) ([]Candidate, error) {
	if maxPerSource < 1 {
		return nil, fmt.Errorf("rank fusion: max per source %d must be at least 1", maxPerSource)
	}

	pairs := make(map[string]*Candidate)
	for _, set := range candidateSets {
		bySource := make(map[string][]Candidate)
		for _, c := range set {
			bySource[c.SourceID] = append(bySource[c.SourceID], c)
		}
		for _, group := range bySource {
			for i, rank := range fractionalRanks(group) {
				c := group[i]
				key := pairKey(c.SourceID, c.TargetID)
				fused, ok := pairs[key]
				if !ok {
					fused = &Candidate{SourceID: c.SourceID, TargetID: c.TargetID, Weight: 1}
					pairs[key] = fused
				}
				fused.Contributions = append(fused.Contributions, Contribution{
					GeneratorID: c.GeneratorID,
					Score:       c.Score,
					Weight:      c.Weight,
					Rank:        rank,
					Metadata:    c.Metadata,
				})
			}
		}
	}

	perSource := make(map[string][]Candidate)
	for _, fused := range pairs {
		sort.Slice(fused.Contributions, func(i, j int) bool {
			return fused.Contributions[i].GeneratorID < fused.Contributions[j].GeneratorID
		})
		names := make([]string, len(fused.Contributions))
		for i, contribution := range fused.Contributions {
			fused.Score += contribution.Weight / (rrfK + contribution.Rank)
			names[i] = contribution.GeneratorID
		}
		fused.GeneratorID = strings.Join(names, "+")
		perSource[fused.SourceID] = append(perSource[fused.SourceID], *fused)
	}

	sourceIDs := make([]string, 0, len(perSource))
	for sourceID := range perSource {
		sourceIDs = append(sourceIDs, sourceID)
	}
	sort.Strings(sourceIDs)

	result := make([]Candidate, 0, len(pairs))
	for _, sourceID := range sourceIDs {
		group := perSource[sourceID]
		sort.Slice(group, func(i, j int) bool {
			if group[i].Score != group[j].Score {
				return group[i].Score > group[j].Score
			}
			return group[i].TargetID < group[j].TargetID
		})
		if len(group) > maxPerSource {
			group = group[:maxPerSource]
		}
		result = append(result, group...)
	}
	return result, nil
}

// fractionalRanks returns each candidate's 1-based rank by Score descending
// (ties broken by TargetID for ordering), with tied scores sharing the mean of
// the positions they span. The result is indexed like the input.
func fractionalRanks(group []Candidate) []float64 {
	order := make([]int, len(group))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool {
		ca, cb := group[order[a]], group[order[b]]
		if ca.Score != cb.Score {
			return ca.Score > cb.Score
		}
		return ca.TargetID < cb.TargetID
	})

	ranks := make([]float64, len(group))
	for i := 0; i < len(order); {
		j := i
		for j+1 < len(order) && group[order[j+1]].Score == group[order[i]].Score {
			j++
		}
		mean := float64(i+1+j+1) / 2
		for k := i; k <= j; k++ {
			ranks[order[k]] = mean
		}
		i = j + 1
	}
	return ranks
}
