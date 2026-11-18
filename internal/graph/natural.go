package graph

import (
	"cmp"
	"strings"
)

// naturalCompare orders identifiers so that digit runs compare by numeric
// value (ac-2 before ac-10) and everything else bytewise. Digit runs are
// compared as strings, never parsed, so runs longer than any integer type
// compare correctly. On a numeric tie the run with fewer leading zeros comes
// first. Strings equal by these rules fall back to a plain comparison.
func naturalCompare(a, b string) int {
	restA, restB := a, b
	for restA != "" && restB != "" {
		var runA, runB string
		runA, restA = splitRun(restA)
		runB, restB = splitRun(restB)
		if c := compareRuns(runA, runB); c != 0 {
			return c
		}
	}
	return cmp.Or(cmp.Compare(len(restA), len(restB)), cmp.Compare(a, b))
}

func isDigit(c byte) bool { return '0' <= c && c <= '9' }

// splitRun cuts the leading run of digits, or of non-digits, off s.
func splitRun(s string) (run, rest string) {
	digits := isDigit(s[0])
	i := 1
	for i < len(s) && isDigit(s[i]) == digits {
		i++
	}
	return s[:i], s[i:]
}

// compareRuns compares two runs; two digit runs compare numerically, any
// other pair bytewise.
func compareRuns(a, b string) int {
	if !isDigit(a[0]) || !isDigit(b[0]) {
		return strings.Compare(a, b)
	}
	sigA, sigB := strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
	return cmp.Or(cmp.Compare(len(sigA), len(sigB)), strings.Compare(sigA, sigB), cmp.Compare(len(a), len(b)))
}
