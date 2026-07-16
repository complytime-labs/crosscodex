package synthesis

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

// defaultViabilityCfg provides a valid ViabilityConfig for fuzz tests.
// Config validation is tested separately in pkg/config; fuzz tests exercise
// the compute functions themselves.
func defaultViabilityCfg() config.ViabilityConfig {
	return config.ViabilityConfig{
		TypeMismatchFactor: 0.8,
		SkipLevelFactor:    0.7,
		IntegralToFactor:   1.1,
	}
}

// defaultAssessmentCfg provides a valid AssessmentConfig for fuzz tests.
func defaultAssessmentCfg() config.AssessmentConfig {
	return config.AssessmentConfig{
		IQRGood:        20.0,
		IQRPoor:        10.0,
		NoRelHigh:      0.97,
		NoRelLow:       0.80,
		ContestedWarn:  0.20,
		ActionableWarn: 0.30,
	}
}

// FuzzComputeViability verifies ComputeViability never panics on arbitrary
// float64 scores and string type/level inputs.
func FuzzComputeViability(f *testing.F) {
	// Seed corpus: valid, empty, mixed, negative, large.
	f.Add(50.0, "Technical", "Technical", "Strategic", "Strategic")
	f.Add(0.0, "", "", "", "")
	f.Add(100.0, "Both", "None", "Operational", "Strategic")
	f.Add(-5.0, "Unknown", "Unknown", "", "")
	f.Add(999999.0, "Procedural", "Technical", "Tactical", "Tactical")
	f.Add(1e-300, "None", "Both", "Operational", "Operational")

	cfg := defaultViabilityCfg()

	f.Fuzz(func(t *testing.T, score float64, srcType, tgtType, srcLevel, tgtLevel string) {
		if math.IsNaN(score) || math.IsInf(score, 0) {
			return
		}

		result := ComputeViability(score, srcType, tgtType, srcLevel, tgtLevel, cfg)

		if math.IsNaN(result) {
			t.Fatalf("ComputeViability returned NaN for score=%v srcType=%q tgtType=%q srcLevel=%q tgtLevel=%q",
				score, srcType, tgtType, srcLevel, tgtLevel)
		}

		// For non-negative scores, viability must also be non-negative.
		if score >= 0 && result < 0 {
			t.Fatalf("ComputeViability returned negative result %v for non-negative score %v", result, score)
		}
	})
}

// FuzzComputeViabilityWeight verifies ComputeViabilityWeight never panics and
// maintains the invariant that weight >= viability when IntegralToFactor >= 1.
func FuzzComputeViabilityWeight(f *testing.F) {
	// Seed corpus: boost, no boost, zero, boundary, negative.
	f.Add(80.0, "Technical", "Technical", "Tactical", "Tactical", "INTEGRAL_TO")
	f.Add(50.0, "None", "Unknown", "Strategic", "Operational", "")
	f.Add(0.0, "", "", "", "", "EXAMPLE_OF")
	f.Add(100.0, "Both", "Procedural", "Strategic", "Tactical", "INTEGRAL_TO")
	f.Add(-10.0, "Procedural", "Technical", "Operational", "Strategic", "INTEGRAL_TO")
	f.Add(1e-15, "Unknown", "None", "Tactical", "Tactical", "")

	cfg := defaultViabilityCfg()

	f.Fuzz(func(t *testing.T, score float64, srcType, tgtType, srcLevel, tgtLevel, contribType string) {
		if math.IsNaN(score) || math.IsInf(score, 0) {
			return
		}

		viability := ComputeViability(score, srcType, tgtType, srcLevel, tgtLevel, cfg)
		weight := ComputeViabilityWeight(score, srcType, tgtType, srcLevel, tgtLevel, contribType, cfg)

		if math.IsNaN(weight) {
			t.Fatalf("ComputeViabilityWeight returned NaN for score=%v contribType=%q",
				score, contribType)
		}

		// The weight >= viability invariant only holds for INTEGRAL_TO,
		// which applies the IntegralToFactor boost. Other types use factor=1.0
		// and rounding can produce weight slightly below viability.
		if contribType == "INTEGRAL_TO" && cfg.IntegralToFactor >= 1.0 && viability >= 0 {
			if weight < viability-1e-9 {
				t.Fatalf("weight (%v) < viability (%v) with IntegralToFactor=%v, score=%v, contribType=%q",
					weight, viability, cfg.IntegralToFactor, score, contribType)
			}
		}
	})
}

// FuzzAssess verifies the Assessor never panics on arbitrary row data and
// maintains TotalPairs >= ViablePairs.
func FuzzAssess(f *testing.F) {
	// Each seed builds one row from primitives:
	// (score, confidence, viabilityWeight, relationship, unanimous)
	f.Add(75.0, 0.8, 42.0, "EQUIVALENT", true)
	f.Add(0.0, 0.0, 0.0, "NO_RELATIONSHIP", false)
	f.Add(100.0, 1.0, 99.9, "SUBSET_OF", true)
	f.Add(-5.0, -1.0, -10.0, "", false)
	f.Add(50.0, 0.5, 0.001, "CONTRIBUTES_TO", true)
	f.Add(999999.0, 999999.0, 999999.0, "INTEGRAL_TO", false)

	assessor := NewAssessor(defaultAssessmentCfg(), []string{
		"EQUIVALENT", "SUBSET_OF", "SUPERSET_OF", "INTERSECTS",
		"COMPLEMENTS", "RELATED_TO", "CONTRIBUTES_TO",
	})

	f.Fuzz(func(t *testing.T, score, confidence, viabilityWeight float64, relationship string, unanimous bool) {
		if math.IsNaN(score) || math.IsInf(score, 0) ||
			math.IsNaN(confidence) || math.IsInf(confidence, 0) ||
			math.IsNaN(viabilityWeight) || math.IsInf(viabilityWeight, 0) {
			return
		}

		rows := []SynthesisRow{
			{
				JobID:                 "fuzz-job",
				SourceID:              "src-1",
				TargetID:              "tgt-1",
				SimilarityMean:        score,
				ConfidenceFraction:    confidence,
				ViabilityWeight:       viabilityWeight,
				ConsensusRelationship: relationship,
				Unanimous:             unanimous,
			},
		}

		report := assessor.Assess(context.Background(), rows)

		if report == nil {
			t.Fatal("Assess returned nil report")
		}
		if report.TotalPairs < report.ViablePairs {
			t.Fatalf("TotalPairs (%d) < ViablePairs (%d)", report.TotalPairs, report.ViablePairs)
		}
		if report.RelationshipCounts == nil {
			t.Fatal("RelationshipCounts is nil")
		}
		if report.Diagnostics == nil {
			t.Fatal("Diagnostics is nil")
		}
	})
}

