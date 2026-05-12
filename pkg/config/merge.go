package config

import "gopkg.in/yaml.v3"

func deepMerge(base, overlay *yaml.Node) (*yaml.Node, error) {
	if base == nil && overlay == nil {
		return nil, nil
	}
	if base == nil {
		return cloneNode(overlay), nil
	}
	if overlay == nil {
		return cloneNode(base), nil
	}

	if base.Kind == yaml.MappingNode && overlay.Kind == yaml.MappingNode {
		return mergeMappings(base, overlay)
	}

	return cloneNode(overlay), nil
}

func mergeMappings(base, overlay *yaml.Node) (*yaml.Node, error) {
	result := &yaml.Node{
		Kind:  yaml.MappingNode,
		Tag:   base.Tag,
		Style: base.Style,
	}

	baseKeys := mapKeys(base)
	result.Content = make([]*yaml.Node, len(base.Content))
	copy(result.Content, base.Content)

	for i := 0; i < len(overlay.Content)-1; i += 2 {
		oKey := overlay.Content[i]
		oVal := overlay.Content[i+1]

		if idx, ok := baseKeys[oKey.Value]; ok {
			merged, err := deepMerge(result.Content[idx+1], oVal)
			if err != nil {
				return nil, err
			}
			result.Content[idx+1] = merged
		} else {
			result.Content = append(result.Content, cloneNode(oKey), cloneNode(oVal))
		}
	}

	return result, nil
}

func mapKeys(node *yaml.Node) map[string]int {
	keys := make(map[string]int, len(node.Content)/2)
	for i := 0; i < len(node.Content)-1; i += 2 {
		keys[node.Content[i].Value] = i
	}
	return keys
}

func cloneNode(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	clone := *n
	if len(n.Content) > 0 {
		clone.Content = make([]*yaml.Node, len(n.Content))
		for i, child := range n.Content {
			clone.Content[i] = cloneNode(child)
		}
	}
	return &clone
}
