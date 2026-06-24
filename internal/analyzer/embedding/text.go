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

	// pdfArtifactRE matches Congressional Record metadata and other PDF
	// extraction artifacts. Each alternative matches a distinct artifact type.
	pdfArtifactRE = regexp.MustCompile(
		`(\|[-| ]+\|.*)` +
			`|(VerDate\s+\S+.*)` +
			`|(Jkt\s+\d+.*)` +
			`|(PO\s+\d{5}.*)` +
			`|(Frm\s+\d+.*)` +
			`|(Fmt\s+\d+.*)` +
			`|(Sfmt\s+\d+.*)` +
			`|(G:\\COMP\\.*)`)

	// multiNewlineRE matches runs of 3+ newlines for collapsing.
	multiNewlineRE = regexp.MustCompile(`\n{3,}`)

	// multiSpaceRE matches runs of 2+ spaces or tabs for collapsing.
	multiSpaceRE = regexp.MustCompile(`[ \t]{2,}`)
)

// prepareText prepends ancestor context, cleans OSCAL artifacts, and
// truncates to maxChars runes. Returns cleaned text ready for embedding.
// If maxChars is 0 or negative, no truncation is performed.
func prepareText(statement, ancestorTitle string, maxChars int) string {
	var text string
	if ancestorTitle != "" {
		text = "[" + ancestorTitle + "] " + statement
	} else {
		text = statement
	}

	text = cleanForEmbedding(text)

	if maxChars > 0 && utf8.RuneCountInString(text) > maxChars {
		runes := []rune(text)
		text = string(runes[:maxChars])
	}

	return text
}

// cleanForEmbedding removes OSCAL template placeholders and PDF extraction
// artifacts from text. Ported from Python oscal_utils.clean_for_embedding().
func cleanForEmbedding(text string) string {
	// Remove OSCAL template placeholders.
	text = oscalTemplateRE.ReplaceAllString(text, "")

	// Remove PDF extraction artifacts.
	text = pdfArtifactRE.ReplaceAllString(text, "")

	// Collapse runs of 3+ newlines to double newline.
	text = multiNewlineRE.ReplaceAllString(text, "\n\n")

	// Collapse runs of 2+ spaces/tabs to single space.
	text = multiSpaceRE.ReplaceAllString(text, " ")

	return strings.TrimSpace(text)
}
