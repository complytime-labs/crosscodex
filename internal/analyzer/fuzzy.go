package analyzer

import (
	"regexp"
	"strings"
)

// DefaultFuzzyThreshold is the minimum token-set overlap ratio for a match.
const DefaultFuzzyThreshold = 0.6

var (
	leadingArticleRe = regexp.MustCompile(`^(?i)(the|a|an)\s+`)
	whitespaceRe     = regexp.MustCompile(`\s+`)
)

// NormalizeArtifactName lowercases, strips trailing periods, removes leading
// articles (the/a/an), and collapses whitespace. Port of Python
// ArtifactExtractor._normalize_artifact_name.
func NormalizeArtifactName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.TrimRight(name, ".")
	name = leadingArticleRe.ReplaceAllString(name, "")
	name = whitespaceRe.ReplaceAllString(name, " ")
	name = strings.TrimSpace(name)
	// If what remains is just an article with no following text, return empty
	if name == "the" || name == "a" || name == "an" {
		return ""
	}
	return name
}

// ArtifactNamesMatch reports whether two normalized artifact names have
// sufficient token-set overlap. Overlap is computed as
// |intersection| / min(|setA|, |setB|) and compared against threshold.
// Port of Python ArtifactExtractor._names_match.
func ArtifactNamesMatch(a, b string, threshold float64) bool {
	if a == "" || b == "" {
		return false
	}
	tokensA := tokenSet(a)
	tokensB := tokenSet(b)
	if len(tokensA) == 0 || len(tokensB) == 0 {
		return false
	}

	overlap := 0
	for t := range tokensA {
		if tokensB[t] {
			overlap++
		}
	}

	minLen := len(tokensA)
	if len(tokensB) < minLen {
		minLen = len(tokensB)
	}

	return float64(overlap)/float64(minLen) >= threshold
}

func tokenSet(s string) map[string]bool {
	tokens := strings.Fields(s)
	set := make(map[string]bool, len(tokens))
	for _, t := range tokens {
		set[t] = true
	}
	return set
}
