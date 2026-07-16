package synthesis_test

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/internal/synthesis"
	"github.com/complytime-labs/crosscodex/pkg/config"
)

// Generators shared across property specs.
var (
	typeGen         = rapid.SampledFrom([]string{"Technical", "Procedural", "Both", "None", "Unknown"})
	levelGen        = rapid.SampledFrom([]string{"Strategic", "Tactical", "Operational"})
	contribTypeGen  = rapid.SampledFrom([]string{"", "INTEGRAL_TO", "EXAMPLE_OF"})
	relationshipGen = rapid.SampledFrom([]string{
		"EQUIVALENT", "SUBSET_OF", "SUPERSET_OF", "INTERSECTS",
		"COMPLEMENTS", "RELATED_TO", "CONTRIBUTES_TO", "NO_RELATIONSHIP",
	})
)

// drawViabilityConfig draws a valid ViabilityConfig with factors in (0, 2].
func drawViabilityConfig(t *rapid.T) config.ViabilityConfig {
	return config.ViabilityConfig{
		TypeMismatchFactor: rapid.Float64Range(0.01, 2.0).Draw(t, "typeMismatchFactor"),
		SkipLevelFactor:    rapid.Float64Range(0.01, 2.0).Draw(t, "skipLevelFactor"),
		IntegralToFactor:   rapid.Float64Range(0.01, 2.0).Draw(t, "integralToFactor"),
	}
}

