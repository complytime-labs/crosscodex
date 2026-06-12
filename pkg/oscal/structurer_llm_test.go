package oscal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// mockCompleter implements Completer for testing
type mockCompleter struct {
	response string
	err      error
}

func (m *mockCompleter) Complete(ctx context.Context, messages []Message) (string, error) {
	return m.response, m.err
}

func TestTierLLMDetect_NilCompleter(t *testing.T) {
	doc := StructuredDoc{RawText: "some text"}
	opts := StructureOptions{}

	items, ok := TierLLMDetect(doc, opts, nil, nil)

	if items != nil {
		t.Errorf("expected nil items, got %v", items)
	}
	if ok {
		t.Error("expected ok=false when completer is nil")
	}
}

func TestTierLLMDetect_SkipLLM(t *testing.T) {
	doc := StructuredDoc{RawText: "some text"}
	opts := StructureOptions{SkipLLM: true}
	completer := &mockCompleter{response: `\d+\.\s+(.+)`}

	items, ok := TierLLMDetect(doc, opts, completer, nil)

	if items != nil {
		t.Errorf("expected nil items, got %v", items)
	}
	if ok {
		t.Error("expected ok=false when SkipLLM is true")
	}
}

func TestTierLLMDetect_ValidPattern(t *testing.T) {
	doc := StructuredDoc{
		RawText: `1. First requirement
2. Second requirement
3. Third requirement`,
	}
	opts := StructureOptions{ChunkChars: 1000}
	// Return a regex pattern that matches numbered sections
	completer := &mockCompleter{response: `(\d+)\.\s+(.+)`}

	items, ok := TierLLMDetect(doc, opts, completer, nil)

	if !ok {
		t.Error("expected ok=true when valid pattern is detected")
	}
	if len(items) != 3 {
		t.Errorf("expected 3 items, got %d", len(items))
	}
	if len(items) > 0 && items[0].ID != "1" {
		t.Errorf("expected first item ID='1', got '%s'", items[0].ID)
	}
}

func TestTierLLMDetect_NoneResponse(t *testing.T) {
	doc := StructuredDoc{RawText: "unstructured text"}
	opts := StructureOptions{}
	completer := &mockCompleter{response: "none"}

	items, ok := TierLLMDetect(doc, opts, completer, nil)

	if items != nil {
		t.Errorf("expected nil items, got %v", items)
	}
	if ok {
		t.Error("expected ok=false when LLM returns 'none'")
	}
}

func TestTierLLMExtract_NilCompleter(t *testing.T) {
	doc := StructuredDoc{RawText: "some text"}
	opts := StructureOptions{}

	items, ok := TierLLMExtract(doc, opts, nil, nil)

	if items != nil {
		t.Errorf("expected nil items, got %v", items)
	}
	if ok {
		t.Error("expected ok=false when completer is nil")
	}
}

func TestTierLLMExtract_SkipLLM(t *testing.T) {
	doc := StructuredDoc{RawText: "some text"}
	opts := StructureOptions{SkipLLM: true}
	completer := &mockCompleter{response: `[]`}

	items, ok := TierLLMExtract(doc, opts, completer, nil)

	if items != nil {
		t.Errorf("expected nil items, got %v", items)
	}
	if ok {
		t.Error("expected ok=false when SkipLLM is true")
	}
}

func TestTierLLMExtract_ValidJSON(t *testing.T) {
	doc := StructuredDoc{RawText: "Text with requirements"}
	opts := StructureOptions{ChunkChars: 1000}
	jsonResponse := `[
		{"id": "req-1", "title": "First", "text": "First requirement text"},
		{"id": "req-2", "title": "Second", "text": "Second requirement text"}
	]`
	completer := &mockCompleter{response: jsonResponse}

	items, ok := TierLLMExtract(doc, opts, completer, nil)

	if !ok {
		t.Error("expected ok=true when valid JSON is returned")
	}
	if len(items) != 2 {
		t.Errorf("expected 2 items, got %d", len(items))
	}
	if len(items) > 0 {
		if items[0].ID != "req-1" {
			t.Errorf("expected first item ID='req-1', got '%s'", items[0].ID)
		}
		if items[0].Title != "First" {
			t.Errorf("expected first item Title='First', got '%s'", items[0].Title)
		}
		if items[0].Text != "First requirement text" {
			t.Errorf("expected first item Text='First requirement text', got '%s'", items[0].Text)
		}
		if items[0].Class != ClassRequirement {
			t.Errorf("expected Class=%s, got %s", ClassRequirement, items[0].Class)
		}
	}
}

