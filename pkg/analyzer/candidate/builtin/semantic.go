package builtin

import (
	"context"
	"fmt"
	"reflect"
	"sort"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/candidate"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// SemanticGenerator produces candidates based on embedding similarity.
// It selects the top-K most similar targets for each source control.
type SemanticGenerator struct {
	tracer trace.Tracer
}

// SemanticOption configures a SemanticGenerator.
type SemanticOption func(*SemanticGenerator)

// WithSemanticTelemetry configures OTel instrumentation.
func WithSemanticTelemetry(tracer trace.Tracer) SemanticOption {
	return func(g *SemanticGenerator) {
		g.tracer = tracer
	}
}

// NewSemanticGenerator creates a new semantic generator.
func NewSemanticGenerator(opts ...SemanticOption) *SemanticGenerator {
	g := &SemanticGenerator{}
	for _, opt := range opts {
		opt(g)
	}
	return g
}

// Name returns the generator identifier.
func (g *SemanticGenerator) Name() string {
	return "semantic"
}

// similarityMatrix is an interface that any embedding matrix type must satisfy.
// This allows us to work with both the actual embedding.SimilarityMatrix and test mocks.
type similarityMatrix interface {
	getIDs() []string
	getValues() [][]float32
}

// extractMatrixData extracts IDs and Values from any struct with those fields.
func extractMatrixData(matrix interface{}) ([]string, [][]float32, bool) {
	v := reflect.ValueOf(matrix)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}

	if v.Kind() != reflect.Struct {
		return nil, nil, false
	}

	idsField := v.FieldByName("IDs")
	valuesField := v.FieldByName("Values")

	if !idsField.IsValid() || !valuesField.IsValid() {
		return nil, nil, false
	}

	ids, ok1 := idsField.Interface().([]string)
	values, ok2 := valuesField.Interface().([][]float32)

	return ids, values, ok1 && ok2
}

// Generate produces candidates from the embedding similarity matrix.
// Parameters:
//   - top_k (int): Number of most-similar targets per source (default: 10)
//   - min_similarity (float64): Minimum similarity threshold [0-100] (default: 0)
//   - weight (float64): Generator weight for aggregation (default: 1.0)
func (g *SemanticGenerator) Generate(ctx context.Context, req candidate.GenerateRequest) ([]candidate.Candidate, error) {
	ctx, span := telemetry.StartSpan(g.tracer, ctx, "semantic.Generate")
	defer span.End()

	// Extract parameters with defaults
	topK := 10
	if v, ok := req.Parameters["top_k"].(int); ok {
		topK = v
	}

	minSimilarity := 0.0
	if v, ok := req.Parameters["min_similarity"].(float64); ok {
		minSimilarity = v
	}

	weight := 1.0
	if v, ok := req.Parameters["weight"].(float64); ok {
		weight = v
	}

	// Check if embedding matrix is provided
	if req.EmbeddingMatrix == nil {
		return []candidate.Candidate{}, nil
	}

	// Extract matrix data using reflection to handle any type with IDs and Values fields
	ids, values, ok := extractMatrixData(req.EmbeddingMatrix)
	if !ok {
		return []candidate.Candidate{}, nil
	}

	// Build index map for quick lookup
	idToIndex := make(map[string]int, len(ids))
	for i, id := range ids {
		idToIndex[id] = i
	}

	var candidates []candidate.Candidate

	// For each source control
	for sourceID := range req.SourceControls {
		sourceIdx, ok := idToIndex[sourceID]
		if !ok {
			continue // Source not in matrix
		}

		// Collect all target similarities
		type scoredTarget struct {
			targetID   string
			similarity float32
		}
		var scored []scoredTarget

		for targetID := range req.TargetControls {
			targetIdx, ok := idToIndex[targetID]
			if !ok {
				continue // Target not in matrix
			}

			similarity := values[sourceIdx][targetIdx]

			// Filter by minimum similarity
			if float64(similarity) >= minSimilarity {
				scored = append(scored, scoredTarget{
					targetID:   targetID,
					similarity: similarity,
				})
			}
		}

		// Sort by similarity descending
		sort.Slice(scored, func(i, j int) bool {
			return scored[i].similarity > scored[j].similarity
		})

		// Take top-K
		limit := topK
		if limit > len(scored) {
			limit = len(scored)
		}

		// Create candidates
		for i := 0; i < limit; i++ {
			candidates = append(candidates, candidate.Candidate{
				SourceID:    sourceID,
				TargetID:    scored[i].targetID,
				Score:       float64(scored[i].similarity) / 100.0, // Normalize to [0, 1]
				Weight:      weight,
				GeneratorID: "semantic",
				Metadata: map[string]string{
					"similarity": fmt.Sprintf("%.2f", scored[i].similarity),
				},
			})
		}
	}

	span.SetAttributes(
		attribute.Int("candidate.count", len(candidates)),
		attribute.Int("source.count", len(req.SourceControls)),
		attribute.Int("target.count", len(req.TargetControls)),
		attribute.Int("top_k", topK),
		attribute.Float64("min_similarity", minSimilarity),
		attribute.Float64("weight", weight),
	)

	return candidates, nil
}
