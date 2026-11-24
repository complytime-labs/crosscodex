package oscal

import (
	"context"
	"io"
)

// Parser parses native OSCAL JSON catalogs into a Tree.
type Parser interface {
	Parse(ctx context.Context, data io.Reader) (*Tree, error)
	FindControl(items []ControlItem, controlID string) (*ControlItem, error)
}

// Structurer converts unstructured/semi-structured text into ControlItems.
type Structurer interface {
	Structure(ctx context.Context, doc StructuredDoc, opts StructureOptions) ([]ControlItem, error)
	// StructureWithTier is Structure that also names the cascade tier whose
	// items it returned, so a caller can refuse a low-confidence tier.
	StructureWithTier(ctx context.Context, doc StructuredDoc, opts StructureOptions) ([]ControlItem, StructureTier, error)
}

// StructureTier names a tier of the Structurer cascade.
type StructureTier string

// The Structurer cascade tiers, in the order they are tried.
const (
	StructureTierRegex      StructureTier = "regex"
	StructureTierHeading    StructureTier = "heading"
	StructureTierTable      StructureTier = "table"
	StructureTierPattern    StructureTier = "pattern"
	StructureTierLLMDetect  StructureTier = "llm-detect"
	StructureTierLLMExtract StructureTier = "llm-extract"
	StructureTierFallback   StructureTier = "fallback"
)

type StructuredDoc struct {
	Sections []DocSection
	Tables   []DocTable
	RawText  string
}

type DocSection struct {
	Level int
	Title string
	Text  string
}

type DocTable struct {
	Headers []string
	Rows    [][]string
}

type StructureOptions struct {
	SectionPattern     string
	SkipLLM            bool
	Decompose          bool
	MinDecomposeWords  int
	FilterByKeywords   bool
	Keywords           []string
	ChunkChars         int
	MaxValidationChars int
	AllowedFormats     []string
	MaxHeadingRepeats  int
	// PrintArtifactPatterns are RE2 patterns, compiled with the multiline
	// flag, whose matches Structure removes from each item's title and text
	// before keyword filtering and decomposition. Empty means no clean-up.
	// Fed by config catalog.structuring.print_artifact_patterns.
	PrintArtifactPatterns []string
}

var DefaultKeywords = []string{
	"shall", "must", "will", "require", "ensure", "should",
	"prohibited", "may not", "must not", "is required", "are required",
	"shall not", "must be", "required to", "obligated",
}

type Assembler interface {
	Assemble(ctx context.Context, tree *Tree, meta CatalogMeta) ([]byte, error)
}

type CatalogMeta struct {
	Title   string
	Version string
}

type Completer interface {
	Complete(ctx context.Context, messages []Message) (string, error)
}

type Message struct {
	Role    string // "system", "user", "assistant"
	Content string
}

type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}
