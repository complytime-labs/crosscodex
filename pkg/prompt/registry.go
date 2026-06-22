package prompt

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// RegistryOption configures the Registry during construction.
type RegistryOption func(*registryOpts)

type registryOpts struct {
	projectDir string
	cliLayers  []string
	tracer     trace.TracerProvider
	meter      metric.MeterProvider
}

// WithProjectDir sets the project root directory for discovering
// .crosscodex/prompts/ overlays.
func WithProjectDir(path string) RegistryOption {
	return func(o *registryOpts) {
		o.projectDir = path
	}
}

// WithCLILayers adds filesystem directories as CLI-layer prompt sources.
func WithCLILayers(paths ...string) RegistryOption {
	return func(o *registryOpts) {
		o.cliLayers = paths
	}
}

// WithTelemetry configures OTel tracing and metrics for the registry.
func WithTelemetry(tp trace.TracerProvider, mp metric.MeterProvider) RegistryOption {
	return func(o *registryOpts) {
		o.tracer = tp
		o.meter = mp
	}
}

// layerData holds the parsed prompts for a single layer.
type layerData struct {
	id            string
	source        string
	merge         string // "merge" or "replace"
	sliceStrategy string // "replace", "append", or "deep_copy"
	specs         map[string]*PromptSpec
}

// registry is the default Registry implementation.
type registry struct {
	cfg    config.PromptConfig
	layers []layerData
	mu     sync.RWMutex
	cache  map[string]*PromptSpec
	tracer trace.TracerProvider
	meter  metric.MeterProvider
}

// NewRegistry creates a Registry from configuration.
// It scans all enabled layers at construction time and caches the results.
func NewRegistry(cfg config.PromptConfig, opts ...RegistryOption) (Registry, error) {
	var ropts registryOpts
	for _, o := range opts {
		o(&ropts)
	}

	r := &registry{
		cfg:    cfg,
		cache:  make(map[string]*PromptSpec),
		tracer: ropts.tracer,
		meter:  ropts.meter,
	}

	if err := r.buildLayers(cfg, &ropts); err != nil {
		return nil, err
	}

	return r, nil
}

// layerDef is an internal type used during layer stack construction.
type layerDef struct {
	id            string
	merge         string
	sliceStrategy string
}

// buildLayers constructs the layer stack based on config.
func (r *registry) buildLayers(cfg config.PromptConfig, ropts *registryOpts) error {
	var order []layerDef

	if !cfg.Layers.Enabled {
		// Only layers explicitly listed in Order are active.
		for _, entry := range cfg.Layers.Order {
			order = append(order, layerDef{
				id:            entry.ID,
				merge:         entry.Merge,
				sliceStrategy: entry.SliceStrategy,
			})
		}
	} else if len(cfg.Layers.Order) > 0 {
		// Custom order specified by config.
		for _, entry := range cfg.Layers.Order {
			order = append(order, layerDef{
				id:            entry.ID,
				merge:         entry.Merge,
				sliceStrategy: entry.SliceStrategy,
			})
		}
		// Append cli if not explicitly listed but CLI paths are provided.
		hasExplicitCLI := false
		for _, entry := range order {
			if entry.id == "cli" {
				hasExplicitCLI = true
				break
			}
		}
		if !hasExplicitCLI && len(ropts.cliLayers) > 0 {
			order = append(order, layerDef{id: "cli"})
		}
	} else {
		// Default order: embedded, user, project, (custom layer_paths), cli.
		order = []layerDef{
			{id: "embedded"},
			{id: "user"},
			{id: "project"},
		}
		for _, lp := range cfg.LayerPaths {
			order = append(order, layerDef{id: lp})
		}
		if len(ropts.cliLayers) > 0 {
			order = append(order, layerDef{id: "cli"})
		}
	}

	for _, def := range order {
		ld := layerData{
			id:            def.id,
			merge:         def.merge,
			sliceStrategy: def.sliceStrategy,
		}
		if ld.merge == "" {
			ld.merge = "merge"
		}
		if ld.sliceStrategy == "" {
			ld.sliceStrategy = "replace"
		}

		var err error
		switch def.id {
		case "embedded":
			ld.source = "embedded"
			ld.specs, err = loadEmbeddedDefaults()
		case "user":
			dir := userPromptDir()
			ld.source = dir
			ld.specs, err = loadDir(dir)
		case "project":
			if ropts.projectDir != "" {
				dir := filepath.Join(ropts.projectDir, ".crosscodex", "prompts")
				ld.source = dir
				ld.specs, err = loadDir(dir)
			}
		case "cli":
			merged := make(map[string]*PromptSpec)
			for _, path := range ropts.cliLayers {
				ld.source = path
				specs, loadErr := loadDir(path)
				if loadErr != nil {
					err = loadErr
					break
				}
				for k, v := range specs {
					merged[k] = v
				}
			}
			ld.specs = merged
		default:
			// Custom layer path from layer_paths.
			ld.source = def.id
			ld.specs, err = loadDir(def.id)
		}

		if err != nil {
			return fmt.Errorf("loading layer %q: %w", def.id, err)
		}

		if ld.specs == nil {
			ld.specs = make(map[string]*PromptSpec)
		}

		r.layers = append(r.layers, ld)
	}

	return nil
}

