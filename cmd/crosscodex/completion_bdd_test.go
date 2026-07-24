package main

import (
	"bytes"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Completion Command", func() {
	var (
		stdout bytes.Buffer
		stderr bytes.Buffer
	)

	BeforeEach(func() {
		stdout.Reset()
		stderr.Reset()
	})

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

	Describe("bash completion", func() {
		It("produces bash completion script", func() {
			root := newRootCmd()
			cmd := newCompletionCmd()
			root.AddCommand(cmd)
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetArgs([]string{"completion", "bash"})
			err := root.Execute()
			Expect(err).NotTo(HaveOccurred())
			output := stdout.String()
			Expect(output).NotTo(BeEmpty())
			Expect(output).To(ContainSubstring("bash"))
		})

		It("has --install flag", func() {
			cmd := newCompletionCmd()
			bashCmd, _, err := cmd.Find([]string{"bash"})
			Expect(err).NotTo(HaveOccurred())
			Expect(bashCmd.Flags().Lookup("install")).NotTo(BeNil())
		})

		It("installs to standard bash completion directory", func() {
			tmpHome, err := os.MkdirTemp("", "crosscodex-bash-*")
			Expect(err).NotTo(HaveOccurred())
			defer os.RemoveAll(tmpHome)
			oldHome := os.Getenv("HOME")
			os.Setenv("HOME", tmpHome)
			defer os.Setenv("HOME", oldHome)

			root := newRootCmd()
			cmd := newCompletionCmd()
			root.AddCommand(cmd)
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetArgs([]string{"completion", "bash", "--install"})
			err = root.Execute()
			Expect(err).NotTo(HaveOccurred())

			bashDir := filepath.Join(tmpHome, ".bash_completion.d")
			bashFile := filepath.Join(bashDir, "crosscodex")
			Expect(bashFile).To(BeAnExistingFile())
		})
	})

	Describe("zsh completion", func() {
		It("produces zsh completion script", func() {
			root := newRootCmd()
			cmd := newCompletionCmd()
			root.AddCommand(cmd)
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetArgs([]string{"completion", "zsh"})
			err := root.Execute()
			Expect(err).NotTo(HaveOccurred())
			output := stdout.String()
			Expect(output).NotTo(BeEmpty())
			Expect(output).To(Or(ContainSubstring("zsh"), ContainSubstring("compdef")))
		})

		It("has --install flag", func() {
			cmd := newCompletionCmd()
			zshCmd, _, err := cmd.Find([]string{"zsh"})
			Expect(err).NotTo(HaveOccurred())
			Expect(zshCmd.Flags().Lookup("install")).NotTo(BeNil())
		})

		It("installs to zfunc directory", func() {
			tmpHome, err := os.MkdirTemp("", "crosscodex-zsh-*")
			Expect(err).NotTo(HaveOccurred())
			defer os.RemoveAll(tmpHome)
			oldHome := os.Getenv("HOME")
			os.Setenv("HOME", tmpHome)
			defer os.Setenv("HOME", oldHome)

			root := newRootCmd()
			cmd := newCompletionCmd()
			root.AddCommand(cmd)
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetArgs([]string{"completion", "zsh", "--install"})
			err = root.Execute()
			Expect(err).NotTo(HaveOccurred())

			zfuncFile := filepath.Join(tmpHome, ".zfunc", "_crosscodex")
			Expect(zfuncFile).To(BeAnExistingFile())
		})
	})

	Describe("fish completion", func() {
		It("produces fish completion script", func() {
			root := newRootCmd()
			cmd := newCompletionCmd()
			root.AddCommand(cmd)
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetArgs([]string{"completion", "fish"})
			err := root.Execute()
			Expect(err).NotTo(HaveOccurred())
			output := stdout.String()
			Expect(output).NotTo(BeEmpty())
			Expect(output).To(ContainSubstring("fish"))
		})

		It("has --install flag", func() {
			cmd := newCompletionCmd()
			fishCmd, _, err := cmd.Find([]string{"fish"})
			Expect(err).NotTo(HaveOccurred())
			Expect(fishCmd.Flags().Lookup("install")).NotTo(BeNil())
		})

		It("installs to fish completions directory", func() {
			tmpHome, err := os.MkdirTemp("", "crosscodex-fish-*")
			Expect(err).NotTo(HaveOccurred())
			defer os.RemoveAll(tmpHome)
			tmpConfig := filepath.Join(tmpHome, ".config")
			oldHome := os.Getenv("HOME")
			oldConfig := os.Getenv("XDG_CONFIG_HOME")
			os.Setenv("HOME", tmpHome)
			os.Setenv("XDG_CONFIG_HOME", tmpConfig)
			defer os.Setenv("HOME", oldHome)
			defer os.Setenv("XDG_CONFIG_HOME", oldConfig)

			root := newRootCmd()
			cmd := newCompletionCmd()
			root.AddCommand(cmd)
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetArgs([]string{"completion", "fish", "--install"})
			err = root.Execute()
			Expect(err).NotTo(HaveOccurred())

			fishFile := filepath.Join(tmpConfig, "fish", "completions", "crosscodex.fish")
			Expect(fishFile).To(BeAnExistingFile())
		})
	})

	Describe("powershell completion", func() {
		It("produces powershell completion script", func() {
			root := newRootCmd()
			cmd := newCompletionCmd()
			root.AddCommand(cmd)
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetArgs([]string{"completion", "powershell"})
			err := root.Execute()
			Expect(err).NotTo(HaveOccurred())
			output := stdout.String()
			Expect(output).NotTo(BeEmpty())
			Expect(output).To(Or(ContainSubstring("PowerShell"), ContainSubstring("param")))
		})
	})
})
