package config

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// removedKeys maps a dotted path that was once a config key to guidance on
// what replaced it. A file that still sets one gets this guidance instead of a
// spelling suggestion.
var removedKeys = map[string]string{
	"analysis.relationship.top_k": "it never had an effect, because candidate pairs come from the generators; " +
		"to limit them, set analysis.candidates.generators[].config.top_k on the semantic generator",
	"tenants.allowed_tenants": "it never enforced anything; the gateway serves only tenants provisioned with " +
		"`crosscodexd admin tenant create`",
}

// maxSuggestionDistance is the largest edit distance at which a valid sibling
// key is offered as a "did you mean" suggestion.
const maxSuggestionDistance = 2

var (
	configType      = reflect.TypeFor[Config]()
	unmarshalerType = reflect.TypeFor[yaml.Unmarshaler]()
)

// checkKnownKeys checks node, one configuration file layer, and returns
// both kinds of problem checkLayer finds, joined, wrapping ErrInvalidConfig.
func checkKnownKeys(node *yaml.Node, source string) error {
	keyErrs, typeErrs := checkLayer(node, source, true)
	if len(keyErrs)+len(typeErrs) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrInvalidConfig, errors.Join(append(keyErrs, typeErrs...)...))
}

// checkLayer checks node, one configuration layer read from source, against
// Config and returns:
//
//   - keyErrs: when checkKeys is set, one ErrUnknownKey FieldError per
//     mapping key that names no Config field. Without it the non-strict
//     decode in Load silently drops a misspelled or removed key, including
//     safety settings, and the operator never learns their setting had no
//     effect.
//   - typeErrs: one ErrWrongType FieldError per value yaml.v3 cannot decode
//     into its setting's type (text for a number, a value where a section
//     belongs), at the line and column of source it came from.
//
// It runs on each layer before merging, so every node still carries the
// position in the file it came from; source names that file, CLI flag or
// environment variable. Both kinds are FieldErrors for the setting's path,
// so config set and unset can compare them across edits. The environment
// overlay is checked for types only (checkKeys false): some CROSSCODEX_*
// variables are not config paths.
func checkLayer(node *yaml.Node, source string, checkKeys bool) (keyErrs, typeErrs []error) {
	w := keyWalker{source: source, checkKeys: checkKeys, visited: make(map[visit]bool)}
	w.walk(node, configType, "", nil)
	return w.errs, w.typeErrs
}

// visit identifies one (node, type) check. An anchor reached again through an
// alias is checked once per type, which bounds the work on alias-heavy input
// ("billion laughs") and terminates on self-referencing aliases.
type visit struct {
	node *yaml.Node
	typ  reflect.Type
}

// overrideScope is set while walking the value of a tenant_overrides map: it
// is the global section the overrides apply to, so a key that is valid
// globally but not overridable gets a message saying exactly that.
//
// The scope covers only the keys directly under tenant_overrides.<tenant>:
// walkStruct passes it to no child, so a key deeper in an override (such as
// tenant_overrides.<tenant>.<section>.<key>) gets the ordinary spelling
// advice, even when a global section of the same name has that key.
type overrideScope struct {
	global reflect.Type
	path   string
}

type keyWalker struct {
	source    string
	checkKeys bool
	visited   map[visit]bool
	errs      []error
	typeErrs  []error
}

