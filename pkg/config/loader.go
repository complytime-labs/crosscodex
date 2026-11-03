package config

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
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

	merged, tracker, layerErr, typePaths, err := l.resolve(&o)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLoadFailed, errors.Join(layerErr, err))
	}

	out, err := yaml.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("%w: marshaling merged config: %w", ErrLoadFailed, err)
	}

	// Every layer (files, CLI flags, environment variables) was checked for
	// values of the wrong type against its own source, so when it reported
	// some, this decode's TypeError restates them in the merged document's
	// line numbers and is dropped. yaml.v3 still decodes every other field.
	// When no layer reported one, the decode is the backstop, and its error,
	// which names no setting, stops the load.
	var cfg Config
	if err := yaml.Unmarshal(out, &cfg); err != nil {
		var te *yaml.TypeError
		if !errors.As(err, &te) || len(typePaths) == 0 {
			return nil, fmt.Errorf("%w: %w", ErrLoadFailed, errors.Join(layerErr, fmt.Errorf("unmarshaling config: %w", err)))
		}
	}

	// Layer problems do not stop validation: the decode above dropped unknown
	// keys and left values of the wrong type at their zero value, and the
	// rest of the configuration is checked as loaded, so one load reports
	// every problem. A setting with a wrong-type value is not validated
	// again, so it is one problem, not two. config set and unset rely on
	// that to tell a repair (one problem fewer) from a change that only
	// exposes another.
	vErr := withoutPaths(validate(&cfg, tracker), typePaths)
	if layerErr != nil {
		return nil, fmt.Errorf("%w: %w", ErrLoadFailed, errors.Join(layerErr, vErr))
	}
	if vErr != nil {
		return nil, fmt.Errorf("%w: %w", ErrLoadFailed, vErr)
	}

	return &cfg, nil
}

// resolve merges every layer. layerErr joins the problems checkLayer found
// in every layer (unknown keys and values of the wrong type), and typePaths
// lists the settings with a value of the wrong type; err is any other error,
// which leaves no merged node.
func (l *loader) resolve(o *loaderOptions) (merged *yaml.Node, tracker *sourceTracker, layerErr error, typePaths []string, err error) {
	defaults, err := defaultNode()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	s := &layerStack{merged: defaults, tracker: newSourceTracker()}
	s.tracker.track(defaults, SourceDefaults)

	err = s.mergeFiles(o)
	if err == nil {
		err = s.applyOverlays(o)
	}
	layerErr = errors.Join(s.problems...)
	if err != nil {
		return nil, nil, layerErr, s.typePaths, err
	}
	return s.merged, s.tracker, layerErr, s.typePaths, nil
}

// withoutPaths drops from err, a validation error, the problems at or below
// any of paths. It leaves err alone when some problem has no path.
func withoutPaths(err error, paths []string) error {
	if err == nil || len(paths) == 0 || !FieldErrorsComplete(err) {
		return err
	}
	var kept []error
	for _, fe := range FieldErrors(err) {
		if !slices.ContainsFunc(paths, func(p string) bool {
			return fe.Path == p || strings.HasPrefix(fe.Path, p+".") || strings.HasPrefix(fe.Path, p+"[")
		}) {
			kept = append(kept, fe)
		}
	}
	return errors.Join(kept...)
}

// layerStack merges file layers, in priority order, over the compiled
// defaults. A layer with unknown keys or values of the wrong type is still
// merged and its problems are collected, so one Load reports the problems of
// every layer and the operator can fix them all in one pass; any other error
// (a file that cannot be read, parsed or merged) stops resolution at once.
type layerStack struct {
	merged    *yaml.Node
	tracker   *sourceTracker
	problems  []error
	typePaths []string
}

// addProblems records what checkLayer found in one layer.
func (s *layerStack) addProblems(keyErrs, typeErrs []error) {
	if len(keyErrs)+len(typeErrs) == 0 {
		return
	}
	s.problems = append(s.problems, fmt.Errorf("%w: %w", ErrInvalidConfig, errors.Join(append(keyErrs, typeErrs...)...)))
	for _, fe := range FieldErrors(errors.Join(typeErrs...)) {
		s.typePaths = append(s.typePaths, fe.Path)
	}
}

