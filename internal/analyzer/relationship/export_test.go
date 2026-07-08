package relationship

// Bridge file: exports unexported symbols for external test packages.
// ParseResponse and ComputeConsensus are already exported — no bridge needed.
// TruncateText and FormatFewShotExamples are now public in internal/analyzer.

// ExportClassificationGuidance exposes the static guidance text.
var ExportClassificationGuidance = classificationGuidance

// ExportRelationshipDefinitions exposes the static definitions text.
var ExportRelationshipDefinitions = relationshipDefinitions
