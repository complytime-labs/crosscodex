package prompt

import (
	"context"
	"log/slog"
)

// Role constants for message construction.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// PromptSpec is the parsed representation of a single YAML prompt file.
type PromptSpec struct {
	Name      string            `yaml:"name"`
	Version   string            `yaml:"version"`
	Templates TemplateSet       `yaml:"templates"`
	FewShot   []FewShotExample  `yaml:"few_shot_examples"`
	Metadata  map[string]string `yaml:"metadata"`
}

// TemplateSet holds the system and user template strings.
type TemplateSet struct {
	System string `yaml:"system"`
	User   string `yaml:"user"`
}

// FewShotExample represents a single few-shot example for prompt construction.
// When Source is set, Input and Output are ignored (source takes precedence).
type FewShotExample struct {
	Input  string `yaml:"input"`
	Output string `yaml:"output"`
	Source string `yaml:"source,omitempty"`

	// SourceDir is the directory from which this example was loaded.
	// It is set internally during layer construction and is not serialized.
	// Used to resolve relative file: paths in Source references.
	SourceDir string `yaml:"-" json:"-"`
}

// Message is a role-content pair representing one segment of a rendered prompt.
// This type mirrors llmclient.ChatMessage structurally but is defined locally
// to avoid a dependency from pkg/prompt to pkg/llmclient.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ResolvedPrompt is the output after layering and rendering.
type ResolvedPrompt struct {
	Name        string
	Version     string
	Messages    []Message
	ContentHash string
	Sources     []string
	Metadata    map[string]string
}

// LogValue implements slog.LogValuer for structured debug logging.
// At all levels it emits name and hash. Callers should use slog.LevelDebug
// to include full message content.
func (r *ResolvedPrompt) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("name", r.Name),
		slog.String("version", r.Version),
		slog.String("content_hash", r.ContentHash),
		slog.Any("sources", r.Sources),
	)
}

// LayerInfo describes one layer in the prompt resolution stack.
type LayerInfo struct {
	ID            string
	Source        string
	Merge         string
	SliceStrategy string
	HasPrompt     bool
}

// Registry resolves, renders, and inspects versioned prompt templates.
type Registry interface {
	// Resolve returns a fully layered, unrendered PromptSpec.
	// Returns ErrPromptNotFound if no layer provides the named prompt.
	Resolve(ctx context.Context, name string) (*PromptSpec, error)

	// Render resolves, substitutes placeholders, loads external few-shot
	// sources, assembles []Message, and computes ContentHash.
	// Returns ErrMissingPlaceholder if a template references an undefined variable.
	Render(ctx context.Context, name string, vars map[string]string) (*ResolvedPrompt, error)

	// List returns all available prompt names (union across all layers).
	List(ctx context.Context) ([]string, error)

	// Layers returns the layer stack for a named prompt showing which layers
	// contribute and their merge configuration.
	Layers(ctx context.Context, name string) ([]LayerInfo, error)
}
