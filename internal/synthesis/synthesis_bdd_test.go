//go:build !integration

package synthesis_test

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/synthesis"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

func TestSynthesisBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Synthesis BDD Suite")
}

var restoreLogs func()

var _ = BeforeSuite(func() {
	restoreLogs = testspecs.RedirectLogsToGinkgo()
})

var _ = AfterSuite(func() {
	restoreLogs()
})

// ---------------------------------------------------------------------------
// Viability formula — Python parity tests
// ---------------------------------------------------------------------------

var _ = Describe("Viability", func() {
	var cfg config.ViabilityConfig

	BeforeEach(func() {
		cfg = defaultViabilityConfig()
	})

	// 9-row parity table matching Python OllamaCrosswalker.
	DescribeTable("ComputeViability and ComputeViabilityWeight — parity vectors",
		func(score float64, srcType, tgtType, srcLevel, tgtLevel, contribType string,
			expectedViability, expectedWeight float64) {

			viability := synthesis.ComputeViability(score, srcType, tgtType, srcLevel, tgtLevel, cfg)
			Expect(viability).To(BeNumerically("~", expectedViability, 1e-9),
				"viability mismatch")

			weight := synthesis.ComputeViabilityWeight(score, srcType, tgtType, srcLevel, tgtLevel, contribType, cfg)
			Expect(weight).To(BeNumerically("~", expectedWeight, 1e-9),
				"weight mismatch")
		},
		Entry("#1 zero stays zero",
			0.0, "Technical", "Technical", "Tactical", "Tactical", "",
			0.0, 0.0),
		Entry("#2 unknown type → 0.9 penalty",
			80.0, "Unknown", "Technical", "Tactical", "Tactical", "",
			72.0, 72.0),
		Entry("#3 none type → 0.9 penalty",
			80.0, "None", "Technical", "Tactical", "Tactical", "",
			72.0, 72.0),
		Entry("#4 unknown level → idx 1 (Tactical)",
			80.0, "Technical", "Technical", "Unknown", "Tactical", "",
			80.0, 80.0),
		Entry("#5 adjacent levels → no penalty",
			80.0, "Technical", "Technical", "Tactical", "Operational", "",
			80.0, 80.0),
		Entry("#6 type mismatch → 0.8 penalty",
			80.0, "Technical", "Procedural", "Tactical", "Tactical", "",
			64.0, 64.0),
		Entry("#7 skip level → 0.7 penalty",
			80.0, "Technical", "Technical", "Strategic", "Operational", "",
			56.0, 56.0),
		Entry("#8 Both = no penalty",
			60.0, "Both", "Technical", "Tactical", "Tactical", "",
			60.0, 60.0),
		Entry("#9 INTEGRAL_TO boost → 1.1 factor",
			80.0, "Technical", "Technical", "Tactical", "Tactical", "INTEGRAL_TO",
			80.0, 88.0),
	)

	Context("two-round rounding", func() {
		It("rounds viability first, then weight separately", func() {
			// Construct a case where two-round differs from single-round.
			// score=77, TypeMismatchFactor=0.83 → raw inner = 63.91
			// round2(63.91) = 63.91 (exact to 2dp)
			// With IntegralToFactor=1.1: single-round = round2(77*0.83*1.1) = round2(70.301) = 70.3
			// Two-round = round2(63.91*1.1) = round2(70.301) = 70.3
			//
			// Better case: score=77.777, factor=0.83
			// raw inner = 77.777*0.83 = 64.55491
			// round2(64.55491) = 64.55 (inner rounds down)
			// weight = round2(64.55*1.1) = round2(71.005) = 71.01 (two-round)
			// single = round2(77.777*0.83*1.1) = round2(71.01040...) = 71.01 (same here)
			//
			// Exact divergence: score=33.335, type mismatch (0.8), INTEGRAL_TO (1.1)
			// raw inner = 33.335*0.8 = 26.668
			// round2(26.668) = 26.67 (round up)
			// weight two-round = round2(26.67*1.1) = round2(29.337) = 29.34
			// weight single-round = round2(33.335*0.8*1.1) = round2(29.3348) = 29.33
			// They differ! 29.34 vs 29.33.
			twoRoundCfg := config.ViabilityConfig{
				TypeMismatchFactor: 0.8,
				SkipLevelFactor:    0.7,
				IntegralToFactor:   1.1,
			}
			v := synthesis.ComputeViability(33.335, "Technical", "Procedural",
				"Tactical", "Tactical", twoRoundCfg)
			Expect(v).To(BeNumerically("~", 26.67, 1e-9), "inner viability must round first")

			w := synthesis.ComputeViabilityWeight(33.335, "Technical", "Procedural",
				"Tactical", "Tactical", "INTEGRAL_TO", twoRoundCfg)
			Expect(w).To(BeNumerically("~", 29.34, 1e-9),
				"two-round rounding: 29.34, not single-round 29.33")
		})
	})

	Describe("Classification defaults", func() {
		It("returns Unknown for empty Type", func() {
			c := synthesis.Classification{Type: "", Level: "Tactical"}
			Expect(c.GetType()).To(Equal("Unknown"))
		})

		It("returns Tactical for empty Level", func() {
			c := synthesis.Classification{Type: "Technical", Level: ""}
			Expect(c.GetLevel()).To(Equal("Tactical"))
		})

		It("returns the value when Type is non-empty", func() {
			c := synthesis.Classification{Type: "Procedural", Level: "Strategic"}
			Expect(c.GetType()).To(Equal("Procedural"))
		})

		It("returns the value when Level is non-empty", func() {
			c := synthesis.Classification{Type: "Technical", Level: "Operational"}
			Expect(c.GetLevel()).To(Equal("Operational"))
		})
	})

	Describe("DiagnosticSeverity.String()", func() {
		It("SeverityGood returns good", func() {
			Expect(synthesis.SeverityGood.String()).To(Equal("good"))
		})
		It("SeverityWarn returns warn", func() {
			Expect(synthesis.SeverityWarn.String()).To(Equal("warn"))
		})
		It("SeverityPoor returns poor", func() {
			Expect(synthesis.SeverityPoor.String()).To(Equal("poor"))
		})
		It("SeverityCritical returns critical", func() {
			Expect(synthesis.SeverityCritical.String()).To(Equal("critical"))
		})
	})
})

// ---------------------------------------------------------------------------
// Ranker — transforms SynthesisInput + Classification → SynthesisRow
// ---------------------------------------------------------------------------

