package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

// fakeFileInfo is an os.FileInfo whose Sys() reports the given owner.
type fakeFileInfo struct{ uid, gid uint32 }

func (f fakeFileInfo) Name() string       { return "config.yaml" }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() os.FileMode  { return 0o600 }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return false }
func (f fakeFileInfo) Sys() any           { return &syscall.Stat_t{Uid: f.uid, Gid: f.gid} }

var _ = Describe("Editing the user config", func() {
	var xdg, userPath, dropIn string

	BeforeEach(func() {
		xdg = GinkgoT().TempDir()
		GinkgoT().Setenv("XDG_CONFIG_HOME", xdg)
		userPath = filepath.Join(xdg, "crosscodex", "config.yaml")
		dropIn = filepath.Join(xdg, "crosscodex", "conf.d", "50-site.yaml")
		Expect(os.MkdirAll(filepath.Dir(userPath), 0o755)).To(Succeed())
	})

	writeUser := func(content string) {
		Expect(os.WriteFile(userPath, []byte(content), 0o600)).To(Succeed())
	}
	expectUser := func(content string) {
		data, err := os.ReadFile(userPath)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(data)).To(Equal(content))
		entries, err := os.ReadDir(filepath.Dir(userPath))
		Expect(err).NotTo(HaveOccurred())
		for _, e := range entries {
			Expect(e.Name()).NotTo(ContainSubstring(".tmp-"))
		}
	}

	Describe("config unset", func() {
		It("removes a key and keeps the rest of the file", func() {
			writeUser("logging:\n  level: debug # quiet\n  format: json\ncli:\n  output: json\n")
			_, _, err := execute(newRootCmd(), "config", "unset", "logging.level")
			Expect(err).NotTo(HaveOccurred())
			data, err := os.ReadFile(userPath)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(data)).NotTo(ContainSubstring("level"))
			Expect(string(data)).To(ContainSubstring("format: json"))
			Expect(string(data)).To(ContainSubstring("output: json"))
		})

		It("removes an unknown key, repairing a user config that does not load", func() {
			writeUser("loging:\n  level: debug\n")
			_, stderr, err := execute(newRootCmd(), "config", "unset", "loging")
			Expect(err).NotTo(HaveOccurred())
			Expect(stderr).To(ContainSubstring("crosscodex config unset"))
			_, _, err = execute(newRootCmd(), "version")
			Expect(err).NotTo(HaveOccurred())
		})

		It("refuses a key the user config does not set, leaving the file unchanged", func() {
			writeUser("cli:\n  output: json\n")
			_, _, err := execute(newRootCmd(), "config", "unset", "logging.level")
			Expect(err).To(MatchError(errKeyNotSet))
			Expect(err).To(MatchError(ContainSubstring("logging.level is not set in " + userPath)))
			expectUser("cli:\n  output: json\n")
		})

		It("refuses a change that would stop the configuration loading, leaving the file unchanged", func() {
			const content = "tls:\n  mode: mutual\n  ca: /c.pem\n  cert: /t.pem\n  key: /k.pem\n"
			writeUser(content)
			_, _, err := execute(newRootCmd(), "config", "unset", "tls.ca")
			Expect(err).To(MatchError(config.ErrInvalidConfig))
			Expect(err).To(MatchError(ContainSubstring("requires cert, key, and ca")))
			Expect(err).To(MatchError(ContainSubstring("not written")))
			expectUser(content)
		})
	})

	DescribeTable("refuses set and unset while another layer breaks the configuration, naming it",
		func(args ...string) {
			const content = "logging:\n  level: debug\n"
			writeUser(content)
			Expect(os.MkdirAll(filepath.Dir(dropIn), 0o755)).To(Succeed())
			Expect(os.WriteFile(dropIn, []byte("pipeline:\n  stage_timout: 1h\n"), 0o600)).To(Succeed())

			_, stderr, err := execute(newRootCmd(), args...)
			Expect(err).To(MatchError(config.ErrUnknownKey))
			Expect(err).To(MatchError(ContainSubstring("already fails to load")))
			Expect(err).To(MatchError(ContainSubstring(dropIn + ":2:3")))
			Expect(err).To(MatchError(ContainSubstring("fix that first")))
			Expect(stderr).To(ContainSubstring(dropIn))
			Expect(stderr).NotTo(ContainSubstring("config set <key>"))
			expectUser(content)
		},
		Entry("config set", "config", "set", "logging.format", "json"),
		Entry("config unset", "config", "unset", "logging.level"),
	)

	It("refuses a change that does not repair a user config that already fails to load", func() {
		const content = "loging:\n  level: debug\n"
		writeUser(content)
		_, _, err := execute(newRootCmd(), "config", "set", "logging.format", "bogus")
		Expect(err).To(MatchError(config.ErrUnknownKey))
		Expect(err).To(MatchError(ContainSubstring("not written")))
		Expect(err).To(MatchError(ContainSubstring("already fails to load")))
		expectUser(content)
	})

	Describe("repairing a user config that fails to load", func() {
		It("writes a change that fixes one of two invalid values, and reports the other", func() {
			writeUser("logging:\n  level: loud\n  format: yaml\n")
			_, stderr, err := execute(newRootCmd(), "config", "set", "logging.level", "debug")
			Expect(err).NotTo(HaveOccurred())
			Expect(stderr).To(ContainSubstring("the change was written, but the configuration does not load"))
			Expect(stderr).To(ContainSubstring("logging.format"))
			data, err := os.ReadFile(userPath)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(data)).To(ContainSubstring("level: debug"))

			_, _, err = execute(newRootCmd(), "config", "set", "logging.format", "json")
			Expect(err).NotTo(HaveOccurred())
			_, _, err = execute(newRootCmd(), "version")
			Expect(err).NotTo(HaveOccurred())
		})

		It("writes an unset that removes an unknown key next to an invalid value", func() {
			writeUser("loging:\n  level: debug\nlogging:\n  level: loud\n")
			_, stderr, err := execute(newRootCmd(), "config", "unset", "loging")
			Expect(err).NotTo(HaveOccurred())
			Expect(stderr).To(ContainSubstring("logging.level"))
			data, err := os.ReadFile(userPath)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(data)).NotTo(ContainSubstring("loging"))
			Expect(string(data)).To(ContainSubstring("level: loud"))
		})

		DescribeTable("refuses a change that is not a strict repair, leaving the file unchanged",
			func(content string, args []string, wantMsgs ...string) {
				writeUser(content)
				_, _, err := execute(newRootCmd(), args...)
				Expect(err).To(MatchError(config.ErrInvalidConfig))
				Expect(err).To(MatchError(ContainSubstring("not written")))
				Expect(err).To(MatchError(ContainSubstring("already fails to load")))
				for _, msg := range wantMsgs {
					Expect(err).To(MatchError(ContainSubstring(msg)))
				}
				expectUser(content)
			},
			Entry("one that adds a new problem",
				"logging:\n  level: loud\n", []string{"config", "set", "logging.format", "bogus"},
				"new problems: logging.format"),
			Entry("one that replaces a bad value with another bad value",
				"logging:\n  level: loud\n", []string{"config", "set", "logging.level", "louder"},
				"still failing: logging.level"),
			Entry("one that fixes a problem but causes another",
				"logging:\n  level: loud\nbackup:\n  destination:\n    backend: azure\n",
				[]string{"config", "set", "backup.destination.backend", "local"},
				"new problems: backup.destination.local.path, backup.dsn"),
		)

		It("writes a change that fixes a value of the wrong type next to an invalid value", func() {
			writeUser("pipeline:\n  max_concurrent_jobs: many\nlogging:\n  level: loud\n")
			_, stderr, err := execute(newRootCmd(), "config", "set", "pipeline.max_concurrent_jobs", "8")
			Expect(err).NotTo(HaveOccurred())
			_, after, found := strings.Cut(stderr, "the change was written")
			Expect(found).To(BeTrue())
			Expect(after).To(ContainSubstring("logging.level"))
			Expect(after).NotTo(ContainSubstring("max_concurrent_jobs"))
		})
	})

	DescribeTable("refuses to edit through an alias or a value, leaving the file unchanged",
		func(content, wantMsg string, args ...string) {
			writeUser(content)
			_, _, err := execute(newRootCmd(), args...)
			Expect(err).To(MatchError(errNotASection))
			Expect(err).To(MatchError(ContainSubstring(wantMsg)))
			expectUser(content)
		},
		Entry("set through an aliased section",
			"llm:\n  tenant_overrides:\n    acme-corp: &o\n      default_model: m\n    beta-inc: *o\n",
			"llm.tenant_overrides.beta-inc is an alias to another section; edit the anchor or the file by hand",
			"config", "set", "llm.tenant_overrides.beta-inc.default_model", "x"),
		Entry("unset through an aliased section",
			"llm:\n  tenant_overrides:\n    acme-corp: &o\n      default_model: m\n    beta-inc: *o\n",
			"llm.tenant_overrides.beta-inc is an alias to another section",
			"config", "unset", "llm.tenant_overrides.beta-inc.default_model"),
		Entry("set below a value", "cli:\n  output: json\n",
			"cli.output is a value, not a section", "config", "set", "cli.output.mode", "x"),
		Entry("set below a list", "llm:\n  allowed_models: [a]\n",
			"llm.allowed_models is a value, not a section", "config", "set", "llm.allowed_models.x", "y"),
	)

	Describe("YAML anchors", func() {
		const sharedSection = "llm:\n  tenant_overrides:\n    acme-corp: &o\n      default_model: m\n    beta-inc:\n      <<: *o\n"
		const sharedValue = "llm:\n  default_model: &m granite\n  tenant_overrides:\n    acme-corp:\n      default_model: *m\n"

		DescribeTable("refuses an edit that would also change what an alias refers to, leaving the file unchanged",
			func(content, wantMsg string, args ...string) {
				writeUser(content)
				_, _, err := execute(newRootCmd(), args...)
				Expect(err).To(MatchError(errNotASection))
				Expect(err).To(MatchError(ContainSubstring(wantMsg)))
				Expect(err).To(MatchError(ContainSubstring("edit the file by hand")))
				expectUser(content)
			},
			Entry("set inside an anchored section", sharedSection,
				"llm.tenant_overrides.acme-corp carries the anchor &o, which the alias at llm.tenant_overrides.beta-inc.<< refers to",
				"config", "set", "llm.tenant_overrides.acme-corp.default_model", "x"),
			Entry("unset inside an anchored section", sharedSection,
				"llm.tenant_overrides.acme-corp carries the anchor &o",
				"config", "unset", "llm.tenant_overrides.acme-corp.default_model"),
			Entry("set of an anchored value", sharedValue,
				"llm.default_model carries the anchor &m, which the alias at llm.tenant_overrides.acme-corp.default_model refers to",
				"config", "set", "llm.default_model", "x"),
			Entry("unset of an anchored value", sharedValue,
				"llm.default_model carries the anchor &m",
				"config", "unset", "llm.default_model"),
			Entry("unset of a section holding an anchor an alias outside it refers to",
				"llm:\n  tenant_overrides:\n    acme-corp: &o\n      timeout: 5\n  <<: *o\n",
				"llm.tenant_overrides.acme-corp carries the anchor &o, which the alias at llm.<< refers to",
				"config", "unset", "llm.tenant_overrides"),
		)

		It("lets unset remove a section whose anchors only aliases inside it refer to", func() {
			writeUser("llm:\n  tenant_overrides:\n    acme-corp: &o\n      timeout: 5\n    beta-inc: *o\n")
			_, _, err := execute(newRootCmd(), "config", "unset", "llm.tenant_overrides")
			Expect(err).NotTo(HaveOccurred())
			data, err := os.ReadFile(userPath)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(data)).NotTo(ContainSubstring("tenant_overrides"))
		})

		It("keeps an anchor no alias refers to on the value it replaces", func() {
			writeUser("llm:\n  default_model: &m granite\n")
			_, _, err := execute(newRootCmd(), "config", "set", "llm.default_model", "x")
			Expect(err).NotTo(HaveOccurred())
			data, err := os.ReadFile(userPath)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(data)).To(ContainSubstring("default_model: &m x"))
		})

		It("keeps an anchor no alias refers to on a null section it fills in", func() {
			writeUser("logging: &l\n")
			_, _, err := execute(newRootCmd(), "config", "set", "logging.level", "debug")
			Expect(err).NotTo(HaveOccurred())
			data, err := os.ReadFile(userPath)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(data)).To(ContainSubstring("logging: &l"))
			Expect(string(data)).To(ContainSubstring("level: debug"))
		})
	})

	It("fills in a section that is empty (null)", func() {
		writeUser("logging:\n")
		_, _, err := execute(newRootCmd(), "config", "set", "logging.level", "debug")
		Expect(err).NotTo(HaveOccurred())
		data, err := os.ReadFile(userPath)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(data)).To(ContainSubstring("level: debug"))
	})

	DescribeTable("refuses to write through a dangling symlink, leaving the link in place",
		func(args ...string) {
			target := filepath.Join(xdg, "dotfiles", "crosscodex.yaml")
			Expect(os.Symlink(target, userPath)).To(Succeed())
			_, _, err := execute(newRootCmd(), args...)
			Expect(err).To(MatchError(ContainSubstring(userPath + " is a symlink to " + target + ", which does not exist")))
			info, err := os.Lstat(userPath)
			Expect(err).NotTo(HaveOccurred())
			Expect(info.Mode() & os.ModeSymlink).NotTo(BeZero())
			Expect(target).NotTo(BeAnExistingFile())
		},
		Entry("config set", "config", "set", "logging.level", "debug"),
		Entry("config unset", "config", "unset", "logging.level"),
	)

	Describe("file ownership", func() {
		It("refuses, naming the owner, when the temporary file cannot be given the config's owner", func() {
			if os.Getuid() == 0 {
				Skip("root may chown to any owner, so the refusal cannot happen")
			}
			tmp, err := os.CreateTemp(GinkgoT().TempDir(), "cfg")
			Expect(err).NotTo(HaveOccurred())
			defer tmp.Close()
			err = matchOwner(tmp, "/x/config.yaml", fakeFileInfo{uid: 0, gid: 0})
			Expect(err).To(MatchError(ContainSubstring("/x/config.yaml is owned by 0:0; run config set as that user")))
		})

		It("leaves the temporary file alone when the owner already matches", func() {
			tmp, err := os.CreateTemp(GinkgoT().TempDir(), "cfg")
			Expect(err).NotTo(HaveOccurred())
			defer tmp.Close()
			Expect(matchOwner(tmp, "/x/config.yaml", fakeFileInfo{uid: uint32(os.Getuid()), gid: uint32(os.Getgid())})).To(Succeed())
		})

		It("keeps another user's ownership of the config when run as root", func() {
			if os.Getuid() != 0 {
				Skip("only root can create a config owned by another user; this spec needs to run as root")
			}
			writeUser("logging:\n  level: info\n")
			Expect(os.Chown(userPath, 65534, 65534)).To(Succeed())
			_, _, err := execute(newRootCmd(), "config", "set", "logging.level", "debug")
			Expect(err).NotTo(HaveOccurred())
			info, err := os.Stat(userPath)
			Expect(err).NotTo(HaveOccurred())
			st := info.Sys().(*syscall.Stat_t)
			Expect([]uint32{st.Uid, st.Gid}).To(Equal([]uint32{65534, 65534}))
		})
	})

	It("says in the help text that --profile checks only that profile", func() {
		for _, sub := range []string{"set", "unset"} {
			stdout, _, err := execute(newRootCmd(), "config", sub, "--help")
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(ContainSubstring("--profile"))
			Expect(stdout).To(ContainSubstring("only that profile"))
		}
	})
})

