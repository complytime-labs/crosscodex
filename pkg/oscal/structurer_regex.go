package oscal

import (
	"fmt"
	"regexp"
	"strings"
)

const maxPatternLen = 512

// TierRegex (Tier 1) uses a custom regex pattern to extract control items.
// Returns (nil, false) when no pattern is provided or no matches are found.
// If the regex has 2+ capture groups: group 1 = ID, group 2 = text.
// If the regex has 1 capture group: that's the text, ID = "sec-N".
func TierRegex(doc StructuredDoc, opts StructureOptions) ([]ControlItem, bool) {
	if opts.SectionPattern == "" {
		return nil, false
	}

	if len(opts.SectionPattern) > maxPatternLen {
		return nil, false
	}

	// Compile with multiline flag to support ^ and $ anchors across newlines
	pattern := opts.SectionPattern
	if !strings.HasPrefix(pattern, "(?m)") {
		pattern = "(?m)" + pattern
	}

	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, false
	}

	matches := re.FindAllStringSubmatch(doc.RawText, -1)
	if len(matches) == 0 {
		return nil, false
	}

	items := make([]ControlItem, 0, len(matches))
	for i, match := range matches {
		var id, text string

		// match[0] is the full match, match[1:] are capture groups
		switch {
		case len(match) >= 3:
			id = match[1]
			text = match[2]
		case len(match) == 2:
			id = fmt.Sprintf("sec-%d", i+1)
			text = match[1]
		default:
			id = fmt.Sprintf("sec-%d", i+1)
			text = match[0]
		}

		items = append(items, ControlItem{
			ID:    id,
			Text:  text,
			Class: ClassRequirement,
		})
	}

	return items, true
}
