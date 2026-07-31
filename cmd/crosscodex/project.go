package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

func newProjectCmd(state *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "project",
		Short: "Manage CrossCodex projects and workspace configuration",
		RunE:  func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}

	cmd.AddCommand(newProjectInitCmd(state))
	cmd.AddCommand(newProjectListCmd(state))
	cmd.AddCommand(newProjectConfigCmd(state))
	cmd.AddCommand(newProjectStatusCmd(state))

	return cmd
}

func newProjectInitCmd(_ *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize a new CrossCodex project",
		Long: `Create a new .crosscodex directory with configuration template
and required subdirectories (prompts/).`,
		RunE: runProjectInit,
	}

	cmd.Flags().String("dir", ".", "directory to initialize")
	cmd.Flags().Bool("force", false, "overwrite existing .crosscodex directory")

	return cmd
}

func runProjectInit(cmd *cobra.Command, args []string) error {
	dir, _ := cmd.Flags().GetString("dir")
	force, _ := cmd.Flags().GetBool("force")

	absDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve directory: %w", err)
	}

	ccxDir := filepath.Join(absDir, ".crosscodex")

	if _, err := os.Stat(ccxDir); err == nil && !force {
		return fmt.Errorf(".crosscodex already exists at %s (use --force to overwrite)", absDir)
	}

	if force {
		if err := os.RemoveAll(ccxDir); err != nil {
			return fmt.Errorf("remove existing .crosscodex: %w", err)
		}
	}

	if err := os.MkdirAll(ccxDir, 0o755); err != nil {
		return fmt.Errorf("create .crosscodex: %w", err)
	}

	if err := os.MkdirAll(filepath.Join(ccxDir, "prompts"), 0o755); err != nil {
		return fmt.Errorf("create prompts directory: %w", err)
	}

	configPath := filepath.Join(ccxDir, "config.yaml")
	configTemplate := `# CrossCodex project configuration
#
# This file is merged with user and system configs according to the
# XDG Base Directory spec. See README.md for configuration details.

# Example: Override CLI output format
# cli:
#   output: json
#   no_color: false

# Example: Set project-specific LLM settings
# llm:
#   default_model: claude-opus-latest

# Example: Configure prompt template layers
# prompt:
#   layer_paths:
#     - ./custom-prompts
`

	if err := os.WriteFile(configPath, []byte(configTemplate), 0o644); err != nil {
		return fmt.Errorf("write config.yaml: %w", err)
	}

	return emit(cmd, func(w io.Writer, color bool) {
		fmt.Fprintf(w, "Initialized CrossCodex project in %s\n", absDir)
	}, map[string]string{
		"status":     "initialized",
		"path":       absDir,
		"config":     configPath,
		"prompt_dir": filepath.Join(ccxDir, "prompts"),
	})
}

func newProjectListCmd(_ *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List CrossCodex projects in current and parent directories",
		RunE:  runProjectList,
	}
	return cmd
}

func runProjectList(cmd *cobra.Command, args []string) error {
	wd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}

	projects := findProjects(wd)

	return emit(cmd, func(w io.Writer, color bool) {
		if len(projects) == 0 {
			fmt.Fprintln(w, "No CrossCodex projects found")
			return
		}
		fmt.Fprintf(w, "CrossCodex projects:\n")
		for _, p := range projects {
			fmt.Fprintf(w, "  %s\n", p)
		}
	}, map[string]any{
		"projects": projects,
	})
}

func findProjects(start string) []string {
	var projects []string
	seen := make(map[string]bool)

	dir := start
	for !seen[dir] {

		seen[dir] = true

		ccxPath := filepath.Join(dir, ".crosscodex")
		if info, err := os.Stat(ccxPath); err == nil && info.IsDir() {
			projects = append(projects, dir)
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	sort.Strings(projects)
	return projects
}

func newProjectConfigCmd(state *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Print resolved project configuration",
		Long: `Print the fully resolved configuration for the current project,
including all XDG config layers and environment overrides.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProjectConfig(cmd, state)
		},
	}
	return cmd
}

func runProjectConfig(cmd *cobra.Command, state *cliState) error {
	if state.cfg == nil {
		return fmt.Errorf("configuration not loaded")
	}

	return emit(cmd, func(w io.Writer, color bool) {
		enc := yaml.NewEncoder(w)
		enc.SetIndent(2)
		if err := enc.Encode(state.cfg); err != nil {
			fmt.Fprintf(w, "Error encoding config: %v\n", err)
		}
		enc.Close()
	}, state.cfg)
}

func newProjectStatusCmd(state *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Check daemon connection status",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProjectStatus(cmd, state)
		},
	}
	return cmd
}

func runProjectStatus(cmd *cobra.Command, state *cliState) error {
	endpoint, _ := cmd.Flags().GetString("endpoint")

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	var status string
	var healthy bool
	var connectedEndpoint string

	if err := connect(ctx, state, endpoint); err != nil {
		status = "disconnected"
		healthy = false
		connectedEndpoint = resolveEndpoint(endpoint, os.Getenv("CROSSCODEX_ENDPOINT"), "")
	} else {
		healthy = healthCheck(ctx, state.client)
		if healthy {
			status = "connected"
		} else {
			status = "unreachable"
		}
		if state.daemon != nil {
			connectedEndpoint = fmt.Sprintf("localhost:%d", state.daemon.port)
		} else {
			connectedEndpoint = resolveEndpoint(endpoint, os.Getenv("CROSSCODEX_ENDPOINT"), "")
		}
	}

	var mode string
	if state.daemon != nil {
		mode = "embedded"
	} else {
		mode = "external"
	}

	return emit(cmd, func(w io.Writer, color bool) {
		var statusSymbol string
		if healthy {
			statusSymbol = "✓"
		} else {
			statusSymbol = "✗"
		}
		fmt.Fprintf(w, "Daemon: %s %s\n", statusSymbol, status)
		if connectedEndpoint != "" {
			fmt.Fprintf(w, "Endpoint: %s\n", connectedEndpoint)
		}
		if healthy {
			fmt.Fprintf(w, "Mode: %s\n", mode)
		}
	}, map[string]any{
		"status":   status,
		"healthy":  healthy,
		"endpoint": connectedEndpoint,
		"mode":     mode,
	})
}
