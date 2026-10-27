package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
)

func TestMainBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Root Command BDD Suite")
}

var _ = Describe("Root Command", func() {
	Describe("newRootCmd", func() {
		It("creates a command with the correct name", func() {
			cmd := newRootCmd()
			Expect(cmd.Use).To(Equal("crosscodex"))
		})

		It("has persistent flags for output control", func() {
			cmd := newRootCmd()
			Expect(cmd.PersistentFlags().Lookup("json")).NotTo(BeNil())
			Expect(cmd.PersistentFlags().Lookup("plain")).NotTo(BeNil())
			Expect(cmd.PersistentFlags().Lookup("no-color")).NotTo(BeNil())
			Expect(cmd.PersistentFlags().Lookup("endpoint")).NotTo(BeNil())
			Expect(cmd.PersistentFlags().Lookup("profile")).NotTo(BeNil())
			Expect(cmd.PersistentFlags().Lookup("verbose")).NotTo(BeNil())
			Expect(cmd.PersistentFlags().Lookup("debug")).NotTo(BeNil())
		})

		It("lowers the default log level when --debug is set", func() {
			prev := slog.Default()
			DeferCleanup(func() { slog.SetDefault(prev) })

			tmpHome := GinkgoT().TempDir()
			GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

			_, _, err := execute(newRootCmd(), "--debug", "config", "show")

			Expect(err).NotTo(HaveOccurred())
			Expect(slog.Default().Enabled(context.Background(), slog.LevelDebug)).To(BeTrue())
		})

		It("keeps the default log level at warn without verbosity flags", func() {
			prev := slog.Default()
			DeferCleanup(func() { slog.SetDefault(prev) })

			tmpHome := GinkgoT().TempDir()
			GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

			_, _, err := execute(newRootCmd(), "config", "show")

			Expect(err).NotTo(HaveOccurred())
			Expect(slog.Default().Enabled(context.Background(), slog.LevelDebug)).To(BeFalse())
			Expect(slog.Default().Enabled(context.Background(), slog.LevelWarn)).To(BeTrue())
			Expect(slog.Default().Enabled(context.Background(), slog.LevelInfo)).To(BeFalse())
		})

		It("has command groups defined", func() {
			cmd := newRootCmd()
			groups := cmd.Groups()
			Expect(groups).To(HaveLen(5))
			names := make([]string, len(groups))
			for i, g := range groups {
				names[i] = g.ID
			}
			Expect(names).To(ConsistOf("project", "analysis", "prompt", "connection", "additional"))
		})

		It("rejects --json and --plain together", func() {
			_, _, err := execute(newRootCmd(), "--json", "--plain", "version")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("json"))
			Expect(err.Error()).To(ContainSubstring("plain"))
		})

		It("shows help with command groups when invoked bare", func() {
			stdout, _, err := execute(newRootCmd(), "--help")
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(ContainSubstring("Project Commands"))
			Expect(stdout).To(ContainSubstring("Analysis Commands"))
			Expect(stdout).To(ContainSubstring("Get started"))
		})
	})

	Describe("Version Command", func() {
		It("outputs version information in human format", func() {
			stdout, _, err := execute(newRootCmd(), "version")
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(ContainSubstring("crosscodex"))
			Expect(stdout).To(ContainSubstring("go"))
		})

		It("outputs version information in JSON format", func() {
			stdout, _, err := execute(newRootCmd(), "version", "--json")
			Expect(err).NotTo(HaveOccurred())
			var result map[string]any
			Expect(json.Unmarshal([]byte(stdout), &result)).To(Succeed())
			Expect(result).To(HaveKey("version"))
			Expect(result).To(HaveKey("go_version"))
			Expect(result).To(HaveKey("os"))
			Expect(result).To(HaveKey("arch"))
		})

		It("is accessible via --version on root", func() {
			stdout, _, err := execute(newRootCmd(), "--version")
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(ContainSubstring("crosscodex"))
		})

		It("is accessible via alias 'ver'", func() {
			stdout, _, err := execute(newRootCmd(), "ver")
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(ContainSubstring("crosscodex"))
			Expect(stdout).To(ContainSubstring("go"))
		})

		Describe("Project Commands", func() {
			var tmpDir string

			BeforeEach(func() {
				var err error
				tmpDir, err = os.MkdirTemp("", "crosscodex-project-test-*")
				Expect(err).NotTo(HaveOccurred())
			})

			AfterEach(func() {
				os.RemoveAll(tmpDir)
			})

			Describe("project init", func() {
				It("creates .crosscodex directory structure", func() {
					_, _, err := execute(newRootCmd(), "project", "init", "--dir", tmpDir)
					Expect(err).NotTo(HaveOccurred())

					Expect(filepath.Join(tmpDir, ".crosscodex", "config.yaml")).To(BeAnExistingFile())
					Expect(filepath.Join(tmpDir, ".crosscodex", "prompts")).To(BeADirectory())
				})

				It("errors when .crosscodex already exists", func() {
					Expect(os.MkdirAll(filepath.Join(tmpDir, ".crosscodex"), 0o755)).To(Succeed())
					_, _, err := execute(newRootCmd(), "project", "init", "--dir", tmpDir)
					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring("already exists"))
				})

				It("overwrites with --force", func() {
					Expect(os.MkdirAll(filepath.Join(tmpDir, ".crosscodex"), 0o755)).To(Succeed())
					_, _, err := execute(newRootCmd(), "project", "init", "--dir", tmpDir, "--force")
					Expect(err).NotTo(HaveOccurred())
					Expect(filepath.Join(tmpDir, ".crosscodex", "config.yaml")).To(BeAnExistingFile())
				})
			})

			Describe("project config", func() {
				It("outputs resolved config as JSON", func() {
					stdout, _, err := execute(newRootCmd(), "project", "config", "--json")
					Expect(err).NotTo(HaveOccurred())
					var result map[string]any
					Expect(json.Unmarshal([]byte(stdout), &result)).To(Succeed())
				})
			})

			Describe("project list", func() {
				// os.Chdir is process-wide — unsafe if specs run with in-process parallelism.
				It("lists current directory when no .crosscodex found", func() {
					tmp := GinkgoT().TempDir()
					origDir, err := os.Getwd()
					Expect(err).NotTo(HaveOccurred())
					Expect(os.Chdir(tmp)).To(Succeed())
					DeferCleanup(func() { Expect(os.Chdir(origDir)).To(Succeed()) })

					stdout, _, err := execute(newRootCmd(), "project", "list")
					Expect(err).NotTo(HaveOccurred())
					Expect(stdout).To(ContainSubstring("No CrossCodex projects"))
				})
			})

			Describe("project status", func() {
				It("reports daemon status", func() {
					stdout, _, err := execute(newRootCmd(), "project", "status")
					Expect(err).NotTo(HaveOccurred())
					Expect(stdout).To(ContainSubstring("Daemon:"))
				})
			})
		})

		Describe("Config Commands", func() {
			var tmpConfigDir string

			BeforeEach(func() {
				var err error
				tmpConfigDir, err = os.MkdirTemp("", "crosscodex-config-test-*")
				Expect(err).NotTo(HaveOccurred())
				os.Setenv("XDG_CONFIG_HOME", tmpConfigDir)
			})

			AfterEach(func() {
				os.Unsetenv("XDG_CONFIG_HOME")
				os.RemoveAll(tmpConfigDir)
			})

			Describe("config show", func() {
				It("outputs full resolved config in human format", func() {
					stdout, _, err := execute(newRootCmd(), "config", "show")
					Expect(err).NotTo(HaveOccurred())
					for _, section := range []string{
						"llm:", "storage:", "tls:", "tenants:", "database:",
						"nats:", "server:", "cli:", "logging:", "auth:",
						"observability:", "catalog:", "attestation:", "prompt:",
						"analysis:", "worker:", "pipeline:", "synthesis:",
					} {
						Expect(stdout).To(ContainSubstring(section), "missing section: "+section)
					}
				})

				It("outputs full resolved config in JSON format", func() {
					stdout, _, err := execute(newRootCmd(), "config", "show", "--json")
					Expect(err).NotTo(HaveOccurred())
					var result map[string]any
					Expect(json.Unmarshal([]byte(stdout), &result)).To(Succeed())
					Expect(result).To(HaveKey("llm"))
					Expect(result).To(HaveKey("database"))
					Expect(result).To(HaveKey("server"))
					Expect(result).To(HaveKey("cli"))
				})

				It("filters output with --section flag", func() {
					stdout, _, err := execute(newRootCmd(), "config", "show", "--section", "database")
					Expect(err).NotTo(HaveOccurred())
					Expect(stdout).To(ContainSubstring("dsn:"))
					Expect(stdout).NotTo(ContainSubstring("nats:"))
				})

				It("filters output with --section flag in JSON format", func() {
					stdout, _, err := execute(newRootCmd(), "config", "show", "--section", "database", "--json")
					Expect(err).NotTo(HaveOccurred())
					var result map[string]any
					Expect(json.Unmarshal([]byte(stdout), &result)).To(Succeed())
					Expect(result).To(HaveKey("dsn"))
					Expect(result).NotTo(HaveKey("llm"))
				})

				It("errors on unknown section", func() {
					_, _, err := execute(newRootCmd(), "config", "show", "--section", "nonexistent")
					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring("unknown section"))
				})
			})

			Describe("config set", func() {
				It("writes a key-value pair to user config", func() {
					userConfigPath := filepath.Join(tmpConfigDir, "crosscodex", "config.yaml")

					_, _, err := execute(newRootCmd(), "config", "set", "cli.output", "plain")
					Expect(err).NotTo(HaveOccurred())

					Expect(userConfigPath).To(BeAnExistingFile())
					data, err := os.ReadFile(userConfigPath)
					Expect(err).NotTo(HaveOccurred())
					Expect(string(data)).To(ContainSubstring("output: plain"))
				})

				DescribeTable("errors when a required argument is missing",
					func(args []string, wantSubstr string) {
						_, _, err := execute(newRootCmd(), args...)
						Expect(err).To(HaveOccurred())
						Expect(err.Error()).To(ContainSubstring(wantSubstr))
					},
					Entry("key", []string{"config", "set"}, "key"),
					Entry("value", []string{"config", "set", "cli.output"}, "value"),
				)
			})

			Describe("config profiles", func() {
				It("lists profile files", func() {
					profileDir := filepath.Join(tmpConfigDir, "crosscodex", "profiles")
					Expect(os.MkdirAll(profileDir, 0o755)).To(Succeed())
					Expect(os.WriteFile(filepath.Join(profileDir, "dev.yaml"), []byte("cli:\n  output: json\n"), 0o644)).To(Succeed())
					Expect(os.WriteFile(filepath.Join(profileDir, "prod.yaml"), []byte("cli:\n  output: plain\n"), 0o644)).To(Succeed())

					stdout, _, err := execute(newRootCmd(), "config", "profiles")
					Expect(err).NotTo(HaveOccurred())
					Expect(stdout).To(ContainSubstring("dev"))
					Expect(stdout).To(ContainSubstring("prod"))
				})

				It("reports when no profiles exist", func() {
					stdout, _, err := execute(newRootCmd(), "config", "profiles")
					Expect(err).NotTo(HaveOccurred())
					Expect(stdout).To(ContainSubstring("No profiles"))
				})
			})

			Describe("setYAMLPath", func() {
				It("creates a new key at the root", func() {
					node := &yaml.Node{Kind: yaml.MappingNode}
					err := setYAMLPath(node, []string{"key"}, "value")
					Expect(err).NotTo(HaveOccurred())
					Expect(node.Content).To(HaveLen(2))
					Expect(node.Content[0].Value).To(Equal("key"))
					Expect(node.Content[1].Value).To(Equal("value"))
				})

				It("updates an existing key", func() {
					node := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
						{Kind: yaml.ScalarNode, Value: "key"},
						{Kind: yaml.ScalarNode, Value: "old"},
					}}
					err := setYAMLPath(node, []string{"key"}, "new")
					Expect(err).NotTo(HaveOccurred())
					Expect(node.Content[1].Value).To(Equal("new"))
				})

				It("creates nested keys", func() {
					node := &yaml.Node{Kind: yaml.MappingNode}
					err := setYAMLPath(node, []string{"a", "b"}, "deep")
					Expect(err).NotTo(HaveOccurred())
					Expect(node.Content).To(HaveLen(2))
					Expect(node.Content[0].Value).To(Equal("a"))
					Expect(node.Content[1].Kind).To(Equal(yaml.MappingNode))
				})

				It("returns error for empty path", func() {
					node := &yaml.Node{Kind: yaml.MappingNode}
					err := setYAMLPath(node, []string{}, "value")
					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring("empty path"))
				})

				It("returns error for non-mapping node", func() {
					node := &yaml.Node{Kind: yaml.ScalarNode}
					err := setYAMLPath(node, []string{"key"}, "value")
					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring("expected mapping"))
				})
			})
		})

		Describe("Run Commands", func() {
			Describe("aliases", func() {
				It("supports 'run submit' as alias for start", func() {
					cmd := newRootCmd()
					runCmd, _, err := cmd.Find([]string{"run", "submit"})
					Expect(err).NotTo(HaveOccurred())
					Expect(runCmd.Name()).To(Equal("start"))
				})

				It("supports 'run stop' as alias for cancel", func() {
					cmd := newRootCmd()
					runCmd, _, err := cmd.Find([]string{"run", "stop"})
					Expect(err).NotTo(HaveOccurred())
					Expect(runCmd.Name()).To(Equal("cancel"))
				})
			})

			Describe("formatJobStatus", func() {
				DescribeTable("maps proto status to human string",
					func(status pb.JobStatus, expected string) {
						Expect(formatJobStatus(status)).To(Equal(expected))
					},
					Entry("pending", pb.JobStatus_JOB_STATUS_PENDING, "PENDING"),
					Entry("running", pb.JobStatus_JOB_STATUS_RUNNING, "RUNNING"),
					Entry("completed", pb.JobStatus_JOB_STATUS_COMPLETED, "COMPLETED"),
					Entry("failed", pb.JobStatus_JOB_STATUS_FAILED, "FAILED"),
					Entry("cancelled", pb.JobStatus_JOB_STATUS_CANCELLED, "CANCELLED"),
					Entry("unspecified falls through", pb.JobStatus_JOB_STATUS_UNSPECIFIED, pb.JobStatus_JOB_STATUS_UNSPECIFIED.String()),
				)
			})
		})
	})

	Describe("Catalog Commands", func() {
		Describe("catalog validate", func() {
			var tmpFile string

			BeforeEach(func() {
				f, err := os.CreateTemp("", "test-catalog-*.json")
				Expect(err).NotTo(HaveOccurred())
				tmpFile = f.Name()
				f.Close()
			})

			AfterEach(func() {
				if tmpFile != "" {
					os.Remove(tmpFile)
				}
			})

			It("rejects invalid JSON", func() {
				Expect(os.WriteFile(tmpFile, []byte("not json"), 0o644)).To(Succeed())
				_, _, err := execute(newRootCmd(), "catalog", "validate", tmpFile)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("JSON"))
			})

			It("rejects JSON without catalog key", func() {
				Expect(os.WriteFile(tmpFile, []byte(`{"foo": "bar"}`), 0o644)).To(Succeed())
				_, _, err := execute(newRootCmd(), "catalog", "validate", tmpFile)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("catalog"))
			})

			It("accepts valid OSCAL catalog structure", func() {
				Expect(os.WriteFile(tmpFile, []byte(`{"catalog": {"uuid": "test", "metadata": {"title": "Test"}}}`), 0o644)).To(Succeed())
				stdout, _, err := execute(newRootCmd(), "catalog", "validate", tmpFile)
				Expect(err).NotTo(HaveOccurred())
				Expect(stdout).To(ContainSubstring("valid"))
			})

		})

		Describe("catalog import", func() {
			It("errors when file does not exist or daemon unavailable", func() {
				_, _, err := execute(newRootCmd(), "catalog", "import", "/nonexistent/path.json")
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(Or(
					ContainSubstring("read file"),
					ContainSubstring("cannot reach"),
					ContainSubstring("failed"),
					ContainSubstring("database not configured"),
				))
			})
		})

		Describe("aliases", func() {
			It("supports 'catalog add' as alias for import", func() {
				cmd := newRootCmd()
				catalogCmd, _, err := cmd.Find([]string{"catalog", "add"})
				Expect(err).NotTo(HaveOccurred())
				Expect(catalogCmd.Name()).To(Equal("import"))
			})

			It("supports 'catalog ls' as alias for list", func() {
				cmd := newRootCmd()
				catalogCmd, _, err := cmd.Find([]string{"catalog", "ls"})
				Expect(err).NotTo(HaveOccurred())
				Expect(catalogCmd.Name()).To(Equal("list"))
			})

			It("supports 'catalog show' as alias for inspect", func() {
				cmd := newRootCmd()
				catalogCmd, _, err := cmd.Find([]string{"catalog", "show"})
				Expect(err).NotTo(HaveOccurred())
				Expect(catalogCmd.Name()).To(Equal("inspect"))
			})

			It("supports 'catalog describe' as alias for inspect", func() {
				cmd := newRootCmd()
				catalogCmd, _, err := cmd.Find([]string{"catalog", "describe"})
				Expect(err).NotTo(HaveOccurred())
				Expect(catalogCmd.Name()).To(Equal("inspect"))
			})
		})
	})

	DescribeTable("errors when the positional argument is missing",
		func(args []string, wantSubstr string) {
			_, _, err := execute(newRootCmd(), args...)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(wantSubstr))
		},
		Entry("run start needs a file", []string{"run", "start"}, "file"),
		Entry("run status needs a job-id", []string{"run", "status"}, "job-id"),
		Entry("run cancel needs a job-id", []string{"run", "cancel"}, "job-id"),
		Entry("catalog validate needs a file", []string{"catalog", "validate"}, "file"),
		Entry("catalog import needs a file", []string{"catalog", "import"}, "file"),
		Entry("catalog inspect needs a catalog ID", []string{"catalog", "inspect"}, "ID"),
	)
})
