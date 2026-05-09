package analyzer

// AnalyzeRequest represents an analysis request.
type AnalyzeRequest struct {
	ArtifactURI string            // URI of artifact to analyze
	TenantID    string            // Tenant identifier
	Parameters  map[string]string // Analyzer-specific parameters
}

// AnalyzeResponse represents analysis results.
type AnalyzeResponse struct {
	Findings []Finding      // Discovered findings
	Metadata map[string]any // Analysis metadata
	Errors   []string       // Non-fatal errors encountered
}

// Finding represents a compliance-relevant observation.
type Finding struct {
	ID          string         // Unique finding identifier
	Type        string         // Finding type (e.g., "control-evidence", "violation")
	Severity    string         // Severity level
	Title       string         // Finding title
	Description string         // Detailed description
	Location    Location       // Source location
	Metadata    map[string]any // Additional metadata
}

// Location identifies where a finding was discovered.
type Location struct {
	File   string // File path
	Line   int    // Line number
	Column int    // Column number
}
