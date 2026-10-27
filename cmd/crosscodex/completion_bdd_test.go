package main

import (
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/types"
)

var _ = Describe("Completion Command", func() {
	Describe("completion command structure", func() {
		It("creates a completion command with the correct name", func() {
			cmd := newCompletionCmd()
			Expect(cmd.Use).To(Equal("completion"))
		})

		It("has bash subcommand", func() {
			cmd := newCompletionCmd()
			bashCmd, _, err := cmd.Find([]string{"bash"})
			Expect(err).NotTo(HaveOccurred())
			Expect(bashCmd.Name()).To(Equal("bash"))
		})

		It("has zsh subcommand", func() {
			cmd := newCompletionCmd()
			zshCmd, _, err := cmd.Find([]string{"zsh"})
			Expect(err).NotTo(HaveOccurred())
			Expect(zshCmd.Name()).To(Equal("zsh"))
		})

		It("has fish subcommand", func() {
			cmd := newCompletionCmd()
			fishCmd, _, err := cmd.Find([]string{"fish"})
			Expect(err).NotTo(HaveOccurred())
			Expect(fishCmd.Name()).To(Equal("fish"))
		})

		It("has powershell subcommand", func() {
			cmd := newCompletionCmd()
			psCmd, _, err := cmd.Find([]string{"powershell"})
			Expect(err).NotTo(HaveOccurred())
			Expect(psCmd.Name()).To(Equal("powershell"))
		})
	})

	DescribeTable("produces a completion script",
		func(shell string, wantScript types.GomegaMatcher) {
			stdout, _, err := execute(newRootCmd(), "completion", shell)
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).NotTo(BeEmpty())
			Expect(stdout).To(wantScript)
		},
		Entry("bash", "bash", ContainSubstring("bash")),
		Entry("zsh", "zsh", Or(ContainSubstring("zsh"), ContainSubstring("compdef"))),
		Entry("fish", "fish", ContainSubstring("fish")),
		Entry("powershell", "powershell", Or(ContainSubstring("PowerShell"), ContainSubstring("param"))),
	)

	DescribeTable("has an --install flag",
		func(shell string) {
			cmd := newCompletionCmd()
			shellCmd, _, err := cmd.Find([]string{shell})
			Expect(err).NotTo(HaveOccurred())
			Expect(shellCmd.Flags().Lookup("install")).NotTo(BeNil())
		},
		Entry("bash", "bash"),
		Entry("zsh", "zsh"),
		Entry("fish", "fish"),
	)

	// Fish resolves its target from XDG_CONFIG_HOME; bash and zsh ignore it.
	DescribeTable("installs the script into the shell's standard location",
		func(shell string, installedRelPath string) {
			tmpHome := GinkgoT().TempDir()
			GinkgoT().Setenv("HOME", tmpHome)
			GinkgoT().Setenv("XDG_CONFIG_HOME", filepath.Join(tmpHome, ".config"))

			_, _, err := execute(newRootCmd(), "completion", shell, "--install")
			Expect(err).NotTo(HaveOccurred())

			Expect(filepath.Join(tmpHome, installedRelPath)).To(BeAnExistingFile())
		},
		Entry("bash", "bash", filepath.Join(".bash_completion.d", "crosscodex")),
		Entry("zsh", "zsh", filepath.Join(".zfunc", "_crosscodex")),
		Entry("fish", "fish", filepath.Join(".config", "fish", "completions", "crosscodex.fish")),
	)
})
