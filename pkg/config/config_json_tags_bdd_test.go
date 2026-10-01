package config_test

import (
	"fmt"
	"reflect"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

var _ = Describe("Config JSON/YAML Tag Consistency", func() {
	// Asserts that every field reachable from Config serializes to the same
	// key under both encoders, so `config show --json` produces the same
	// top-level and nested keys as the YAML output (issue #116).
	It("has matching json and yaml tags on every field", func() {
		violations := checkTagConsistency(reflect.TypeOf(config.Config{}))
		Expect(violations).To(BeEmpty())
	})

	// Proves the checker itself fires on every way the two encoders can
	// diverge. Without these negative cases the invariant test above is a
	// tautology: a checker that never reports a violation would pass
	// against a fully-tagged Config while silently permitting a future
	// untagged field to reintroduce issue #116.
	DescribeTable("checkTagConsistency violation detection",
		func(typ reflect.Type, wantCount int, wantMatch string) {
			got := checkTagConsistency(typ)
			Expect(got).To(HaveLen(wantCount), "violations: %v", got)
			if wantMatch != "" {
				Expect(got[0]).To(ContainSubstring(wantMatch))
			}
		},
		Entry("matched keys pass", reflect.TypeOf(matched{}), 0, ""),
		Entry("yaml:- json:- pass", reflect.TypeOf(excluded{}), 0, ""),
		Entry("unexported field skipped", reflect.TypeOf(unexportedSkipped{}), 0, ""),
		Entry("yaml without json", reflect.TypeOf(missingJSON{}), 1, "no json tag"),
		Entry("json without yaml", reflect.TypeOf(missingYAML{}), 1, "no yaml tag"),
		Entry("neither tag", reflect.TypeOf(neither{}), 1, "neither"),
		Entry("mismatched keys", reflect.TypeOf(mismatched{}), 1, "does not match"),
		Entry("violation in nested struct", reflect.TypeOf(nestedBad{}), 1, "Inner.A"),
	)
})

type matched struct {
	A string `yaml:"a" json:"a"`
}
type excluded struct {
	A string `yaml:"-" json:"-"`
}
type unexportedSkipped struct {
	a string //nolint:unused // exercised via reflection, must be ignored
	B string `yaml:"b" json:"b"`
}
type missingJSON struct {
	A string `yaml:"a"`
}
type missingYAML struct {
	A string `json:"a"`
}
type neither struct {
	A string
}
type mismatched struct {
	A string `yaml:"a" json:"b"`
}
type nestedBad struct {
	Inner missingJSON `yaml:"inner" json:"inner"`
}

// checkTagConsistency walks the exported field tree rooted at rt and returns one
// message per field whose yaml and json struct tags would serialize to
// different keys. A serializable field must carry both a yaml and a json tag
// with identical keys (yaml:"-" pairs with json:"-"). A field carrying neither
// tag is a violation too: gopkg.in/yaml.v3 emits the lowercased Go field name
// while encoding/json emits the PascalCase name -- the exact key divergence
// issue #116 fixed. Unexported fields are ignored; recursion into pointers,
// slices, arrays, and maps follows their element type, and each struct type is
// visited once.
func checkTagConsistency(root reflect.Type) []string {
	var violations []string
	seen := make(map[reflect.Type]bool)
	var walk func(rt reflect.Type, path string)
	walk = func(rt reflect.Type, path string) {
		for rt.Kind() == reflect.Pointer || rt.Kind() == reflect.Slice ||
			rt.Kind() == reflect.Array || rt.Kind() == reflect.Map {
			rt = rt.Elem()
		}
		if rt.Kind() != reflect.Struct || seen[rt] {
			return
		}
		seen[rt] = true

		for i := range rt.NumField() {
			f := rt.Field(i)
			if f.PkgPath != "" { // unexported
				continue
			}
			fieldPath := path + "." + f.Name
			yamlKey, hasYAML := tagKey(f, "yaml")
			jsonKey, hasJSON := tagKey(f, "json")
			switch {
			case !hasYAML && !hasJSON:
				violations = append(violations,
					fmt.Sprintf("%s: exported field has neither a yaml nor a json tag; the two encoders would serialize it under different keys", fieldPath))
			case hasYAML && !hasJSON:
				violations = append(violations,
					fmt.Sprintf("%s: has yaml tag %q but no json tag", fieldPath, yamlKey))
			case hasJSON && !hasYAML:
				violations = append(violations,
					fmt.Sprintf("%s: has json tag %q but no yaml tag", fieldPath, jsonKey))
			case jsonKey != yamlKey:
				violations = append(violations,
					fmt.Sprintf("%s: json key %q does not match yaml key %q", fieldPath, jsonKey, yamlKey))
			}
			walk(f.Type, fieldPath)
		}
	}
	walk(root, root.Name())
	return violations
}

// tagKey returns the tag's key (the part before any comma-separated options)
// and whether the tag is present.
func tagKey(f reflect.StructField, name string) (string, bool) {
	raw, ok := f.Tag.Lookup(name)
	if !ok {
		return "", false
	}
	if comma := strings.IndexByte(raw, ','); comma >= 0 {
		raw = raw[:comma]
	}
	return raw, true
}
