package oscal

import "fmt"

// TierHeading (Tier 2) extracts control items from markdown headings.
// Returns (nil, false) when no sections are present.
// Filters out spurious headings that appear more than MaxHeadingRepeats times (default 3).
func TierHeading(doc StructuredDoc, opts StructureOptions) ([]ControlItem, bool) {
	if len(doc.Sections) == 0 {
		return nil, false
	}

	maxRepeats := opts.MaxHeadingRepeats
	if maxRepeats == 0 {
		maxRepeats = 3
	}

	// Count title occurrences
	titleCounts := make(map[string]int)
	for _, sec := range doc.Sections {
		titleCounts[sec.Title]++
	}

	// Filter out spurious headings
	items := make([]ControlItem, 0, len(doc.Sections))
	for i, sec := range doc.Sections {
		// Skip sections with titles that appear too many times
		if titleCounts[sec.Title] > maxRepeats {
			continue
		}

		items = append(items, ControlItem{
			ID:    fmt.Sprintf("sec-%d", i+1),
			Title: sec.Title,
			Text:  sec.Text,
			Class: ClassRequirement,
		})
	}

	// Return false if all sections were filtered out
	if len(items) == 0 {
		return nil, false
	}

	return items, true
}
