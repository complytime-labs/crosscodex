//go:build !integration

package prompt_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/prompt"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
)

func TestPromptBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Prompt System BDD Suite")
}

var _ = BeforeSuite(func() { DeferCleanup(testspecs.RedirectLogsToGinkgo()) })

var _ = Describe("ParsePromptSpec", func() {
	Context("with a valid YAML spec", func() {
		It("parses all fields correctly", func() {
			yaml := `
name: test-prompt
version: 1.0.0
metadata:
  domain: testing
templates:
  system: "System template ${var1}"
  user: "User template ${var2}"
few_shot_examples:
  - input: "example input"
    output: "example output"
`
			spec, err := prompt.ExportParsePromptSpec([]byte(yaml))
			Expect(err).NotTo(HaveOccurred())
			Expect(spec.Name).To(Equal("test-prompt"))
			Expect(spec.Version).To(Equal("1.0.0"))
			Expect(spec.Templates.System).To(ContainSubstring("${var1}"))
			Expect(spec.Templates.User).To(ContainSubstring("${var2}"))
			Expect(spec.FewShot).To(HaveLen(1))
			Expect(spec.FewShot[0].Input).To(Equal("example input"))
			Expect(spec.Metadata).To(HaveKeyWithValue("domain", "testing"))
		})
	})

	Context("with a missing name", func() {
		It("returns ErrInvalidPromptSpec", func() {
			yaml := `
version: 1.0.0
templates:
  system: "hello"
`
			_, err := prompt.ExportParsePromptSpec([]byte(yaml))
			Expect(err).To(MatchError(prompt.ErrInvalidPromptSpec))
		})
	})

	Context("with malformed YAML", func() {
		It("returns an error", func() {
			_, err := prompt.ExportParsePromptSpec([]byte("{{invalid"))
			Expect(err).To(HaveOccurred())
		})
	})

	Context("with unknown fields", func() {
		It("ignores them without error", func() {
			yaml := `
name: test
version: 1.0.0
unknown_field: "ignored"
templates:
  system: "hello"
`
			spec, err := prompt.ExportParsePromptSpec([]byte(yaml))
			Expect(err).NotTo(HaveOccurred())
			Expect(spec.Name).To(Equal("test"))
		})
	})

	Context("with few-shot source field", func() {
		It("parses the source reference", func() {
			yaml := `
name: test
version: 1.0.0
few_shot_examples:
  - source: "file:examples/shots.json"
`
			spec, err := prompt.ExportParsePromptSpec([]byte(yaml))
			Expect(err).NotTo(HaveOccurred())
			Expect(spec.FewShot).To(HaveLen(1))
			Expect(spec.FewShot[0].Source).To(Equal("file:examples/shots.json"))
		})
	})
})

var _ = Describe("SubstitutePlaceholders", func() {
	Context("with all variables provided", func() {
		It("replaces all placeholders", func() {
			result, err := prompt.ExportSubstitutePlaceholders(
				"Hello ${name}, welcome to ${place}.",
				map[string]string{"name": "Alice", "place": "Wonderland"},
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal("Hello Alice, welcome to Wonderland."))
		})
	})

	Context("with a missing variable", func() {
		It("returns ErrMissingPlaceholder", func() {
			_, err := prompt.ExportSubstitutePlaceholders(
				"Hello ${name}, welcome to ${place}.",
				map[string]string{"name": "Alice"},
			)
			Expect(err).To(MatchError(prompt.ErrMissingPlaceholder))
			Expect(err.Error()).To(ContainSubstring("place"))
		})
	})

	Context("with extra variables", func() {
		It("ignores unused variables", func() {
			result, err := prompt.ExportSubstitutePlaceholders(
				"Hello ${name}.",
				map[string]string{"name": "Alice", "unused": "ignored"},
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal("Hello Alice."))
		})
	})

	Context("with no placeholders", func() {
		It("returns the template unchanged", func() {
			result, err := prompt.ExportSubstitutePlaceholders(
				"No placeholders here.",
				map[string]string{"name": "Alice"},
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal("No placeholders here."))
		})
	})

	Context("with empty string as variable value", func() {
		It("substitutes with empty string", func() {
			result, err := prompt.ExportSubstitutePlaceholders(
				"Value: ${val}.",
				map[string]string{"val": ""},
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal("Value: ."))
		})
	})

	Context("with no recursive expansion", func() {
		It("does not expand placeholders in variable values", func() {
			result, err := prompt.ExportSubstitutePlaceholders(
				"${a}",
				map[string]string{"a": "${b}", "b": "recursive"},
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal("${b}"))
		})
	})
})

var _ = Describe("AssembleMessages", func() {
	Context("with system, few-shot, and user content", func() {
		It("assembles messages in correct order", func() {
			msgs := prompt.ExportAssembleMessages(
				"System prompt",
				"User query",
				[]prompt.FewShotExample{
					{Input: "example input", Output: "example output"},
				},
			)
			Expect(msgs).To(HaveLen(4))
			Expect(msgs[0].Role).To(Equal("system"))
			Expect(msgs[0].Content).To(Equal("System prompt"))
			Expect(msgs[1].Role).To(Equal("user"))
			Expect(msgs[1].Content).To(Equal("example input"))
			Expect(msgs[2].Role).To(Equal("assistant"))
			Expect(msgs[2].Content).To(Equal("example output"))
			Expect(msgs[3].Role).To(Equal("user"))
			Expect(msgs[3].Content).To(Equal("User query"))
		})
	})

	Context("with empty system template", func() {
		It("omits the system message", func() {
			msgs := prompt.ExportAssembleMessages("", "User query", nil)
			Expect(msgs).To(HaveLen(1))
			Expect(msgs[0].Role).To(Equal("user"))
		})
	})

	Context("with empty user template", func() {
		It("omits the user message", func() {
			msgs := prompt.ExportAssembleMessages("System prompt", "", nil)
			Expect(msgs).To(HaveLen(1))
			Expect(msgs[0].Role).To(Equal("system"))
		})
	})

	Context("with no content at all", func() {
		It("returns empty slice", func() {
			msgs := prompt.ExportAssembleMessages("", "", nil)
			Expect(msgs).To(BeEmpty())
		})
	})
})

