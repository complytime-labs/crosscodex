package oscal

import (
	"regexp"
	"strings"
)

var (
	// oscalTemplatePattern strips OSCAL template markers (non-greedy)
	oscalTemplatePattern = regexp.MustCompile(`\{\{.*?\}\}`)

	// PDF artifact patterns (each applied with multiline flag, matching complete lines)
	tableSeperatorPattern = regexp.MustCompile(`(?m)^.*\|[-| ]+\|.*$\n?`)
	verDatePattern        = regexp.MustCompile(`(?m)^.*VerDate\s+.*$\n?`)
	jktPattern            = regexp.MustCompile(`(?m)^.*Jkt\s+\d+.*$\n?`)
	poPattern             = regexp.MustCompile(`(?m)^.*PO\s+\d{5}.*$\n?`)
	frmPattern            = regexp.MustCompile(`(?m)^.*Frm\s+\d+.*$\n?`)
	fmtPattern            = regexp.MustCompile(`(?m)^.*Fmt\s+\d+.*$\n?`)
	sfmtPattern           = regexp.MustCompile(`(?m)^.*Sfmt\s+\d+.*$\n?`)
	windowsPathPattern    = regexp.MustCompile(`(?m)^.*G:\\COMP\\.*$\n?`)

	// Whitespace collapse patterns
	excessiveNewlinePattern = regexp.MustCompile(`\n{3,}`)
	excessiveSpacePattern   = regexp.MustCompile(`[ \t]{2,}`)
)

// CleanForEmbedding strips artifacts before embedding generation.
//
// Operations applied in order:
//  1. Strip OSCAL templates: \{\{[^}]*\}\} → empty
//  2. Strip PDF artifacts (8 patterns):
//     - Markdown table separators
//     - VerDate metadata (Congressional Record dates)
//     - Jkt metadata (Job ticket)
//     - PO metadata (PO box)
//     - Frm metadata (Form)
//     - Fmt metadata (Format)
//     - Sfmt metadata (Sub-format)
//     - Windows path metadata (G:\COMP\...)
//  3. Collapse 3+ consecutive newlines → double newline
//  4. Collapse 2+ consecutive spaces/tabs → single space
//  5. Trim leading/trailing whitespace
func CleanForEmbedding(text string) string {
	// 1. Strip OSCAL templates
	text = oscalTemplatePattern.ReplaceAllString(text, "")

	// 2. Strip PDF artifacts
	text = tableSeperatorPattern.ReplaceAllString(text, "")
	text = verDatePattern.ReplaceAllString(text, "")
	text = jktPattern.ReplaceAllString(text, "")
	text = poPattern.ReplaceAllString(text, "")
	text = frmPattern.ReplaceAllString(text, "")
	text = fmtPattern.ReplaceAllString(text, "")
	text = sfmtPattern.ReplaceAllString(text, "")
	text = windowsPathPattern.ReplaceAllString(text, "")

	// 3. Collapse excessive newlines
	text = excessiveNewlinePattern.ReplaceAllString(text, "\n\n")

	// 4. Collapse excessive spaces
	text = excessiveSpacePattern.ReplaceAllString(text, " ")

	// 5. Trim
	return strings.TrimSpace(text)
}
