package oscal

import (
	"fmt"

	oscalTypes "github.com/defenseunicorns/go-oscal/src/types/oscal-1-1-3"
)

// ControlVisitor is a function type for visiting controls during traversal.
// ctrl is the control being visited
// groupID is the ID of the enclosing group (empty string for top-level catalog controls)
// depth is the nesting level (0 for top-level controls, 1+ for sub-controls/enhancements)
type ControlVisitor func(ctrl oscalTypes.Control, groupID string, depth int)

// WalkControls recursively walks all controls in the catalog, visiting each control with the provided visitor function.
// It traverses:
// - Top-level catalog controls (catalog.Controls) with empty groupID
// - All groups recursively (Group.Groups can nest)
// - Controls within each group
// - Sub-controls (Control.Controls — OSCAL "enhancements") with incremented depth
func WalkControls(catalog *oscalTypes.Catalog, visitor ControlVisitor) {
	if catalog == nil {
		return
	}

	// Visit top-level catalog controls (ungrouped)
	if catalog.Controls != nil {
		for _, ctrl := range *catalog.Controls {
			visitControl(ctrl, "", 0, visitor)
		}
	}

	// Walk all groups recursively
	if catalog.Groups != nil {
		for _, group := range *catalog.Groups {
			walkGroup(group, visitor)
		}
	}
}

// walkGroup recursively walks a group and all its nested groups
func walkGroup(group oscalTypes.Group, visitor ControlVisitor) {
	groupID := group.ID

	// Visit controls in this group
	if group.Controls != nil {
		for _, ctrl := range *group.Controls {
			visitControl(ctrl, groupID, 0, visitor)
		}
	}

	// Recursively walk nested groups
	if group.Groups != nil {
		for _, nestedGroup := range *group.Groups {
			walkGroup(nestedGroup, visitor)
		}
	}
}

// visitControl visits a control and all its sub-controls recursively
func visitControl(ctrl oscalTypes.Control, groupID string, depth int, visitor ControlVisitor) {
	// Visit this control
	visitor(ctrl, groupID, depth)

	// Visit sub-controls (enhancements) with incremented depth
	if ctrl.Controls != nil {
		for _, subCtrl := range *ctrl.Controls {
			visitControl(subCtrl, groupID, depth+1, visitor)
		}
	}
}

// FindControlInSlice searches for a control with the exact matching ID in the provided slice.
// Returns a pointer to the matching ControlItem in the slice.
// Returns ErrControlNotFound (wrapped with control ID) if not found.
// Only exact ID matches are accepted — no prefix or partial matching.
func FindControlInSlice(items []ControlItem, controlID string) (*ControlItem, error) {
	for i := range items {
		if items[i].ID == controlID {
			return &items[i], nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrControlNotFound, controlID)
}
