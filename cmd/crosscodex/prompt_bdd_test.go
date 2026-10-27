package main

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Prompt Commands", func() {
	Describe("prompt list", func() {
		It("runs without error when no prompts are available", func() {
			stdout, _, err := execute(newRootCmd(), "prompt", "list")
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(Or(
				ContainSubstring("No prompt templates"),
				ContainSubstring("Available prompts"),
			))
		})

		It("supports JSON output mode", func() {
			_, _, err := execute(newRootCmd(), "prompt", "list", "--json")
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("prompt show", func() {
		It("requires a prompt name argument", func() {
			_, _, err := execute(newRootCmd(), "prompt", "show")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("requires"))
		})

		It("accepts a prompt name and fails with resolution error, not arg error", func() {
			_, _, err := execute(newRootCmd(), "prompt", "show", "test-prompt")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).NotTo(ContainSubstring("requires"))
			Expect(err.Error()).To(ContainSubstring("prompt"))
		})
	})

	Describe("prompt layer", func() {
		It("runs without error", func() {
			stdout, _, err := execute(newRootCmd(), "prompt", "layer")
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(Or(
				ContainSubstring("No prompt layers"),
				ContainSubstring("Prompt layer stack"),
			))
		})

		It("supports JSON output mode", func() {
			_, _, err := execute(newRootCmd(), "prompt", "layer", "--json")
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
