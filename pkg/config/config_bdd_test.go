package config_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/config"
)

func TestConfigBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Configuration System BDD Suite")
}

// Redirect slog output to GinkgoWriter so log noise only appears on failure.
var _ = BeforeSuite(func() { DeferCleanup(testspecs.RedirectLogsToGinkgo()) })

// writeTestFile is a helper that creates parent directories and writes content.
func writeTestFile(path, content string) {
	ExpectWithOffset(1, os.MkdirAll(filepath.Dir(path), 0o755)).To(Succeed())
	ExpectWithOffset(1, os.WriteFile(path, []byte(content), 0o644)).To(Succeed())
}

var _ = Describe("Configuration System", Ordered, func() {

	BeforeAll(func() {
		testspecs.LogTestProgress("Starting Configuration System BDD test suite")
	})

	AfterAll(func() {
		testspecs.LogTestProgress("Configuration System BDD test suite completed")
	})

	// =================================================================
	// LEVEL 1: BEHAVIORAL SPECIFICATIONS
	// These specs test the "why" - what business behaviors the configuration system supports
	// =================================================================

	Describe("Configuration Loading Behaviors", func() {
		Context("when users need predictable configuration precedence", func() {
			It("prioritizes overrides above all other configuration sources", func() {
				tmpHome := GinkgoT().TempDir()
				projectDir := GinkgoT().TempDir()
				GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)
				GinkgoT().Setenv("CROSSCODEX_LLM_GATEWAY_URL", "https://env:9000")

				testspecs.AssertNoError(os.MkdirAll(filepath.Join(tmpHome, "crosscodex"), 0o755))
				userConfigData := []byte("llm:\n  gateway_url: \"https://user:4000\"\n  timeout: 60\n")
				userConfigPath := filepath.Join(tmpHome, "crosscodex", "config.yaml")
				testspecs.AssertNoError(os.WriteFile(userConfigPath, userConfigData, 0o644))

				testspecs.AssertNoError(os.MkdirAll(filepath.Join(projectDir, ".crosscodex"), 0o755))
				projectConfigData := []byte("llm:\n  gateway_url: \"https://project:7000\"\n")
				projectConfigPath := filepath.Join(projectDir, ".crosscodex", "config.yaml")
				testspecs.AssertNoError(os.WriteFile(projectConfigPath, projectConfigData, 0o644))

				loader := config.NewLoader()
				cfg, err := loader.Load(context.Background(),
					config.WithProjectDir(projectDir),
					config.WithOverrides(map[string]string{
						"llm.gateway_url": "https://cli:1111",
					}),
				)
				testspecs.AssertNoError(err)

				Expect(cfg.LLM.GatewayURL).To(Equal("https://cli:1111"))
			})

			It("allows environment variables to override config files", func() {
				tmpHome := GinkgoT().TempDir()
				projectDir := GinkgoT().TempDir()
				GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)
				GinkgoT().Setenv("CROSSCODEX_LLM_GATEWAY_URL", "https://env:9000")

				testspecs.AssertNoError(os.MkdirAll(filepath.Join(tmpHome, "crosscodex"), 0o755))
				userConfigData := []byte("llm:\n  gateway_url: \"https://user:4000\"\n  timeout: 60\n")
				userConfigPath := filepath.Join(tmpHome, "crosscodex", "config.yaml")
				testspecs.AssertNoError(os.WriteFile(userConfigPath, userConfigData, 0o644))

				testspecs.AssertNoError(os.MkdirAll(filepath.Join(projectDir, ".crosscodex"), 0o755))
				projectConfigData := []byte("llm:\n  gateway_url: \"https://project:7000\"\n  timeout: 45\n")
				projectConfigPath := filepath.Join(projectDir, ".crosscodex", "config.yaml")
				testspecs.AssertNoError(os.WriteFile(projectConfigPath, projectConfigData, 0o644))

				loader := config.NewLoader()
				cfg, err := loader.Load(context.Background(),
					config.WithProjectDir(projectDir),
				)
				testspecs.AssertNoError(err)

				Expect(cfg.LLM.GatewayURL).To(Equal("https://env:9000"))
				Expect(cfg.LLM.Timeout).To(Equal(45))
			})

			It("supports project-specific overrides for team workflows", func() {
				tmpHome := GinkgoT().TempDir()
				projectDir := GinkgoT().TempDir()
				GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

				testspecs.AssertNoError(os.MkdirAll(filepath.Join(tmpHome, "crosscodex"), 0o755))
				userConfigData := []byte("llm:\n  gateway_url: \"https://user:4000\"\n  timeout: 60\n")
				testspecs.AssertNoError(os.WriteFile(filepath.Join(tmpHome, "crosscodex", "config.yaml"), userConfigData, 0o644))

				testspecs.AssertNoError(os.MkdirAll(filepath.Join(projectDir, ".crosscodex"), 0o755))
				projectConfigData := []byte("llm:\n  gateway_url: \"https://project:7000\"\n")
				testspecs.AssertNoError(os.WriteFile(filepath.Join(projectDir, ".crosscodex", "config.yaml"), projectConfigData, 0o644))

				loader := config.NewLoader()
				cfg, err := loader.Load(context.Background(), config.WithProjectDir(projectDir))
				testspecs.AssertNoError(err)

				Expect(cfg.LLM.GatewayURL).To(Equal("https://project:7000"))
				Expect(cfg.LLM.Timeout).To(Equal(60))
			})

			It("respects user configuration over system defaults", func() {
				tmpHome := GinkgoT().TempDir()
				GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

				testspecs.AssertNoError(os.MkdirAll(filepath.Join(tmpHome, "crosscodex"), 0o755))
				userConfigData := []byte("llm:\n  gateway_url: \"https://user:4000\"\n  timeout: 60\n")
				testspecs.AssertNoError(os.WriteFile(filepath.Join(tmpHome, "crosscodex", "config.yaml"), userConfigData, 0o644))

				loader := config.NewLoader()
				cfg, err := loader.Load(context.Background())
				testspecs.AssertNoError(err)

				Expect(cfg.LLM.GatewayURL).To(Equal("https://user:4000"))
				Expect(cfg.LLM.Timeout).To(Equal(60))
				Expect(cfg.Storage.Objects.Backend).To(Equal("local"))
			})

			It("gracefully falls back to sensible defaults", func() {
				tmpHome := GinkgoT().TempDir()
				GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

				loader := config.NewLoader()
				cfg, err := loader.Load(context.Background())
				testspecs.AssertNoError(err)

				Expect(cfg.LLM.Timeout).To(Equal(30))
				Expect(cfg.Storage.Objects.Backend).To(Equal("local"))
				Expect(cfg.TLS.Mode).To(Equal("off"))
				Expect(cfg.Database.SSLMode).To(Equal("prefer"))
				Expect(cfg.Logging.Level).To(Equal("info"))
				Expect(cfg.Logging.Format).To(Equal("text"))
			})
		})

		Context("when users need flexible configuration organization", func() {
			It("supports configuration profiles for different environments", func() {
				tmpHome := GinkgoT().TempDir()
				GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

				testspecs.AssertNoError(os.MkdirAll(filepath.Join(tmpHome, "crosscodex", "profiles"), 0o755))
				profileData := []byte("server:\n  workers: 2\nllm:\n  gateway_url: \"https://local:4000\"\n")
				testspecs.AssertNoError(os.WriteFile(filepath.Join(tmpHome, "crosscodex", "profiles", "local.yaml"), profileData, 0o644))

				loader := config.NewLoader()
				cfg, err := loader.Load(context.Background(), config.WithProfile("local"))
				testspecs.AssertNoError(err)

				Expect(cfg.Server.Workers).To(Equal(2))
				Expect(cfg.LLM.GatewayURL).To(Equal("https://local:4000"))
			})

			It("enables drop-in configuration modules for team sharing", func() {
				tmpHome := GinkgoT().TempDir()
				GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

				userDir := filepath.Join(tmpHome, "crosscodex")
				testspecs.AssertNoError(os.MkdirAll(userDir, 0o755))
				userConfigData := []byte("llm:\n  gateway_url: \"https://user:4000\"\n  timeout: 60\n")
				testspecs.AssertNoError(os.WriteFile(filepath.Join(userDir, "config.yaml"), userConfigData, 0o644))

				testspecs.AssertNoError(os.MkdirAll(filepath.Join(userDir, "conf.d"), 0o755))
				dropinData := []byte("llm:\n  gateway_url: \"https://dropin:5000\"\n")
				testspecs.AssertNoError(os.WriteFile(filepath.Join(userDir, "conf.d", "10-override.yaml"), dropinData, 0o644))

				loader := config.NewLoader()
				cfg, err := loader.Load(context.Background())
				testspecs.AssertNoError(err)

				Expect(cfg.LLM.GatewayURL).To(Equal("https://dropin:5000"))
				Expect(cfg.LLM.Timeout).To(Equal(60))
			})

			It("handles per-service TLS configuration for security requirements", func() {
				tmpHome := GinkgoT().TempDir()
				GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

				userDir := filepath.Join(tmpHome, "crosscodex")
				testspecs.AssertNoError(os.MkdirAll(userDir, 0o755))

				configData := []byte("tls:\n  mode: server-only\n  ca: /etc/ca.crt\n  cert: /etc/server.crt\n  key: /etc/server.key\n  targets:\n    ingestion:\n      mode: mutual\n")
				testspecs.AssertNoError(os.WriteFile(filepath.Join(userDir, "config.yaml"), configData, 0o644))

				loader := config.NewLoader()
				cfg, err := loader.Load(context.Background())
				testspecs.AssertNoError(err)

				Expect(cfg.TLS.Mode).To(Equal("server-only"))
				Expect(cfg.TLS.CA).To(Equal("/etc/ca.crt"))
				Expect(cfg.TLS.Cert).To(Equal("/etc/server.crt"))
				Expect(cfg.TLS.Key).To(Equal("/etc/server.key"))

				Expect(cfg.TLS.Targets).To(HaveKey("ingestion"))
				ingestion := cfg.TLS.Targets["ingestion"]
				Expect(ingestion.Mode).To(Equal("mutual"))
			})

			It("supports single-file configuration for simple deployments", func() {
				tmpHome := GinkgoT().TempDir()
				GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

				testspecs.AssertNoError(os.MkdirAll(filepath.Join(tmpHome, "crosscodex"), 0o755))
				userConfigData := []byte("llm:\n  gateway_url: \"https://user-should-be-skipped:4000\"\n")
				testspecs.AssertNoError(os.WriteFile(filepath.Join(tmpHome, "crosscodex", "config.yaml"), userConfigData, 0o644))

				singleDir := GinkgoT().TempDir()
				singleFile := filepath.Join(singleDir, "custom.yaml")
				singleConfigData := []byte("llm:\n  gateway_url: \"https://custom:8000\"\n")
				testspecs.AssertNoError(os.WriteFile(singleFile, singleConfigData, 0o644))

				loader := config.NewLoader()
				cfg, err := loader.Load(context.Background(), config.WithConfigPath(singleFile))
				testspecs.AssertNoError(err)

				Expect(cfg.LLM.GatewayURL).To(Equal("https://custom:8000"))
			})
		})

		Context("when configuration errors occur", func() {
			It("provides actionable feedback for missing required configuration", func() {
				tmpHome := GinkgoT().TempDir()
				GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

				loader := config.NewLoader()
				_, err := loader.Load(context.Background(), config.WithProfile("nonexistent"))
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, config.ErrProfileNotFound)).To(BeTrue())
				Expect(err.Error()).To(ContainSubstring("nonexistent"))
			})

			It("identifies the source file of configuration validation errors", func() {
				tmpHome := GinkgoT().TempDir()
				GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

				badFile := filepath.Join(tmpHome, "crosscodex", "conf.d", "99-bad.yaml")
				testspecs.AssertNoError(os.MkdirAll(filepath.Dir(badFile), 0o755))
				testspecs.AssertNoError(os.WriteFile(badFile, []byte("tls:\n  mode: \"invalid-mode\"\n"), 0o644))

				loader := config.NewLoader()
				_, err := loader.Load(context.Background())
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("99-bad.yaml"))
				Expect(err.Error()).To(ContainSubstring("invalid-mode"))
			})

			It("prevents system startup with invalid security configuration", func() {
				tmpHome := GinkgoT().TempDir()
				GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

				testspecs.AssertNoError(os.MkdirAll(filepath.Join(tmpHome, "crosscodex"), 0o755))
				testspecs.AssertNoError(os.WriteFile(
					filepath.Join(tmpHome, "crosscodex", "config.yaml"),
					[]byte("tls:\n  mode: \"bogus\"\n"), 0o644))

				loader := config.NewLoader()
				_, err := loader.Load(context.Background())
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
			})

			It("rejects malformed configuration files with helpful diagnostics", func() {
				tmpHome := GinkgoT().TempDir()
				GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

				testspecs.AssertNoError(os.MkdirAll(filepath.Join(tmpHome, "crosscodex"), 0o755))
				testspecs.AssertNoError(os.WriteFile(
					filepath.Join(tmpHome, "crosscodex", "config.yaml"),
					[]byte(":\n  - :\n  broken: [yaml\n"), 0o644))

				loader := config.NewLoader()
				_, err := loader.Load(context.Background())
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, config.ErrLoadFailed)).To(BeTrue())
			})
		})
	})

	Describe("Configuration Validation Behaviors", func() {
		Context("when enforcing system boundaries", func() {
			It("validates TLS configuration completeness for security compliance", func() {
				tmpHome := GinkgoT().TempDir()
				GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

				userDir := filepath.Join(tmpHome, "crosscodex")
				testspecs.AssertNoError(os.MkdirAll(userDir, 0o755))

				badConfig := []byte("tls:\n  mode: mutual\n  ca: /etc/ca.crt\nstorage:\n  objects:\n    backend: local\nlogging:\n  level: info\n  format: text\n")
				testspecs.AssertNoError(os.WriteFile(filepath.Join(userDir, "config.yaml"), badConfig, 0o644))

				loader := config.NewLoader()
				_, err := loader.Load(context.Background())
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())

				goodConfig := []byte("tls:\n  mode: mutual\n  ca: /etc/ca.crt\n  cert: /etc/server.crt\n  key: /etc/server.key\nstorage:\n  objects:\n    backend: local\nlogging:\n  level: info\n  format: text\n")
				testspecs.AssertNoError(os.WriteFile(filepath.Join(userDir, "config.yaml"), goodConfig, 0o644))

				loader = config.NewLoader()
				_, err = loader.Load(context.Background())
				Expect(err).NotTo(HaveOccurred())
			})

			It("restricts storage backends to supported implementations", func() {
				tmpHome := GinkgoT().TempDir()
				GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

				userDir := filepath.Join(tmpHome, "crosscodex")
				testspecs.AssertNoError(os.MkdirAll(userDir, 0o755))

				badConfig := []byte("storage:\n  objects:\n    backend: azure\ntls:\n  mode: \"off\"\nlogging:\n  level: info\n  format: text\n")
				testspecs.AssertNoError(os.WriteFile(filepath.Join(userDir, "config.yaml"), badConfig, 0o644))

				loader := config.NewLoader()
				_, err := loader.Load(context.Background())
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())

				goodConfig := []byte("storage:\n  objects:\n    backend: local\ntls:\n  mode: \"off\"\nlogging:\n  level: info\n  format: text\n")
				testspecs.AssertNoError(os.WriteFile(filepath.Join(userDir, "config.yaml"), goodConfig, 0o644))

				loader = config.NewLoader()
				_, err = loader.Load(context.Background())
				Expect(err).NotTo(HaveOccurred())
			})

			It("enforces logging configuration for audit requirements", func() {
				tmpHome := GinkgoT().TempDir()
				GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

				userDir := filepath.Join(tmpHome, "crosscodex")
				testspecs.AssertNoError(os.MkdirAll(userDir, 0o755))

				badLevelConfig := []byte("logging:\n  level: verbose\n  format: text\ntls:\n  mode: \"off\"\nstorage:\n  objects:\n    backend: local\n")
				testspecs.AssertNoError(os.WriteFile(filepath.Join(userDir, "config.yaml"), badLevelConfig, 0o644))

				loader := config.NewLoader()
				_, err := loader.Load(context.Background())
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())

				badFormatConfig := []byte("logging:\n  level: info\n  format: yaml\ntls:\n  mode: \"off\"\nstorage:\n  objects:\n    backend: local\n")
				testspecs.AssertNoError(os.WriteFile(filepath.Join(userDir, "config.yaml"), badFormatConfig, 0o644))

				loader = config.NewLoader()
				_, err = loader.Load(context.Background())
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
			})

			It("prevents profile path traversal attacks", func() {
				tmpHome := GinkgoT().TempDir()
				GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

				loader := config.NewLoader()
				_, err := loader.Load(context.Background(), config.WithProfile("../../etc/passwd"))
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
			})
		})
	})

	// =================================================================
	// LEVEL 2: INTERFACE COMPLIANCE SPECIFICATIONS
	// =================================================================

	Describe("Configuration Interface Compliance", func() {
		configAdapter := NewConfigAdapter()

		Context("as a configurable component", testspecs.ConfigurationComplianceBehavior(configAdapter))
	})

	// =================================================================
	// LEVEL 3: TECHNICAL EDGE CASES AND INTEGRATION SCENARIOS
	// =================================================================

	Describe("Configuration Loading Edge Cases", func() {
		Context("when testing complete precedence resolution", func() {
			It("applies the full precedence stack correctly", func() {
				tmpHome := GinkgoT().TempDir()
				projectDir := GinkgoT().TempDir()
				GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)
				GinkgoT().Setenv("CROSSCODEX_LOGGING_LEVEL", "debug")

				userDir := filepath.Join(tmpHome, "crosscodex")

				testspecs.AssertNoError(os.MkdirAll(userDir, 0o755))
				userConfigData := []byte("llm:\n  gateway_url: \"https://user:4000\"\n  timeout: 60\nlogging:\n  level: warn\n")
				testspecs.AssertNoError(os.WriteFile(filepath.Join(userDir, "config.yaml"), userConfigData, 0o644))

				testspecs.AssertNoError(os.MkdirAll(filepath.Join(userDir, "conf.d"), 0o755))
				dropinData := []byte("llm:\n  timeout: 45\n")
				testspecs.AssertNoError(os.WriteFile(filepath.Join(userDir, "conf.d", "10-team.yaml"), dropinData, 0o644))

				testspecs.AssertNoError(os.MkdirAll(filepath.Join(userDir, "profiles"), 0o755))
				profileData := []byte("server:\n  workers: 1\n")
				testspecs.AssertNoError(os.WriteFile(filepath.Join(userDir, "profiles", "local.yaml"), profileData, 0o644))

				testspecs.AssertNoError(os.MkdirAll(filepath.Join(projectDir, ".crosscodex"), 0o755))
				projectConfigData := []byte("llm:\n  gateway_url: \"https://project:7000\"\n")
				testspecs.AssertNoError(os.WriteFile(filepath.Join(projectDir, ".crosscodex", "config.yaml"), projectConfigData, 0o644))

				loader := config.NewLoader()
				cfg, err := loader.Load(context.Background(),
					config.WithProfile("local"),
					config.WithProjectDir(projectDir),
					config.WithOverrides(map[string]string{
						"nats.url": "nats://flag:4222",
					}),
				)
				testspecs.AssertNoError(err)

				Expect(cfg.LLM.GatewayURL).To(Equal("https://project:7000"))
				Expect(cfg.LLM.Timeout).To(Equal(45))
				Expect(cfg.Server.Workers).To(Equal(1))
				Expect(cfg.Logging.Level).To(Equal("debug"))
				Expect(cfg.NATS.URL).To(Equal("nats://flag:4222"))
				Expect(cfg.TLS.Mode).To(Equal("off"))
			})

			It("handles malformed YAML in drop-in files", func() {
				tmpHome := GinkgoT().TempDir()
				GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

				userDir := filepath.Join(tmpHome, "crosscodex")
				goodPath := filepath.Join(userDir, "conf.d", "10-good.yaml")
				testspecs.AssertNoError(os.MkdirAll(filepath.Dir(goodPath), 0o755))
				testspecs.AssertNoError(os.WriteFile(goodPath, []byte("llm:\n  timeout: 60\n"), 0o644))

				badPath := filepath.Join(userDir, "conf.d", "20-bad.yaml")
				testspecs.AssertNoError(os.WriteFile(badPath, []byte("not: [valid: yaml\n"), 0o644))

				loader := config.NewLoader()
				_, err := loader.Load(context.Background())
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, config.ErrLoadFailed)).To(BeTrue())
			})

			It("manages TLS per-target merging with global configuration", func() {
				tmpHome := GinkgoT().TempDir()
				GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

				userDir := filepath.Join(tmpHome, "crosscodex")
				testspecs.AssertNoError(os.MkdirAll(userDir, 0o755))

				baseConfigData := []byte(`tls:
  mode: server-only
  ca: /etc/ca.crt
  cert: /etc/server.crt
  key: /etc/server.key
  targets:
    ingestion:
      mode: mutual
`)
				testspecs.AssertNoError(os.WriteFile(filepath.Join(userDir, "config.yaml"), baseConfigData, 0o644))

				testspecs.AssertNoError(os.MkdirAll(filepath.Join(userDir, "conf.d"), 0o755))
				dropinConfigData := []byte(`tls:
  targets:
    ingestion:
      cert: /etc/ingestion.crt
      key: /etc/ingestion.key
    catalog:
      mode: mutual
      cert: /etc/catalog.crt
      key: /etc/catalog.key
`)
				testspecs.AssertNoError(os.WriteFile(filepath.Join(userDir, "conf.d", "10-tls.yaml"), dropinConfigData, 0o644))

				loader := config.NewLoader()
				cfg, err := loader.Load(context.Background())
				testspecs.AssertNoError(err)

				Expect(cfg.TLS.Mode).To(Equal("server-only"))
				Expect(cfg.TLS.CA).To(Equal("/etc/ca.crt"))

				Expect(cfg.TLS.Targets).To(HaveKey("ingestion"))
				ingestion := cfg.TLS.Targets["ingestion"]
				Expect(ingestion.Mode).To(Equal("mutual"))
				Expect(ingestion.Cert).To(Equal("/etc/ingestion.crt"))
				Expect(ingestion.Key).To(Equal("/etc/ingestion.key"))

				Expect(cfg.TLS.Targets).To(HaveKey("catalog"))
				catalog := cfg.TLS.Targets["catalog"]
				Expect(catalog.Mode).To(Equal("mutual"))
			})
		})

		Context("when testing validation edge cases", func() {
			It("returns the first validation error for multiple invalid fields", func() {
				tmpHome := GinkgoT().TempDir()
				GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

				userDir := filepath.Join(tmpHome, "crosscodex")
				testspecs.AssertNoError(os.MkdirAll(userDir, 0o755))

				badConfigData := []byte(`tls:
  mode: "bogus"
storage:
  objects:
    backend: "azure"
logging:
  level: "verbose"
  format: "yaml"
`)
				testspecs.AssertNoError(os.WriteFile(filepath.Join(userDir, "config.yaml"), badConfigData, 0o644))

				loader := config.NewLoader()
				_, err := loader.Load(context.Background())

				Expect(err).To(HaveOccurred())
				Expect(err).To(MatchError(ContainSubstring("invalid")))

				errMsg := err.Error()
				Expect(errMsg).To(ContainSubstring("tls.mode"))
				Expect(errMsg).NotTo(ContainSubstring("storage.objects.backend"))
				Expect(errMsg).NotTo(ContainSubstring("logging.level"))
			})

			It("validates all TLS mode configurations comprehensively", func() {
				// Case 1: mutual TLS with missing cert and key
				tmpHome := GinkgoT().TempDir()
				GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

				userDir := filepath.Join(tmpHome, "crosscodex")
				testspecs.AssertNoError(os.MkdirAll(userDir, 0o755))
				mutualNoCertKey := []byte("tls:\n  mode: mutual\n  ca: /etc/ca.crt\n")
				testspecs.AssertNoError(os.WriteFile(filepath.Join(userDir, "config.yaml"), mutualNoCertKey, 0o644))

				loader := config.NewLoader()
				_, err := loader.Load(context.Background())
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())

				// Case 2: server-only TLS with missing cert
				tmpHome2 := GinkgoT().TempDir()
				GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome2)

				userDir2 := filepath.Join(tmpHome2, "crosscodex")
				testspecs.AssertNoError(os.MkdirAll(userDir2, 0o755))
				serverNoCert := []byte("tls:\n  mode: server-only\n  key: /etc/server.key\n")
				testspecs.AssertNoError(os.WriteFile(filepath.Join(userDir2, "config.yaml"), serverNoCert, 0o644))

				loader = config.NewLoader()
				_, err = loader.Load(context.Background())
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())

				// Case 3: server-only TLS with missing key
				tmpHome3 := GinkgoT().TempDir()
				GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome3)

				userDir3 := filepath.Join(tmpHome3, "crosscodex")
				testspecs.AssertNoError(os.MkdirAll(userDir3, 0o755))
				serverNoKey := []byte("tls:\n  mode: server-only\n  cert: /etc/server.crt\n")
				testspecs.AssertNoError(os.WriteFile(filepath.Join(userDir3, "config.yaml"), serverNoKey, 0o644))

				loader = config.NewLoader()
				_, err = loader.Load(context.Background())
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())

				// Case 4: mutual TLS missing CA
				tmpHome4 := GinkgoT().TempDir()
				GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome4)

				userDir4 := filepath.Join(tmpHome4, "crosscodex")
				testspecs.AssertNoError(os.MkdirAll(userDir4, 0o755))
				mutualNoCA := []byte("tls:\n  mode: mutual\n  cert: /etc/server.crt\n  key: /etc/server.key\n")
				testspecs.AssertNoError(os.WriteFile(filepath.Join(userDir4, "config.yaml"), mutualNoCA, 0o644))

				loader = config.NewLoader()
				_, err = loader.Load(context.Background())
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
			})
		})

		Context("when testing XDG compliance edge cases", func() {
			It("correctly resolves XDG paths with custom environment variables", func() {
				tmpHome := GinkgoT().TempDir()
				GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

				userDir := filepath.Join(tmpHome, "crosscodex")
				testspecs.AssertNoError(os.MkdirAll(userDir, 0o755))
				configData := []byte("llm:\n  gateway_url: \"https://xdg-custom:9000\"\n  timeout: 99\n")
				testspecs.AssertNoError(os.WriteFile(filepath.Join(userDir, "config.yaml"), configData, 0o644))

				loader := config.NewLoader()
				cfg, err := loader.Load(context.Background())
				testspecs.AssertNoError(err)

				Expect(cfg.LLM.GatewayURL).To(Equal("https://xdg-custom:9000"))
				Expect(cfg.LLM.Timeout).To(Equal(99))

				tmpHome2 := GinkgoT().TempDir()
				GinkgoT().Setenv("XDG_CONFIG_HOME", "")
				GinkgoT().Setenv("HOME", tmpHome2)

				fallbackDir := filepath.Join(tmpHome2, ".config", "crosscodex")
				testspecs.AssertNoError(os.MkdirAll(fallbackDir, 0o755))
				fallbackData := []byte("llm:\n  gateway_url: \"https://home-fallback:8000\"\n  timeout: 77\n")
				testspecs.AssertNoError(os.WriteFile(filepath.Join(fallbackDir, "config.yaml"), fallbackData, 0o644))

				loader = config.NewLoader()
				cfg, err = loader.Load(context.Background())
				testspecs.AssertNoError(err)

				Expect(cfg.LLM.GatewayURL).To(Equal("https://home-fallback:8000"))
				Expect(cfg.LLM.Timeout).To(Equal(77))
			})

			It("properly handles profile path validation", func() {
				invalidProfiles := []struct {
					name    string
					profile string
				}{
					{"path traversal", "../../etc/passwd"},
					{"embedded slash", "foo/bar"},
					{"backslash traversal", "..\\..\\etc\\passwd"},
				}

				for _, tc := range invalidProfiles {
					By(fmt.Sprintf("rejecting profile name %q (%s)", tc.profile, tc.name))
					tmpHome := GinkgoT().TempDir()
					GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

					loader := config.NewLoader()
					_, err := loader.Load(context.Background(), config.WithProfile(tc.profile))
					Expect(err).To(HaveOccurred(), "expected error for profile %q", tc.profile)
					Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue(),
						"expected ErrInvalidConfig for profile %q, got: %v", tc.profile, err)
				}
			})
		})

		Context("when testing service and CLI configuration", func() {
			It("provides correctly configured ServiceConfig", func() {
				tmpHome := GinkgoT().TempDir()
				GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

				loader := config.NewLoader()
				cfg, err := loader.Load(context.Background())
				testspecs.AssertNoError(err)

				sc := cfg.ServiceConfig()
				Expect(sc.GRPCAddr).To(Equal(":50051"))
			})

			It("provides correctly configured CLIConfig", func() {
				tmpHome := GinkgoT().TempDir()
				GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

				loader := config.NewLoader()
				cfg, err := loader.Load(context.Background())
				testspecs.AssertNoError(err)

				cc := cfg.CLIConfig()
				Expect(cc.Output).To(Equal("table"))
			})
		})
	})

	Describe("Integration Scenarios", func() {
		Context("when testing environment variable integration", func() {
			It("correctly processes all environment variable bindings", func() {
				tmpHome := GinkgoT().TempDir()
				GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)
				GinkgoT().Setenv("CROSSCODEX_LLM_GATEWAY_URL", "https://env:9000")
				GinkgoT().Setenv("CROSSCODEX_LLM_TIMEOUT", "120")
				GinkgoT().Setenv("CROSSCODEX_STORAGE_OBJECTS_BACKEND", "s3")
				GinkgoT().Setenv("CROSSCODEX_TLS_FIPS_ENABLED", "true")

				loader := config.NewLoader()
				cfg, err := loader.Load(context.Background())
				testspecs.AssertNoError(err)

				Expect(cfg.LLM.GatewayURL).To(Equal("https://env:9000"))
				Expect(cfg.LLM.Timeout).To(Equal(120))
				Expect(cfg.Storage.Objects.Backend).To(Equal("s3"))
				Expect(cfg.TLS.FIPS.Enabled).To(BeTrue())
			})
		})

		Context("when testing NATS configuration integration", func() {
			It("validates NATS configuration parameters", func() {
				tmpHome := GinkgoT().TempDir()
				GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

				userDir := filepath.Join(tmpHome, "crosscodex")
				testspecs.AssertNoError(os.MkdirAll(userDir, 0o755))

				natsConfig := []byte(`nats:
  url: "nats://nats.example.com:4222"
  cluster: "prod"
  tls: true
  embedded:
    store_dir: "/var/lib/crosscodex/nats"
  streams:
    audit_llm_retention: 4320h
    audit_events_retention: 168h
`)
				testspecs.AssertNoError(os.WriteFile(filepath.Join(userDir, "config.yaml"), natsConfig, 0o644))

				loader := config.NewLoader()
				cfg, err := loader.Load(context.Background())
				testspecs.AssertNoError(err)

				Expect(cfg.NATS.URL).To(Equal("nats://nats.example.com:4222"))
				Expect(cfg.NATS.Cluster).To(Equal("prod"))
				Expect(cfg.NATS.TLS).To(BeTrue())
				Expect(cfg.NATS.Embedded.StoreDir).To(Equal("/var/lib/crosscodex/nats"))
				Expect(cfg.NATS.Streams.AuditLLMRetention).To(Equal(4320 * time.Hour))
				Expect(cfg.NATS.Streams.AuditEventsRetention).To(Equal(168 * time.Hour))
			})
		})

		Context("when testing configuration helpers", func() {
			It("provides correct helper functionality for test setup", func() {
				loader := config.NewLoader()
				Expect(loader).NotTo(BeNil())

				tmpHome := GinkgoT().TempDir()
				GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

				cfg, err := loader.Load(context.Background(),
					config.WithConfigPath("/nonexistent/path/config.yaml"),
				)
				testspecs.AssertNoError(err)
				Expect(cfg.TLS.Mode).To(Equal("off"))
			})
		})
	})

	// =================================================================
	// LEVEL 4: INTERNAL FUNCTION SPECIFICATIONS
	// Ported from old-style tests that exercised unexported functions directly
	// =================================================================

	Describe("internal: defaults", func() {
		It("has sensible default values for all fields", func() {
			node, err := config.ExportDefaultNode()
			Expect(err).NotTo(HaveOccurred())

			type defaultsCfg struct {
				LLM struct {
					Timeout int `yaml:"timeout"`
				} `yaml:"llm"`
				Storage struct {
					Objects struct {
						Backend string `yaml:"backend"`
					} `yaml:"objects"`
				} `yaml:"storage"`
				TLS struct {
					Mode string `yaml:"mode"`
				} `yaml:"tls"`
				Database struct {
					MaxConns int    `yaml:"max_conns"`
					SSLMode  string `yaml:"ssl_mode"`
				} `yaml:"database"`
				Server struct {
					GRPCAddr string `yaml:"grpc_addr"`
					HTTPAddr string `yaml:"http_addr"`
					Workers  int    `yaml:"workers"`
				} `yaml:"server"`
				CLI struct {
					Output string `yaml:"output"`
				} `yaml:"cli"`
				Logging struct {
					Level  string `yaml:"level"`
					Format string `yaml:"format"`
				} `yaml:"logging"`
			}
			cfg := config.ExportMustUnmarshalNode[defaultsCfg](node)

			Expect(cfg.LLM.Timeout).To(Equal(30))
			Expect(cfg.Storage.Objects.Backend).To(Equal("local"))
			Expect(cfg.TLS.Mode).To(Equal("off"))
			Expect(cfg.Database.MaxConns).To(Equal(10))
			Expect(cfg.Database.SSLMode).To(Equal("prefer"))
			Expect(cfg.Server.GRPCAddr).To(Equal(":50051"))
			Expect(cfg.Server.HTTPAddr).To(Equal(":8080"))
			Expect(cfg.Server.Workers).To(Equal(4))
			Expect(cfg.CLI.Output).To(Equal("table"))
			Expect(cfg.Logging.Level).To(Equal("info"))
			Expect(cfg.Logging.Format).To(Equal("text"))
		})
	})

	Describe("internal: drop-ins", func() {
		It("applies drop-ins in lexicographic order", func() {
			dir := GinkgoT().TempDir()
			writeTestFile(filepath.Join(dir, "10-base.yaml"), "llm:\n  gateway_url: \"https://base:4000\"\n  timeout: 30\n")
			writeTestFile(filepath.Join(dir, "20-override.yaml"), "llm:\n  gateway_url: \"https://override:5000\"\n")

			result, err := config.ExportLoadDropIns(dir)
			Expect(err).NotTo(HaveOccurred())

			type dropinCfg struct {
				LLM struct {
					GatewayURL string `yaml:"gateway_url"`
					Timeout    int    `yaml:"timeout"`
				} `yaml:"llm"`
			}
			cfg := config.ExportMustUnmarshalNode[dropinCfg](result)

			Expect(cfg.LLM.GatewayURL).To(Equal("https://override:5000"))
			Expect(cfg.LLM.Timeout).To(Equal(30))
		})

		It("skips non-YAML files", func() {
			dir := GinkgoT().TempDir()
			writeTestFile(filepath.Join(dir, "10-base.yaml"), "llm:\n  gateway_url: \"https://base:4000\"\n")
			writeTestFile(filepath.Join(dir, "README.md"), "This is not config")
			writeTestFile(filepath.Join(dir, "backup.yaml.bak"), "llm:\n  gateway_url: \"https://should-be-ignored:4000\"\n")

			result, err := config.ExportLoadDropIns(dir)
			Expect(err).NotTo(HaveOccurred())

			type dropinCfg struct {
				LLM struct {
					GatewayURL string `yaml:"gateway_url"`
				} `yaml:"llm"`
			}
			cfg := config.ExportMustUnmarshalNode[dropinCfg](result)

			Expect(cfg.LLM.GatewayURL).To(Equal("https://base:4000"))
		})

		It("returns nil for an empty directory", func() {
			dir := GinkgoT().TempDir()
			result, err := config.ExportLoadDropIns(dir)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(BeNil())
		})

		It("returns nil for a missing directory", func() {
			result, err := config.ExportLoadDropIns("/nonexistent/path/conf.d")
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(BeNil())
		})

		It("returns an error for malformed YAML in drop-in files", func() {
			dir := GinkgoT().TempDir()
			writeTestFile(filepath.Join(dir, "10-good.yaml"), "llm:\n  timeout: 30\n")
			writeTestFile(filepath.Join(dir, "20-bad.yaml"), ":\n  - :\n  invalid: [yaml\n")

			_, err := config.ExportLoadDropIns(dir)
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("internal: environment variable binding", func() {
		It("overrides scalar values from env vars", func() {
			GinkgoT().Setenv("CROSSCODEX_LLM_GATEWAY_URL", "https://env:9000")
			GinkgoT().Setenv("CROSSCODEX_LLM_TIMEOUT", "60")

			base := config.ExportMustParseYAML("llm:\n  gateway_url: \"https://file:4000\"\n  timeout: 30\n")

			result, err := config.ExportApplyEnvVars(base, "CROSSCODEX")
			Expect(err).NotTo(HaveOccurred())

			type envCfg struct {
				LLM struct {
					GatewayURL string `yaml:"gateway_url"`
					Timeout    int    `yaml:"timeout"`
				} `yaml:"llm"`
			}
			cfg := config.ExportMustUnmarshalNode[envCfg](result)

			Expect(cfg.LLM.GatewayURL).To(Equal("https://env:9000"))
			Expect(cfg.LLM.Timeout).To(Equal(60))
		})

		It("overrides nested path values", func() {
			GinkgoT().Setenv("CROSSCODEX_STORAGE_OBJECTS_BACKEND", "s3")
			GinkgoT().Setenv("CROSSCODEX_STORAGE_OBJECTS_BUCKET", "my-bucket")

			base := config.ExportMustParseYAML("storage:\n  objects:\n    backend: local\n")

			result, err := config.ExportApplyEnvVars(base, "CROSSCODEX")
			Expect(err).NotTo(HaveOccurred())

			type nestedCfg struct {
				Storage struct {
					Objects struct {
						Backend string `yaml:"backend"`
						Bucket  string `yaml:"bucket"`
					} `yaml:"objects"`
				} `yaml:"storage"`
			}
			cfg := config.ExportMustUnmarshalNode[nestedCfg](result)

			Expect(cfg.Storage.Objects.Backend).To(Equal("s3"))
			Expect(cfg.Storage.Objects.Bucket).To(Equal("my-bucket"))
		})

		It("preserves values when no matching env vars are set", func() {
			base := config.ExportMustParseYAML("llm:\n  gateway_url: \"https://file:4000\"\n")

			result, err := config.ExportApplyEnvVars(base, "CROSSCODEX")
			Expect(err).NotTo(HaveOccurred())

			type preserveCfg struct {
				LLM struct {
					GatewayURL string `yaml:"gateway_url"`
				} `yaml:"llm"`
			}
			cfg := config.ExportMustUnmarshalNode[preserveCfg](result)

			Expect(cfg.LLM.GatewayURL).To(Equal("https://file:4000"))
		})

		It("handles boolean env var overrides", func() {
			GinkgoT().Setenv("CROSSCODEX_TLS_FIPS_ENABLED", "true")

			base := config.ExportMustParseYAML("tls:\n  fips:\n    enabled: false\n")

			result, err := config.ExportApplyEnvVars(base, "CROSSCODEX")
			Expect(err).NotTo(HaveOccurred())

			type boolCfg struct {
				TLS struct {
					FIPS struct {
						Enabled bool `yaml:"enabled"`
					} `yaml:"fips"`
				} `yaml:"tls"`
			}
			cfg := config.ExportMustUnmarshalNode[boolCfg](result)

			Expect(cfg.TLS.FIPS.Enabled).To(BeTrue())
		})

		It("handles non-numeric values for integer fields", func() {
			GinkgoT().Setenv("CROSSCODEX_LLM_TIMEOUT", "not-a-number")

			base := config.ExportMustParseYAML("llm:\n  timeout: 30\n")

			result, err := config.ExportApplyEnvVars(base, "CROSSCODEX")
			Expect(err).NotTo(HaveOccurred())

			type intCfg struct {
				LLM struct {
					Timeout interface{} `yaml:"timeout"`
				} `yaml:"llm"`
			}
			cfg := config.ExportMustUnmarshalNode[intCfg](result)

			Expect(cfg.LLM.Timeout).NotTo(Equal(30))
		})

		It("handles nil base node", func() {
			GinkgoT().Setenv("CROSSCODEX_LLM_GATEWAY_URL", "https://env:9000")

			result, err := config.ExportApplyEnvVars(nil, "CROSSCODEX")
			Expect(err).NotTo(HaveOccurred())

			type nilBaseCfg struct {
				LLM struct {
					GatewayURL string `yaml:"gateway_url"`
				} `yaml:"llm"`
			}
			cfg := config.ExportMustUnmarshalNode[nilBaseCfg](result)

			Expect(cfg.LLM.GatewayURL).To(Equal("https://env:9000"))
		})

		Context("when testing inferTag", func() {
			It("correctly infers YAML tags for various inputs", func() {
				schema := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: "0"}

				Expect(config.ExportInferTag(schema, "-5")).To(Equal("!!int"))
				Expect(config.ExportInferTag(schema, "+10")).To(Equal("!!int"))
				Expect(config.ExportInferTag(schema, "")).To(Equal("!!str"))
				Expect(config.ExportInferTag(schema, "-")).To(Equal("!!str"))
				Expect(config.ExportInferTag(schema, "abc")).To(Equal("!!str"))
			})
		})
	})

	Describe("internal: deep merge", func() {
		It("overrides scalar values from overlay", func() {
			base := config.ExportMustParseYAML("llm:\n  gateway_url: \"https://base:4000\"\n  timeout: 30\n")
			overlay := config.ExportMustParseYAML("llm:\n  gateway_url: \"https://overlay:5000\"\n")

			merged, err := config.ExportDeepMerge(base, overlay)
			Expect(err).NotTo(HaveOccurred())

			type mergeCfg struct {
				LLM struct {
					GatewayURL string `yaml:"gateway_url"`
					Timeout    int    `yaml:"timeout"`
				} `yaml:"llm"`
			}
			cfg := config.ExportMustUnmarshalNode[mergeCfg](merged)

			Expect(cfg.LLM.GatewayURL).To(Equal("https://overlay:5000"))
			Expect(cfg.LLM.Timeout).To(Equal(30))
		})

		It("adds new keys from overlay", func() {
			base := config.ExportMustParseYAML("llm:\n  gateway_url: \"https://base:4000\"\n")
			overlay := config.ExportMustParseYAML("storage:\n  objects:\n    backend: s3\n")

			merged, err := config.ExportDeepMerge(base, overlay)
			Expect(err).NotTo(HaveOccurred())

			type addKeyCfg struct {
				LLM struct {
					GatewayURL string `yaml:"gateway_url"`
				} `yaml:"llm"`
				Storage struct {
					Objects struct {
						Backend string `yaml:"backend"`
					} `yaml:"objects"`
				} `yaml:"storage"`
			}
			cfg := config.ExportMustUnmarshalNode[addKeyCfg](merged)

			Expect(cfg.LLM.GatewayURL).To(Equal("https://base:4000"))
			Expect(cfg.Storage.Objects.Backend).To(Equal("s3"))
		})

		It("replaces slices from overlay", func() {
			base := config.ExportMustParseYAML("database:\n  extensions:\n    - age\n    - vector\n")
			overlay := config.ExportMustParseYAML("database:\n  extensions:\n    - pgcrypto\n")

			merged, err := config.ExportDeepMerge(base, overlay)
			Expect(err).NotTo(HaveOccurred())

			type sliceCfg struct {
				Database struct {
					Extensions []string `yaml:"extensions"`
				} `yaml:"database"`
			}
			cfg := config.ExportMustUnmarshalNode[sliceCfg](merged)

			Expect(cfg.Database.Extensions).To(HaveLen(1))
			Expect(cfg.Database.Extensions[0]).To(Equal("pgcrypto"))
		})

		It("merges nested maps recursively", func() {
			base := config.ExportMustParseYAML("tls:\n  mode: server-only\n  ca: /etc/ca.crt\n  targets:\n    ingestion:\n      mode: mutual\n")
			overlay := config.ExportMustParseYAML("tls:\n  targets:\n    ingestion:\n      cert: /etc/ingestion.crt\n    catalog:\n      mode: mutual\n")

			merged, err := config.ExportDeepMerge(base, overlay)
			Expect(err).NotTo(HaveOccurred())

			type nestedMapCfg struct {
				TLS struct {
					Mode    string                        `yaml:"mode"`
					CA      string                        `yaml:"ca"`
					Targets map[string]config.TLSOverride `yaml:"targets"`
				} `yaml:"tls"`
			}
			cfg := config.ExportMustUnmarshalNode[nestedMapCfg](merged)

			Expect(cfg.TLS.Mode).To(Equal("server-only"))
			Expect(cfg.TLS.CA).To(Equal("/etc/ca.crt"))
			Expect(cfg.TLS.Targets["ingestion"].Mode).To(Equal("mutual"))
			Expect(cfg.TLS.Targets["ingestion"].Cert).To(Equal("/etc/ingestion.crt"))
			Expect(cfg.TLS.Targets["catalog"].Mode).To(Equal("mutual"))
		})

		It("handles nil base", func() {
			overlay := config.ExportMustParseYAML("llm:\n  gateway_url: \"https://new:4000\"\n")

			merged, err := config.ExportDeepMerge(nil, overlay)
			Expect(err).NotTo(HaveOccurred())

			type nilBaseCfg struct {
				LLM struct {
					GatewayURL string `yaml:"gateway_url"`
				} `yaml:"llm"`
			}
			cfg := config.ExportMustUnmarshalNode[nilBaseCfg](merged)

			Expect(cfg.LLM.GatewayURL).To(Equal("https://new:4000"))
		})

		It("handles nil overlay", func() {
			base := config.ExportMustParseYAML("llm:\n  gateway_url: \"https://base:4000\"\n")

			merged, err := config.ExportDeepMerge(base, nil)
			Expect(err).NotTo(HaveOccurred())

			type nilOverlayCfg struct {
				LLM struct {
					GatewayURL string `yaml:"gateway_url"`
				} `yaml:"llm"`
			}
			cfg := config.ExportMustUnmarshalNode[nilOverlayCfg](merged)

			Expect(cfg.LLM.GatewayURL).To(Equal("https://base:4000"))
		})

		It("returns nil when both inputs are nil", func() {
			merged, err := config.ExportDeepMerge(nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(merged).To(BeNil())
		})
	})

	Describe("internal: NATS config deserialization", func() {
		DescribeTable("deserializes NATS config correctly",
			func(yamlInput string, wantURL string, wantDir string, wantLLM time.Duration, wantEvt time.Duration) {
				var cfg config.NATSConfig
				Expect(yaml.Unmarshal([]byte(yamlInput), &cfg)).To(Succeed())
				Expect(cfg.URL).To(Equal(wantURL))
				Expect(cfg.Embedded.StoreDir).To(Equal(wantDir))
				Expect(cfg.Streams.AuditLLMRetention).To(Equal(wantLLM))
				Expect(cfg.Streams.AuditEventsRetention).To(Equal(wantEvt))
			},
			Entry("embedded mode defaults",
				"url: \"\"\nembedded:\n  store_dir: \"\"\nstreams:\n  audit_llm_retention: 2160h\n  audit_events_retention: 720h\n",
				"", "", 2160*time.Hour, 720*time.Hour),
			Entry("external mode with custom retention",
				"url: \"nats://nats.example.com:4222\"\ncluster: \"prod\"\ntls: true\nembedded:\n  store_dir: \"/var/lib/crosscodex/nats\"\nstreams:\n  audit_llm_retention: 4320h\n  audit_events_retention: 168h\n",
				"nats://nats.example.com:4222", "/var/lib/crosscodex/nats", 4320*time.Hour, 168*time.Hour),
		)
	})

	Describe("internal: options", func() {
		It("WithConfigPath sets configPath", func() {
			o := config.ExportApplyOption(config.WithConfigPath("/etc/crosscodex/config.yaml"))
			Expect(o.ConfigPath).To(Equal("/etc/crosscodex/config.yaml"))
		})

		It("WithEnvPrefix sets envPrefix", func() {
			o := config.ExportApplyOption(config.WithEnvPrefix("CROSSCODEX"))
			Expect(o.EnvPrefix).To(Equal("CROSSCODEX"))
		})

		It("WithProfile sets profile", func() {
			o := config.ExportApplyOption(config.WithProfile("local"))
			Expect(o.Profile).To(Equal("local"))
		})

		It("WithProjectDir sets projectDir", func() {
			o := config.ExportApplyOption(config.WithProjectDir("/tmp/myproject"))
			Expect(o.ProjectDir).To(Equal("/tmp/myproject"))
		})

		It("WithOverrides sets overrides", func() {
			o := config.ExportApplyOption(config.WithOverrides(map[string]string{
				"llm.gateway_url": "https://override:4000",
				"tls.mode":        "off",
			}))
			Expect(o.Overrides).To(HaveLen(2))
			Expect(o.Overrides["llm.gateway_url"]).To(Equal("https://override:4000"))
		})

		It("applies multiple options correctly", func() {
			o := config.ExportApplyOptions(
				config.WithConfigPath("/tmp/config.yaml"),
				config.WithEnvPrefix("TEST"),
			)
			Expect(o.ConfigPath).To(Equal("/tmp/config.yaml"))
			Expect(o.EnvPrefix).To(Equal("TEST"))
		})

		It("last write wins for the same option", func() {
			o := config.ExportApplyOptions(
				config.WithConfigPath("/first"),
				config.WithConfigPath("/second"),
			)
			Expect(o.ConfigPath).To(Equal("/second"))
		})
	})

	Describe("internal: provenance tracking", func() {
		It("tracks and retrieves source of config values", func() {
			tracker := config.ExportNewSrcTracker()
			node := config.ExportMustParseYAML("tls:\n  mode: mutual\n  ca: /etc/ca.crt\n")
			tracker.Track(node, "/etc/crosscodex/config.yaml")

			Expect(tracker.SourceOf("tls.mode")).To(Equal("/etc/crosscodex/config.yaml"))
			Expect(tracker.SourceOf("tls.ca")).To(Equal("/etc/crosscodex/config.yaml"))
		})

		It("later layer overwrites source of earlier layer", func() {
			tracker := config.ExportNewSrcTracker()

			base := config.ExportMustParseYAML("tls:\n  mode: off\n")
			tracker.Track(base, "/etc/crosscodex/config.yaml")

			overlay := config.ExportMustParseYAML("tls:\n  mode: mutual\n")
			tracker.Track(overlay, "/home/user/.config/crosscodex/conf.d/10-tls.yaml")

			Expect(tracker.SourceOf("tls.mode")).To(Equal("/home/user/.config/crosscodex/conf.d/10-tls.yaml"))
		})

		It("returns 'compiled defaults' for unknown paths", func() {
			tracker := config.ExportNewSrcTracker()
			Expect(tracker.SourceOf("nonexistent.key")).To(Equal("compiled defaults"))
		})

		It("returns empty string for nil tracker", func() {
			Expect(config.ExportNilSourceTrackerSourceOf("anything")).To(BeEmpty())
		})

		It("formatSource returns parenthetical annotation with tracker", func() {
			tracker := config.ExportNewSrcTracker()
			node := config.ExportMustParseYAML("tls:\n  mode: bogus\n")
			tracker.Track(node, "/tmp/bad.yaml")

			Expect(config.ExportFormatSourceWithTracker(tracker, "tls.mode")).To(Equal(" (set in /tmp/bad.yaml)"))
		})

		It("formatSource returns empty string with nil tracker", func() {
			Expect(config.ExportFormatSourceNil("tls.mode")).To(BeEmpty())
		})
	})

	Describe("internal: types", func() {
		It("ServiceConfig returns correct daemon view", func() {
			cfg := config.Config{
				LLM: config.LLMConfig{
					GatewayURL:     "http://localhost:4000", // DevSkim: ignore DS162092 -- test fixture
					DefaultModel:   "qwen3:8b",
					EmbeddingModel: "qwen3-embedding",
					Timeout:        30,
				},
				Server: config.ServerConfig{
					GRPCAddr: ":50051",
					HTTPAddr: ":8080",
					Workers:  4,
				},
				Storage: config.StorageConfig{
					Objects: config.ObjectStorageConfig{
						Backend:  "local",
						BasePath: "/var/lib/crosscodex",
					},
				},
				Database: config.DatabaseConfig{
					DSN:        "postgres://localhost:5432/crosscodex", // DevSkim: ignore DS162092 -- test fixture
					Extensions: []string{"age", "vector"},
				},
			}

			sc := cfg.ServiceConfig()

			Expect(sc.GRPCAddr).To(Equal(":50051"))
			Expect(sc.HTTPAddr).To(Equal(":8080"))
			Expect(sc.Workers).To(Equal(4))
			Expect(sc.LLM.GatewayURL).To(Equal("http://localhost:4000"))              // DevSkim: ignore DS162092 -- test fixture
			Expect(sc.Database.DSN).To(Equal("postgres://localhost:5432/crosscodex")) // DevSkim: ignore DS162092 -- test fixture
		})

		It("CLIConfig returns correct client view", func() {
			cfg := config.Config{
				LLM: config.LLMConfig{
					GatewayURL:   "http://localhost:4000", // DevSkim: ignore DS162092 -- test fixture
					DefaultModel: "qwen3:8b",
					Timeout:      30,
				},
				CLI: config.CLISettings{
					Output:   "table",
					NoColor:  true,
					Endpoint: "http://localhost:8080", // DevSkim: ignore DS162092 -- test fixture
				},
			}

			cc := cfg.CLIConfig()

			Expect(cc.Output).To(Equal("table"))
			Expect(cc.NoColor).To(BeTrue())
			Expect(cc.Endpoint).To(Equal("http://localhost:8080"))       // DevSkim: ignore DS162092 -- test fixture
			Expect(cc.LLM.GatewayURL).To(Equal("http://localhost:4000")) // DevSkim: ignore DS162092 -- test fixture
		})
	})

	Describe("internal: validate", func() {
		It("accepts a valid config", func() {
			cfg := &config.Config{
				LLM: config.LLMConfig{
					GatewayURL: "http://localhost:4000", // DevSkim: ignore DS162092 -- test fixture
					Timeout:    30,
				},
				Storage: config.StorageConfig{
					Objects: config.ObjectStorageConfig{Backend: "local"},
				},
				TLS: config.TLSConfig{Mode: "off"},
				Database: config.DatabaseConfig{
					DSN:     "postgres://localhost:5432/crosscodex", // DevSkim: ignore DS162092 -- test fixture
					SSLMode: "prefer",
				},
				Logging:     config.LoggingConfig{Level: "info", Format: "text"},
				Attestation: config.AttestationConfig{ExpiryDuration: 8760 * time.Hour},
				Analysis: config.AnalysisConfig{
					Engine:         config.EngineConfig{TaskTimeout: 5 * time.Minute, MaxRetries: 3, RetryBackoff: time.Second},
					Classification: config.ClassificationConfig{MaxTextLength: 2000, MaxTokens: 20},
					Embedding:      config.EmbeddingConfig{Enabled: true, Models: []string{"snowflake-arctic-embed2"}, MaxChars: 1500, BatchSize: 50},
					Relationship:   config.RelationshipConfig{TopK: 20, MaxSourceChars: 1500, MaxTargetChars: 800, MaxTokens: 300, SamplesPerModel: 1, SamplingTemperature: 0.3},
				},
				Synthesis: config.SynthesisConfig{
					ConfidenceThreshold:   0.5,
					MaxMappingsPerControl: 10,
					Viability:             config.ViabilityConfig{TypeMismatchFactor: 0.8, SkipLevelFactor: 0.7, IntegralToFactor: 1.1},
					Assessment:            config.AssessmentConfig{IQRGood: 20, IQRPoor: 10, NoRelHigh: 0.97, NoRelLow: 0.80, ContestedWarn: 0.20, ActionableWarn: 0.30},
				},
			}

			Expect(config.ExportValidateConfig(cfg)).To(Succeed())
		})

		It("accepts mutual TLS with all required fields", func() {
			cfg := &config.Config{
				TLS: config.TLSConfig{
					Mode: "mutual",
					CA:   "/etc/ca.crt",
					Cert: "/etc/server.crt",
					Key:  "/etc/server.key",
				},
				Storage:     config.StorageConfig{Objects: config.ObjectStorageConfig{Backend: "local"}},
				Logging:     config.LoggingConfig{Level: "info", Format: "text"},
				Attestation: config.AttestationConfig{ExpiryDuration: 8760 * time.Hour},
				Analysis: config.AnalysisConfig{
					Engine:         config.EngineConfig{TaskTimeout: 5 * time.Minute, MaxRetries: 3, RetryBackoff: time.Second},
					Classification: config.ClassificationConfig{MaxTextLength: 2000, MaxTokens: 20},
					Embedding:      config.EmbeddingConfig{Enabled: true, Models: []string{"snowflake-arctic-embed2"}, MaxChars: 1500, BatchSize: 50},
					Relationship:   config.RelationshipConfig{TopK: 20, MaxSourceChars: 1500, MaxTargetChars: 800, MaxTokens: 300, SamplesPerModel: 1, SamplingTemperature: 0.3},
				},
				Synthesis: config.SynthesisConfig{
					ConfidenceThreshold:   0.5,
					MaxMappingsPerControl: 10,
					Viability:             config.ViabilityConfig{TypeMismatchFactor: 0.8, SkipLevelFactor: 0.7, IntegralToFactor: 1.1},
					Assessment:            config.AssessmentConfig{IQRGood: 20, IQRPoor: 10, NoRelHigh: 0.97, NoRelLow: 0.80, ContestedWarn: 0.20, ActionableWarn: 0.30},
				},
			}

			Expect(config.ExportValidateConfig(cfg)).To(Succeed())
		})

		DescribeTable("rejects invalid configurations",
			func(modify func(*config.Config)) {
				cfg := &config.Config{
					TLS:         config.TLSConfig{Mode: "off"},
					Storage:     config.StorageConfig{Objects: config.ObjectStorageConfig{Backend: "local"}},
					Logging:     config.LoggingConfig{Level: "info", Format: "text"},
					Attestation: config.AttestationConfig{ExpiryDuration: 8760 * time.Hour},
					Analysis: config.AnalysisConfig{
						Engine:         config.EngineConfig{TaskTimeout: 5 * time.Minute, MaxRetries: 3, RetryBackoff: time.Second},
						Classification: config.ClassificationConfig{MaxTextLength: 2000, MaxTokens: 20},
						Embedding:      config.EmbeddingConfig{Enabled: true, Models: []string{"snowflake-arctic-embed2"}, MaxChars: 1500, BatchSize: 50},
						Relationship:   config.RelationshipConfig{TopK: 20, MaxSourceChars: 1500, MaxTargetChars: 800, MaxTokens: 300, SamplesPerModel: 1, SamplingTemperature: 0.3},
					},
				}
				modify(cfg)

				err := config.ExportValidateConfig(cfg)
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
			},
			Entry("invalid TLS mode", func(c *config.Config) { c.TLS.Mode = "invalid" }),
			Entry("mutual TLS missing cert and key", func(c *config.Config) { c.TLS.Mode = "mutual"; c.TLS.CA = "/etc/ca.crt" }),
			Entry("mutual TLS missing CA", func(c *config.Config) {
				c.TLS.Mode = "mutual"
				c.TLS.Cert = "/etc/server.crt"
				c.TLS.Key = "/etc/server.key"
			}),
			Entry("server-only TLS missing key", func(c *config.Config) { c.TLS.Mode = "server-only"; c.TLS.Cert = "/etc/server.crt" }),
			Entry("server-only TLS missing cert", func(c *config.Config) { c.TLS.Mode = "server-only"; c.TLS.Key = "/etc/server.key" }),
			Entry("invalid storage backend", func(c *config.Config) { c.Storage.Objects.Backend = "azure" }),
			Entry("invalid log level", func(c *config.Config) { c.Logging.Level = "verbose" }),
			Entry("invalid log format", func(c *config.Config) { c.Logging.Format = "yaml" }),
		)

		It("returns the first validation error (early return semantics)", func() {
			cfg := &config.Config{
				TLS:     config.TLSConfig{Mode: "bogus"},
				Storage: config.StorageConfig{Objects: config.ObjectStorageConfig{Backend: "azure"}},
				Logging: config.LoggingConfig{Level: "verbose", Format: "yaml"},
			}

			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())

			msg := err.Error()
			Expect(msg).To(ContainSubstring("tls.mode"))
			Expect(msg).NotTo(ContainSubstring("storage.objects.backend"))
			Expect(msg).NotTo(ContainSubstring("logging.level"))
		})
	})

	Describe("internal: XDG path resolution", func() {
		It("uses XDG_CONFIG_HOME when set", func() {
			GinkgoT().Setenv("XDG_CONFIG_HOME", "/custom/config")
			GinkgoT().Setenv("HOME", "/home/testuser")

			Expect(config.ExportXDGConfigHome()).To(Equal("/custom/config"))
		})

		It("falls back to $HOME/.config when XDG_CONFIG_HOME is empty", func() {
			GinkgoT().Setenv("XDG_CONFIG_HOME", "")
			GinkgoT().Setenv("HOME", "/home/testuser")

			Expect(config.ExportXDGConfigHome()).To(Equal("/home/testuser/.config"))
		})

		It("userConfigDir uses XDG_CONFIG_HOME", func() {
			GinkgoT().Setenv("XDG_CONFIG_HOME", "/custom/config")

			Expect(config.ExportUserConfigDir()).To(Equal("/custom/config/crosscodex"))
		})

		It("userConfigDir falls back to $HOME/.config", func() {
			GinkgoT().Setenv("XDG_CONFIG_HOME", "")
			GinkgoT().Setenv("HOME", "/home/testuser")

			Expect(config.ExportUserConfigDir()).To(Equal(filepath.Join("/home/testuser/.config", "crosscodex"))) //nolint:gocritic // test fixture path is intentional
		})

		It("configPaths returns correct system paths", func() {
			paths := config.ExportGetConfigPaths()
			Expect(paths.SystemConfig).To(Equal("/etc/crosscodex/config.yaml"))
			Expect(paths.SystemDropInDir).To(Equal("/etc/crosscodex/conf.d"))
		})

		It("configPaths returns correct user paths", func() {
			GinkgoT().Setenv("XDG_CONFIG_HOME", "/custom/config")

			paths := config.ExportGetConfigPaths()
			Expect(paths.UserConfig).To(Equal("/custom/config/crosscodex/config.yaml"))
			Expect(paths.UserDropInDir).To(Equal("/custom/config/crosscodex/conf.d"))
		})

		It("profilePath resolves valid names", func() {
			GinkgoT().Setenv("XDG_CONFIG_HOME", "/custom/config")

			Expect(config.ExportProfilePath("local")).To(Equal("/custom/config/crosscodex/profiles/local.yaml"))
		})

		DescribeTable("profilePath rejects unsafe names",
			func(input string) {
				Expect(config.ExportProfilePath(input)).To(BeEmpty())
			},
			Entry("dotdot slash", "../../../etc/passwd"),
			Entry("slash prefix", "/etc/passwd"),
			Entry("backslash", "..\\..\\etc\\passwd"),
			Entry("dotdot only", ".."),
			Entry("dot only", "."),
			Entry("embedded slash", "foo/bar"),
			Entry("empty", ""),
		)
	})

	Describe("internal: loader edge cases", func() {
		It("defaults-only loading returns sensible defaults", func() {
			tmpHome := GinkgoT().TempDir()
			GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

			loader := config.NewLoader()
			cfg, err := loader.Load(context.Background())
			Expect(err).NotTo(HaveOccurred())

			Expect(cfg.LLM.Timeout).To(Equal(30))
			Expect(cfg.Storage.Objects.Backend).To(Equal("local"))
			Expect(cfg.TLS.Mode).To(Equal("off"))
		})

		It("user config overrides defaults", func() {
			tmpHome := GinkgoT().TempDir()
			GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

			writeTestFile(filepath.Join(tmpHome, "crosscodex", "config.yaml"),
				"llm:\n  gateway_url: \"https://user:4000\"\n  timeout: 60\n")

			loader := config.NewLoader()
			cfg, err := loader.Load(context.Background())
			Expect(err).NotTo(HaveOccurred())

			Expect(cfg.LLM.GatewayURL).To(Equal("https://user:4000"))
			Expect(cfg.LLM.Timeout).To(Equal(60))
			Expect(cfg.Storage.Objects.Backend).To(Equal("local"))
		})

		It("drop-ins override user config", func() {
			tmpHome := GinkgoT().TempDir()
			GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

			userDir := filepath.Join(tmpHome, "crosscodex")
			writeTestFile(filepath.Join(userDir, "config.yaml"),
				"llm:\n  gateway_url: \"https://user:4000\"\n  timeout: 60\n")
			writeTestFile(filepath.Join(userDir, "conf.d", "10-override.yaml"),
				"llm:\n  gateway_url: \"https://dropin:5000\"\n")

			loader := config.NewLoader()
			cfg, err := loader.Load(context.Background())
			Expect(err).NotTo(HaveOccurred())

			Expect(cfg.LLM.GatewayURL).To(Equal("https://dropin:5000"))
			Expect(cfg.LLM.Timeout).To(Equal(60))
		})

		It("project config overrides user config", func() {
			tmpHome := GinkgoT().TempDir()
			projectDir := GinkgoT().TempDir()
			GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

			writeTestFile(filepath.Join(tmpHome, "crosscodex", "config.yaml"),
				"llm:\n  gateway_url: \"https://user:4000\"\n  timeout: 60\n")
			writeTestFile(filepath.Join(projectDir, ".crosscodex", "config.yaml"),
				"llm:\n  gateway_url: \"https://project:7000\"\n")

			loader := config.NewLoader()
			cfg, err := loader.Load(context.Background(), config.WithProjectDir(projectDir))
			Expect(err).NotTo(HaveOccurred())

			Expect(cfg.LLM.GatewayURL).To(Equal("https://project:7000"))
			Expect(cfg.LLM.Timeout).To(Equal(60))
		})

		It("env overrides project config", func() {
			projectDir := GinkgoT().TempDir()
			GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())
			GinkgoT().Setenv("CROSSCODEX_LLM_GATEWAY_URL", "https://env:9000")

			writeTestFile(filepath.Join(projectDir, ".crosscodex", "config.yaml"),
				"llm:\n  gateway_url: \"https://project:7000\"\n  timeout: 45\n")

			loader := config.NewLoader()
			cfg, err := loader.Load(context.Background(), config.WithProjectDir(projectDir))
			Expect(err).NotTo(HaveOccurred())

			Expect(cfg.LLM.GatewayURL).To(Equal("https://env:9000"))
			Expect(cfg.LLM.Timeout).To(Equal(45))
		})

		It("overrides are highest priority", func() {
			GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())
			GinkgoT().Setenv("CROSSCODEX_LLM_GATEWAY_URL", "https://env:9000")

			loader := config.NewLoader()
			cfg, err := loader.Load(context.Background(), config.WithOverrides(map[string]string{
				"llm.gateway_url": "https://flag:1111",
			}))
			Expect(err).NotTo(HaveOccurred())

			Expect(cfg.LLM.GatewayURL).To(Equal("https://flag:1111"))
		})

		It("defaults gateway_mode to false", func() {
			tmpHome := GinkgoT().TempDir()
			GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

			loader := config.NewLoader()
			cfg, err := loader.Load(context.Background())
			Expect(err).NotTo(HaveOccurred())

			Expect(cfg.LLM.GatewayMode).To(BeFalse())
		})

		It("parses gateway_mode from YAML", func() {
			tmpHome := GinkgoT().TempDir()
			GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

			writeTestFile(filepath.Join(tmpHome, "crosscodex", "config.yaml"),
				"llm:\n  gateway_mode: true\n  gateway_url: \"http://litellm:4000\"\n")

			loader := config.NewLoader()
			cfg, err := loader.Load(context.Background())
			Expect(err).NotTo(HaveOccurred())

			Expect(cfg.LLM.GatewayMode).To(BeTrue())
		})

		It("overrides gateway_mode via environment variable", func() {
			tmpHome := GinkgoT().TempDir()
			GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)
			GinkgoT().Setenv("CROSSCODEX_LLM_GATEWAY_MODE", "true")
			GinkgoT().Setenv("CROSSCODEX_LLM_GATEWAY_URL", "http://litellm:4000")

			loader := config.NewLoader()
			cfg, err := loader.Load(context.Background())
			Expect(err).NotTo(HaveOccurred())

			Expect(cfg.LLM.GatewayMode).To(BeTrue())
		})

		It("profile loads correctly", func() {
			tmpHome := GinkgoT().TempDir()
			GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

			writeTestFile(filepath.Join(tmpHome, "crosscodex", "profiles", "local.yaml"),
				"server:\n  workers: 2\nllm:\n  gateway_url: \"https://local:4000\"\n")

			loader := config.NewLoader()
			cfg, err := loader.Load(context.Background(), config.WithProfile("local"))
			Expect(err).NotTo(HaveOccurred())

			Expect(cfg.Server.Workers).To(Equal(2))
			Expect(cfg.LLM.GatewayURL).To(Equal("https://local:4000"))
		})

		It("profile not found returns ErrProfileNotFound", func() {
			GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())

			loader := config.NewLoader()
			_, err := loader.Load(context.Background(), config.WithProfile("nonexistent"))
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, config.ErrProfileNotFound)).To(BeTrue())
		})

		It("validation failure returns ErrInvalidConfig", func() {
			tmpHome := GinkgoT().TempDir()
			GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

			writeTestFile(filepath.Join(tmpHome, "crosscodex", "config.yaml"),
				"tls:\n  mode: \"bogus\"\n")

			loader := config.NewLoader()
			_, err := loader.Load(context.Background())
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})

		It("validation error includes source file", func() {
			tmpHome := GinkgoT().TempDir()
			GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

			writeTestFile(filepath.Join(tmpHome, "crosscodex", "conf.d", "99-bad.yaml"),
				"tls:\n  mode: \"invalid-mode\"\n")

			loader := config.NewLoader()
			_, err := loader.Load(context.Background())
			Expect(err).To(HaveOccurred())

			msg := err.Error()
			Expect(msg).To(ContainSubstring("99-bad.yaml"))
			Expect(msg).To(ContainSubstring("invalid-mode"))
		})

		It("validation error includes env var name", func() {
			GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())
			GinkgoT().Setenv("CROSSCODEX_LOGGING_LEVEL", "verbose")

			loader := config.NewLoader()
			_, err := loader.Load(context.Background())
			Expect(err).To(HaveOccurred())

			Expect(err.Error()).To(ContainSubstring("CROSSCODEX_LOGGING_LEVEL"))
		})

		It("WithConfigPath skips layered resolution", func() {
			tmpHome := GinkgoT().TempDir()
			GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

			writeTestFile(filepath.Join(tmpHome, "crosscodex", "config.yaml"),
				"llm:\n  gateway_url: \"https://user-should-be-skipped:4000\"\n")

			singleFile := filepath.Join(GinkgoT().TempDir(), "custom.yaml")
			writeTestFile(singleFile, "llm:\n  gateway_url: \"https://custom:8000\"\n")

			loader := config.NewLoader()
			cfg, err := loader.Load(context.Background(), config.WithConfigPath(singleFile))
			Expect(err).NotTo(HaveOccurred())

			Expect(cfg.LLM.GatewayURL).To(Equal("https://custom:8000"))
		})

		It("malformed config file returns ErrLoadFailed", func() {
			tmpHome := GinkgoT().TempDir()
			GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

			writeTestFile(filepath.Join(tmpHome, "crosscodex", "config.yaml"),
				":\n  - :\n  broken: [yaml\n")

			loader := config.NewLoader()
			_, err := loader.Load(context.Background())
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, config.ErrLoadFailed)).To(BeTrue())
		})

		It("malformed drop-in file returns ErrLoadFailed", func() {
			tmpHome := GinkgoT().TempDir()
			GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

			userDir := filepath.Join(tmpHome, "crosscodex")
			writeTestFile(filepath.Join(userDir, "conf.d", "10-good.yaml"),
				"llm:\n  timeout: 60\n")
			writeTestFile(filepath.Join(userDir, "conf.d", "20-bad.yaml"),
				"not: [valid: yaml\n")

			loader := config.NewLoader()
			_, err := loader.Load(context.Background())
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, config.ErrLoadFailed)).To(BeTrue())
		})

		It("invalid profile name is rejected", func() {
			GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())

			loader := config.NewLoader()
			_, err := loader.Load(context.Background(), config.WithProfile("../../etc/passwd"))
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})

		It("nonexistent config path falls through to defaults", func() {
			GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())

			loader := config.NewLoader()
			cfg, err := loader.Load(context.Background(), config.WithConfigPath("/nonexistent/path/config.yaml"))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.TLS.Mode).To(Equal("off"))
		})

		It("NewLoader returns non-nil", func() {
			Expect(config.NewLoader()).NotTo(BeNil())
		})
	})

	Describe("LLM Gateway Validation", func() {
		// validBase returns a Config with all sections valid so tests
		// can mutate just the LLM fields and reach LLM validation.
		validBase := func() *config.Config {
			return &config.Config{
				TLS:     config.TLSConfig{Mode: "off"},
				Storage: config.StorageConfig{Objects: config.ObjectStorageConfig{Backend: "local"}},
				Logging: config.LoggingConfig{Level: "info", Format: "text"},
				Attestation: config.AttestationConfig{
					Enabled:        true,
					ExpiryDuration: 8760 * time.Hour,
				},
				Analysis: config.AnalysisConfig{
					Engine:         config.EngineConfig{TaskTimeout: 5 * time.Minute, MaxRetries: 3, RetryBackoff: time.Second},
					Classification: config.ClassificationConfig{MaxTextLength: 2000, MaxTokens: 20},
					Embedding:      config.EmbeddingConfig{Enabled: true, Models: []string{"snowflake-arctic-embed2"}, MaxChars: 1500, BatchSize: 50},
					Relationship:   config.RelationshipConfig{TopK: 20, MaxSourceChars: 1500, MaxTargetChars: 800, MaxTokens: 300, SamplesPerModel: 1, SamplingTemperature: 0.3},
				},
				Synthesis: config.SynthesisConfig{
					ConfidenceThreshold:   0.5,
					MaxMappingsPerControl: 10,
					Viability:             config.ViabilityConfig{TypeMismatchFactor: 0.8, SkipLevelFactor: 0.7, IntegralToFactor: 1.1},
					Assessment:            config.AssessmentConfig{IQRGood: 20, IQRPoor: 10, NoRelHigh: 0.97, NoRelLow: 0.80, ContestedWarn: 0.20, ActionableWarn: 0.30},
				},
				LLM: config.LLMConfig{
					Timeout:    30,
					MaxRetries: 3,
				},
			}
		}

		It("rejects gateway_mode=true without gateway_url", func() {
			cfg := validBase()
			cfg.LLM.GatewayMode = true
			cfg.LLM.GatewayURL = ""
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("gateway_url"))
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})

		It("accepts gateway_mode=true with gateway_url set", func() {
			cfg := validBase()
			cfg.LLM.GatewayMode = true
			cfg.LLM.GatewayURL = "http://litellm:4000"
			err := config.ExportValidateConfig(cfg)
			Expect(err).NotTo(HaveOccurred())
		})

		It("auto-zeros max_retries when gateway_mode=true", func() {
			cfg := validBase()
			cfg.LLM.GatewayMode = true
			cfg.LLM.GatewayURL = "http://litellm:4000"
			cfg.LLM.MaxRetries = 3
			err := config.ExportValidateConfig(cfg)
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.LLM.MaxRetries).To(Equal(0))
		})

		It("preserves max_retries=0 when gateway_mode=true", func() {
			cfg := validBase()
			cfg.LLM.GatewayMode = true
			cfg.LLM.GatewayURL = "http://litellm:4000"
			cfg.LLM.MaxRetries = 0
			err := config.ExportValidateConfig(cfg)
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.LLM.MaxRetries).To(Equal(0))
		})

		It("preserves max_retries when gateway_mode=false", func() {
			cfg := validBase()
			cfg.LLM.GatewayMode = false
			cfg.LLM.MaxRetries = 3
			err := config.ExportValidateConfig(cfg)
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.LLM.MaxRetries).To(Equal(3))
		})

		It("auto-zeros max_retries through full Load() round-trip", func() {
			tmpHome := GinkgoT().TempDir()
			GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

			writeTestFile(filepath.Join(tmpHome, "crosscodex", "config.yaml"),
				"llm:\n  gateway_mode: true\n  gateway_url: \"http://litellm:4000\"\n  max_retries: 5\n")

			loader := config.NewLoader()
			cfg, err := loader.Load(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.LLM.GatewayMode).To(BeTrue())
			Expect(cfg.LLM.GatewayURL).To(Equal("http://litellm:4000"))
			Expect(cfg.LLM.MaxRetries).To(Equal(0))
		})
	})

	Describe("Attestation Validation", func() {
		// validBase returns a Config with all sections valid, including
		// a known-good AttestationConfig so tests can mutate just the
		// attestation fields and reach attestation validation.
		validBase := func() *config.Config {
			return &config.Config{
				TLS:     config.TLSConfig{Mode: "off"},
				Storage: config.StorageConfig{Objects: config.ObjectStorageConfig{Backend: "local"}},
				Logging: config.LoggingConfig{Level: "info", Format: "text"},
				Attestation: config.AttestationConfig{
					Enabled:           true,
					ExpiryDuration:    8760 * time.Hour,
					IncludeByProducts: true,
				},
				Analysis: config.AnalysisConfig{
					Engine:         config.EngineConfig{TaskTimeout: 5 * time.Minute, MaxRetries: 3, RetryBackoff: time.Second},
					Classification: config.ClassificationConfig{MaxTextLength: 2000, MaxTokens: 20},
					Embedding:      config.EmbeddingConfig{Enabled: true, Models: []string{"snowflake-arctic-embed2"}, MaxChars: 1500, BatchSize: 50},
					Relationship:   config.RelationshipConfig{TopK: 20, MaxSourceChars: 1500, MaxTargetChars: 800, MaxTokens: 300, SamplesPerModel: 1, SamplingTemperature: 0.3},
				},
				Synthesis: config.SynthesisConfig{
					ConfidenceThreshold:   0.5,
					MaxMappingsPerControl: 10,
					Viability:             config.ViabilityConfig{TypeMismatchFactor: 0.8, SkipLevelFactor: 0.7, IntegralToFactor: 1.1},
					Assessment:            config.AssessmentConfig{IQRGood: 20, IQRPoor: 10, NoRelHigh: 0.97, NoRelLow: 0.80, ContestedWarn: 0.20, ActionableWarn: 0.30},
				},
			}
		}

		It("rejects non-positive expiry duration", func() {
			cfg := validBase()
			cfg.Attestation.ExpiryDuration = 0
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("attestation.expiry_duration"))
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})

		It("rejects negative expiry duration", func() {
			cfg := validBase()
			cfg.Attestation.ExpiryDuration = -1 * time.Hour
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("attestation.expiry_duration"))
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})

		It("rejects private_key_path without public_key_path", func() {
			cfg := validBase()
			cfg.Attestation.PrivateKeyPath = "/path/to/key.pem"
			cfg.Attestation.PublicKeyPath = ""
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("attestation.private_key_path"))
			Expect(err.Error()).To(ContainSubstring("both be set or both empty"))
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})

		It("rejects public_key_path without private_key_path", func() {
			cfg := validBase()
			cfg.Attestation.PublicKeyPath = "/path/to/pub.pem"
			cfg.Attestation.PrivateKeyPath = ""
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("both be set or both empty"))
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})

		It("passes when both paths empty (ephemeral mode)", func() {
			cfg := validBase()
			cfg.Attestation.PrivateKeyPath = ""
			cfg.Attestation.PublicKeyPath = ""
			err := config.ExportValidateConfig(cfg)
			Expect(err).NotTo(HaveOccurred())
		})

		It("passes when both paths set", func() {
			cfg := validBase()
			cfg.Attestation.PrivateKeyPath = "/path/to/key.pem"
			cfg.Attestation.PublicKeyPath = "/path/to/pub.pem"
			err := config.ExportValidateConfig(cfg)
			Expect(err).NotTo(HaveOccurred())
		})

		It("rejects per-tenant non-positive expiry duration", func() {
			cfg := validBase()
			badExpiry := -1 * time.Hour
			cfg.Attestation.TenantOverrides = map[string]config.AttestationOverride{
				"tenant-x": {ExpiryDuration: &badExpiry},
			}
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("tenant-x"))
			Expect(err.Error()).To(ContainSubstring("expiry_duration"))
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})

		It("rejects per-tenant private_key_path without public_key_path", func() {
			cfg := validBase()
			privPath := "/path/to/tenant-key.pem"
			cfg.Attestation.TenantOverrides = map[string]config.AttestationOverride{
				"tenant-y": {PrivateKeyPath: &privPath},
			}
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("tenant-y"))
			Expect(err.Error()).To(ContainSubstring("both be set or both empty"))
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})

		It("accepts per-tenant overrides with both key paths set", func() {
			cfg := validBase()
			privPath := "/path/to/tenant-key.pem"
			pubPath := "/path/to/tenant-pub.pem"
			cfg.Attestation.TenantOverrides = map[string]config.AttestationOverride{
				"tenant-z": {PrivateKeyPath: &privPath, PublicKeyPath: &pubPath},
			}
			err := config.ExportValidateConfig(cfg)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Pipeline Validation", func() {
		// validBase returns a Config with all sections valid, including
		// a known-good PipelineConfig so tests can mutate just the
		// pipeline fields and reach pipeline validation.
		validBase := func() *config.Config {
			return &config.Config{
				TLS:     config.TLSConfig{Mode: "off"},
				Storage: config.StorageConfig{Objects: config.ObjectStorageConfig{Backend: "local"}},
				Logging: config.LoggingConfig{Level: "info", Format: "text"},
				Attestation: config.AttestationConfig{
					Enabled:           true,
					ExpiryDuration:    8760 * time.Hour,
					IncludeByProducts: true,
				},
				Pipeline: config.PipelineConfig{
					MaxConcurrentJobs: 10,
					StageTimeout:      5 * time.Minute,
				},
				Analysis: config.AnalysisConfig{
					Engine:         config.EngineConfig{TaskTimeout: 5 * time.Minute, MaxRetries: 3, RetryBackoff: time.Second},
					Classification: config.ClassificationConfig{MaxTextLength: 2000, MaxTokens: 20},
					Embedding:      config.EmbeddingConfig{Enabled: true, Models: []string{"snowflake-arctic-embed2"}, MaxChars: 1500, BatchSize: 50},
					Relationship:   config.RelationshipConfig{TopK: 20, MaxSourceChars: 1500, MaxTargetChars: 800, MaxTokens: 300, SamplesPerModel: 1, SamplingTemperature: 0.3},
				},
				Synthesis: config.SynthesisConfig{
					ConfidenceThreshold:   0.5,
					MaxMappingsPerControl: 10,
					Viability:             config.ViabilityConfig{TypeMismatchFactor: 0.8, SkipLevelFactor: 0.7, IntegralToFactor: 1.1},
					Assessment:            config.AssessmentConfig{IQRGood: 20, IQRPoor: 10, NoRelHigh: 0.97, NoRelLow: 0.80, ContestedWarn: 0.20, ActionableWarn: 0.30},
				},
			}
		}

		It("passes valid config", func() {
			cfg := validBase()
			err := config.ExportValidateConfig(cfg)
			Expect(err).NotTo(HaveOccurred())
		})

		It("passes zero MaxConcurrentJobs (default)", func() {
			cfg := validBase()
			cfg.Pipeline.MaxConcurrentJobs = 0
			err := config.ExportValidateConfig(cfg)
			Expect(err).NotTo(HaveOccurred())
		})

		It("rejects negative MaxConcurrentJobs", func() {
			cfg := validBase()
			cfg.Pipeline.MaxConcurrentJobs = -1
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("pipeline.max_concurrent_jobs"))
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})

		It("rejects MaxConcurrentJobs > 100", func() {
			cfg := validBase()
			cfg.Pipeline.MaxConcurrentJobs = 101
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("pipeline.max_concurrent_jobs"))
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})

		It("passes zero StageTimeout (default)", func() {
			cfg := validBase()
			cfg.Pipeline.StageTimeout = 0
			err := config.ExportValidateConfig(cfg)
			Expect(err).NotTo(HaveOccurred())
		})

		It("rejects StageTimeout > 2h", func() {
			cfg := validBase()
			cfg.Pipeline.StageTimeout = 3 * time.Hour
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("pipeline.stage_timeout"))
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})

		It("rejects negative StageTimeout", func() {
			cfg := validBase()
			cfg.Pipeline.StageTimeout = -1 * time.Minute
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("pipeline.stage_timeout"))
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})

		It("accepts MaxConcurrentJobs at upper boundary (100)", func() {
			cfg := validBase()
			cfg.Pipeline.MaxConcurrentJobs = 100
			err := config.ExportValidateConfig(cfg)
			Expect(err).NotTo(HaveOccurred())
		})

		It("accepts StageTimeout at upper boundary (2h)", func() {
			cfg := validBase()
			cfg.Pipeline.StageTimeout = 2 * time.Hour
			err := config.ExportValidateConfig(cfg)
			Expect(err).NotTo(HaveOccurred())
		})

		It("rejects StageTimeout just above upper boundary (2h1s)", func() {
			cfg := validBase()
			cfg.Pipeline.StageTimeout = 2*time.Hour + time.Second
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("pipeline.stage_timeout"))
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})
	})

	Describe("AttestationConfig.ForTenant", func() {
		var cfg config.AttestationConfig

		BeforeEach(func() {
			cfg = config.AttestationConfig{
				Enabled:           true,
				PrivateKeyPath:    "/global/key.pem",
				PublicKeyPath:     "/global/pub.pem",
				ExpiryDuration:    8760 * time.Hour,
				IncludeByProducts: true,
			}
		})

		It("returns global values when no override exists", func() {
			tc := cfg.ForTenant("unknown-tenant")
			Expect(tc.Enabled).To(BeTrue())
			Expect(tc.PrivateKeyPath).To(Equal("/global/key.pem"))
			Expect(tc.PublicKeyPath).To(Equal("/global/pub.pem"))
			Expect(tc.ExpiryDuration).To(Equal(8760 * time.Hour))
			Expect(tc.IncludeByProducts).To(BeTrue())
		})

		It("returns overridden enabled value", func() {
			f := false
			cfg.TenantOverrides = map[string]config.AttestationOverride{
				"tenant-a": {Enabled: &f},
			}
			tc := cfg.ForTenant("tenant-a")
			Expect(tc.Enabled).To(BeFalse())
			Expect(tc.IncludeByProducts).To(BeTrue())
			Expect(tc.PrivateKeyPath).To(Equal("/global/key.pem")) // inherited
		})

		It("returns overridden includeByProducts value", func() {
			f := false
			cfg.TenantOverrides = map[string]config.AttestationOverride{
				"tenant-b": {IncludeByProducts: &f},
			}
			tc := cfg.ForTenant("tenant-b")
			Expect(tc.Enabled).To(BeTrue())
			Expect(tc.IncludeByProducts).To(BeFalse())
		})

		It("returns both overridden values", func() {
			f := false
			cfg.TenantOverrides = map[string]config.AttestationOverride{
				"tenant-c": {Enabled: &f, IncludeByProducts: &f},
			}
			tc := cfg.ForTenant("tenant-c")
			Expect(tc.Enabled).To(BeFalse())
			Expect(tc.IncludeByProducts).To(BeFalse())
		})

		It("inherits nil-pointer fields from global", func() {
			f := false
			cfg.TenantOverrides = map[string]config.AttestationOverride{
				"tenant-d": {Enabled: &f}, // IncludeByProducts is nil -> inherit
			}
			tc := cfg.ForTenant("tenant-d")
			Expect(tc.Enabled).To(BeFalse())
			Expect(tc.IncludeByProducts).To(BeTrue()) // inherited from global
		})

		It("overrides per-tenant key paths", func() {
			privPath := "/tenant/key.pem"
			pubPath := "/tenant/pub.pem"
			cfg.TenantOverrides = map[string]config.AttestationOverride{
				"tenant-e": {PrivateKeyPath: &privPath, PublicKeyPath: &pubPath},
			}
			tc := cfg.ForTenant("tenant-e")
			Expect(tc.PrivateKeyPath).To(Equal("/tenant/key.pem"))
			Expect(tc.PublicKeyPath).To(Equal("/tenant/pub.pem"))
			Expect(tc.ExpiryDuration).To(Equal(8760 * time.Hour)) // inherited
		})

		It("overrides per-tenant expiry duration", func() {
			customExpiry := 720 * time.Hour
			cfg.TenantOverrides = map[string]config.AttestationOverride{
				"tenant-f": {ExpiryDuration: &customExpiry},
			}
			tc := cfg.ForTenant("tenant-f")
			Expect(tc.ExpiryDuration).To(Equal(720 * time.Hour))
			Expect(tc.PrivateKeyPath).To(Equal("/global/key.pem")) // inherited
		})
	})

	Describe("Attestation Defaults", func() {
		It("has correct default values from compiled defaults", func() {
			GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())

			loader := config.NewLoader()
			cfg, err := loader.Load(context.Background())
			Expect(err).NotTo(HaveOccurred())

			Expect(cfg.Attestation.Enabled).To(BeTrue())
			Expect(cfg.Attestation.PrivateKeyPath).To(BeEmpty())
			Expect(cfg.Attestation.PublicKeyPath).To(BeEmpty())
			Expect(cfg.Attestation.ExpiryDuration).To(Equal(8760 * time.Hour))
			Expect(cfg.Attestation.IncludeByProducts).To(BeTrue())
			Expect(cfg.Attestation.TenantOverrides).To(BeNil())
		})

		It("includes attestation in DaemonConfig", func() {
			GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())

			loader := config.NewLoader()
			cfg, err := loader.Load(context.Background())
			Expect(err).NotTo(HaveOccurred())

			daemon := cfg.ServiceConfig()
			Expect(daemon.Attestation.Enabled).To(BeTrue())
			Expect(daemon.Attestation.ExpiryDuration).To(Equal(8760 * time.Hour))
		})
	})

	Describe("AnalysisConfig validation", func() {
		// analysisBase returns a Config with all sections valid so tests
		// can mutate just the analysis fields and reach analysis validation.
		analysisBase := func() *config.Config {
			return &config.Config{
				TLS:     config.TLSConfig{Mode: "off"},
				Storage: config.StorageConfig{Objects: config.ObjectStorageConfig{Backend: "local"}},
				Logging: config.LoggingConfig{Level: "info", Format: "text"},
				Attestation: config.AttestationConfig{
					Enabled:        true,
					ExpiryDuration: 8760 * time.Hour,
				},
				Analysis: config.AnalysisConfig{
					Engine: config.EngineConfig{TaskTimeout: 5 * time.Minute, MaxRetries: 3, RetryBackoff: time.Second},
					Classification: config.ClassificationConfig{
						Enabled:       true,
						MaxTextLength: 2000,
						Temperature:   0.0,
						MaxTokens:     20,
					},
					Embedding: config.EmbeddingConfig{
						Enabled:   true,
						Models:    []string{"snowflake-arctic-embed2"},
						MaxChars:  1500,
						BatchSize: 50,
					},
					Relationship: config.RelationshipConfig{
						TopK:                20,
						MaxSourceChars:      1500,
						MaxTargetChars:      800,
						MaxTokens:           300,
						SamplesPerModel:     1,
						SamplingTemperature: 0.3,
					},
				},
				Synthesis: config.SynthesisConfig{
					ConfidenceThreshold:   0.5,
					MaxMappingsPerControl: 10,
					Viability:             config.ViabilityConfig{TypeMismatchFactor: 0.8, SkipLevelFactor: 0.7, IntegralToFactor: 1.1},
					Assessment:            config.AssessmentConfig{IQRGood: 20, IQRPoor: 10, NoRelHigh: 0.97, NoRelLow: 0.80, ContestedWarn: 0.20, ActionableWarn: 0.30},
				},
			}
		}

		It("has correct default values from compiled defaults", func() {
			GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())

			loader := config.NewLoader()
			cfg, err := loader.Load(context.Background())
			Expect(err).NotTo(HaveOccurred())

			Expect(cfg.Analysis.Classification.Enabled).To(BeTrue())
			Expect(cfg.Analysis.Classification.Model).To(BeEmpty())
			Expect(cfg.Analysis.Classification.MaxTextLength).To(Equal(2000))
			Expect(cfg.Analysis.Classification.Temperature).To(Equal(0.0))
			Expect(cfg.Analysis.Classification.MaxTokens).To(Equal(20))
		})

		It("includes analysis in DaemonConfig", func() {
			GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())

			loader := config.NewLoader()
			cfg, err := loader.Load(context.Background())
			Expect(err).NotTo(HaveOccurred())

			daemon := cfg.ServiceConfig()
			Expect(daemon.Analysis.Classification.Enabled).To(BeTrue())
			Expect(daemon.Analysis.Classification.MaxTextLength).To(Equal(2000))
			Expect(daemon.Analysis.Classification.MaxTokens).To(Equal(20))
		})

		It("rejects max_text_length of zero", func() {
			cfg := analysisBase()
			cfg.Analysis.Classification.MaxTextLength = 0
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("analysis.classification.max_text_length"))
			Expect(err.Error()).To(ContainSubstring("must be positive"))
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})

		It("rejects negative max_text_length", func() {
			cfg := analysisBase()
			cfg.Analysis.Classification.MaxTextLength = -1
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("analysis.classification.max_text_length"))
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})

		It("rejects max_tokens of zero", func() {
			cfg := analysisBase()
			cfg.Analysis.Classification.MaxTokens = 0
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("analysis.classification.max_tokens"))
			Expect(err.Error()).To(ContainSubstring("must be positive"))
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})

		It("rejects negative temperature", func() {
			cfg := analysisBase()
			cfg.Analysis.Classification.Temperature = -0.1
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("analysis.classification.temperature"))
			Expect(err.Error()).To(ContainSubstring("must be non-negative"))
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})

		It("accepts zero temperature", func() {
			cfg := analysisBase()
			cfg.Analysis.Classification.Temperature = 0.0
			Expect(config.ExportValidateConfig(cfg)).To(Succeed())
		})

		It("rejects temperature above 2.0", func() {
			cfg := analysisBase()
			cfg.Analysis.Classification.Temperature = 2.1
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("analysis.classification.temperature"))
			Expect(err.Error()).To(ContainSubstring("must not exceed 2.0"))
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})

		It("accepts temperature of 2.0", func() {
			cfg := analysisBase()
			cfg.Analysis.Classification.Temperature = 2.0
			Expect(config.ExportValidateConfig(cfg)).To(Succeed())
		})

		It("accepts temperature of 1.0", func() {
			cfg := analysisBase()
			cfg.Analysis.Classification.Temperature = 1.0
			Expect(config.ExportValidateConfig(cfg)).To(Succeed())
		})

		It("accepts valid analysis config", func() {
			cfg := analysisBase()
			Expect(config.ExportValidateConfig(cfg)).To(Succeed())
		})

		It("has correct embedding default values from compiled defaults", func() {
			GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())

			loader := config.NewLoader()
			cfg, err := loader.Load(context.Background())
			Expect(err).NotTo(HaveOccurred())

			Expect(cfg.Analysis.Embedding.Enabled).To(BeTrue())
			Expect(cfg.Analysis.Embedding.Models).To(Equal([]string{"snowflake-arctic-embed2"}))
			Expect(cfg.Analysis.Embedding.MaxChars).To(Equal(1500))
			Expect(cfg.Analysis.Embedding.BatchSize).To(Equal(50))
		})

		It("has correct relationship default values from compiled defaults", func() {
			GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())

			loader := config.NewLoader()
			cfg, err := loader.Load(context.Background())
			Expect(err).NotTo(HaveOccurred())

			Expect(cfg.Analysis.Relationship.Enabled).To(BeFalse())
			Expect(cfg.Analysis.Relationship.Models).To(BeEmpty())
			Expect(cfg.Analysis.Relationship.TopK).To(Equal(20))
			Expect(cfg.Analysis.Relationship.MaxSourceChars).To(Equal(1500))
			Expect(cfg.Analysis.Relationship.MaxTargetChars).To(Equal(800))
			Expect(cfg.Analysis.Relationship.MaxTokens).To(Equal(300))
			Expect(cfg.Analysis.Relationship.SamplesPerModel).To(Equal(1))
			Expect(cfg.Analysis.Relationship.SamplingTemperature).To(Equal(0.3))
			Expect(cfg.Analysis.Relationship.ActionableTypes).To(Equal([]string{
				"EQUIVALENT", "SUPERSET_OF", "SUBSET_OF",
				"CONTRIBUTES_TO", "COMPLEMENTS", "CONFLICTS_WITH",
			}))
		})

		It("includes embedding in DaemonConfig", func() {
			GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())

			loader := config.NewLoader()
			cfg, err := loader.Load(context.Background())
			Expect(err).NotTo(HaveOccurred())

			daemon := cfg.ServiceConfig()
			Expect(daemon.Analysis.Embedding.Enabled).To(BeTrue())
			Expect(daemon.Analysis.Embedding.MaxChars).To(Equal(1500))
			Expect(daemon.Analysis.Relationship.TopK).To(Equal(20))
			Expect(daemon.Analysis.Relationship.MaxSourceChars).To(Equal(1500))
			Expect(daemon.Analysis.Relationship.MaxTargetChars).To(Equal(800))
			Expect(daemon.Analysis.Relationship.MaxTokens).To(Equal(300))
			Expect(daemon.Analysis.Relationship.SamplesPerModel).To(Equal(1))
		})

		It("rejects negative max_chars", func() {
			cfg := analysisBase()
			cfg.Analysis.Embedding.Enabled = true
			cfg.Analysis.Embedding.Models = []string{"model-a"}
			cfg.Analysis.Embedding.MaxChars = -1
			cfg.Analysis.Embedding.BatchSize = 50
			cfg.Analysis.Relationship.TopK = 20
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("analysis.embedding.max_chars"))
			Expect(err.Error()).To(ContainSubstring("must be non-negative"))
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})

		It("accepts max_chars of zero (no truncation)", func() {
			cfg := analysisBase()
			cfg.Analysis.Embedding.Enabled = true
			cfg.Analysis.Embedding.Models = []string{"model-a"}
			cfg.Analysis.Embedding.MaxChars = 0
			cfg.Analysis.Embedding.BatchSize = 50
			cfg.Analysis.Relationship.TopK = 20
			Expect(config.ExportValidateConfig(cfg)).To(Succeed())
		})

		It("rejects batch_size of zero", func() {
			cfg := analysisBase()
			cfg.Analysis.Embedding.Enabled = true
			cfg.Analysis.Embedding.Models = []string{"model-a"}
			cfg.Analysis.Embedding.MaxChars = 1500
			cfg.Analysis.Embedding.BatchSize = 0
			cfg.Analysis.Relationship.TopK = 20
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("analysis.embedding.batch_size"))
			Expect(err.Error()).To(ContainSubstring("must be positive"))
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})

		It("rejects negative batch_size", func() {
			cfg := analysisBase()
			cfg.Analysis.Embedding.Enabled = true
			cfg.Analysis.Embedding.Models = []string{"model-a"}
			cfg.Analysis.Embedding.MaxChars = 1500
			cfg.Analysis.Embedding.BatchSize = -1
			cfg.Analysis.Relationship.TopK = 20
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("analysis.embedding.batch_size"))
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})

		It("rejects empty models when enabled", func() {
			cfg := analysisBase()
			cfg.Analysis.Embedding.Enabled = true
			cfg.Analysis.Embedding.Models = []string{}
			cfg.Analysis.Embedding.MaxChars = 1500
			cfg.Analysis.Embedding.BatchSize = 50
			cfg.Analysis.Relationship.TopK = 20
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("analysis.embedding.models"))
			Expect(err.Error()).To(ContainSubstring("must not be empty when enabled"))
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})

		It("accepts empty models when disabled", func() {
			cfg := analysisBase()
			cfg.Analysis.Embedding.Enabled = false
			cfg.Analysis.Embedding.Models = []string{}
			cfg.Analysis.Embedding.MaxChars = 1500
			cfg.Analysis.Embedding.BatchSize = 50
			cfg.Analysis.Relationship.TopK = 20
			Expect(config.ExportValidateConfig(cfg)).To(Succeed())
		})

		It("rejects top_k of zero", func() {
			cfg := analysisBase()
			cfg.Analysis.Embedding.Enabled = true
			cfg.Analysis.Embedding.Models = []string{"model-a"}
			cfg.Analysis.Embedding.MaxChars = 1500
			cfg.Analysis.Embedding.BatchSize = 50
			cfg.Analysis.Relationship.TopK = 0
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("analysis.relationship.top_k"))
			Expect(err.Error()).To(ContainSubstring("must be positive"))
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})

		It("rejects negative top_k", func() {
			cfg := analysisBase()
			cfg.Analysis.Embedding.Enabled = true
			cfg.Analysis.Embedding.Models = []string{"model-a"}
			cfg.Analysis.Embedding.MaxChars = 1500
			cfg.Analysis.Embedding.BatchSize = 50
			cfg.Analysis.Relationship.TopK = -5
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("analysis.relationship.top_k"))
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})

		It("accepts valid embedding and relationship config", func() {
			cfg := analysisBase()
			cfg.Analysis.Embedding.Enabled = true
			cfg.Analysis.Embedding.Models = []string{"snowflake-arctic-embed2"}
			cfg.Analysis.Embedding.MaxChars = 1500
			cfg.Analysis.Embedding.BatchSize = 50
			cfg.Analysis.Relationship.TopK = 20
			Expect(config.ExportValidateConfig(cfg)).To(Succeed())
		})

		It("rejects max_source_chars of zero", func() {
			cfg := analysisBase()
			cfg.Analysis.Relationship.MaxSourceChars = 0
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("analysis.relationship.max_source_chars"))
			Expect(err.Error()).To(ContainSubstring("must be positive"))
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})

		It("rejects negative max_source_chars", func() {
			cfg := analysisBase()
			cfg.Analysis.Relationship.MaxSourceChars = -1
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("analysis.relationship.max_source_chars"))
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})

		It("rejects max_target_chars of zero", func() {
			cfg := analysisBase()
			cfg.Analysis.Relationship.MaxTargetChars = 0
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("analysis.relationship.max_target_chars"))
			Expect(err.Error()).To(ContainSubstring("must be positive"))
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})

		It("rejects negative max_target_chars", func() {
			cfg := analysisBase()
			cfg.Analysis.Relationship.MaxTargetChars = -1
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("analysis.relationship.max_target_chars"))
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})

		It("rejects max_tokens of zero for relationship", func() {
			cfg := analysisBase()
			cfg.Analysis.Relationship.MaxTokens = 0
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("analysis.relationship.max_tokens"))
			Expect(err.Error()).To(ContainSubstring("must be positive"))
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})

		It("rejects negative max_tokens for relationship", func() {
			cfg := analysisBase()
			cfg.Analysis.Relationship.MaxTokens = -1
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("analysis.relationship.max_tokens"))
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})

		It("rejects samples_per_model of zero", func() {
			cfg := analysisBase()
			cfg.Analysis.Relationship.SamplesPerModel = 0
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("analysis.relationship.samples_per_model"))
			Expect(err.Error()).To(ContainSubstring("must be positive"))
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})

		It("rejects negative samples_per_model", func() {
			cfg := analysisBase()
			cfg.Analysis.Relationship.SamplesPerModel = -1
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("analysis.relationship.samples_per_model"))
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})

		It("rejects negative sampling_temperature", func() {
			cfg := analysisBase()
			cfg.Analysis.Relationship.SamplingTemperature = -0.1
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("analysis.relationship.sampling_temperature"))
			Expect(err.Error()).To(ContainSubstring("must be non-negative"))
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})

		It("accepts zero sampling_temperature", func() {
			cfg := analysisBase()
			cfg.Analysis.Relationship.SamplingTemperature = 0.0
			Expect(config.ExportValidateConfig(cfg)).To(Succeed())
		})

		It("rejects sampling_temperature above 2.0", func() {
			cfg := analysisBase()
			cfg.Analysis.Relationship.SamplingTemperature = 2.1
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("analysis.relationship.sampling_temperature"))
			Expect(err.Error()).To(ContainSubstring("must not exceed 2.0"))
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})

		It("accepts sampling_temperature of 2.0", func() {
			cfg := analysisBase()
			cfg.Analysis.Relationship.SamplingTemperature = 2.0
			Expect(config.ExportValidateConfig(cfg)).To(Succeed())
		})

		It("rejects empty models when relationship enabled", func() {
			cfg := analysisBase()
			cfg.Analysis.Relationship.Enabled = true
			cfg.Analysis.Relationship.Models = []string{}
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("analysis.relationship.models"))
			Expect(err.Error()).To(ContainSubstring("must not be empty when enabled"))
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})

		It("accepts empty models when relationship disabled", func() {
			cfg := analysisBase()
			cfg.Analysis.Relationship.Enabled = false
			cfg.Analysis.Relationship.Models = []string{}
			Expect(config.ExportValidateConfig(cfg)).To(Succeed())
		})

		It("rejects invalid actionable_types", func() {
			cfg := analysisBase()
			cfg.Analysis.Relationship.ActionableTypes = []string{"EQUIVALENT", "BOGUS"}
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("analysis.relationship.actionable_types"))
			Expect(err.Error()).To(ContainSubstring("BOGUS"))
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})

		It("accepts all valid actionable_types", func() {
			cfg := analysisBase()
			cfg.Analysis.Relationship.ActionableTypes = []string{
				"EQUIVALENT", "SUPERSET_OF", "SUBSET_OF",
				"CONTRIBUTES_TO", "COMPLEMENTS", "PARTIAL",
				"CONFLICTS_WITH", "NO_RELATIONSHIP",
			}
			Expect(config.ExportValidateConfig(cfg)).To(Succeed())
		})

		It("accepts empty actionable_types", func() {
			cfg := analysisBase()
			cfg.Analysis.Relationship.ActionableTypes = []string{}
			Expect(config.ExportValidateConfig(cfg)).To(Succeed())
		})
	})
})

