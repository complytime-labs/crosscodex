package relationship

import "fmt"

// RelationshipType represents NIST IR 8477 relationship classifications.
// Iota order matches priority: lower value = higher tiebreak priority.
type RelationshipType int

const (
	RelEquivalent RelationshipType = iota // Highest priority
	RelSupersetOf
	RelSubsetOf
	RelContributesTo
	RelComplements
	RelPartial
	RelConflictsWith
	RelNoRelationship // Lowest priority
)

// relationshipNames maps each RelationshipType to its canonical string.
var relationshipNames = [...]string{
	RelEquivalent:     "EQUIVALENT",
	RelSupersetOf:     "SUPERSET_OF",
	RelSubsetOf:       "SUBSET_OF",
	RelContributesTo:  "CONTRIBUTES_TO",
	RelComplements:    "COMPLEMENTS",
	RelPartial:        "PARTIAL",
	RelConflictsWith:  "CONFLICTS_WITH",
	RelNoRelationship: "NO_RELATIONSHIP",
}

// String returns the canonical SCREAMING_SNAKE_CASE name.
func (r RelationshipType) String() string {
	if r >= 0 && int(r) < len(relationshipNames) {
		return relationshipNames[r]
	}
	return fmt.Sprintf("UNKNOWN(%d)", int(r))
}

// Valid reports whether r is a defined RelationshipType value.
func (r RelationshipType) Valid() bool {
	return r >= RelEquivalent && r <= RelNoRelationship
}

// AllRelationshipTypes returns every defined type in priority order.
func AllRelationshipTypes() []RelationshipType {
	return []RelationshipType{
		RelEquivalent, RelSupersetOf, RelSubsetOf, RelContributesTo,
		RelComplements, RelPartial, RelConflictsWith, RelNoRelationship,
	}
}

// ParseRelationshipType converts a string to a RelationshipType.
// Returns ok=false if the string is not a valid type name.
func ParseRelationshipType(s string) (RelationshipType, bool) {
	for i, name := range relationshipNames {
		if name == s {
			return RelationshipType(i), true
		}
	}
	return 0, false
}

// RelationshipPriority returns the tiebreak order for consensus voting.
// Index = priority (lower wins). This is identical to AllRelationshipTypes()
// because iota declaration order IS priority order by design.
func RelationshipPriority() []RelationshipType {
	return AllRelationshipTypes()
}

// ContributionType sub-classifies CONTRIBUTES_TO relationships.
type ContributionType int

const (
	ContribNone       ContributionType = iota // Not CONTRIBUTES_TO, or unspecified
	ContribIntegralTo                         // Source cannot be satisfied without target
	ContribExampleOf                          // Target is one way among alternatives
)

// contributionNames maps each ContributionType to its canonical string.
var contributionNames = [...]string{
	ContribNone:       "",
	ContribIntegralTo: "INTEGRAL_TO",
	ContribExampleOf:  "EXAMPLE_OF",
}

// String returns the canonical string or empty for ContribNone.
func (c ContributionType) String() string {
	if c >= 0 && int(c) < len(contributionNames) {
		return contributionNames[c]
	}
	return fmt.Sprintf("UNKNOWN(%d)", int(c))
}

// Valid reports whether c is a defined ContributionType value.
func (c ContributionType) Valid() bool {
	return c >= ContribNone && c <= ContribExampleOf
}

// ParseContributionType converts a string to a ContributionType.
// Returns ContribNone for empty, "N/A", or unrecognized strings.
func ParseContributionType(s string) ContributionType {
	switch s {
	case "INTEGRAL_TO":
		return ContribIntegralTo
	case "EXAMPLE_OF":
		return ContribExampleOf
	default:
		return ContribNone
	}
}

// ConfidenceLevel categorizes LLM-reported confidence.
type ConfidenceLevel int

const (
	ConfidenceUnknown ConfidenceLevel = iota
	ConfidenceHigh
	ConfidenceMedium
	ConfidenceLow
)

// confidenceNames maps each ConfidenceLevel to its canonical string.
var confidenceNames = [...]string{
	ConfidenceUnknown: "",
	ConfidenceHigh:    "HIGH",
	ConfidenceMedium:  "MEDIUM",
	ConfidenceLow:     "LOW",
}

// String returns the canonical string.
func (c ConfidenceLevel) String() string {
	if c >= 0 && int(c) < len(confidenceNames) {
		return confidenceNames[c]
	}
	return fmt.Sprintf("UNKNOWN(%d)", int(c))
}

// Valid reports whether c is a defined ConfidenceLevel value.
func (c ConfidenceLevel) Valid() bool {
	return c >= ConfidenceUnknown && c <= ConfidenceLow
}

// ParseConfidenceLevel converts a string to a ConfidenceLevel.
// Returns ConfidenceLow for unrecognized strings (fail-closed).
func ParseConfidenceLevel(s string) ConfidenceLevel {
	switch s {
	case "HIGH":
		return ConfidenceHigh
	case "MEDIUM":
		return ConfidenceMedium
	case "LOW":
		return ConfidenceLow
	default:
		return ConfidenceLow
	}
}

// ParseStatus indicates whether an LLM response was successfully parsed.
type ParseStatus int

const (
	ParseOK    ParseStatus = iota // Successfully extracted all fields
	ParseFail                     // Regex matched no valid relationship
	ParseError                    // HTTP/connection error calling LLM
)

// parseStatusNames maps each ParseStatus to its canonical string.
var parseStatusNames = [...]string{
	ParseOK:    "OK",
	ParseFail:  "PARSE_FAIL",
	ParseError: "PARSE_ERROR",
}

// String returns the canonical string.
func (p ParseStatus) String() string {
	if p >= 0 && int(p) < len(parseStatusNames) {
		return parseStatusNames[p]
	}
	return fmt.Sprintf("UNKNOWN(%d)", int(p))
}

// Vote captures a single LLM response for a control pair.
type Vote struct {
	VoteKey          string           `json:"vote_key"`     // e.g. "qwen3:8b" or "qwen3:8b__s0"
	Model            string           `json:"model"`        // Base model name
	SampleIndex      int              `json:"sample_index"` // 0 for single-sample, 0..N-1 for multi
	Relationship     RelationshipType `json:"relationship"`
	ContributionType ContributionType `json:"contribution_type"`
	Justification    string           `json:"justification"`
	Confidence       ConfidenceLevel  `json:"confidence"`
	RawResponse      string           `json:"raw_response"`
	ParseStatus      ParseStatus      `json:"parse_status"`
	DurationMS       int64            `json:"duration_ms"`
}

// Consensus captures the aggregated outcome for a control pair.
type Consensus struct {
	Relationship       RelationshipType `json:"relationship"`
	ContributionType   ContributionType `json:"contribution_type"`
	ConfidenceFraction float64          `json:"confidence_fraction"` // winner_count / valid_votes, rounded to 3 decimals
	Unanimous          bool             `json:"unanimous"`
	AllVotes           []string         `json:"all_votes"` // All vote relationship names (including parse errors)
	ValidVoteCount     int              `json:"valid_vote_count"`
}

// PairResult holds all votes and the consensus for a single control pair.
type PairResult struct {
	SourceControlID string           `json:"source_control_id"`
	TargetControlID string           `json:"target_control_id"`
	Votes           map[string]*Vote `json:"votes"` // Keyed by VoteKey
	Consensus       Consensus        `json:"consensus"`
	SimilarityScore float32          `json:"similarity_score"` // From embedding similarity matrix
}
