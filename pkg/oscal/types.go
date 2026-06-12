package oscal

// ControlItem is the internal domain representation of a compliance control.
type ControlItem struct {
	ID       string            // Control identifier (e.g., "ac-2", "ac-2.a")
	Title    string            // Human-readable title
	Text     string            // Full statement prose (params substituted)
	Class    string            // One of ClassSection, ClassRequirement, ClassContext
	ParentID string            // Parent control ID (empty for top-level)
	GroupID  string            // Enclosing group ID
	Props    map[string]string // Arbitrary properties (e.g., "parent-id", "source-part")
}

const (
	ClassSection     = "compliance-section"
	ClassRequirement = "compliance-requirement"
	ClassContext     = "compliance-context"
)