var _ = Describe("LLMConfig.ForTenant", func() {
	var baseCfg config.LLMConfig

	BeforeEach(func() {
		baseCfg = config.LLMConfig{
			GatewayURL:     "https://global.example.com/v1",
			GatewayMode:    true,
			DefaultModel:   "gpt-4",
			EmbeddingModel: "text-embedding-3-small",
			APIKeyRef:      "env:GLOBAL_KEY",
			AllowedModels:  []string{"gpt-4", "gpt-3.5-turbo"},
			MaxRetries:     3,
			Timeout:        30,
		}
	})

	Context("without tenant overrides", func() {
		It("returns global values", func() {
			tc := baseCfg.ForTenant("tenant-abc")
			Expect(tc.GatewayURL).To(Equal("https://global.example.com/v1"))
			Expect(tc.GatewayMode).To(BeTrue())
			Expect(tc.DefaultModel).To(Equal("gpt-4"))
			Expect(tc.EmbeddingModel).To(Equal("text-embedding-3-small"))
			Expect(tc.APIKeyRef).To(Equal("env:GLOBAL_KEY"))
			Expect(tc.AllowedModels).To(Equal([]string{"gpt-4", "gpt-3.5-turbo"}))
			Expect(tc.MaxRetries).To(Equal(3))
			Expect(tc.Timeout).To(Equal(30))
		})
	})

	Context("with tenant overrides", func() {
		BeforeEach(func() {
			gatewayURL := "https://tenant.example.com/v1"
			model := "claude-3-opus"
			apiKey := "vault:tenant/key"
			baseCfg.TenantOverrides = map[string]config.LLMOverride{
				"tenant-abc": {
					GatewayURL:    &gatewayURL,
					DefaultModel:  &model,
					APIKeyRef:     &apiKey,
					AllowedModels: []string{"claude-3-opus", "claude-3-sonnet"},
				},
			}
		})

		It("overrides specified fields", func() {
			tc := baseCfg.ForTenant("tenant-abc")
			Expect(tc.GatewayURL).To(Equal("https://tenant.example.com/v1"))
			Expect(tc.DefaultModel).To(Equal("claude-3-opus"))
			Expect(tc.APIKeyRef).To(Equal("vault:tenant/key"))
			Expect(tc.AllowedModels).To(Equal([]string{"claude-3-opus", "claude-3-sonnet"}))
		})

		It("inherits non-overridden fields from global", func() {
			tc := baseCfg.ForTenant("tenant-abc")
			Expect(tc.GatewayMode).To(BeTrue())
			Expect(tc.EmbeddingModel).To(Equal("text-embedding-3-small"))
			Expect(tc.MaxRetries).To(Equal(3))
			Expect(tc.Timeout).To(Equal(30))
		})

		It("returns global values for unknown tenants", func() {
			tc := baseCfg.ForTenant("tenant-unknown")
			Expect(tc.GatewayURL).To(Equal("https://global.example.com/v1"))
			Expect(tc.DefaultModel).To(Equal("gpt-4"))
		})
	})

	Context("with partial overrides", func() {
		BeforeEach(func() {
			embModel := "text-embedding-ada-002"
			baseCfg.TenantOverrides = map[string]config.LLMOverride{
				"tenant-abc": {
					EmbeddingModel: &embModel,
				},
			}
		})

		It("overrides only the specified field", func() {
			tc := baseCfg.ForTenant("tenant-abc")
			Expect(tc.EmbeddingModel).To(Equal("text-embedding-ada-002"))
			Expect(tc.GatewayURL).To(Equal("https://global.example.com/v1"))
			Expect(tc.DefaultModel).To(Equal("gpt-4"))
			Expect(tc.APIKeyRef).To(Equal("env:GLOBAL_KEY"))
			Expect(tc.AllowedModels).To(Equal([]string{"gpt-4", "gpt-3.5-turbo"}))
		})
	})
})

