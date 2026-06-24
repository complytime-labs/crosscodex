package classify

import "github.com/complytime-labs/crosscodex/pkg/prompt"

// ExportSanitizeText exposes sanitizeText for testing.
var ExportSanitizeText = sanitizeText

// ExportFormatFewShotExamples exposes formatFewShotExamples for testing.
var ExportFormatFewShotExamples = func(examples []prompt.FewShotExample) string {
	return formatFewShotExamples(examples)
}

// ExportSubstitutePlaceholders delegates to the canonical prompt.SubstitutePlaceholders.
var ExportSubstitutePlaceholders = prompt.SubstitutePlaceholders
