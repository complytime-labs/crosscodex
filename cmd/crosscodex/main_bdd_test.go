package main

import (
	"bytes"
	"encoding/json"
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
	var (
		stdout bytes.Buffer
		stderr bytes.Buffer
	)

	BeforeEach(func() {
		stdout.Reset()
		stderr.Reset()
	})

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
			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"--json", "--plain", "version"})
			err := cmd.Execute()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("json"))
			Expect(err.Error()).To(ContainSubstring("plain"))
		})

		It("shows help with command groups when invoked bare", func() {
			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"--help"})
			err := cmd.Execute()
			Expect(err).NotTo(HaveOccurred())
			output := stdout.String()
			Expect(output).To(ContainSubstring("Project Commands"))
			Expect(output).To(ContainSubstring("Analysis Commands"))
			Expect(output).To(ContainSubstring("Get started"))
		})
	})

	Describe("Version Command", func() {
		It("outputs version information in human format", func() {
			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"version"})
			err := cmd.Execute()
			Expect(err).NotTo(HaveOccurred())
			output := stdout.String()
			Expect(output).To(ContainSubstring("crosscodex"))
			Expect(output).To(ContainSubstring("go"))
		})

		It("outputs version information in JSON format", func() {
			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"version", "--json"})
			err := cmd.Execute()
			Expect(err).NotTo(HaveOccurred())
			var result map[string]any
			Expect(json.Unmarshal(stdout.Bytes(), &result)).To(Succeed())
			Expect(result).To(HaveKey("version"))
			Expect(result).To(HaveKey("go_version"))
			Expect(result).To(HaveKey("os"))
			Expect(result).To(HaveKey("arch"))
		})

		It("is accessible via --version on root", func() {
			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"--version"})
			err := cmd.Execute()
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout.String()).To(ContainSubstring("crosscodex"))
		})

		It("is accessible via alias 'ver'", func() {
			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"ver"})
			err := cmd.Execute()
			Expect(err).NotTo(HaveOccurred())
			output := stdout.String()
			Expect(output).To(ContainSubstring("crosscodex"))
			Expect(output).To(ContainSubstring("go"))
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
					cmd := newRootCmd()
					cmd.SetOut(&stdout)
					cmd.SetErr(&stderr)
					cmd.SetArgs([]string{"project", "init", "--dir", tmpDir})
					err := cmd.Execute()
					Expect(err).NotTo(HaveOccurred())

					Expect(filepath.Join(tmpDir, ".crosscodex", "config.yaml")).To(BeAnExistingFile())
					Expect(filepath.Join(tmpDir, ".crosscodex", "prompts")).To(BeADirectory())
				})

				It("errors when .crosscodex already exists", func() {
					Expect(os.MkdirAll(filepath.Join(tmpDir, ".crosscodex"), 0o755)).To(Succeed())
					cmd := newRootCmd()
					cmd.SetOut(&stdout)
					cmd.SetErr(&stderr)
					cmd.SetArgs([]string{"project", "init", "--dir", tmpDir})
					err := cmd.Execute()
					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring("already exists"))
				})

				It("overwrites with --force", func() {
					Expect(os.MkdirAll(filepath.Join(tmpDir, ".crosscodex"), 0o755)).To(Succeed())
					cmd := newRootCmd()
					cmd.SetOut(&stdout)
					cmd.SetErr(&stderr)
					cmd.SetArgs([]string{"project", "init", "--dir", tmpDir, "--force"})
					err := cmd.Execute()
					Expect(err).NotTo(HaveOccurred())
					Expect(filepath.Join(tmpDir, ".crosscodex", "config.yaml")).To(BeAnExistingFile())
				})
			})

			Describe("project config", func() {
				It("outputs resolved config as JSON", func() {
					cmd := newRootCmd()
					cmd.SetOut(&stdout)
					cmd.SetErr(&stderr)
					cmd.SetArgs([]string{"project", "config", "--json"})
					err := cmd.Execute()
					Expect(err).NotTo(HaveOccurred())
					var result map[string]any
					Expect(json.Unmarshal(stdout.Bytes(), &result)).To(Succeed())
				})
			})

			Describe("project list", func() {
				It("lists current directory when no .crosscodex found", func() {
					cmd := newRootCmd()
					cmd.SetOut(&stdout)
					cmd.SetErr(&stderr)
					cmd.SetArgs([]string{"project", "list"})
					err := cmd.Execute()
					Expect(err).NotTo(HaveOccurred())
					Expect(stdout.String()).To(ContainSubstring("No CrossCodex projects"))
				})
			})

			Describe("project status", func() {
				It("reports daemon status", func() {
					cmd := newRootCmd()
					cmd.SetOut(&stdout)
					cmd.SetErr(&stderr)
					cmd.SetArgs([]string{"project", "status"})
					err := cmd.Execute()
					Expect(err).NotTo(HaveOccurred())
					Expect(stdout.String()).To(ContainSubstring("Daemon:"))
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
				It("outputs resolved config in human format", func() {
					cmd := newRootCmd()
					cmd.SetOut(&stdout)
					cmd.SetErr(&stderr)
					cmd.SetArgs([]string{"config", "show"})
					err := cmd.Execute()
					Expect(err).NotTo(HaveOccurred())
					output := stdout.String()
					Expect(output).To(ContainSubstring("output:"))
					Expect(output).To(ContainSubstring("endpoint:"))
				})

				It("outputs resolved config in JSON format", func() {
					cmd := newRootCmd()
					cmd.SetOut(&stdout)
					cmd.SetErr(&stderr)
					cmd.SetArgs([]string{"config", "show", "--json"})
					err := cmd.Execute()
					Expect(err).NotTo(HaveOccurred())
					var result map[string]any
					Expect(json.Unmarshal(stdout.Bytes(), &result)).To(Succeed())
					Expect(result).To(HaveKey("output"))
					Expect(result).To(HaveKey("endpoint"))
				})
			})

			Describe("config set", func() {
				It("writes a key-value pair to user config", func() {
					userConfigPath := filepath.Join(tmpConfigDir, "crosscodex", "config.yaml")

					cmd := newRootCmd()
					cmd.SetOut(&stdout)
					cmd.SetErr(&stderr)
					cmd.SetArgs([]string{"config", "set", "cli.output", "plain"})
					err := cmd.Execute()
					Expect(err).NotTo(HaveOccurred())

					Expect(userConfigPath).To(BeAnExistingFile())
					data, err := os.ReadFile(userConfigPath)
					Expect(err).NotTo(HaveOccurred())
					Expect(string(data)).To(ContainSubstring("output: plain"))
				})

				It("errors when key is missing", func() {
					cmd := newRootCmd()
					cmd.SetOut(&stdout)
					cmd.SetErr(&stderr)
					cmd.SetArgs([]string{"config", "set"})
					err := cmd.Execute()
					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring("key"))
				})

				It("errors when value is missing", func() {
					cmd := newRootCmd()
					cmd.SetOut(&stdout)
					cmd.SetErr(&stderr)
					cmd.SetArgs([]string{"config", "set", "cli.output"})
					err := cmd.Execute()
					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring("value"))
				})
			})

			Describe("config profiles", func() {
				It("lists profile files", func() {
					profileDir := filepath.Join(tmpConfigDir, "crosscodex", "profiles")
					Expect(os.MkdirAll(profileDir, 0o755)).To(Succeed())
					Expect(os.WriteFile(filepath.Join(profileDir, "dev.yaml"), []byte("cli:\n  output: json\n"), 0o644)).To(Succeed())
					Expect(os.WriteFile(filepath.Join(profileDir, "prod.yaml"), []byte("cli:\n  output: plain\n"), 0o644)).To(Succeed())

					cmd := newRootCmd()
					cmd.SetOut(&stdout)
					cmd.SetErr(&stderr)
					cmd.SetArgs([]string{"config", "profiles"})
					err := cmd.Execute()
					Expect(err).NotTo(HaveOccurred())
					output := stdout.String()
					Expect(output).To(ContainSubstring("dev"))
					Expect(output).To(ContainSubstring("prod"))
				})

				It("reports when no profiles exist", func() {
					cmd := newRootCmd()
					cmd.SetOut(&stdout)
					cmd.SetErr(&stderr)
					cmd.SetArgs([]string{"config", "profiles"})
					err := cmd.Execute()
					Expect(err).NotTo(HaveOccurred())
					output := stdout.String()
					Expect(output).To(ContainSubstring("No profiles"))
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
			Describe("run start", func() {
				It("requires a file argument", func() {
					cmd := newRootCmd()
					cmd.SetOut(&stdout)
					cmd.SetErr(&stderr)
					cmd.SetArgs([]string{"run", "start"})
					err := cmd.Execute()
					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring("file"))
				})

			})

			Describe("run status", func() {
				It("requires a job-id argument", func() {
					cmd := newRootCmd()
					cmd.SetOut(&stdout)
					cmd.SetErr(&stderr)
					cmd.SetArgs([]string{"run", "status"})
					err := cmd.Execute()
					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring("job-id"))
				})
			})

			Describe("run cancel", func() {
				It("requires a job-id argument", func() {
					cmd := newRootCmd()
					cmd.SetOut(&stdout)
					cmd.SetErr(&stderr)
					cmd.SetArgs([]string{"run", "cancel"})
					err := cmd.Execute()
					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring("job-id"))
				})
			})

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
				cmd := newRootCmd()
				cmd.SetOut(&stdout)
				cmd.SetErr(&stderr)
				cmd.SetArgs([]string{"catalog", "validate", tmpFile})
				err := cmd.Execute()
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("JSON"))
			})

			It("rejects JSON without catalog key", func() {
				Expect(os.WriteFile(tmpFile, []byte(`{"foo": "bar"}`), 0o644)).To(Succeed())
				cmd := newRootCmd()
				cmd.SetOut(&stdout)
				cmd.SetErr(&stderr)
				cmd.SetArgs([]string{"catalog", "validate", tmpFile})
				err := cmd.Execute()
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("catalog"))
			})

			It("accepts valid OSCAL catalog structure", func() {
				Expect(os.WriteFile(tmpFile, []byte(`{"catalog": {"uuid": "test", "metadata": {"title": "Test"}}}`), 0o644)).To(Succeed())
				cmd := newRootCmd()
				cmd.SetOut(&stdout)
				cmd.SetErr(&stderr)
				cmd.SetArgs([]string{"catalog", "validate", tmpFile})
				err := cmd.Execute()
				Expect(err).NotTo(HaveOccurred())
				output := stdout.String()
				Expect(output).To(ContainSubstring("valid"))
			})

			It("requires a file argument", func() {
				cmd := newRootCmd()
				cmd.SetOut(&stdout)
				cmd.SetErr(&stderr)
				cmd.SetArgs([]string{"catalog", "validate"})
				err := cmd.Execute()
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("file"))
			})
		})

		Describe("catalog import", func() {
			It("requires a file argument", func() {
				cmd := newRootCmd()
				cmd.SetOut(&stdout)
				cmd.SetErr(&stderr)
				cmd.SetArgs([]string{"catalog", "import"})
				err := cmd.Execute()
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("file"))
			})

			It("errors when file does not exist or daemon unavailable", func() {
				cmd := newRootCmd()
				cmd.SetOut(&stdout)
				cmd.SetErr(&stderr)
				cmd.SetArgs([]string{"catalog", "import", "/nonexistent/path.json"})
				err := cmd.Execute()
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(Or(
					ContainSubstring("read file"),
					ContainSubstring("cannot reach"),
					ContainSubstring("failed"),
					ContainSubstring("database not configured"),
				))
			})
		})

		Describe("catalog inspect", func() {
			It("requires a catalog ID argument", func() {
				cmd := newRootCmd()
				cmd.SetOut(&stdout)
				cmd.SetErr(&stderr)
				cmd.SetArgs([]string{"catalog", "inspect"})
				err := cmd.Execute()
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("ID"))
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
})
