package requires

import (
	"regexp"
	"strings"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/consensus"
)

var (
	reRequires      = regexp.MustCompile(`(?i)REQUIRES:\s*(\S+)`)
	reJustification = regexp.MustCompile(`(?i)JUSTIFICATION:\s*(.+?)(?:\n|$)`)
	reConfidence    = regexp.MustCompile(`(?i)CONFIDENCE:\s*(\S+)`)
)

// ParseResponse extracts a consensus.Vote from a raw LLM response matching
// the requires prompt's fixed output format (pkg/prompt/defaults/requires.yaml).
// A response with no recognizable "REQUIRES: YES|NO" line produces a vote
// with a nil Decision, which consensus.Computer.Compute treats as an
// invalid/errored vote for error-rate purposes.
func ParseResponse(voterID, raw string) consensus.Vote {
	vote := consensus.Vote{VoterID: voterID, RawResponse: raw, Confidence: "LOW"}

	m := reRequires.FindStringSubmatch(raw)
	if m == nil {
		return vote
	}
	switch strings.ToUpper(m[1]) {
	case "YES":
		v := true
		vote.Decision = &v
	case "NO":
		v := false
		vote.Decision = &v
	default:
		return vote
	}

	if jm := reJustification.FindStringSubmatch(raw); jm != nil {
		vote.Justification = strings.TrimSpace(jm[1])
	}
	if cm := reConfidence.FindStringSubmatch(raw); cm != nil {
		vote.Confidence = strings.ToUpper(cm[1])
	}
	return vote
}
