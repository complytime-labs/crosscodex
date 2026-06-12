package oscal

import (
	"fmt"
	"strings"
)

// TierFallback (Tier 6) splits text by paragraph breaks as a last resort.
// Splits doc.RawText by "\n\n" (double newline) and creates one item per non-empty paragraph.
// Always returns (items, true) — this is the final fallback and never fails.
func TierFallback(doc StructuredDoc, opts StructureOptions) ([]ControlItem, bool) {
	paragraphs := strings.Split(doc.RawText, "\n\n")

	items := make([]ControlItem, 0)
	for i, para := range paragraphs {
		trimmed := strings.TrimSpace(para)
		if trimmed == "" {
			continue
		}

		items = append(items, ControlItem{
			ID:    fmt.Sprintf("para-%d", i+1),
			Text:  trimmed,
			Class: ClassRequirement,
		})
	}

	// Even if no paragraphs were extracted, return true because this is the final fallback
	// In practice, if RawText is non-empty, we should get at least one paragraph
	return items, true
}
