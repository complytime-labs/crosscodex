package config

import "testing"

func TestOptions_ApplyCorrectly(t *testing.T) {
	tests := []struct {
		name  string
		opt   Option
		check func(t *testing.T, o loaderOptions)
	}{
		{
			name: "WithConfigPath sets configPath",
			opt:  WithConfigPath("/etc/crosscodex/config.yaml"),
			check: func(t *testing.T, o loaderOptions) {
				if o.configPath != "/etc/crosscodex/config.yaml" {
					t.Errorf("configPath = %q, want %q", o.configPath, "/etc/crosscodex/config.yaml")
				}
			},
		},
		{
			name: "WithEnvPrefix sets envPrefix",
			opt:  WithEnvPrefix("CROSSCODEX"),
			check: func(t *testing.T, o loaderOptions) {
				if o.envPrefix != "CROSSCODEX" {
					t.Errorf("envPrefix = %q, want %q", o.envPrefix, "CROSSCODEX")
				}
			},
		},
		{
			name: "WithProfile sets profile",
			opt:  WithProfile("local"),
			check: func(t *testing.T, o loaderOptions) {
				if o.profile != "local" {
					t.Errorf("profile = %q, want %q", o.profile, "local")
				}
			},
		},
		{
			name: "WithProjectDir sets projectDir",
			opt:  WithProjectDir("/tmp/myproject"),
			check: func(t *testing.T, o loaderOptions) {
				if o.projectDir != "/tmp/myproject" {
					t.Errorf("projectDir = %q, want %q", o.projectDir, "/tmp/myproject")
				}
			},
		},
		{
			name: "WithOverrides sets overrides",
			opt: WithOverrides(map[string]string{
				"llm.gateway_url": "http://override:4000",
				"tls.mode":        "off",
			}),
			check: func(t *testing.T, o loaderOptions) {
				if len(o.overrides) != 2 {
					t.Fatalf("overrides length = %d, want 2", len(o.overrides))
				}
				if o.overrides["llm.gateway_url"] != "http://override:4000" {
					t.Errorf("overrides[llm.gateway_url] = %q, want %q",
						o.overrides["llm.gateway_url"], "http://override:4000")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var opts loaderOptions
			tt.opt(&opts)
			tt.check(t, opts)
		})
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

func TestOptions_LastWriteWins(t *testing.T) {
	var opts loaderOptions
	WithConfigPath("/first")(&opts)
	WithConfigPath("/second")(&opts)

	if opts.configPath != "/second" {
		t.Errorf("configPath = %q, want %q after override", opts.configPath, "/second")
	}
}
