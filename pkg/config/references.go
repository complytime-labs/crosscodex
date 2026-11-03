package config

import (
	"fmt"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// resolveReferences returns a copy of n, one configuration file layer, with
// its YAML references written out: every alias is replaced by a copy of its
// anchor's value, every "<<" merge key by the pairs it merges in, anchors
// are dropped, and a key is written as the name yaml.v3 decodes it to (see
// resolveKey). n is not modified.
//
// deepMerge combines layers node by node and knows nothing of references, so
// an unresolved layer loses settings silently: a merge key's values land next
// to the explicit keys an earlier layer (or the compiled defaults) set, where
// the explicit keys win, and an alias replaces a whole section of an earlier
// layer with a node deepMerge cannot merge into. Resolving each layer first
// makes everything it sets explicit in that layer, so later layers override
// earlier ones key by key, references or not.
//
// Merge keys resolve as yaml.v3 decodes them, following the YAML merge key
// specification: a key set explicitly in a mapping wins over a merged one,
// and in a sequence of merged mappings an earlier mapping wins over a later
// one. Merged pairs follow the mapping's own pairs.
//
// Before resolving, n is decoded once with yaml.v3, which rejects what the
// expansion must not attempt (a merge value that is not a mapping, two merge
// keys in one mapping, an anchor that contains itself) and refuses a document
// that expands into excessive aliasing ("billion laughs"), so the resolved
// copy is never larger than what yaml.v3 would decode anyway.
func resolveReferences(n *yaml.Node, source string) (*yaml.Node, error) {
	if err := n.Decode(new(any)); err != nil {
		return nil, fmt.Errorf("resolving anchors, aliases and merge keys in %s: %w", source, err)
	}
	return resolveNode(n), nil
}

func resolveNode(n *yaml.Node) *yaml.Node {
	if n.Kind == yaml.AliasNode {
		return resolveNode(n.Alias)
	}
	out := *n
	out.Anchor = ""
	out.Content = nil
	if n.Kind == yaml.MappingNode {
		out.Content = mergedPairs(n, make(map[string]bool))
		return &out
	}
	for _, c := range n.Content {
		out.Content = append(out.Content, resolveNode(c))
	}
	return &out
}

// mergedPairs returns mapping m's resolved key/value pairs, leaving out the
// keys in set, then the pairs its merge key (if any) merges in. Every key it
// returns is added to set first, so neither a merged mapping nor a later
// entry of a merge sequence can set a key again.
//
// Keys are compared by their text, which assumes string keys: that is how
// yaml.v3 matches keys when it decodes into a struct or a map[string]T, the
// only mappings Config has. Decoding into interface{} distinguishes an int
// key 1 from a string key "1"; resolution does not.
func mergedPairs(m *yaml.Node, set map[string]bool) []*yaml.Node {
	var pairs []*yaml.Node
	var merge *yaml.Node
	for i := 0; i+1 < len(m.Content); i += 2 {
		if isMergeKey(m.Content[i]) {
			merge = m.Content[i+1]
			continue
		}
		key := resolveKey(m.Content[i])
		if key.Kind == yaml.ScalarNode && set[key.Value] {
			continue // set by the mapping that merges m, or by an earlier source
		}
		pairs = append(pairs, key, resolveNode(m.Content[i+1]))
	}
	for i := 0; i < len(pairs); i += 2 {
		if pairs[i].Kind == yaml.ScalarNode {
			set[pairs[i].Value] = true
		}
	}
	if merge != nil {
		for _, src := range mergeSources(merge) {
			pairs = append(pairs, mergedPairs(src, set)...)
		}
	}
	return pairs
}

// resolveKey resolves a mapping key like any node, and writes a !!binary
// key, whose text differs from the name yaml.v3 decodes it to, as that name,
// so deepMerge, which matches keys by text, matches it too.
func resolveKey(key *yaml.Node) *yaml.Node {
	out := resolveNode(key)
	if out.Kind == yaml.ScalarNode && out.ShortTag() == "!!binary" {
		// A name that is not UTF-8 names no field; the key walker reports
		// it, and the key stays binary so the layer still marshals.
		if name := keyName(out); utf8.ValidString(name) {
			out.Value, out.Tag, out.Style = name, "!!str", 0
		}
	}
	return out
}

// mergeSources returns the mappings a merge key's value names, in order:
// the mapping itself, the mapping an alias points to, or each of those in a
// sequence. resolveReferences has already had yaml.v3 reject any other
// shape.
func mergeSources(val *yaml.Node) []*yaml.Node {
	switch val.Kind {
	case yaml.AliasNode:
		return mergeSources(val.Alias)
	case yaml.SequenceNode:
		var srcs []*yaml.Node
		for _, item := range val.Content {
			srcs = append(srcs, mergeSources(item)...)
		}
		return srcs
	default:
		return []*yaml.Node{val}
	}
}

// isMergeKey reports whether key is a "<<" merge key exactly as yaml.v3's
// decoder decides it: the scalar "<<" with no tag, the non-specific tag "!"
// or !!merge. A quoted "<<" (tagged !!str) and a !!merge-tagged scalar with
// any other text are ordinary keys.
func isMergeKey(key *yaml.Node) bool {
	return key.Kind == yaml.ScalarNode && key.Value == "<<" &&
		(key.Tag == "" || key.Tag == "!" || key.ShortTag() == "!!merge")
}
