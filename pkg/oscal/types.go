package oscal

// ControlItem is the internal domain representation of a compliance control.
type ControlItem struct {
	ID       string // Control identifier (e.g., "ac-2", "ac-2.a")
	Title    string // Human-readable title
	Text     string // Full statement prose (params substituted)
	Class    string // One of ClassSection, ClassRequirement, ClassContext
	ParentID string // Parent control or part ID (empty for a top-level control, even inside a group)
	GroupID  string // Source ID of the innermost enclosing group ("" if none or ID-less)
	// ContainerKey is the Key of the innermost enclosing Container, set on every
	// item of a grouped control (root, enhancements, statement parts); "" when
	// the control is not in a group.
	ContainerKey string
	Props        map[string]string // Arbitrary properties (e.g., "parent-id", "source-part")
}

// Container organizes controls without carrying requirement text: an OSCAL
// group, and by design a ReqIF chapter or SysML package. Containers are stored
// apart from controls and rendered as their own graph nodes.
type Container struct {
	Key        string // Position key, unique within a catalog: "g" + sibling ordinals joined by "." ("g2.1")
	Identifier string // Source ID, "" when the source group has none
	Title      string
	Label      string // Published label (group "label" prop), "" when the source gives none
	ParentKey  string // Key of the enclosing container, "" for a top-level one
}

// Tree is a parsed catalog: its containers in pre-order (parents first) and
// its control, enhancement and statement-part items in document order.
type Tree struct {
	Containers []Container
	Items      []ControlItem
	// Textless lists, in document order, the controls that have neither
	// requirement text nor a usable title (see UsableTitle). They are still in
	// Items, keeping their place in the tree, but nothing can be analysed for
	// them; the import reports them so the user can name the part that holds
	// their text.
	Textless []TextlessControl
}

// TextlessControl identifies a control imported without requirement text.
type TextlessControl struct {
	Identifier string // The control's OSCAL id
	// PartNames are the names of the control's direct child parts that hold
	// prose anywhere in their subtree, sorted and deduplicated: the candidates
	// for the requirement part names.
	PartNames []string
}

const (
	ClassSection     = "compliance-section"
	ClassRequirement = "compliance-requirement"
	ClassContext     = "compliance-context"
)

// Node-kind vocabulary. Every item states its structural role in
// Props[PropNodeKind]; readers dispatch on it and never infer a role from
// Class, ParentID or other props. Items without it predate the vocabulary or
// come from non-OSCAL imports and keep legacy behaviour.
const (
	PropNodeKind              = "node-kind"
	PropNodeKindContainer     = "container"      // names container graph nodes; no row in controls carries it
	PropNodeKindControl       = "control"        // requirement-bearing node, possibly nested under another control
	PropNodeKindStatementPart = "statement-part" // decomposed piece of a control's text
)
