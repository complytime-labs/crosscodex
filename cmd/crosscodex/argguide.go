package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

type argGuide struct {
	noun     string
	find     string
	usage    string
	examples []string
}

func guidedArgs(cmd *cobra.Command, validator cobra.PositionalArgs, guide argGuide) {
	if len(guide.examples) > 0 {
		cmd.Example = exampleBlock(guide.examples)
	}

	cmd.Args = func(cmd *cobra.Command, args []string) error {
		if err := validator(cmd, args); err != nil {
			return fmt.Errorf("'%s' requires %s\n\n  Usage: %s\n\n  Find available values:\n    %s\n\n  Examples:\n%s",
				cmd.CommandPath(), guide.noun, guide.usage, guide.find, exampleBlock(guide.examples))
		}
		return nil
	}
}

func exampleBlock(examples []string) string {
	var b strings.Builder
	for _, ex := range examples {
		b.WriteString("  ")
		b.WriteString(ex)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