var _ = Describe("Embedded Defaults", func() {
	It("loads all three default prompts", func() {
		specs, err := prompt.ExportLoadEmbeddedDefaults()
		Expect(err).NotTo(HaveOccurred())
		Expect(specs).To(HaveKey("section-detect"))
		Expect(specs).To(HaveKey("structured-extract"))
		Expect(specs).To(HaveKey("enrichment"))
		Expect(specs["section-detect"].Version).To(Equal("1.0.0"))
		Expect(specs["section-detect"].Templates.System).NotTo(BeEmpty())
		Expect(specs["section-detect"].Templates.User).To(Equal("${document_chunk}"))
	})
})

var _ = Describe("MergeSpecs", func() {
	Context("with default merge (replace slices)", func() {
		It("overlays higher-priority fields", func() {
			base := &prompt.PromptSpec{
				Name:    "test",
				Version: "1.0.0",
				Templates: prompt.TemplateSet{
					System: "base system",
					User:   "base user",
				},
				FewShot: []prompt.FewShotExample{
					{Input: "base-in", Output: "base-out"},
				},
				Metadata: map[string]string{"key1": "val1", "key2": "val2"},
			}
			overlay := &prompt.PromptSpec{
				Name:    "test",
				Version: "2.0.0",
				Templates: prompt.TemplateSet{
					System: "overlay system",
				},
				FewShot: []prompt.FewShotExample{
					{Input: "overlay-in", Output: "overlay-out"},
				},
				Metadata: map[string]string{"key2": "override"},
			}
			result, err := prompt.ExportMergeSpecs(base, overlay, "replace")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Version).To(Equal("2.0.0"))
			Expect(result.Templates.System).To(Equal("overlay system"))
			Expect(result.Templates.User).To(Equal("base user"))
			Expect(result.FewShot).To(HaveLen(1))
			Expect(result.FewShot[0].Input).To(Equal("overlay-in"))
			Expect(result.Metadata).To(HaveKeyWithValue("key1", "val1"))
			Expect(result.Metadata).To(HaveKeyWithValue("key2", "override"))
		})
	})

	Context("with append slice strategy", func() {
		It("appends few-shot examples", func() {
			base := &prompt.PromptSpec{
				Name: "test",
				FewShot: []prompt.FewShotExample{
					{Input: "base", Output: "base"},
				},
			}
			overlay := &prompt.PromptSpec{
				Name: "test",
				FewShot: []prompt.FewShotExample{
					{Input: "new", Output: "new"},
				},
			}
			result, err := prompt.ExportMergeSpecs(base, overlay, "append")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.FewShot).To(HaveLen(2))
			Expect(result.FewShot[0].Input).To(Equal("base"))
			Expect(result.FewShot[1].Input).To(Equal("new"))
		})
	})

	Context("with name mismatch", func() {
		It("returns ErrLayerConflict", func() {
			base := &prompt.PromptSpec{Name: "alpha"}
			overlay := &prompt.PromptSpec{Name: "beta"}
			_, err := prompt.ExportMergeSpecs(base, overlay, "replace")
			Expect(err).To(MatchError(prompt.ErrLayerConflict))
		})
	})
})

