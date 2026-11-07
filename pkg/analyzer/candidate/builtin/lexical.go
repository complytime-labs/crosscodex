package builtin

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/candidate"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Okapi BM25 constants. Fixed rather than configurable: they are the
// standard defaults and the generator's scores are only used for ranking.
const (
	bm25K1 = 1.2
	bm25B  = 0.75
)

// LexicalGenerator produces candidates by ranking targets with BM25 over
// control text, using the target set as the corpus.
type LexicalGenerator struct {
	tracer trace.Tracer
}

// LexicalOption configures a LexicalGenerator.
type LexicalOption func(*LexicalGenerator)

// WithLexicalTelemetry configures OTel instrumentation.
func WithLexicalTelemetry(tracer trace.Tracer) LexicalOption {
	return func(g *LexicalGenerator) {
		g.tracer = tracer
	}
}

// NewLexicalGenerator creates a new lexical generator.
func NewLexicalGenerator(opts ...LexicalOption) *LexicalGenerator {
	g := &LexicalGenerator{}
	for _, opt := range opts {
		opt(g)
	}
	return g
}

// Name returns the generator identifier.
func (g *LexicalGenerator) Name() string {
	return "lexical"
}

// tokenize lowercases s and splits it on every non-letter, non-digit rune.
func tokenize(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

type posting struct {
	targetID string
	tf       int
}

// Generate produces candidates ranked by BM25 of the source text against
// each target's text.
// Parameters:
//   - top_k (integer): Number of best-scoring targets per source (default: 10)
//   - weight (number): Generator weight for aggregation (default: 1.0)
//
// Score is the BM25 value divided by the best BM25 for that source, so the
// top candidate scores 1.0. Targets sharing no term with the source, and the
// source itself, are never returned. Ties order by target ID ascending.
func (g *LexicalGenerator) Generate(ctx context.Context, req candidate.GenerateRequest) ([]candidate.Candidate, error) {
	_, span := telemetry.StartSpan(g.tracer, ctx, "lexical.Generate")
	defer span.End()

	topK, err := intParam("lexical", req.Parameters, "top_k", 10)
	if err != nil {
		return nil, err
	}
	if topK < 1 {
		return nil, fmt.Errorf("lexical: parameter top_k %s must be a positive integer", describeParam(req.Parameters["top_k"]))
	}
	weight, err := floatParam("lexical", req.Parameters, "weight", 1.0)
	if err != nil {
		return nil, err
	}

	postings := make(map[string][]posting)
	docLen := make(map[string]int, len(req.TargetControls))
	totalLen := 0
	for targetID, target := range req.TargetControls {
		tokens := tokenize(target.Text)
		docLen[targetID] = len(tokens)
		totalLen += len(tokens)
		tf := make(map[string]int)
		for _, tok := range tokens {
			tf[tok]++
		}
		for tok, n := range tf {
			postings[tok] = append(postings[tok], posting{targetID: targetID, tf: n})
		}
	}
	n := float64(len(req.TargetControls))
	avgdl := 0.0
	if n > 0 {
		avgdl = float64(totalLen) / n
	}

	sourceIDs := make([]string, 0, len(req.SourceControls))
	for sourceID := range req.SourceControls {
		sourceIDs = append(sourceIDs, sourceID)
	}
	sort.Strings(sourceIDs)

	type scoredTarget struct {
		targetID string
		bm25     float64
	}
	candidates := []candidate.Candidate{}
	for _, sourceID := range sourceIDs {
		seen := make(map[string]struct{})
		var queryTerms []string
		for _, tok := range tokenize(req.SourceControls[sourceID].Text) {
			if _, dup := seen[tok]; !dup {
				seen[tok] = struct{}{}
				queryTerms = append(queryTerms, tok)
			}
		}
		sort.Strings(queryTerms) // float addition is order-dependent

		scores := make(map[string]float64)
		for _, term := range queryTerms {
			plist := postings[term]
			df := float64(len(plist))
			idf := math.Log(1 + (n-df+0.5)/(df+0.5))
			for _, p := range plist {
				tf := float64(p.tf)
				norm := 1 - bm25B + bm25B*float64(docLen[p.targetID])/avgdl
				scores[p.targetID] += idf * tf * (bm25K1 + 1) / (tf + bm25K1*norm)
			}
		}

		scored := make([]scoredTarget, 0, len(scores))
		for targetID, s := range scores {
			if targetID == sourceID || s <= 0 {
				continue
			}
			scored = append(scored, scoredTarget{targetID: targetID, bm25: s})
		}
		sort.Slice(scored, func(i, j int) bool {
			if scored[i].bm25 != scored[j].bm25 {
				return scored[i].bm25 > scored[j].bm25
			}
			return scored[i].targetID < scored[j].targetID
		})
		if len(scored) > topK {
			scored = scored[:topK]
		}

		for _, st := range scored {
			candidates = append(candidates, candidate.Candidate{
				SourceID:    sourceID,
				TargetID:    st.targetID,
				Score:       st.bm25 / scored[0].bm25,
				Weight:      weight,
				GeneratorID: "lexical",
				Metadata: map[string]string{
					"bm25": fmt.Sprintf("%.4f", st.bm25),
				},
			})
		}
	}

	span.SetAttributes(
		attribute.Int("candidate.count", len(candidates)),
		attribute.Int("source.count", len(req.SourceControls)),
		attribute.Int("target.count", len(req.TargetControls)),
		attribute.Int("top_k", topK),
		attribute.Float64("weight", weight),
	)

	return candidates, nil
}
