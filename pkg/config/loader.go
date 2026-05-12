package config

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type loader struct{}

// NewLoader returns a Loader that resolves configuration from nine layers.
func NewLoader() Loader {
	return &loader{}
}

func (l *loader) Load(_ context.Context, opts ...Option) (*Config, error) {
	o := loaderOptions{
		envPrefix: "CROSSCODEX",
	}
	for _, opt := range opts {
		opt(&o)
	}

	merged, tracker, err := l.resolve(&o)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLoadFailed, err)
	}

	out, err := yaml.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("%w: marshaling merged config: %w", ErrLoadFailed, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(out, &cfg); err != nil {
		return nil, fmt.Errorf("%w: unmarshaling config: %w", ErrLoadFailed, err)
	}

	if err := validate(&cfg, tracker); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func (l *loader) resolve(o *loaderOptions) (*yaml.Node, *sourceTracker, error) {
	tracker := newSourceTracker()

	merged, err := defaultNode()
	if err != nil {
		return nil, nil, err
	}
	tracker.track(merged, "compiled defaults")

	if o.configPath != "" {
		return l.resolveWithConfigPath(merged, o, tracker)
	}

	paths := configPaths()

	// Layer 2: system config
	merged, err = l.mergeFileTracked(merged, paths.systemConfig, tracker)
	if err != nil {
		return nil, nil, err
	}

	// Layer 3: system drop-ins
	merged, err = l.mergeDropInsTracked(merged, paths.systemDropInDir, tracker)
	if err != nil {
		return nil, nil, err
	}

	// Layer 4: user config
	merged, err = l.mergeFileTracked(merged, paths.userConfig, tracker)
	if err != nil {
		return nil, nil, err
	}

	// Layer 5: user drop-ins
	merged, err = l.mergeDropInsTracked(merged, paths.userDropInDir, tracker)
	if err != nil {
		return nil, nil, err
	}

	// Layer 6: profile
	if o.profile != "" {
		pPath := profilePath(o.profile)
		if pPath == "" {
			return nil, nil, fmt.Errorf("profile %q: %w", o.profile, ErrInvalidConfig)
		}
		node, loadErr := loadYAMLFile(pPath)
		if loadErr != nil {
			return nil, nil, loadErr
		}
		if node == nil {
			return nil, nil, fmt.Errorf("profile %q at %s: %w", o.profile, pPath, ErrProfileNotFound)
		}
		tracker.track(node, pPath)
		merged, err = deepMerge(merged, node)
		if err != nil {
			return nil, nil, err
		}
	}

	// Layer 7: project config
	if o.projectDir != "" {
		projPath := filepath.Join(o.projectDir, ".crosscodex", "config.yaml")
		merged, err = l.mergeFileTracked(merged, projPath, tracker)
		if err != nil {
			return nil, nil, err
		}
	}

	// Layer 8: environment variables
	envOverlay, err := buildEnvOverlay(o.envPrefix, tracker)
	if err != nil {
		return nil, nil, err
	}
	if envOverlay != nil {
		merged, err = deepMerge(merged, envOverlay)
		if err != nil {
			return nil, nil, err
		}
	}

	// Layer 9: CLI flag overrides
	flagOverlay, err := buildOverrideOverlay(o.overrides, tracker)
	if err != nil {
		return nil, nil, err
	}
	if flagOverlay != nil {
		merged, err = deepMerge(merged, flagOverlay)
		if err != nil {
			return nil, nil, err
		}
	}

	return merged, tracker, nil
}

func (l *loader) resolveWithConfigPath(merged *yaml.Node, o *loaderOptions, tracker *sourceTracker) (*yaml.Node, *sourceTracker, error) {
	var err error
	merged, err = l.mergeFileTracked(merged, o.configPath, tracker)
	if err != nil {
		return nil, nil, err
	}

	envOverlay, err := buildEnvOverlay(o.envPrefix, tracker)
	if err != nil {
		return nil, nil, err
	}
	if envOverlay != nil {
		merged, err = deepMerge(merged, envOverlay)
		if err != nil {
			return nil, nil, err
		}
	}

	flagOverlay, err := buildOverrideOverlay(o.overrides, tracker)
	if err != nil {
		return nil, nil, err
	}
	if flagOverlay != nil {
		merged, err = deepMerge(merged, flagOverlay)
		if err != nil {
			return nil, nil, err
		}
	}

	return merged, tracker, nil
}

func (l *loader) mergeFileTracked(base *yaml.Node, path string, tracker *sourceTracker) (*yaml.Node, error) {
	node, err := loadYAMLFile(path)
	if err != nil {
		return nil, err
	}
	if node == nil {
		return base, nil
	}
	tracker.track(node, path)
	return deepMerge(base, node)
}

func (l *loader) mergeDropInsTracked(base *yaml.Node, dir string, tracker *sourceTracker) (*yaml.Node, error) {
	files, err := discoverDropIns(dir)
	if err != nil {
		return nil, err
	}
	result := base
	for _, f := range files {
		tracker.track(f.Node, f.Path)
		result, err = deepMerge(result, f.Node)
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func buildOverrideOverlay(overrides map[string]string, tracker *sourceTracker) (*yaml.Node, error) {
	if len(overrides) == 0 {
		return nil, nil
	}

	var overlay *yaml.Node
	for path, val := range overrides {
		segments := strings.Split(path, ".")
		node := buildNodeSimple(segments, val)
		if tracker != nil {
			tracker.track(node, "CLI flag "+path)
		}
		var err error
		overlay, err = deepMerge(overlay, node)
		if err != nil {
			return nil, err
		}
	}
	return overlay, nil
}
