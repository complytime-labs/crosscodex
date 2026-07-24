package main

import (
	"bytes"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Prompt Commands", func() {
	var (
		stdout bytes.Buffer
		stderr bytes.Buffer
	)

	BeforeEach(func() {
		stdout.Reset()
		stderr.Reset()
	})

	Describe("prompt list", func() {
		It("runs without error when no prompts are available", func() {
			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"prompt", "list"})
			err := cmd.Execute()
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout.String()).To(Or(
				ContainSubstring("No prompt templates"),
				ContainSubstring("Available prompts"),
			))
		})

		It("supports JSON output mode", func() {
			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"prompt", "list", "--json"})
			err := cmd.Execute()
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("prompt show", func() {
		It("requires a prompt name argument", func() {
			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"prompt", "show"})
			err := cmd.Execute()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("arg"))
		})

		It("accepts a prompt name and fails with resolution error, not arg error", func() {
			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"prompt", "show", "test-prompt"})
			err := cmd.Execute()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).NotTo(ContainSubstring("requires"))
			Expect(err.Error()).To(ContainSubstring("prompt"))
		})
	})

	Describe("prompt layer", func() {
		It("runs without error", func() {
			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"prompt", "layer"})
			err := cmd.Execute()
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout.String()).To(Or(
				ContainSubstring("No prompt layers"),
				ContainSubstring("Prompt layer stack"),
			))
		})

		It("supports JSON output mode", func() {
			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"prompt", "layer", "--json"})
			err := cmd.Execute()
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
