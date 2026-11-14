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

// Assemble converts a Tree into a valid OSCAL catalog JSON document.
// Containers become (nested) groups, with the group id omitted when the
// container's Identifier is empty. Items carrying a PropNodeKind rebuild the
// source tree: control rows become controls nested under their base control or
// placed in their ContainerKey group, and statement-part rows become parts.
// Unmarked items keep the legacy rules (see buildTree). Groups and controls are
// emitted in input order. Output is validated if schema is set.
func (a *assembler) Assemble(ctx context.Context, tree *Tree, meta CatalogMeta) ([]byte, error) {
	groups, topControls := buildTree(tree)

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
	}
	if len(groups) > 0 {
		catalog.Groups = &groups
	}
	if len(topControls) > 0 {
		catalog.Controls = &topControls
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

// groupNode is a group under construction; children keep input order.
type groupNode struct {
	id       string
	title    string
	groups   []*groupNode
	controls []*controlNode
}

// controlNode is a control under construction: its row, nested controls
// (enhancements) and its statement-part rows by parent ID.
type controlNode struct {
	item     ControlItem
	controls []*controlNode
}

// buildTree arranges a tree into top-level groups and top-level controls.
//
// Containers become groups, nested by ParentKey (a container whose parent is
// unknown, or itself, is top level).
//
// Placement per item:
//   - control row: nested under its ParentID control if that is a control row;
//     otherwise in its ContainerKey group; otherwise in the group whose id is
//     its GroupID (synthesized, titled by its ID, if no container has that
//     Identifier); otherwise top-level controls when it has no GroupID.
//   - statement-part row, or any unmarked item with a ParentID: a part of its
//     parent (resolved by buildParts).
//   - unmarked item without ParentID: a control in its ContainerKey group, else
//     in the GroupID group, which joins the container with that Identifier if
//     there is one, else a group titled by its ID is synthesized; a top-level
//     control when GroupID is empty (no group is invented for it).
func buildTree(tree *Tree) (groups []oscalTypes.Group, topControls []oscalTypes.Control) {
	byKey := make(map[string]*groupNode)
	byGroupID := make(map[string]*groupNode)
	var topGroups []*groupNode
	// Register containers first so children can reference a container that
	// appears later in the input; parenting below uses first-appearance order.
	for _, c := range tree.Containers {
		node := &groupNode{id: c.Identifier, title: c.Title}
		byKey[c.Key] = node
		if _, taken := byGroupID[c.Identifier]; c.Identifier != "" && !taken {
			byGroupID[c.Identifier] = node
		}
	}
	for _, c := range tree.Containers {
		if parent, ok := byKey[c.ParentKey]; ok && c.ParentKey != c.Key {
			parent.groups = append(parent.groups, byKey[c.Key])
		} else {
			topGroups = append(topGroups, byKey[c.Key])
		}
	}

	controlNodes := make(map[string]*controlNode)
	for _, it := range tree.Items {
		if it.Props[PropNodeKind] == PropNodeKindControl {
			controlNodes[it.ID] = &controlNode{item: it}
		}
	}

	childrenMap := make(map[string][]ControlItem)
	var topCtrlNodes []*controlNode
	groupFor := func(id string) *groupNode {
		if g, ok := byGroupID[id]; ok {
			return g
		}
		g := &groupNode{id: id, title: id}
		byGroupID[id] = g
		topGroups = append(topGroups, g)
		return g
	}
	for _, it := range tree.Items {
		switch it.Props[PropNodeKind] {
		case PropNodeKindControl:
			node := controlNodes[it.ID]
			if parent, ok := controlNodes[it.ParentID]; ok && it.ParentID != it.ID {
				parent.controls = append(parent.controls, node)
			} else if g, ok := byKey[it.ContainerKey]; ok {
				g.controls = append(g.controls, node)
			} else if it.GroupID != "" {
				g := groupFor(it.GroupID)
				g.controls = append(g.controls, node)
			} else {
				topCtrlNodes = append(topCtrlNodes, node)
			}
		default:
			if it.ParentID != "" {
				childrenMap[it.ParentID] = append(childrenMap[it.ParentID], it)
				continue
			}
			if g, ok := byKey[it.ContainerKey]; ok {
				g.controls = append(g.controls, &controlNode{item: it})
				continue
			}
			if it.GroupID == "" {
				topCtrlNodes = append(topCtrlNodes, &controlNode{item: it})
				continue
			}
			g := groupFor(it.GroupID)
			g.controls = append(g.controls, &controlNode{item: it})
		}
	}

	groups = make([]oscalTypes.Group, 0, len(topGroups))
	for _, g := range topGroups {
		groups = append(groups, buildGroup(g, childrenMap))
	}
	for _, c := range topCtrlNodes {
		topControls = append(topControls, buildControl(c, childrenMap))
	}
	return groups, topControls
}

// buildGroup converts a groupNode, and recursively its subgroups, to an OSCAL Group.
func buildGroup(g *groupNode, childrenMap map[string][]ControlItem) oscalTypes.Group {
	group := oscalTypes.Group{ID: g.id, Title: g.title}
	if len(g.controls) > 0 {
		controls := make([]oscalTypes.Control, 0, len(g.controls))
		for _, c := range g.controls {
			controls = append(controls, buildControl(c, childrenMap))
		}
		group.Controls = &controls
	}
	if len(g.groups) > 0 {
		subgroups := make([]oscalTypes.Group, 0, len(g.groups))
		for _, sub := range g.groups {
			subgroups = append(subgroups, buildGroup(sub, childrenMap))
		}
		group.Groups = &subgroups
	}
	return group
}

// buildControl converts a controlNode to an OSCAL Control: its statement-part
// rows become parts and its enhancements become nested controls.
func buildControl(n *controlNode, childrenMap map[string][]ControlItem) oscalTypes.Control {
	ctrl := ItemToControl(n.item)
	if children, hasChildren := childrenMap[n.item.ID]; hasChildren {
		parts := buildParts(children, childrenMap)
		if ctrl.Parts == nil {
			ctrl.Parts = &parts
		} else {
			existing := append(*ctrl.Parts, parts...)
			ctrl.Parts = &existing
		}
	}
	if len(n.controls) > 0 {
		nested := make([]oscalTypes.Control, 0, len(n.controls))
		for _, c := range n.controls {
			nested = append(nested, buildControl(c, childrenMap))
		}
		ctrl.Controls = &nested
	}
	return ctrl
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
