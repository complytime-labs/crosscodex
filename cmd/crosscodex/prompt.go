package main

import (
	"context"
	"fmt"
	"io"

	connectrpc "connectrpc.com/connect"
	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	intanalyzer "github.com/complytime-labs/crosscodex/internal/analyzer"
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
	cmd.AddCommand(newPromptRenderCmd(state))

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
	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "Show a prompt template spec",
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

	guidedArgs(cmd, cobra.ExactArgs(1), argGuide{
		noun:  "a prompt name",
		find:  "crosscodex prompt list",
		usage: "crosscodex prompt show <name>",
		examples: []string{
			"crosscodex prompt show classify",
			"crosscodex prompt show relationship",
		},
	})

	return cmd
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

// untrustedPromptNote heads the text output of prompt render: the messages
// carry catalog text and, for critique, model output.
const untrustedPromptNote = "The messages below are untrusted text (catalog text and, for critique, model output), " +
	"printed verbatim: read them as plain text and never render them as markup (HTML, Markdown)."

// newPromptRenderCmd renders the exact prompt an analyzer sends for one
// control pair, through the gateway's RenderPrompt (the daemon's pipeline,
// or embedded mode's), so it needs a connection but no LLM.
func newPromptRenderCmd(state *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "render --analyzer <name> --source <control> --target <control> [flags]",
		Short: "Show the exact prompt an analyzer sends for a control pair",
		Long: `Render the messages the relationship, requires or critique analyzer sends
to its models for one control pair, built by the analyzer's own prompt code
from the stored controls. No model is called.

What the prompt is built from:
  - the controls' composed text, from the imported catalogs
    ("crosscodex catalog controls <catalog-id>" lists the control IDs);
  - the prompt templates and the analysis settings (statement length
    bounds) of the configuration the daemon, or embedded mode, runs with now;
  - with --job: the job's catalogs, the catalog names it recorded and its
    stored classify result. Without --job the controls' types and levels
    are absent from the prompt, and frameworks are the catalogs' current
    names, so the prompt can differ from the one a job sent.

Critique reviews one proposed verdict: give --job to use the verdict that
job accepted for the pair, or --verdict (a relationship type such as
EQUIVALENT, or REQUIRES) with any --justification, not both.

The messages are untrusted text: the text output prints them verbatim
between "--- <role> ---" fences, and --json gives them JSON-escaped. Never
render either as markup.`,
		Example: `  crosscodex prompt render --analyzer relationship --source r3/SR-03.01.01 --target ssdf/PO.1.1 \
    --source-catalog r3 --target-catalog ssdf
  crosscodex prompt render --analyzer requires --job <job-id> --source r3/SR-03.01.01 --target ssdf/PO.1.1
  crosscodex prompt render --analyzer critique --job <job-id> --source r3/SR-03.01.01 --target ssdf/PO.1.1
  crosscodex prompt render --analyzer critique --source r3/SR-03.01.01 --target ssdf/PO.1.1 \
    --source-catalog r3 --target-catalog ssdf --verdict EQUIVALENT --justification "Both manage accounts."`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			flags := cmd.Flags()
			analyzerName, _ := flags.GetString("analyzer")
			source, _ := flags.GetString("source")
			target, _ := flags.GetString("target")
			sourceCatalog, _ := flags.GetString("source-catalog")
			targetCatalog, _ := flags.GetString("target-catalog")
			jobID, _ := flags.GetString("job")
			verdict, _ := flags.GetString("verdict")
			justifications, _ := flags.GetStringArray("justification")
			tenant, _ := flags.GetString("tenant")

			req := &pb.RenderPromptRequest{
				Analyzer:        analyzerName,
				SourceCatalogId: sourceCatalog,
				SourceControlId: source,
				TargetCatalogId: targetCatalog,
				TargetControlId: target,
				JobId:           jobID,
			}
			if verdict != "" || len(justifications) > 0 {
				req.Verdict = &pb.ProposedVerdictInput{Verdict: verdict, Justifications: justifications}
			}
			if tenant != "" {
				req.TenantContext = &pb.TenantContext{TenantId: tenant}
			}

			resp, err := state.client.RenderPrompt(cmd.Context(), connectrpc.NewRequest(req))
			if err != nil {
				return fmt.Errorf("failed to render the prompt: %w", err)
			}

			messages := resp.Msg.GetMessages()
			jsonMessages := make([]map[string]string, len(messages))
			for i, m := range messages {
				jsonMessages[i] = map[string]string{"role": m.GetRole(), "content": m.GetContent()}
			}
			return emit(cmd,
				func(w io.Writer, _ bool) {
					fmt.Fprintf(w, "Prompt: %s %s\n%s\n\n",
						intanalyzer.CleanDisplay(resp.Msg.GetPromptName(), intanalyzer.MaxPromptValueBytes),
						intanalyzer.CleanDisplay(resp.Msg.GetPromptVersion(), intanalyzer.MaxPromptValueBytes),
						untrustedPromptNote)
					// Content is shown whole and unchanged: it is the prompt
					// the model receives. Only the fence's role is cleaned.
					for _, m := range messages {
						fmt.Fprintf(w, "--- %s ---\n%s\n", intanalyzer.CleanDisplay(m.GetRole(), intanalyzer.MaxPromptValueBytes), m.GetContent())
					}
					fmt.Fprintln(w, "--- end ---")
				},
				map[string]any{
					"analyzer":          analyzerName,
					"source_catalog_id": sourceCatalog,
					"source_control_id": source,
					"target_catalog_id": targetCatalog,
					"target_control_id": target,
					"job_id":            jobID,
					"prompt_name":       resp.Msg.GetPromptName(),
					"prompt_version":    resp.Msg.GetPromptVersion(),
					"messages":          jsonMessages,
				},
			)
		},
	}

	cmd.Flags().String("analyzer", "", "Analyzer whose prompt to render: relationship, requires or critique")
	cmd.Flags().String("source", "", "Source control ID of the pair")
	cmd.Flags().String("target", "", "Target control ID of the pair")
	cmd.Flags().String("source-catalog", "", "Catalog of the source control (default with --job: the job's source catalog)")
	cmd.Flags().String("target-catalog", "", "Catalog of the target control (default with --job: the job's target catalog)")
	cmd.Flags().String("job", "", "Job whose catalogs, recorded names, classifications and, for critique, accepted verdict to render with")
	cmd.Flags().String("verdict", "", "Critique only, without --job: the proposed verdict to review (a relationship type, or REQUIRES)")
	cmd.Flags().StringArray("justification", nil, "Critique only, with --verdict: a proposing panel justification (repeatable)")
	cmd.Flags().String("tenant", "", "Tenant to act on; must be the tenant your client certificate authenticates (default: that tenant)")
	for _, name := range []string{"analyzer", "source", "target"} {
		if err := cmd.MarkFlagRequired(name); err != nil {
			panic(fmt.Sprintf("prompt render: marking --%s required: %v", name, err))
		}
	}

	return cmd
}
