package oscal

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	oscalTypes "github.com/defenseunicorns/go-oscal/src/types/oscal-1-1-3"
	"github.com/google/uuid"
)

// assembler implements the Assembler interface.
type assembler struct {
	schemaPath string
}

// NewAssembler creates an Assembler that converts ControlItems into OSCAL JSON.
// If schemaPath is empty, output validation is skipped.
func NewAssembler(schemaPath string) Assembler {
	return &assembler{schemaPath: schemaPath}
}

// Assemble converts ControlItems into a valid OSCAL catalog JSON document.
// Groups items by GroupID, wires parent-child relationships, and validates output if schema is set.
func (a *assembler) Assemble(ctx context.Context, items []ControlItem, meta CatalogMeta) ([]byte, error) {
	// Group items by GroupID
	grouped := groupByGroupID(items)

	// Build groups and controls
	groups := make([]oscalTypes.Group, 0, len(grouped))
	for groupID, groupItems := range grouped {
		group := buildGroup(groupID, groupItems)
		groups = append(groups, group)
	}

	// Build catalog metadata
	catalogMeta := oscalTypes.Metadata{
		Title:        meta.Title,
		Version:      meta.Version,
		OscalVersion: "1.1.3",
		LastModified: time.Now(),
	}

	// Build catalog
	catalog := oscalTypes.Catalog{
		UUID:     uuid.New().String(),
		Metadata: catalogMeta,
		Groups:   &groups,
	}

	// Wrap in OscalCompleteSchema
	wrapper := oscalTypes.OscalCompleteSchema{
		Catalog: &catalog,
	}

	// Marshal to JSON
	data, err := json.Marshal(wrapper)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal OSCAL catalog: %w", err)
	}

	// Validate if schema is set
	if a.schemaPath != "" {
		if err := ValidateSchema(data, a.schemaPath); err != nil {
			return nil, err
		}
	}

	return data, nil
}

// groupByGroupID organizes items into a map keyed by GroupID.
// Items with empty GroupID go to "default" group.
func groupByGroupID(items []ControlItem) map[string][]ControlItem {
	grouped := make(map[string][]ControlItem)
	for _, item := range items {
		groupID := item.GroupID
		if groupID == "" {
			groupID = "default"
		}
		grouped[groupID] = append(grouped[groupID], item)
	}
	return grouped
}

// buildGroup creates an OSCAL Group from ControlItems.
// Separates top-level items (ParentID == "") from children and wires children as Parts.
func buildGroup(groupID string, items []ControlItem) oscalTypes.Group {
	// Separate top-level items from children
	var topLevel []ControlItem
	childrenMap := make(map[string][]ControlItem)

	for _, item := range items {
		if item.ParentID == "" {
			topLevel = append(topLevel, item)
		} else {
			childrenMap[item.ParentID] = append(childrenMap[item.ParentID], item)
		}
	}

	// Build controls from top-level items
	controls := make([]oscalTypes.Control, 0, len(topLevel))
	for _, item := range topLevel {
		ctrl := ItemToControl(item)

		// Wire children as parts
		if children, hasChildren := childrenMap[item.ID]; hasChildren {
			parts := buildParts(children, childrenMap)
			if ctrl.Parts == nil {
				ctrl.Parts = &parts
			} else {
				// Append to existing parts
				existing := *ctrl.Parts
				existing = append(existing, parts...)
				ctrl.Parts = &existing
			}
		}

		controls = append(controls, ctrl)
	}

	// Build group
	group := oscalTypes.Group{
		ID:       groupID,
		Controls: &controls,
	}

	// Set title for non-default groups
	if groupID != "default" {
		group.Title = groupID
	}

	return group
}

// buildParts recursively converts ControlItems to OSCAL Parts.
// Each item becomes a Part, and its children (via childrenMap) become nested Parts.
func buildParts(items []ControlItem, childrenMap map[string][]ControlItem) []oscalTypes.Part {
	parts := make([]oscalTypes.Part, 0, len(items))

	for _, item := range items {
		part := oscalTypes.Part{
			ID:    item.ID,
			Name:  "item",
			Prose: item.Text,
		}

		// Add title if present
		if item.Title != "" {
			part.Title = item.Title
		}

		// Recursively add children
		if children, hasChildren := childrenMap[item.ID]; hasChildren {
			childParts := buildParts(children, childrenMap)
			part.Parts = &childParts
		}

		parts = append(parts, part)
	}

	return parts
}
