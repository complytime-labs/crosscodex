package builtin

import (
	"context"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/candidate"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// LevelGenerator produces candidates based on control abstraction level hierarchy.
// Sources can only REQUIRE targets at higher abstraction levels:
//   - Operational (2) can require Tactical (1) or Strategic (0)
//   - Tactical (1) can require Strategic (0)
//   - Strategic (0) cannot require anything (top level)
type LevelGenerator struct {
	tracer trace.Tracer
}

// LevelOption configures a LevelGenerator.
type LevelOption func(*LevelGenerator)

// WithLevelTelemetry configures OTel instrumentation.
func WithLevelTelemetry(tracer trace.Tracer) LevelOption {
	return func(g *LevelGenerator) {
		g.tracer = tracer
	}
}

// NewLevelGenerator creates a new level-based generator.
func NewLevelGenerator(opts ...LevelOption) *LevelGenerator {
	g := &LevelGenerator{}
	for _, opt := range opts {
		opt(g)
	}
	return g
}

// Name returns the generator identifier.
func (g *LevelGenerator) Name() string {
	return "level"
}

// levelIndex maps level names to their hierarchy position.
// Lower index = higher abstraction level.
var levelIndex = map[string]int{
	"Strategic":   0,
	"Tactical":    1,
	"Operational": 2,
}

// Generate produces candidates by pairing sources with targets at higher abstraction levels.
// Parameters:
//   - weight (float64): Generator weight for aggregation (default: 1.0)
func (g *LevelGenerator) Generate(ctx context.Context, req candidate.GenerateRequest) ([]candidate.Candidate, error) {
	ctx, span := telemetry.StartSpan(g.tracer, ctx, "level.Generate")
	defer span.End()

	// Extract parameters
	weight := 1.0
	if v, ok := req.Parameters["weight"].(float64); ok {
		weight = v
	}

	var candidates []candidate.Candidate

	// For each source control
	for sourceID, sourceData := range req.SourceControls {
		sourceLevel := sourceData.Level
		sourceLevelIdx, sourceOK := levelIndex[sourceLevel]

		// Skip sources with unknown or empty levels
		if !sourceOK || sourceLevel == "" {
			continue
		}

		// Find targets at higher abstraction level (lower index)
		for targetID, targetData := range req.TargetControls {
			targetLevel := targetData.Level
			targetLevelIdx, targetOK := levelIndex[targetLevel]

			// Skip targets with unknown or empty levels
			if !targetOK || targetLevel == "" {
				continue
			}

			// Only pair if target is at higher abstraction level (lower index)
			if targetLevelIdx < sourceLevelIdx {
				candidates = append(candidates, candidate.Candidate{
					SourceID:    sourceID,
					TargetID:    targetID,
					Score:       1.0, // Binary - either higher level or not
					Weight:      weight,
					GeneratorID: "level",
					Metadata: map[string]string{
						"source_level": sourceLevel,
						"target_level": targetLevel,
					},
				})
			}
		}
	}

	span.SetAttributes(
		attribute.Int("candidate.count", len(candidates)),
		attribute.Int("source.count", len(req.SourceControls)),
		attribute.Int("target.count", len(req.TargetControls)),
		attribute.Float64("weight", weight),
	)

	return candidates, nil
}
