package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/spf13/cobra"
)

func newResultsCmd(state *cliState) *cobra.Command {
	resultsCmd := &cobra.Command{
		Use:   "results",
		Short: "View, export, and verify analysis results",
		RunE:  func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}

	resultsCmd.AddCommand(newResultsSummaryCmd(state))
	resultsCmd.AddCommand(newResultsExportCmd(state))
	resultsCmd.AddCommand(newResultsQueryCmd(state))
	resultsCmd.AddCommand(newResultsDebugCmd(state))
	resultsCmd.AddCommand(newResultsReviewCmd(state))
	resultsCmd.AddCommand(newResultsVerifyCmd(state))

	return resultsCmd
}

func newResultsSummaryCmd(state *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "summary <job-id>",
		Aliases: []string{"sum"},
		Short:   "Show mapping relationship counts by type",
		Long: `Show mapping relationship counts by type.

Note: job-id is accepted for display labeling but the current API
returns all mappings. Job-scoped filtering will be added when the
proto supports it.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			jobID := args[0]

			req := &pb.GetControlMappingsRequest{}

			resp, err := state.client.GetControlMappings(cmd.Context(), req)
			if err != nil {
				return fmt.Errorf("failed to get control mappings: %w", err)
			}

			counts := make(map[string]int)
			for _, m := range resp.GetMappings() {
				relType := m.GetRelationshipType().String()
				counts[relType]++
			}

			return emit(cmd,
				func(w io.Writer, color bool) {
					fmt.Fprintf(w, "Summary for job %s\n\n", jobID)
					fmt.Fprintf(w, "Total mappings: %d\n\n", len(resp.GetMappings()))
					fmt.Fprintf(w, "By relationship type:\n")
					for relType, count := range counts {
						fmt.Fprintf(w, "  %-20s %d\n", relType, count)
					}
				},
				map[string]any{
					"job_id":        jobID,
					"total":         len(resp.GetMappings()),
					"by_type":       counts,
					"mapping_count": len(resp.GetMappings()),
				},
			)
		},
	}

	guidedArgs(cmd, cobra.ExactArgs(1), argGuide{
		noun:  "a job-id",
		find:  "crosscodex results summary <job-id>",
		usage: "crosscodex results summary <job-id>",
		examples: []string{
			"crosscodex results summary abc123",
			"crosscodex results sum abc123",
		},
	})

	return cmd
}

func newResultsExportCmd(state *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "export <job-id>",
		Aliases: []string{"dump"},
		Short:   "Export mappings to JSON or CSV",
		Long: `Export control mappings to JSON or CSV format.

Note: job-id is accepted for display labeling but the current API
returns all mappings. Job-scoped filtering will be added when the
proto supports it.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			jobID := args[0]
			format, _ := cmd.Flags().GetString("format")
			outputFile, _ := cmd.Flags().GetString("output")

			req := &pb.GetControlMappingsRequest{}

			resp, err := state.client.GetControlMappings(cmd.Context(), req)
			if err != nil {
				return fmt.Errorf("failed to get control mappings: %w", err)
			}

			var out io.Writer
			if outputFile != "" {
				f, err := os.Create(outputFile)
				if err != nil {
					return fmt.Errorf("failed to create output file: %w", err)
				}
				defer f.Close()
				out = f
			} else {
				out = cmd.OutOrStdout()
			}

			switch format {
			case "json":
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]any{
					"job_id":   jobID,
					"mappings": resp.GetMappings(),
				})
			case "csv":
				w := csv.NewWriter(out)
				defer w.Flush()

				if err := w.Write([]string{"mapping_id", "source_control_id", "target_control_id", "relationship_type", "confidence", "viability_score", "is_viable"}); err != nil {
					return fmt.Errorf("write CSV header: %w", err)
				}
				for _, m := range resp.GetMappings() {
					if err := w.Write([]string{
						m.GetMappingId(),
						m.GetSourceControlId(),
						m.GetTargetControlId(),
						m.GetRelationshipType().String(),
						fmt.Sprintf("%.3f", m.GetConfidence()),
						fmt.Sprintf("%.3f", m.GetViabilityScore()),
						fmt.Sprintf("%t", m.GetIsViable()),
					}); err != nil {
						return fmt.Errorf("write CSV row: %w", err)
					}
				}
				w.Flush()
				if err := w.Error(); err != nil {
					return fmt.Errorf("write CSV: %w", err)
				}
				return nil
			default:
				return fmt.Errorf("unsupported format: %s", format)
			}
		},
	}

	guidedArgs(cmd, cobra.ExactArgs(1), argGuide{
		noun:  "a job-id",
		find:  "crosscodex results export <job-id>",
		usage: "crosscodex results export <job-id> [--format json|csv] [--output file]",
		examples: []string{
			"crosscodex results export abc123",
			"crosscodex results export abc123 --format csv --output mappings.csv",
		},
	})

	cmd.Flags().String("format", "json", "output format (json or csv)")
	cmd.Flags().String("output", "", "output file path (default: stdout)")

	return cmd
}

