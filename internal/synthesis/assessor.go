package synthesis

import (
	"context"
	"fmt"
	"sort"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

// Assessor evaluates []SynthesisRow and produces a *QualityReport with
// structured diagnostics across four categories: embedding spread (IQR),
// NO_RELATIONSHIP rate, contested pairs, and actionable coverage.
type Assessor struct {
	cfg             config.AssessmentConfig
	actionableTypes map[string]bool
	tracer          trace.Tracer
}

// NewAssessor creates an Assessor. actionableTypes is the set of relationship
// types considered actionable (sourced from analysis.relationship config, not
// duplicated in SynthesisConfig).
func NewAssessor(cfg config.AssessmentConfig, actionableTypes []string) *Assessor {
	m := make(map[string]bool, len(actionableTypes))
	for _, t := range actionableTypes {
		m[t] = true
	}
	return &Assessor{cfg: cfg, actionableTypes: m}
}

// WithTelemetry enables OTel tracing for the Assessor. Returns a for chaining.
func (a *Assessor) WithTelemetry(tp trace.TracerProvider) *Assessor {
	if tp != nil {
		a.tracer = tp.Tracer("crosscodex/internal/synthesis")
	}
	return a
}

// Assess evaluates rows and returns a QualityReport. Zero or nil rows produce
// an empty report with non-nil maps and slices, and no diagnostics.
func (a *Assessor) Assess(ctx context.Context, rows []SynthesisRow) *QualityReport {
	if a.tracer != nil {
		var span trace.Span
		ctx, span = a.tracer.Start(ctx, "synthesis.Assess") //nolint:ineffassign,staticcheck // ctx retained for child spans once sub-operations are instrumented
		defer span.End()
		span.SetAttributes(attribute.Int("row.count", len(rows)))
	}
	report := &QualityReport{
		RelationshipCounts: make(map[string]int),
		Diagnostics:        []Diagnostic{},
	}

	if len(rows) == 0 {
		return report
	}

	report.TotalPairs = len(rows)

	var sumConfidence, sumViability float64
	var noRelCount, contestedCount int

	for i := range rows {
		row := &rows[i]

		if row.ViabilityWeight > 0 {
			report.ViablePairs++
		}

		sumConfidence += row.ConfidenceFraction
		sumViability += row.ViabilityWeight

		report.RelationshipCounts[row.ConsensusRelationship]++

		if row.ConsensusRelationship == "NO_RELATIONSHIP" {
			noRelCount++
		}
		if !row.Unanimous {
			contestedCount++
		}
	}

	n := float64(report.TotalPairs)
	report.AvgConfidence = sumConfidence / n
	report.AvgViability = sumViability / n

	// Diagnostic 1: Embedding spread (IQR).
	report.Diagnostics = append(report.Diagnostics, a.diagnoseIQR(rows))

	// Diagnostic 2: NO_RELATIONSHIP rate.
	report.Diagnostics = append(report.Diagnostics, a.diagnoseNoRelRate(noRelCount, report.TotalPairs))

	// Diagnostic 3: Contested pairs.
	report.Diagnostics = append(report.Diagnostics, a.diagnoseContested(contestedCount, report.TotalPairs))

	// Diagnostic 4: Actionable coverage.
	report.Diagnostics = append(report.Diagnostics, a.diagnoseActionableCoverage(rows))

	return report
}

// diagnoseIQR computes the interquartile range of SimilarityMean values using
// linear interpolation at the 25th and 75th percentiles.
func (a *Assessor) diagnoseIQR(rows []SynthesisRow) Diagnostic {
	n := len(rows)
	if n < 4 {
		return Diagnostic{
			Category: "embedding_spread",
			Severity: SeverityWarn,
			Message:  fmt.Sprintf("insufficient data for IQR (n=%d)", n),
			Value:    0,
		}
	}

	vals := make([]float64, n)
	for i := range rows {
		vals[i] = rows[i].SimilarityMean
	}
	sort.Float64s(vals)

	iqr := percentile(vals, 0.75) - percentile(vals, 0.25)

	var severity DiagnosticSeverity
	var msg string

	switch {
	case iqr >= a.cfg.IQRGood:
		severity = SeverityGood
		msg = fmt.Sprintf("good embedding discrimination (IQR=%.2f)", iqr)
	case iqr < a.cfg.IQRPoor:
		severity = SeverityPoor
		msg = fmt.Sprintf("diluted averaged matrix (IQR=%.2f)", iqr)
	default:
		severity = SeverityWarn
		msg = fmt.Sprintf("moderate embedding spread (IQR=%.2f)", iqr)
	}

	return Diagnostic{
		Category: "embedding_spread",
		Severity: severity,
		Message:  msg,
		Value:    iqr,
	}
}

// percentile computes the p-th percentile (0 <= p <= 1) of sorted values using
// linear interpolation.
func percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return sorted[0]
	}

	idx := p * float64(n-1)
	lo := int(idx)
	hi := lo + 1
	if hi >= n {
		return sorted[n-1]
	}

	frac := idx - float64(lo)
	return sorted[lo] + frac*(sorted[hi]-sorted[lo])
}