var _ = Describe("Config paths with quoted keys", func() {
	It("lets config unset remove a map key that contains a dot", func() {
		xdg := GinkgoT().TempDir()
		GinkgoT().Setenv("XDG_CONFIG_HOME", xdg)
		userPath := filepath.Join(xdg, "crosscodex", "config.yaml")
		Expect(os.MkdirAll(filepath.Dir(userPath), 0o755)).To(Succeed())
		Expect(os.WriteFile(userPath, []byte("llm:\n  tenant_overrides:\n    \"a.b\":\n      timeout: 5\n"), 0o600)).To(Succeed())

		_, _, err := execute(newRootCmd(), "config", "unset", `llm.tenant_overrides["a.b"]`)
		Expect(err).NotTo(HaveOccurred())
		_, _, err = execute(newRootCmd(), "version")
		Expect(err).NotTo(HaveOccurred())
	})

	It("rejects a malformed path with ErrInvalidPath", func() {
		_, _, err := execute(newRootCmd(), "config", "set", "logging..level", "debug")
		Expect(err).To(MatchError(config.ErrInvalidPath))
	})
})

var _ = Describe("Judging a repair", func() {
	fe := func(path string) *config.FieldError {
		return &config.FieldError{Path: path, Err: fmt.Errorf("%s is wrong: %w", path, config.ErrInvalidConfig)}
	}
	paths := func(fes ...*config.FieldError) []*config.FieldError { return fes }

	DescribeTable("compareProblems compares paths as multisets",
		func(before, after []*config.FieldError, wantStill, wantAdded []string) {
			still, added := compareProblems(before, after)
			Expect(still).To(Equal(wantStill))
			Expect(added).To(Equal(wantAdded))
		},
		Entry("one of two fixed", paths(fe("a"), fe("b")), paths(fe("b")), []string{"b"}, []string(nil)),
		Entry("a second problem at a path that had one", paths(fe("a"), fe("b")), paths(fe("b"), fe("b")), []string{"b"}, []string{"b"}),
		Entry("one of two at the same path fixed", paths(fe("a"), fe("a")), paths(fe("a")), []string{"a"}, []string(nil)),
		Entry("a new path", paths(fe("a")), paths(fe("a"), fe("c")), []string{"a"}, []string{"c"}),
		Entry("all fixed", paths(fe("a")), paths(), []string(nil), []string(nil)),
	)

	loadFile := func(content string) (string, error) {
		GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())
		path := filepath.Join(GinkgoT().TempDir(), "config.yaml")
		Expect(os.WriteFile(path, []byte(content), 0o600)).To(Succeed())
		_, err := config.NewLoader().Load(context.Background(), config.WithConfigPath(path))
		return path, err
	}
	noOtherLayer := func() error { return nil }

	It("accepts fixing one of two problems at the same path", func() {
		file, before := loadFile("analysis:\n  relationship:\n    actionable_types: [BOGUS, WRONG]\n")
		Expect(config.FieldErrors(before)).To(HaveLen(2))
		_, after := loadFile("analysis:\n  relationship:\n    actionable_types: [EQUIVALENT, WRONG]\n")
		remaining, err := judgeRepair(file, "analysis.relationship.actionable_types", before, after, noOtherLayer)
		Expect(err).NotTo(HaveOccurred())
		Expect(remaining).To(MatchError(ContainSubstring("WRONG")))
	})

	It("refuses a second problem at a path that had one", func() {
		file, before := loadFile("analysis:\n  relationship:\n    actionable_types: [BOGUS]\nlogging:\n  level: loud\n")
		_, after := loadFile("analysis:\n  relationship:\n    actionable_types: [BOGUS, WRONG]\n")
		_, err := judgeRepair(file, "analysis.relationship.actionable_types", before, after, noOtherLayer)
		Expect(err).To(MatchError(ContainSubstring("new problems: analysis.relationship.actionable_types")))
	})

	DescribeTable("falls back to the strict rule when a problem has no path",
		func(current, candidate error, otherLayer error, wantMsg string) {
			remaining, err := judgeRepair("/u/config.yaml", "logging.level", current, candidate, func() error { return otherLayer })
			Expect(remaining).NotTo(HaveOccurred())
			Expect(err).To(MatchError(ContainSubstring(wantMsg)))
		},
		Entry("an untagged current problem",
			fmt.Errorf("x: %w", config.ErrInvalidConfig), error(fe("logging.level")), nil, "does not make it load"),
		Entry("an untagged candidate problem",
			errors.Join(fe("logging.level"), fe("logging.format")), fmt.Errorf("x: %w", config.ErrInvalidConfig), nil, "does not make it load"),
		Entry("an untagged current problem while another layer fails alone",
			fmt.Errorf("x: %w", config.ErrInvalidConfig), error(fe("logging.level")), fmt.Errorf("y: %w", config.ErrInvalidConfig), "layer other than /u/config.yaml"),
	)
})

