// Package results holds the JSON shapes analyzers persist into
// analysis_results.result_data and the graph subscriber reads back out.
// It exists to avoid an internal/graph <-> analyzer import cycle: both
// producers (analyzers) and the consumer (graph subscriber) depend on
// this neutral package instead of on each other.
package results

// RequiresAnalyzer and RelationshipAnalyzer are the names of the requires and
// relationship analyzers: the analyzer_name of that analyzer's
// analysis_results row and of the vote_summaries rows of its pairs.
// CritiqueAnalyzer is the critique analyzer's name and the analyzer_name of
// its analysis_results row, whose result_data is a JSON []CritiqueResult.
const (
	RequiresAnalyzer     = "requires"
	RelationshipAnalyzer = "relationship"
	CritiqueAnalyzer     = "critique"
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

// SemanticMatchSynthesizedIDKind is the first graphdb.DerivedID part of a
// synthesized SEMANTIC_MATCH edge ID: DerivedID(SemanticMatchSynthesizedIDKind,
// jobID, src, tgt). It differs from SemanticMatchEdgeIDKind so a pair's
// analyzer edge and synthesized edge in one job do not collide.
const SemanticMatchSynthesizedIDKind = "semantic-match-synthesized"

// StageProperty is the SEMANTIC_MATCH edge property naming the pipeline stage
// that produced the edge: StageAnalyzer for the relationship analyzer's raw
// result, StageSynthesized for the synthesis stage's consensus. Readers treat
// an edge without the property as StageAnalyzer.
const (
	StageProperty    = "stage"
	StageAnalyzer    = "analyzer"
	StageSynthesized = "synthesized"
)

// SynthesisStage is the pipeline stage name of synthesis. Its stage event
// carries no rows; the graph subscriber's PG resolver serves them from
// vote_summaries as a JSON []SynthesizedMapping.
const SynthesisStage = "synthesis"

// SynthesizedMapping is one synthesized relationship of a job, read from a
// vote_summaries row of the relationship analyzer. RelationshipType is the
// row's consensus; IsViable is Viability > 0. Critique is the pair's
// relationship critique, nil when the job ran no critique or the critique has
// no result for the pair.
type SynthesizedMapping struct {
	SourceID         string          `json:"source_id"`
	TargetID         string          `json:"target_id"`
	RelationshipType string          `json:"relationship_type"`
	Confidence       float64         `json:"confidence"`
	Viability        float64         `json:"viability"`
	IsViable         bool            `json:"is_viable"`
	Critique         *CritiqueResult `json:"critique,omitempty"`
}

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
	// Justifications are the panel's reasons for the decision: the
	// justification of every valid YES vote, in panel order (models as
	// configured in analysis.requires.models, then sample number). They are model output: store them as data, and render them
	// into a prompt only through the analyzer prompt hardening. Absent in
	// results stored before they were recorded.
	Justifications []string `json:"justifications,omitempty"`
}

// SemanticMatchResult is one consensus-computed relationship pair from
// the relationship analyzer.
type SemanticMatchResult struct {
	SourceID         string            `json:"source_id"`
	TargetID         string            `json:"target_id"`
	RelationshipType string            `json:"relationship_type"`
	Confidence       float64           `json:"confidence"`
	Properties       map[string]string `json:"properties"`
	// Justifications are the panel's reasons for the decision: the
	// justification of every valid vote for RelationshipType, in panel order
	// (models as configured in analysis.relationship.models, then sample
	// number). They are model output: store them as data,
	// and render them into a prompt only through the analyzer prompt
	// hardening. Absent in results stored before they were recorded.
	Justifications []string `json:"justifications,omitempty"`
}

// Values of CritiqueResult.Outcome.
const (
	// CritiqueUpheld: the verdict is kept and no critic rejected it.
	CritiqueUpheld = "upheld"
	// CritiqueSplit: the verdict is kept, but at least one critic rejected it.
	CritiqueSplit = "split"
	// CritiqueVetoed: enough critics rejected the verdict to drop it
	// (analysis.critique.veto on).
	CritiqueVetoed = "vetoed"
	// CritiqueRejected: enough critics rejected the verdict to veto it, but
	// analysis.critique.veto was off, so the verdict is kept and flagged.
	CritiqueRejected = "rejected"
	// CritiqueFailed: the critique failed (no valid vote, or failed votes
	// over analysis.critique.max_error_rate), so the verdict is kept unreviewed.
	CritiqueFailed = "failed"
)

// CritiqueResult is the critique panel's review of one proposed verdict: a
// relationship pair (Kind "relationship") or a REQUIRES pair (Kind
// "requires"). Upheld, Rejected and Failed count the panel's votes (Failed:
// task errors, responses without a valid VERDICT line and votes with no
// result), so they sum to the panel size. Outcome says what the critique
// decided (CritiqueUpheld, CritiqueSplit, CritiqueRejected, CritiqueVetoed
// or CritiqueFailed) without the run's config. Rejected / (Upheld + Rejected)
// reaching analysis.critique.consensus_threshold, with at least one valid
// vote and the failures within analysis.critique.max_error_rate, is
// CritiqueVetoed when analysis.critique.veto was on and CritiqueRejected
// (kept, flagged) when it was off. Vetoed is Outcome == CritiqueVetoed. Reasons holds one entry per panel vote,
// in panel order, kept or vetoed, so the result is the audit record of the
// decision: "<model> s<N> <UPHOLD|REJECT|FAILED>: <reason>", which Note reads.
// Each entry's reason is model output stored as data (cleaned with
// CleanText, not escaped): escape it when rendering it into a prompt or page.
type CritiqueResult struct {
	Kind            string   `json:"kind"`
	SourceControlID string   `json:"source_control_id"`
	TargetControlID string   `json:"target_control_id"`
	Upheld          int      `json:"upheld"`
	Rejected        int      `json:"rejected"`
	Failed          int      `json:"failed"`
	Outcome         string   `json:"outcome"`
	Vetoed          bool     `json:"vetoed"`
	Reasons         []string `json:"reasons"`
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