var _ = Describe("Registry", func() {
	var (
		cfg config.PromptConfig
		reg prompt.Registry
	)

	BeforeEach(func() {
		cfg = config.PromptConfig{
			CaptureContent: true,
			AllowCommands:  false,
			Layers: config.PromptLayerConfig{
				Enabled: true,
			},
		}
	})

	Context("with only embedded defaults", func() {
		BeforeEach(func() {
			var err error
			reg, err = prompt.NewRegistry(cfg)
			Expect(err).NotTo(HaveOccurred())
		})

		It("lists all embedded prompts", func() {
			names, err := reg.List(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(names).To(ContainElement("section-detect"))
			Expect(names).To(ContainElement("structured-extract"))
			Expect(names).To(ContainElement("enrichment"))
		})

		It("resolves an embedded prompt", func() {
			spec, err := reg.Resolve(context.Background(), "section-detect")
			Expect(err).NotTo(HaveOccurred())
			Expect(spec.Name).To(Equal("section-detect"))
			Expect(spec.Version).To(Equal("1.0.0"))
			Expect(spec.Templates.System).NotTo(BeEmpty())
		})

		It("returns ErrPromptNotFound for unknown prompts", func() {
			_, err := reg.Resolve(context.Background(), "nonexistent")
			Expect(err).To(MatchError(prompt.ErrPromptNotFound))
		})

		It("renders a prompt with variables", func() {
			resolved, err := reg.Render(context.Background(), "section-detect",
				map[string]string{"document_chunk": "test content"})
			Expect(err).NotTo(HaveOccurred())
			Expect(resolved.Name).To(Equal("section-detect"))
			Expect(resolved.ContentHash).NotTo(BeEmpty())
			Expect(resolved.Messages).To(HaveLen(2)) // system + user
			Expect(resolved.Messages[0].Role).To(Equal("system"))
			Expect(resolved.Messages[1].Role).To(Equal("user"))
			Expect(resolved.Messages[1].Content).To(Equal("test content"))
		})

		It("returns ErrMissingPlaceholder when vars are incomplete", func() {
			_, err := reg.Render(context.Background(), "section-detect", nil)
			Expect(err).To(MatchError(prompt.ErrMissingPlaceholder))
		})
	})

	Context("with a project overlay directory", func() {
		var tmpDir string

		BeforeEach(func() {
			var err error
			tmpDir, err = os.MkdirTemp("", "prompt-test-*")
			Expect(err).NotTo(HaveOccurred())

			// Create a project overlay that overrides section-detect
			promptDir := filepath.Join(tmpDir, ".crosscodex", "prompts")
			Expect(os.MkdirAll(promptDir, 0o755)).To(Succeed())

			overlayYAML := `
name: section-detect
version: 2.0.0
templates:
  system: "Overridden system prompt"
`
			Expect(os.WriteFile(filepath.Join(promptDir, "section_detect.yaml"),
				[]byte(overlayYAML), 0o644)).To(Succeed())

			reg, err = prompt.NewRegistry(cfg, prompt.WithProjectDir(tmpDir))
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			os.RemoveAll(tmpDir)
		})

		It("merges project overlay on top of embedded defaults", func() {
			spec, err := reg.Resolve(context.Background(), "section-detect")
			Expect(err).NotTo(HaveOccurred())
			Expect(spec.Version).To(Equal("2.0.0"))
			Expect(spec.Templates.System).To(Equal("Overridden system prompt"))
		})

		It("reports layers contributing to the prompt", func() {
			layers, err := reg.Layers(context.Background(), "section-detect")
			Expect(err).NotTo(HaveOccurred())
			Expect(len(layers)).To(BeNumerically(">=", 2))

			embeddedFound := false
			projectFound := false
			for _, l := range layers {
				if l.ID == "embedded" && l.HasPrompt {
					embeddedFound = true
				}
				if l.ID == "project" && l.HasPrompt {
					projectFound = true
				}
			}
			Expect(embeddedFound).To(BeTrue())
			Expect(projectFound).To(BeTrue())
		})
	})

	Context("with layers disabled", func() {
		It("returns no prompts when enabled is false and order is empty", func() {
			cfg.Layers.Enabled = false
			var err error
			reg, err = prompt.NewRegistry(cfg)
			Expect(err).NotTo(HaveOccurred())

			names, err := reg.List(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(names).To(BeEmpty())
		})

		It("loads only explicitly listed layers when enabled is false with non-empty order", func() {
			cfg.Layers.Enabled = false
			cfg.Layers.Order = []config.PromptLayerEntry{
				{ID: "embedded"},
			}
			var err error
			reg, err = prompt.NewRegistry(cfg)
			Expect(err).NotTo(HaveOccurred())

			names, err := reg.List(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(names).To(ContainElement("section-detect"))
			Expect(names).To(ContainElement("enrichment"))
		})
	})

	Context("with replace merge mode on a layer", func() {
		var tmpDir string

		BeforeEach(func() {
			var err error
			tmpDir, err = os.MkdirTemp("", "prompt-replace-*")
			Expect(err).NotTo(HaveOccurred())

			promptDir := filepath.Join(tmpDir, ".crosscodex", "prompts")
			Expect(os.MkdirAll(promptDir, 0o755)).To(Succeed())

			// Replacement overlay: only provides system template, no few-shot
			overlayYAML := `
name: section-detect
version: 3.0.0
templates:
  system: "Replacement only"
`
			Expect(os.WriteFile(filepath.Join(promptDir, "section_detect.yaml"),
				[]byte(overlayYAML), 0o644)).To(Succeed())

			cfg.Layers.Order = []config.PromptLayerEntry{
				{ID: "embedded"},
				{ID: "project", Merge: "replace"},
			}
			reg, err = prompt.NewRegistry(cfg, prompt.WithProjectDir(tmpDir))
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			os.RemoveAll(tmpDir)
		})

		It("replaces the entire spec from the replacing layer", func() {
			spec, err := reg.Resolve(context.Background(), "section-detect")
			Expect(err).NotTo(HaveOccurred())
			Expect(spec.Version).To(Equal("3.0.0"))
			Expect(spec.Templates.System).To(Equal("Replacement only"))
			// User template should be empty because replace discards base
			Expect(spec.Templates.User).To(BeEmpty())
		})
	})

	Context("with WithCLILayers option", func() {
		var tmpDir string

		BeforeEach(func() {
			var err error
			tmpDir, err = os.MkdirTemp("", "prompt-cli-*")
			Expect(err).NotTo(HaveOccurred())

			cliYAML := `
name: cli-prompt
version: 1.0.0
templates:
  system: "CLI-provided system prompt"
  user: "${query}"
`
			Expect(os.WriteFile(filepath.Join(tmpDir, "cli.yaml"),
				[]byte(cliYAML), 0o644)).To(Succeed())

			reg, err = prompt.NewRegistry(cfg, prompt.WithCLILayers(tmpDir))
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			os.RemoveAll(tmpDir)
		})

		It("includes prompts from CLI layer directories", func() {
			names, err := reg.List(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(names).To(ContainElement("cli-prompt"))
		})

		It("renders CLI-provided prompts", func() {
			resolved, err := reg.Render(context.Background(), "cli-prompt",
				map[string]string{"query": "test"})
			Expect(err).NotTo(HaveOccurred())
			Expect(resolved.Messages[0].Content).To(Equal("CLI-provided system prompt"))
		})
	})

	Context("with LayerPaths config", func() {
		var tmpDir string

		BeforeEach(func() {
			var err error
			tmpDir, err = os.MkdirTemp("", "prompt-layerpath-*")
			Expect(err).NotTo(HaveOccurred())

			customYAML := `
name: custom-layer-prompt
version: 1.0.0
templates:
  system: "From custom layer path"
`
			Expect(os.WriteFile(filepath.Join(tmpDir, "custom.yaml"),
				[]byte(customYAML), 0o644)).To(Succeed())

			cfg.LayerPaths = []string{tmpDir}
			reg, err = prompt.NewRegistry(cfg)
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			os.RemoveAll(tmpDir)
		})

		It("includes prompts from custom layer paths", func() {
			names, err := reg.List(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(names).To(ContainElement("custom-layer-prompt"))
		})
	})

	Context("cache copy-safety", func() {
		BeforeEach(func() {
			var err error
			reg, err = prompt.NewRegistry(cfg)
			Expect(err).NotTo(HaveOccurred())
		})

		It("does not corrupt the cache when returned spec is modified", func() {
			spec1, err := reg.Resolve(context.Background(), "section-detect")
			Expect(err).NotTo(HaveOccurred())
			originalVersion := spec1.Version
			originalSystem := spec1.Templates.System

			// Mutate the returned spec
			spec1.Version = "MUTATED"
			spec1.Templates.System = "MUTATED SYSTEM"
			if spec1.Metadata == nil {
				spec1.Metadata = make(map[string]string)
			}
			spec1.Metadata["injected"] = "value"

			// Second resolve should return the original, uncorrupted values
			spec2, err := reg.Resolve(context.Background(), "section-detect")
			Expect(err).NotTo(HaveOccurred())
			Expect(spec2.Version).To(Equal(originalVersion))
			Expect(spec2.Templates.System).To(Equal(originalSystem))
			Expect(spec2.Metadata).NotTo(HaveKey("injected"))
		})
	})

	Context("concurrent access", func() {
		BeforeEach(func() {
			var err error
			reg, err = prompt.NewRegistry(cfg)
			Expect(err).NotTo(HaveOccurred())
		})

		It("handles concurrent Resolve calls without data races", func() {
			const goroutines = 50
			var wg sync.WaitGroup
			errs := make(chan error, goroutines)

			wg.Add(goroutines)
			for i := 0; i < goroutines; i++ {
				go func() {
					defer wg.Done()
					defer GinkgoRecover()

					spec, err := reg.Resolve(context.Background(), "section-detect")
					if err != nil {
						errs <- err
						return
					}
					if spec.Name != "section-detect" {
						errs <- context.DeadlineExceeded // sentinel
						return
					}
				}()
			}
			wg.Wait()
			close(errs)

			for err := range errs {
				Expect(err).NotTo(HaveOccurred())
			}
		})
	})

	Context("List returns sorted names", func() {
		BeforeEach(func() {
			var err error
			reg, err = prompt.NewRegistry(cfg)
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns names in alphabetical order", func() {
			names, err := reg.List(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(len(names)).To(BeNumerically(">=", 3))
			for i := 1; i < len(names); i++ {
				Expect(names[i] > names[i-1]).To(BeTrue(),
					"names should be sorted: %q should come after %q", names[i], names[i-1])
			}
		})
	})

	Context("Layers returns merge and sliceStrategy fields", func() {
		BeforeEach(func() {
			var err error
			reg, err = prompt.NewRegistry(cfg)
			Expect(err).NotTo(HaveOccurred())
		})

		It("includes merge and sliceStrategy for each layer", func() {
			layers, err := reg.Layers(context.Background(), "section-detect")
			Expect(err).NotTo(HaveOccurred())
			Expect(layers).NotTo(BeEmpty())

			for _, l := range layers {
				Expect(l.Merge).NotTo(BeEmpty(), "layer %q should have merge set", l.ID)
				Expect(l.SliceStrategy).NotTo(BeEmpty(), "layer %q should have sliceStrategy set", l.ID)
			}
		})

		It("returns layers with HasPrompt=false for nonexistent prompt", func() {
			layers, err := reg.Layers(context.Background(), "nonexistent-prompt")
			Expect(err).NotTo(HaveOccurred())
			for _, l := range layers {
				Expect(l.HasPrompt).To(BeFalse(),
					"layer %q should not have nonexistent prompt", l.ID)
			}
		})
	})

	Context("Render populates Sources and Metadata", func() {
		BeforeEach(func() {
			var err error
			reg, err = prompt.NewRegistry(cfg)
			Expect(err).NotTo(HaveOccurred())
		})

		It("includes source layer info in Sources", func() {
			resolved, err := reg.Render(context.Background(), "section-detect",
				map[string]string{"document_chunk": "test"})
			Expect(err).NotTo(HaveOccurred())
			Expect(resolved.Sources).NotTo(BeEmpty())
			Expect(resolved.Sources[0]).To(ContainSubstring("embedded"))
		})

		It("propagates metadata from spec to resolved prompt", func() {
			// enrichment has metadata in the default YAML; its user template uses ${text_fragment}
			resolved, err := reg.Render(context.Background(), "enrichment",
				map[string]string{"text_fragment": "test content"})
			Expect(err).NotTo(HaveOccurred())
			Expect(resolved.Name).To(Equal("enrichment"))
			Expect(resolved.Metadata).NotTo(BeNil())
			Expect(resolved.Metadata).To(HaveKeyWithValue("domain", "oscal"))
		})
	})

	Context("Render with few-shot cmd: source (commands disabled)", func() {
		BeforeEach(func() {
			cfg.AllowCommands = false
		})

		It("returns ErrCommandDisabled", func() {
			tmpDir, err := os.MkdirTemp("", "prompt-cmd-*")
			Expect(err).NotTo(HaveOccurred())
			defer os.RemoveAll(tmpDir)

			promptDir := filepath.Join(tmpDir, ".crosscodex", "prompts")
			Expect(os.MkdirAll(promptDir, 0o755)).To(Succeed())

			cmdYAML := `
name: cmd-test
version: 1.0.0
templates:
  system: "test"
few_shot_examples:
  - source: "cmd:echo hello"
`
			Expect(os.WriteFile(filepath.Join(promptDir, "cmd.yaml"),
				[]byte(cmdYAML), 0o644)).To(Succeed())

			reg, err = prompt.NewRegistry(cfg, prompt.WithProjectDir(tmpDir))
			Expect(err).NotTo(HaveOccurred())

			_, err = reg.Render(context.Background(), "cmd-test", nil)
			Expect(err).To(MatchError(prompt.ErrCommandDisabled))
		})
	})

	Context("Render with few-shot cmd: source (commands enabled)", func() {
		BeforeEach(func() {
			cfg.AllowCommands = true
		})

		It("returns ErrCommandDisabled (not yet implemented)", func() {
			tmpDir, err := os.MkdirTemp("", "prompt-cmd-enabled-*")
			Expect(err).NotTo(HaveOccurred())
			defer os.RemoveAll(tmpDir)

			promptDir := filepath.Join(tmpDir, ".crosscodex", "prompts")
			Expect(os.MkdirAll(promptDir, 0o755)).To(Succeed())

			cmdYAML := `
name: cmd-enabled
version: 1.0.0
templates:
  system: "test"
few_shot_examples:
  - source: "cmd:echo hello"
`
			Expect(os.WriteFile(filepath.Join(promptDir, "cmd.yaml"),
				[]byte(cmdYAML), 0o644)).To(Succeed())

			reg, err = prompt.NewRegistry(cfg, prompt.WithProjectDir(tmpDir))
			Expect(err).NotTo(HaveOccurred())

			_, err = reg.Render(context.Background(), "cmd-enabled", nil)
			Expect(err).To(MatchError(prompt.ErrCommandDisabled))
		})
	})

	Context("Render with unknown few-shot source protocol", func() {
		It("returns ErrInvalidPromptSpec", func() {
			tmpDir, err := os.MkdirTemp("", "prompt-unknown-*")
			Expect(err).NotTo(HaveOccurred())
			defer os.RemoveAll(tmpDir)

			promptDir := filepath.Join(tmpDir, ".crosscodex", "prompts")
			Expect(os.MkdirAll(promptDir, 0o755)).To(Succeed())

			unknownYAML := `
name: unknown-source
version: 1.0.0
templates:
  system: "test"
few_shot_examples:
  - source: "http://example.com/shots.json"
`
			Expect(os.WriteFile(filepath.Join(promptDir, "unknown.yaml"),
				[]byte(unknownYAML), 0o644)).To(Succeed())

			reg, err = prompt.NewRegistry(cfg, prompt.WithProjectDir(tmpDir))
			Expect(err).NotTo(HaveOccurred())

			_, err = reg.Render(context.Background(), "unknown-source", nil)
			Expect(err).To(MatchError(prompt.ErrInvalidPromptSpec))
		})
	})
})

var _ = Describe("LoadFewShotFile", func() {
	var baseDir string

	BeforeEach(func() {
		var err error
		baseDir, err = os.MkdirTemp("", "fewshot-test-*")
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		os.RemoveAll(baseDir)
	})

	Context("with a valid JSON file", func() {
		It("loads few-shot examples", func() {
			examples := []prompt.FewShotExample{
				{Input: "hello", Output: "world"},
				{Input: "foo", Output: "bar"},
			}
			data, err := json.Marshal(examples)
			Expect(err).NotTo(HaveOccurred())
			Expect(os.WriteFile(filepath.Join(baseDir, "shots.json"), data, 0o644)).To(Succeed())

			result, err := prompt.ExportLoadFewShotFile(baseDir, "file:shots.json")
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(HaveLen(2))
			Expect(result[0].Input).To(Equal("hello"))
			Expect(result[1].Output).To(Equal("bar"))
		})
	})

	Context("with file: prefix stripping", func() {
		It("handles paths without file: prefix", func() {
			examples := []prompt.FewShotExample{{Input: "a", Output: "b"}}
			data, err := json.Marshal(examples)
			Expect(err).NotTo(HaveOccurred())
			Expect(os.WriteFile(filepath.Join(baseDir, "plain.json"), data, 0o644)).To(Succeed())

			result, err := prompt.ExportLoadFewShotFile(baseDir, "plain.json")
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(HaveLen(1))
		})
	})

	Context("path traversal prevention", func() {
		It("rejects relative path traversal with ..", func() {
			_, err := prompt.ExportLoadFewShotFile(baseDir, "file:../../../etc/passwd")
			Expect(err).To(HaveOccurred())
			Expect(err).To(MatchError(prompt.ErrInvalidPromptSpec))
		})

		It("rejects path traversal in middle segments", func() {
			_, err := prompt.ExportLoadFewShotFile(baseDir, "file:sub/../../../etc/passwd")
			Expect(err).To(HaveOccurred())
			Expect(err).To(MatchError(prompt.ErrInvalidPromptSpec))
		})

		It("rejects absolute paths outside baseDir", func() {
			_, err := prompt.ExportLoadFewShotFile(baseDir, "file:/etc/passwd")
			Expect(err).To(HaveOccurred())
			// Either ErrInvalidPromptSpec (path traversal) or file-not-found
		})
	})

	Context("symlink attack prevention", func() {
		It("rejects symlinks pointing outside baseDir", func() {
			if runtime.GOOS == "windows" {
				Skip("symlinks require elevated privileges on Windows")
			}

			// Create a target file outside baseDir
			outsideDir, err := os.MkdirTemp("", "fewshot-outside-*")
			Expect(err).NotTo(HaveOccurred())
			defer os.RemoveAll(outsideDir)

			secretFile := filepath.Join(outsideDir, "secret.json")
			Expect(os.WriteFile(secretFile,
				[]byte(`[{"input":"secret","output":"data"}]`), 0o644)).To(Succeed())

			// Create a symlink inside baseDir pointing to the secret file
			symlinkPath := filepath.Join(baseDir, "evil.json")
			Expect(os.Symlink(secretFile, symlinkPath)).To(Succeed())

			_, err = prompt.ExportLoadFewShotFile(baseDir, "file:evil.json")
			Expect(err).To(HaveOccurred())
			Expect(err).To(MatchError(prompt.ErrInvalidPromptSpec))
		})
	})

	Context("with a nonexistent file", func() {
		It("returns an error", func() {
			_, err := prompt.ExportLoadFewShotFile(baseDir, "file:missing.json")
			Expect(err).To(HaveOccurred())
		})
	})

	Context("with invalid JSON content", func() {
		It("returns a parse error", func() {
			Expect(os.WriteFile(filepath.Join(baseDir, "bad.json"),
				[]byte("not valid json"), 0o644)).To(Succeed())

			_, err := prompt.ExportLoadFewShotFile(baseDir, "file:bad.json")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("parsing few-shot JSON"))
		})
	})

	Context("with an empty JSON array", func() {
		It("returns an empty slice", func() {
			Expect(os.WriteFile(filepath.Join(baseDir, "empty.json"),
				[]byte("[]"), 0o644)).To(Succeed())

			result, err := prompt.ExportLoadFewShotFile(baseDir, "file:empty.json")
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(BeEmpty())
		})
	})

	Context("with a subdirectory path", func() {
		It("loads files from subdirectories within baseDir", func() {
			subDir := filepath.Join(baseDir, "examples")
			Expect(os.MkdirAll(subDir, 0o755)).To(Succeed())

			examples := []prompt.FewShotExample{{Input: "sub", Output: "dir"}}
			data, err := json.Marshal(examples)
			Expect(err).NotTo(HaveOccurred())
			Expect(os.WriteFile(filepath.Join(subDir, "shots.json"), data, 0o644)).To(Succeed())

			result, err := prompt.ExportLoadFewShotFile(baseDir, "file:examples/shots.json")
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(HaveLen(1))
			Expect(result[0].Input).To(Equal("sub"))
		})
	})
})

var _ = Describe("LoadDir", func() {
	Context("with a nonexistent directory", func() {
		It("returns nil, nil", func() {
			specs, err := prompt.ExportLoadDir("/nonexistent/path/that/does/not/exist")
			Expect(err).NotTo(HaveOccurred())
			Expect(specs).To(BeNil())
		})
	})

	Context("with a directory containing non-YAML files", func() {
		It("skips non-YAML files", func() {
			tmpDir, err := os.MkdirTemp("", "loaddir-*")
			Expect(err).NotTo(HaveOccurred())
			defer os.RemoveAll(tmpDir)

			Expect(os.WriteFile(filepath.Join(tmpDir, "readme.md"),
				[]byte("# README"), 0o644)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(tmpDir, "data.json"),
				[]byte("{}"), 0o644)).To(Succeed())

			specs, err := prompt.ExportLoadDir(tmpDir)
			Expect(err).NotTo(HaveOccurred())
			Expect(specs).To(BeEmpty())
		})
	})

	Context("with a directory containing invalid YAML files", func() {
		It("silently skips invalid files and loads valid ones", func() {
			tmpDir, err := os.MkdirTemp("", "loaddir-invalid-*")
			Expect(err).NotTo(HaveOccurred())
			defer os.RemoveAll(tmpDir)

			// Valid file
			Expect(os.WriteFile(filepath.Join(tmpDir, "good.yaml"),
				[]byte("name: good-prompt\nversion: 1.0.0\n"), 0o644)).To(Succeed())
			// Invalid YAML
			Expect(os.WriteFile(filepath.Join(tmpDir, "bad.yaml"),
				[]byte("{{not yaml"), 0o644)).To(Succeed())
			// Missing name
			Expect(os.WriteFile(filepath.Join(tmpDir, "noname.yaml"),
				[]byte("version: 1.0.0\n"), 0o644)).To(Succeed())

			specs, err := prompt.ExportLoadDir(tmpDir)
			Expect(err).NotTo(HaveOccurred())
			Expect(specs).To(HaveLen(1))
			Expect(specs).To(HaveKey("good-prompt"))
		})
	})

	Context("with .yml extension", func() {
		It("loads .yml files", func() {
			tmpDir, err := os.MkdirTemp("", "loaddir-yml-*")
			Expect(err).NotTo(HaveOccurred())
			defer os.RemoveAll(tmpDir)

			Expect(os.WriteFile(filepath.Join(tmpDir, "test.yml"),
				[]byte("name: yml-prompt\nversion: 1.0.0\n"), 0o644)).To(Succeed())

			specs, err := prompt.ExportLoadDir(tmpDir)
			Expect(err).NotTo(HaveOccurred())
			Expect(specs).To(HaveKey("yml-prompt"))
		})
	})

	Context("with duplicate prompt names in same directory", func() {
		It("last file wins", func() {
			tmpDir, err := os.MkdirTemp("", "loaddir-dup-*")
			Expect(err).NotTo(HaveOccurred())
			defer os.RemoveAll(tmpDir)

			// Files are read in directory order (alphabetical on most systems)
			Expect(os.WriteFile(filepath.Join(tmpDir, "a_first.yaml"),
				[]byte("name: dup\nversion: 1.0.0\n"), 0o644)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(tmpDir, "b_second.yaml"),
				[]byte("name: dup\nversion: 2.0.0\n"), 0o644)).To(Succeed())

			specs, err := prompt.ExportLoadDir(tmpDir)
			Expect(err).NotTo(HaveOccurred())
			Expect(specs).To(HaveLen(1))
			Expect(specs["dup"].Version).To(Equal("2.0.0"))
		})
	})
})

var _ = Describe("MergeSpecs (extended)", func() {
	Context("with deep_copy slice strategy", func() {
		It("uses deep copy semantics for slice merging", func() {
			base := &prompt.PromptSpec{
				Name: "test",
				FewShot: []prompt.FewShotExample{
					{Input: "base1", Output: "base1-out"},
					{Input: "base2", Output: "base2-out"},
				},
			}
			overlay := &prompt.PromptSpec{
				Name: "test",
				FewShot: []prompt.FewShotExample{
					{Input: "overlay1", Output: "overlay1-out"},
				},
			}
			result, err := prompt.ExportMergeSpecs(base, overlay, "deep_copy")
			Expect(err).NotTo(HaveOccurred())
			// With deep_copy, mergo's SliceDeepCopy replaces the slice
			// (same as override for non-empty overlay slices)
			Expect(result.FewShot).NotTo(BeEmpty())
			// Verify it doesn't crash and produces a valid result
			Expect(result.Name).To(Equal("test"))
		})

		It("produces a different result than append", func() {
			base := &prompt.PromptSpec{
				Name: "test",
				FewShot: []prompt.FewShotExample{
					{Input: "base1", Output: "base1-out"},
				},
			}
			overlay := &prompt.PromptSpec{
				Name: "test",
				FewShot: []prompt.FewShotExample{
					{Input: "overlay1", Output: "overlay1-out"},
				},
			}

			replaceResult, err := prompt.ExportMergeSpecs(base, overlay, "deep_copy")
			Expect(err).NotTo(HaveOccurred())

			appendResult, err := prompt.ExportMergeSpecs(base, overlay, "append")
			Expect(err).NotTo(HaveOccurred())

			// Append should have 2 items, deep_copy should have 1
			Expect(appendResult.FewShot).To(HaveLen(2))
			Expect(replaceResult.FewShot).To(HaveLen(1))
		})
	})

	Context("input mutation prevention", func() {
		It("does not modify the base spec", func() {
			base := &prompt.PromptSpec{
				Name:    "test",
				Version: "1.0.0",
				Templates: prompt.TemplateSet{
					System: "original",
				},
				FewShot:  []prompt.FewShotExample{{Input: "base", Output: "base"}},
				Metadata: map[string]string{"key": "val"},
			}
			overlay := &prompt.PromptSpec{
				Name:    "test",
				Version: "2.0.0",
				Templates: prompt.TemplateSet{
					System: "modified",
				},
			}

			_, err := prompt.ExportMergeSpecs(base, overlay, "replace")
			Expect(err).NotTo(HaveOccurred())

			// Base should be unchanged
			Expect(base.Version).To(Equal("1.0.0"))
			Expect(base.Templates.System).To(Equal("original"))
			Expect(base.FewShot).To(HaveLen(1))
			Expect(base.FewShot[0].Input).To(Equal("base"))
			Expect(base.Metadata).To(HaveKeyWithValue("key", "val"))
		})

		It("does not modify the overlay spec", func() {
			base := &prompt.PromptSpec{
				Name:     "test",
				Metadata: map[string]string{"base-key": "base-val"},
			}
			overlay := &prompt.PromptSpec{
				Name:     "test",
				Version:  "2.0.0",
				Metadata: map[string]string{"overlay-key": "overlay-val"},
			}

			_, err := prompt.ExportMergeSpecs(base, overlay, "replace")
			Expect(err).NotTo(HaveOccurred())

			// Overlay should be unchanged
			Expect(overlay.Version).To(Equal("2.0.0"))
			Expect(overlay.Metadata).To(HaveLen(1))
			Expect(overlay.Metadata).To(HaveKeyWithValue("overlay-key", "overlay-val"))
		})
	})

	Context("with one empty name", func() {
		It("allows merge when overlay has empty name", func() {
			base := &prompt.PromptSpec{Name: "test", Version: "1.0.0"}
			overlay := &prompt.PromptSpec{Version: "2.0.0"}

			result, err := prompt.ExportMergeSpecs(base, overlay, "replace")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Name).To(Equal("test"))
			Expect(result.Version).To(Equal("2.0.0"))
		})
	})

	Context("SourceDir preservation through merge", func() {
		It("preserves SourceDir on FewShotExample after replace merge", func() {
			base := &prompt.PromptSpec{
				Name: "test",
				FewShot: []prompt.FewShotExample{
					{Input: "base", Output: "base", SourceDir: "/base/dir"},
				},
			}
			overlay := &prompt.PromptSpec{
				Name: "test",
				FewShot: []prompt.FewShotExample{
					{Input: "overlay", Output: "overlay", SourceDir: "/overlay/dir"},
				},
			}

			result, err := prompt.ExportMergeSpecs(base, overlay, "replace")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.FewShot).To(HaveLen(1))
			Expect(result.FewShot[0].SourceDir).To(Equal("/overlay/dir"))
		})

		It("preserves SourceDir on FewShotExample after append merge", func() {
			base := &prompt.PromptSpec{
				Name: "test",
				FewShot: []prompt.FewShotExample{
					{Input: "base", Output: "base", SourceDir: "/base/dir"},
				},
			}
			overlay := &prompt.PromptSpec{
				Name: "test",
				FewShot: []prompt.FewShotExample{
					{Input: "overlay", Output: "overlay", SourceDir: "/overlay/dir"},
				},
			}

			result, err := prompt.ExportMergeSpecs(base, overlay, "append")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.FewShot).To(HaveLen(2))
			Expect(result.FewShot[0].SourceDir).To(Equal("/base/dir"))
			Expect(result.FewShot[1].SourceDir).To(Equal("/overlay/dir"))
		})
	})
})