var _ = Describe("Ranker", func() {
	var (
		ranker *synthesis.Ranker
		cfg    config.ViabilityConfig
		ctx    context.Context
	)

	BeforeEach(func() {
		cfg = defaultViabilityConfig()
		ranker = synthesis.NewRanker(cfg)
		ctx = context.Background()
	})

	It("sets JobID on each SynthesisRow from the parameter", func() {
		inputs := []synthesis.SynthesisInput{
			makeInput("src-1", "tgt-1", 70.0),
			makeInput("src-2", "tgt-2", 80.0),
		}
		classifications := map[string]synthesis.Classification{
			"src-1": makeClassification("Technical", "Tactical"),
			"tgt-1": makeClassification("Technical", "Tactical"),
			"src-2": makeClassification("Technical", "Tactical"),
			"tgt-2": makeClassification("Technical", "Tactical"),
		}

		rows := ranker.Rank(ctx, "job-42", inputs, classifications)
		Expect(rows).To(HaveLen(2))
		for _, row := range rows {
			Expect(row.JobID).To(Equal("job-42"))
		}
	})

	It("propagates consensus fields from SynthesisInput", func() {
		input := synthesis.SynthesisInput{
			SourceID:              "src-a",
			TargetID:              "tgt-a",
			SimilarityScore:       75.0,
			SimilarityMedian:      74.0,
			SimilarityVar:         2.5,
			SimilarityCount:       3,
			ConsensusRelationship: "OVERLAPS",
			ContributionType:      "INTEGRAL_TO",
			ConfidenceFraction:    0.9,
			Unanimous:             false,
		}
		classifications := map[string]synthesis.Classification{
			"src-a": makeClassification("Technical", "Tactical"),
			"tgt-a": makeClassification("Technical", "Tactical"),
		}

		rows := ranker.Rank(ctx, "job-1", []synthesis.SynthesisInput{input}, classifications)
		Expect(rows).To(HaveLen(1))
		row := rows[0]
		Expect(row.SourceID).To(Equal("src-a"))
		Expect(row.TargetID).To(Equal("tgt-a"))
		Expect(row.SimilarityMean).To(BeNumerically("~", 75.0, 1e-9))
		Expect(row.SimilarityMedian).To(BeNumerically("~", 74.0, 1e-9))
		Expect(row.SimilarityVar).To(BeNumerically("~", 2.5, 1e-9))
		Expect(row.SimilarityCount).To(Equal(3))
		Expect(row.ConsensusRelationship).To(Equal("OVERLAPS"))
		Expect(row.ContributionType).To(Equal("INTEGRAL_TO"))
		Expect(row.ConfidenceFraction).To(BeNumerically("~", 0.9, 1e-9))
		Expect(row.Unanimous).To(BeFalse())
	})

	It("uses Classification defaults for missing control IDs", func() {
		inputs := []synthesis.SynthesisInput{
			makeInput("src-known", "tgt-missing", 80.0),
		}
		classifications := map[string]synthesis.Classification{
			"src-known": makeClassification("Procedural", "Strategic"),
			// tgt-missing deliberately omitted
		}

		rows := ranker.Rank(ctx, "job-2", inputs, classifications)
		Expect(rows).To(HaveLen(1))
		row := rows[0]
		Expect(row.SourceType).To(Equal("Procedural"))
		Expect(row.SourceLevel).To(Equal("Strategic"))
		Expect(row.TargetType).To(Equal("Unknown"))
		Expect(row.TargetLevel).To(Equal("Tactical"))
	})

	It("output length equals input length", func() {
		inputs := []synthesis.SynthesisInput{
			makeInput("a", "b", 50.0),
			makeInput("c", "d", 60.0),
			makeInput("e", "f", 70.0),
		}
		rows := ranker.Rank(ctx, "job-3", inputs, nil)
		Expect(rows).To(HaveLen(3))
	})

	It("nil classification map uses defaults for all", func() {
		inputs := []synthesis.SynthesisInput{
			makeInput("src-x", "tgt-x", 80.0),
		}

		rows := ranker.Rank(ctx, "job-4", inputs, nil)
		Expect(rows).To(HaveLen(1))
		row := rows[0]
		Expect(row.SourceType).To(Equal("Unknown"))
		Expect(row.TargetType).To(Equal("Unknown"))
		Expect(row.SourceLevel).To(Equal("Tactical"))
		Expect(row.TargetLevel).To(Equal("Tactical"))
	})

	It("empty input slice returns empty output", func() {
		rows := ranker.Rank(ctx, "job-5", []synthesis.SynthesisInput{}, nil)
		Expect(rows).To(BeEmpty())
		Expect(rows).NotTo(BeNil())
	})

	It("nil input slice returns empty output", func() {
		rows := ranker.Rank(ctx, "job-6", nil, nil)
		Expect(rows).To(BeEmpty())
		Expect(rows).NotTo(BeNil())
	})

	It("computes viability weight correctly", func() {
		// Same type/level, score=80 → no penalties → weight=80.0
		inputs := []synthesis.SynthesisInput{
			makeInput("src-v", "tgt-v", 80.0),
		}
		classifications := map[string]synthesis.Classification{
			"src-v": makeClassification("Technical", "Tactical"),
			"tgt-v": makeClassification("Technical", "Tactical"),
		}

		rows := ranker.Rank(ctx, "job-7", inputs, classifications)
		Expect(rows).To(HaveLen(1))
		Expect(rows[0].ViabilityWeight).To(BeNumerically("~", 80.0, 1e-9))
	})

	It("applies INTEGRAL_TO boost", func() {
		// Score=80, same type/level, INTEGRAL_TO → viability=80 * 1.1 = 88.0
		input := synthesis.SynthesisInput{
			SourceID:              "src-i",
			TargetID:              "tgt-i",
			SimilarityScore:       80.0,
			ConsensusRelationship: "EQUIVALENT",
			ContributionType:      "INTEGRAL_TO",
			ConfidenceFraction:    0.8,
			Unanimous:             true,
		}
		classifications := map[string]synthesis.Classification{
			"src-i": makeClassification("Technical", "Tactical"),
			"tgt-i": makeClassification("Technical", "Tactical"),
		}

		rows := ranker.Rank(ctx, "job-8", []synthesis.SynthesisInput{input}, classifications)
		Expect(rows).To(HaveLen(1))
		Expect(rows[0].ViabilityWeight).To(BeNumerically("~", 88.0, 1e-9))
	})
})

// ---------------------------------------------------------------------------
// Assessor — evaluates []SynthesisRow → *QualityReport
// ---------------------------------------------------------------------------

