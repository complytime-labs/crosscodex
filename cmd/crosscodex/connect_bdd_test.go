package main

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Connection Management", func() {
	Describe("xdgStateDir", func() {
		It("uses XDG_STATE_HOME when set", func() {
			original := os.Getenv("XDG_STATE_HOME")
			defer os.Setenv("XDG_STATE_HOME", original)
			os.Setenv("XDG_STATE_HOME", "/tmp/test-state")
			Expect(xdgStateDir()).To(Equal("/tmp/test-state/crosscodex"))
		})

		It("falls back to ~/.local/state when XDG_STATE_HOME is empty", func() {
			original := os.Getenv("XDG_STATE_HOME")
			defer os.Setenv("XDG_STATE_HOME", original)
			os.Unsetenv("XDG_STATE_HOME")
			home, _ := os.UserHomeDir()
			Expect(xdgStateDir()).To(Equal(filepath.Join(home, ".local", "state", "crosscodex")))
		})
	})

	Describe("PID file", func() {
		var tmpDir string

		BeforeEach(func() {
			var err error
			tmpDir, err = os.MkdirTemp("", "crosscodex-test-*")
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			os.RemoveAll(tmpDir)
		})

		It("round-trips port through PID file", func() {
			path := filepath.Join(tmpDir, "daemon.pid")
			Expect(writePIDFile(path, 12345)).To(Succeed())
			port, alive := readPIDFile(path)
			Expect(port).To(Equal(12345))
			Expect(alive).To(BeTrue())
		})

		It("returns zero port for missing file", func() {
			path := filepath.Join(tmpDir, "nonexistent.pid")
			port, alive := readPIDFile(path)
			Expect(port).To(Equal(0))
			Expect(alive).To(BeFalse())
		})

		It("returns zero for single-line file", func() {
			path := filepath.Join(tmpDir, "bad.pid")
			Expect(os.WriteFile(path, []byte("12345"), 0o600)).To(Succeed())
			port, alive := readPIDFile(path)
			Expect(port).To(Equal(0))
			Expect(alive).To(BeFalse())
		})

		It("returns zero for non-numeric PID", func() {
			path := filepath.Join(tmpDir, "bad.pid")
			Expect(os.WriteFile(path, []byte("notanumber\n50051\n"), 0o600)).To(Succeed())
			port, alive := readPIDFile(path)
			Expect(port).To(Equal(0))
			Expect(alive).To(BeFalse())
		})

		It("returns zero for non-numeric port", func() {
			path := filepath.Join(tmpDir, "bad.pid")
			Expect(os.WriteFile(path, []byte("12345\nnotaport\n"), 0o600)).To(Succeed())
			port, alive := readPIDFile(path)
			Expect(port).To(Equal(0))
			Expect(alive).To(BeFalse())
		})

		It("returns false alive for dead process", func() {
			path := filepath.Join(tmpDir, "dead.pid")
			Expect(os.WriteFile(path, []byte("999999999\n50051\n"), 0o600)).To(Succeed())
			port, alive := readPIDFile(path)
			Expect(port).To(Equal(50051))
			Expect(alive).To(BeFalse())
		})
	})

	Describe("needsConnection", func() {
		DescribeTable("returns expected result for command paths",
			func(cmdPath string, expected bool) {
				Expect(needsConnection(cmdPath)).To(Equal(expected))
			},
			// Single-word local commands (no prefix stripping needed)
			Entry("version", "version", false),
			Entry("help", "help", false),
			Entry("completion", "completion", false),

			// Multi-word local commands (as returned by cmd.CommandPath())
			Entry("crosscodex config show", "crosscodex config show", false),
			Entry("crosscodex catalog validate", "crosscodex catalog validate", false),
			Entry("crosscodex prompt list", "crosscodex prompt list", false),
			Entry("crosscodex completion bash", "crosscodex completion bash", false),
			Entry("crosscodex project init", "crosscodex project init", false),
			// Remote commands (need daemon connection)
			Entry("crosscodex catalog import", "crosscodex catalog import", true),
			Entry("crosscodex catalog list", "crosscodex catalog list", true),
			Entry("crosscodex run start", "crosscodex run start", true),
			Entry("crosscodex results summary", "crosscodex results summary", true),

			// Unknown commands return true
			Entry("nonexistent", "nonexistent", true),
			Entry("crosscodex unknown", "crosscodex unknown", true),
		)
	})

	Describe("resolveEndpoint", func() {
		It("prefers explicit flag value", func() {
			endpoint := resolveEndpoint("flag-value:9090", "env-value:9090", "config-value:9090")
			Expect(endpoint).To(Equal("flag-value:9090"))
		})

		It("falls back to env when flag is empty", func() {
			endpoint := resolveEndpoint("", "env-value:9090", "config-value:9090")
			Expect(endpoint).To(Equal("env-value:9090"))
		})

		It("falls back to config when flag and env are empty", func() {
			endpoint := resolveEndpoint("", "", "config-value:9090")
			Expect(endpoint).To(Equal("config-value:9090"))
		})

		It("returns default when all are empty", func() {
			endpoint := resolveEndpoint("", "", "")
			Expect(endpoint).To(Equal("localhost:50051"))
		})
	})
})
