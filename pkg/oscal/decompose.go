package oscal

import (
	"fmt"
	"regexp"
	"strings"

	oscalTypes "github.com/defenseunicorns/go-oscal/src/types/oscal-1-1-3"
)

// Regex patterns for text decomposition (5 tiers)
// Note: Go regexp doesn't support lookahead, so we match line by line
var (
	// Tier 1: Parenthesized clauses (a), (b), (1), (2), etc.
	tierParenthesized = regexp.MustCompile(`(?m)^\s*\(([a-zA-Z]{1,3}|\d+)\)\s+(.+)$`)

	// Tier 2: Numbered paragraphs (1. 2. 3.)
	tierNumbered = regexp.MustCompile(`(?m)^\s*(\d{1,4})\.\s+(.+)$`)

	// Tier 3: Roman numeral paragraphs (i. ii. iii.)
	tierRoman = regexp.MustCompile(`(?m)^\s*(xiii|xii|xi|viii|vii|vi|iv|iii|ii|ix|x|v|i)\.\s+(.+)$`)

	// Tier 4: Lowercase letter paragraphs (a. b. c.)
	tierLowercase = regexp.MustCompile(`(?m)^\s*([a-z])\.\s+(.+)$`)

	// Tier 5: Uppercase letter paragraphs (A. B. C.)
	tierUppercase = regexp.MustCompile(`(?m)^\s*([A-Z])\.\s+(.+)$`)
)

// DecomposeStatements breaks an OSCAL control with statement items into
// parent (ClassSection) + children (ClassRequirement).
//
// Logic:
//  1. Find the "statement" part (part where Name == "statement")
//  2. If no statement or no items within it → return single ControlItem with ClassRequirement
//  3. Collect "item" parts from within the statement
//  4. Create parent ControlItem: original control with ClassSection, prose = statement's top-level prose only
//  5. For each item part:
//     - If the item has its own sub-items → item becomes ClassSection, sub-items become ClassRequirement
//     - If no sub-items → item becomes ClassRequirement
//  6. ID derivation: strip `_smt` or `_stmt` suffix from part ID (e.g., `ac-2_smt.a` → `ac-2.a`)
//  7. Fallback ID: `{controlID}.{index}` when part ID doesn't follow the pattern
//
// Parent-child linkage: children set `ParentID` to parent's ID and have `Props["parent-id"]` set.
func DecomposeStatements(ctrl oscalTypes.Control, groupID string, params map[string]string) []ControlItem {
	controlID := ctrl.ID
	title := ctrl.Title

	// Find the statement part
	statement := findStatement(ctrl)
	if statement == nil {
		// No statement part → return single item with ClassRequirement
		return []ControlItem{{
			ID:      controlID,
			Title:   title,
			Text:    CleanProse(extractControlProse(ctrl), params),
			Class:   ClassRequirement,
			GroupID: groupID,
			Props:   make(map[string]string),
		}}
	}

	// Collect item parts from within the statement
	items := collectItems(*statement)
	if len(items) == 0 {
		// Statement exists but has no items → return single item with ClassRequirement
		return []ControlItem{{
			ID:      controlID,
			Title:   title,
			Text:    CleanProse(statement.Prose, params),
			Class:   ClassRequirement,
			GroupID: groupID,
			Props:   make(map[string]string),
		}}
	}

	// Create parent ControlItem with ClassSection
	parent := ControlItem{
		ID:      controlID,
		Title:   title,
		Text:    CleanProse(statement.Prose, params),
		Class:   ClassSection,
		GroupID: groupID,
		Props:   make(map[string]string),
	}

	result := []ControlItem{parent}

	// Process each item
	for idx, item := range items {
		childID := deriveChildID(controlID, item, idx)

		// Check if item has sub-items
		subItems := collectItems(item)

		if len(subItems) > 0 {
			// Item has sub-items → item becomes ClassSection
			itemSection := ControlItem{
				ID:       childID,
				Title:    "", // Items don't have titles
				Text:     CleanProse(item.Prose, params),
				Class:    ClassSection,
				ParentID: controlID,
				GroupID:  groupID,
				Props: map[string]string{
					"parent-id":   controlID,
					"source-part": item.ID,
				},
			}
			result = append(result, itemSection)

			// Process sub-items as ClassRequirement
			for subIdx, subItem := range subItems {
				subChildID := deriveChildID(childID, subItem, subIdx)
				subRequirement := ControlItem{
					ID:       subChildID,
					Title:    "",
					Text:     CleanProse(subItem.Prose, params),
					Class:    ClassRequirement,
					ParentID: childID,
					GroupID:  groupID,
					Props: map[string]string{
						"parent-id":   childID,
						"source-part": subItem.ID,
					},
				}
				result = append(result, subRequirement)
			}
		} else {
			// No sub-items → item becomes ClassRequirement
			itemRequirement := ControlItem{
				ID:       childID,
				Title:    "",
				Text:     CleanProse(item.Prose, params),
				Class:    ClassRequirement,
				ParentID: controlID,
				GroupID:  groupID,
				Props: map[string]string{
					"parent-id":   controlID,
					"source-part": item.ID,
				},
			}
			result = append(result, itemRequirement)
		}
	}

	return result
}