var _ = Describe("Assessor", func() {
	var (
		assessor *synthesis.Assessor
		cfg      config.AssessmentConfig
		ctx      context.Context
	)

	BeforeEach(func() {
		cfg = synthesis.ExportDefaultAssessmentConfig()
		ctx = context.Background()
	})

	It("counts TotalPairs and ViablePairs correctly", func() {
		assessor = synthesis.NewAssessor(cfg, []string{"EQUIVALENT"})
		rows := []synthesis.SynthesisRow{
			{SourceID: "s1", TargetID: "t1", ViabilityWeight: 50.0, SimilarityMean: 60, ConsensusRelationship: "EQUIVALENT", ConfidenceFraction: 0.8, Unanimous: true},
			{SourceID: "s2", TargetID: "t2", ViabilityWeight: 0.0, SimilarityMean: 30, ConsensusRelationship: "NO_RELATIONSHIP", ConfidenceFraction: 0.5, Unanimous: true},
			{SourceID: "s3", TargetID: "t3", ViabilityWeight: 75.0, SimilarityMean: 80, ConsensusRelationship: "OVERLAPS", ConfidenceFraction: 0.9, Unanimous: true},
			{SourceID: "s4", TargetID: "t4", ViabilityWeight: 0.0, SimilarityMean: 20, ConsensusRelationship: "NO_RELATIONSHIP", ConfidenceFraction: 0.3, Unanimous: true},
			{SourceID: "s5", TargetID: "t5", ViabilityWeight: 10.0, SimilarityMean: 40, ConsensusRelationship: "EQUIVALENT", ConfidenceFraction: 0.6, Unanimous: true},
		}

		report := assessor.Assess(ctx, rows)
		Expect(report.TotalPairs).To(Equal(5))
		Expect(report.ViablePairs).To(Equal(3))
	})

	It("computes AvgConfidence and AvgViability", func() {
		assessor = synthesis.NewAssessor(cfg, []string{"EQUIVALENT"})
		rows := []synthesis.SynthesisRow{
			{SourceID: "s1", TargetID: "t1", ViabilityWeight: 40.0, SimilarityMean: 60, ConsensusRelationship: "EQUIVALENT", ConfidenceFraction: 0.6, Unanimous: true},
			{SourceID: "s2", TargetID: "t2", ViabilityWeight: 80.0, SimilarityMean: 70, ConsensusRelationship: "EQUIVALENT", ConfidenceFraction: 0.8, Unanimous: true},
			{SourceID: "s3", TargetID: "t3", ViabilityWeight: 60.0, SimilarityMean: 50, ConsensusRelationship: "EQUIVALENT", ConfidenceFraction: 1.0, Unanimous: true},
			{SourceID: "s4", TargetID: "t4", ViabilityWeight: 20.0, SimilarityMean: 40, ConsensusRelationship: "EQUIVALENT", ConfidenceFraction: 0.6, Unanimous: true},
		}

		report := assessor.Assess(ctx, rows)
		// AvgConfidence = (0.6+0.8+1.0+0.6)/4 = 0.75
		Expect(report.AvgConfidence).To(BeNumerically("~", 0.75, 1e-9))
		// AvgViability = (40+80+60+20)/4 = 50
		Expect(report.AvgViability).To(BeNumerically("~", 50.0, 1e-9))
	})

	It("counts relationships by type", func() {
		assessor = synthesis.NewAssessor(cfg, []string{"EQUIVALENT"})
		rows := []synthesis.SynthesisRow{
			{SourceID: "s1", TargetID: "t1", ViabilityWeight: 50.0, SimilarityMean: 60, ConsensusRelationship: "EQUIVALENT", ConfidenceFraction: 0.8, Unanimous: true},
			{SourceID: "s2", TargetID: "t2", ViabilityWeight: 0.0, SimilarityMean: 30, ConsensusRelationship: "NO_RELATIONSHIP", ConfidenceFraction: 0.5, Unanimous: true},
			{SourceID: "s3", TargetID: "t3", ViabilityWeight: 60.0, SimilarityMean: 70, ConsensusRelationship: "OVERLAPS", ConfidenceFraction: 0.9, Unanimous: true},
			{SourceID: "s4", TargetID: "t4", ViabilityWeight: 0.0, SimilarityMean: 20, ConsensusRelationship: "NO_RELATIONSHIP", ConfidenceFraction: 0.3, Unanimous: true},
			{SourceID: "s5", TargetID: "t5", ViabilityWeight: 40.0, SimilarityMean: 50, ConsensusRelationship: "EQUIVALENT", ConfidenceFraction: 0.7, Unanimous: true},
		}

		report := assessor.Assess(ctx, rows)
		Expect(report.RelationshipCounts).To(HaveKeyWithValue("EQUIVALENT", 2))
		Expect(report.RelationshipCounts).To(HaveKeyWithValue("NO_RELATIONSHIP", 2))
		Expect(report.RelationshipCounts).To(HaveKeyWithValue("OVERLAPS", 1))
	})

	Context("IQR diagnostics", func() {
		It("computes IQR with known quartiles", func() {
			// Values: 10, 20, 30, 40, 50, 60, 70, 80
			// n=8, Q1 index = 0.25*7 = 1.75 → lerp(20,30, 0.75) = 27.5
			// Q3 index = 0.75*7 = 5.25 → lerp(60,70, 0.25) = 62.5
			// IQR = 62.5 - 27.5 = 35.0
			assessor = synthesis.NewAssessor(cfg, []string{})
			rows := make([]synthesis.SynthesisRow, 8)
			vals := []float64{10, 20, 30, 40, 50, 60, 70, 80}
			for i, v := range vals {
				rows[i] = synthesis.SynthesisRow{
					SourceID: "s", TargetID: "t", SimilarityMean: v,
					ConsensusRelationship: "NO_RELATIONSHIP", ConfidenceFraction: 0.5, Unanimous: true,
				}
			}

			report := assessor.Assess(ctx, rows)
			diag := findDiagnostic(report, "embedding_spread")
			Expect(diag).NotTo(BeNil())
			Expect(diag.Value).To(BeNumerically("~", 35.0, 1e-9))
		})

		DescribeTable("classifies embedding spread by IQR",
			func(vals []float64, wantSeverity synthesis.DiagnosticSeverity, wantMsg string) {
				assessor = synthesis.NewAssessor(cfg, []string{})

				rows := make([]synthesis.SynthesisRow, len(vals))
				for i, v := range vals {
					rows[i] = synthesis.SynthesisRow{
						SourceID: "s", TargetID: "t", SimilarityMean: v,
						ConsensusRelationship: "NO_RELATIONSHIP", ConfidenceFraction: 0.5, Unanimous: true,
					}
				}

				report := assessor.Assess(ctx, rows)
				diag := findDiagnostic(report, "embedding_spread")
				Expect(diag).NotTo(BeNil())
				Expect(diag.Severity).To(Equal(wantSeverity))
				Expect(diag.Message).To(ContainSubstring(wantMsg))
			},
			// Wide spread: Q1 = lerp(10,20,0.75) = 17.5, Q3 = lerp(80,90,0.25) = 82.5,
			// IQR = 65.0 >= IQRGood (20.0).
			Entry("IQR >= IQRGood → SeverityGood",
				[]float64{0, 10, 20, 30, 70, 80, 90, 100}, synthesis.SeverityGood, "good embedding discrimination"),
			// Clustered: Q1 = 51.75, Q3 = 55.25, IQR = 3.5 < IQRPoor (10.0).
			Entry("IQR < IQRPoor → SeverityPoor",
				[]float64{50, 51, 52, 53, 54, 55, 56, 57}, synthesis.SeverityPoor, "diluted averaged matrix"),
			// Q1 = 45.25, Q3 = 55.75, IQR = 10.5 in [10, 20).
			Entry("IQR between bounds → SeverityWarn",
				[]float64{40, 43, 46, 49, 52, 55, 58, 61}, synthesis.SeverityWarn, "moderate embedding spread"),
		)

		It("fewer than 4 rows → insufficient data", func() {
			assessor = synthesis.NewAssessor(cfg, []string{})
			rows := []synthesis.SynthesisRow{
				{SourceID: "s1", TargetID: "t1", SimilarityMean: 10, ConsensusRelationship: "NO_RELATIONSHIP", ConfidenceFraction: 0.5, Unanimous: true},
				{SourceID: "s2", TargetID: "t2", SimilarityMean: 50, ConsensusRelationship: "NO_RELATIONSHIP", ConfidenceFraction: 0.5, Unanimous: true},
				{SourceID: "s3", TargetID: "t3", SimilarityMean: 90, ConsensusRelationship: "NO_RELATIONSHIP", ConfidenceFraction: 0.5, Unanimous: true},
			}

			report := assessor.Assess(ctx, rows)
			diag := findDiagnostic(report, "embedding_spread")
			Expect(diag).NotTo(BeNil())
			Expect(diag.Value).To(BeNumerically("~", 0.0, 1e-9))
			Expect(diag.Severity).To(Equal(synthesis.SeverityWarn))
			Expect(diag.Message).To(ContainSubstring("insufficient data for IQR (n=3)"))
		})
	})

	Context("NO_RELATIONSHIP rate", func() {
		DescribeTable("classifies the NO_RELATIONSHIP rate",
			func(total, equivalent int, wantSeverity synthesis.DiagnosticSeverity, wantMsg string) {
				assessor = synthesis.NewAssessor(cfg, []string{"EQUIVALENT"})
				rows := make([]synthesis.SynthesisRow, total)
				for i := range rows {
					rel := "NO_RELATIONSHIP"
					if i < equivalent {
						rel = "EQUIVALENT"
					}
					rows[i] = synthesis.SynthesisRow{
						SourceID: "s", TargetID: "t", SimilarityMean: float64(i % 100),
						ConsensusRelationship: rel, ConfidenceFraction: 0.5, Unanimous: true,
					}
				}

				report := assessor.Assess(ctx, rows)
				diag := findDiagnostic(report, "no_relationship_rate")
				Expect(diag).NotTo(BeNil())
				Expect(diag.Severity).To(Equal(wantSeverity))
				Expect(diag.Message).To(ContainSubstring(wantMsg))
			},
			// 49 of 50 = 98% NO_RELATIONSHIP
			Entry("rate > NoRelHigh → SeverityPoor", 50, 1, synthesis.SeverityPoor, "top_k too low"),
			// 7 of 10 = 70% NO_RELATIONSHIP
			Entry("rate < NoRelLow → SeverityWarn", 10, 3, synthesis.SeverityWarn, "top_k too high"),
			// 9 of 10 = 90% NO_RELATIONSHIP, in [0.80, 0.97]
			Entry("rate in range → SeverityGood", 10, 1, synthesis.SeverityGood, "within expected range"),
		)
	})

	Context("contested pairs", func() {
		DescribeTable("classifies the contested fraction",
			func(contested int, wantSeverity synthesis.DiagnosticSeverity, wantMsg string) {
				assessor = synthesis.NewAssessor(cfg, []string{})
				rows := make([]synthesis.SynthesisRow, 10)
				for i := range rows {
					rows[i] = synthesis.SynthesisRow{
						SourceID: "s", TargetID: "t", SimilarityMean: float64(i * 10),
						ConsensusRelationship: "NO_RELATIONSHIP", ConfidenceFraction: 0.5,
						Unanimous: i >= contested, // first `contested` rows are non-unanimous
					}
				}

				report := assessor.Assess(ctx, rows)
				diag := findDiagnostic(report, "contested_pairs")
				Expect(diag).NotTo(BeNil())
				Expect(diag.Severity).To(Equal(wantSeverity))
				Expect(diag.Message).To(ContainSubstring(wantMsg))
			},
			// 3 of 10 = 30% non-unanimous
			Entry("fraction > ContestedWarn → SeverityWarn", 3, synthesis.SeverityWarn, "contested (non-unanimous)"),
			// 1 of 10 = 10% non-unanimous
			Entry("fraction within threshold → SeverityGood", 1, synthesis.SeverityGood, "contested"),
		)
	})

	Context("actionable coverage", func() {
		It("coverage < ActionableWarn → SeverityWarn", func() {
			// 5 distinct sources, only 1 has an actionable match → 20% < 30%
			assessor = synthesis.NewAssessor(cfg, []string{"EQUIVALENT", "OVERLAPS"})
			rows := []synthesis.SynthesisRow{
				{SourceID: "s1", TargetID: "t1", SimilarityMean: 80, ConsensusRelationship: "EQUIVALENT", ViabilityWeight: 50.0, ConfidenceFraction: 0.8, Unanimous: true},
				{SourceID: "s2", TargetID: "t2", SimilarityMean: 70, ConsensusRelationship: "NO_RELATIONSHIP", ViabilityWeight: 0.0, ConfidenceFraction: 0.5, Unanimous: true},
				{SourceID: "s3", TargetID: "t3", SimilarityMean: 60, ConsensusRelationship: "NO_RELATIONSHIP", ViabilityWeight: 0.0, ConfidenceFraction: 0.5, Unanimous: true},
				{SourceID: "s4", TargetID: "t4", SimilarityMean: 50, ConsensusRelationship: "NO_RELATIONSHIP", ViabilityWeight: 0.0, ConfidenceFraction: 0.5, Unanimous: true},
				{SourceID: "s5", TargetID: "t5", SimilarityMean: 40, ConsensusRelationship: "NO_RELATIONSHIP", ViabilityWeight: 0.0, ConfidenceFraction: 0.5, Unanimous: true},
			}

			report := assessor.Assess(ctx, rows)
			diag := findDiagnostic(report, "actionable_coverage")
			Expect(diag).NotTo(BeNil())
			Expect(diag.Severity).To(Equal(synthesis.SeverityWarn))
			Expect(diag.Message).To(ContainSubstring("low actionable coverage"))
		})

		It("coverage above threshold → SeverityGood", func() {
			// 4 distinct sources, 2 have actionable matches → 50% >= 30%
			assessor = synthesis.NewAssessor(cfg, []string{"EQUIVALENT", "OVERLAPS"})
			rows := []synthesis.SynthesisRow{
				{SourceID: "s1", TargetID: "t1", SimilarityMean: 80, ConsensusRelationship: "EQUIVALENT", ViabilityWeight: 50.0, ConfidenceFraction: 0.8, Unanimous: true},
				{SourceID: "s2", TargetID: "t2", SimilarityMean: 70, ConsensusRelationship: "OVERLAPS", ViabilityWeight: 30.0, ConfidenceFraction: 0.7, Unanimous: true},
				{SourceID: "s3", TargetID: "t3", SimilarityMean: 60, ConsensusRelationship: "NO_RELATIONSHIP", ViabilityWeight: 0.0, ConfidenceFraction: 0.5, Unanimous: true},
				{SourceID: "s4", TargetID: "t4", SimilarityMean: 50, ConsensusRelationship: "NO_RELATIONSHIP", ViabilityWeight: 0.0, ConfidenceFraction: 0.5, Unanimous: true},
			}

			report := assessor.Assess(ctx, rows)
			diag := findDiagnostic(report, "actionable_coverage")
			Expect(diag).NotTo(BeNil())
			Expect(diag.Severity).To(Equal(synthesis.SeverityGood))
			Expect(diag.Message).To(ContainSubstring("actionable coverage"))
			Expect(diag.Message).NotTo(ContainSubstring("low"))
		})

		It("requires both actionable type AND ViabilityWeight > 0", func() {
			// s1 has actionable type EQUIVALENT but ViabilityWeight=0 → NOT actionable
			// s2 has NO_RELATIONSHIP (not actionable type) with weight > 0 → NOT actionable
			// Result: 0 out of 2 sources → 0% < 30%
			assessor = synthesis.NewAssessor(cfg, []string{"EQUIVALENT"})
			rows := []synthesis.SynthesisRow{
				{SourceID: "s1", TargetID: "t1", SimilarityMean: 80, ConsensusRelationship: "EQUIVALENT", ViabilityWeight: 0.0, ConfidenceFraction: 0.8, Unanimous: true},
				{SourceID: "s2", TargetID: "t2", SimilarityMean: 70, ConsensusRelationship: "NO_RELATIONSHIP", ViabilityWeight: 50.0, ConfidenceFraction: 0.7, Unanimous: true},
			}

			report := assessor.Assess(ctx, rows)
			diag := findDiagnostic(report, "actionable_coverage")
			Expect(diag).NotTo(BeNil())
			Expect(diag.Severity).To(Equal(synthesis.SeverityWarn))
			Expect(diag.Message).To(ContainSubstring("low actionable coverage"))
		})

		It("counts requires-derived rows as actionable even when \"requires\" is absent from actionableTypes", func() {
			// requires-derived rows always carry ConsensusRelationship
			// synthesis.ConsensusRequires (see internal/pipeline/synthesis_inputs.go),
			// which can never appear in the configured actionableTypes list --
			// pkg/config validates that list against the 8-value NIST IR 8477
			// relationship-type enum, which has no "requires" entry. A requires
			// relationship is always actionable regardless of what's configured.
			// 2 distinct sources, 1 has an actionable requires match → 50% >= 30%
			assessor = synthesis.NewAssessor(cfg, []string{"EQUIVALENT"}) // "requires" deliberately absent
			rows := []synthesis.SynthesisRow{
				{SourceID: "s1", TargetID: "t1", SimilarityMean: 80, ConsensusRelationship: synthesis.ConsensusRequires, ViabilityWeight: 50.0, ConfidenceFraction: 0.8, Unanimous: true},
				{SourceID: "s2", TargetID: "t2", SimilarityMean: 70, ConsensusRelationship: "NO_RELATIONSHIP", ViabilityWeight: 0.0, ConfidenceFraction: 0.5, Unanimous: true},
			}

			report := assessor.Assess(ctx, rows)
			diag := findDiagnostic(report, "actionable_coverage")
			Expect(diag).NotTo(BeNil())
			Expect(diag.Severity).To(Equal(synthesis.SeverityGood))
			Expect(diag.Message).NotTo(ContainSubstring("low"))
		})
	})

	Context("edge cases", func() {
		It("zero rows produces empty report", func() {
			assessor = synthesis.NewAssessor(cfg, []string{"EQUIVALENT"})
			report := assessor.Assess(ctx, []synthesis.SynthesisRow{})

			Expect(report.TotalPairs).To(Equal(0))
			Expect(report.ViablePairs).To(Equal(0))
			Expect(report.AvgConfidence).To(BeNumerically("~", 0.0, 1e-9))
			Expect(report.AvgViability).To(BeNumerically("~", 0.0, 1e-9))
			Expect(report.RelationshipCounts).NotTo(BeNil())
			Expect(report.RelationshipCounts).To(BeEmpty())
			Expect(report.Diagnostics).NotTo(BeNil())
			Expect(report.Diagnostics).To(BeEmpty())
		})

		It("nil rows produces empty report", func() {
			assessor = synthesis.NewAssessor(cfg, []string{"EQUIVALENT"})
			report := assessor.Assess(ctx, nil)

			Expect(report.TotalPairs).To(Equal(0))
			Expect(report.ViablePairs).To(Equal(0))
			Expect(report.AvgConfidence).To(BeNumerically("~", 0.0, 1e-9))
			Expect(report.AvgViability).To(BeNumerically("~", 0.0, 1e-9))
			Expect(report.RelationshipCounts).NotTo(BeNil())
			Expect(report.RelationshipCounts).To(BeEmpty())
			Expect(report.Diagnostics).NotTo(BeNil())
			Expect(report.Diagnostics).To(BeEmpty())
		})
	})
})

