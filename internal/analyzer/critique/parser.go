package critique

import (
	"regexp"
	"strings"

	intanalyzer "github.com/complytime-labs/crosscodex/internal/analyzer"
)

// Values of Critique.Verdict.
const (
	VerdictUphold = "UPHOLD"
	VerdictReject = "REJECT"
)

var (
	reVerdict = regexp.MustCompile(`(?im)^[ \t]*VERDICT:[ \t]*(\S*?)[ \t\r]*$`)
	reReason  = regexp.MustCompile(`(?im)^[ \t]*REASON:[ \t]*(.*)$`)
)

// Critique is one parsed critique vote. Verdict is VerdictUphold,
// VerdictReject, or "" for an invalid vote.
type Critique struct {
	Verdict string
	Reason  string
}

// Valid reports whether the vote counts: it has an UPHOLD or REJECT verdict.
func (c Critique) Valid() bool { return c.Verdict != "" }

// ParseCritique reads a response in the critique prompt's format
// (pkg/prompt/defaults/critique.yaml): a line "VERDICT: UPHOLD|REJECT" and a
// line "REASON: <text>". The value is matched case-insensitively after
// trimming. The vote is invalid (Verdict "") when any of these holds:
//   - the VERDICT line is missing;
//   - its value is anything else;
//   - two VERDICT lines disagree;
//   - the REASON line is missing or empty.
//
// An invalid vote keeps whatever reason it has. The reason is model output
// kept in the stored audit record, so it is cleaned with intanalyzer.CleanText
// (no NUL, valid UTF-8), cut to intanalyzer.MaxStoredJustificationBytes and
// trimmed. It is not escaped: escape it when rendering it into a prompt.
func ParseCritique(raw string) Critique {
	var c Critique
	if m := reReason.FindStringSubmatch(raw); m != nil {
		c.Reason = strings.TrimSpace(intanalyzer.CleanText(m[1], intanalyzer.MaxStoredJustificationBytes))
	}
	verdicts := reVerdict.FindAllStringSubmatch(raw, -1)
	if len(verdicts) == 0 || c.Reason == "" {
		return c
	}
	verdict := strings.ToUpper(verdicts[0][1])
	for _, v := range verdicts[1:] {
		if strings.ToUpper(v[1]) != verdict {
			return c
		}
	}
	switch verdict {
	case VerdictUphold, VerdictReject:
		c.Verdict = verdict
	}
	return c
}
