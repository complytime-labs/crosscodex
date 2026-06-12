package oscal

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// PromptLoader loads prompt templates by name.
type PromptLoader interface {
	LoadPrompt(name string) (string, error)
}

const maxItemsPerChunk = 500

const defaultSectionDetectPrompt = `You are a document structure analyst. Given a document, identify the repeating structural pattern used for section numbering or labeling. Return ONLY the regex pattern that matches section headers, or "none" if no pattern is found.`

const defaultStructuredExtractPrompt = `You are a compliance requirements extractor. Given a text, extract individual requirements as a JSON array. Each element should have "id" (identifier), "title" (short title), and "text" (full requirement text). Return ONLY valid JSON.`

// TierLLMDetect (Tier 4) uses an LLM to detect the section pattern, then delegates to TierRegex.
// Returns (nil, false) when completer is nil or opts.SkipLLM is true.
func TierLLMDetect(doc StructuredDoc, opts StructureOptions, completer Completer, prompts PromptLoader) ([]ControlItem, bool) {
	if completer == nil || opts.SkipLLM {
		return nil, false
	}

	// Load prompt template
	systemPrompt := defaultSectionDetectPrompt
	if prompts != nil {
		if p, err := prompts.LoadPrompt("section_detect"); err == nil && p != "" {
			systemPrompt = p
		}
	}

	// Truncate document text to ChunkChars
	chunkSize := opts.ChunkChars
	if chunkSize <= 0 {
		chunkSize = 3000
	}
	text := doc.RawText
	if len(text) > chunkSize {
		text = text[:chunkSize]
	}

	// Ask LLM to detect pattern
	messages := []Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: text},
	}

	ctx := context.Background()
	response, err := completer.Complete(ctx, messages)
	if err != nil {
		return nil, false
	}

	response = strings.TrimSpace(response)
	if response == "" || strings.ToLower(response) == "none" {
		return nil, false
	}

	// Try using the detected pattern with TierRegex
	optsWithPattern := opts
	optsWithPattern.SectionPattern = response
	return TierRegex(doc, optsWithPattern)
}

// TierLLMExtract (Tier 5) uses an LLM to extract structured requirements from text.
// Returns (nil, false) when completer is nil or opts.SkipLLM is true.
func TierLLMExtract(doc StructuredDoc, opts StructureOptions, completer Completer, prompts PromptLoader) ([]ControlItem, bool) {
	if completer == nil || opts.SkipLLM {
		return nil, false
	}

	// Load prompt template
	systemPrompt := defaultStructuredExtractPrompt
	if prompts != nil {
		if p, err := prompts.LoadPrompt("structured_extract"); err == nil && p != "" {
			systemPrompt = p
		}
	}

	// Determine chunk size
	chunkSize := opts.ChunkChars
	if chunkSize <= 0 {
		chunkSize = 3000
	}

	// Chunk the text
	chunks := ChunkText(doc.RawText, chunkSize)

	var allItems []ControlItem
	ctx := context.Background()

	for _, chunk := range chunks {
		messages := []Message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: chunk},
		}

		response, err := completer.Complete(ctx, messages)
		if err != nil {
			continue // Skip failed chunks
		}

		// Parse JSON response
		var extracted []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
			Text  string `json:"text"`
		}

		if err := json.Unmarshal([]byte(response), &extracted); err != nil {
			continue // Skip invalid JSON
		}

		if len(extracted) > maxItemsPerChunk {
			extracted = extracted[:maxItemsPerChunk]
		}

		// Convert to ControlItems
		for _, item := range extracted {
			allItems = append(allItems, ControlItem{
				ID:    item.ID,
				Title: item.Title,
				Text:  item.Text,
				Class: ClassRequirement,
			})
		}
	}

	if len(allItems) == 0 {
		return nil, false
	}

	return allItems, true
}

// ChunkText splits text into chunks of approximately maxChars characters.
// Breaks at newline boundaries to avoid splitting mid-line.
// Returns a single-element slice if text fits in one chunk.
func ChunkText(text string, maxChars int) []string {
	if maxChars <= 0 {
		maxChars = 3000
	}

	if len(text) <= maxChars {
		return []string{text}
	}

	var chunks []string
	lines := strings.Split(text, "\n")
	var currentChunk strings.Builder
	currentSize := 0

	for _, line := range lines {
		lineLen := len(line) + 1 // +1 for the newline

		// If adding this line would exceed maxChars, finalize current chunk
		if currentSize > 0 && currentSize+lineLen > maxChars {
			chunks = append(chunks, currentChunk.String())
			currentChunk.Reset()
			currentSize = 0
		}

		// Add line to current chunk
		if currentSize > 0 {
			currentChunk.WriteString("\n")
		}
		currentChunk.WriteString(line)
		currentSize += lineLen
	}

	// Add the last chunk if it has content
	if currentSize > 0 {
		chunks = append(chunks, currentChunk.String())
	}

	// Return single chunk if we somehow ended up with empty chunks
	if len(chunks) == 0 {
		return []string{text}
	}

	return chunks
}

// mockPromptLoader is a simple implementation for testing
type mockPromptLoader struct {
	prompts map[string]string
}

func (m *mockPromptLoader) LoadPrompt(name string) (string, error) {
	if m.prompts == nil {
		return "", fmt.Errorf("prompt not found: %s", name)
	}
	if p, ok := m.prompts[name]; ok {
		return p, nil
	}
	return "", fmt.Errorf("prompt not found: %s", name)
}
