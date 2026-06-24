// Package classify implements a compliance requirement classification analyzer.
//
// It classifies each requirement on two dimensions:
//   - Implementation Type: Technical, Procedural, Both, or None
//   - Abstraction Level: Strategic, Tactical, Operational, or None
//
// The analyzer implements [github.com/complytime-labs/crosscodex/pkg/analyzer.Analyzer]
// parameterized on [*catalogpb.Control]. It uses the classification prompt
// resolved via [github.com/complytime-labs/crosscodex/pkg/prompt.Registry] and
// calls the LLM via [github.com/complytime-labs/crosscodex/pkg/llmclient.Client].
//
// Section controls (class == "compliance-section") are auto-classified as None|None
// without an LLM call. This matches the behavior of the Python OllamaCrosswalker.
package classify
