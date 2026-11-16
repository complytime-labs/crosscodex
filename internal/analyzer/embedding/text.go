package embedding

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Regex patterns for text cleaning. These match the Python
// OllamaCrosswalker's clean_for_embedding() function in oscal_utils.py.
var (
	// oscalTemplateRE matches OSCAL parameter insertion placeholders like
	// {{ insert: param, ac-1_prm_1 }}.
	oscalTemplateRE = regexp.MustCompile(`\{\{[^}]*\}\}`)

	// tableSeparatorRE matches a markdown table separator row through to the
	// end of its line. Publisher print artefacts are not stripped here: a
	// control's text may come from any catalog, and those patterns are
	// applied to document-derived text alone
	// (oscal.StructureOptions.PrintArtifactPatterns).
	tableSeparatorRE = regexp.MustCompile(`\|[-| ]+\|.*`)

	// multiNewlineRE matches runs of 3+ newlines for collapsing.
	multiNewlineRE = regexp.MustCompile(`\n{3,}`)

	// multiSpaceRE matches runs of 2+ spaces or tabs for collapsing.
	multiSpaceRE = regexp.MustCompile(`[ \t]{2,}`)
)

// contextSeparator joins the titled ancestors in a control's "context" part;
// contextCutMarker prefixes a context whose root-most segments were dropped
// (the same marker intanalyzer.PromptContext uses).
const (
	contextSeparator = " > "
	contextCutMarker = "\u2026 > "
)

// prepareText cleans OSCAL artifacts from each context segment, the
// title and the statement separately, composes "[<context>] <title>:
// <statement>" from the pieces left non-empty, and bounds the result to
// maxChars runes (0 or negative means unbounded). Cleaning the pieces apart
// keeps an artifact pattern that runs to the end of the line (a table
// separator such as "|---|") inside a title or context from erasing the
// statement. Over budget,
// context segments are dropped root-first (the remainder is prefixed with
// contextCutMarker) and then the context is dropped entirely before the text
// is truncated, so the title and statement survive as long as possible.
func prepareText(context, title, statement string, maxChars int) string {
	var segments []string
	if context != "" {
		for _, seg := range strings.Split(context, contextSeparator) {
			if seg = cleanForEmbedding(seg); seg != "" {
				segments = append(segments, seg)
			}
		}
	}
	title = cleanForEmbedding(title)
	statement = cleanForEmbedding(statement)
	for dropped := 0; dropped < len(segments); dropped++ {
		ctx := strings.Join(segments[dropped:], contextSeparator)
		if dropped > 0 {
			ctx = contextCutMarker + ctx
		}
		text := composeText(ctx, title, statement)
		if maxChars <= 0 || utf8.RuneCountInString(text) <= maxChars {
			return text
		}
	}

	text := composeText("", title, statement)
	if maxChars > 0 && utf8.RuneCountInString(text) > maxChars {
		text = string([]rune(text)[:maxChars])
	}
	return text
}

// composeText joins the non-empty pieces, which prepareText has cleaned.
func composeText(context, title, statement string) string {
	var text string
	if context != "" {
		text = "[" + context + "] "
	}
	if title != "" {
		text += title + ": "
	}
	return strings.TrimSpace(text + statement)
}

// cleanForEmbedding removes OSCAL template placeholders and markdown table
// separators from text. Ported from Python oscal_utils.clean_for_embedding(),
// without its US Federal Register print-artefact patterns.
func cleanForEmbedding(text string) string {
	// Remove OSCAL template placeholders.
	text = oscalTemplateRE.ReplaceAllString(text, "")

	// Remove markdown table separators.
	text = tableSeparatorRE.ReplaceAllString(text, "")

	// Collapse runs of 3+ newlines to double newline.
	text = multiNewlineRE.ReplaceAllString(text, "\n\n")

	// Collapse runs of 2+ spaces/tabs to single space.
	text = multiSpaceRE.ReplaceAllString(text, " ")

	return strings.TrimSpace(text)
}
