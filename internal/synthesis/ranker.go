package synthesis

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

// Ranker transforms []SynthesisInput + classification data into []SynthesisRow
// with viability weights. Stateless; delegates numerical computation to
// ComputeViabilityWeight.
type Ranker struct {
	cfg    config.ViabilityConfig
	tracer trace.Tracer
}

// NewRanker creates a Ranker with the given viability configuration.
func NewRanker(cfg config.ViabilityConfig) *Ranker {
	return &Ranker{cfg: cfg}
}

// WithTelemetry enables OTel tracing for the Ranker. Returns r for chaining.
func (r *Ranker) WithTelemetry(tp trace.TracerProvider) *Ranker {
	if tp != nil {
		r.tracer = tp.Tracer("crosscodex/internal/synthesis")
	}
	return r
}

// Rank produces one SynthesisRow per input, preserving order. Classifications
// missing from the map fall back to zero-value Classification (GetType →
// "Unknown", GetLevel → "Tactical").
func (r *Ranker) Rank(ctx context.Context, jobID string, inputs []SynthesisInput, classifications map[string]Classification) []SynthesisRow {
	if r.tracer != nil {
		var span trace.Span
		ctx, span = r.tracer.Start(ctx, "synthesis.Rank") //nolint:ineffassign,staticcheck // ctx retained for child spans once sub-operations are instrumented
		defer span.End()
		span.SetAttributes(attribute.Int("input.count", len(inputs)))
	}

	if len(inputs) == 0 {
		return []SynthesisRow{}
	}

	rows := make([]SynthesisRow, len(inputs))
	for i, inp := range inputs {
		srcClass := lookupClassification(classifications, inp.SourceID)
		tgtClass := lookupClassification(classifications, inp.TargetID)

		srcType := srcClass.GetType()
		tgtType := tgtClass.GetType()
		srcLevel := srcClass.GetLevel()
		tgtLevel := tgtClass.GetLevel()

		rows[i] = SynthesisRow{
			JobID:                 jobID,
			SourceID:              inp.SourceID,
			TargetID:              inp.TargetID,
			SimilarityMean:        inp.SimilarityScore,
			SimilarityMedian:      inp.SimilarityMedian,
			SimilarityVar:         inp.SimilarityVar,
			SimilarityCount:       inp.SimilarityCount,
			SourceType:            srcType,
			TargetType:            tgtType,
			SourceLevel:           srcLevel,
			TargetLevel:           tgtLevel,
			ConsensusRelationship: inp.ConsensusRelationship,
			ConfidenceFraction:    inp.ConfidenceFraction,
			Unanimous:             inp.Unanimous,
			ContributionType:      inp.ContributionType,
			ViabilityWeight:       ComputeViabilityWeight(inp.SimilarityScore, srcType, tgtType, srcLevel, tgtLevel, inp.ContributionType, r.cfg),
		}
	}

	return rows
}

// lookupClassification returns the Classification for a control ID, or a
// zero-value Classification if the map is nil or the key is absent.
func lookupClassification(m map[string]Classification, id string) Classification {
	if m == nil {
		return Classification{}
	}
	return m[id]
}