// DecomposeText applies a 5-tier regex cascade to split unstructured text into sub-requirements.
//
// 5-tier regex cascade for splitting unstructured text into sub-requirements:
//
//	| Tier | Pattern                                          | Min words per match |
//	|------|--------------------------------------------------|---------------------|
//	| 1    | ^\s*\(([a-zA-Z]{1,3}|\d+)\)\s+(.+)$             | none                |
//	| 2    | ^\s*(\d{1,4})\.\s+(.+)$                         | 8                   |
//	| 3    | ^\s*(xiii|xii|xi|viii|vii|vi|iv|iii|ii|ix|x|v|i)\.\s+(.+)$ | 8    |
//	| 4    | ^\s*([a-z])\.\s+(.+)$                           | 8                   |
//	| 5    | ^\s*([A-Z])\.\s+(.+)$                           | 8                   |
//
// Rules:
//   - Each tier requires at least 2 matches to trigger
//   - Tiers tried in order; first that produces results wins
//   - minWords check: if text has fewer than minWords words, return single item (no decomposition)
//   - Decomposition is recursive: matched sub-clauses are passed through the cascade again
//   - Containers (items with sub-matches) get ClassSection; leaves get ClassRequirement
func DecomposeText(baseID string, text string, minWords int) []ControlItem {
	// Check minimum word count
	if wordCount(text) < minWords {
		return []ControlItem{{
			ID:    baseID,
			Text:  strings.TrimSpace(text),
			Class: ClassRequirement,
			Props: make(map[string]string),
		}}
	}

	// Try each tier in order
	tiers := []struct {
		pattern     *regexp.Regexp
		minWordsReq int
		tierName    string
	}{
		{tierParenthesized, 0, "parenthesized"},
		{tierNumbered, 8, "numbered"},
		{tierRoman, 8, "roman"},
		{tierLowercase, 8, "lowercase"},
		{tierUppercase, 8, "uppercase"},
	}

	for _, tier := range tiers {
		items := extractItems(text, tier.pattern, tier.minWordsReq)
		if len(items) < 2 {
			continue
		}

		// This tier produced valid results — process items
		var result []ControlItem

		for _, item := range items {
			childID := fmt.Sprintf("%s.%s", baseID, item.label)

			// Recursively decompose the matched text
			children := DecomposeText(childID, item.text, minWords)

			if len(children) > 1 {
				// Recursive decomposition produced sub-items → this is a section
				section := ControlItem{
					ID:    childID,
					Text:  item.text,
					Class: ClassSection,
					Props: map[string]string{
						"label": item.label,
					},
				}
				result = append(result, section)

				// Add children with parent linkage
				for _, child := range children {
					child.ParentID = childID
					if child.Props == nil {
						child.Props = make(map[string]string)
					}
					child.Props["parent-id"] = childID
					result = append(result, child)
				}
			} else {
				// No sub-decomposition → this is a requirement
				result = append(result, ControlItem{
					ID:    childID,
					Text:  item.text,
					Class: ClassRequirement,
					Props: map[string]string{
						"label": item.label,
					},
				})
			}
		}

		if len(result) > 0 {
			return result
		}
	}

	// No tier produced results → return single item
	return []ControlItem{{
		ID:    baseID,
		Text:  strings.TrimSpace(text),
		Class: ClassRequirement,
		Props: make(map[string]string),
	}}
}

