// Package results holds the JSON shapes analyzers persist into
// analysis_results.result_data and the graph subscriber reads back out.
// It exists to avoid an internal/graph <-> analyzer import cycle: both
// producers (analyzers) and the consumer (graph subscriber) depend on
// this neutral package instead of on each other.
package results

// RequiresResult is one consensus-computed prerequisite pair from the
// requires analyzer.
type RequiresResult struct {
	SourceID   string   `json:"source_id"`
	TargetID   string   `json:"target_id"`
	Confidence float64  `json:"confidence"`
	Unanimous  bool     `json:"unanimous"`
	ValidVotes int      `json:"valid_votes"`
	TotalVotes int      `json:"total_votes"`
	Models     []string `json:"models"`
}

// SemanticMatchResult is one consensus-computed relationship pair from
// the relationship analyzer.
type SemanticMatchResult struct {
	SourceID         string            `json:"source_id"`
	TargetID         string            `json:"target_id"`
	RelationshipType string            `json:"relationship_type"`
	Confidence       float64           `json:"confidence"`
	Properties       map[string]string `json:"properties"`
}

// Artifact is one consensus artifact extracted for a control.
type Artifact struct {
	Name       string  `json:"name"`
	Type       string  `json:"type"`
	Frequency  string  `json:"frequency"`
	OwnerRole  string  `json:"owner_role"`
	Confidence float64 `json:"confidence"`
}

// ArtifactResult groups a control's consensus artifacts from the
// artifacts analyzer.
type ArtifactResult struct {
	ControlID string     `json:"control_id"`
	Artifacts []Artifact `json:"artifacts"`
}

// ClassifyResult is one control's type/level classification.
type ClassifyResult struct {
	ControlID string `json:"control_id"`
	Type      string `json:"type"`
	Level     string `json:"level"`
}

// EmbedResult records that a control was embedded. Kept minimal: graph
// materialization for embeddings is a no-op (vectors live in vectordb,
// not the graph), so this blob exists only for readback consistency.
type EmbedResult struct {
	ControlID string `json:"control_id"`
}
