package config_test

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

// loadConfigFile writes content to a temporary config file and loads it as
// the only file layer, with an empty XDG config home so no user file leaks in.
func loadConfigFile(content string) (string, *config.Config, error) {
	GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())
	path := filepath.Join(GinkgoT().TempDir(), "config.yaml")
	writeTestFile(path, content)
	cfg, err := config.NewLoader().Load(context.Background(), config.WithConfigPath(path))
	return path, cfg, err
}

// InlineInner, InlineViaPointer and InlineViaValue exercise ",inline" on a
// struct and on a pointer to one, which yaml.v3 decodes the same way.
type InlineInner struct {
	A string `yaml:"a"`
	B int
}

type InlineViaPointer struct {
	Inner *InlineInner `yaml:",inline"`
	C     string       `yaml:"c"`
}

type InlineViaValue struct {
	Inner InlineInner `yaml:",inline"`
	C     string      `yaml:"c"`
}

var _ = Describe("Unknown configuration keys", func() {
	DescribeTable("yamlFields takes an inlined struct's keys from the same keys yaml.v3 decodes",
		func(target any) {
			fields, inlineMap := config.ExportYAMLFields(reflect.TypeOf(target).Elem())
			Expect(inlineMap).To(BeNil())
			Expect(slices.Sorted(maps.Keys(fields))).To(Equal([]string{"a", "b", "c"}))

			dec := yaml.NewDecoder(bytes.NewBufferString("a: x\nb: 1\nc: y\n"))
			dec.KnownFields(true)
			Expect(dec.Decode(target)).To(Succeed())
		},
		Entry("an inlined struct value", &InlineViaValue{}),
		Entry("an inlined pointer to a struct", &InlineViaPointer{}),
	)

	DescribeTable("rejects a misspelled key with its path, location and a suggestion",
		func(content, dottedPath, position, suggestion string) {
			path, _, err := loadConfigFile(content)
			Expect(err).To(MatchError(config.ErrUnknownKey))
			Expect(err).To(MatchError(config.ErrInvalidConfig))
			Expect(err.Error()).To(ContainSubstring("%q", dottedPath))
			Expect(err.Error()).To(ContainSubstring(path + ":" + position))
			Expect(err.Error()).To(ContainSubstring("did you mean %q?", suggestion))
		},
		Entry("a top-level key", "logging:\n  level: info\nloging:\n  level: debug\n",
			"loging", "3:1", "logging"),
		Entry("a nested key", "pipeline:\n  stage_timout: 1h\n",
			"pipeline.stage_timout", "2:3", "stage_timeout"),
		Entry("a misspelled section name", "analysis:\n  relatoinship:\n    enabled: true\n",
			"analysis.relatoinship", "2:3", "relationship"),
		Entry("a key inside a tls.targets value", "tls:\n  targets:\n    nats:\n      mod: mutual\n",
			"tls.targets.nats.mod", "4:7", "mode"),
		Entry("a key inside a tenant_overrides value", "llm:\n  tenant_overrides:\n    acme-corp:\n      defualt_model: x\n",
			"llm.tenant_overrides.acme-corp.defualt_model", "4:7", "default_model"),
		Entry("a key inside a list element", "auth:\n  x509_mappings:\n    - tenant: acme\n      tenat: beta\n",
			"auth.x509_mappings[0].tenat", "4:7", "tenant"),
	)

	It("lists the valid sibling keys and offers no suggestion for a key unlike any of them", func() {
		_, _, err := loadConfigFile("pipeline:\n  zzzzzzzz: 1\n")
		Expect(err).To(MatchError(config.ErrUnknownKey))
		Expect(err.Error()).To(ContainSubstring(`"pipeline.zzzzzzzz"`))
		Expect(err.Error()).NotTo(ContainSubstring("did you mean"))
		Expect(err.Error()).To(ContainSubstring("valid keys under pipeline:"))
		Expect(err.Error()).To(ContainSubstring("stage_timeout"))
		Expect(err.Error()).To(ContainSubstring("max_concurrent_jobs"))
	})

	It("matches a !!binary key by its decoded name, as yaml.v3 does", func() {
		_, cfg, err := loadConfigFile("logging:\n  !!binary bGV2ZWw=: debug\n")
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Logging.Level).To(Equal("debug"))
	})

	It("rejects a !!binary key whose text, but not its decoded name, is a field", func() {
		_, _, err := loadConfigFile("tls:\n  !!binary mode: \"off\"\n")
		Expect(err).To(MatchError(config.ErrUnknownKey))
		Expect(err.Error()).To(ContainSubstring(`"tls.mode"`))
	})

	It("checks a !!merge-tagged key that is not << as an ordinary key", func() {
		_, _, err := loadConfigFile("logging:\n  !!merge foo: {level: debug}\n")
		Expect(err).To(MatchError(config.ErrUnknownKey))
		Expect(err.Error()).To(ContainSubstring(`"logging.foo"`))
	})

	It("reports unknown keys and invalid values together, each with its path", func() {
		path, _, err := loadConfigFile("loging:\n  level: debug\nlogging:\n  level: loud\n")
		Expect(err).To(MatchError(config.ErrUnknownKey))
		Expect(err).To(MatchError(config.ErrInvalidConfig))
		Expect(err.Error()).To(ContainSubstring(path + ":1:1"))
		Expect(err.Error()).To(ContainSubstring("must be one of debug, info, warn, error"))
		var paths []string
		for _, fe := range config.FieldErrors(err) {
			paths = append(paths, fe.Path)
		}
		Expect(paths).To(ConsistOf("loging", "logging.level"))
		Expect(config.FieldErrorsComplete(err)).To(BeTrue())
	})

	DescribeTable("wraps ErrLoadFailed whatever the problems are",
		func(content string) {
			_, _, err := loadConfigFile(content)
			Expect(err).To(MatchError(config.ErrLoadFailed))
			Expect(err).To(MatchError(config.ErrInvalidConfig))
			Expect(config.FieldErrorsComplete(err)).To(BeTrue())
		},
		Entry("an invalid value only", "logging:\n  level: loud\n"),
		Entry("an unknown key only", "loging:\n  level: debug\n"),
		Entry("both", "loging:\n  level: debug\nlogging:\n  level: loud\n"),
	)

	DescribeTable("names the file that set the half of a key pair that is present",
		func(content, path string) {
			file, _, err := loadConfigFile(content)
			Expect(err).To(MatchError(ContainSubstring("(set in " + file + ")")))
			expectOnlyFieldError(err, path)
		},
		Entry("only the private key", "attestation:\n  private_key_path: /k.pem\n", "attestation.private_key_path"),
		Entry("only the public key", "attestation:\n  public_key_path: /p.pem\n", "attestation.public_key_path"),
	)

	DescribeTable("names the file that made a backup setting required",
		func(content, path, wantMsg string) {
			file, _, err := loadConfigFile(content)
			Expect(err).To(MatchError(ContainSubstring(wantMsg + " (set in " + file + ")")))
			expectOnlyFieldError(err, path)
		},
		Entry("a missing dsn", "backup:\n  destination:\n    backend: local\n    local:\n      path: /srv/b\n",
			"backup.dsn", `backup.destination.backend is "local"`),
		Entry("a missing s3 bucket", "backup:\n  dsn: \"postgres://u@h/d\"\n  destination:\n    backend: s3\n",
			"backup.destination.s3.bucket", "backup.destination.backend is s3"),
	)

	It("names the source file of a value checked by a section's Validate method", func() {
		path, _, err := loadConfigFile("analysis:\n  engine:\n    task_timeout: 0s\n")
		Expect(err).To(MatchError(config.ErrInvalidConfig))
		Expect(err.Error()).To(ContainSubstring("analysis.engine.task_timeout 0s (set in " + path + ")"))
		expectOnlyFieldError(err, "analysis.engine.task_timeout")
	})

	It("rejects a key a struct field excludes with yaml:\"-\"", func() {
		_, _, err := loadConfigFile("worker:\n  llm:\n    default_model: x\n")
		Expect(err).To(MatchError(config.ErrUnknownKey))
		Expect(err.Error()).To(ContainSubstring(`"worker.llm"`))
	})

	DescribeTable("tells the operator to delete a removed key",
		func(content, dottedPath, replacement string) {
			_, _, err := loadConfigFile(content)
			Expect(err).To(MatchError(config.ErrUnknownKey))
			Expect(err.Error()).To(ContainSubstring("%q", dottedPath))
			Expect(err.Error()).To(ContainSubstring("was removed"))
			Expect(err.Error()).To(ContainSubstring(replacement))
			Expect(err.Error()).To(ContainSubstring("Delete it."))
		},
		Entry("analysis.relationship.top_k", "analysis:\n  relationship:\n    top_k: 10\n",
			"analysis.relationship.top_k", "analysis.candidates.generators[].config.top_k"),
		Entry("tenants.allowed_tenants", "tenants:\n  allowed_tenants: [acme]\n",
			"tenants.allowed_tenants", "crosscodexd admin tenant"),
	)

	It("reports every unknown key in a file in one error", func() {
		path, _, err := loadConfigFile("pipeline:\n  stage_timout: 1h\nlogging:\n  levle: info\n")
		Expect(err).To(MatchError(config.ErrUnknownKey))
		Expect(err.Error()).To(ContainSubstring(`"pipeline.stage_timout" at ` + path + ":2:3"))
		Expect(err.Error()).To(ContainSubstring(`"logging.levle" at ` + path + ":4:3"))
	})

	It("leaves the loaded data unchanged by not returning a config", func() {
		_, cfg, err := loadConfigFile("pipeline:\n  stage_timout: 1h\n")
		Expect(err).To(MatchError(config.ErrUnknownKey))
		Expect(cfg).To(BeNil())
	})

	Describe("tenant_overrides", func() {
		It("accepts any tenant name as a key", func() {
			_, cfg, err := loadConfigFile("llm:\n  tenant_overrides:\n    acme-corp:\n      default_model: m1\n    beta-inc:\n      timeout: 10\n")
			Expect(err).NotTo(HaveOccurred())
			Expect(*cfg.LLM.TenantOverrides["acme-corp"].DefaultModel).To(Equal("m1"))
			Expect(*cfg.LLM.TenantOverrides["beta-inc"].Timeout).To(Equal(10))
		})

		It("names the global key and the overridable keys for a global-only setting", func() {
			_, _, err := loadConfigFile("llm:\n  tenant_overrides:\n    acme-corp:\n      gateway_mode: true\n")
			Expect(err).To(MatchError(config.ErrUnknownKey))
			Expect(err.Error()).To(ContainSubstring(`"llm.tenant_overrides.acme-corp.gateway_mode"`))
			Expect(err.Error()).To(ContainSubstring("cannot be overridden per tenant"))
			Expect(err.Error()).To(ContainSubstring("set llm.gateway_mode instead"))
			Expect(err.Error()).To(ContainSubstring("overridable keys: allowed_models, api_key_ref, default_model"))
		})

		It("checks sections nested inside an override", func() {
			_, _, err := loadConfigFile("synthesis:\n  tenant_overrides:\n    acme-corp:\n      viability:\n        skip_level_factr: 0.5\n")
			Expect(err).To(MatchError(config.ErrUnknownKey))
			Expect(err.Error()).To(ContainSubstring(`"synthesis.tenant_overrides.acme-corp.viability.skip_level_factr"`))
			Expect(err.Error()).To(ContainSubstring(`did you mean "skip_level_factor"?`))
		})
	})

	Describe("candidate generator config", func() {
		It("accepts generator settings, which the walker leaves open", func() {
			_, cfg, err := loadConfigFile("analysis:\n  candidates:\n    generators:\n      - name: semantic\n        enabled: true\n        config:\n          top_k: 3\n")
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.Analysis.Candidates.Generators[0].Config).To(HaveKeyWithValue("top_k", 3))
		})

		It("still rejects an unknown generator setting through the generator's own validation", func() {
			_, _, err := loadConfigFile("analysis:\n  candidates:\n    generators:\n      - name: semantic\n        config:\n          bogus: 3\n")
			Expect(err).To(MatchError(config.ErrInvalidConfig))
			Expect(err).NotTo(MatchError(config.ErrUnknownKey))
			Expect(err.Error()).To(ContainSubstring(`is not a setting of generator "semantic"`))
		})
	})

	Describe("YAML anchors, aliases and merge keys", func() {
		It("accepts a valid anchor reused through an alias and a merge key", func() {
			_, cfg, err := loadConfigFile("tls:\n  targets:\n    llm: &mtls\n      mode: mutual\n      ca: /a.pem\n    nats:\n      <<: *mtls\n      cert: /c.pem\n    db: *mtls\n")
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.TLS.Targets["nats"].Mode).To(Equal("mutual"))
			Expect(cfg.TLS.Targets["nats"].Cert).To(Equal("/c.pem"))
			Expect(cfg.TLS.Targets["db"].CA).To(Equal("/a.pem"))
		})

		It("rejects an unknown key reached through a merge key, at the anchor's location", func() {
			path, _, err := loadConfigFile("tls:\n  targets:\n    llm: &bad\n      mod: mutual\n    nats:\n      <<: *bad\n")
			Expect(err).To(MatchError(config.ErrUnknownKey))
			Expect(err.Error()).To(ContainSubstring(path + ":4:7"))
			Expect(err.Error()).To(ContainSubstring(`did you mean "mode"?`))
		})

		It("rejects an unknown key reached only through an alias", func() {
			_, _, err := loadConfigFile("llm:\n  allowed_models: &m [a]\n  tenant_overrides:\n    acme-corp: &o\n      default_model: x\n      bogus_key: 1\n    beta-inc: *o\n")
			Expect(err).To(MatchError(config.ErrUnknownKey))
			Expect(err.Error()).To(ContainSubstring("bogus_key"))
		})
	})

	Describe("layer sources", func() {
		var xdg string

		BeforeEach(func() {
			xdg = GinkgoT().TempDir()
			GinkgoT().Setenv("XDG_CONFIG_HOME", xdg)
		})

		It("reports a drop-in file's own path", func() {
			dropIn := filepath.Join(xdg, "crosscodex", "conf.d", "50-local.yaml")
			writeTestFile(filepath.Join(xdg, "crosscodex", "config.yaml"), "logging:\n  level: info\n")
			writeTestFile(dropIn, "logging:\n  formt: json\n")
			_, err := config.NewLoader().Load(context.Background())
			Expect(err).To(MatchError(config.ErrUnknownKey))
			Expect(err.Error()).To(ContainSubstring(dropIn + ":2:3"))
		})

		It("reports the unknown keys of every file layer in one error", func() {
			user := filepath.Join(xdg, "crosscodex", "config.yaml")
			dropIn := filepath.Join(xdg, "crosscodex", "conf.d", "50-local.yaml")
			profile := filepath.Join(xdg, "crosscodex", "profiles", "dev.yaml")
			writeTestFile(user, "loging:\n  level: info\n")
			writeTestFile(dropIn, "logging:\n  formt: json\n")
			writeTestFile(profile, "pipeline:\n  stage_timout: 1h\n")
			cfg, err := config.NewLoader().Load(context.Background(), config.WithProfile("dev"),
				config.WithOverrides(map[string]string{"llm.timout": "5"}))
			Expect(err).To(MatchError(config.ErrUnknownKey))
			Expect(err).To(MatchError(config.ErrInvalidConfig))
			Expect(err.Error()).To(ContainSubstring(user + ":1:1"))
			Expect(err.Error()).To(ContainSubstring(dropIn + ":2:3"))
			Expect(err.Error()).To(ContainSubstring(profile + ":2:3"))
			Expect(err.Error()).To(ContainSubstring("CLI flag llm.timout"))
			Expect(cfg).To(BeNil())
		})

		It("still stops at a file that does not parse, keeping the unknown keys found before it", func() {
			user := filepath.Join(xdg, "crosscodex", "config.yaml")
			dropIn := filepath.Join(xdg, "crosscodex", "conf.d", "50-local.yaml")
			writeTestFile(user, "loging:\n  level: info\n")
			writeTestFile(dropIn, "logging: [unclosed\n")
			_, err := config.NewLoader().Load(context.Background())
			Expect(err).To(MatchError(config.ErrUnknownKey))
			Expect(err.Error()).To(ContainSubstring(user + ":1:1"))
			Expect(err.Error()).To(ContainSubstring("50-local.yaml"))
		})

		It("reports a profile file's own path", func() {
			profile := filepath.Join(xdg, "crosscodex", "profiles", "dev.yaml")
			writeTestFile(profile, "loging:\n  level: debug\n")
			_, err := config.NewLoader().Load(context.Background(), config.WithProfile("dev"))
			Expect(err).To(MatchError(config.ErrUnknownKey))
			Expect(err.Error()).To(ContainSubstring(profile + ":1:1"))
		})

		It("reports a project file's own path", func() {
			projectDir := GinkgoT().TempDir()
			project := filepath.Join(projectDir, ".crosscodex", "config.yaml")
			writeTestFile(project, "llm:\n  timout: 5\n")
			_, err := config.NewLoader().Load(context.Background(), config.WithProjectDir(projectDir))
			Expect(err).To(MatchError(config.ErrUnknownKey))
			Expect(err.Error()).To(ContainSubstring(project + ":2:3"))
		})

		It("rejects a CLI flag override of an unknown path", func() {
			_, err := config.NewLoader().Load(context.Background(),
				config.WithOverrides(map[string]string{"pipeline.stage_timout": "1h"}))
			Expect(err).To(MatchError(config.ErrUnknownKey))
			Expect(err.Error()).To(ContainSubstring(`"pipeline.stage_timout" at CLI flag pipeline.stage_timout`))
			Expect(err.Error()).To(ContainSubstring(`did you mean "stage_timeout"?`))
		})

		It("reports every unknown CLI flag override in one error", func() {
			_, err := config.NewLoader().Load(context.Background(),
				config.WithOverrides(map[string]string{"pipeline.stage_timout": "1h", "loging.level": "debug", "logging.level": "debug"}))
			Expect(err).To(MatchError(config.ErrUnknownKey))
			Expect(err).To(MatchError(config.ErrInvalidConfig))
			Expect(err.Error()).To(ContainSubstring(`"pipeline.stage_timout" at CLI flag pipeline.stage_timout`))
			Expect(err.Error()).To(ContainSubstring(`"loging" at CLI flag loging.level`))
		})

		It("accepts a CLI flag override of a known path", func() {
			cfg, err := config.NewLoader().Load(context.Background(),
				config.WithOverrides(map[string]string{"logging.level": "debug"}))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.Logging.Level).To(Equal("debug"))
		})

		It("ignores a CROSSCODEX_ environment variable that is not a config path", func() {
			GinkgoT().Setenv("CROSSCODEX_LLM_API_KEY", "sk-test")
			GinkgoT().Setenv("CROSSCODEX_COLOR", "never")
			_, err := config.NewLoader().Load(context.Background())
			Expect(err).NotTo(HaveOccurred())
		})
	})

	It("accepts the compiled defaults", func() {
		node, err := config.ExportDefaultNode()
		Expect(err).NotTo(HaveOccurred())
		Expect(config.ExportCheckKnownKeys(node, "compiled defaults")).To(Succeed())
	})

	It("discovers the repository's configuration files", func() {
		Expect(repoConfigErr).NotTo(HaveOccurred())
		Expect(repoConfigs).To(ContainElements(
			"deploy/config.yaml",
			"deploy/backup.config.yaml",
			"test/e2e/crosscodexd.e2e.yaml",
			"test/e2e/crosscodexd.e2e.llm.yaml",
			"test/backup/config-local.yaml",
			"test/backup/config-s3.yaml",
		))
		Expect(repoConfigs).NotTo(ContainElement("test/compose.yaml"))
	})

	DescribeTable("accepts every configuration file shipped in the repository",
		func(relPath string) {
			path := filepath.Join(repoRoot, relPath)
			data, err := os.ReadFile(path)
			Expect(err).NotTo(HaveOccurred())
			var doc yaml.Node
			Expect(yaml.Unmarshal(data, &doc)).To(Succeed())
			Expect(doc.Content).NotTo(BeEmpty())
			Expect(config.ExportCheckKnownKeys(doc.Content[0], path)).To(Succeed())
		},
		repoConfigEntries(),
	)
})