var _ = Describe("Property Specifications", Ordered, func() {

	// -----------------------------------------------------------------------
	// 1. Viability output bounded
	// -----------------------------------------------------------------------

	Context("ComputeViability — output bounded", func() {
		It("always returns a value in [0, score*4] for non-negative scores and config factors in (0,2]", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				score := rapid.Float64Range(0, 100).Draw(t, "score")
				srcType := typeGen.Draw(t, "srcType")
				tgtType := typeGen.Draw(t, "tgtType")
				srcLevel := levelGen.Draw(t, "srcLevel")
				tgtLevel := levelGen.Draw(t, "tgtLevel")
				cfg := drawViabilityConfig(t)

				v := synthesis.ComputeViability(score, srcType, tgtType, srcLevel, tgtLevel, cfg)

				// Viability is score * typeFactor * levelFactor, where factors
				// are in (0, 2]. With non-negative score, viability >= 0.
				if v < 0 {
					t.Fatalf("viability %f < 0 for score %f", v, score)
				}
				// Upper bound: score * max_factor * max_factor = score * 2.0 * 2.0
				// But since factors come from config (max 2.0) and hardcoded (max 1.0),
				// the true upper bound is score * max(cfg factors, 1.0) * max(cfg factors, 1.0).
				// Most conservatively: score * 2.0 * 2.0 = score * 4.0.
				// But the typeFactor/levelFactor functions cap at max(1.0, cfg.Factor),
				// and cfg.Factor <= 2.0, so viability <= score * 2.0 * 2.0 always holds.
				upperBound := score * 4.0
				if v > upperBound+0.01 { // +0.01 for rounding tolerance
					t.Fatalf("viability %f exceeds upper bound %f for score %f", v, upperBound, score)
				}
			})
		})

		It("never returns negative for non-negative scores", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				score := rapid.Float64Range(0, 1000).Draw(t, "score")
				srcType := typeGen.Draw(t, "srcType")
				tgtType := typeGen.Draw(t, "tgtType")
				srcLevel := levelGen.Draw(t, "srcLevel")
				tgtLevel := levelGen.Draw(t, "tgtLevel")
				cfg := drawViabilityConfig(t)

				v := synthesis.ComputeViability(score, srcType, tgtType, srcLevel, tgtLevel, cfg)
				if v < 0 {
					t.Fatalf("ComputeViability(%f, ...) = %f, want >= 0", score, v)
				}

				w := synthesis.ComputeViabilityWeight(score, srcType, tgtType, srcLevel, tgtLevel,
					contribTypeGen.Draw(t, "contribType"), cfg)
				if w < 0 {
					t.Fatalf("ComputeViabilityWeight(%f, ...) = %f, want >= 0", score, w)
				}
			})
		})
	})

	// -----------------------------------------------------------------------
	// 2. Determinism
	// -----------------------------------------------------------------------

	Context("ComputeViability — determinism", func() {
		It("produces identical results for identical inputs", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				score := rapid.Float64Range(0, 100).Draw(t, "score")
				srcType := typeGen.Draw(t, "srcType")
				tgtType := typeGen.Draw(t, "tgtType")
				srcLevel := levelGen.Draw(t, "srcLevel")
				tgtLevel := levelGen.Draw(t, "tgtLevel")
				contribType := contribTypeGen.Draw(t, "contribType")
				cfg := drawViabilityConfig(t)

				v1 := synthesis.ComputeViability(score, srcType, tgtType, srcLevel, tgtLevel, cfg)
				v2 := synthesis.ComputeViability(score, srcType, tgtType, srcLevel, tgtLevel, cfg)
				if v1 != v2 {
					t.Fatalf("ComputeViability not deterministic: %f != %f", v1, v2)
				}

				w1 := synthesis.ComputeViabilityWeight(score, srcType, tgtType, srcLevel, tgtLevel, contribType, cfg)
				w2 := synthesis.ComputeViabilityWeight(score, srcType, tgtType, srcLevel, tgtLevel, contribType, cfg)
				if w1 != w2 {
					t.Fatalf("ComputeViabilityWeight not deterministic: %f != %f", w1, w2)
				}
			})
		})
	})

	// -----------------------------------------------------------------------
	// 3. Zero score always produces zero viability
	// -----------------------------------------------------------------------

	Context("ComputeViability — zero score", func() {
		It("always returns 0 when score is 0", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				srcType := typeGen.Draw(t, "srcType")
				tgtType := typeGen.Draw(t, "tgtType")
				srcLevel := levelGen.Draw(t, "srcLevel")
				tgtLevel := levelGen.Draw(t, "tgtLevel")
				contribType := contribTypeGen.Draw(t, "contribType")
				cfg := drawViabilityConfig(t)

				v := synthesis.ComputeViability(0, srcType, tgtType, srcLevel, tgtLevel, cfg)
				if v != 0 {
					t.Fatalf("ComputeViability(0, ...) = %f, want 0", v)
				}

				w := synthesis.ComputeViabilityWeight(0, srcType, tgtType, srcLevel, tgtLevel, contribType, cfg)
				if w != 0 {
					t.Fatalf("ComputeViabilityWeight(0, ...) = %f, want 0", w)
				}
			})
		})
	})

	// -----------------------------------------------------------------------
	// 4. Weight >= viability when INTEGRAL_TO and factor >= 1.0
	// -----------------------------------------------------------------------

	Context("ComputeViabilityWeight — INTEGRAL_TO boost", func() {
		It("weight >= viability when contribType is INTEGRAL_TO and IntegralToFactor >= 1.0", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				score := rapid.Float64Range(0, 100).Draw(t, "score")
				srcType := typeGen.Draw(t, "srcType")
				tgtType := typeGen.Draw(t, "tgtType")
				srcLevel := levelGen.Draw(t, "srcLevel")
				tgtLevel := levelGen.Draw(t, "tgtLevel")
				cfg := config.ViabilityConfig{
					TypeMismatchFactor: rapid.Float64Range(0.01, 2.0).Draw(t, "typeMismatchFactor"),
					SkipLevelFactor:    rapid.Float64Range(0.01, 2.0).Draw(t, "skipLevelFactor"),
					IntegralToFactor:   rapid.Float64Range(1.0, 2.0).Draw(t, "integralToFactor"),
				}

				viability := synthesis.ComputeViability(score, srcType, tgtType, srcLevel, tgtLevel, cfg)
				weight := synthesis.ComputeViabilityWeight(score, srcType, tgtType, srcLevel, tgtLevel, "INTEGRAL_TO", cfg)

				// weight = round2(viability * IntegralToFactor)
				// When IntegralToFactor >= 1.0 and viability >= 0, weight >= viability.
				// Rounding can cause a tiny decrease, but at most 0.005.
				if weight < viability-0.01 {
					t.Fatalf("weight %f < viability %f (INTEGRAL_TO factor=%f)",
						weight, viability, cfg.IntegralToFactor)
				}
			})
		})
	})

	// -----------------------------------------------------------------------
	// 5. Diagnostic count bounded
	// -----------------------------------------------------------------------

	Context("Assessor — diagnostic count", func() {
		It("always produces exactly 4 diagnostics for non-empty input or 0 for empty", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				n := rapid.IntRange(0, 20).Draw(t, "rowCount")
				rows := make([]synthesis.SynthesisRow, n)
				for i := range rows {
					rows[i] = synthesis.SynthesisRow{
						SourceID:              fmt.Sprintf("src-%d", i),
						TargetID:              fmt.Sprintf("tgt-%d", i),
						SimilarityMean:        rapid.Float64Range(0, 100).Draw(t, fmt.Sprintf("sim-%d", i)),
						ViabilityWeight:       rapid.Float64Range(0, 100).Draw(t, fmt.Sprintf("vw-%d", i)),
						ConsensusRelationship: relationshipGen.Draw(t, fmt.Sprintf("rel-%d", i)),
						ConfidenceFraction:    rapid.Float64Range(0, 1).Draw(t, fmt.Sprintf("conf-%d", i)),
						Unanimous:             rapid.Bool().Draw(t, fmt.Sprintf("unan-%d", i)),
					}
				}

				cfg := defaultAssessmentConfig()
				actionableTypes := []string{"EQUIVALENT", "SUBSET_OF", "SUPERSET_OF"}
				assessor := synthesis.NewAssessor(cfg, actionableTypes)
				report := assessor.Assess(context.Background(), rows)

				if n == 0 {
					if len(report.Diagnostics) != 0 {
						t.Fatalf("expected 0 diagnostics for empty input, got %d", len(report.Diagnostics))
					}
				} else {
					if len(report.Diagnostics) != 4 {
						t.Fatalf("expected 4 diagnostics for %d rows, got %d", n, len(report.Diagnostics))
					}
				}
			})
		})
	})

	// -----------------------------------------------------------------------
	// 6. Relationship count sum equals total pairs
	// -----------------------------------------------------------------------

	Context("Assessor — relationship count sum", func() {
		It("sum of all RelationshipCounts values equals TotalPairs", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				n := rapid.IntRange(1, 20).Draw(t, "rowCount")
				rows := make([]synthesis.SynthesisRow, n)
				for i := range rows {
					rows[i] = synthesis.SynthesisRow{
						SourceID:              fmt.Sprintf("src-%d", i),
						TargetID:              fmt.Sprintf("tgt-%d", i),
						SimilarityMean:        rapid.Float64Range(0, 100).Draw(t, fmt.Sprintf("sim-%d", i)),
						ViabilityWeight:       rapid.Float64Range(0, 100).Draw(t, fmt.Sprintf("vw-%d", i)),
						ConsensusRelationship: relationshipGen.Draw(t, fmt.Sprintf("rel-%d", i)),
						ConfidenceFraction:    rapid.Float64Range(0, 1).Draw(t, fmt.Sprintf("conf-%d", i)),
						Unanimous:             rapid.Bool().Draw(t, fmt.Sprintf("unan-%d", i)),
					}
				}

				cfg := defaultAssessmentConfig()
				assessor := synthesis.NewAssessor(cfg, []string{"EQUIVALENT"})
				report := assessor.Assess(context.Background(), rows)

				sum := 0
				for _, count := range report.RelationshipCounts {
					sum += count
				}
				if sum != report.TotalPairs {
					t.Fatalf("sum of RelationshipCounts (%d) != TotalPairs (%d)", sum, report.TotalPairs)
				}
			})
		})
	})

	// -----------------------------------------------------------------------
	// 7. ViablePairs <= TotalPairs
	// -----------------------------------------------------------------------

	Context("Assessor — ViablePairs bounded", func() {
		It("ViablePairs <= TotalPairs always holds", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				n := rapid.IntRange(0, 20).Draw(t, "rowCount")
				rows := make([]synthesis.SynthesisRow, n)
				for i := range rows {
					rows[i] = synthesis.SynthesisRow{
						SourceID:              fmt.Sprintf("src-%d", i),
						TargetID:              fmt.Sprintf("tgt-%d", i),
						SimilarityMean:        rapid.Float64Range(0, 100).Draw(t, fmt.Sprintf("sim-%d", i)),
						ViabilityWeight:       rapid.Float64Range(-10, 100).Draw(t, fmt.Sprintf("vw-%d", i)),
						ConsensusRelationship: relationshipGen.Draw(t, fmt.Sprintf("rel-%d", i)),
						ConfidenceFraction:    rapid.Float64Range(0, 1).Draw(t, fmt.Sprintf("conf-%d", i)),
						Unanimous:             rapid.Bool().Draw(t, fmt.Sprintf("unan-%d", i)),
					}
				}

				cfg := defaultAssessmentConfig()
				assessor := synthesis.NewAssessor(cfg, []string{"EQUIVALENT"})
				report := assessor.Assess(context.Background(), rows)

				if report.ViablePairs > report.TotalPairs {
					t.Fatalf("ViablePairs (%d) > TotalPairs (%d)", report.ViablePairs, report.TotalPairs)
				}
			})
		})
	})

	// -----------------------------------------------------------------------
	// 8. No negative values in assessor output
	// -----------------------------------------------------------------------

	Context("Assessor — no negative output values", func() {
		It("ViablePairs, AvgConfidence, and AvgViability are non-negative for valid inputs", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				n := rapid.IntRange(1, 20).Draw(t, "rowCount")
				rows := make([]synthesis.SynthesisRow, n)
				for i := range rows {
					rows[i] = synthesis.SynthesisRow{
						SourceID:              fmt.Sprintf("src-%d", i),
						TargetID:              fmt.Sprintf("tgt-%d", i),
						SimilarityMean:        rapid.Float64Range(0, 100).Draw(t, fmt.Sprintf("sim-%d", i)),
						ViabilityWeight:       rapid.Float64Range(0, 100).Draw(t, fmt.Sprintf("vw-%d", i)),
						ConsensusRelationship: relationshipGen.Draw(t, fmt.Sprintf("rel-%d", i)),
						ConfidenceFraction:    rapid.Float64Range(0, 1).Draw(t, fmt.Sprintf("conf-%d", i)),
						Unanimous:             rapid.Bool().Draw(t, fmt.Sprintf("unan-%d", i)),
					}
				}

				cfg := defaultAssessmentConfig()
				assessor := synthesis.NewAssessor(cfg, []string{"EQUIVALENT"})
				report := assessor.Assess(context.Background(), rows)

				if report.ViablePairs < 0 {
					t.Fatalf("ViablePairs %d < 0", report.ViablePairs)
				}
				if report.AvgConfidence < 0 {
					t.Fatalf("AvgConfidence %f < 0", report.AvgConfidence)
				}
				if report.AvgViability < 0 {
					t.Fatalf("AvgViability %f < 0", report.AvgViability)
				}
			})
		})
	})

	// -----------------------------------------------------------------------
	// Bonus: Ranker output length equals input length
	// -----------------------------------------------------------------------

	Context("Ranker — output length preservation", func() {
		It("always produces exactly len(inputs) rows", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				n := rapid.IntRange(0, 20).Draw(t, "inputCount")
				inputs := make([]synthesis.SynthesisInput, n)
				classifications := make(map[string]synthesis.Classification)

				for i := range inputs {
					srcID := fmt.Sprintf("src-%d", i)
					tgtID := fmt.Sprintf("tgt-%d", i)
					inputs[i] = synthesis.SynthesisInput{
						SourceID:              srcID,
						TargetID:              tgtID,
						SimilarityScore:       rapid.Float64Range(0, 100).Draw(t, fmt.Sprintf("score-%d", i)),
						SimilarityMedian:      rapid.Float64Range(0, 100).Draw(t, fmt.Sprintf("median-%d", i)),
						SimilarityVar:         rapid.Float64Range(0, 50).Draw(t, fmt.Sprintf("var-%d", i)),
						SimilarityCount:       rapid.IntRange(0, 10).Draw(t, fmt.Sprintf("count-%d", i)),
						ConsensusRelationship: relationshipGen.Draw(t, fmt.Sprintf("rel-%d", i)),
						ContributionType:      contribTypeGen.Draw(t, fmt.Sprintf("contrib-%d", i)),
						ConfidenceFraction:    rapid.Float64Range(0, 1).Draw(t, fmt.Sprintf("conf-%d", i)),
						Unanimous:             rapid.Bool().Draw(t, fmt.Sprintf("unan-%d", i)),
					}
					classifications[srcID] = synthesis.Classification{
						Type:  typeGen.Draw(t, fmt.Sprintf("srcType-%d", i)),
						Level: levelGen.Draw(t, fmt.Sprintf("srcLevel-%d", i)),
					}
					classifications[tgtID] = synthesis.Classification{
						Type:  typeGen.Draw(t, fmt.Sprintf("tgtType-%d", i)),
						Level: levelGen.Draw(t, fmt.Sprintf("tgtLevel-%d", i)),
					}
				}

				cfg := drawViabilityConfig(t)
				ranker := synthesis.NewRanker(cfg)
				rows := ranker.Rank(context.Background(), "job-prop", inputs, classifications)

				if len(rows) != n {
					t.Fatalf("expected %d rows, got %d", n, len(rows))
				}
			})
		})
	})
})