func newResultsQueryCmd(state *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "query <cypher>",
		Short: "Execute a Cypher query against the graph",
		Long:  `Execute a Cypher query against the graph database.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cypher := args[0]

			req := &pb.QueryGraphRequest{
				Cypher: cypher,
			}

			resp, err := state.client.QueryGraph(cmd.Context(), req)
			if err != nil {
				return fmt.Errorf("failed to query graph: %w", err)
			}

			return emit(cmd,
				func(w io.Writer, color bool) {
					if resp.GetResponse() != nil {
						fmt.Fprintln(w, "Query results:")
						fmt.Fprintln(w, resp.GetResponse())
					}
				},
				map[string]any{
					"cypher":  cypher,
					"results": resp.GetResponse(),
				},
			)
		},
	}

	guidedArgs(cmd, cobra.ExactArgs(1), argGuide{
		noun:  "a cypher query",
		find:  "crosscodex results query <cypher>",
		usage: "crosscodex results query <cypher>",
		examples: []string{
			`crosscodex results query "MATCH (c:Control) RETURN c LIMIT 10"`,
			`crosscodex results query "MATCH (a:Control)-[r:EQUIVALENT]->(b:Control) RETURN a, r, b"`,
		},
	})

	return cmd
}

func newResultsDebugCmd(state *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "debug <job-id> <control-id>",
		Short: "Show detailed analyzer contributions for a mapping",
		Long:  `Show detailed analyzer contributions for a specific control mapping.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			jobID := args[0]
			controlID := args[1]

			req := &pb.GetControlMappingsRequest{
				ControlId: controlID,
			}

			resp, err := state.client.GetControlMappings(cmd.Context(), req)
			if err != nil {
				return fmt.Errorf("failed to get control mappings: %w", err)
			}

			return emit(cmd,
				func(w io.Writer, color bool) {
					fmt.Fprintf(w, "Debug for job %s, control %s\n\n", jobID, controlID)
					fmt.Fprintf(w, "Found %d mapping(s)\n\n", len(resp.GetMappings()))
					for _, m := range resp.GetMappings() {
						fmt.Fprintf(w, "Mapping: %s -> %s (%s)\n", m.GetSourceControlId(), m.GetTargetControlId(), m.GetRelationshipType())
						fmt.Fprintf(w, "  Confidence: %.3f, Viability: %.3f, Viable: %t\n", m.GetConfidence(), m.GetViabilityScore(), m.GetIsViable())
						if len(m.GetContributions()) > 0 {
							fmt.Fprintf(w, "  Contributions:\n")
							for _, c := range m.GetContributions() {
								fmt.Fprintf(w, "    - %s\n", c)
							}
						}
						fmt.Fprintln(w)
					}
				},
				map[string]any{
					"job_id":     jobID,
					"control_id": controlID,
					"mappings":   resp.GetMappings(),
				},
			)
		},
	}

	guidedArgs(cmd, cobra.ExactArgs(2), argGuide{
		noun:  "a job-id and control-id",
		find:  "crosscodex results debug <job-id> <control-id>",
		usage: "crosscodex results debug <job-id> <control-id>",
		examples: []string{
			"crosscodex results debug abc123 ctrl-001",
		},
	})

	return cmd
}

