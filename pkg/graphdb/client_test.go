package graphdb

import (
	"strings"
	"testing"
	"time"
)

func TestEscapeCypher(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{name: "no special chars", input: "hello", expected: "hello"},
		{name: "backslash", input: `a\b`, expected: `a\\b`},
		{name: "single quote", input: "it's", expected: `it\'s`},
		{name: "both", input: `it's a\b`, expected: `it\'s a\\b`},
		{name: "empty", input: "", expected: ""},
		{name: "multiple backslashes", input: `a\\b`, expected: `a\\\\b`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := escapeCypher(tt.input)
			if got != tt.expected {
				t.Errorf("escapeCypher(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestCypherValue(t *testing.T) {
	tests := []struct {
		name     string
		input    any
		expected string
	}{
		{name: "string", input: "hello", expected: "'hello'"},
		{name: "string with quote", input: "it's", expected: `'it\'s'`},
		{name: "float64", input: float64(3.14), expected: "3.14"},
		{name: "float64 integer", input: float64(42), expected: "42"},
		{name: "float32", input: float32(2.5), expected: "2.5"},
		{name: "int", input: 7, expected: "7"},
		{name: "int64", input: int64(99), expected: "99"},
		{name: "bool true", input: true, expected: "true"},
		{name: "bool false", input: false, expected: "false"},
		{name: "other type", input: []int{1, 2}, expected: "'[1 2]'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cypherValue(tt.input)
			if got != tt.expected {
				t.Errorf("cypherValue(%v) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestNodeToAGProperties(t *testing.T) {
	validFrom, _ := time.Parse(time.RFC3339, "2025-01-01T00:00:00Z")
	validTo, _ := time.Parse(time.RFC3339, "2025-12-31T23:59:59Z")

	t.Run("minimal node", func(t *testing.T) {
		n := Node{
			ID:        "req-1",
			ValidFrom: validFrom,
		}
		got := nodeToAGProperties(n)
		if !strings.Contains(got, "id: 'req-1'") {
			t.Errorf("expected id field in output, got %s", got)
		}
		if !strings.Contains(got, "valid_from: '2025-01-01T00:00:00Z'") {
			t.Errorf("expected valid_from field in output, got %s", got)
		}
		if !strings.HasPrefix(got, "{") || !strings.HasSuffix(got, "}") {
			t.Errorf("expected output wrapped in braces, got %s", got)
		}
		// Minimal node should not contain valid_to or created_by.
		if strings.Contains(got, "valid_to") {
			t.Errorf("unexpected valid_to in minimal node output: %s", got)
		}
		if strings.Contains(got, "created_by") {
			t.Errorf("unexpected created_by in minimal node output: %s", got)
		}
	})

	t.Run("full node", func(t *testing.T) {
		n := Node{
			ID:             "req-2",
			ValidFrom:      validFrom,
			ValidTo:        &validTo,
			CreatedBy:      "admin",
			CreationMethod: "import",
			Properties:     map[string]any{"severity": "high"},
		}
		got := nodeToAGProperties(n)
		if !strings.Contains(got, "id: 'req-2'") {
			t.Errorf("expected id field, got %s", got)
		}
		if !strings.Contains(got, "valid_to: '2025-12-31T23:59:59Z'") {
			t.Errorf("expected valid_to field, got %s", got)
		}
		if !strings.Contains(got, "created_by: 'admin'") {
			t.Errorf("expected created_by field, got %s", got)
		}
		if !strings.Contains(got, "creation_method: 'import'") {
			t.Errorf("expected creation_method field, got %s", got)
		}
		if !strings.Contains(got, "severity: 'high'") {
			t.Errorf("expected custom property severity, got %s", got)
		}
	})
}

func TestEdgeToAGProperties(t *testing.T) {
	validFrom, _ := time.Parse(time.RFC3339, "2025-01-01T00:00:00Z")

	t.Run("minimal edge", func(t *testing.T) {
		e := Edge{
			ID:        "e-1",
			Source:    "a",
			Target:    "b",
			ValidFrom: validFrom,
		}
		got := edgeToAGProperties(e)
		if !strings.Contains(got, "id: 'e-1'") {
			t.Errorf("expected id field, got %s", got)
		}
		if !strings.Contains(got, "source: 'a'") {
			t.Errorf("expected source field, got %s", got)
		}
		if !strings.Contains(got, "target: 'b'") {
			t.Errorf("expected target field, got %s", got)
		}
		if !strings.HasPrefix(got, "{") || !strings.HasSuffix(got, "}") {
			t.Errorf("expected output wrapped in braces, got %s", got)
		}
	})

	t.Run("edge with optional fields", func(t *testing.T) {
		validTo, _ := time.Parse(time.RFC3339, "2025-12-31T23:59:59Z")
		e := Edge{
			ID:                "e-2",
			Source:            "src",
			Target:            "tgt",
			ValidFrom:         validFrom,
			ValidTo:           &validTo,
			DeterminedBy:      "scanner",
			DeterminationType: "automated",
			Confidence:        0.85,
			Supersedes:        "e-0",
		}
		got := edgeToAGProperties(e)
		if !strings.Contains(got, "determined_by: 'scanner'") {
			t.Errorf("expected determined_by field, got %s", got)
		}
		if !strings.Contains(got, "determination_type: 'automated'") {
			t.Errorf("expected determination_type field, got %s", got)
		}
		if !strings.Contains(got, "confidence: 0.85") {
			t.Errorf("expected confidence field, got %s", got)
		}
		if !strings.Contains(got, "supersedes: 'e-0'") {
			t.Errorf("expected supersedes field, got %s", got)
		}
	})
}

func TestCreateNodeValidation(t *testing.T) {
	validFrom, _ := time.Parse(time.RFC3339, "2025-01-01T00:00:00Z")
	client := &ageClient{} // nil db is fine since we never reach the query

	tests := []struct {
		name    string
		node    Node
		wantErr string
	}{
		{
			name:    "empty id",
			node:    Node{Label: "X", ValidFrom: validFrom},
			wantErr: "create node: id is required",
		},
		{
			name:    "empty label",
			node:    Node{ID: "n-1", ValidFrom: validFrom},
			wantErr: "create node: label is required",
		},
		{
			name:    "zero valid_from",
			node:    Node{ID: "n-1", Label: "X"},
			wantErr: "create node: valid_from is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := client.CreateNode(nil, "test-tenant", tt.node) //nolint:staticcheck // nil context is fine for validation-only path
			if err == nil {
				t.Fatal("expected error")
			}
			if err.Error() != tt.wantErr {
				t.Errorf("got error %q, want %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestCreateEdgeValidation(t *testing.T) {
	validFrom, _ := time.Parse(time.RFC3339, "2025-01-01T00:00:00Z")
	client := &ageClient{} // nil db is fine since we never reach the query

	tests := []struct {
		name    string
		edge    Edge
		wantErr string
	}{
		{
			name:    "empty label",
			edge:    Edge{Source: "a", Target: "b", ValidFrom: validFrom},
			wantErr: "create edge: label is required",
		},
		{
			name:    "empty source",
			edge:    Edge{Label: "R", Target: "b", ValidFrom: validFrom},
			wantErr: "create edge: source and target are required",
		},
		{
			name:    "empty target",
			edge:    Edge{Label: "R", Source: "a", ValidFrom: validFrom},
			wantErr: "create edge: source and target are required",
		},
		{
			name:    "zero valid_from",
			edge:    Edge{Label: "R", Source: "a", Target: "b"},
			wantErr: "create edge: valid_from is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := client.CreateEdge(nil, "test-tenant", tt.edge) //nolint:staticcheck // nil context is fine for validation-only path
			if err == nil {
				t.Fatal("expected error")
			}
			if err.Error() != tt.wantErr {
				t.Errorf("got error %q, want %q", err.Error(), tt.wantErr)
			}
		})
	}
}
