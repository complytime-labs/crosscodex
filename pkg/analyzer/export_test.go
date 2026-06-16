// Package analyzer exports internal functions for white-box testing.
// These exports are only available to test code via the _test build constraint.
package analyzer

var (
	ExportKahnSort    = kahnSort
	ExportFindCycle   = findCycle
	ExportFormatCycle = formatCycle
)
