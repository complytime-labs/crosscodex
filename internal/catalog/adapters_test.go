package catalog_test

import (
	"github.com/complytime-labs/crosscodex/internal/catalog"
	"github.com/complytime-labs/crosscodex/pkg/oscal"
)

// Compile-time interface satisfaction checks.
var _ oscal.Completer = (*catalog.LLMCompleter)(nil)
var _ oscal.Embedder = (*catalog.LLMEmbedder)(nil)
