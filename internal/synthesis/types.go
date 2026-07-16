package synthesis

// SynthesisRow represents a ranked control pair with viability weight.
// 16 fields matching Python synthesis_stats columns exactly.
type SynthesisRow struct {
	JobID                 string
	SourceID              string
	TargetID              string
	SimilarityMean        float64
	SimilarityMedian      float64
	SimilarityVar         float64 // variance of similarity scores across samples
	SimilarityCount       int     // number of similarity samples aggregated
	SourceType            string
	TargetType            string
	SourceLevel           string
	TargetLevel           string
	ConsensusRelationship string
	ConfidenceFraction    float64
	Unanimous             bool // true when all panel votes agreed on ConsensusRelationship
	ContributionType      string
	ViabilityWeight       float64 // final weighted viability score in [0, score], see ComputeViabilityWeight
}

// Classification holds type and level labels for a control.
// Defaults deliberately diverge from classify package ("Unknown"/"Tactical"
// vs "None"/"None") for Python parity.
type Classification struct {
	Type  string
	Level string
}

// GetType returns the classification type, defaulting to "Unknown" for empty strings.
func (c Classification) GetType() string {
	if c.Type == "" {
		return "Unknown"
	}
	return c.Type
}

// GetLevel returns the classification level, defaulting to "Tactical" for empty strings.
func (c Classification) GetLevel() string {
	if c.Level == "" {
		return "Tactical"
	}
	return c.Level
}

// SynthesisInput is constructed by the pipeline orchestrator from upstream
// PairResult and embedding data. The pipeline service assembles this from
// relationship analyzer PairResult messages and embedding similarity matrices.
type SynthesisInput struct {
	SourceID              string
	TargetID              string
	SimilarityScore       float64
	SimilarityMedian      float64
	SimilarityVar         float64
	SimilarityCount       int
	ConsensusRelationship string
	ContributionType      string
	ConfidenceFraction    float64
	Unanimous             bool
}

// DiagnosticSeverity indicates the severity of a quality diagnostic.
type DiagnosticSeverity int

const (
	SeverityGood     DiagnosticSeverity = iota // 0
	SeverityWarn                               // 1
	SeverityPoor                               // 2
	SeverityCritical                           // 3
)

// String returns the severity name.
func (s DiagnosticSeverity) String() string {
	switch s {
	case SeverityGood:
		return "good"
	case SeverityWarn:
		return "warn"
	case SeverityPoor:
		return "poor"
	case SeverityCritical:
		return "critical"
	default:
		return "unknown"
	}
}

// Diagnostic is a single quality finding from the assessor.
type Diagnostic struct {
	Category string
	Severity DiagnosticSeverity
	Message  string
	Value    float64
}

// QualityReport aggregates quality metrics and diagnostics for a synthesis run.
type QualityReport struct {
	TotalPairs         int
	ViablePairs        int     // count of pairs with ViabilityWeight > 0
	AvgConfidence      float64 // mean ConfidenceFraction across all pairs, in [0, 1]
	AvgViability       float64 // mean ViabilityWeight across all pairs
	RelationshipCounts map[string]int
	Diagnostics        []Diagnostic
}

// ExecuteResult is the outcome of Service.Execute, containing ranked rows,
// quality diagnostics, and a deterministic SHA-256 content hash of the report.
type ExecuteResult struct {
	Rows        []SynthesisRow
	Report      *QualityReport
	ContentHash string
}
