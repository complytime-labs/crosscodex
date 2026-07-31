package main

import (
	"fmt"
	"os"

	"github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/complytime-labs/crosscodex/internal/version"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/spf13/cobra"
)

type cliState struct {
	cfg     *config.ClientConfig
	fullCfg *config.Config
	client  crosscodexv1connect.GatewayServiceClient
	daemon  *embeddedDaemon
}

func newRootCmd() *cobra.Command {
	cobra.EnableCommandSorting = false

	state := &cliState{}

	root := &cobra.Command{
		Use:   "crosscodex",
		Short: "CrossCodex — compliance mapping with AI-assisted analysis",
		Long: `CrossCodex maps relationships between compliance standards using
AI-assisted analysis. Import catalogs, run analysis jobs, and
explore the results.

Get started:
  crosscodex project init          Initialize a new project
  crosscodex catalog import        Import a compliance catalog
  crosscodex run start             Start an analysis job
  crosscodex results summary       View analysis results`,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			profile, _ := cmd.Flags().GetString("profile")
			if err := loadConfig(state, profile); err != nil {
				return err
			}

			if needsConnection(cmd.CommandPath()) {
				endpoint, _ := cmd.Flags().GetString("endpoint")
				if err := connect(cmd.Context(), state, endpoint); err != nil {
					return err
				}
			}
			return nil
		},
		PersistentPostRun: func(cmd *cobra.Command, args []string) {
			disconnect(state)
		},
	}

	root.Version = version.Version
	root.SetVersionTemplate("crosscodex {{.Version}}\n")
	root.CompletionOptions.DisableDefaultCmd = true

	root.PersistentFlags().String("endpoint", "", "crosscodexd gRPC address (default: localhost:50051)")
	root.PersistentFlags().Bool("json", false, "output as JSON")
	root.PersistentFlags().Bool("plain", false, "output without formatting or color")
	root.PersistentFlags().Bool("no-color", false, "disable color output")
	root.PersistentFlags().String("profile", "", "configuration profile name")

	root.MarkFlagsMutuallyExclusive("json", "plain")

	root.AddGroup(
		&cobra.Group{ID: "project", Title: "Project Commands:"},
		&cobra.Group{ID: "analysis", Title: "Analysis Commands:"},
		&cobra.Group{ID: "prompt", Title: "Prompt Commands:"},
		&cobra.Group{ID: "connection", Title: "Connection Commands:"},
		&cobra.Group{ID: "additional", Title: "Additional Commands:"},
	)

	addTo := func(group string, c *cobra.Command) {
		c.GroupID = group
		root.AddCommand(c)
	}

	addTo("project", newProjectCmd(state))
	addTo("project", newConfigCmd(state))
	addTo("analysis", newCatalogCmd(state))
	addTo("analysis", newRunCmd(state))
	addTo("analysis", newResultsCmd(state))
	addTo("prompt", newPromptCmd(state))
	addTo("additional", newVersionCmd(state))
	addTo("additional", newCompletionCmd())

	root.SetHelpCommandGroupID("additional")

	return root
}

func main() {
	root := newRootCmd()
	if err := root.Execute(); err != nil {
		isJSON := jsonMode(root)
		fmt.Fprintln(os.Stderr, formatCLIError(err, isJSON))
		os.Exit(1)
	}
}
