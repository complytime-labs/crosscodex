package pipeline

import (
	"sort"

	"gonum.org/v1/gonum/stat"

	"github.com/complytime-labs/crosscodex/internal/synthesis"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
)

// conventionalMedian returns the textbook median (numpy/R "type 7" quantile
// definition) of an already-sorted, non-empty slice: the middle element for
// an odd-length slice, the average of the two middle elements for an
// even-length slice. gonum's stat.Quantile deliberately does not implement
// this: its two CumulantKinds are Empirical (nearest-rank; h = N*p, "type 1")
// and LinInterp (h = N*p with linear interpolation, "type 4" in the
// Hyndman-Fan taxonomy) — at p=0.5 neither reduces to averaging the two
// middle values for an even-length sample (e.g. both return 80, not 85, for
// [80, 90]; verified against gonum v0.17.0). Computing the median directly
// here is the correct choice, not a hand-rolled substitute for a gonum
// capability that exists — gonum has no CumulantKind for the conventional
// median.
func conventionalMedian(sorted []float64) float64 {
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// similarityMatrix mirrors the anonymous struct shape CandidateGenerator.Generate
// already builds for candidate.GenerateRequest.EmbeddingMatrix (IDs/Values,
// reflection-read by the candidate generators) — used here to look up one
// pair's similarity value per configured embedding model.
type similarityMatrix struct {
	IDs    []string
	Values [][]float32
}

// buildSynthesisInputs constructs synthesis.Service.Execute's inputs and
// classifications from persisted classify/requires/relationship results and
// per-model embedding similarity matrices, per the durable-pipeline design's
// §5.1 table. A pair present in both requiresResults and relResults
// contributes two distinct rows (different ContributionType), matching
// Ranker.Rank's per-row (not per-pair) contract.
func buildSynthesisInputs(
	classifyResults []results.ClassifyResult,
	requiresResults []results.RequiresResult,
	relResults []results.SemanticMatchResult,
	similarityByModel map[string]similarityMatrix,
) ([]synthesis.SynthesisInput, map[string]synthesis.Classification) {
	classifications := make(map[string]synthesis.Classification, len(classifyResults))
	for _, c := range classifyResults {
		classifications[c.ControlID] = synthesis.Classification{Type: c.Type, Level: c.Level}
	}

	// Pre-index each model's matrix by control ID for O(1) pair lookups.
	type indexedMatrix struct {
		index  map[string]int
		values [][]float32
	}
	indexed := make(map[string]indexedMatrix, len(similarityByModel))
	for model, m := range similarityByModel {
		idx := make(map[string]int, len(m.IDs))
		for i, id := range m.IDs {
			idx[id] = i
		}
		indexed[model] = indexedMatrix{index: idx, values: m.Values}
	}

	similarityStats := func(sourceID, targetID string) (mean, median, variance float64, count int) {
		var values []float64
		modelNames := make([]string, 0, len(indexed))
		for model := range indexed {
			modelNames = append(modelNames, model)
		}
		sort.Strings(modelNames) // deterministic iteration for reproducible stats
		for _, model := range modelNames {
			m := indexed[model]
			si, sok := m.index[sourceID]
			ti, tok := m.index[targetID]
			if !sok || !tok {
				continue
			}
			values = append(values, float64(m.values[si][ti]))
		}
		count = len(values)
		if count == 0 {
			return 0, 0, 0, 0
		}
		sorted := append([]float64(nil), values...)
		sort.Float64s(sorted)
		mean = stat.Mean(values, nil)
		median = conventionalMedian(sorted)
		if count > 1 {
			variance = stat.Variance(values, nil)
		}
		return mean, median, variance, count
	}

	var inputs []synthesis.SynthesisInput
	for _, r := range requiresResults {
		mean, median, variance, count := similarityStats(r.SourceID, r.TargetID)
		inputs = append(inputs, synthesis.SynthesisInput{
			SourceID:              r.SourceID,
			TargetID:              r.TargetID,
			SimilarityScore:       mean,
			SimilarityMedian:      median,
			SimilarityVar:         variance,
			SimilarityCount:       count,
			ConsensusRelationship: synthesis.ConsensusRequires,
			ContributionType:      "requires",
			ConfidenceFraction:    r.Confidence,
			Unanimous:             r.Unanimous,
		})
	}
	for _, r := range relResults {
		mean, median, variance, count := similarityStats(r.SourceID, r.TargetID)
		inputs = append(inputs, synthesis.SynthesisInput{
			SourceID:              r.SourceID,
			TargetID:              r.TargetID,
			SimilarityScore:       mean,
			SimilarityMedian:      median,
			SimilarityVar:         variance,
			SimilarityCount:       count,
			ConsensusRelationship: r.RelationshipType,
			ContributionType:      "relationship",
			ConfidenceFraction:    r.Confidence,
			Unanimous:             true,
		})
	}
	return inputs, classifications
}
