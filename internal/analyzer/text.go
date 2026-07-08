package analyzer

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/complytime-labs/crosscodex/pkg/prompt"
)

// TruncateText replaces newlines with spaces and truncates to maxChars runes.
// A maxChars of 0 or negative disables truncation.
func TruncateText(text string, maxChars int) string {
	text = strings.ReplaceAll(text, "\n", " ")
	text = strings.ReplaceAll(text, "\r", " ")
	if maxChars > 0 && utf8.RuneCountInString(text) > maxChars {
		runes := []rune(text)
		text = string(runes[:maxChars])
	}
	return text
}

// FormatFewShotExamples formats few-shot examples into numbered inline text.
// This is the multi-line format used by relationship and requires analyzers.
func FormatFewShotExamples(examples []prompt.FewShotExample) string {
	if len(examples) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("EXAMPLES:\n\n")
	for i, ex := range examples {
		fmt.Fprintf(&b, "Example %d:\n%s\nExpected output:\n%s\n", i+1, ex.Input, ex.Output)
	}
	return b.String()
}