func (w *keyWalker) walk(n *yaml.Node, t reflect.Type, path string, scope *overrideScope) {
	if n == nil {
		return
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if n.Kind == yaml.AliasNode {
		w.walk(n.Alias, t, path, scope)
		return
	}
	v := visit{node: n, typ: t}
	if w.visited[v] {
		return
	}
	w.visited[v] = true

	if reflect.PointerTo(t).Implements(unmarshalerType) {
		// Its own UnmarshalYAML decides what it accepts.
		w.checkType(n, t, path)
		return
	}

	switch t.Kind() {
	case reflect.Struct:
		if n.Kind != yaml.MappingNode {
			w.checkType(n, t, path)
			return
		}
		w.walkStruct(n, t, path, scope)
	case reflect.Map:
		if n.Kind != yaml.MappingNode {
			w.checkType(n, t, path)
			return
		}
		w.eachPair(n, t, path, func(key, val *yaml.Node) {
			// The key is decoded too: a mapping or sequence used as a key
			// cannot become the map's key type. It has no text, so it is
			// reported at the map's own path.
			keyPath := path
			if key.Kind == yaml.ScalarNode {
				keyPath = mapKeyPath(path, key.Value)
			}
			w.checkType(key, t.Key(), keyPath)
			w.walk(val, t.Elem(), mapKeyPath(path, key.Value), scope)
		})
	case reflect.Slice, reflect.Array:
		if n.Kind != yaml.SequenceNode {
			w.checkType(n, t, path)
			return
		}
		for i, item := range n.Content {
			w.walk(item, t.Elem(), path+"["+strconv.Itoa(i)+"]", scope)
		}
	case reflect.Interface:
		// Open values (such as a candidate generator's config, which
		// validateCandidateGenerators checks) take any shape.
	default:
		// Scalars and time.Duration have no keys below them; only their
		// type is checked.
		w.checkType(n, t, path)
	}
}

// checkType decodes n into a new t the way Load's decode will, and reports
// a value it cannot decode. A null decodes into anything: it leaves the
// default in place.
func (w *keyWalker) checkType(n *yaml.Node, t reflect.Type, path string) {
	err := n.Decode(reflect.New(t).Interface())
	if err == nil {
		return
	}
	detail := err.Error()
	var te *yaml.TypeError
	if errors.As(err, &te) {
		// yaml.v3 prefixes each with "line N: "; the position is reported
		// once, below, in the file's terms.
		msgs := make([]string, len(te.Errors))
		for i, m := range te.Errors {
			if _, rest, ok := strings.Cut(m, ": "); ok && strings.HasPrefix(m, "line ") {
				m = rest
			}
			msgs[i] = m
		}
		detail = strings.Join(msgs, "; ")
	}
	label := path
	if label == "" {
		label = "the top level"
	}
	w.typeErrs = append(w.typeErrs, &FieldError{Path: path, Source: w.source,
		Err: fmt.Errorf("%s at %s: %s: %w", label, w.where(n), detail, ErrWrongType)})
}

// where names the position of n: source, with the line and column when n
// came from a file.
func (w *keyWalker) where(n *yaml.Node) string {
	if n.Line > 0 {
		return fmt.Sprintf("%s:%d:%d", w.source, n.Line, n.Column)
	}
	return w.source
}

func (w *keyWalker) walkStruct(n *yaml.Node, t reflect.Type, path string, scope *overrideScope) {
	fields, inlineMap := yamlFields(t)
	w.eachPair(n, t, path, func(key, val *yaml.Node) {
		name := keyName(key)
		f, ok := fields[name]
		switch {
		case ok:
		case inlineMap != nil:
			f = inlineMap.Elem()
		default:
			if w.checkKeys {
				w.reportUnknown(key, path, fields, scope)
			}
			return
		}
		var childScope *overrideScope
		if name == "tenant_overrides" && ok && f.Kind() == reflect.Map {
			childScope = &overrideScope{global: t, path: path}
		}
		w.walk(val, f, joinPath(path, name), childScope)
	})
}

// eachPair calls fn for every key/value pair of a mapping node, expanding
// "<<" merge keys (a mapping, an alias to one, or a sequence of those) as
// yaml.v3 does when decoding, so merged-in keys are checked against t too.
func (w *keyWalker) eachPair(n *yaml.Node, t reflect.Type, path string, fn func(key, val *yaml.Node)) {
	if n.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, val := n.Content[i], n.Content[i+1]
		if isMergeKey(key) {
			w.walkMerge(val, t, path, fn)
			continue
		}
		fn(key, val)
	}
}

