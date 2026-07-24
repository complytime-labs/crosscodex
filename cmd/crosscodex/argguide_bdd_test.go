package main

import (
	"bytes"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/spf13/cobra"
)

var _ = Describe("Arg Guide", func() {
	Describe("guidedArgs", func() {
		It("sets Example from the guide", func() {
			cmd := &cobra.Command{Use: "import"}
			guidedArgs(cmd, cobra.ExactArgs(1), argGuide{
				noun:     "a file path",
				find:     "crosscodex catalog list",
				usage:    "crosscodex catalog import <file>",
				examples: []string{"crosscodex catalog import nist-800-53.json"},
			})
			Expect(cmd.Example).To(ContainSubstring("crosscodex catalog import nist-800-53.json"))
		})

		It("produces actionable error when args are wrong", func() {
			cmd := &cobra.Command{Use: "import"}
			var stderr bytes.Buffer
			cmd.SetErr(&stderr)
			guidedArgs(cmd, cobra.ExactArgs(1), argGuide{
				noun:     "a file path",
				find:     "crosscodex catalog list",
				usage:    "crosscodex catalog import <file>",
				examples: []string{"crosscodex catalog import nist-800-53.json"},
			})
			err := cmd.Args(cmd, []string{})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("a file path"))
			Expect(err.Error()).To(ContainSubstring("crosscodex catalog import <file>"))
		})
	})
})
