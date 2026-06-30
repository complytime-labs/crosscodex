package relationship

import (
	"regexp"
	"strings"
)

var (
	reRelationship  = regexp.MustCompile(`(?i)RELATIONSHIP:\s*(\S+)`)
	reContribType   = regexp.MustCompile(`(?i)CONTRIBUTION_TYPE:\s*(\S+)`)
	reJustification = regexp.MustCompile(`(?i)JUSTIFICATION:\s*(.+?)(?:\n|$)`)
	reConfidence    = regexp.MustCompile(`(?i)CONFIDENCE:\s*(\S+)`)
)

// ParseResponse extracts structured fields from an LLM response using regex.
// This is a direct port of Python _parse_response() with fail-closed defaults:
// missing CONFIDENCE defaults to LOW, missing CONTRIBUTION_TYPE defaults to
// ContribNone, and unrecognized RELATIONSHIP returns ParseFail.
func ParseResponse(raw string) *Vote {
	vote := &Vote{
		RawResponse:  raw,
		Relationship: RelNoRelationship,
		Confidence:   ConfidenceLow,
		ParseStatus:  ParseFail,
	}

	// Extract relationship type.
	relMatch := reRelationship.FindStringSubmatch(raw)
	if relMatch == nil {
		return vote
	}

	relStr := strings.ToUpper(relMatch[1])
	relType, ok := ParseRelationshipType(relStr)
	if !ok {
		return vote
	}

	vote.Relationship = relType
	vote.ParseStatus = ParseOK

	// Extract contribution type (optional).
	ctMatch := reContribType.FindStringSubmatch(raw)
	if ctMatch != nil {
		ctStr := strings.ToUpper(ctMatch[1])
		vote.ContributionType = ParseContributionType(ctStr)
	}

	// Extract justification (optional).
	justMatch := reJustification.FindStringSubmatch(raw)
	if justMatch != nil {
		vote.Justification = strings.TrimSpace(justMatch[1])
	}

	// Extract confidence (optional, defaults to LOW).
	confMatch := reConfidence.FindStringSubmatch(raw)
	if confMatch != nil {
		vote.Confidence = ParseConfidenceLevel(strings.ToUpper(confMatch[1]))
	}

	return vote
}