var _ = Describe("Telling another layer's problems from the user config's", func() {
	It("lets set repair the user config while a drop-in only loads together with it", func() {
		xdg := GinkgoT().TempDir()
		GinkgoT().Setenv("XDG_CONFIG_HOME", xdg)
		userPath := filepath.Join(xdg, "crosscodex", "config.yaml")
		dropIn := filepath.Join(xdg, "crosscodex", "conf.d", "50-gateway.yaml")
		Expect(os.MkdirAll(filepath.Dir(dropIn), 0o755)).To(Succeed())
		Expect(os.WriteFile(dropIn, []byte("llm:\n  gateway_mode: true\n"), 0o600)).To(Succeed())
		Expect(os.WriteFile(userPath, []byte("llm:\n  gateway_url: http://litellm:4000\nlogging:\n  level: loud\n  format: yaml\n"), 0o600)).To(Succeed())

		_, stderr, err := execute(newRootCmd(), "config", "set", "logging.level", "debug")
		Expect(err).NotTo(HaveOccurred())
		Expect(stderr).To(ContainSubstring("logging.format"))
	})

	It("refuses while a problem comes from another layer, naming it", func() {
		xdg := GinkgoT().TempDir()
		GinkgoT().Setenv("XDG_CONFIG_HOME", xdg)
		userPath := filepath.Join(xdg, "crosscodex", "config.yaml")
		dropIn := filepath.Join(xdg, "crosscodex", "conf.d", "50-site.yaml")
		Expect(os.MkdirAll(filepath.Dir(dropIn), 0o755)).To(Succeed())
		Expect(os.WriteFile(dropIn, []byte("logging:\n  format: yaml\n"), 0o600)).To(Succeed())
		const content = "logging:\n  level: loud\n"
		Expect(os.WriteFile(userPath, []byte(content), 0o600)).To(Succeed())

		_, _, err := execute(newRootCmd(), "config", "set", "logging.level", "debug")
		Expect(err).To(MatchError(ContainSubstring("already fails to load because of " + dropIn)))
		Expect(err).To(MatchError(ContainSubstring("fix that first")))
		data, readErr := os.ReadFile(userPath)
		Expect(readErr).NotTo(HaveOccurred())
		Expect(string(data)).To(Equal(content))
	})
})