func TestTierLLMExtract_InvalidJSON(t *testing.T) {
	doc := StructuredDoc{RawText: "Text"}
	opts := StructureOptions{}
	completer := &mockCompleter{response: "not json"}

	items, ok := TierLLMExtract(doc, opts, completer, nil)

	if items != nil {
		t.Errorf("expected nil items for invalid JSON, got %v", items)
	}
	if ok {
		t.Error("expected ok=false for invalid JSON")
	}
}

func TestChunkText_SingleChunk(t *testing.T) {
	text := "Short text"
	chunks := ChunkText(text, 100)

	if len(chunks) != 1 {
		t.Errorf("expected 1 chunk, got %d", len(chunks))
	}
	if chunks[0] != text {
		t.Errorf("expected chunk to equal input text")
	}
}

func TestChunkText_MultipleChunks(t *testing.T) {
	// Create text that will require multiple chunks
	lines := []string{
		"Line 1: " + strings.Repeat("a", 40),
		"Line 2: " + strings.Repeat("b", 40),
		"Line 3: " + strings.Repeat("c", 40),
		"Line 4: " + strings.Repeat("d", 40),
	}
	text := strings.Join(lines, "\n")

	chunks := ChunkText(text, 100) // Small chunk size to force splitting

	if len(chunks) < 2 {
		t.Errorf("expected at least 2 chunks, got %d", len(chunks))
	}

	// Verify each chunk doesn't exceed maxChars significantly
	// (it can slightly exceed due to line boundaries)
	for i, chunk := range chunks {
		if len(chunk) > 150 { // Allow some overflow for line boundaries
			t.Errorf("chunk %d is too large: %d chars", i, len(chunk))
		}
	}

	// Verify we can reconstruct the original text
	reconstructed := strings.Join(chunks, "\n")
	if reconstructed != text {
		t.Error("reconstructed text doesn't match original")
	}
}

func TestChunkText_NewlineBoundaries(t *testing.T) {
	text := "Line 1\nLine 2\nLine 3"
	chunks := ChunkText(text, 10) // Force chunking at line boundaries

	// Each chunk should contain complete lines
	for i, chunk := range chunks {
		if strings.HasPrefix(chunk, "\n") && i > 0 {
			t.Errorf("chunk %d starts with newline: %q", i, chunk)
		}
	}
}

func TestChunkText_ZeroMaxChars(t *testing.T) {
	// Create text with newlines so it can actually be chunked
	lines := make([]string, 100)
	for i := range lines {
		lines[i] = strings.Repeat("x", 50)
	}
	text := strings.Join(lines, "\n")
	chunks := ChunkText(text, 0) // Should use default of 3000

	if len(chunks) < 2 {
		t.Errorf("expected multiple chunks with default size, got %d", len(chunks))
	}
}

func TestChunkText_SingleLongLine(t *testing.T) {
	// Edge case: single line longer than maxChars
	text := strings.Repeat("x", 500)
	chunks := ChunkText(text, 100)

	// Should still return the line as a single chunk
	if len(chunks) != 1 {
		t.Errorf("expected 1 chunk for single long line, got %d", len(chunks))
	}
	if chunks[0] != text {
		t.Error("chunk doesn't match original text")
	}
}

