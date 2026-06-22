package prompt_test

import (
	"testing"

	"github.com/complytime-labs/crosscodex/pkg/prompt"
)

func FuzzParsePromptSpec(f *testing.F) {
	f.Add([]byte(`name: test
version: 1.0.0
templates:
  system: hello`))
	f.Add([]byte(""))
	f.Add([]byte("{{invalid yaml"))
	f.Add([]byte("name: \x00null-byte"))
	f.Add([]byte(`name: test
few_shot_examples:
  - input: a
    output: b`))
	f.Add([]byte(`version: 1.0.0`))   // missing name
	f.Add([]byte("name: x\nname: y")) // duplicate key
	f.Add([]byte(`name: test
metadata:
  k1: v1
  k2: v2`))
	f.Add([]byte("name: \u65e5\u672c\u8a9e\nversion: 1.0.0"))
	f.Add([]byte("---\nname: test\n---\nname: test2"))

	f.Fuzz(func(t *testing.T, data []byte) {
		// Must not panic regardless of input
		_, _ = prompt.ExportParsePromptSpec(data)
	})
}

func FuzzSubstitutePlaceholders(f *testing.F) {
	f.Add("Hello ${name}", "name=Alice")
	f.Add("No placeholders", "")
	f.Add("${a}${b}${c}", "a=1,b=2,c=3")
	f.Add("${}", "")
	f.Add("$${name}", "name=test")
	f.Add("${invalid-name}", "invalid-name=val")
	f.Add("${_underscore}", "_underscore=ok")
	f.Add("${a${b}}", "a=1,b=2")
	f.Add("${"+string([]byte{0x00})+"}", "")
	f.Add("${\u540d\u524d}", "\u540d\u524d=value")

	f.Fuzz(func(t *testing.T, template, varsStr string) {
		// Parse vars from comma-separated k=v pairs
		vars := make(map[string]string)
		if varsStr != "" {
			for _, kv := range splitVars(varsStr) {
				parts := splitOnFirst(kv, '=')
				if len(parts) == 2 {
					vars[parts[0]] = parts[1]
				}
			}
		}
		// Must not panic regardless of input
		_, _ = prompt.ExportSubstitutePlaceholders(template, vars)
	})
}

func splitVars(s string) []string {
	var result []string
	current := ""
	for _, c := range s {
		if c == ',' {
			result = append(result, current)
			current = ""
		} else {
			current += string(c)
		}
	}
	if current != "" {
		result = append(result, current)
	}
	return result
}

func splitOnFirst(s string, sep byte) []string {
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			return []string{s[:i], s[i+1:]}
		}
	}
	return []string{s}
}

func FuzzLoadFewShotFile(f *testing.F) {
	f.Add("file:shots.json")
	f.Add("file:../../../etc/passwd")
	f.Add("file:sub/../../../etc/shadow")
	f.Add("/etc/passwd")
	f.Add("file:/absolute/path")
	f.Add("file:" + string([]byte{0x00}) + "null")
	f.Add("file:very/deep/nested/path/to/file.json")
	f.Add("file:.")
	f.Add("file:..")
	f.Add("file:symlink.json")

	f.Fuzz(func(t *testing.T, ref string) {
		tmpDir := t.TempDir()
		// Must not panic regardless of input
		_, _ = prompt.ExportLoadFewShotFile(tmpDir, ref)
	})
}

func FuzzMergeSpecs(f *testing.F) {
	f.Add("name1", "1.0.0", "sys1", "name1", "2.0.0", "sys2", "replace")
	f.Add("name1", "1.0.0", "sys1", "name1", "2.0.0", "sys2", "append")
	f.Add("name1", "1.0.0", "sys1", "name1", "2.0.0", "sys2", "deep_copy")
	f.Add("alpha", "", "", "beta", "", "", "replace")
	f.Add("", "", "", "", "", "", "replace")
	f.Add("name", "1.0.0", "", "name", "", "override", "")
	f.Add("a", "1.0.0", "s", "a", "2.0.0", "s", "unknown_strategy")
	f.Add("test", "1.0.0", "sys", "test", "1.0.0", "sys", "replace")
	f.Add("x", "0.0.1", "a", "x", "99.99.99", "b", "append")
	f.Add("name", "v1", string([]byte{0x00}), "name", "v2", string([]byte{0x00}), "replace")

	f.Fuzz(func(t *testing.T, baseName, baseVer, baseSys, overlayName, overlayVer, overlaySys, strategy string) {
		base := &prompt.PromptSpec{
			Name:      baseName,
			Version:   baseVer,
			Templates: prompt.TemplateSet{System: baseSys},
		}
		overlay := &prompt.PromptSpec{
			Name:      overlayName,
			Version:   overlayVer,
			Templates: prompt.TemplateSet{System: overlaySys},
		}
		// Must not panic regardless of input
		_, _ = prompt.ExportMergeSpecs(base, overlay, strategy)
	})
}