// FuzzAssessMultiRow verifies the Assessor handles arbitrary multi-row
// inputs without panics, and that TotalPairs >= ViablePairs always holds.
func FuzzAssessMultiRow(f *testing.F) {
	// Seeds: (rowCount, score1, score2, score3, score4, rel)
	// rowCount=0 exercises the early-return branch (assessor.go:56-58).
	f.Add(0, 0.0, 0.0, 0.0, 0.0, "NO_RELATIONSHIP")
	f.Add(4, 75.0, 50.0, 25.0, 90.0, "EQUIVALENT")
	f.Add(1, 0.0, 0.0, 0.0, 0.0, "NO_RELATIONSHIP")
	f.Add(10, 100.0, 80.0, 60.0, 40.0, "SUBSET_OF")
	f.Add(4, -5.0, -10.0, -15.0, -20.0, "INTEGRAL_TO")

	assessor := NewAssessor(defaultAssessmentCfg(), []string{
		"EQUIVALENT", "SUBSET_OF", "SUPERSET_OF", "INTERSECTS",
		"COMPLEMENTS", "RELATED_TO", "CONTRIBUTES_TO",
	})

	f.Fuzz(func(t *testing.T, rowCount int, s1, s2, s3, s4 float64, relationship string) {
		if math.IsNaN(s1) || math.IsInf(s1, 0) || math.IsNaN(s2) || math.IsInf(s2, 0) ||
			math.IsNaN(s3) || math.IsInf(s3, 0) || math.IsNaN(s4) || math.IsInf(s4, 0) {
			return
		}
		if rowCount < 0 {
			rowCount = 0
		}
		if rowCount > 20 {
			rowCount = 20
		}

		scores := []float64{s1, s2, s3, s4}
		rows := make([]SynthesisRow, rowCount)
		for i := range rows {
			rows[i] = SynthesisRow{
				JobID:                 "fuzz-job",
				SourceID:              fmt.Sprintf("src-%d", i),
				TargetID:              fmt.Sprintf("tgt-%d", i),
				SimilarityMean:        scores[i%4],
				ViabilityWeight:       scores[i%4],
				ConsensusRelationship: relationship,
			}
		}

		report := assessor.Assess(context.Background(), rows)

		if report == nil {
			t.Fatal("Assess returned nil report")
		}
		if report.TotalPairs < report.ViablePairs {
			t.Fatalf("TotalPairs (%d) < ViablePairs (%d)", report.TotalPairs, report.ViablePairs)
		}
		if report.RelationshipCounts == nil {
			t.Fatal("RelationshipCounts is nil")
		}
		if report.Diagnostics == nil {
			t.Fatal("Diagnostics is nil")
		}
	})
}

// FuzzRank verifies the Ranker never panics on arbitrary inputs and always
// returns output length equal to input length.
func FuzzRank(f *testing.F) {
	// Each seed builds one input from primitives:
	// (sourceID, targetID, score, relationship, contribType, confidence, unanimous)
	f.Add("ctrl-a", "ctrl-b", 75.0, "EQUIVALENT", "INTEGRAL_TO", 0.8, true)
	f.Add("", "", 0.0, "", "", 0.0, false)
	f.Add("src", "tgt", -10.0, "NO_RELATIONSHIP", "EXAMPLE_OF", -1.0, true)
	f.Add("x", "y", 999999.0, "SUBSET_OF", "", 1.0, false)
	f.Add("a\x00b", "c\td", 50.0, "COMPLEMENTS", "INTEGRAL_TO", 0.5, true)

	ranker := NewRanker(defaultViabilityCfg())

	f.Fuzz(func(t *testing.T, sourceID, targetID string, score float64, relationship, contribType string, confidence float64, unanimous bool) {
		if math.IsNaN(score) || math.IsInf(score, 0) ||
			math.IsNaN(confidence) || math.IsInf(confidence, 0) {
			return
		}

		inputs := []SynthesisInput{
			{
				SourceID:              sourceID,
				TargetID:              targetID,
				SimilarityScore:       score,
				ConsensusRelationship: relationship,
				ContributionType:      contribType,
				ConfidenceFraction:    confidence,
				Unanimous:             unanimous,
			},
		}

		classifications := map[string]Classification{
			sourceID: {Type: "Technical", Level: "Strategic"},
			targetID: {Type: "Procedural", Level: "Tactical"},
		}

		rows := ranker.Rank(context.Background(), "fuzz-job", inputs, classifications)

		if len(rows) != len(inputs) {
			t.Fatalf("Rank output length (%d) != input length (%d)", len(rows), len(inputs))
		}

		for i, row := range rows {
			if math.IsNaN(row.ViabilityWeight) {
				t.Fatalf("row[%d].ViabilityWeight is NaN", i)
			}
		}
	})
}
