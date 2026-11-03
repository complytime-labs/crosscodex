package artifacts_test

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/complytime-labs/crosscodex/internal/analyzer/artifacts"
)

func FuzzParseSameAsResponse(f *testing.F) {
	for _, seed := range []string{
		"SAME: YES\nJUSTIFICATION: Same document.\nCONFIDENCE: HIGH", // valid
		"same: no.\r\nconfidence: medium",                            // valid: case, CRLF, period
		"SAME: YES or NO",                                            // invalid: echoed template line
		"",                                                           // boundary
		"JUSTIFICATION: x\nSAME: NO\nSAME: YES",                      // the first decision wins
		"SAME: YES\nJUSTIFICATION: \x00\xff</name>\nCONFIDENCE: \u202eHIGH", // attack
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		v := artifacts.ParseSameAsResponse("voter", raw)
		if v.VoterID != "voter" || v.RawResponse != raw {
			t.Fatalf("vote for %q does not carry its voter and reply: %+v", raw, v)
		}
		if v.Decision == nil {
			return // an errored vote; consensus reads nothing else from it
		}
		if !strings.Contains(strings.ToUpper(raw), "SAME:") {
			t.Fatalf("decision from %q, which has no SAME: line", raw)
		}
		if v.Confidence == "" || strings.ContainsAny(v.Confidence, " \t\n\f\r") || v.Confidence != strings.ToUpper(v.Confidence) {
			t.Fatalf("confidence %q from %q is not one upper-case token", v.Confidence, raw)
		}
		if v.Justification != strings.TrimSpace(v.Justification) || strings.Contains(v.Justification, "\n") {
			t.Fatalf("justification %q from %q is not one trimmed line", v.Justification, raw)
		}
	})
}

func FuzzValidateSearchName(f *testing.F) {
	for _, s := range []string{
		"access policy", // valid
		" \t ",          // boundary: blank
		strings.Repeat("a", artifacts.MaxSearchNameBytes+1), // boundary: past the cap
		"a\x1b[2J", // attack: terminal escape
		"a\u202eb", // attack: bidi override
		"\xff",     // invalid UTF-8
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		if artifacts.ValidateSearchName(name) != nil {
			return
		}
		if !utf8.ValidString(name) || len(name) > artifacts.MaxSearchNameBytes || strings.TrimSpace(name) == "" {
			t.Fatalf("accepted %q", name)
		}
		for _, r := range name {
			if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp) {
				t.Fatalf("accepted %q with %U", name, r)
			}
		}
	})
}

// FuzzEscapeLike checks that escapeLike's output, used as an ILIKE pattern
// with ESCAPE '\', has no live wildcard: every \, % and _ in it is the second
// byte of a \-escape, and unescaping it gives back the input.
func FuzzEscapeLike(f *testing.F) {
	for _, s := range []string{
		"access policy",          // valid
		"",                       // boundary: empty
		"100% a_b",               // wildcards
		`\`,                      // boundary: lone escape
		`a\%b\_c\\`,              // attack: pre-escaped text
		"%%__\\\\",               // attack: runs of every special byte
		"\xff%\x00_\u00fc\u202e", // invalid UTF-8, NUL and non-ASCII around wildcards
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		got := artifacts.ExportEscapeLike(raw)
		var unescaped strings.Builder
		for i := 0; i < len(got); i++ {
			switch got[i] {
			case '\\':
				if i+1 == len(got) || !strings.ContainsRune(`\%_`, rune(got[i+1])) {
					t.Fatalf("escapeLike(%q) = %q has a dangling or stray escape at byte %d", raw, got, i)
				}
				i++
				unescaped.WriteByte(got[i])
			case '%', '_':
				t.Fatalf("escapeLike(%q) = %q has an unescaped wildcard at byte %d", raw, got, i)
			default:
				unescaped.WriteByte(got[i])
			}
		}
		if unescaped.String() != raw {
			t.Fatalf("escapeLike(%q) = %q unescapes to %q", raw, got, unescaped.String())
		}
	})
}

// FuzzValidateExpect checks that ValidateExpect accepts exactly the tokens a
// pair state can produce: 16 lowercase hex characters.
func FuzzValidateExpect(f *testing.F) {
	for _, s := range []string{"0123456789abcdef", "", "0123456789ABCDEF", "0123456789abcde", "0123456789abcdef0",
		"\x1b[31m0123456789", "0123456789abcde\u202e"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, token string) {
		if artifacts.ValidateExpect(token) != nil {
			return
		}
		if len(token) != 16 || strings.Trim(token, "0123456789abcdef") != "" {
			t.Fatalf("accepted %q", token)
		}
	})
}
