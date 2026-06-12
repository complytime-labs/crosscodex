// Package oscal provides OSCAL catalog parsing, structuring, assembly,
// and validation for compliance frameworks.
//
// The package handles three primary workflows:
//
//  1. Native OSCAL JSON parsing via go-oscal types (Parser)
//  2. Unstructured document conversion to OSCAL (Structurer)
//  3. ControlItem assembly back to valid OSCAL JSON (Assembler)
//
// Domain types use ControlItem as the pipeline-internal representation.
// go-oscal types are used only at the parsing boundary and converted
// immediately via convert.go.
//
// LLM integration uses injected Completer and Embedder interfaces,
// keeping this package free of infrastructure coupling.
package oscal
