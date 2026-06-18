// Package oscal exports internal functions for white-box testing.
package oscal

// ExportWordCount exposes wordCount for property testing.
var ExportWordCount = wordCount

// ExportComplianceMapperNS exposes complianceMapperNS for BDD tests.
const ExportComplianceMapperNS = complianceMapperNS

// ExportExtractStatementProse exposes extractStatementProse for BDD tests.
var ExportExtractStatementProse = extractStatementProse

// ExportDeriveChildID exposes deriveChildID for BDD tests.
var ExportDeriveChildID = deriveChildID

// ExportMaxItemsPerChunk exposes maxItemsPerChunk for BDD tests.
const ExportMaxItemsPerChunk = maxItemsPerChunk
