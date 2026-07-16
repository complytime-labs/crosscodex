// Package synthesis provides test-package bridge exports for cross-package
// test access to unexported synthesis internals.
package synthesis

// ExportFilterByConfidence exposes filterByConfidence for BDD tests.
var ExportFilterByConfidence = filterByConfidence

// ExportCapMappingsPerSource exposes capMappingsPerSource for BDD tests.
var ExportCapMappingsPerSource = capMappingsPerSource