// userPromptDir returns the XDG_DATA_HOME-based prompt directory.
func userPromptDir() string {
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		home, _ := os.UserHomeDir()
		dataHome = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dataHome, "crosscodex", "prompts")
}

// Resolve returns a fully layered, unrendered PromptSpec.
func (r *registry) Resolve(_ context.Context, name string) (*PromptSpec, error) {
	// Fast path: check cache under read lock.
	r.mu.RLock()
	if cached, ok := r.cache[name]; ok {
		cp := copySpec(cached)
		r.mu.RUnlock()
		return &cp, nil
	}
	r.mu.RUnlock()

	// Slow path: resolve and cache under write lock.
	r.mu.Lock()
	defer r.mu.Unlock()

	// Double-check after acquiring write lock.
	if cached, ok := r.cache[name]; ok {
		cp := copySpec(cached)
		return &cp, nil
	}

	var result *PromptSpec

	for _, layer := range r.layers {
		spec, exists := layer.specs[name]
		if !exists {
			continue
		}

		if result == nil || layer.merge == "replace" {
			cp := copySpec(spec)
			result = &cp
			continue
		}

		merged, err := mergeSpecs(result, spec, layer.sliceStrategy)
		if err != nil {
			return nil, err
		}
		result = merged
	}

	if result == nil {
		return nil, fmt.Errorf("no layer provides prompt %q: %w", name, ErrPromptNotFound)
	}

	r.cache[name] = result
	cp := copySpec(result)
	return &cp, nil
}

// Render resolves, substitutes placeholders, loads external few-shot sources,
// assembles []Message, and computes ContentHash.
func (r *registry) Render(ctx context.Context, name string, vars map[string]string) (*ResolvedPrompt, error) {
	spec, err := r.Resolve(ctx, name)
	if err != nil {
		return nil, err
	}

	system, err := substitutePlaceholders(spec.Templates.System, vars)
	if err != nil {
		return nil, fmt.Errorf("system template for %q: %w", name, err)
	}

	user, err := substitutePlaceholders(spec.Templates.User, vars)
	if err != nil {
		return nil, fmt.Errorf("user template for %q: %w", name, err)
	}

	resolvedFewShot, err := r.resolveFewShot(spec.FewShot)
	if err != nil {
		return nil, fmt.Errorf("few-shot examples for %q: %w", name, err)
	}

	messages := assembleMessages(system, user, resolvedFewShot)

	msgBytes, err := json.Marshal(messages)
	if err != nil {
		return nil, fmt.Errorf("serializing messages for hash: %w", err)
	}
	contentHash := storage.ContentHash(msgBytes)

	var sources []string
	for _, layer := range r.layers {
		if _, exists := layer.specs[name]; exists {
			sources = append(sources, fmt.Sprintf("%s:%s", layer.id, layer.source))
		}
	}

	resolved := &ResolvedPrompt{
		Name:        spec.Name,
		Version:     spec.Version,
		Messages:    messages,
		ContentHash: contentHash,
		Sources:     sources,
		Metadata:    spec.Metadata,
	}

	return resolved, nil
}

// resolveFewShot processes few-shot examples, loading external sources.
func (r *registry) resolveFewShot(examples []FewShotExample) ([]FewShotExample, error) {
	var resolved []FewShotExample

	for _, ex := range examples {
		if ex.Source == "" {
			resolved = append(resolved, ex)
			continue
		}

		if strings.HasPrefix(ex.Source, "cmd:") {
			if !r.cfg.AllowCommands {
				return nil, fmt.Errorf("few-shot source %q requires allow_commands=true: %w",
					ex.Source, ErrCommandDisabled)
			}
			return nil, fmt.Errorf("command few-shot sources are not yet implemented: %w", ErrCommandDisabled)
		}

		if strings.HasPrefix(ex.Source, "file:") {
			baseDir := ex.SourceDir
			if baseDir == "" {
				baseDir = "."
			}
			loaded, err := loadFewShotFile(baseDir, ex.Source)
			if err != nil {
				return nil, err
			}
			resolved = append(resolved, loaded...)
			continue
		}

		return nil, fmt.Errorf("unknown few-shot source protocol in %q: %w", ex.Source, ErrInvalidPromptSpec)
	}

	return resolved, nil
}

// List returns all available prompt names (union across all layers).
func (r *registry) List(_ context.Context) ([]string, error) {
	seen := make(map[string]bool)
	for _, layer := range r.layers {
		for name := range layer.specs {
			seen[name] = true
		}
	}

	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)

	return names, nil
}

// Layers returns the layer stack for a named prompt.
func (r *registry) Layers(_ context.Context, name string) ([]LayerInfo, error) {
	var infos []LayerInfo

	for _, layer := range r.layers {
		_, hasPrompt := layer.specs[name]
		infos = append(infos, LayerInfo{
			ID:            layer.id,
			Source:        layer.source,
			Merge:         layer.merge,
			SliceStrategy: layer.sliceStrategy,
			HasPrompt:     hasPrompt,
		})
	}

	return infos, nil
}