var _ = Describe("Rendering configuration problems", func() {
	var xdg, userPath string

	BeforeEach(func() {
		xdg = GinkgoT().TempDir()
		GinkgoT().Setenv("XDG_CONFIG_HOME", xdg)
		userPath = filepath.Join(xdg, "crosscodex", "config.yaml")
		Expect(os.MkdirAll(filepath.Dir(userPath), 0o755)).To(Succeed())
	})

	// expectBullets asserts out is one summary line followed by one
	// "  - " line per problem, each naming one of paths, and that no line
	// ends with the repeated sentinel text.
	expectBullets := func(out string, paths ...string) {
		lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
		Expect(lines).To(HaveLen(1+len(paths)), out)
		Expect(lines[0]).NotTo(HavePrefix("  - "))
		for i, p := range paths {
			Expect(lines[1+i]).To(HavePrefix("  - "))
			Expect(lines[1+i]).To(ContainSubstring(p))
			Expect(lines[1+i]).NotTo(HaveSuffix("invalid configuration"))
		}
	}

	It("lists each problem of a refused edit on its own line under one summary", func() {
		Expect(os.WriteFile(userPath, []byte("logging:\n  level: loud\n"), 0o600)).To(Succeed())
		_, _, err := execute(newRootCmd(), "config", "set", "logging.format", "bogus")
		Expect(err).To(HaveOccurred())
		out := formatCLIError(err, false)
		Expect(out).To(HavePrefix("error: set config: " + userPath + " was not written"))
		expectBullets(out, "logging.level", "logging.format")
		Expect(err.Error()).To(ContainSubstring(": invalid configuration"), "the error value keeps its text")
	})

	It("lists the problems left after a partial repair", func() {
		Expect(os.WriteFile(userPath, []byte("logging:\n  level: loud\n  format: yaml\ntls:\n  mode: bogus\n"), 0o600)).To(Succeed())
		_, stderr, err := execute(newRootCmd(), "config", "set", "logging.level", "debug")
		Expect(err).NotTo(HaveOccurred())
		_, after, found := strings.Cut(stderr, "warning: the change was written")
		Expect(found).To(BeTrue())
		expectBullets("warning: the change was written"+after, "tls.mode", "logging.format")
	})

	It("lists the problems in the startup warning, followed by the advice", func() {
		Expect(os.WriteFile(userPath, []byte("logging:\n  level: loud\n  format: yaml\n"), 0o600)).To(Succeed())
		_, stderr, _ := execute(newRootCmd(), "config", "profiles")
		lines := strings.Split(strings.TrimRight(stderr, "\n"), "\n")
		Expect(lines).To(HaveLen(4), stderr)
		Expect(lines[0]).To(HavePrefix("warning: "))
		Expect(lines[1]).To(HavePrefix("  - "))
		Expect(lines[2]).To(HavePrefix("  - "))
		Expect(lines[3]).To(ContainSubstring("crosscodex config set"))
	})

	It("leaves an error without configuration problems as it is", func() {
		err := fmt.Errorf("dial crosscodexd: %w", errors.New("connection refused"))
		Expect(formatCLIError(err, false)).To(Equal("error: dial crosscodexd: connection refused"))
	})

	It("keeps JSON output as the raw error text", func() {
		Expect(os.WriteFile(userPath, []byte("logging:\n  level: loud\n"), 0o600)).To(Succeed())
		_, _, err := execute(newRootCmd(), "config", "set", "logging.format", "bogus")
		Expect(formatCLIError(err, true)).To(ContainSubstring(`invalid configuration`))
		Expect(formatCLIError(err, true)).NotTo(ContainSubstring("  - "))
	})
})
