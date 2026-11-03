package config

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// sourceTracker records which configuration source set each config value.
// It maps dotted paths (e.g., "tls.mode") to a human-readable source
// description (e.g., a file path or "environment variable CROSSCODEX_TLS_MODE").
type sourceTracker struct {
	sources map[string]string
}

func newSourceTracker() *sourceTracker {
	return &sourceTracker{sources: make(map[string]string)}
}

// track walks a yaml.Node tree and records every leaf path as originating
// from the given source. Later calls overwrite earlier ones, matching the
// "last layer wins" merge semantics.
func (s *sourceTracker) track(node *yaml.Node, source string) {
	if node == nil {
		return
	}
	s.walkPaths(node, "", source)
}

func (s *sourceTracker) walkPaths(node *yaml.Node, prefix, source string) {
	switch node.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i].Value
			child := node.Content[i+1]
			path := key
			if prefix != "" {
				path = prefix + "." + key
			}
			s.walkPaths(child, path, source)
		}
	case yaml.ScalarNode, yaml.SequenceNode:
		s.sources[prefix] = source
	}
}

func (s *sourceTracker) sourceOf(path string) string {
	if s == nil {
		return ""
	}
	if src, ok := s.sources[path]; ok {
		return src
	}
	return SourceDefaults
}

// SourceDefaults is the FieldError.Source of a value no layer set: the
// compiled defaults supplied it, or a required value is missing.
const SourceDefaults = "compiled defaults"

// sourceOfSetting returns the source of the value a FieldError path names:
// the source of that setting, or for a section (or a list item, which is
// tracked as part of its list), the source every value tracked below it
// shares. It returns "" when none or several sources set it.
func (s *sourceTracker) sourceOfSetting(path string) string {
	key := trackerKey(path)
	if src, ok := s.sources[key]; ok {
		return src
	}
	found := ""
	for p, src := range s.sources {
		if !strings.HasPrefix(p, key+".") {
			continue
		}
		if found != "" && found != src {
			return ""
		}
		found = src
	}
	return found
}

// formatSource returns a parenthetical source annotation for error messages.
// Returns an empty string when the tracker is nil (e.g., in unit tests that
// call validate directly).
func formatSource(tracker *sourceTracker, path string) string {
	if tracker == nil {
		return ""
	}
	return " (set in " + tracker.sourceOf(path) + ")"
}
