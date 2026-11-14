package oscal

import (
	"fmt"
	"strconv"

	oscalTypes "github.com/defenseunicorns/go-oscal/src/types/oscal-1-1-3"
)

// ControlVisitor is a function type for visiting controls during traversal.
// ctrl is the control being visited
// groupID is the source ID of the enclosing group (empty for top-level catalog
// controls and for controls in an ID-less group)
// groupKey is the position key of the enclosing group (empty for top-level
// catalog controls); see groupKey
// depth is the nesting level (0 for top-level controls, 1+ for sub-controls/enhancements)
type ControlVisitor func(ctrl oscalTypes.Control, groupID, groupKey string, depth int)

// WalkControls recursively walks all controls in the catalog, visiting each control with the provided visitor function.
// It traverses:
// - Top-level catalog controls (catalog.Controls) with empty groupID and groupKey
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
			visitControl(ctrl, "", "", 0, visitor)
		}
	}

	// Walk all groups recursively
	WalkGroups(catalog, func(group oscalTypes.Group, key, _ string) {
		if group.Controls == nil {
			return
		}
		for _, ctrl := range *group.Controls {
			visitControl(ctrl, group.ID, key, 0, visitor)
		}
	})
}

// GroupVisitor is a function type for visiting groups during traversal.
// key is the group's position key and parentKey the enclosing group's key
// (empty for a top-level group).
type GroupVisitor func(group oscalTypes.Group, key, parentKey string)

// WalkGroups visits every group in the catalog, with or without an ID, in
// pre-order (a group before its nested groups). A nil catalog visits nothing.
// The position key is "g" plus the 1-based sibling ordinals of the group and
// its ancestors joined by "." ("g2" is the second top-level group, "g2.1" its
// first subgroup), so a key identifies a group within one catalog regardless
// of its source ID.
func WalkGroups(catalog *oscalTypes.Catalog, visitor GroupVisitor) {
	if catalog == nil || catalog.Groups == nil {
		return
	}
	visitGroups(*catalog.Groups, "", visitor)
}

func visitGroups(groups []oscalTypes.Group, parentKey string, visitor GroupVisitor) {
	for i, group := range groups {
		ordinal := strconv.Itoa(i + 1)
		key := "g" + ordinal
		if parentKey != "" {
			key = parentKey + "." + ordinal
		}
		visitor(group, key, parentKey)
		if group.Groups != nil {
			visitGroups(*group.Groups, key, visitor)
		}
	}
}

// visitControl visits a control and all its sub-controls recursively
func visitControl(ctrl oscalTypes.Control, groupID, groupKey string, depth int, visitor ControlVisitor) {
	// Visit this control
	visitor(ctrl, groupID, groupKey, depth)

	// Visit sub-controls (enhancements) with incremented depth
	if ctrl.Controls != nil {
		for _, subCtrl := range *ctrl.Controls {
			visitControl(subCtrl, groupID, groupKey, depth+1, visitor)
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