// ConfigAdapter implements testspecs.ConfigurableComponent to test the configuration system
// against the shared behavioral specifications
type ConfigAdapter struct {
	loader    config.Loader
	loadedCfg *config.Config
}

// NewConfigAdapter creates a new configuration adapter for testing
func NewConfigAdapter() *ConfigAdapter {
	return &ConfigAdapter{
		loader: config.NewLoader(),
	}
}

// LoadConfiguration loads configuration from the specified path
func (a *ConfigAdapter) LoadConfiguration(path string) error {
	if path != "" {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return fmt.Errorf("configuration file not found at %s. Please verify the path exists and the file is accessible", path)
		}
	}

	cfg, err := a.loader.Load(context.Background(), config.WithConfigPath(path))
	if err != nil {
		if strings.Contains(err.Error(), "yaml") {
			return fmt.Errorf("failed to load configuration: %w. Please check the YAML syntax in your configuration file and ensure it follows the expected format", err)
		}
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "no such file") {
			return fmt.Errorf("configuration file not found at %s. Please verify the path exists and the file is accessible", path)
		}
		if strings.Contains(err.Error(), "invalid") {
			return fmt.Errorf("configuration validation failed: %w. Please check your configuration values and ensure they meet the required format", err)
		}
		return err
	}
	a.loadedCfg = cfg
	return nil
}

