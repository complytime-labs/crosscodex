package graphdb

import (
	"testing"
	"time"
)

func TestParseAGVertex(t *testing.T) {
	t.Run("valid vertex", func(t *testing.T) {
		raw := `{"id": 123, "label": "Requirement", "properties": {"id": "req-1", "valid_from": "2025-01-01T00:00:00Z", "created_by": "test"}}::vertex`
		node, err := parseAGVertex(raw)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if node.ID != "req-1" {
			t.Errorf("expected ID %q, got %q", "req-1", node.ID)
		}
		if node.Label != "Requirement" {
			t.Errorf("expected Label %q, got %q", "Requirement", node.Label)
		}
		if node.CreatedBy != "test" {
			t.Errorf("expected CreatedBy %q, got %q", "test", node.CreatedBy)
		}
		expectedTime, _ := time.Parse(time.RFC3339, "2025-01-01T00:00:00Z")
		if !node.ValidFrom.Equal(expectedTime) {
			t.Errorf("expected ValidFrom %v, got %v", expectedTime, node.ValidFrom)
		}
	})

	t.Run("vertex without property id falls back to graph id", func(t *testing.T) {
		raw := `{"id": 456, "label": "Control", "properties": {"valid_from": "2025-06-01T00:00:00Z"}}::vertex`
		node, err := parseAGVertex(raw)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if node.ID != "456" {
			t.Errorf("expected fallback ID %q, got %q", "456", node.ID)
		}
	})

	t.Run("missing ::vertex suffix", func(t *testing.T) {
		raw := `{"id": 1, "label": "X", "properties": {}}`
		_, err := parseAGVertex(raw)
		if err == nil {
			t.Fatal("expected error for missing ::vertex suffix")
		}
	})

	t.Run("invalid JSON", func(t *testing.T) {
		raw := `{bad json}::vertex`
		_, err := parseAGVertex(raw)
		if err == nil {
			t.Fatal("expected error for invalid JSON")
		}
	})

	t.Run("vertex with valid_to", func(t *testing.T) {
		raw := `{"id": 10, "label": "Policy", "properties": {"id": "pol-1", "valid_from": "2025-01-01T00:00:00Z", "valid_to": "2025-12-31T23:59:59Z"}}::vertex`
		node, err := parseAGVertex(raw)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if node.ValidTo == nil {
			t.Fatal("expected ValidTo to be set")
		}
		expectedTo, _ := time.Parse(time.RFC3339, "2025-12-31T23:59:59Z")
		if !node.ValidTo.Equal(expectedTo) {
			t.Errorf("expected ValidTo %v, got %v", expectedTo, *node.ValidTo)
		}
	})
}

func TestParseAGEdge(t *testing.T) {
	t.Run("valid edge", func(t *testing.T) {
		raw := `{"id": 789, "label": "SATISFIES", "start_id": 100, "end_id": 200, "properties": {"id": "edge-1", "source": "req-1", "target": "ctrl-1", "valid_from": "2025-01-01T00:00:00Z", "confidence": 0.95}}::edge`
		edge, err := parseAGEdge(raw)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if edge.ID != "edge-1" {
			t.Errorf("expected ID %q, got %q", "edge-1", edge.ID)
		}
		if edge.Label != "SATISFIES" {
			t.Errorf("expected Label %q, got %q", "SATISFIES", edge.Label)
		}
		if edge.Source != "req-1" {
			t.Errorf("expected Source %q, got %q", "req-1", edge.Source)
		}
		if edge.Target != "ctrl-1" {
			t.Errorf("expected Target %q, got %q", "ctrl-1", edge.Target)
		}
		if edge.Confidence != 0.95 {
			t.Errorf("expected Confidence %v, got %v", 0.95, edge.Confidence)
		}
	})

	t.Run("edge without property source/target falls back to start_id/end_id", func(t *testing.T) {
		raw := `{"id": 50, "label": "RELATES", "start_id": 10, "end_id": 20, "properties": {"valid_from": "2025-01-01T00:00:00Z"}}::edge`
		edge, err := parseAGEdge(raw)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if edge.Source != "10" {
			t.Errorf("expected fallback Source %q, got %q", "10", edge.Source)
		}
		if edge.Target != "20" {
			t.Errorf("expected fallback Target %q, got %q", "20", edge.Target)
		}
	})

	t.Run("missing ::edge suffix", func(t *testing.T) {
		raw := `{"id": 1, "label": "X", "start_id": 1, "end_id": 2, "properties": {}}`
		_, err := parseAGEdge(raw)
		if err == nil {
			t.Fatal("expected error for missing ::edge suffix")
		}
	})

	t.Run("invalid JSON", func(t *testing.T) {
		raw := `not json::edge`
		_, err := parseAGEdge(raw)
		if err == nil {
			t.Fatal("expected error for invalid JSON")
		}
	})
}

