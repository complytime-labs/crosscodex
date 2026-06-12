package oscal

import (
	"context"
	"io"
)

// Parser parses native OSCAL JSON catalogs into ControlItems.
type Parser interface {
	Parse(ctx context.Context, data io.Reader) ([]ControlItem, error)
	FindControl(items []ControlItem, controlID string) (*ControlItem, error)
}

// Structurer converts unstructured/semi-structured text into ControlItems.
type Structurer interface {
	Structure(ctx context.Context, doc StructuredDoc, opts StructureOptions) ([]ControlItem, error)
}

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
}

var DefaultKeywords = []string{
	"shall", "must", "will", "require", "ensure", "should",
	"prohibited", "may not", "must not", "is required", "are required",
	"shall not", "must be", "required to", "obligated",
}

type Assembler interface {
	Assemble(ctx context.Context, items []ControlItem, meta CatalogMeta) ([]byte, error)
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
