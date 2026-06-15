package oscal

import (
	"fmt"
	"strings"
)

// TierTable (Tier 2b) extracts control items from markdown tables.
// Returns (nil, false) when no tables are present.
// For each table row: ID = first column (or "tbl-N"), Text = all columns joined by " | ".
func TierTable(doc StructuredDoc, opts StructureOptions) ([]ControlItem, bool) {
	if len(doc.Tables) == 0 {
		return nil, false
	}

	items := make([]ControlItem, 0)
	globalRowIndex := 0

	for _, table := range doc.Tables {
		for _, row := range table.Rows {
			if len(row) == 0 {
				continue
			}

			globalRowIndex++
			var id string
			var textParts []string

			// Use first column as ID if it looks like an identifier (non-empty and not too long)
			firstCol := strings.TrimSpace(row[0])
			if firstCol != "" && len(firstCol) < 50 {
				id = firstCol
				// Include all columns in text, including the first
				textParts = row
			} else {
				// Auto-generate ID
				id = fmt.Sprintf("tbl-%d", globalRowIndex)
				textParts = row
			}

			text := strings.Join(textParts, " | ")

			items = append(items, ControlItem{
				ID:    id,
				Text:  text,
				Class: ClassRequirement,
			})
		}
	}

	if len(items) == 0 {
		return nil, false
	}

	return items, true
}