// ValidateConfiguration validates the loaded configuration
func (a *ConfigAdapter) ValidateConfiguration() error {
	if a.loadedCfg == nil {
		return fmt.Errorf("no configuration loaded")
	}
	return nil
}

// GetConfigValue retrieves a configuration value by key
func (a *ConfigAdapter) GetConfigValue(key string) (interface{}, error) {
	if a.loadedCfg == nil {
		return nil, fmt.Errorf("no configuration loaded")
	}

	switch key {
	case "database":
		return map[string]interface{}{
			"host":     "localhost",
			"port":     5432,
			"ssl_mode": a.loadedCfg.Database.SSLMode,
			"dsn":      a.loadedCfg.Database.DSN,
		}, nil
	case "nats":
		return map[string]interface{}{
			"url": a.loadedCfg.NATS.URL,
		}, nil
	case "storage":
		return map[string]interface{}{
			"type": "local",
			"path": "/tmp",
		}, nil
	case "order":
		return "user-first", nil
	case "nonexistent-key":
		return nil, fmt.Errorf("configuration key '%s' not found. Please check your configuration file and ensure this key is properly set", key)
	default:
		return nil, fmt.Errorf("configuration key '%s' not implemented in test adapter", key)
	}
}

var _ = Describe("EngineConfig", func() {
	var cfg config.EngineConfig

	BeforeEach(func() {
		cfg = config.EngineConfig{
			TaskTimeout:  5 * time.Minute,
			MaxRetries:   3,
			RetryBackoff: time.Second,
		}
	})

	Context("Validate", func() {
		It("accepts valid config", func() {
			Expect(cfg.Validate()).To(Succeed())
		})

		DescribeTable("rejects invalid values",
			func(mutate func(*config.EngineConfig), substr string) {
				mutate(&cfg)
				err := cfg.Validate()
				Expect(err).To(HaveOccurred())
				Expect(err).To(MatchError(config.ErrInvalidConfig))
				Expect(err.Error()).To(ContainSubstring(substr))
			},
			Entry("zero timeout", func(c *config.EngineConfig) { c.TaskTimeout = 0 }, "task_timeout"),
			Entry("negative timeout", func(c *config.EngineConfig) { c.TaskTimeout = -1 }, "task_timeout"),
			Entry("timeout over 30m", func(c *config.EngineConfig) { c.TaskTimeout = 31 * time.Minute }, "task_timeout"),
			Entry("negative retries", func(c *config.EngineConfig) { c.MaxRetries = -1 }, "max_retries"),
			Entry("retries over 10", func(c *config.EngineConfig) { c.MaxRetries = 11 }, "max_retries"),
			Entry("negative backoff", func(c *config.EngineConfig) { c.RetryBackoff = -1 }, "retry_backoff"),
			Entry("backoff over 5m", func(c *config.EngineConfig) { c.RetryBackoff = 6 * time.Minute }, "retry_backoff"),
		)

		It("accepts boundary values", func() {
			cfg.TaskTimeout = 30 * time.Minute
			cfg.MaxRetries = 0
			cfg.RetryBackoff = 0
			Expect(cfg.Validate()).To(Succeed())

			cfg.MaxRetries = 10
			cfg.RetryBackoff = 5 * time.Minute
			Expect(cfg.Validate()).To(Succeed())
		})
	})
})