var _ = Describe("CopySpec", func() {
	It("creates a deep copy with independent slices and maps", func() {
		original := &prompt.PromptSpec{
			Name:    "original",
			Version: "1.0.0",
			FewShot: []prompt.FewShotExample{
				{Input: "a", Output: "b"},
			},
			Metadata: map[string]string{"key": "val"},
		}

		cp := prompt.ExportCopySpec(original)

		// Modify the copy
		cp.Name = "modified"
		cp.FewShot[0].Input = "changed"
		cp.Metadata["key"] = "changed"
		cp.Metadata["new"] = "added"

		// Original must be unchanged
		Expect(original.Name).To(Equal("original"))
		Expect(original.FewShot[0].Input).To(Equal("a"))
		Expect(original.Metadata).To(HaveLen(1))
		Expect(original.Metadata["key"]).To(Equal("val"))
	})
})

var _ = Describe("SubstitutePlaceholders (extended)", func() {
	Context("with multiple missing variables", func() {
		It("reports all missing variable names in error", func() {
			_, err := prompt.ExportSubstitutePlaceholders(
				"Hello ${first} ${second} ${third}",
				map[string]string{},
			)
			Expect(err).To(MatchError(prompt.ErrMissingPlaceholder))
			Expect(err.Error()).To(ContainSubstring("first"))
			Expect(err.Error()).To(ContainSubstring("second"))
			Expect(err.Error()).To(ContainSubstring("third"))
		})
	})

	Context("with nil vars map", func() {
		It("returns ErrMissingPlaceholder for templates with placeholders", func() {
			_, err := prompt.ExportSubstitutePlaceholders("${name}", nil)
			Expect(err).To(MatchError(prompt.ErrMissingPlaceholder))
		})

		It("succeeds for templates without placeholders", func() {
			result, err := prompt.ExportSubstitutePlaceholders("no placeholders", nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal("no placeholders"))
		})
	})

	Context("with placeholder-like but invalid patterns", func() {
		It("treats ${} as literal text", func() {
			result, err := prompt.ExportSubstitutePlaceholders("${}", nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal("${}"))
		})

		It("treats ${123invalid} as literal text", func() {
			result, err := prompt.ExportSubstitutePlaceholders("${123invalid}", nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal("${123invalid}"))
		})

		It("treats ${-dash} as literal text", func() {
			result, err := prompt.ExportSubstitutePlaceholders("${-dash}", nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal("${-dash}"))
		})
	})
})