func newResultsReviewCmd(state *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "review <job-id>",
		Short: "Show contested mappings in the review queue",
		Long: `Show contested mappings in the review queue for human review.

Note: job-id is accepted for display labeling but the current API
returns the full queue. Job-scoped filtering will be added when the
proto supports it.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			jobID := args[0]

			req := &pb.GetReviewQueueRequest{}

			resp, err := state.client.GetReviewQueue(cmd.Context(), req)
			if err != nil {
				return fmt.Errorf("failed to get review queue: %w", err)
			}

			return emit(cmd,
				func(w io.Writer, color bool) {
					fmt.Fprintf(w, "Review queue for job %s\n\n", jobID)
					fmt.Fprintf(w, "Items in queue: %d\n\n", len(resp.GetItems()))
					for i, item := range resp.GetItems() {
						fmt.Fprintf(w, "%d. %s\n", i+1, item)
					}
				},
				map[string]any{
					"job_id": jobID,
					"items":  resp.GetItems(),
				},
			)
		},
	}

	guidedArgs(cmd, cobra.ExactArgs(1), argGuide{
		noun:  "a job-id",
		find:  "crosscodex results review <job-id>",
		usage: "crosscodex results review <job-id>",
		examples: []string{
			"crosscodex results review abc123",
		},
	})

	return cmd
}

func newResultsVerifyCmd(_ *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "verify <bundle-file>",
		Short: "Verify attestation bundle signatures",
		Long:  `Verify attestation bundle signatures using in-toto verification.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			bundleFile := args[0]
			publicKeyPath, _ := cmd.Flags().GetString("public-key")

			bundleData, err := os.ReadFile(bundleFile)
			if err != nil {
				return fmt.Errorf("failed to read bundle file: %w", err)
			}

			if publicKeyPath == "" {
				var bundle map[string]any
				if err := json.Unmarshal(bundleData, &bundle); err != nil {
					return fmt.Errorf("invalid attestation bundle (not valid JSON): %w", err)
				}

				return emit(cmd,
					func(w io.Writer, color bool) {
						fmt.Fprintf(w, "Bundle file: %s\n", bundleFile)
						fmt.Fprintf(w, "Status: Structure valid (JSON parsed successfully)\n")
						fmt.Fprintf(w, "\nFull verification requires --public-key flag\n")
					},
					map[string]any{
						"bundle_file": bundleFile,
						"status":      "structure_valid",
						"note":        "full verification requires public key",
					},
				)
			}

			kp := &attestation.FileKeyProvider{
				PublicKeyPath: publicKeyPath,
			}

			gen, err := attestation.NewGenerator(kp)
			if err != nil {
				return fmt.Errorf("failed to create attestation generator: %w", err)
			}

			verifiedLink, err := gen.Verify(cmd.Context(), bundleData)
			if err != nil {
				return fmt.Errorf("verification failed: %w", err)
			}

			return emit(cmd,
				func(w io.Writer, color bool) {
					fmt.Fprintf(w, "Bundle file: %s\n", bundleFile)
					fmt.Fprintf(w, "Status: Verified\n")
					fmt.Fprintf(w, "Step: %s\n", verifiedLink.Step)
				},
				map[string]any{
					"bundle_file": bundleFile,
					"status":      "verified",
					"step":        verifiedLink.Step,
				},
			)
		},
	}

	guidedArgs(cmd, cobra.ExactArgs(1), argGuide{
		noun:  "a bundle file path",
		find:  "crosscodex results verify <bundle-file>",
		usage: "crosscodex results verify <bundle-file> [--public-key <path>]",
		examples: []string{
			"crosscodex results verify attestation-bundle.json --public-key public.pem",
			"crosscodex results verify bundle.json",
		},
	})

	cmd.Flags().String("public-key", "", "path to public key PEM file for verification")

	return cmd
}
