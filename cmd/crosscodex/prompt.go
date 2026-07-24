package main

import (
	"context"
	"fmt"
	"io"

	"github.com/complytime-labs/crosscodex/pkg/prompt"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

func newPromptCmd(state *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "prompt",
		Short: "Inspect and layer prompt templates",
		RunE:  func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}

	cmd.AddCommand(newPromptListCmd(state))
	cmd.AddCommand(newPromptShowCmd(state))
	cmd.AddCommand(newPromptLayerCmd(state))

	return cmd
}

func newPromptListCmd(state *cliState) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List available prompt templates",
		RunE: func(cmd *cobra.Command, args []string) error {
			if state.cfg == nil {
				return fmt.Errorf("configuration not loaded")
			}

			reg, err := prompt.NewRegistry(state.cfg.Prompt)
			if err != nil {
				return fmt.Errorf("creating prompt registry: %w", err)
			}

			names, err := reg.List(context.Background())
			if err != nil {
				return fmt.Errorf("listing prompts: %w", err)
			}

			return emit(cmd,
				func(w io.Writer, color bool) {
					if len(names) == 0 {
						fmt.Fprintln(w, "No prompt templates found.")
						return
					}
					fmt.Fprintf(w, "Available prompts (%d):\n\n", len(names))
					for _, name := range names {
						fmt.Fprintf(w, "  %s\n", name)
					}
				},
				map[string]any{
					"prompts": names,
					"count":   len(names),
				},
			)
		},
	}
}

func newPromptShowCmd(state *cliState) *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Show a prompt template spec",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if state.cfg == nil {
				return fmt.Errorf("configuration not loaded")
			}

			name := args[0]

			reg, err := prompt.NewRegistry(state.cfg.Prompt)
			if err != nil {
				return fmt.Errorf("creating prompt registry: %w", err)
			}

			spec, err := reg.Resolve(context.Background(), name)
			if err != nil {
				return fmt.Errorf("resolving prompt %q: %w", name, err)
			}

			return emit(cmd,
				func(w io.Writer, color bool) {
					enc := yaml.NewEncoder(w)
					enc.SetIndent(2)
					if err := enc.Encode(spec); err != nil {
						fmt.Fprintf(w, "Error encoding spec: %v\n", err)
					}
				},
				spec,
			)
		},
	}
}

func newPromptLayerCmd(state *cliState) *cobra.Command {
	return &cobra.Command{
		Use:   "layer",
		Short: "Show the prompt layer stack configuration",
		RunE: func(cmd *cobra.Command, args []string) error {
			if state.cfg == nil {
				return fmt.Errorf("configuration not loaded")
			}

			reg, err := prompt.NewRegistry(state.cfg.Prompt)
			if err != nil {
				return fmt.Errorf("creating prompt registry: %w", err)
			}

			// Get layer info for a probe prompt (empty name shows all layers)
			// Registry.Layers returns all layers regardless of whether they have
			// the named prompt
			layers, err := reg.Layers(context.Background(), "")
			if err != nil {
				return fmt.Errorf("listing layers: %w", err)
			}

			return emit(cmd,
				func(w io.Writer, color bool) {
					if len(layers) == 0 {
						fmt.Fprintln(w, "No prompt layers configured.")
						return
					}
					fmt.Fprintf(w, "Prompt layer stack (%d layers, highest precedence last):\n\n", len(layers))
					for i, layer := range layers {
						fmt.Fprintf(w, "%2d. %-12s  source: %-30s  merge: %-8s  slice: %s\n",
							i+1, layer.ID, layer.Source, layer.Merge, layer.SliceStrategy)
					}
				},
				map[string]any{
					"layers": layers,
					"count":  len(layers),
				},
			)
		},
	}
}