var _ = Describe("Embedded Defaults (extended)", func() {
	It("loads exactly three defaults", func() {
		specs, err := prompt.ExportLoadEmbeddedDefaults()
		Expect(err).NotTo(HaveOccurred())
		Expect(specs).To(HaveLen(3))
	})
})

var _ = Describe("ResolvedPrompt LogValue", func() {
	It("implements slog.LogValuer with expected fields", func() {
		resolved := &prompt.ResolvedPrompt{
			Name:        "test-prompt",
			Version:     "1.0.0",
			ContentHash: "abc123",
			Sources:     []string{"embedded:embedded"},
		}

		val := resolved.LogValue()
		// slog.Value should be a group
		Expect(val.Kind()).To(Equal(slog.KindGroup))

		attrs := val.Group()
		attrMap := make(map[string]string)
		for _, a := range attrs {
			if a.Value.Kind() == slog.KindString {
				attrMap[a.Key] = a.Value.String()
			}
		}
		Expect(attrMap).To(HaveKeyWithValue("name", "test-prompt"))
		Expect(attrMap).To(HaveKeyWithValue("version", "1.0.0"))
		Expect(attrMap).To(HaveKeyWithValue("content_hash", "abc123"))
	})
})

var _ = Describe("ParsePromptSpec (extended)", func() {
	Context("with empty input", func() {
		It("returns ErrInvalidPromptSpec", func() {
			_, err := prompt.ExportParsePromptSpec([]byte(""))
			Expect(err).To(MatchError(prompt.ErrInvalidPromptSpec))
		})
	})

	Context("with only whitespace", func() {
		It("returns ErrInvalidPromptSpec", func() {
			_, err := prompt.ExportParsePromptSpec([]byte("   \n\n  "))
			Expect(err).To(MatchError(prompt.ErrInvalidPromptSpec))
		})
	})
})