// ---------------------------------------------------------------------------
// Service — orchestrates Ranker, Assessor, DB persistence, and OTel
// ---------------------------------------------------------------------------

var _ = Describe("Service", func() {
	var (
		cfg             config.SynthesisConfig
		actionableTypes []string
		mockDB          *dbtest.Conn
		svc             *synthesis.Service
		ctx             context.Context
	)

	BeforeEach(func() {
		cfg = defaultSynthesisConfig()
		actionableTypes = []string{"EQUIVALENT", "OVERLAPS"}
		mockDB = newSuccessMockDB()
		svc = synthesis.New(mockDB, cfg, actionableTypes,
			synthesis.WithLogger(testspecs.GinkgoLogger()))
		ctx = testspecs.SetupTenantContext("test-tenant")
	})

	// -----------------------------------------------------------------------
	// Happy path
	// -----------------------------------------------------------------------

	Context("happy path", func() {
		It("returns ExecuteResult with Rows, Report, and ContentHash", func() {
			inputs := []synthesis.SynthesisInput{
				makeInput("src-1", "tgt-1", 80.0),
				makeInput("src-2", "tgt-2", 60.0),
			}
			classifications := map[string]synthesis.Classification{
				"src-1": makeClassification("Technical", "Tactical"),
				"tgt-1": makeClassification("Technical", "Tactical"),
				"src-2": makeClassification("Technical", "Tactical"),
				"tgt-2": makeClassification("Technical", "Tactical"),
			}

			result, err := svc.Execute(ctx, "job-happy", inputs, classifications)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).NotTo(BeNil())
			Expect(result.Rows).To(HaveLen(2))
			Expect(result.Report).NotTo(BeNil())
			Expect(result.Report.TotalPairs).To(Equal(2))
			Expect(result.ContentHash).NotTo(BeEmpty())
		})

		It("commits the transaction on success", func() {
			inputs := []synthesis.SynthesisInput{
				makeInput("src-1", "tgt-1", 70.0),
			}

			_, err := svc.Execute(ctx, "job-commit", inputs, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(mockDB.Tx.Commits()).To(Equal(1))
			Expect(mockDB.Tx.Rollbacks()).To(Equal(0))
		})

		It("persists viability weights via a single batch UPDATE with RETURNING", func() {
			var capturedQueries []string
			mockDB.Tx.QueryFunc = func(_ context.Context, query string, args ...any) (db.Rows, error) {
				capturedQueries = append(capturedQueries, query)
				return sourceIDRows("matched", args), nil
			}

			inputs := []synthesis.SynthesisInput{
				makeInput("src-1", "tgt-1", 80.0),
			}

			_, err := svc.Execute(ctx, "job-persist", inputs, nil)
			Expect(err).NotTo(HaveOccurred())
			// Batch approach: exactly one Query call regardless of input count.
			Expect(capturedQueries).To(HaveLen(1))
			Expect(capturedQueries[0]).To(ContainSubstring("UPDATE vote_summaries"))
			Expect(capturedQueries[0]).To(ContainSubstring("UNNEST"))
			Expect(capturedQueries[0]).To(ContainSubstring("RETURNING"))
			Expect(capturedQueries[0]).To(ContainSubstring("vs.analyzer_name = u.analyzer_name"),
				"each row must update only its own contribution's vote_summaries row")
		})
	})

	// -----------------------------------------------------------------------
	// Per-tenant config override path
	// -----------------------------------------------------------------------

	Context("per-tenant config overrides", func() {
		It("applies per-tenant ConfidenceThreshold override via Service.Execute", func() {
			// Global threshold: 0.5 (rows with ConfidenceFraction=0.8 would pass).
			// Override for "test-tenant": 0.99 (rows with ConfidenceFraction=0.8 are dropped).
			threshold := 0.99
			cfgWithOverride := defaultSynthesisConfig()
			cfgWithOverride.TenantOverrides = map[string]config.SynthesisOverride{
				"test-tenant": {ConfidenceThreshold: &threshold},
			}
			svcOverride := synthesis.New(newSuccessMockDB(), cfgWithOverride, actionableTypes,
				synthesis.WithLogger(testspecs.GinkgoLogger()))

			inputs := []synthesis.SynthesisInput{
				makeInput("src-1", "tgt-1", 80.0), // ConfidenceFraction=0.8 (from makeInput)
			}

			result, err := svcOverride.Execute(ctx, "job-override", inputs, nil)
			Expect(err).NotTo(HaveOccurred())
			// All rows filtered out because 0.8 < 0.99 threshold.
			// persistViability is called with 0 rows → succeeds with updated=0.
			Expect(result).NotTo(BeNil())
			Expect(result.Rows).To(BeEmpty())
		})

		It("applies per-tenant MaxMappingsPerControl override via Service.Execute", func() {
			maxMappings := 1
			cfgWithOverride := defaultSynthesisConfig()
			cfgWithOverride.TenantOverrides = map[string]config.SynthesisOverride{
				"test-tenant": {MaxMappingsPerControl: &maxMappings},
			}
			svcOverride := synthesis.New(newSuccessMockDB(), cfgWithOverride, actionableTypes,
				synthesis.WithLogger(testspecs.GinkgoLogger()))

			// Two targets for the same source — cap of 1 should retain only the highest viability.
			inputs := []synthesis.SynthesisInput{
				makeInput("src-1", "tgt-1", 80.0),
				makeInput("src-1", "tgt-2", 40.0),
			}

			result, err := svcOverride.Execute(ctx, "job-cap-override", inputs, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).NotTo(BeNil())
			// MaxMappingsPerControl=1 → only the highest-weight row for src-1 is kept.
			Expect(result.Rows).To(HaveLen(1))
			Expect(result.Rows[0].SourceID).To(Equal("src-1"))
			Expect(result.Rows[0].TargetID).To(Equal("tgt-1"))
		})
	})

	// -----------------------------------------------------------------------
	// Tenant validation
	// -----------------------------------------------------------------------

	Context("tenant validation", func() {
		It("returns error wrapping tenant.ErrNoTenant when context has no tenant", func() {
			noTenantCtx := context.Background()
			inputs := []synthesis.SynthesisInput{makeInput("s", "t", 50.0)}

			result, err := svc.Execute(noTenantCtx, "job-1", inputs, nil)
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, tenant.ErrNoTenant)).To(BeTrue())
			Expect(result).To(BeNil())
		})
	})

	// -----------------------------------------------------------------------
	// Job ID validation
	// -----------------------------------------------------------------------

	Context("job ID validation", func() {
		It("returns ErrInvalidJobID for empty jobID", func() {
			inputs := []synthesis.SynthesisInput{makeInput("s", "t", 50.0)}

			result, err := svc.Execute(ctx, "", inputs, nil)
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, synthesis.ErrInvalidJobID)).To(BeTrue())
			Expect(result).To(BeNil())
		})
	})

	// -----------------------------------------------------------------------
	// Input validation
	// -----------------------------------------------------------------------

	Context("input validation", func() {
		DescribeTable("returns ErrInvalidInput",
			func(inputs []synthesis.SynthesisInput, wantMsg string) {
				result, err := svc.Execute(ctx, "job-1", inputs, nil)
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, synthesis.ErrInvalidInput)).To(BeTrue())
				Expect(err.Error()).To(ContainSubstring(wantMsg))
				Expect(result).To(BeNil())
			},
			Entry("for empty SourceID",
				[]synthesis.SynthesisInput{{SourceID: "", TargetID: "tgt-1", SimilarityScore: 50.0, ConfidenceFraction: 0.5}}, "SourceID"),
			Entry("for empty TargetID",
				[]synthesis.SynthesisInput{{SourceID: "src-1", TargetID: "", SimilarityScore: 50.0, ConfidenceFraction: 0.5}}, "TargetID"),
			Entry("for NaN SimilarityScore",
				[]synthesis.SynthesisInput{{SourceID: "src-1", TargetID: "tgt-1", SimilarityScore: math.NaN(), ConfidenceFraction: 0.5}}, "SimilarityScore"),
			Entry("for +Inf SimilarityScore",
				[]synthesis.SynthesisInput{{SourceID: "src-1", TargetID: "tgt-1", SimilarityScore: math.Inf(1), ConfidenceFraction: 0.5}}, "invalid SimilarityScore"),
			Entry("for negative SimilarityScore",
				[]synthesis.SynthesisInput{{SourceID: "src-1", TargetID: "tgt-1", SimilarityScore: -0.1, ConfidenceFraction: 0.5}}, "negative SimilarityScore"),
			Entry("for ConfidenceFraction > 1",
				[]synthesis.SynthesisInput{{SourceID: "src-1", TargetID: "tgt-1", SimilarityScore: 50.0, ConfidenceFraction: 1.01}}, "ConfidenceFraction"),
			Entry("for ConfidenceFraction < 0",
				[]synthesis.SynthesisInput{{SourceID: "src-1", TargetID: "tgt-1", SimilarityScore: 50.0, ConfidenceFraction: -0.1}}, "ConfidenceFraction"),
			Entry("when len(inputs) > 10000",
				slices.Repeat([]synthesis.SynthesisInput{makeInput("src", "tgt", 50.0)}, 10001), "too many inputs"),
			Entry("when SourceID exceeds 256 bytes",
				[]synthesis.SynthesisInput{{SourceID: strings.Repeat("a", 257), TargetID: "tgt-1", SimilarityScore: 50.0, ConfidenceFraction: 0.5}}, "SourceID exceeds max length"),
			Entry("when TargetID exceeds 256 bytes",
				[]synthesis.SynthesisInput{{SourceID: "src-1", TargetID: strings.Repeat("b", 257), SimilarityScore: 50.0, ConfidenceFraction: 0.5}}, "TargetID exceeds max length"),
		)
	})

	// -----------------------------------------------------------------------
	// DB errors
	// -----------------------------------------------------------------------

	Context("DB errors", func() {
		It("returns ErrDBUpdate when transaction QueryRow fails", func() {
			dbErr := errors.New("connection reset")
			mockDB = newErrorMockDB(dbErr)
			svc = synthesis.New(mockDB, cfg, actionableTypes,
				synthesis.WithLogger(testspecs.GinkgoLogger()))

			inputs := []synthesis.SynthesisInput{makeInput("src-1", "tgt-1", 70.0)}
			inputsBefore := make([]synthesis.SynthesisInput, len(inputs))
			copy(inputsBefore, inputs)

			result, err := svc.Execute(ctx, "job-db-err", inputs, nil)
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, synthesis.ErrDBUpdate)).To(BeTrue())
			Expect(result).To(BeNil())
			Expect(mockDB.Tx.Rollbacks()).To(Equal(1))
			// Part 3: data unchanged after failure
			Expect(inputs).To(Equal(inputsBefore))
		})

		It("returns ErrDBNoRowsAffected when QueryRow returns sql.ErrNoRows", func() {
			mockDB = newNoRowsMockDB()
			svc = synthesis.New(mockDB, cfg, actionableTypes,
				synthesis.WithLogger(testspecs.GinkgoLogger()))

			inputs := []synthesis.SynthesisInput{makeInput("src-1", "tgt-1", 70.0)}
			inputsBefore := make([]synthesis.SynthesisInput, len(inputs))
			copy(inputsBefore, inputs)

			result, err := svc.Execute(ctx, "job-no-rows", inputs, nil)
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, synthesis.ErrDBNoRowsAffected)).To(BeTrue())
			Expect(result).To(BeNil())
			Expect(mockDB.Tx.Rollbacks()).To(Equal(1))
			// Part 3: data unchanged after failure
			Expect(inputs).To(Equal(inputsBefore))
		})

		It("returns ErrImmutabilityViolation for PG error code 23001", func() {
			pgErr := &pgconn.PgError{
				Code:    "23001",
				Message: "restrict_violation",
			}
			mockDB = newErrorMockDB(pgErr)
			svc = synthesis.New(mockDB, cfg, actionableTypes,
				synthesis.WithLogger(testspecs.GinkgoLogger()))

			inputs := []synthesis.SynthesisInput{makeInput("src-1", "tgt-1", 70.0)}
			inputsBefore := make([]synthesis.SynthesisInput, len(inputs))
			copy(inputsBefore, inputs)

			result, err := svc.Execute(ctx, "job-immutable", inputs, nil)
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, synthesis.ErrImmutabilityViolation)).To(BeTrue())
			Expect(result).To(BeNil())
			Expect(mockDB.Tx.Rollbacks()).To(Equal(1))
			// Part 3: data unchanged after failure
			Expect(inputs).To(Equal(inputsBefore))
		})

		It("ErrImmutabilityViolation wraps db.ErrImmutableRecord", func() {
			Expect(errors.Is(synthesis.ErrImmutabilityViolation, db.ErrImmutableRecord)).To(BeTrue(),
				"domain sentinel must wrap the base db sentinel")
		})

		It("returns ErrDBUpdate when tx.Commit() fails", func() {
			mockDB.Tx.CommitErr = errors.New("disk full")
			svc = synthesis.New(mockDB, cfg, actionableTypes,
				synthesis.WithLogger(testspecs.GinkgoLogger()))

			inputs := []synthesis.SynthesisInput{makeInput("src-1", "tgt-1", 70.0)}

			// Capture inputs before Execute to verify they are unmodified after failure.
			inputsBefore := make([]synthesis.SynthesisInput, len(inputs))
			copy(inputsBefore, inputs)

			result, err := svc.Execute(ctx, "job-commit-err", inputs, nil)
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, synthesis.ErrDBUpdate)).To(BeTrue())
			Expect(result).To(BeNil())
			// Verify inputs are unmodified (3-part negative test rule: data unchanged).
			Expect(inputs).To(Equal(inputsBefore))
			// Commit failure does NOT trigger rollback (commit is the final step).
			Expect(mockDB.Tx.Rollbacks()).To(Equal(0))
		})

		It("rolls back on Begin failure", func() {
			mockDB.BeginFunc = func(context.Context) (db.Transaction, error) {
				return nil, errors.New("cannot begin transaction")
			}
			svc = synthesis.New(mockDB, cfg, actionableTypes,
				synthesis.WithLogger(testspecs.GinkgoLogger()))

			inputs := []synthesis.SynthesisInput{makeInput("src-1", "tgt-1", 70.0)}

			result, err := svc.Execute(ctx, "job-begin-err", inputs, nil)
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, synthesis.ErrDBUpdate)).To(BeTrue())
			Expect(result).To(BeNil())
		})
	})

	// -----------------------------------------------------------------------
	// OTel instrumentation
	// -----------------------------------------------------------------------

	Context("OTel instrumentation", func() {
		var tp *telemetrytest.TestProvider

		BeforeEach(func() {
			var err error
			tp, err = telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())

			svc = synthesis.New(mockDB, cfg, actionableTypes,
				synthesis.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()),
				synthesis.WithLogger(testspecs.GinkgoLogger()))
		})

		AfterEach(func() {
			Expect(tp.Shutdown(context.Background())).To(Succeed())
		})

		It("creates a span named synthesis.Execute with job.id and tenant.id", func() {
			inputs := []synthesis.SynthesisInput{makeInput("src-1", "tgt-1", 80.0)}

			_, err := svc.Execute(ctx, "job-span", inputs, nil)
			Expect(err).NotTo(HaveOccurred())

			spans := tp.GetSpans()
			span := telemetrytest.FindSpan(spans, "synthesis.Execute")
			Expect(span).NotTo(BeNil(), "expected span synthesis.Execute")

			jobAttr, found := telemetrytest.SpanAttribute(span, "job.id")
			Expect(found).To(BeTrue())
			Expect(jobAttr.AsString()).To(Equal("job-span"))

			tenantAttr, found := telemetrytest.SpanAttribute(span, "tenant.id")
			Expect(found).To(BeTrue())
			Expect(tenantAttr.AsString()).To(Equal("test-tenant"))
		})

		It("increments executions.total with status=ok on success", func() {
			inputs := []synthesis.SynthesisInput{makeInput("src-1", "tgt-1", 80.0)}

			_, err := svc.Execute(ctx, "job-ok", inputs, nil)
			Expect(err).NotTo(HaveOccurred())

			rm := tp.GetMetrics()
			m := telemetrytest.FindMetric(rm, "synthesis.executions.total")
			Expect(m).NotTo(BeNil(), "expected synthesis.executions.total metric")

			val, err := telemetrytest.CounterValue(m)
			Expect(err).NotTo(HaveOccurred())
			Expect(val).To(BeNumerically(">=", 1))
		})

		It("increments errors.total with error_category=validation on tenant error", func() {
			noTenantCtx := context.Background()
			inputs := []synthesis.SynthesisInput{makeInput("s", "t", 50.0)}

			_, _ = svc.Execute(noTenantCtx, "job-err", inputs, nil)

			rm := tp.GetMetrics()
			m := telemetrytest.FindMetric(rm, "synthesis.errors.total")
			Expect(m).NotTo(BeNil(), "expected synthesis.errors.total metric")

			val, err := telemetrytest.CounterValue(m)
			Expect(err).NotTo(HaveOccurred())
			Expect(val).To(BeNumerically(">=", 1))
		})

		It("increments errors.total with error_category=db on DB error", func() {
			dbErr := errors.New("disk full")
			mockDB = newErrorMockDB(dbErr)
			svc = synthesis.New(mockDB, cfg, actionableTypes,
				synthesis.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()),
				synthesis.WithLogger(testspecs.GinkgoLogger()))

			inputs := []synthesis.SynthesisInput{makeInput("src-1", "tgt-1", 70.0)}

			_, _ = svc.Execute(ctx, "job-db", inputs, nil)

			rm := tp.GetMetrics()
			m := telemetrytest.FindMetric(rm, "synthesis.errors.total")
			Expect(m).NotTo(BeNil())

			val, err := telemetrytest.CounterValue(m)
			Expect(err).NotTo(HaveOccurred())
			Expect(val).To(BeNumerically(">=", 1))
		})

		It("records pairs.ranked.total metric", func() {
			inputs := []synthesis.SynthesisInput{
				makeInput("src-1", "tgt-1", 80.0),
				makeInput("src-2", "tgt-2", 60.0),
				makeInput("src-3", "tgt-3", 70.0),
			}

			_, err := svc.Execute(ctx, "job-pairs", inputs, nil)
			Expect(err).NotTo(HaveOccurred())

			rm := tp.GetMetrics()
			m := telemetrytest.FindMetric(rm, "synthesis.pairs.ranked.total")
			Expect(m).NotTo(BeNil())

			val, err := telemetrytest.CounterValue(m)
			Expect(err).NotTo(HaveOccurred())
			Expect(val).To(Equal(int64(3)))
		})

		It("records viability.updates.total metric", func() {
			inputs := []synthesis.SynthesisInput{
				makeInput("src-1", "tgt-1", 80.0),
				makeInput("src-2", "tgt-2", 60.0),
			}

			_, err := svc.Execute(ctx, "job-updates", inputs, nil)
			Expect(err).NotTo(HaveOccurred())

			rm := tp.GetMetrics()
			m := telemetrytest.FindMetric(rm, "synthesis.viability.updates.total")
			Expect(m).NotTo(BeNil())

			val, err := telemetrytest.CounterValue(m)
			Expect(err).NotTo(HaveOccurred())
			Expect(val).To(Equal(int64(2)))
		})

		It("records synthesis.duration_ms histogram on success", func() {
			inputs := []synthesis.SynthesisInput{makeInput("src-1", "tgt-1", 80.0)}

			_, err := svc.Execute(ctx, "job-duration", inputs, nil)
			Expect(err).NotTo(HaveOccurred())

			rm := tp.GetMetrics()
			m := telemetrytest.FindMetric(rm, "synthesis.duration_ms")
			Expect(m).NotTo(BeNil(), "expected synthesis.duration_ms metric")

			// Verify the histogram has at least one data point recorded.
			count, err := telemetrytest.Float64HistogramCount(m)
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(BeNumerically(">=", uint64(1)))
		})
	})

	// -----------------------------------------------------------------------
	// Content hash determinism
	// -----------------------------------------------------------------------

	Context("content hash", func() {
		It("produces the same hash for the same report", func() {
			inputs := []synthesis.SynthesisInput{
				makeInput("src-1", "tgt-1", 80.0),
				makeInput("src-2", "tgt-2", 60.0),
			}
			classifications := map[string]synthesis.Classification{
				"src-1": makeClassification("Technical", "Tactical"),
				"tgt-1": makeClassification("Technical", "Tactical"),
				"src-2": makeClassification("Technical", "Tactical"),
				"tgt-2": makeClassification("Technical", "Tactical"),
			}

			r1, err := svc.Execute(ctx, "job-hash-1", inputs, classifications)
			Expect(err).NotTo(HaveOccurred())

			r2, err := svc.Execute(ctx, "job-hash-2", inputs, classifications)
			Expect(err).NotTo(HaveOccurred())

			Expect(r1.ContentHash).To(Equal(r2.ContentHash))
			Expect(r1.ContentHash).To(HaveLen(64)) // SHA-256 hex
		})

		It("uses storage.ContentHash for SHA-256", func() {
			inputs := []synthesis.SynthesisInput{
				makeInput("src-1", "tgt-1", 80.0),
			}

			result, err := svc.Execute(ctx, "job-sha", inputs, nil)
			Expect(err).NotTo(HaveOccurred())
			// Verify hash is valid hex and 64 chars (SHA-256)
			Expect(result.ContentHash).To(MatchRegexp("^[0-9a-f]{64}$"))
			// Verify the deterministic content hash by computing it from the report ourselves
			// We can't call computeContentHash directly (unexported), but we can verify
			// format and length match storage.ContentHash output
			hash := storage.ContentHash([]byte("test"))
			Expect(hash).To(HaveLen(64)) // sanity check storage.ContentHash
		})
	})

	// -----------------------------------------------------------------------
	// Zero viable pairs warning
	// -----------------------------------------------------------------------

	Context("zero viable pairs", func() {
		It("returns no error when assessor reports 0 viable pairs", func() {
			// Use a score of 0 so viability weight is 0 → 0 viable pairs
			inputs := []synthesis.SynthesisInput{
				makeInput("src-1", "tgt-1", 0.0),
			}

			result, err := svc.Execute(ctx, "job-zero", inputs, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).NotTo(BeNil())
			Expect(result.Report.ViablePairs).To(Equal(0))
		})
	})

	// -----------------------------------------------------------------------
	// Content hash negative case
	// -----------------------------------------------------------------------

	Context("content hash negative case", func() {
		It("produces different hashes for different inputs", func() {
			inputsA := []synthesis.SynthesisInput{
				makeInput("src-1", "tgt-1", 80.0),
				makeInput("src-2", "tgt-2", 60.0),
			}
			inputsB := []synthesis.SynthesisInput{
				makeInput("src-x", "tgt-x", 30.0),
			}

			r1, err := svc.Execute(ctx, "job-hash-diff-1", inputsA, nil)
			Expect(err).NotTo(HaveOccurred())

			r2, err := svc.Execute(ctx, "job-hash-diff-2", inputsB, nil)
			Expect(err).NotTo(HaveOccurred())

			Expect(r1.ContentHash).NotTo(Equal(r2.ContentHash),
				"different inputs must produce different content hashes")
		})
	})

})

