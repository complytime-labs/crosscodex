// Package candidate provides a reusable framework for candidate pair generation.
//
// It defines the Generator interface and supports pluggable candidate generation
// strategies (semantic, keyword-based, level-based, etc.) with configurable
// aggregation. Extracted from requires analyzer to support reuse across
// relationship, requires, and future analyzers that need candidate pairs.
//
// Usage:
//
//	registry := candidate.NewRegistry()
//	registry.Register(semantic.New(...))
//	registry.Register(keyword.New(...))
//
//	candidates, err := registry.Generate(ctx, req, strategy)
package candidate
