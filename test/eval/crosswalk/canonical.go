package crosswalk

import (
	"strings"
	"unicode"
)

// Canonical reduces an identifier to a format-agnostic comparison form:
// lowercase, split into maximal letter runs and ASCII digit runs (every other
// character is a separator), digit runs stripped of leading zeros, joined with
// ".". It encodes no knowledge of any framework's identifier syntax. An
// identifier with no letters or digits canonicalises to "".
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
	for _, r := range strings.ToLower(id) {
		isDigit := r >= '0' && r <= '9'
		if !isDigit && !unicode.IsLetter(r) {
			flush()
			continue
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