func (w *keyWalker) walkMerge(val *yaml.Node, t reflect.Type, path string, fn func(key, val *yaml.Node)) {
	switch val.Kind {
	case yaml.AliasNode:
		w.walkMerge(val.Alias, t, path, fn)
	case yaml.SequenceNode:
		for _, item := range val.Content {
			w.walkMerge(item, t, path, fn)
		}
	case yaml.MappingNode:
		v := visit{node: val, typ: t}
		if w.visited[v] {
			return
		}
		w.visited[v] = true
		w.eachPair(val, t, path, fn)
	default:
		// A scalar merge value is invalid YAML merge syntax; the decoder
		// rejects it with its own error.
	}
}

func (w *keyWalker) reportUnknown(key *yaml.Node, parent string, fields map[string]reflect.Type, scope *overrideScope) {
	path := mapKeyPath(parent, key.Value)
	where := w.where(key)
	valid := slices.Sorted(maps.Keys(fields))

	var advice string
	switch {
	case removedKeys[path] != "":
		advice = "it was removed: " + removedKeys[path] + ". Delete it."
	case scope != nil && key.Value != "tenant_overrides" && globalField(scope.global, key.Value):
		advice = fmt.Sprintf("%s cannot be overridden per tenant; set %s instead, or use one of the overridable keys: %s",
			key.Value, joinPath(scope.path, key.Value), strings.Join(valid, ", "))
	default:
		advice = "remove it or correct the spelling"
		if s := closestKey(key.Value, valid); s != "" {
			advice = fmt.Sprintf("did you mean %q?", s)
		}
		section := "valid top-level keys"
		if parent != "" {
			section = "valid keys under " + parent
		}
		advice += fmt.Sprintf(" (%s: %s)", section, strings.Join(valid, ", "))
	}
	w.errs = append(w.errs, &FieldError{Path: path, Source: w.source,
		Err: fmt.Errorf("%w %q at %s: %s", ErrUnknownKey, path, where, advice)})
}

// yamlFields returns the YAML key → field type map yaml.v3 decodes struct t
// with: the tag name, or the lowercased field name when untagged; "-" and
// unexported fields are not keys; an ",inline" struct or pointer to a struct
// contributes its fields, and an ",inline" map (returned as inlineMap)
// accepts every other key.
func yamlFields(t reflect.Type) (fields map[string]reflect.Type, inlineMap reflect.Type) {
	fields = make(map[string]reflect.Type, t.NumField())
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("yaml")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if slices.Contains(strings.Split(opts, ","), "inline") {
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			switch {
			case f.Type.Kind() == reflect.Map:
				inlineMap = f.Type
			case ft.Kind() == reflect.Struct:
				// yaml.v3 inlines a struct and a pointer to one alike,
				// allocating the pointer when it decodes an inlined key.
				inner, innerMap := yamlFields(ft)
				maps.Copy(fields, inner)
				if innerMap != nil {
					inlineMap = innerMap
				}
			default:
				// yaml.v3 rejects ",inline" on any other kind (including a
				// pointer to a map) when decoding.
			}
			continue
		}
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		fields[name] = f.Type
	}
	return fields, inlineMap
}

// keyName returns the field name yaml.v3 matches mapping key against: the key
// decoded as a string, which differs from its text for a !!binary key. A key
// that does not decode to a string is returned as written; it names no
// field either way.
func keyName(key *yaml.Node) string {
	var name string
	if key.Decode(&name) != nil {
		return key.Value
	}
	return name
}

func globalField(t reflect.Type, key string) bool {
	fields, _ := yamlFields(t)
	_, ok := fields[key]
	return ok
}

func joinPath(parent, key string) string {
	if parent == "" {
		return key
	}
	return parent + "." + key
}

// closestKey returns the candidate nearest to key by edit distance, if it is
// within maxSuggestionDistance; ties go to the alphabetically first candidate.
func closestKey(key string, candidates []string) string {
	best, bestDist := "", maxSuggestionDistance+1
	for _, c := range candidates {
		if d := editDistance(key, c); d < bestDist {
			best, bestDist = c, d
		}
	}
	return best
}

// editDistance is the Levenshtein distance between a and b, counted in bytes
// (config keys are ASCII).
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
