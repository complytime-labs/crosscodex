package oscal

// Catalog represents an OSCAL catalog.
type Catalog struct {
	UUID     string    // Unique catalog identifier
	Metadata Metadata  // Catalog metadata
	Groups   []Group   // Control groups
	Controls []Control // Top-level controls (ungrouped)
}

// Metadata holds catalog metadata.
type Metadata struct {
	Title        string            // Catalog title
	Version      string            // Catalog version
	Published    int64             // Publication timestamp
	LastModified int64             // Last modification timestamp
	Properties   map[string]string // Additional properties
}

// Group represents a control group.
type Group struct {
	ID       string    // Group identifier
	Title    string    // Group title
	Controls []Control // Controls in this group
	Groups   []Group   // Nested groups
}

// Control represents a security or privacy control.
type Control struct {
	ID         string            // Control identifier (e.g., "AC-1")
	Title      string            // Control title
	Parameters []Parameter       // Control parameters
	Parts      []Part            // Control statement parts
	Properties map[string]string // Additional properties
}

// Parameter represents a control parameter.
type Parameter struct {
	ID      string   // Parameter identifier
	Label   string   // Parameter label
	Values  []string // Allowed values
	Default string   // Default value
}

// Part represents a control statement part.
type Part struct {
	ID    string // Part identifier
	Name  string // Part name (e.g., "statement", "guidance")
	Prose string // Part text content
	Parts []Part // Nested parts
}
