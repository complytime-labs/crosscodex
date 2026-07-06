package candidate

// ControlData wraps a control with its classification and text.
type ControlData struct {
	ControlID string // Control identifier (e.g., "NIST-800-53-AC-1")
	Text      string // Control statement text
	Type      string // From classification: "Procedural", "Technical", "Administrative"
	Level     string // From classification: "Strategic", "Tactical", "Operational"
	Ancestor  string // Root control title for context
}

// GenerateRequest carries all data a generator might need.
type GenerateRequest struct {
	TenantID        string                  // Tenant identifier
	JobID           string                  // Analysis job identifier
	SourceControls  map[string]*ControlData // All source controls with metadata
	TargetControls  map[string]*ControlData // All target controls with metadata
	EmbeddingMatrix interface{}             // Optional: similarity matrix from embedding analyzer
	Parameters      map[string]interface{}  // Generator-specific config from YAML
}

// Candidate represents a candidate pair with scoring and provenance.
type Candidate struct {
	SourceID    string            // Source control ID
	TargetID    string            // Target control ID
	Score       float64           // [0.0, 1.0] confidence/similarity score
	Weight      float64           // [0.0, 1.0] from generator config
	GeneratorID string            // Which generator produced this
	Metadata    map[string]string // Generator-specific metadata
}
