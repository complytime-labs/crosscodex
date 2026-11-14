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
	// reSanityCheck matches only a line that starts with SANITY_CHECK:, so a
	// justification quoting the field cannot stand in for the real line.
	reSanityCheck = regexp.MustCompile(`(?im)^[ \t]*SANITY_CHECK:[ \t]*(\S+)`)
)

// ParseResponse extracts structured fields from an LLM response using regex.
// This is a direct port of Python _parse_response() with fail-closed defaults:
// missing CONFIDENCE defaults to LOW, missing CONTRIBUTION_TYPE defaults to
// ContribNone, and unrecognized RELATIONSHIP returns ParseFail. A missing or
// non-PASS/FAIL SANITY_CHECK also returns ParseFail; SANITY_CHECK: FAIL
// downgrades the verdict to NO_RELATIONSHIP and keeps the justification.
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

	sanity := reSanityCheck.FindStringSubmatch(raw)
	if sanity == nil {
		return vote
	}
	switch strings.ToUpper(sanity[1]) {
	case SanityPass, SanityFail:
		vote.SanityCheck = strings.ToUpper(sanity[1])
	default:
		return vote
	}

	vote.Relationship = relType
	if vote.SanityCheck == SanityFail {
		vote.Relationship = RelNoRelationship
	}
	vote.ParseStatus = ParseOK

	// Extract contribution type (optional); a FAIL vote is NO_RELATIONSHIP, so
	// any sub-classification the model gave is discarded.
	ctMatch := reContribType.FindStringSubmatch(raw)
	if ctMatch != nil && vote.SanityCheck != SanityFail {
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
