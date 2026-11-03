package analyzer

import (
	"math"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// MaxPromptValueBytes caps a short untrusted value (a name, ID, type,
// level or ancestor title) rendered into a prompt with PromptValue, before
// escaping.
const MaxPromptValueBytes = 200

var (
	// promptBreaks matches runs of control characters (newlines and tabs
	// included) and Unicode separators (spaces, line and paragraph
	// separators).
	promptBreaks = regexp.MustCompile(`[\p{Cc}\p{Z}]+`)
	// promptInvisibles matches Unicode format characters (zero-width
	// characters, bidi overrides, tag characters). They render as nothing, so
	// removing them, rather than turning them into a space, leaves the text a
	// human reviewer sees; tag characters can otherwise smuggle hidden text
	// to the model.
	promptInvisibles = regexp.MustCompile(`\p{Cf}+`)
	// promptEscaper entity-encodes the characters that could open or close a
	// prompt tag.
	promptEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
)

// CleanText makes s storable in PostgreSQL text and jsonb, which reject NUL
// and invalid UTF-8, and cuts it to at most n bytes on a rune boundary.
func CleanText(s string, n int) string {
	s = strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", ""), "�")
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// CleanDisplay makes untrusted text safe to show as one line, in a prompt
// or a terminal: it repairs invalid UTF-8, removes Unicode format
// characters, collapses control and separator runs to one space, caps the
// result at maxBytes on a rune boundary and trims it. maxBytes must be >= 0;
// callers pass constants, so it is not clamped.
func CleanDisplay(s string, maxBytes int) string {
	// Repair invalid UTF-8 first: stripping an invisible rune from between the
	// bytes of a split sequence would otherwise splice them into a new rune such as U+202E.
	s = strings.ToValidUTF8(s, "\uFFFD")
	s = promptBreaks.ReplaceAllString(promptInvisibles.ReplaceAllString(s, ""), " ")
	return strings.TrimSpace(CleanText(s, maxBytes))
}

// PromptValue makes an untrusted short value safe to render inside a prompt
// tag: promptClean (CleanDisplay plus NFKC), cut so the entity-escaped result is at most maxBytes
// bytes, trimmed, then escaped so no character can close the tag. The cut
// falls between source characters, so it never splits an entity or a rune.
func PromptValue(s string, maxBytes int) string {
	return escapeWithin(promptClean(s), maxBytes, utf8.RuneLen)
}

// PromptText is PromptValue for long untrusted text, such as a control
// statement, whose bound is configured in characters (the pair analyzers'
// max_source_chars / max_target_chars): the entity-escaped result is at most
// maxChars runes (0 or less disables the bound). Cleaning comes first, so
// invisible characters do not use up the bound.
func PromptText(s string, maxChars int) string {
	if maxChars <= 0 {
		maxChars = math.MaxInt
	}
	return escapeWithin(promptClean(s), maxChars, func(rune) int { return 1 })
}

// promptClean is CleanDisplay, uncapped, with NFKC normalization between
// the UTF-8 repair and the format-character removal: compatibility
// look-alikes such as fullwidth U+FF1C/U+FF1E and small-form U+FE64/U+FE65
// fold to ASCII < and >, which escapeWithin then escapes. NFKC can lengthen
// a string, so callers cap after it. CleanDisplay itself does not normalize:
// it shows stored values to an operator, who should see them as stored.
func promptClean(s string) string {
	return CleanDisplay(norm.NFKC.String(strings.ToValidUTF8(s, "\uFFFD")), math.MaxInt)
}

// escapeWithin cuts s at the longest prefix whose entity-escaped size is at
// most limit, where size sums width over the runes left unescaped and the
// entity length for &, < and >; it then trims and escapes the prefix.
func escapeWithin(s string, limit int, width func(rune) int) string {
	n := 0
	for i, r := range s {
		w := width(r)
		switch r {
		case '&':
			w = len("&amp;")
		case '<', '>':
			w = len("&lt;")
		}
		if n+w > limit {
			s = s[:i]
			break
		}
		n += w
	}
	return promptEscaper.Replace(strings.TrimSpace(s))
}