// diagnoseNoRelRate classifies the NO_RELATIONSHIP fraction.
func (a *Assessor) diagnoseNoRelRate(noRelCount, total int) Diagnostic {
	rate := float64(noRelCount) / float64(total)
	pct := rate * 100.0

	var severity DiagnosticSeverity
	var msg string

	switch {
	case rate > a.cfg.NoRelHigh:
		severity = SeverityPoor
		msg = fmt.Sprintf("top_k too low: %.1f%% NO_RELATIONSHIP", pct)
	case rate < a.cfg.NoRelLow:
		severity = SeverityWarn
		msg = fmt.Sprintf("top_k too high: %.1f%% NO_RELATIONSHIP", pct)
	default:
		severity = SeverityGood
		msg = fmt.Sprintf("NO_RELATIONSHIP rate %.1f%% within expected range", pct)
	}

	return Diagnostic{
		Category: "no_relationship_rate",
		Severity: severity,
		Message:  msg,
		Value:    rate,
	}
}

// diagnoseContested classifies the non-unanimous pair fraction.
func (a *Assessor) diagnoseContested(contestedCount, total int) Diagnostic {
	fraction := float64(contestedCount) / float64(total)
	pct := fraction * 100.0

	var severity DiagnosticSeverity
	var msg string

	if fraction > a.cfg.ContestedWarn {
		severity = SeverityWarn
		msg = fmt.Sprintf("%.1f%% of pairs contested (non-unanimous)", pct)
	} else {
		severity = SeverityGood
		msg = fmt.Sprintf("%.1f%% of pairs contested", pct)
	}

	return Diagnostic{
		Category: "contested_pairs",
		Severity: severity,
		Message:  msg,
		Value:    fraction,
	}
}

// diagnoseActionableCoverage computes the fraction of distinct SourceIDs that
// have at least one actionable match (actionable type AND ViabilityWeight > 0).
func (a *Assessor) diagnoseActionableCoverage(rows []SynthesisRow) Diagnostic {
	allSources := make(map[string]bool)
	coveredSources := make(map[string]bool)

	for i := range rows {
		row := &rows[i]
		allSources[row.SourceID] = true

		if a.actionableTypes[row.ConsensusRelationship] && row.ViabilityWeight > 0 {
			coveredSources[row.SourceID] = true
		}
	}

	var coverage float64
	if len(allSources) > 0 {
		coverage = float64(len(coveredSources)) / float64(len(allSources))
	}
	pct := coverage * 100.0

	var severity DiagnosticSeverity
	var msg string

	if coverage < a.cfg.ActionableWarn {
		severity = SeverityWarn
		msg = fmt.Sprintf("low actionable coverage: %.1f%%", pct)
	} else {
		severity = SeverityGood
		msg = fmt.Sprintf("actionable coverage: %.1f%%", pct)
	}

	return Diagnostic{
		Category: "actionable_coverage",
		Severity: severity,
		Message:  msg,
		Value:    coverage,
	}
}
