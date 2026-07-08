package artifacts

import (
	"math"
	"sort"

	intanalyzer "github.com/complytime-labs/crosscodex/internal/analyzer"
)

type taggedArtifact struct {
	voterKey string
	artifact ExtractedArtifact
}

// ComputeConsensus aggregates artifacts across model votes using fuzzy
// token-set matching to group semantically equivalent artifacts.
// This is a pure function — no context, no spans. The calling pipeline
// should create a wrapping span for trace coverage.
//
// Port of Python ArtifactExtractor._compute_consensus.
func ComputeConsensus(votes map[string]*Vote, totalVoters int, fuzzyThreshold float64) []ConsensusArtifact {
	if totalVoters == 0 {
		return nil
	}

	// Collect all artifacts from valid votes.
	// Sort vote keys to ensure deterministic iteration order.
	voteKeys := make([]string, 0, len(votes))
	for k := range votes {
		voteKeys = append(voteKeys, k)
	}
	sort.Strings(voteKeys)

	var all []taggedArtifact
	for _, k := range voteKeys {
		vote := votes[k]
		if vote == nil || vote.ParseStatus != ParseOK {
			continue
		}
		for _, art := range vote.Artifacts {
			all = append(all, taggedArtifact{voterKey: vote.VoteKey, artifact: art})
		}
	}

	if len(all) == 0 {
		return nil
	}

	// Greedy single-pass grouping by fuzzy name match.
	var groups [][]taggedArtifact
	for _, item := range all {
		normName := intanalyzer.NormalizeArtifactName(item.artifact.Name)
		matched := false
		for i, group := range groups {
			repName := intanalyzer.NormalizeArtifactName(group[0].artifact.Name)
			if intanalyzer.ArtifactNamesMatch(normName, repName, fuzzyThreshold) {
				groups[i] = append(groups[i], item)
				matched = true
				break
			}
		}
		if !matched {
			groups = append(groups, []taggedArtifact{item})
		}
	}

	// Build consensus artifacts from groups.
	result := make([]ConsensusArtifact, 0, len(groups))
	for _, group := range groups {
		ca := buildConsensusFromGroup(group, totalVoters)
		result = append(result, ca)
	}

	// Sort by confidence descending, then by name ascending for deterministic ordering.
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Confidence != result[j].Confidence {
			return result[i].Confidence > result[j].Confidence
		}
		return result[i].Name < result[j].Name
	})

	return result
}

func buildConsensusFromGroup(group []taggedArtifact, totalVoters int) ConsensusArtifact {
	// Unique voters.
	voterSet := make(map[string]bool)
	for _, item := range group {
		voterSet[item.voterKey] = true
	}
	voterKeys := make([]string, 0, len(voterSet))
	for k := range voterSet {
		voterKeys = append(voterKeys, k)
	}
	sort.Strings(voterKeys)

	confidence := math.Round(float64(len(voterSet))/float64(totalVoters)*1000) / 1000

	// Type: majority vote, tiebreak by enum priority (lower iota wins).
	typeCounts := make(map[ArtifactType]int)
	for _, item := range group {
		typeCounts[item.artifact.Type]++
	}
	maxCount := 0
	for _, count := range typeCounts {
		if count > maxCount {
			maxCount = count
		}
	}
	winner := ArtifactProcess // lowest priority default
	for _, at := range AllArtifactTypes() {
		if typeCounts[at] == maxCount {
			winner = at
			break
		}
	}

	// Name: first non-empty.
	var name string
	for _, item := range group {
		if item.artifact.Name != "" {
			name = item.artifact.Name
			break
		}
	}

	// Frequency: first non-empty.
	var frequency string
	for _, item := range group {
		if item.artifact.Frequency != "" {
			frequency = item.artifact.Frequency
			break
		}
	}

	// OwnerRole: first non-empty.
	var ownerRole string
	for _, item := range group {
		if item.artifact.OwnerRole != "" {
			ownerRole = item.artifact.OwnerRole
			break
		}
	}

	// Description: longest.
	var description string
	for _, item := range group {
		if len(item.artifact.Description) > len(description) {
			description = item.artifact.Description
		}
	}

	// Properties: first-writer-wins per key.
	var properties map[string]string
	for _, item := range group {
		for k, v := range item.artifact.Properties {
			if properties == nil {
				properties = make(map[string]string)
			}
			if _, exists := properties[k]; !exists {
				properties[k] = v
			}
		}
	}

	return ConsensusArtifact{
		Name:        name,
		Type:        winner,
		Frequency:   frequency,
		OwnerRole:   ownerRole,
		Description: description,
		Confidence:  confidence,
		VoterKeys:   voterKeys,
		VoteCount:   len(voterSet),
		Unanimous:   len(voterSet) == totalVoters,
		Properties:  properties,
	}
}
