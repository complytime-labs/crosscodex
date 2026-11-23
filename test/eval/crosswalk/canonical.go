package crosswalk

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Canonical reduces an identifier to a format-agnostic comparison form:
// NFKC-normalised and lowercase, split into maximal letter runs and digit
// runs (every other character is a separator), digit runs written with ASCII
// digits (every Unicode decimal digit maps to its numeric value, so "AC-２" and
// Arabic-Indic digits match "AC-2") and stripped of leading zeros, joined with
// ".". Invalid UTF-8 bytes are separators. It encodes no knowledge of any
// framework's identifier syntax. An identifier with no letters or digits
// canonicalises to "".
func Canonical(id string) string {
	var parts []string
	var run []rune
	runIsDigit := false
	flush := func() {
		if len(run) == 0 {
			return
		}
		s := string(run)
		if runIsDigit {
			if s = strings.TrimLeft(s, "0"); s == "" {
				s = "0"
			}
		}
		parts = append(parts, s)
		run = run[:0]
	}
	for _, r := range strings.ToLower(norm.NFKC.String(strings.ToValidUTF8(id, "\uFFFD"))) {
		isDigit := unicode.IsDigit(r)
		if !isDigit && !unicode.IsLetter(r) {
			flush()
			continue
		}
		if isDigit {
			r = '0' + digitValue(r)
		}
		if len(run) > 0 && isDigit != runIsDigit {
			flush()
		}
		runIsDigit = isDigit
		run = append(run, r)
	}
	flush()
	return strings.Join(parts, ".")
}

// digitValue is the numeric value of a Unicode decimal digit (category Nd).
// The standard library has no such lookup, but every Nd block is a whole
// number of contiguous 0-9 sequences, so a digit's value is its offset from
// the start of its (possibly merged) range table entry modulo 10.
func digitValue(r rune) rune {
	for _, rg := range unicode.Nd.R16 {
		if r >= rune(rg.Lo) && r <= rune(rg.Hi) {
			return (r - rune(rg.Lo)) % 10
		}
	}
	for _, rg := range unicode.Nd.R32 {
		if r >= rune(rg.Lo) && r <= rune(rg.Hi) {
			return (r - rune(rg.Lo)) % 10
		}
	}
	return 0
}
