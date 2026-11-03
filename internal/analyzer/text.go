package analyzer

import (
	"errors"
	"fmt"
	"strings"

	"github.com/complytime-labs/crosscodex/pkg/prompt"
)

// ErrMissingControlText reports a candidate pair whose source or target
// control has no statement text in AnalyzerConfig.Parameters
// ("source_text_<id>" / "target_text_<id>"). The pair analyzers refuse to
// build such a prompt: an empty requirement wastes a billed LLM call and
// yields a meaningless verdict.
var ErrMissingControlText = errors.New("candidate control has no statement text")

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
