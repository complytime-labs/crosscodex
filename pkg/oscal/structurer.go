package oscal

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/complytime-labs/crosscodex/pkg/prompt"
	"go.opentelemetry.io/otel/trace"
)

// defaultStructurer orchestrates the 6-tier cascade with keyword filtering and decomposition.
type defaultStructurer struct {
	completer Completer
	registry  prompt.Registry
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
func NewStructurer(completer Completer, registry prompt.Registry, opts ...StructurerOption) Structurer {
	s := &defaultStructurer{
		completer: completer,
		registry:  registry,
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
//  4. Strip opts.PrintArtifactPatterns from each item's title and text
//  5. Apply keyword filtering if opts.FilterByKeywords is true
//  6. Apply decomposition if opts.Decompose is true
//
// An unusable print-artefact pattern returns ErrInvalidPrintArtifactPattern
// before any tier runs.
func (s *defaultStructurer) Structure(ctx context.Context, doc StructuredDoc, opts StructureOptions) ([]ControlItem, error) {
	items, _, err := s.StructureWithTier(ctx, doc, opts)
	return items, err
}

// StructureWithTier runs the cascade Structure describes and also returns the
// tier whose items won ("" with an error).
func (s *defaultStructurer) StructureWithTier(ctx context.Context, doc StructuredDoc, opts StructureOptions) ([]ControlItem, StructureTier, error) {
	if s.tracer != nil {
		var span trace.Span
		_, span = s.tracer.Start(ctx, "oscal.Structure")
		defer span.End()
	}

	// Compiled before any tier runs, so a bad pattern costs no LLM call.
	artifacts, err := compilePrintArtifactPatterns(opts.PrintArtifactPatterns)
	if err != nil {
		return nil, "", err
	}

	tiers := []struct {
		tier StructureTier
		run  func() ([]ControlItem, bool)
	}{
		{StructureTierRegex, func() ([]ControlItem, bool) { return TierRegex(doc, opts) }},
		{StructureTierHeading, func() ([]ControlItem, bool) { return TierHeading(doc, opts) }},
		{StructureTierTable, func() ([]ControlItem, bool) { return TierTable(doc, opts) }},
		{StructureTierPattern, func() ([]ControlItem, bool) { return TierPattern(doc, opts) }},
		{StructureTierLLMDetect, func() ([]ControlItem, bool) { return TierLLMDetect(doc, opts, s.completer, s.registry) }},
		{StructureTierLLMExtract, func() ([]ControlItem, bool) { return TierLLMExtract(doc, opts, s.completer, s.registry) }},
		{StructureTierFallback, func() ([]ControlItem, bool) { return TierFallback(doc, opts) }},
	}
	for _, t := range tiers {
		items, found := t.run()
		// The fallback tier always reports found; it wins only with items.
		if found && (t.tier != StructureTierFallback || len(items) > 0) {
			return s.postProcess(items, opts, artifacts), t.tier, nil
		}
	}

	// All tiers failed (should be rare)
	return nil, "", ErrStructureFailed
}

// postProcess strips print artefacts, then applies keyword filtering and
// decomposition to extracted items.
func (s *defaultStructurer) postProcess(items []ControlItem, opts StructureOptions, artifacts []*regexp.Regexp) []ControlItem {
	if len(artifacts) > 0 {
		for i := range items {
			items[i].Title = stripPrintArtifacts(items[i].Title, artifacts)
			items[i].Text = stripPrintArtifacts(items[i].Text, artifacts)
		}
	}

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

// applyDecomposition applies DecomposeText to each item's text. An item the
// text does not split stays as it is, with its text trimmed. A split item is
// replaced by its sub-items: each keeps the item's Title (the heading or
// title it was written under, so no sub-item is left untitled) and GroupID,
// and the top-level ones take its ParentID; deeper ones keep the parent
// DecomposeText gave them.
func (s *defaultStructurer) applyDecomposition(items []ControlItem, opts StructureOptions) []ControlItem {
	minWords := opts.MinDecomposeWords
	if minWords <= 0 {
		minWords = 40
	}

	var result []ControlItem
	for _, item := range items {
		decomposed := DecomposeText(item.ID, item.Text, minWords)
		if len(decomposed) == 1 && decomposed[0].ID == item.ID {
			item.Text = decomposed[0].Text
			result = append(result, item)
			continue
		}
		for _, sub := range decomposed {
			sub.Title = item.Title
			sub.GroupID = item.GroupID
			if sub.ParentID == "" && item.ParentID != "" {
				sub.ParentID = item.ParentID
				sub.Props["parent-id"] = item.ParentID
			}
			result = append(result, sub)
		}
	}

	return result
}

// compilePrintArtifactPatterns compiles print-artefact patterns with the
// multiline flag and the checks pkg/config applies, so a caller that skipped
// config validation fails here instead of erasing text.
func compilePrintArtifactPatterns(patterns []string) ([]*regexp.Regexp, error) {
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for i, pattern := range patterns {
		re, err := regexp.Compile("(?m)" + pattern)
		if err != nil {
			return nil, fmt.Errorf("catalog.structuring.print_artifact_patterns[%d] %q is not valid regex: %v; fix or remove it: %w",
				i, pattern, err, ErrInvalidPrintArtifactPattern)
		}
		if re.MatchString("") {
			return nil, fmt.Errorf("catalog.structuring.print_artifact_patterns[%d] %q matches empty text, so it would match at every position; anchor it to the artefact it removes: %w",
				i, pattern, ErrInvalidPrintArtifactPattern)
		}
		compiled = append(compiled, re)
	}
	return compiled, nil
}

// stripPrintArtifacts removes every pattern's matches from text. It returns
// text unchanged when only whitespace would remain: a pattern that covers a
// whole statement is more likely matching real prose than a running header,
// and a statement is never erased.
func stripPrintArtifacts(text string, patterns []*regexp.Regexp) string {
	stripped := text
	for _, re := range patterns {
		stripped = re.ReplaceAllString(stripped, "")
	}
	if stripped == text {
		return text
	}
	if strings.TrimSpace(stripped) == "" {
		return text
	}
	// Only the line breaks a removed line leaves at the edges: indentation
	// is the text's own.
	return strings.Trim(stripped, "\r\n")
}