// extractedItem holds a labeled text segment extracted by a tier pattern.
type extractedItem struct {
	label string
	text  string
}

// extractItems groups consecutive matching lines into labeled text segments.
// It scans the text line by line, grouping consecutive lines that match the pattern
// together under the same label.
func extractItems(text string, pattern *regexp.Regexp, minWords int) []extractedItem {
	lines := strings.Split(text, "\n")
	var items []extractedItem
	var currentLabel string
	var currentText strings.Builder

	flush := func() {
		if currentLabel != "" && currentText.Len() > 0 {
			itemText := strings.TrimSpace(currentText.String())
			if minWords == 0 || wordCount(itemText) >= minWords {
				items = append(items, extractedItem{
					label: currentLabel,
					text:  itemText,
				})
			}
			currentLabel = ""
			currentText.Reset()
		}
	}

	for _, line := range lines {
		match := pattern.FindStringSubmatch(line)
		if len(match) >= 3 {
			// New labeled item
			flush()
			currentLabel = match[1]
			currentText.WriteString(match[2])
		} else if currentLabel != "" {
			// Continuation of current item
			if currentText.Len() > 0 {
				currentText.WriteString("\n")
			}
			currentText.WriteString(line)
		}
	}

	flush()
	return items
}

// findStatement finds the statement part within a control.
func findStatement(ctrl oscalTypes.Control) *oscalTypes.Part {
	if ctrl.Parts == nil {
		return nil
	}
	for _, part := range *ctrl.Parts {
		if part.Name == "statement" {
			return &part
		}
	}
	return nil
}

// collectItems collects parts with Name=="item" from a given part.
func collectItems(part oscalTypes.Part) []oscalTypes.Part {
	if part.Parts == nil {
		return nil
	}
	var items []oscalTypes.Part
	for _, p := range *part.Parts {
		if p.Name == "item" {
			items = append(items, p)
		}
	}
	return items
}

// deriveChildID derives a child ID by stripping _smt/_stmt suffix from part ID.
// Falls back to {parentID}.{index} when part ID doesn't follow the pattern.
//
// Examples:
//   - ac-2_smt.a → ac-2.a
//   - ac-2_stmt.a → ac-2.a
//   - ac-2.a → ac-2.a (no suffix to strip)
//   - empty → {parentID}.{index}
func deriveChildID(parentID string, part oscalTypes.Part, index int) string {
	partID := part.ID
	if partID == "" {
		return fmt.Sprintf("%s.%d", parentID, index+1)
	}

	// Try to strip _smt or _stmt suffix
	if strings.Contains(partID, "_smt.") {
		return strings.Replace(partID, "_smt.", ".", 1)
	}
	if strings.Contains(partID, "_stmt.") {
		return strings.Replace(partID, "_stmt.", ".", 1)
	}

	// Check if it already looks like a derived ID (has a dot after parent)
	if strings.HasPrefix(partID, parentID+".") {
		return partID
	}

	// Fallback: use index
	return fmt.Sprintf("%s.%d", parentID, index+1)
}

// wordCount counts words in a string using strings.Fields.
func wordCount(s string) int {
	return len(strings.Fields(s))
}

// extractControlProse extracts prose from a control's parts (fallback for controls without statement).
func extractControlProse(ctrl oscalTypes.Control) string {
	if ctrl.Parts == nil || len(*ctrl.Parts) == 0 {
		return ""
	}

	var prose strings.Builder
	for _, part := range *ctrl.Parts {
		if part.Prose != "" {
			if prose.Len() > 0 {
				prose.WriteString("\n\n")
			}
			prose.WriteString(part.Prose)
		}
	}
	return prose.String()
}
