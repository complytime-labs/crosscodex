package config

import (
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// buildEnvOverlay scans the process environment for variables matching the
// given prefix and returns a yaml.Node overlay. When tracker is non-nil,
// each matched variable is recorded individually (e.g., "environment variable
// CROSSCODEX_TLS_MODE").
func buildEnvOverlay(prefix string, tracker *sourceTracker) (*yaml.Node, error) {
	envPrefix := prefix + "_"
	var overlay *yaml.Node

	for _, env := range os.Environ() {
		eqIdx := strings.IndexByte(env, '=')
		if eqIdx < 0 {
			continue
		}
		key := env[:eqIdx]
		val := env[eqIdx+1:]

		if !strings.HasPrefix(key, envPrefix) {
			continue
		}

		path := strings.ToLower(strings.TrimPrefix(key, envPrefix))
		segments := strings.Split(path, "_")
		node := buildNodeFromPath(segments, val)

		if tracker != nil {
			tracker.track(node, "environment variable "+key)
		}

		var err error
		overlay, err = deepMerge(overlay, node)
		if err != nil {
			return nil, err
		}
	}
	return overlay, nil
}

func applyEnvVars(base *yaml.Node, prefix string) (*yaml.Node, error) {
	overlay, err := buildEnvOverlay(prefix, nil)
	if err != nil {
		return nil, err
	}
	if overlay == nil {
		if base == nil {
			return nil, nil
		}
		return cloneNode(base), nil
	}
	return deepMerge(base, overlay)
}

func buildNodeFromPath(segments []string, value string) *yaml.Node {
	if len(segments) == 0 {
		return &yaml.Node{Kind: yaml.ScalarNode, Value: value, Tag: "!!str"}
	}

	defNode, err := defaultNode()
	if err != nil {
		return buildNodeSimple(segments, value)
	}
	return buildNodeWithSchema(segments, value, defNode)
}

func buildNodeWithSchema(segments []string, value string, schema *yaml.Node) *yaml.Node {
	if schema == nil || schema.Kind != yaml.MappingNode {
		return buildNodeSimple(segments, value)
	}

	keys := mapKeys(schema)

	for length := len(segments); length >= 1; length-- {
		candidate := strings.Join(segments[:length], "_")
		if idx, ok := keys[candidate]; ok {
			keyNode := &yaml.Node{Kind: yaml.ScalarNode, Value: candidate, Tag: "!!str"}
			var valNode *yaml.Node
			if length == len(segments) {
				schemaVal := schema.Content[idx+1]
				tag := inferTag(schemaVal, value)
				valNode = &yaml.Node{Kind: yaml.ScalarNode, Value: value, Tag: tag}
			} else {
				childSchema := schema.Content[idx+1]
				valNode = buildNodeWithSchema(segments[length:], value, childSchema)
			}
			return &yaml.Node{
				Kind:    yaml.MappingNode,
				Tag:     "!!map",
				Content: []*yaml.Node{keyNode, valNode},
			}
		}
	}

	return buildNodeSimple(segments, value)
}

// inferTag returns the YAML tag to use for a value based on the schema node's
// existing tag. This allows environment variable strings like "60" or "true"
// to be deserialized into the correct Go types.
func inferTag(schemaVal *yaml.Node, value string) string {
	if schemaVal == nil || schemaVal.Kind != yaml.ScalarNode {
		return "!!str"
	}
	switch schemaVal.Tag {
	case "!!bool":
		switch strings.ToLower(value) {
		case "true", "false", "yes", "no":
			return "!!bool"
		}
	case "!!int":
		digits := value
		if len(digits) > 0 && (digits[0] == '-' || digits[0] == '+') {
			digits = digits[1:]
		}
		if len(digits) == 0 {
			return "!!str"
		}
		for _, c := range digits {
			if c < '0' || c > '9' {
				return "!!str"
			}
		}
		return "!!int"
	case "!!float":
		return "!!float"
	}
	return schemaVal.Tag
}

func buildNodeSimple(segments []string, value string) *yaml.Node {
	if len(segments) == 0 {
		return &yaml.Node{Kind: yaml.ScalarNode, Value: value, Tag: "!!str"}
	}

	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Value: segments[0], Tag: "!!str"}
	valNode := buildNodeSimple(segments[1:], value)
	return &yaml.Node{
		Kind:    yaml.MappingNode,
		Tag:     "!!map",
		Content: []*yaml.Node{keyNode, valNode},
	}
}
