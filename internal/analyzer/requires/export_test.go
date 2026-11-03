package requires

// Bridge file: exports unexported symbols for external test packages.
// FormatFewShotExamples is public in internal/analyzer.

// ExportRequiresGuidance exposes the static guidance text.
var ExportRequiresGuidance = requiresGuidance

// ExportDependencyDefinitions exposes the static definitions text.
var ExportDependencyDefinitions = dependencyDefinitions
