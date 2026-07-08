package requires

// Bridge file: exports unexported symbols for external test packages.

// ExportTruncateText exposes truncateText for testing.
var ExportTruncateText = truncateText

// ExportFormatFewShotExamples exposes formatFewShotExamples for testing.
var ExportFormatFewShotExamples = formatFewShotExamples

// ExportRequiresGuidance exposes the static guidance text.
var ExportRequiresGuidance = requiresGuidance

// ExportDependencyDefinitions exposes the static definitions text.
var ExportDependencyDefinitions = dependencyDefinitions
