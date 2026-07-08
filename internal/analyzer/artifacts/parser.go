package artifacts

import (
	"regexp"
	"strings"
)

var (
	reNoneSentinel = regexp.MustCompile(`(?i)ARTIFACTS:\s*NONE`)
	reBlockSplit   = regexp.MustCompile(`\n\s*---\s*\n?`)
	reArtName      = regexp.MustCompile(`(?i)ARTIFACT_NAME:\s*(.+?)(?:\n|$)`)
	reArtType      = regexp.MustCompile(`(?i)ARTIFACT_TYPE:\s*(\S+)`)
	reFrequency    = regexp.MustCompile(`(?i)FREQUENCY:\s*(.+?)(?:\n|$)`)
	reOwnerRole    = regexp.MustCompile(`(?i)OWNER_ROLE:\s*(.+?)(?:\n|$)`)
	reDescription  = regexp.MustCompile(`(?i)DESCRIPTION:\s*(.+?)(?:\n|$)`)
	reGenericField = regexp.MustCompile(`(?m)^([A-Z][A-Z_]+):\s*(.+?)$`)
)

var reservedFields = map[string]bool{
	"ARTIFACT_NAME": true,
	"ARTIFACT_TYPE": true,
	"FREQUENCY":     true,
	"OWNER_ROLE":    true,
	"DESCRIPTION":   true,
	"ARTIFACTS":     true,
}

// ParseResponse extracts structured artifacts from an LLM response.
// Returns the list of extracted artifacts and a parse status.
//
// Valid artifact blocks always take priority over the ARTIFACTS: NONE sentinel.
// This prevents false NONE classification when the sentinel appears in
// preamble text preceding valid blocks.
func ParseResponse(raw string) ([]ExtractedArtifact, ParseStatus) {
	if strings.TrimSpace(raw) == "" {
		return nil, ParseFail
	}

	blocks := reBlockSplit.Split(raw, -1)
	var result []ExtractedArtifact

	for _, block := range blocks {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}

		art, ok := parseBlock(block)
		if ok {
			result = append(result, art)
		}
	}

	if len(result) > 0 {
		return result, ParseOK
	}

	if reNoneSentinel.MatchString(raw) {
		return nil, ParseNone
	}

	return nil, ParseFail
}

func parseBlock(block string) (ExtractedArtifact, bool) {
	var art ExtractedArtifact

	if m := reArtName.FindStringSubmatch(block); m != nil {
		art.Name = strings.TrimSpace(m[1])
	}

	if m := reArtType.FindStringSubmatch(block); m != nil {
		typeStr := strings.ToUpper(m[1])
		at, ok := ParseArtifactType(typeStr)
		if !ok {
			return art, false
		}
		art.Type = at
	} else {
		return art, false
	}

	if art.Name == "" {
		return art, false
	}

	if m := reFrequency.FindStringSubmatch(block); m != nil {
		freq := strings.TrimSpace(m[1])
		if !strings.EqualFold(freq, "NONE") {
			art.Frequency = freq
		}
	}

	if m := reOwnerRole.FindStringSubmatch(block); m != nil {
		role := strings.TrimSpace(m[1])
		if !strings.EqualFold(role, "NONE") {
			art.OwnerRole = role
		}
	}

	if m := reDescription.FindStringSubmatch(block); m != nil {
		art.Description = strings.TrimSpace(m[1])
	}

	// Capture unknown fields into Properties.
	matches := reGenericField.FindAllStringSubmatch(block, -1)
	for _, m := range matches {
		key := strings.ToUpper(m[1])
		if reservedFields[key] {
			continue
		}
		if art.Properties == nil {
			art.Properties = make(map[string]string)
		}
		art.Properties[key] = strings.TrimSpace(m[2])
	}

	return art, true
}
