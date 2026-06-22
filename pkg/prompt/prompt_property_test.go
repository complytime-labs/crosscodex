//go:build !integration

package prompt_test

import (
	"encoding/json"
	"regexp"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/prompt"
	"github.com/complytime-labs/crosscodex/pkg/storage"
)

// Suite bootstrap lives in prompt_bdd_test.go.

var _ = Describe("Property Specifications", Ordered, func() {
	Context("substitutePlaceholders — roundtrip", func() {
		It("templates with N placeholders and N matching vars produce no ${...} remnants", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				// Generate 1-5 variable names
				n := rapid.IntRange(1, 5).Draw(t, "numVars")
				vars := make(map[string]string)
				template := ""
				for i := 0; i < n; i++ {
					name := rapid.StringMatching(`[a-z][a-z0-9_]{0,9}`).Draw(t, "varName")
					value := rapid.String().Draw(t, "varValue")
					vars[name] = value
					template += "prefix ${" + name + "} "
				}

				result, err := prompt.ExportSubstitutePlaceholders(template, vars)
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}

				re := regexp.MustCompile(`\$\{[a-zA-Z_][a-zA-Z0-9_]*\}`)
				if re.MatchString(result) {
					t.Fatalf("unresolved placeholder in result: %q", result)
				}
			})
		})
	})

	Context("ContentHash — determinism", func() {
		It("same messages always produce identical hash", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				role := rapid.SampledFrom([]string{"system", "user", "assistant"}).Draw(t, "role")
				content := rapid.String().Draw(t, "content")
				msgs := []prompt.Message{{Role: role, Content: content}}

				bytes1, _ := json.Marshal(msgs)
				bytes2, _ := json.Marshal(msgs)

				hash1 := storage.ContentHash(bytes1)
				hash2 := storage.ContentHash(bytes2)

				if hash1 != hash2 {
					t.Fatalf("hash mismatch: %s != %s", hash1, hash2)
				}
			})
		})
	})

	Context("mergeSpecs — idempotency", func() {
		It("merging a spec with itself produces identical spec", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				name := rapid.StringMatching(`[a-z][a-z0-9-]{2,10}`).Draw(t, "name")
				version := rapid.StringMatching(`\d+\.\d+\.\d+`).Draw(t, "version")
				system := rapid.String().Draw(t, "system")

				spec := &prompt.PromptSpec{
					Name:    name,
					Version: version,
					Templates: prompt.TemplateSet{
						System: system,
					},
				}

				result, err := prompt.ExportMergeSpecs(spec, spec, "replace")
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if result.Name != spec.Name || result.Version != spec.Version || result.Templates.System != spec.Templates.System {
					t.Fatalf("idempotency violated: result differs from input")
				}
			})
		})
	})

	Context("mergeSpecs — overlay wins", func() {
		It("overlay values always appear in merged result for non-empty fields", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				name := rapid.StringMatching(`[a-z][a-z0-9-]{2,10}`).Draw(t, "name")
				baseVersion := rapid.StringMatching(`\d+\.\d+\.\d+`).Draw(t, "baseVer")
				overlayVersion := rapid.StringMatching(`\d+\.\d+\.\d+`).Draw(t, "overlayVer")
				baseSystem := rapid.StringMatching(`[a-z]{1,20}`).Draw(t, "baseSys")
				overlaySystem := rapid.StringMatching(`[a-z]{1,20}`).Draw(t, "overlaySys")

				base := &prompt.PromptSpec{
					Name:    name,
					Version: baseVersion,
					Templates: prompt.TemplateSet{
						System: baseSystem,
					},
				}
				overlay := &prompt.PromptSpec{
					Name:    name,
					Version: overlayVersion,
					Templates: prompt.TemplateSet{
						System: overlaySystem,
					},
				}

				result, err := prompt.ExportMergeSpecs(base, overlay, "replace")
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if result.Version != overlayVersion {
					t.Fatalf("overlay version not applied: got %q, want %q", result.Version, overlayVersion)
				}
				if result.Templates.System != overlaySystem {
					t.Fatalf("overlay system not applied: got %q, want %q", result.Templates.System, overlaySystem)
				}
			})
		})
	})

	Context("copySpec — roundtrip", func() {
		It("copy is deeply equal to original", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				name := rapid.StringMatching(`[a-z][a-z0-9-]{2,10}`).Draw(t, "name")
				version := rapid.StringMatching(`\d+\.\d+\.\d+`).Draw(t, "version")
				system := rapid.String().Draw(t, "system")
				user := rapid.String().Draw(t, "user")

				spec := &prompt.PromptSpec{
					Name:    name,
					Version: version,
					Templates: prompt.TemplateSet{
						System: system,
						User:   user,
					},
					FewShot: []prompt.FewShotExample{
						{Input: rapid.String().Draw(t, "input"), Output: rapid.String().Draw(t, "output")},
					},
					Metadata: map[string]string{
						rapid.StringMatching(`[a-z]{1,5}`).Draw(t, "metaKey"): rapid.String().Draw(t, "metaVal"),
					},
				}

				cp := prompt.ExportCopySpec(spec)

				if cp.Name != spec.Name || cp.Version != spec.Version {
					t.Fatalf("copy name/version mismatch")
				}
				if cp.Templates.System != spec.Templates.System || cp.Templates.User != spec.Templates.User {
					t.Fatalf("copy templates mismatch")
				}
				if len(cp.FewShot) != len(spec.FewShot) {
					t.Fatalf("copy fewshot length mismatch")
				}
				if cp.FewShot[0].Input != spec.FewShot[0].Input {
					t.Fatalf("copy fewshot content mismatch")
				}
			})
		})
	})

	Context("substitutePlaceholders — preserves non-placeholder text", func() {
		It("text without ${...} patterns remains unchanged after substitution", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				// Generate text guaranteed to have no ${ patterns
				text := rapid.StringMatching(`[a-zA-Z0-9 ,.!?]{1,100}`).Draw(t, "text")

				result, err := prompt.ExportSubstitutePlaceholders(text, nil)
				if err != nil {
					t.Fatalf("unexpected error for text without placeholders: %v", err)
				}
				if result != text {
					t.Fatalf("non-placeholder text was modified: %q -> %q", text, result)
				}
			})
		})
	})
})

// Ensure Gomega dot-import is used (framework requirement).
var _ = Expect
