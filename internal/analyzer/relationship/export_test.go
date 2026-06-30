package relationship

// Bridge file: exports unexported symbols for external test packages.
// ParseResponse and ComputeConsensus are already exported — no bridge needed.

// ExportTruncateText exposes truncateText for testing.
var ExportTruncateText = truncateText

// ExportFormatFewShotExamples exposes formatFewShotExamples for testing.
var ExportFormatFewShotExamples = formatFewShotExamples

// ExportClassificationGuidance exposes the static guidance text.
var ExportClassificationGuidance = classificationGuidance

// ExportRelationshipDefinitions exposes the static definitions text.
var ExportRelationshipDefinitions = relationshipDefinitions
