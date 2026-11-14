package oscal

import (
	"fmt"
	"regexp"
	"slices"
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

// DefaultRequirementPartNames is the requirement part name list used when the
// parser is given none: the OSCAL "statement" part. Callers must not mutate
// it; copy it to derive another list.
var DefaultRequirementPartNames = []string{"statement"}

// DecomposeStatements turns an OSCAL control into its control row and one row
// per part of its requirement part.
//
// The requirement part is found by findRequirementPart: for each name in
// requirementParts, in order, the first direct child part of the control with
// that name whose subtree holds non-blank prose. No other part (guidance,
// overview, ...) contributes text, so a control without a requirement part
// gets a single ClassRequirement row with blank text.
//
// When the requirement part has no child parts, the control is a single
// ClassRequirement row carrying the part's prose. Otherwise the control row is
// a ClassSection carrying the part's own prose (its lead-in), and every child
// part, whatever its name, is walked to any depth: a part with children becomes
// a ClassSection row (its prose a lead-in), a leaf a ClassRequirement row. Each
// row's ID is derived from its part ID against its parent row's ID
// (deriveChildID); it records Props "parent-id" and "source-part" and the
// part's published label and sort-id. Parts decoded from JSON are a finite
// tree, so the walk terminates without a depth bound.
func DecomposeStatements(ctrl oscalTypes.Control, groupID string, params map[string]string, requirementParts []string) []ControlItem {
	control := ControlItem{
		ID:      ctrl.ID,
		Title:   ctrl.Title,
		Class:   ClassRequirement,
		GroupID: groupID,
		Props:   make(map[string]string),
	}
	setPublishedProps(control.Props, ctrl.Props)

	req := findRequirementPart(ctrl, requirementParts)
	if req == nil {
		return []ControlItem{control}
	}
	control.Text = CleanProse(req.Prose, params)
	if req.Parts == nil || len(*req.Parts) == 0 {
		return []ControlItem{control}
	}
	control.Class = ClassSection
	return appendPartRows([]ControlItem{control}, ctrl.ID, *req.Parts, groupID, params)
}

// appendPartRows appends a row for each part, in document order, each followed
// by the rows of its own child parts.
func appendPartRows(rows []ControlItem, parentID string, parts []oscalTypes.Part, groupID string, params map[string]string) []ControlItem {
	for idx, part := range parts {
		hasChildren := part.Parts != nil && len(*part.Parts) > 0
		row := ControlItem{
			ID:       deriveChildID(parentID, part, idx),
			Text:     CleanProse(part.Prose, params),
			Class:    ClassRequirement,
			ParentID: parentID,
			GroupID:  groupID,
			Props: map[string]string{
				"parent-id":   parentID,
				"source-part": part.ID,
			},
		}
		if hasChildren {
			row.Class = ClassSection
		}
		setPublishedProps(row.Props, part.Props)
		rows = append(rows, row)
		if hasChildren {
			rows = appendPartRows(rows, row.ID, *part.Parts, groupID, params)
		}
	}
	return rows
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
func DecomposeText(baseID, text string, minWords int) []ControlItem {
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

// findRequirementPart returns the control's requirement part: for each name in
// order, the first direct child part with that name whose subtree has prose.
// It returns nil when no name yields one.
func findRequirementPart(ctrl oscalTypes.Control, names []string) *oscalTypes.Part {
	if ctrl.Parts == nil {
		return nil
	}
	for _, name := range names {
		for i := range *ctrl.Parts {
			part := &(*ctrl.Parts)[i]
			if part.Name == name && hasProse(*part) {
				return part
			}
		}
	}
	return nil
}

// hasProse reports whether the part or any part below it has non-blank prose.
func hasProse(part oscalTypes.Part) bool {
	if strings.TrimSpace(part.Prose) != "" {
		return true
	}
	if part.Parts == nil {
		return false
	}
	for _, child := range *part.Parts {
		if hasProse(child) {
			return true
		}
	}
	return false
}

// prosePartNames returns the sorted, deduplicated names of the control's
// direct child parts whose subtree has prose. Unnamed parts are skipped: a
// blank name cannot be configured as a requirement part name.
func prosePartNames(ctrl oscalTypes.Control) []string {
	if ctrl.Parts == nil {
		return nil
	}
	var names []string
	for _, part := range *ctrl.Parts {
		if part.Name != "" && hasProse(part) {
			names = append(names, part.Name)
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
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
	if id, ok := idFromPartID(parentID, part.ID); ok {
		return id
	}
	return fmt.Sprintf("%s.%d", parentID, index+1)
}

// idFromPartID derives a child ID from the part's own ID, stripping a _smt or
// _stmt suffix or keeping an ID that already extends parentID. ok is false
// when the part ID is empty or follows neither pattern, and the caller must
// number the part by position instead.
func idFromPartID(parentID, partID string) (id string, ok bool) {
	if partID == "" {
		return "", false
	}
	if strings.Contains(partID, "_smt.") {
		return strings.Replace(partID, "_smt.", ".", 1), true
	}
	if strings.Contains(partID, "_stmt.") {
		return strings.Replace(partID, "_stmt.", ".", 1), true
	}
	if strings.HasPrefix(partID, parentID+".") {
		return partID, true
	}
	return "", false
}

// wordCount counts words in a string using strings.Fields.
func wordCount(s string) int {
	return len(strings.Fields(s))
}
