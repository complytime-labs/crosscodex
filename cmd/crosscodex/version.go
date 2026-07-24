package main

import (
	"fmt"
	"io"

	"github.com/complytime-labs/crosscodex/internal/version"
	"github.com/spf13/cobra"
)

func newVersionCmd(_ *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "version",
		Short:   "Show CLI version information",
		Long:    `Show version information for the CrossCodex CLI.`,
		Aliases: []string{"ver"},
		RunE: func(cmd *cobra.Command, args []string) error {
			info := version.GetInfo()

			return emit(cmd, func(w io.Writer, color bool) {
				fmt.Fprintf(w, "crosscodex %s\n", info.Version)
				fmt.Fprintf(w, "  commit:  %s\n", info.GitCommit)
				fmt.Fprintf(w, "  built:   %s\n", info.BuildDate)
				fmt.Fprintf(w, "  go:      %s\n", info.GoVersion)
				fmt.Fprintf(w, "  os/arch: %s/%s\n", info.OS, info.Arch)
			}, map[string]any{
				"version":    info.Version,
				"commit":     info.GitCommit,
				"build_date": info.BuildDate,
				"go_version": info.GoVersion,
				"os":         info.OS,
				"arch":       info.Arch,
			})
		},
	}
	return cmd
}
