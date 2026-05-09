package config

import "testing"

func TestWithConfigPath_SetsPath(t *testing.T) {
	var opts loaderOptions
	WithConfigPath("/etc/crosscodex/config.yaml")(&opts)

	if opts.configPath != "/etc/crosscodex/config.yaml" {
		t.Errorf("configPath = %q, want %q", opts.configPath, "/etc/crosscodex/config.yaml")
	}
}

func TestWithEnvPrefix_SetsPrefix(t *testing.T) {
	var opts loaderOptions
	WithEnvPrefix("CROSSCODEX")(&opts)

	if opts.envPrefix != "CROSSCODEX" {
		t.Errorf("envPrefix = %q, want %q", opts.envPrefix, "CROSSCODEX")
	}
}

func TestOptions_MultipleApply(t *testing.T) {
	var opts loaderOptions
	WithConfigPath("/tmp/config.yaml")(&opts)
	WithEnvPrefix("TEST")(&opts)

	if opts.configPath != "/tmp/config.yaml" {
		t.Errorf("configPath = %q, want %q", opts.configPath, "/tmp/config.yaml")
	}
	if opts.envPrefix != "TEST" {
		t.Errorf("envPrefix = %q, want %q", opts.envPrefix, "TEST")
	}
}

func TestWithConfigPath_OverridesPrevious(t *testing.T) {
	var opts loaderOptions
	WithConfigPath("/first")(&opts)
	WithConfigPath("/second")(&opts)

	if opts.configPath != "/second" {
		t.Errorf("configPath = %q, want %q after override", opts.configPath, "/second")
	}
}