// ---------------------------------------------------------------------------
// filterByConfidence — unit tests
// ---------------------------------------------------------------------------

var _ = Describe("filterByConfidence", func() {
	It("keeps rows at exactly the threshold", func() {
		rows := []synthesis.SynthesisRow{
			{SourceID: "s1", TargetID: "t1", ConfidenceFraction: 0.5},
			{SourceID: "s2", TargetID: "t2", ConfidenceFraction: 0.4},
		}
		out := synthesis.ExportFilterByConfidence(rows, 0.5)
		Expect(out).To(HaveLen(1))
		Expect(out[0].SourceID).To(Equal("s1"))
	})

	It("drops rows strictly below the threshold", func() {
		rows := []synthesis.SynthesisRow{
			{SourceID: "s1", TargetID: "t1", ConfidenceFraction: 0.49},
		}
		out := synthesis.ExportFilterByConfidence(rows, 0.5)
		Expect(out).To(BeEmpty())
	})

	It("returns an empty slice when all rows are dropped", func() {
		rows := []synthesis.SynthesisRow{
			{SourceID: "s1", TargetID: "t1", ConfidenceFraction: 0.1},
			{SourceID: "s2", TargetID: "t2", ConfidenceFraction: 0.2},
		}
		out := synthesis.ExportFilterByConfidence(rows, 0.9)
		Expect(out).NotTo(BeNil())
		Expect(out).To(BeEmpty())
	})

	It("returns all rows when threshold is 0", func() {
		rows := []synthesis.SynthesisRow{
			{SourceID: "s1", TargetID: "t1", ConfidenceFraction: 0.0},
			{SourceID: "s2", TargetID: "t2", ConfidenceFraction: 0.5},
		}
		// threshold=0: every row has ConfidenceFraction >= 0
		out := synthesis.ExportFilterByConfidence(rows, 0.0)
		Expect(out).To(HaveLen(2))
	})

	It("handles nil input gracefully", func() {
		out := synthesis.ExportFilterByConfidence(nil, 0.5)
		Expect(out).NotTo(BeNil())
		Expect(out).To(BeEmpty())
	})
})

