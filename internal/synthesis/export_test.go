// Package synthesis provides test-package bridge exports for cross-package
// test access to unexported synthesis internals.
package synthesis

import "github.com/complytime-labs/crosscodex/pkg/config"

// ExportFilterByConfidence exposes filterByConfidence for BDD tests.
var ExportFilterByConfidence = filterByConfidence

// ExportCapMappingsPerSource exposes capMappingsPerSource for BDD tests.
var ExportCapMappingsPerSource = capMappingsPerSource

// ExportDefaultAssessmentConfig returns the spec-default AssessmentConfig.
// It is defined once here (untagged, package synthesis) so the in-package
// fuzz specs and the synthesis_test property/BDD specs share one value under
// every build-tag set.
func ExportDefaultAssessmentConfig() config.AssessmentConfig {
	return config.AssessmentConfig{
		IQRGood:        20.0,
		IQRPoor:        10.0,
		NoRelHigh:      0.97,
		NoRelLow:       0.80,
		ContestedWarn:  0.20,
		ActionableWarn: 0.30,
	}
}
