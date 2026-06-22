package prompt

import (
	"embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed defaults/*.yaml
var defaultsFS embed.FS

// loadEmbeddedDefaults parses all embedded default YAML files.
func loadEmbeddedDefaults() (map[string]*PromptSpec, error) {
	entries, err := defaultsFS.ReadDir("defaults")
	if err != nil {
		return nil, fmt.Errorf("reading embedded defaults: %w", err)
	}

	specs := make(map[string]*PromptSpec)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".yaml") && !strings.HasSuffix(entry.Name(), ".yml") {
			continue
		}

		data, err := defaultsFS.ReadFile("defaults/" + entry.Name())
		if err != nil {
			return nil, fmt.Errorf("reading embedded default %q: %w", entry.Name(), err)
		}

		spec, err := parsePromptSpec(data)
		if err != nil {
			return nil, fmt.Errorf("parsing embedded default %q: %w", entry.Name(), err)
		}

		// Embedded defaults have no filesystem directory for relative path
		// resolution. Tag any few-shot sources with an empty SourceDir;
		// the resolveFewShot function falls back to CWD when SourceDir is empty.
		specs[spec.Name] = spec
	}

	return specs, nil
}

// parsePromptSpec parses raw YAML bytes into a PromptSpec.
// Returns ErrInvalidPromptSpec if the name field is empty.
func parsePromptSpec(data []byte) (*PromptSpec, error) {
	var spec PromptSpec
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("parsing prompt YAML: %w", err)
	}
	if spec.Name == "" {
		return nil, fmt.Errorf("prompt name is required: %w", ErrInvalidPromptSpec)
	}
	return &spec, nil
}

// loadFewShotFile loads few-shot examples from a JSON file.
// Path is resolved relative to baseDir.
// Returns a slice of FewShotExample with Input/Output populated.
//
// Security: resolves symlinks via filepath.EvalSymlinks before checking
// containment within baseDir, preventing TOCTOU bypass via symlinks.
func loadFewShotFile(baseDir, ref string) ([]FewShotExample, error) {
	// Strip "file:" prefix.
	path := strings.TrimPrefix(ref, "file:")
	if !filepath.IsAbs(path) {
		path = filepath.Join(baseDir, path)
	}

	// Resolve base directory with symlink evaluation.
	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return nil, fmt.Errorf("resolving base directory %q: %w", baseDir, err)
	}
	realBase, err := filepath.EvalSymlinks(absBase)
	if err != nil {
		return nil, fmt.Errorf("evaluating symlinks in base directory %q: %w", absBase, err)
	}

	// Resolve target path with symlink evaluation.
	cleaned := filepath.Clean(path)
	absCleaned, err := filepath.Abs(cleaned)
	if err != nil {
		return nil, fmt.Errorf("resolving few-shot path %q: %w", cleaned, err)
	}
	realCleaned, err := filepath.EvalSymlinks(absCleaned)
	if err != nil {
		// File may not exist yet — check the cleaned path for traversal
		// before returning the EvalSymlinks error.
		if !strings.HasPrefix(absCleaned, absBase+string(filepath.Separator)) && absCleaned != absBase {
			return nil, fmt.Errorf("path traversal in few-shot file reference %q: %w", ref, ErrInvalidPromptSpec)
		}
		return nil, fmt.Errorf("resolving few-shot path %q: %w", cleaned, err)
	}

	// Prevent path traversal: real resolved path must stay within real base.
	if !strings.HasPrefix(realCleaned, realBase+string(filepath.Separator)) && realCleaned != realBase {
		return nil, fmt.Errorf("path traversal in few-shot file reference %q (resolves to %q, outside %q): %w",
			ref, realCleaned, realBase, ErrInvalidPromptSpec)
	}

	data, err := os.ReadFile(realCleaned)
	if err != nil {
		return nil, fmt.Errorf("reading few-shot file %q: %w", realCleaned, err)
	}

	var examples []FewShotExample
	if err := json.Unmarshal(data, &examples); err != nil {
		return nil, fmt.Errorf("parsing few-shot JSON from %q: %w", realCleaned, err)
	}

	return examples, nil
}

// loadDir scans a directory for *.yaml files and parses each into a PromptSpec.
// Returns a map keyed by prompt name. Skips files that fail to parse.
func loadDir(dir string) (map[string]*PromptSpec, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading prompt directory %q: %w", dir, err)
	}

	specs := make(map[string]*PromptSpec)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".yaml") && !strings.HasSuffix(entry.Name(), ".yml") {
			continue
		}

		filePath := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(filePath)
		if err != nil {
			slog.Warn("skipping unreadable prompt file",
				"path", filePath,
				"error", err)
			continue
		}

		spec, err := parsePromptSpec(data)
		if err != nil {
			slog.Warn("skipping invalid prompt file",
				"path", filePath,
				"error", err)
			continue
		}

		// Tag few-shot examples with source directory for relative path resolution.
		for i := range spec.FewShot {
			if spec.FewShot[i].Source != "" {
				spec.FewShot[i].SourceDir = dir
			}
		}

		specs[spec.Name] = spec
	}

	return specs, nil
}
