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
	reSanityCheck   = regexp.MustCompile(`(?i)SANITY_CHECK:[ \t]*(\S+)`)
)

// ParseResponse extracts a consensus.Vote from a raw LLM response matching
// the requires prompt's fixed output format (pkg/prompt/defaults/requires.yaml).
// A response with no recognizable "REQUIRES: YES|NO" line, or no
// "SANITY_CHECK: PASS|FAIL" line, produces a vote with a nil Decision, which
// consensus.Computer.Compute treats as an invalid/errored vote for error-rate
// purposes. SANITY_CHECK: FAIL forces the decision to NO and keeps the
// justification.
func ParseResponse(voterID, raw string) consensus.Vote {
	vote := consensus.Vote{VoterID: voterID, RawResponse: raw, Confidence: "LOW"}

	m := reRequires.FindStringSubmatch(raw)
	sanity := reSanityCheck.FindStringSubmatch(raw)
	if m == nil || sanity == nil {
		return vote
	}

	var decision bool
	switch strings.ToUpper(m[1]) {
	case "YES":
		decision = true
	case "NO":
	default:
		return vote
	}
	switch strings.ToUpper(sanity[1]) {
	case "PASS":
	case "FAIL":
		decision = false
	default:
		return vote
	}
	vote.Decision = &decision

	if jm := reJustification.FindStringSubmatch(raw); jm != nil {
		vote.Justification = strings.TrimSpace(jm[1])
	}
	if cm := reConfidence.FindStringSubmatch(raw); cm != nil {
		vote.Confidence = strings.ToUpper(cm[1])
	}
	return vote
}
