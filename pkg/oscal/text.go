package oscal

import (
	"regexp"
	"strings"
)

var (
	// oscalTemplatePattern strips OSCAL template markers (non-greedy)
	oscalTemplatePattern = regexp.MustCompile(`\{\{.*?\}\}`)

	// Markdown table separator rows (multiline flag, matching complete lines)
	tableSeperatorPattern = regexp.MustCompile(`(?m)^.*\|[-| ]+\|.*$\n?`)

	// Whitespace collapse patterns
	excessiveNewlinePattern = regexp.MustCompile(`\n{3,}`)
	excessiveSpacePattern   = regexp.MustCompile(`[ \t]{2,}`)
)

// CleanForEmbedding strips artifacts before embedding generation. It runs on
// every catalog and on search queries, so it removes only markup, never
// publisher print artefacts: those are stripped from document-derived text
// alone (StructureOptions.PrintArtifactPatterns), since an OSCAL statement is
// often a single line that a line-deleting pattern would erase.
//
// Operations applied in order:
//  1. Strip OSCAL templates: \{\{[^}]*\}\} → empty
//  2. Strip markdown table separator lines
//  3. Collapse 3+ consecutive newlines → double newline
//  4. Collapse 2+ consecutive spaces/tabs → single space
//  5. Trim leading/trailing whitespace
func CleanForEmbedding(text string) string {
	// 1. Strip OSCAL templates
	text = oscalTemplatePattern.ReplaceAllString(text, "")

	// 2. Strip table separators
	text = tableSeperatorPattern.ReplaceAllString(text, "")

	// 3. Collapse excessive newlines
	text = excessiveNewlinePattern.ReplaceAllString(text, "\n\n")

	// 4. Collapse excessive spaces
	text = excessiveSpacePattern.ReplaceAllString(text, " ")

	// 5. Trim
	return strings.TrimSpace(text)
}
