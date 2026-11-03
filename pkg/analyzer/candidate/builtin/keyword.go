package builtin

import (
	"context"
	"strings"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/candidate"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// KeywordGenerator produces candidates by matching foundational keywords in target controls.
// All source controls are paired with targets that contain foundational keywords.
type KeywordGenerator struct {
	tracer trace.Tracer
}

// KeywordOption configures a KeywordGenerator.
type KeywordOption func(*KeywordGenerator)

// WithKeywordTelemetry configures OTel instrumentation.
func WithKeywordTelemetry(tracer trace.Tracer) KeywordOption {
	return func(g *KeywordGenerator) {
		g.tracer = tracer
	}
}

// NewKeywordGenerator creates a new keyword generator.
func NewKeywordGenerator(opts ...KeywordOption) *KeywordGenerator {
	g := &KeywordGenerator{}
	for _, opt := range opts {
		opt(g)
	}
	return g
}

// Name returns the generator identifier.
func (g *KeywordGenerator) Name() string {
	return "keyword"
}

// defaultKeywords are common foundational capability indicators.
var defaultKeywords = []string{
	"policy",
	"procedure",
	"plan",
	"program",
	"framework",
	"establish",
	"define",
	"designate",
	"inventory",
	"baseline",
	"register",
	"catalogue",
	"catalog",
	"architecture",
	"strategy",
	"governance",
	"charter",
}

// Generate produces candidates by pairing all sources with targets containing foundational keywords.
// Parameters:
//   - keywords (list of strings): Non-empty, non-blank keywords to search for in target text (default: defaultKeywords)
//   - weight (number: int, int64 or float64): Generator weight for aggregation (default: 1.0)
func (g *KeywordGenerator) Generate(ctx context.Context, req candidate.GenerateRequest) ([]candidate.Candidate, error) {
	_, span := telemetry.StartSpan(g.tracer, ctx, "keyword.Generate")
	defer span.End()

	// Extract parameters
	keywords, err := stringListParam("keyword", req.Parameters, "keywords", defaultKeywords)
	if err != nil {
		return nil, err
	}

	weight, err := floatParam("keyword", req.Parameters, "weight", 1.0)
	if err != nil {
		return nil, err
	}

	// Find all targets that contain foundational keywords
	foundationalTargets := make(map[string][]string) // targetID -> matched keywords

	for targetID, targetData := range req.TargetControls {
		targetTextLower := strings.ToLower(targetData.Text)
		var matched []string

		for _, keyword := range keywords {
			keywordLower := strings.ToLower(keyword)
			// Check if keyword appears as a substring (allows for plural forms, etc.)
			if strings.Contains(targetTextLower, keywordLower) {
				matched = append(matched, keyword)
			}
		}

		if len(matched) > 0 {
			foundationalTargets[targetID] = matched
		}
	}

	// Pair all sources with all foundational targets
	var candidates []candidate.Candidate

	for sourceID := range req.SourceControls {
		for targetID, matchedKeywords := range foundationalTargets {
			if targetID == sourceID {
				continue // a control is never its own candidate
			}
			candidates = append(candidates, candidate.Candidate{
				SourceID:    sourceID,
				TargetID:    targetID,
				Score:       1.0, // Binary match - either has keywords or doesn't
				Weight:      weight,
				GeneratorID: "keyword",
				Metadata: map[string]string{
					"keywords_matched": strings.Join(matchedKeywords, ","),
				},
			})
		}
	}

	span.SetAttributes(
		attribute.Int("candidate.count", len(candidates)),
		attribute.Int("source.count", len(req.SourceControls)),
		attribute.Int("target.count", len(req.TargetControls)),
		attribute.Int("foundational_targets.count", len(foundationalTargets)),
		attribute.Int("keywords.count", len(keywords)),
		attribute.Float64("weight", weight),
	)

	return candidates, nil
}