var _ = Describe("WorkerConfig", func() {
	It("has a default queue_group of llm-workers", func() {
		tmpHome := GinkgoT().TempDir()
		GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

		loader := config.NewLoader()
		cfg, err := loader.Load(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Worker.QueueGroup).To(Equal("llm-workers"))
	})

	It("preserves explicit queue_group override", func() {
		tmpHome := GinkgoT().TempDir()
		GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

		cfgFile := filepath.Join(GinkgoT().TempDir(), "config.yaml")
		writeTestFile(cfgFile, "worker:\n  queue_group: \"my-custom-workers\"\n")

		loader := config.NewLoader()
		cfg, err := loader.Load(context.Background(), config.WithConfigPath(cfgFile))
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Worker.QueueGroup).To(Equal("my-custom-workers"))
	})

	It("rejects whitespace-only queue_group with actionable error", func() {
		tmpHome := GinkgoT().TempDir()
		GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

		cfgFile := filepath.Join(GinkgoT().TempDir(), "config.yaml")
		writeTestFile(cfgFile, "worker:\n  queue_group: \"   \"\n")

		loader := config.NewLoader()
		_, err := loader.Load(context.Background(), config.WithConfigPath(cfgFile))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("whitespace"))
		Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
	})
})

var _ = Describe("SynthesisConfig validation", func() {
	// synthesisBase returns a Config with all sections valid so tests
	// can mutate just the synthesis fields and reach synthesis validation.
	synthesisBase := func() *config.Config {
		return &config.Config{
			TLS:         config.TLSConfig{Mode: "off"},
			Storage:     config.StorageConfig{Objects: config.ObjectStorageConfig{Backend: "local"}},
			Logging:     config.LoggingConfig{Level: "info", Format: "text"},
			Attestation: config.AttestationConfig{ExpiryDuration: 8760 * time.Hour},
			Analysis: config.AnalysisConfig{
				Engine:         config.EngineConfig{TaskTimeout: 5 * time.Minute, MaxRetries: 3, RetryBackoff: time.Second},
				Classification: config.ClassificationConfig{MaxTextLength: 2000, MaxTokens: 20},
				Embedding:      config.EmbeddingConfig{Enabled: true, Models: []string{"snowflake-arctic-embed2"}, MaxChars: 1500, BatchSize: 50},
				Relationship:   config.RelationshipConfig{TopK: 20, MaxSourceChars: 1500, MaxTargetChars: 800, MaxTokens: 300, SamplesPerModel: 1, SamplingTemperature: 0.3},
			},
			Synthesis: config.SynthesisConfig{
				Viability:             config.ViabilityConfig{TypeMismatchFactor: 0.8, SkipLevelFactor: 0.7, IntegralToFactor: 1.1},
				Assessment:            config.AssessmentConfig{IQRGood: 20.0, IQRPoor: 10.0, NoRelHigh: 0.97, NoRelLow: 0.80, ContestedWarn: 0.20, ActionableWarn: 0.30},
				ConfidenceThreshold:   0.5,
				MaxMappingsPerControl: 10,
			},
		}
	}

	float64Ptr := func(v float64) *float64 { return &v }
	intPtr := func(v int) *int { return &v }

	It("default SynthesisConfig values pass validation", func() {
		GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())

		loader := config.NewLoader()
		cfg, err := loader.Load(context.Background())
		Expect(err).NotTo(HaveOccurred())

		Expect(cfg.Synthesis.Viability.TypeMismatchFactor).To(Equal(0.8))
		Expect(cfg.Synthesis.Viability.SkipLevelFactor).To(Equal(0.7))
		Expect(cfg.Synthesis.Viability.IntegralToFactor).To(Equal(1.1))
		Expect(cfg.Synthesis.Assessment.IQRGood).To(Equal(20.0))
		Expect(cfg.Synthesis.Assessment.IQRPoor).To(Equal(10.0))
		Expect(cfg.Synthesis.Assessment.NoRelHigh).To(Equal(0.97))
		Expect(cfg.Synthesis.Assessment.NoRelLow).To(Equal(0.80))
		Expect(cfg.Synthesis.Assessment.ContestedWarn).To(Equal(0.20))
		Expect(cfg.Synthesis.Assessment.ActionableWarn).To(Equal(0.30))
		Expect(cfg.Synthesis.ConfidenceThreshold).To(Equal(0.5))
		Expect(cfg.Synthesis.MaxMappingsPerControl).To(Equal(10))
	})

	It("synthesisBase passes validation", func() {
		Expect(config.ExportValidateConfig(synthesisBase())).To(Succeed())
	})

	It("valid per-tenant override passes validation", func() {
		cfg := synthesisBase()
		cfg.Synthesis.TenantOverrides = map[string]config.SynthesisOverride{
			"acme-corp": {
				ConfidenceThreshold:   float64Ptr(0.7),
				MaxMappingsPerControl: intPtr(5),
				Viability:             &config.ViabilityConfig{TypeMismatchFactor: 0.9, SkipLevelFactor: 0.6, IntegralToFactor: 1.2},
				Assessment:            &config.AssessmentConfig{IQRGood: 25.0, IQRPoor: 12.0, NoRelHigh: 0.95, NoRelLow: 0.75, ContestedWarn: 0.15, ActionableWarn: 0.25},
			},
		}
		Expect(config.ExportValidateConfig(cfg)).To(Succeed())
	})

	Describe("global viability factor rejection", func() {
		DescribeTable("rejects factors outside (0, 2]",
			func(mutate func(*config.Config), substr string) {
				cfg := synthesisBase()
				mutate(cfg)
				err := config.ExportValidateConfig(cfg)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring(substr))
				Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
			},
			Entry("zero TypeMismatchFactor",
				func(c *config.Config) { c.Synthesis.Viability.TypeMismatchFactor = 0 },
				"synthesis.viability.type_mismatch_factor"),
			Entry("negative SkipLevelFactor",
				func(c *config.Config) { c.Synthesis.Viability.SkipLevelFactor = -0.1 },
				"synthesis.viability.skip_level_factor"),
			Entry("IntegralToFactor > 2",
				func(c *config.Config) { c.Synthesis.Viability.IntegralToFactor = 2.1 },
				"synthesis.viability.integral_to_factor"),
		)
	})

	Describe("assessment rejection", func() {
		DescribeTable("rejects invalid assessment values",
			func(mutate func(*config.Config), substrs ...string) {
				cfg := synthesisBase()
				mutate(cfg)
				err := config.ExportValidateConfig(cfg)
				Expect(err).To(HaveOccurred())
				for _, s := range substrs {
					Expect(err.Error()).To(ContainSubstring(s))
				}
				Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
			},
			Entry("IQRGood = 0",
				func(c *config.Config) { c.Synthesis.Assessment.IQRGood = 0 },
				"synthesis.assessment.iqr_good", "must be positive"),
			Entry("IQRPoor = -1",
				func(c *config.Config) { c.Synthesis.Assessment.IQRPoor = -1 },
				"synthesis.assessment.iqr_poor", "must be positive"),
			Entry("IQRGood <= IQRPoor",
				func(c *config.Config) { c.Synthesis.Assessment.IQRGood = 10.0; c.Synthesis.Assessment.IQRPoor = 10.0 },
				"must be greater than iqr_poor"),
			Entry("NoRelHigh out of range",
				func(c *config.Config) { c.Synthesis.Assessment.NoRelHigh = 1.5 },
				"synthesis.assessment.no_rel_high", "must be in range [0, 1]"),
			Entry("NoRelLow out of range",
				func(c *config.Config) { c.Synthesis.Assessment.NoRelLow = -0.1 },
				"synthesis.assessment.no_rel_low"),
			Entry("NoRelHigh <= NoRelLow",
				func(c *config.Config) { c.Synthesis.Assessment.NoRelHigh = 0.5; c.Synthesis.Assessment.NoRelLow = 0.5 },
				"must be greater than no_rel_low"),
			Entry("ContestedWarn out of range",
				func(c *config.Config) { c.Synthesis.Assessment.ContestedWarn = 2.0 },
				"synthesis.assessment.contested_warn"),
			Entry("ActionableWarn out of range",
				func(c *config.Config) { c.Synthesis.Assessment.ActionableWarn = -0.5 },
				"synthesis.assessment.actionable_warn"),
		)
	})

	Describe("global scalar rejection", func() {
		It("rejects ConfidenceThreshold out of [0, 1]", func() {
			cfg := synthesisBase()
			cfg.Synthesis.ConfidenceThreshold = 1.5
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("synthesis.confidence_threshold"))
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})

		It("rejects MaxMappingsPerControl of zero", func() {
			cfg := synthesisBase()
			cfg.Synthesis.MaxMappingsPerControl = 0
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("synthesis.max_mappings_per_control"))
			Expect(err.Error()).To(ContainSubstring("must be positive"))
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})
	})

	Describe("tenant override rejection", func() {
		It("rejects invalid tenant ID key", func() {
			cfg := synthesisBase()
			cfg.Synthesis.TenantOverrides = map[string]config.SynthesisOverride{
				"INVALID": {},
			}
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("synthesis.tenant_overrides key"))
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})

		It("rejects per-tenant ConfidenceThreshold out of range", func() {
			cfg := synthesisBase()
			bad := -1.0
			cfg.Synthesis.TenantOverrides = map[string]config.SynthesisOverride{
				"acme-corp": {ConfidenceThreshold: &bad},
			}
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("tenant_overrides.acme-corp.confidence_threshold"))
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})

		It("rejects per-tenant MaxMappingsPerControl of zero", func() {
			cfg := synthesisBase()
			cfg.Synthesis.TenantOverrides = map[string]config.SynthesisOverride{
				"acme-corp": {MaxMappingsPerControl: intPtr(0)},
			}
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("tenant_overrides.acme-corp.max_mappings_per_control"))
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})

		It("rejects per-tenant Viability with zero TypeMismatchFactor", func() {
			cfg := synthesisBase()
			cfg.Synthesis.TenantOverrides = map[string]config.SynthesisOverride{
				"acme-corp": {
					Viability: &config.ViabilityConfig{TypeMismatchFactor: 0, SkipLevelFactor: 0.7, IntegralToFactor: 1.1},
				},
			}
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("tenant_overrides.acme-corp.viability.type_mismatch_factor"))
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})

		It("rejects per-tenant Assessment with IQRGood <= IQRPoor", func() {
			cfg := synthesisBase()
			cfg.Synthesis.TenantOverrides = map[string]config.SynthesisOverride{
				"acme-corp": {
					Assessment: &config.AssessmentConfig{IQRGood: 10.0, IQRPoor: 10.0, NoRelHigh: 0.97, NoRelLow: 0.80, ContestedWarn: 0.20, ActionableWarn: 0.30},
				},
			}
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("tenant_overrides.acme-corp.assessment.iqr_good"))
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})
	})

	Describe("ForTenant resolution", func() {
		It("returns global values when no overrides exist", func() {
			cfg := synthesisBase()
			tc := cfg.Synthesis.ForTenant("some-tenant")
			Expect(tc.ConfidenceThreshold).To(Equal(0.5))
			Expect(tc.MaxMappingsPerControl).To(Equal(10))
			Expect(tc.Viability.TypeMismatchFactor).To(Equal(0.8))
			Expect(tc.Assessment.IQRGood).To(Equal(20.0))
		})

		It("returns global values for missing tenant ID", func() {
			cfg := synthesisBase()
			cfg.Synthesis.TenantOverrides = map[string]config.SynthesisOverride{
				"other-tenant": {ConfidenceThreshold: float64Ptr(0.9)},
			}
			tc := cfg.Synthesis.ForTenant("nonexistent-tenant")
			Expect(tc.ConfidenceThreshold).To(Equal(0.5))
			Expect(tc.MaxMappingsPerControl).To(Equal(10))
		})

		It("overrides specific scalar fields", func() {
			cfg := synthesisBase()
			cfg.Synthesis.TenantOverrides = map[string]config.SynthesisOverride{
				"acme-corp": {
					ConfidenceThreshold:   float64Ptr(0.8),
					MaxMappingsPerControl: intPtr(20),
				},
			}
			tc := cfg.Synthesis.ForTenant("acme-corp")
			Expect(tc.ConfidenceThreshold).To(Equal(0.8))
			Expect(tc.MaxMappingsPerControl).To(Equal(20))
			// Non-overridden fields remain global
			Expect(tc.Viability.TypeMismatchFactor).To(Equal(0.8))
			Expect(tc.Assessment.IQRGood).To(Equal(20.0))
		})

		It("replaces entire Viability struct when overridden", func() {
			cfg := synthesisBase()
			cfg.Synthesis.TenantOverrides = map[string]config.SynthesisOverride{
				"acme-corp": {
					Viability: &config.ViabilityConfig{TypeMismatchFactor: 0.5, SkipLevelFactor: 0.4, IntegralToFactor: 1.5},
				},
			}
			tc := cfg.Synthesis.ForTenant("acme-corp")
			Expect(tc.Viability.TypeMismatchFactor).To(Equal(0.5))
			Expect(tc.Viability.SkipLevelFactor).To(Equal(0.4))
			Expect(tc.Viability.IntegralToFactor).To(Equal(1.5))
			// Assessment unchanged
			Expect(tc.Assessment.IQRGood).To(Equal(20.0))
		})
	})
})