// repoRoot is the repository root, relative to this package's directory,
// where the test binary runs.
var repoRoot = filepath.Join("..", "..")

// repoConfigs lists, relative to repoRoot, the configuration files
// discoverRepoConfigs found; repoConfigErr is why discovery failed, if it
// did. They are computed while the spec tree is built, so a spec asserts
// the error instead of discovery failing outside one.
var repoConfigs, repoConfigErr = discoverRepoConfigs()

func repoConfigEntries() []TableEntry {
	entries := make([]TableEntry, len(repoConfigs))
	for i, p := range repoConfigs {
		entries[i] = Entry(p, p)
	}
	return entries
}

// discoverRepoConfigs returns the deploy/*config*.yaml files and the YAML
// files anywhere under test/ whose top-level mapping sets at least one
// configuration key, so a new config file is checked without being listed.
// One configuration key is enough: a config file with a misspelled
// top-level key must fail the check, not drop out of discovery. Compose
// files and Venom suites set none and are skipped.
func discoverRepoConfigs() ([]string, error) {
	paths, err := filepath.Glob(filepath.Join(repoRoot, "deploy", "*config*.yaml"))
	if err != nil {
		return nil, err
	}
	err = filepath.WalkDir(filepath.Join(repoRoot, "test"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && (strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml")) {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	configKeys, _ := config.ExportYAMLFields(reflect.TypeFor[config.Config]())
	var found []string
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var doc yaml.Node
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", path, err)
		}
		if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
			continue
		}
		root := doc.Content[0]
		for i := 0; i+1 < len(root.Content); i += 2 {
			if _, ok := configKeys[root.Content[i].Value]; ok {
				rel, err := filepath.Rel(repoRoot, path)
				if err != nil {
					return nil, err
				}
				found = append(found, filepath.ToSlash(rel))
				break
			}
		}
	}
	return found, nil
}
