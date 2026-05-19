package graphdb

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type ageVertex struct {
	ID         int64          `json:"id"`
	Label      string         `json:"label"`
	Properties map[string]any `json:"properties"`
}

type ageEdge struct {
	ID         int64          `json:"id"`
	Label      string         `json:"label"`
	StartID    int64          `json:"start_id"`
	EndID      int64          `json:"end_id"`
	Properties map[string]any `json:"properties"`
}

func parseAGVertex(raw string) (Node, error) {
	body, err := stripSuffix(raw, "::vertex")
	if err != nil {
		return Node{}, err
	}
	var v ageVertex
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		return Node{}, fmt.Errorf("unmarshal vertex: %w", err)
	}
	return vertexToNode(v), nil
}

func parseAGEdge(raw string) (Edge, error) {
	body, err := stripSuffix(raw, "::edge")
	if err != nil {
		return Edge{}, err
	}
	var e ageEdge
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		return Edge{}, fmt.Errorf("unmarshal edge: %w", err)
	}
	return ageEdgeToEdge(e), nil
}

func parseAGPath(raw string) (Path, error) {
	trimmed := strings.TrimSpace(raw)
	if !strings.HasSuffix(trimmed, "::path") {
		return Path{}, fmt.Errorf("expected ::path suffix in agtype: %.80s", trimmed)
	}
	inner := trimmed[:len(trimmed)-len("::path")]
	inner = strings.TrimSpace(inner)
	if !strings.HasPrefix(inner, "[") || !strings.HasSuffix(inner, "]") {
		return Path{}, fmt.Errorf("expected path array in agtype: %.80s", inner)
	}
	inner = inner[1 : len(inner)-1]

	elements := splitAGPathElements(inner)
	var p Path
	for _, elem := range elements {
		elem = strings.TrimSpace(elem)
		switch {
		case strings.HasSuffix(elem, "::vertex"):
			node, err := parseAGVertex(elem)
			if err != nil {
				return Path{}, fmt.Errorf("parse path vertex: %w", err)
			}
			p.Nodes = append(p.Nodes, node)
		case strings.HasSuffix(elem, "::edge"):
			edge, err := parseAGEdge(elem)
			if err != nil {
				return Path{}, fmt.Errorf("parse path edge: %w", err)
			}
			p.Edges = append(p.Edges, edge)
		default:
			return Path{}, fmt.Errorf("unknown path element type: %.80s", elem)
		}
	}
	return p, nil
}

func stripSuffix(raw, suffix string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if !strings.HasSuffix(trimmed, suffix) {
		return "", fmt.Errorf("expected %s suffix in agtype: %.80s", suffix, trimmed)
	}
	return trimmed[:len(trimmed)-len(suffix)], nil
}

func vertexToNode(v ageVertex) Node {
	props := cloneProps(v.Properties)
	n := Node{
		ID:             extractString(props, "id"),
		Label:          v.Label,
		ValidFrom:      extractTime(props, "valid_from"),
		ValidTo:        extractTimePtr(props, "valid_to"),
		CreatedBy:      extractString(props, "created_by"),
		CreationMethod: extractString(props, "creation_method"),
	}
	if n.ID == "" {
		n.ID = fmt.Sprintf("%d", v.ID)
	}
	n.Properties = props
	return n
}

func ageEdgeToEdge(e ageEdge) Edge {
	props := cloneProps(e.Properties)
	edge := Edge{
		ID:                extractString(props, "id"),
		Label:             e.Label,
		Source:            extractString(props, "source"),
		Target:            extractString(props, "target"),
		ValidFrom:         extractTime(props, "valid_from"),
		ValidTo:           extractTimePtr(props, "valid_to"),
		DeterminedBy:      extractString(props, "determined_by"),
		DeterminationType: extractString(props, "determination_type"),
		Confidence:        extractFloat(props, "confidence"),
		Supersedes:        extractString(props, "supersedes"),
	}
	if edge.ID == "" {
		edge.ID = fmt.Sprintf("%d", e.ID)
	}
	if edge.Source == "" {
		edge.Source = fmt.Sprintf("%d", e.StartID)
	}
	if edge.Target == "" {
		edge.Target = fmt.Sprintf("%d", e.EndID)
	}
	edge.Properties = props
	return edge
}

func cloneProps(src map[string]any) map[string]any {
	if src == nil {
		return make(map[string]any)
	}
	dst := make(map[string]any, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func extractString(props map[string]any, key string) string {
	v, ok := props[key]
	if !ok {
		return ""
	}
	delete(props, key)
	s, _ := v.(string)
	return s
}

func extractFloat(props map[string]any, key string) float64 {
	v, ok := props[key]
	if !ok {
		return 0
	}
	delete(props, key)
	switch f := v.(type) {
	case float64:
		return f
	case json.Number:
		n, _ := f.Float64()
		return n
	default:
		return 0
	}
}

func extractTime(props map[string]any, key string) time.Time {
	s := extractString(props, key)
	if s == "" {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

func extractTimePtr(props map[string]any, key string) *time.Time {
	s := extractString(props, key)
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return nil
	}
	return &t
}

func splitAGPathElements(s string) []string {
	var elements []string
	depth := 0
	start := 0
	for i, ch := range s {
		switch ch {
		case '{':
			depth++
		case '}':
			depth--
		case ',':
			if depth == 0 {
				elements = append(elements, s[start:i])
				start = i + 1
			}
		}
	}
	if start < len(s) {
		elements = append(elements, s[start:])
	}
	return elements
}
