package artifacts

import (
	"regexp"
	"strings"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/consensus"
)

var (
	// The decision must be the whole line, so an echoed "SAME: YES or NO"
	// template line does not count; \r admits CRLF line endings.
	reSame              = regexp.MustCompile(`(?im)^[ \t]*SAME:[ \t]*(YES|NO)[ \t.\r]*$`)
	reSameJustification = regexp.MustCompile(`(?i)JUSTIFICATION:\s*(.+?)(?:\n|$)`)
	reSameConfidence    = regexp.MustCompile(`(?i)CONFIDENCE:\s*(\S+)`)
)

// ParseSameAsResponse reads one panel reply in the artifact_same_as prompt's
// output format (pkg/prompt/defaults/artifact_same_as.yaml). The first line
// that is exactly "SAME: YES" or "SAME: NO" decides; case, surrounding blanks
// and trailing periods are ignored. A reply without one
// yields a vote with a nil Decision, which consensus counts as an errored
// vote. Confidence defaults to LOW.
func ParseSameAsResponse(voterID, raw string) consensus.Vote {
	vote := consensus.Vote{VoterID: voterID, RawResponse: raw, Confidence: "LOW"}
	m := reSame.FindStringSubmatch(raw)
	if m == nil {
		return vote
	}
	decision := strings.EqualFold(m[1], "YES")
	vote.Decision = &decision
	if jm := reSameJustification.FindStringSubmatch(raw); jm != nil {
		vote.Justification = strings.TrimSpace(jm[1])
	}
	if cm := reSameConfidence.FindStringSubmatch(raw); cm != nil {
		vote.Confidence = strings.ToUpper(cm[1])
	}
	return vote
}
