// Package results holds the JSON shapes analyzers persist into
// analysis_results.result_data and the graph subscriber reads back out.
// It exists to avoid an internal/graph <-> analyzer import cycle: both
// producers (analyzers) and the consumer (graph subscriber) depend on
// this neutral package instead of on each other.
package results

// RequiresAnalyzer and RelationshipAnalyzer are the names of the requires and
// relationship analyzers: the analyzer_name of that analyzer's
// analysis_results row and of the vote_summaries rows of its pairs.
const (
	RequiresAnalyzer     = "requires"
	RelationshipAnalyzer = "relationship"
)

// SemanticMatchContributionTypeKey and SemanticMatchUnanimousKey are
// SemanticMatchResult.Properties keys. The relationship analyzer writes them;
// the pipeline reads them to build synthesis inputs. The contribution type is
// the CONTRIBUTES_TO sub-classification ("INTEGRAL_TO", "EXAMPLE_OF", or ""
// when there is none); unanimity is strconv.FormatBool output.
const (
	SemanticMatchContributionTypeKey = "contribution_type"
	SemanticMatchUnanimousKey        = "unanimous"
)

// SemanticMatchEdgeLabel is the graph edge label for relationship analyzer
// results. The graph subscriber writes it; readers filter on it.
const SemanticMatchEdgeLabel = "SEMANTIC_MATCH"

// SemanticMatchEdgeIDKind is the first graphdb.DerivedID part of a
// SEMANTIC_MATCH edge ID: DerivedID(SemanticMatchEdgeIDKind, jobID, src, tgt).
// The mappings API derives mapping_id the same way, so a mapping and its edge
// share an ID.
const SemanticMatchEdgeIDKind = "semantic-match"

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
