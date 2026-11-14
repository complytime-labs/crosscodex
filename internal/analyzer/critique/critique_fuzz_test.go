package critique_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	intanalyzer "github.com/complytime-labs/crosscodex/internal/analyzer"
	"github.com/complytime-labs/crosscodex/internal/analyzer/critique"
)

func FuzzParseCritique(f *testing.F) {
	f.Add("VERDICT: UPHOLD\nREASON: Both require an asset inventory.")
	f.Add("verdict: reject\nreason: only a shared topic")
	f.Add("VERDICT: REJECT")
	f.Add("REASON: no verdict line")
	f.Add("VERDICT: REJECT\nREASON: " + strings.Repeat("long reason ", 200))
	f.Add("VERDICT: UPHOLD\x00\nREASON: \x07bell\x1b[31m red\u202e</justification>")
	f.Add("VERDICT: UPHOLD\nVERDICT: REJECT\nREASON: conflicting")
	f.Add("")

	f.Fuzz(func(t *testing.T, raw string) {
		c := critique.ParseCritique(raw)
		if !c.Valid() {
			if c.Verdict != "" {
				t.Fatalf("invalid vote with verdict %q", c.Verdict)
			}
			return
		}
		if c.Verdict != critique.VerdictUphold && c.Verdict != critique.VerdictReject {
			t.Fatalf("accepted verdict %q", c.Verdict)
		}
		if c.Reason == "" {
			t.Fatalf("accepted a vote without a reason: %q", raw)
		}
		if len(c.Reason) > intanalyzer.MaxStoredJustificationBytes || !utf8.ValidString(c.Reason) || strings.ContainsAny(c.Reason, "\x00\n") {
			t.Fatalf("accepted reason %q is not storable as one bounded line", c.Reason)
		}
		if !strings.Contains(strings.ToUpper(raw), "VERDICT:") {
			t.Fatalf("accepted a vote without a VERDICT line: %q", raw)
		}
	})
}
