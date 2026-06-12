package oscal

import (
	"regexp"
	"strings"
)

// TierPattern (Tier 3) auto-detects common numbering patterns.
// Tries 12 candidate patterns in order, selecting the first that matches at least 3 lines.
// Returns (nil, false) when no pattern matches enough lines.
func TierPattern(doc StructuredDoc, opts StructureOptions) ([]ControlItem, bool) {
	// 12 candidate patterns in priority order
	patterns := []string{
		`^\d+\.\d+`,      // 1.1, 2.3
		`^[A-Z]\.\d+`,    // A.1, B.2
		`^Article\s+\d+`, // Article 1
		`^Section\s+\d+`, // Section 1
		`^Rule\s+\d+`,    // Rule 1
		`^§\s*\d+`,       // § 1, §1
		`^\([a-z]\)`,     // (a), (b)
		`^\(\d+\)`,       // (1), (2)
		`^[ivxlcdm]+\.`,  // i., ii., iii. (roman numerals)
		`^[a-z]\.`,       // a., b., c.
		`^[A-Z]\.`,       // A., B., C.
		`^\d+\.`,         // 1., 2., 3.
	}

	lines := strings.Split(doc.RawText, "\n")
	minMatches := 3

	// Try each pattern
	for _, patternStr := range patterns {
		re := regexp.MustCompile(patternStr)
		var matchedLines []string

		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
			if re.MatchString(trimmed) {
				matchedLines = append(matchedLines, trimmed)
			}
		}

		// If this pattern matched enough lines, use it
		if len(matchedLines) >= minMatches {
			items := make([]ControlItem, 0, len(matchedLines))
			for _, line := range matchedLines {
				// Extract ID from the pattern match
				match := re.FindString(line)
				id := strings.TrimSpace(match)

				items = append(items, ControlItem{
					ID:    id,
					Text:  line,
					Class: ClassRequirement,
				})
			}
			return items, true
		}
	}

	return nil, false
}