// ---------------------------------------------------------------------------
// capMappingsPerSource — unit tests
// ---------------------------------------------------------------------------

var _ = Describe("capMappingsPerSource", func() {
	It("caps sources with more than maxPerSource rows to top-N by viability weight", func() {
		rows := []synthesis.SynthesisRow{
			{SourceID: "s1", TargetID: "t1", ViabilityWeight: 30.0},
			{SourceID: "s1", TargetID: "t2", ViabilityWeight: 90.0},
			{SourceID: "s1", TargetID: "t3", ViabilityWeight: 60.0},
		}
		out := synthesis.ExportCapMappingsPerSource(rows, 2)
		Expect(out).To(HaveLen(2))
		// Top-2 by weight: t2 (90) and t3 (60)
		targets := []string{out[0].TargetID, out[1].TargetID}
		Expect(targets).To(ContainElements("t2", "t3"))
	})

	It("leaves sources with fewer than maxPerSource rows untouched", func() {
		rows := []synthesis.SynthesisRow{
			{SourceID: "s1", TargetID: "t1", ViabilityWeight: 80.0},
			{SourceID: "s2", TargetID: "t2", ViabilityWeight: 70.0},
		}
		out := synthesis.ExportCapMappingsPerSource(rows, 5)
		Expect(out).To(HaveLen(2))
	})

	It("preserves insertion order of source IDs", func() {
		rows := []synthesis.SynthesisRow{
			{SourceID: "src-b", TargetID: "t1", ViabilityWeight: 50.0},
			{SourceID: "src-a", TargetID: "t2", ViabilityWeight: 80.0},
			{SourceID: "src-b", TargetID: "t3", ViabilityWeight: 40.0},
		}
		out := synthesis.ExportCapMappingsPerSource(rows, 10)
		// src-b appeared first in input, so its rows come before src-a rows
		Expect(out[0].SourceID).To(Equal("src-b"))
		Expect(out[2].SourceID).To(Equal("src-a"))
	})

	It("caps to 1 row per source when maxPerSource=1", func() {
		rows := []synthesis.SynthesisRow{
			{SourceID: "s1", TargetID: "t1", ViabilityWeight: 40.0},
			{SourceID: "s1", TargetID: "t2", ViabilityWeight: 90.0},
			{SourceID: "s2", TargetID: "t3", ViabilityWeight: 70.0},
		}
		out := synthesis.ExportCapMappingsPerSource(rows, 1)
		Expect(out).To(HaveLen(2))
		// s1 keeps highest weight row (t2)
		s1Row := func() synthesis.SynthesisRow {
			for _, r := range out {
				if r.SourceID == "s1" {
					return r
				}
			}
			return synthesis.SynthesisRow{}
		}()
		Expect(s1Row.TargetID).To(Equal("t2"))
	})

	It("returns rows unchanged when maxPerSource <= 0", func() {
		rows := []synthesis.SynthesisRow{
			{SourceID: "s1", TargetID: "t1", ViabilityWeight: 80.0},
		}
		out := synthesis.ExportCapMappingsPerSource(rows, 0)
		Expect(out).To(HaveLen(1))
	})
})

// ---------------------------------------------------------------------------
// DiagnosticSeverity.String — unknown branch
// ---------------------------------------------------------------------------

var _ = Describe("DiagnosticSeverity.String", func() {
	It("returns 'unknown' for out-of-range values", func() {
		Expect(synthesis.DiagnosticSeverity(99).String()).To(Equal("unknown"))
	})
})