var _ = Describe("AssembleMessages (extended)", func() {
	Context("with few-shot example having empty input but non-empty output", func() {
		It("produces only an assistant message", func() {
			msgs := prompt.ExportAssembleMessages("sys", "usr",
				[]prompt.FewShotExample{{Input: "", Output: "only-output"}})
			// system + assistant + user = 3 (no user for the few-shot)
			Expect(msgs).To(HaveLen(3))
			Expect(msgs[0].Role).To(Equal("system"))
			Expect(msgs[1].Role).To(Equal("assistant"))
			Expect(msgs[1].Content).To(Equal("only-output"))
			Expect(msgs[2].Role).To(Equal("user"))
		})
	})

	Context("with few-shot example having empty output but non-empty input", func() {
		It("produces only a user message for the few-shot", func() {
			msgs := prompt.ExportAssembleMessages("sys", "usr",
				[]prompt.FewShotExample{{Input: "only-input", Output: ""}})
			// system + user(few-shot) + user = 3
			Expect(msgs).To(HaveLen(3))
			Expect(msgs[0].Role).To(Equal("system"))
			Expect(msgs[1].Role).To(Equal("user"))
			Expect(msgs[1].Content).To(Equal("only-input"))
			Expect(msgs[2].Role).To(Equal("user"))
			Expect(msgs[2].Content).To(Equal("usr"))
		})
	})
})

