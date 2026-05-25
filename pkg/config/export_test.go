package config

import "gopkg.in/yaml.v3"

// Exported for testing. Do not use outside tests.

// Functions
var (
	ExportDefaultNode      = defaultNode
	ExportLoadDropIns      = loadDropIns
	ExportApplyEnvVars     = applyEnvVars
	ExportInferTag         = inferTag
	ExportDeepMerge        = deepMerge
	ExportValidate         = validate
	ExportFormatSource     = formatSource
	ExportXDGConfigHome    = xdgConfigHome
	ExportUserConfigDir    = userConfigDir
	ExportConfigPaths      = configPaths
	ExportProfilePath      = profilePath
	ExportNewSourceTracker = newSourceTracker
)

// ExportLoaderOptions mirrors the unexported loaderOptions struct for
// testing that functional options set the correct fields.
type ExportLoaderOptions struct {
	ConfigPath string
	EnvPrefix  string
	Profile    string
	ProjectDir string
	Overrides  map[string]string
}

// ExportApplyOption applies an Option to a fresh loaderOptions and returns
// the result as an ExportLoaderOptions so external tests can inspect fields.
func ExportApplyOption(opt Option) ExportLoaderOptions {
	var o loaderOptions
	opt(&o)
	return ExportLoaderOptions{
		ConfigPath: o.configPath,
		EnvPrefix:  o.envPrefix,
		Profile:    o.profile,
		ProjectDir: o.projectDir,
		Overrides:  o.overrides,
	}
}

// ExportApplyOptions applies multiple Options to a single loaderOptions
// and returns the result, for testing last-write-wins and composition.
func ExportApplyOptions(opts ...Option) ExportLoaderOptions {
	var o loaderOptions
	for _, opt := range opts {
		opt(&o)
	}
	return ExportLoaderOptions{
		ConfigPath: o.configPath,
		EnvPrefix:  o.envPrefix,
		Profile:    o.profile,
		ProjectDir: o.projectDir,
		Overrides:  o.overrides,
	}
}

// ExportResolvedPaths mirrors the unexported resolvedPaths struct.
type ExportResolvedPaths struct {
	SystemConfig    string
	SystemDropInDir string
	UserConfig      string
	UserDropInDir   string
}

// ExportGetConfigPaths calls configPaths() and returns an exported struct.
func ExportGetConfigPaths() ExportResolvedPaths {
	p := configPaths()
	return ExportResolvedPaths{
		SystemConfig:    p.systemConfig,
		SystemDropInDir: p.systemDropInDir,
		UserConfig:      p.userConfig,
		UserDropInDir:   p.userDropInDir,
	}
}

// ExportSourceTracker wraps the unexported sourceTracker for external tests.
type ExportSourceTracker struct {
	inner *sourceTracker
}

// ExportNewSrcTracker creates a new source tracker wrapper.
func ExportNewSrcTracker() *ExportSourceTracker {
	return &ExportSourceTracker{inner: newSourceTracker()}
}

// Track records a node's leaf paths as originating from source.
func (e *ExportSourceTracker) Track(node *yaml.Node, source string) {
	e.inner.track(node, source)
}

// SourceOf returns the source for a given dotted config path.
func (e *ExportSourceTracker) SourceOf(path string) string {
	return e.inner.sourceOf(path)
}

// Inner returns the raw *sourceTracker for passing to ExportFormatSource.
func (e *ExportSourceTracker) Inner() interface{} {
	return e.inner
}

// ExportNilSourceTrackerSourceOf calls sourceOf on a nil *sourceTracker.
func ExportNilSourceTrackerSourceOf(path string) string {
	var s *sourceTracker
	return s.sourceOf(path)
}

// ExportFormatSourceWithTracker calls formatSource with the inner tracker.
func ExportFormatSourceWithTracker(tracker *ExportSourceTracker, path string) string {
	return formatSource(tracker.inner, path)
}

// ExportFormatSourceNil calls formatSource with a nil tracker.
func ExportFormatSourceNil(path string) string {
	return formatSource(nil, path)
}

// ExportMustParseYAML parses a YAML string into a *yaml.Node for use by
// external test packages. Panics on parse failure.
func ExportMustParseYAML(input string) *yaml.Node {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(input), &doc); err != nil {
		panic("ExportMustParseYAML: " + err.Error())
	}
	return doc.Content[0]
}

// ExportMustUnmarshalNode round-trips a yaml.Node through Marshal/Unmarshal
// into an arbitrary Go type. Panics on failure.
func ExportMustUnmarshalNode[T any](node *yaml.Node) T {
	out, err := yaml.Marshal(node)
	if err != nil {
		panic("ExportMustUnmarshalNode marshal: " + err.Error())
	}
	var v T
	if err := yaml.Unmarshal(out, &v); err != nil {
		panic("ExportMustUnmarshalNode unmarshal: " + err.Error())
	}
	return v
}

// ExportValidateConfig calls validate with a nil tracker.
func ExportValidateConfig(cfg *Config) error {
	return validate(cfg, nil)
}

// ExportValidateConfigWithTracker calls validate with the given tracker.
func ExportValidateConfigWithTracker(cfg *Config, tracker *ExportSourceTracker) error {
	return validate(cfg, tracker.inner)
}
