package artifacts

import (
	"fmt"
	"strings"
)

// ArtifactType classifies extracted artifacts into a 9-type taxonomy.
// Iota order matches enum priority for tiebreaking in consensus.
type ArtifactType int

const (
	ArtifactPolicy        ArtifactType = iota // Governance document
	ArtifactProcedure                         // Step-by-step instructions
	ArtifactPlan                              // Forward-looking scope/schedule
	ArtifactReport                            // Periodic findings/status
	ArtifactRecord                            // Evidence of event/action
	ArtifactConfiguration                     // Technical setting, verifiable state
	ArtifactMechanism                         // Technical capability/control
	ArtifactRole                              // Organizational position/responsibility
	ArtifactProcess                           // Recurring activity with frequency/trigger
)

var artifactTypeNames = [...]string{
	ArtifactPolicy:        "POLICY",
	ArtifactProcedure:     "PROCEDURE",
	ArtifactPlan:          "PLAN",
	ArtifactReport:        "REPORT",
	ArtifactRecord:        "RECORD",
	ArtifactConfiguration: "CONFIGURATION",
	ArtifactMechanism:     "MECHANISM",
	ArtifactRole:          "ROLE",
	ArtifactProcess:       "PROCESS",
}

// String returns the canonical SCREAMING_SNAKE_CASE name.
func (a ArtifactType) String() string {
	if a >= 0 && int(a) < len(artifactTypeNames) {
		return artifactTypeNames[a]
	}
	return fmt.Sprintf("UNKNOWN(%d)", int(a))
}

// Valid reports whether a is a defined ArtifactType value.
func (a ArtifactType) Valid() bool {
	return a >= ArtifactPolicy && a <= ArtifactProcess
}

// AllArtifactTypes returns every defined type in priority order.
func AllArtifactTypes() []ArtifactType {
	return []ArtifactType{
		ArtifactPolicy, ArtifactProcedure, ArtifactPlan, ArtifactReport,
		ArtifactRecord, ArtifactConfiguration, ArtifactMechanism,
		ArtifactRole, ArtifactProcess,
	}
}

// ParseArtifactType converts a string to an ArtifactType.
// Returns ok=false if the string is not a valid type name.
func ParseArtifactType(s string) (ArtifactType, bool) {
	for i, name := range artifactTypeNames {
		if name == s {
			return ArtifactType(i), true
		}
	}
	return 0, false
}

// TitleCase returns the title-cased form (e.g., "Policy") for graph node names.
func (a ArtifactType) TitleCase() string {
	if !a.Valid() {
		return fmt.Sprintf("Unknown(%d)", int(a))
	}
	name := artifactTypeNames[a]
	return string(name[0]) + strings.ToLower(name[1:])
}

// ParseStatus indicates the outcome of parsing an LLM response.
type ParseStatus int

const (
	ParseOK   ParseStatus = iota // Successfully extracted artifacts
	ParseFail                    // Regex matched no valid artifacts
	ParseNone                    // LLM returned ARTIFACTS: NONE (valid response)
)

var parseStatusNames = [...]string{
	ParseOK:   "OK",
	ParseFail: "PARSE_FAIL",
	ParseNone: "ARTIFACTS_NONE",
}

// String returns the canonical string.
func (p ParseStatus) String() string {
	if p >= 0 && int(p) < len(parseStatusNames) {
		return parseStatusNames[p]
	}
	return fmt.Sprintf("UNKNOWN(%d)", int(p))
}

// ExtractedArtifact is what the parser produces from one LLM response block.
type ExtractedArtifact struct {
	Name        string            `json:"name"`
	Type        ArtifactType      `json:"type"`
	Frequency   string            `json:"frequency"`
	OwnerRole   string            `json:"owner_role"`
	Description string            `json:"description"`
	Properties  map[string]string `json:"properties,omitempty"`
}

// Vote captures a single LLM response for a control.
type Vote struct {
	VoteKey     string              `json:"vote_key"`
	Model       string              `json:"model"`
	SampleIndex int                 `json:"sample_index"`
	Artifacts   []ExtractedArtifact `json:"artifacts"`
	RawResponse string              `json:"raw_response"`
	ParseStatus ParseStatus         `json:"parse_status"`
	DurationMS  int64               `json:"duration_ms"`
}

// ConsensusArtifact is the result after fuzzy grouping across model votes.
type ConsensusArtifact struct {
	Name        string            `json:"name"`
	Type        ArtifactType      `json:"type"`
	Frequency   string            `json:"frequency"`
	OwnerRole   string            `json:"owner_role"`
	Description string            `json:"description"`
	Confidence  float64           `json:"confidence"`
	VoterKeys   []string          `json:"voter_keys"`
	VoteCount   int               `json:"vote_count"`
	Unanimous   bool              `json:"unanimous"`
	Properties  map[string]string `json:"properties,omitempty"`
}

// ControlResult holds all votes and the consensus for a single control.
type ControlResult struct {
	ControlID string              `json:"control_id"`
	Votes     map[string]*Vote    `json:"votes"`
	Artifacts []ConsensusArtifact `json:"artifacts"`
}