func TestParseAGPath(t *testing.T) {
	t.Run("vertex-edge-vertex path", func(t *testing.T) {
		vertex1 := `{"id": 1, "label": "Requirement", "properties": {"id": "req-1", "valid_from": "2025-01-01T00:00:00Z"}}::vertex`
		edge := `{"id": 10, "label": "SATISFIES", "start_id": 1, "end_id": 2, "properties": {"id": "e-1", "source": "req-1", "target": "ctrl-1", "valid_from": "2025-01-01T00:00:00Z"}}::edge`
		vertex2 := `{"id": 2, "label": "Control", "properties": {"id": "ctrl-1", "valid_from": "2025-01-01T00:00:00Z"}}::vertex`
		raw := "[" + vertex1 + ", " + edge + ", " + vertex2 + "]::path"

		path, err := parseAGPath(raw)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(path.Nodes) != 2 {
			t.Fatalf("expected 2 nodes, got %d", len(path.Nodes))
		}
		if len(path.Edges) != 1 {
			t.Fatalf("expected 1 edge, got %d", len(path.Edges))
		}
		if path.Nodes[0].ID != "req-1" {
			t.Errorf("expected first node ID %q, got %q", "req-1", path.Nodes[0].ID)
		}
		if path.Nodes[1].ID != "ctrl-1" {
			t.Errorf("expected second node ID %q, got %q", "ctrl-1", path.Nodes[1].ID)
		}
		if path.Edges[0].Label != "SATISFIES" {
			t.Errorf("expected edge label %q, got %q", "SATISFIES", path.Edges[0].Label)
		}
	})

	t.Run("missing ::path suffix", func(t *testing.T) {
		raw := `[{"id": 1, "label": "X", "properties": {}}::vertex]`
		_, err := parseAGPath(raw)
		if err == nil {
			t.Fatal("expected error for missing ::path suffix")
		}
	})

	t.Run("not an array", func(t *testing.T) {
		raw := `{"id": 1}::path`
		_, err := parseAGPath(raw)
		if err == nil {
			t.Fatal("expected error for non-array path")
		}
	})

	t.Run("unknown element type", func(t *testing.T) {
		raw := `[{"id": 1}::unknown]::path`
		_, err := parseAGPath(raw)
		if err == nil {
			t.Fatal("expected error for unknown element type")
		}
	})
}

func TestSplitAGPathElements(t *testing.T) {
	t.Run("single element", func(t *testing.T) {
		result := splitAGPathElements(`{"id": 1, "label": "X", "properties": {}}::vertex`)
		if len(result) != 1 {
			t.Fatalf("expected 1 element, got %d", len(result))
		}
	})

	t.Run("three elements with nested JSON", func(t *testing.T) {
		v1 := `{"id": 1, "label": "A", "properties": {"key": "val"}}::vertex`
		e := `{"id": 10, "label": "R", "start_id": 1, "end_id": 2, "properties": {"x": "y"}}::edge`
		v2 := `{"id": 2, "label": "B", "properties": {}}::vertex`
		input := v1 + ", " + e + ", " + v2

		result := splitAGPathElements(input)
		if len(result) != 3 {
			t.Fatalf("expected 3 elements, got %d", len(result))
		}
		// Verify commas inside JSON braces are not split on.
		if got := len(splitAGPathElements(`{"a": 1, "b": 2}::vertex`)); got != 1 {
			t.Errorf("expected 1 element for nested commas, got %d", got)
		}
	})

	t.Run("empty input", func(t *testing.T) {
		result := splitAGPathElements("")
		if len(result) != 0 {
			t.Fatalf("expected 0 elements, got %d", len(result))
		}
	})
}

func TestStripSuffix(t *testing.T) {
	t.Run("valid suffix", func(t *testing.T) {
		body, err := stripSuffix(`{"id": 1}::vertex`, "::vertex")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if body != `{"id": 1}` {
			t.Errorf("expected %q, got %q", `{"id": 1}`, body)
		}
	})

	t.Run("strips surrounding whitespace", func(t *testing.T) {
		body, err := stripSuffix(`  {"id": 1}::edge  `, "::edge")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if body != `{"id": 1}` {
			t.Errorf("expected %q, got %q", `{"id": 1}`, body)
		}
	})

	t.Run("wrong suffix", func(t *testing.T) {
		_, err := stripSuffix(`{"id": 1}::vertex`, "::edge")
		if err == nil {
			t.Fatal("expected error for wrong suffix")
		}
	})

	t.Run("no suffix at all", func(t *testing.T) {
		_, err := stripSuffix(`{"id": 1}`, "::vertex")
		if err == nil {
			t.Fatal("expected error for missing suffix")
		}
	})
}
