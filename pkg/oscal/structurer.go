package oscal

import (
	"context"
	"strings"

	"go.opentelemetry.io/otel/trace"
)

// defaultStructurer orchestrates the 6-tier cascade with keyword filtering and decomposition.
type defaultStructurer struct {
	completer Completer
	prompts   PromptLoader
	tracer    trace.Tracer
}

// StructurerOption configures a Structurer.
type StructurerOption func(*defaultStructurer)

// WithStructurerTracer sets the OpenTelemetry tracer for the structurer.
func WithStructurerTracer(t trace.Tracer) StructurerOption {
	return func(s *defaultStructurer) {
		s.tracer = t
	}
}

// NewStructurer creates a new Structurer.
// Pass nil completer to skip LLM tiers (TierLLMDetect and TierLLMExtract).
func NewStructurer(completer Completer, prompts PromptLoader, opts ...StructurerOption) Structurer {
	s := &defaultStructurer{
		completer: completer,
		prompts:   prompts,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Structure orchestrates the 6-tier cascade to extract control items from a document.
//
// Flow:
//  1. Try tiers in order:
//     - TierRegex (custom pattern)
//     - TierHeading (markdown headings)
//     - TierTable (markdown tables)
//     - TierPattern (auto-detected numbering)
//     - TierLLMDetect (LLM-detected pattern) — skipped if completer is nil or opts.SkipLLM
//     - TierLLMExtract (LLM extraction) — skipped if completer is nil or opts.SkipLLM
//     - TierFallback (paragraph splitting) — always succeeds
//  2. First tier that returns (items, true) wins
//  3. If all tiers fail (should be rare), return ErrStructureFailed
//  4. Apply keyword filtering if opts.FilterByKeywords is true
//  5. Apply decomposition if opts.Decompose is true
func (s *defaultStructurer) Structure(ctx context.Context, doc StructuredDoc, opts StructureOptions) ([]ControlItem, error) {
	if s.tracer != nil {
		var span trace.Span
		_, span = s.tracer.Start(ctx, "oscal.Structure")
		defer span.End()
	}

	var items []ControlItem
	var found bool

	// Tier 1: Regex
	items, found = TierRegex(doc, opts)
	if found {
		return s.postProcess(items, opts), nil
	}

	// Tier 2: Heading
	items, found = TierHeading(doc, opts)
	if found {
		return s.postProcess(items, opts), nil
	}

	// Tier 2b: Table
	items, found = TierTable(doc, opts)
	if found {
		return s.postProcess(items, opts), nil
	}

	// Tier 3: Pattern
	items, found = TierPattern(doc, opts)
	if found {
		return s.postProcess(items, opts), nil
	}

	// Tier 4: LLM Detect
	items, found = TierLLMDetect(doc, opts, s.completer, s.prompts)
	if found {
		return s.postProcess(items, opts), nil
	}

	// Tier 5: LLM Extract
	items, found = TierLLMExtract(doc, opts, s.completer, s.prompts)
	if found {
		return s.postProcess(items, opts), nil
	}

	// Tier 6: Fallback
	items, found = TierFallback(doc, opts)
	if found && len(items) > 0 {
		return s.postProcess(items, opts), nil
	}

	// All tiers failed (should be rare)
	return nil, ErrStructureFailed
}

// postProcess applies keyword filtering and decomposition to extracted items.
func (s *defaultStructurer) postProcess(items []ControlItem, opts StructureOptions) []ControlItem {
	// Apply keyword filtering if enabled
	if opts.FilterByKeywords {
		items = s.filterByKeywords(items, opts)
	}

	// Apply decomposition if enabled
	if opts.Decompose {
		items = s.applyDecomposition(items, opts)
	}

	return items
}

// filterByKeywords filters items by keywords.
// Keeps items whose Text or Title contains any keyword (case-insensitive).
// If all items are filtered out, returns the original set.
func (s *defaultStructurer) filterByKeywords(items []ControlItem, opts StructureOptions) []ControlItem {
	keywords := opts.Keywords
	if len(keywords) == 0 {
		keywords = DefaultKeywords
	}

	// Convert keywords to lowercase for case-insensitive matching
	lowerKeywords := make([]string, len(keywords))
	for i, kw := range keywords {
		lowerKeywords[i] = strings.ToLower(kw)
	}

	var filtered []ControlItem
	for _, item := range items {
		if s.containsKeyword(item, lowerKeywords) {
			filtered = append(filtered, item)
		}
	}

	// If all items were filtered out, return original set
	if len(filtered) == 0 {
		return items
	}

	return filtered
}

// containsKeyword checks if an item's Text or Title contains any keyword (case-insensitive).
func (s *defaultStructurer) containsKeyword(item ControlItem, lowerKeywords []string) bool {
	lowerText := strings.ToLower(item.Text)
	lowerTitle := strings.ToLower(item.Title)

	for _, keyword := range lowerKeywords {
		if strings.Contains(lowerText, keyword) || strings.Contains(lowerTitle, keyword) {
			return true
		}
	}
	return false
}

// applyDecomposition applies DecomposeText to each item's text.
// Replaces each item with its decomposed sub-items.
func (s *defaultStructurer) applyDecomposition(items []ControlItem, opts StructureOptions) []ControlItem {
	minWords := opts.MinDecomposeWords
	if minWords <= 0 {
		minWords = 40
	}

	var result []ControlItem
	for _, item := range items {
		decomposed := DecomposeText(item.ID, item.Text, minWords)
		result = append(result, decomposed...)
	}

	return result
}
