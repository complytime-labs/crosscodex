package main

import (
	"bytes"

	"github.com/spf13/cobra"
)

// execute runs cmd with args using fresh output buffers and returns what it
// wrote to stdout and stderr along with the Execute error.
func execute(cmd *cobra.Command, args ...string) (stdout, stderr string, err error) {
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return out.String(), errOut.String(), err
}
