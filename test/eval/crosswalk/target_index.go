package crosswalk

import (
	"slices"

	oscalTypes "github.com/defenseunicorns/go-oscal/src/types/oscal-1-1-3"

	"github.com/complytime-labs/crosscodex/pkg/oscal"
)

// ResolutionState is the outcome of resolving an identifier against a catalog.
type ResolutionState int

const (
	// Resolved means exactly one control matches.
	Resolved ResolutionState = iota
	// Unresolved means no control matches.
	Unresolved
	// Ambiguous means two or more different controls match; the identifier is
	// reported and never resolved by guessing (fail closed).
	Ambiguous
)

// Resolution is the result of TargetIndex.Resolve.
type Resolution struct {
	ControlID string
	State     ResolutionState
	// Candidates lists the matching control IDs when State is Ambiguous.
	Candidates []string
}

// TargetIndex resolves identifiers against one target catalog. It indexes each
// control under the Canonical form of its ID and of every label prop, so the
// catalog alone defines which identifier spellings are understood.
//
// The importer also stores each control's statement items (pl-2.a.10 under
// pl-2) as controls, and the analyzers map those items, so Has and Base know
// them too. Resolve does not: references cite OSCAL controls.
type TargetIndex struct {
	byForm map[string][]string // canonical form -> distinct control IDs, document order
	parent map[string]string   // control or item ID -> parent ID ("" at top level)
	items  map[string]string   // statement item ID -> parent ID, for IDs that are not controls
	owner  map[string]string   // statement item ID -> the control it was derived from
}

// NewTargetIndex indexes every control of c, at any depth.
func NewTargetIndex(c *Catalog) *TargetIndex {
	x := &TargetIndex{byForm: map[string][]string{}, parent: map[string]string{}, items: map[string]string{}, owner: map[string]string{}}
	var walk func(controls *[]oscalTypes.Control, parent string)
	var walkGroups func(groups *[]oscalTypes.Group)
	walk = func(controls *[]oscalTypes.Control, parent string) {
		if controls == nil {
			return
		}
		for _, ctrl := range *controls {
			x.add(ctrl, parent)
			walk(ctrl.Controls, ctrl.ID)
		}
	}
	walkGroups = func(groups *[]oscalTypes.Group) {
		if groups == nil {
			return
		}
		for _, g := range *groups {
			walk(g.Controls, "")
			walkGroups(g.Groups)
		}
	}
	walkGroups(c.Doc.Groups)
	walk(c.Doc.Controls, "")
	return x
}

func (x *TargetIndex) add(ctrl oscalTypes.Control, parent string) {
	x.parent[ctrl.ID] = parent
	// The importer derives item IDs with exactly this function; the params
	// only fill statement text, which the index does not use.
	for _, item := range oscal.DecomposeStatements(ctrl, "", nil, oscal.DefaultRequirementPartNames) {
		if item.ID != ctrl.ID {
			x.items[item.ID] = item.ParentID
			x.owner[item.ID] = ctrl.ID
		}
	}
	forms := []string{Canonical(ctrl.ID)}
	if ctrl.Props != nil {
		for _, p := range *ctrl.Props {
			if p.Name == "label" {
				forms = append(forms, Canonical(p.Value))
			}
		}
	}
	for _, f := range forms {
		if f != "" && !slices.Contains(x.byForm[f], ctrl.ID) {
			x.byForm[f] = append(x.byForm[f], ctrl.ID)
		}
	}
}

// Resolve maps an identifier to the one control it names. An identifier that
// canonicalises to nothing, or matches no control, is Unresolved.
func (x *TargetIndex) Resolve(identifier string) Resolution {
	ids := x.byForm[Canonical(identifier)]
	switch len(ids) {
	case 0:
		return Resolution{State: Unresolved}
	case 1:
		return Resolution{ControlID: ids[0], State: Resolved}
	default:
		return Resolution{State: Ambiguous, Candidates: slices.Clone(ids)}
	}
}

// Base returns the outermost ancestor control of controlID (the control itself
// at top level), or "" when controlID is unknown. An item's ancestors are its
// section items and then its control.
func (x *TargetIndex) Base(controlID string) string {
	parent, ok := x.lookup(controlID)
	if !ok {
		return ""
	}
	for parent != "" {
		controlID = parent
		parent, _ = x.lookup(controlID)
	}
	return controlID
}

// Has reports whether controlID is a control or statement item of the indexed
// catalog.
func (x *TargetIndex) Has(controlID string) bool {
	_, ok := x.lookup(controlID)
	return ok
}

// Owner returns the control that contains controlID: the control itself, or the
// control whose statement the importer derived the item from. It returns ""
// when controlID is unknown. Unlike Base it stops at the nearest control, so an
// item of an enhancement belongs to the enhancement.
func (x *TargetIndex) Owner(controlID string) string {
	if _, ok := x.parent[controlID]; ok {
		return controlID
	}
	return x.owner[controlID]
}

// lookup returns the parent of controlID. A control wins over an item of the
// same ID (the importer's fallback item IDs, "<control>.<n>", can equal an
// enhancement's ID), so the control tree decides.
func (x *TargetIndex) lookup(controlID string) (string, bool) {
	if parent, ok := x.parent[controlID]; ok {
		return parent, true
	}
	parent, ok := x.items[controlID]
	return parent, ok
}