var _ = Describe("Registry with WithTelemetry", func() {
	It("accepts telemetry providers and functions correctly", func() {
		tp, err := telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = tp.Shutdown(context.Background()) }()

		cfg := config.PromptConfig{
			CaptureContent: true,
			AllowCommands:  false,
			Layers: config.PromptLayerConfig{
				Enabled: true,
			},
		}

		reg, err := prompt.NewRegistry(cfg,
			prompt.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()))
		Expect(err).NotTo(HaveOccurred())

		// Verify the registry operates correctly with telemetry configured
		names, err := reg.List(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(names).To(ContainElement("section-detect"))

		spec, err := reg.Resolve(context.Background(), "section-detect")
		Expect(err).NotTo(HaveOccurred())
		Expect(spec.Name).To(Equal("section-detect"))

		resolved, err := reg.Render(context.Background(), "section-detect",
			map[string]string{"document_chunk": "test content"})
		Expect(err).NotTo(HaveOccurred())
		Expect(resolved.ContentHash).NotTo(BeEmpty())
	})
})

var _ = Describe("Registry Render with file: few-shot using SourceDir", func() {
	It("resolves file: references relative to the prompt source directory", func() {
		// Create a project directory with a prompt that uses file: few-shot source
		tmpDir, err := os.MkdirTemp("", "prompt-sourcedir-*")
		Expect(err).NotTo(HaveOccurred())
		defer os.RemoveAll(tmpDir)

		promptDir := filepath.Join(tmpDir, ".crosscodex", "prompts")
		Expect(os.MkdirAll(promptDir, 0o755)).To(Succeed())

		// Create few-shot examples JSON file in the prompt directory
		examplesJSON := `[{"input":"example q","output":"example a"}]`
		Expect(os.WriteFile(filepath.Join(promptDir, "examples.json"),
			[]byte(examplesJSON), 0o644)).To(Succeed())

		// Create a prompt that references the file relative to its own directory
		promptYAML := `
name: sourcedir-test
version: 1.0.0
templates:
  system: "System prompt"
  user: "${query}"
few_shot_examples:
  - source: "file:examples.json"
`
		Expect(os.WriteFile(filepath.Join(promptDir, "sourcedir_test.yaml"),
			[]byte(promptYAML), 0o644)).To(Succeed())

		cfg := config.PromptConfig{
			CaptureContent: true,
			AllowCommands:  false,
			Layers: config.PromptLayerConfig{
				Enabled: true,
			},
		}

		reg, err := prompt.NewRegistry(cfg, prompt.WithProjectDir(tmpDir))
		Expect(err).NotTo(HaveOccurred())

		resolved, err := reg.Render(context.Background(), "sourcedir-test",
			map[string]string{"query": "test question"})
		Expect(err).NotTo(HaveOccurred())

		// Should have: system + user(few-shot input) + assistant(few-shot output) + user(query)
		Expect(resolved.Messages).To(HaveLen(4))
		Expect(resolved.Messages[0].Role).To(Equal("system"))
		Expect(resolved.Messages[1].Role).To(Equal("user"))
		Expect(resolved.Messages[1].Content).To(Equal("example q"))
		Expect(resolved.Messages[2].Role).To(Equal("assistant"))
		Expect(resolved.Messages[2].Content).To(Equal("example a"))
		Expect(resolved.Messages[3].Role).To(Equal("user"))
		Expect(resolved.Messages[3].Content).To(Equal("test question"))
	})

	It("fails when file: reference is relative to wrong directory", func() {
		// Create a project directory with a prompt referencing a file
		// that exists only next to the prompt, not in the CWD
		tmpDir, err := os.MkdirTemp("", "prompt-sourcedir-fail-*")
		Expect(err).NotTo(HaveOccurred())
		defer os.RemoveAll(tmpDir)

		promptDir := filepath.Join(tmpDir, ".crosscodex", "prompts")
		Expect(os.MkdirAll(promptDir, 0o755)).To(Succeed())

		// Do NOT create the examples.json file — it only exists in some other dir
		promptYAML := `
name: missing-file-test
version: 1.0.0
templates:
  system: "System prompt"
few_shot_examples:
  - source: "file:nonexistent.json"
`
		Expect(os.WriteFile(filepath.Join(promptDir, "missing.yaml"),
			[]byte(promptYAML), 0o644)).To(Succeed())

		cfg := config.PromptConfig{
			CaptureContent: true,
			AllowCommands:  false,
			Layers: config.PromptLayerConfig{
				Enabled: true,
			},
		}

		reg, err := prompt.NewRegistry(cfg, prompt.WithProjectDir(tmpDir))
		Expect(err).NotTo(HaveOccurred())

		_, err = reg.Render(context.Background(), "missing-file-test", nil)
		Expect(err).To(HaveOccurred())
		// Should fail because the file doesn't exist relative to the prompt source dir
	})
})