func TestTierLLMExtract_MultipleChunks(t *testing.T) {
	// Create a document that will be chunked
	longText := strings.Repeat("requirement text\n", 500)
	doc := StructuredDoc{RawText: longText}
	opts := StructureOptions{ChunkChars: 1000}

	// Use a completer that returns valid JSON
	// Multiple chunks will result in multiple items
	completer := &mockCompleter{
		response: `[{"id": "req-1", "title": "First", "text": "First req"}]`,
	}

	items, ok := TierLLMExtract(doc, opts, completer, nil)

	if !ok {
		t.Error("expected ok=true")
	}
	// Should have multiple items from multiple chunks (each chunk returns 1 item)
	if len(items) < 2 {
		t.Errorf("expected at least 2 items from multiple chunks, got %d", len(items))
	}
}

func TestTierLLMDetect_WithCustomPrompt(t *testing.T) {
	doc := StructuredDoc{
		RawText: `Section A.1: First
Section A.2: Second`,
	}
	opts := StructureOptions{}
	completer := &mockCompleter{response: `Section ([A-Z]\.\d+):\s+(.+)`}

	prompts := &mockPromptLoader{
		prompts: map[string]string{
			"section_detect": "Custom prompt for section detection",
		},
	}

	items, ok := TierLLMDetect(doc, opts, completer, prompts)

	if !ok {
		t.Error("expected ok=true")
	}
	if len(items) != 2 {
		t.Errorf("expected 2 items, got %d", len(items))
	}
}

func TestTierLLMExtract_WithCustomPrompt(t *testing.T) {
	doc := StructuredDoc{RawText: "Text"}
	opts := StructureOptions{}
	completer := &mockCompleter{
		response: `[{"id": "c-1", "title": "Custom", "text": "Custom req"}]`,
	}

	prompts := &mockPromptLoader{
		prompts: map[string]string{
			"structured_extract": "Custom extraction prompt",
		},
	}

	items, ok := TierLLMExtract(doc, opts, completer, prompts)

	if !ok {
		t.Error("expected ok=true")
	}
	if len(items) != 1 {
		t.Errorf("expected 1 item, got %d", len(items))
	}
}

func TestTierLLMExtract_CapsItemsPerChunk(t *testing.T) {
	type item struct {
		ID    string `json:"id"`
		Title string `json:"title"`
		Text  string `json:"text"`
	}
	bigResponse := make([]item, 600)
	for i := range bigResponse {
		bigResponse[i] = item{
			ID:    fmt.Sprintf("item-%d", i),
			Title: fmt.Sprintf("Title %d", i),
			Text:  fmt.Sprintf("Text %d", i),
		}
	}
	responseJSON, _ := json.Marshal(bigResponse)

	completer := &mockCompleter{response: string(responseJSON)}
	doc := StructuredDoc{RawText: "some text"}
	opts := StructureOptions{ChunkChars: 50000}

	items, ok := TierLLMExtract(doc, opts, completer, nil)
	if !ok {
		t.Fatal("expected extraction to succeed")
	}
	if len(items) > maxItemsPerChunk {
		t.Errorf("expected at most %d items, got %d", maxItemsPerChunk, len(items))
	}
}

func TestTierLLMExtract_CompleterErrorSkipsChunk(t *testing.T) {
	completer := &mockCompleter{err: errors.New("LLM unavailable")}
	doc := StructuredDoc{RawText: "some text that is chunked"}
	opts := StructureOptions{ChunkChars: 10}

	items, ok := TierLLMExtract(doc, opts, completer, nil)
	if ok {
		t.Error("expected failure when all chunks error")
	}
	if items != nil {
		t.Errorf("expected nil items, got %d", len(items))
	}
}

func TestTierLLMDetect_CompleterError(t *testing.T) {
	completer := &mockCompleter{err: errors.New("LLM unavailable")}
	doc := StructuredDoc{RawText: "1.1 First section\n1.2 Second section\n"}
	opts := StructureOptions{}

	items, ok := TierLLMDetect(doc, opts, completer, nil)
	if ok {
		t.Error("expected failure when completer errors")
	}
	if items != nil {
		t.Errorf("expected nil items, got %d", len(items))
	}
}