// mergeFiles merges layers 2-7, or only o.configPath when it is set.
func (s *layerStack) mergeFiles(o *loaderOptions) error {
	if o.configPath != "" {
		return s.mergeFile(o.configPath, nil)
	}

	paths := configPaths()

	// Layer 2: system config
	if err := s.mergeFile(paths.systemConfig, nil); err != nil {
		return err
	}

	// Layer 3: system drop-ins
	if err := s.mergeDropIns(paths.systemDropInDir); err != nil {
		return err
	}

	// Layer 4: user config
	if err := s.mergeFile(paths.userConfig, o.userConfigContent); err != nil {
		return err
	}

	// Layer 5: user drop-ins
	if err := s.mergeDropIns(paths.userDropInDir); err != nil {
		return err
	}

	// Layer 6: profile
	if o.profile != "" {
		pPath := profilePath(o.profile)
		if pPath == "" {
			return fmt.Errorf("profile %q: %w", o.profile, ErrInvalidConfig)
		}
		node, err := loadYAMLFile(pPath)
		if err != nil {
			return err
		}
		if node == nil {
			return fmt.Errorf("profile %q at %s: %w", o.profile, pPath, ErrProfileNotFound)
		}
		if err := s.mergeLayer(node, pPath); err != nil {
			return err
		}
	}

	// Layer 7: project config
	if o.projectDir != "" {
		if err := s.mergeFile(filepath.Join(o.projectDir, ".crosscodex", "config.yaml"), nil); err != nil {
			return err
		}
	}
	return nil
}

// mergeFile merges the file at path, or content as if it were that file's
// content when content is non-nil (see WithUserConfigContent). A missing or
// empty file is no layer.
func (s *layerStack) mergeFile(path string, content []byte) error {
	var node *yaml.Node
	var err error
	if content != nil {
		node, err = parseYAML(content, path)
	} else {
		node, err = loadYAMLFile(path)
	}
	if err != nil || node == nil {
		return err
	}
	return s.mergeLayer(node, path)
}

func (s *layerStack) mergeDropIns(dir string) error {
	files, err := discoverDropIns(dir)
	if err != nil {
		return err
	}
	for _, f := range files {
		if err := s.mergeLayer(f.Node, f.Path); err != nil {
			return err
		}
	}
	return nil
}

// mergeLayer checks node, the file layer read from source, for unknown keys,
// resolves its YAML references (see resolveReferences) and merges it.
func (s *layerStack) mergeLayer(node *yaml.Node, source string) error {
	s.addProblems(checkLayer(node, source, true))
	resolved, err := resolveReferences(node, source)
	if err != nil {
		return err
	}
	s.tracker.track(resolved, source)
	s.merged, err = deepMerge(s.merged, resolved)
	return err
}

// applyOverlays merges environment variable and CLI flag override layers onto
// the base config node.
func (s *layerStack) applyOverlays(o *loaderOptions) error {
	merged, tracker := s.merged, s.tracker

	// Unlike file layers and CLI flags, the environment overlay is not
	// checked by checkKnownKeys: some CROSSCODEX_* variables are not config
	// paths (CROSSCODEX_LLM_API_KEY is read through llm.api_key_ref,
	// CROSSCODEX_COLOR and CROSSCODEX_ENDPOINT by the CLI, others only by
	// compose), so rejecting unmatched names would break those deployments.
	envOverlay, err := buildEnvOverlay(o.envPrefix, tracker, func(node *yaml.Node, source string) {
		_, typeErrs := checkLayer(node, source, false)
		s.addProblems(nil, typeErrs)
	})
	if err != nil {
		return err
	}
	if envOverlay != nil {
		merged, err = deepMerge(merged, envOverlay)
		if err != nil {
			return err
		}
	}

	flagOverlay, flagErrs, err := buildOverrideOverlay(o.overrides, tracker)
	s.problems = append(s.problems, flagErrs...)
	if err != nil {
		return err
	}
	if flagOverlay != nil {
		merged, err = deepMerge(merged, flagOverlay)
		if err != nil {
			return err
		}
	}

	s.merged = merged
	return nil
}

// buildOverrideOverlay builds the CLI flag override layer from the flags
// that name a configuration key with a value of its type; flagErrs holds
// the problem with each other flag, which is left out of the layer.
func buildOverrideOverlay(overrides map[string]string, tracker *sourceTracker) (overlay *yaml.Node, flagErrs []error, err error) {
	if len(overrides) == 0 {
		return nil, nil, nil
	}

	for _, path := range slices.Sorted(maps.Keys(overrides)) {
		val := overrides[path]
		segments := strings.Split(path, ".")
		node := buildNodeSimple(segments, val)
		if err := checkKnownKeys(node, "CLI flag "+path); err != nil {
			flagErrs = append(flagErrs, err)
			continue
		}
		if tracker != nil {
			tracker.track(node, "CLI flag "+path)
		}
		overlay, err = deepMerge(overlay, node)
		if err != nil {
			return nil, flagErrs, err
		}
	}
	return overlay, flagErrs, nil
}
