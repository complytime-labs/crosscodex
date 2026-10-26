package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"

	connectrpc "connectrpc.com/connect"
	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
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
		Long:    `Show the number of mappings of each relationship type produced by a job.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			jobID := args[0]

			req, err := mappingRequestFromFlags(cmd, jobID)
			if err != nil {
				return err
			}
			mappings, err := listAllMappings(cmd.Context(), state.client, req)
			if err != nil {
				return err
			}

			counts := make(map[string]int)
			for _, m := range mappings {
				counts[m.GetRelationshipType().String()]++
			}
			// Canonical lowercase name, not the raw flag, which is case-insensitive.
			stage := strings.ToLower(strings.TrimPrefix(req.GetStage().String(), "MAPPING_STAGE_"))

			return emit(cmd,
				func(w io.Writer, color bool) {
					fmt.Fprintf(w, "Summary for job %s (%s mappings)\n\n", jobID, stage)
					fmt.Fprintf(w, "Total mappings: %d\n\n", len(mappings))
					fmt.Fprintf(w, "By relationship type:\n")
					for _, relType := range slices.Sorted(maps.Keys(counts)) {
						fmt.Fprintf(w, "  %-20s %d\n", relType, counts[relType])
					}
				},
				map[string]any{
					"job_id":        jobID,
					"stage":         stage,
					"total":         len(mappings),
					"by_type":       counts,
					"mapping_count": len(mappings),
				},
			)
		},
	}

	guidedArgs(cmd, cobra.ExactArgs(1), argGuide{
		noun:  "a job-id",
		find:  "crosscodex results summary <job-id>",
		usage: "crosscodex results summary <job-id> [--stage synthesized|analyzer] [--type <type>]... [--control <id>] [--min-confidence <0-1>] [--include-non-viable]",
		examples: []string{
			"crosscodex results summary abc123",
			"crosscodex results sum abc123 --stage analyzer --type equivalent",
		},
	})
	addMappingFlags(cmd)

	return cmd
}

// mappingStages maps the --stage flag values to the API enum.
var mappingStages = map[string]pb.MappingStage{
	"synthesized": pb.MappingStage_MAPPING_STAGE_SYNTHESIZED,
	"analyzer":    pb.MappingStage_MAPPING_STAGE_ANALYZER,
}

// mappingPageSize is the server's maximum page size; the CLI always reads
// whole result sets, so it asks for the largest pages allowed.
const mappingPageSize = 1000

const relationshipTypePrefix = "RELATIONSHIP_TYPE_"

// addMappingFlags registers the mapping filter flags shared by summary and
// export.
func addMappingFlags(cmd *cobra.Command) {
	cmd.Flags().String("stage", "synthesized", "mapping stage to read: synthesized or analyzer")
	cmd.Flags().Float64("min-confidence", 0, "only mappings with confidence >= this value (0 to 1)")
	cmd.Flags().StringSlice("type", nil, "only these relationship types (repeatable, case-insensitive)")
	cmd.Flags().String("control", "", "only mappings whose source or target is this control id")
	cmd.Flags().Bool("include-non-viable", false, "include pairs synthesis filtered out (synthesized stage only)")
}

// mappingRequestFromFlags validates the shared mapping flags and builds the
// request, so mistakes surface here with guidance before any network call.
func mappingRequestFromFlags(cmd *cobra.Command, jobID string) (*pb.ListJobMappingsRequest, error) {
	stageName, _ := cmd.Flags().GetString("stage")
	stage, ok := mappingStages[strings.ToLower(stageName)]
	if !ok {
		return nil, fmt.Errorf("invalid --stage %q: use one of synthesized, analyzer", stageName)
	}

	minConfidence, _ := cmd.Flags().GetFloat64("min-confidence")
	if !(minConfidence >= 0 && minConfidence <= 1) {
		return nil, fmt.Errorf("invalid --min-confidence %v: use a value between 0 and 1", minConfidence)
	}

	includeNonViable, _ := cmd.Flags().GetBool("include-non-viable")
	if includeNonViable && stage != pb.MappingStage_MAPPING_STAGE_SYNTHESIZED {
		return nil, fmt.Errorf("--include-non-viable only applies to the synthesized stage: analyzer mappings have no viability, so drop the flag or use --stage synthesized")
	}

	typeNames, _ := cmd.Flags().GetStringSlice("type")
	types := make([]pb.RelationshipType, 0, len(typeNames))
	for _, name := range typeNames {
		t, err := parseRelationshipType(name)
		if err != nil {
			return nil, err
		}
		types = append(types, t)
	}

	controlID, _ := cmd.Flags().GetString("control")
	return &pb.ListJobMappingsRequest{
		JobId:             jobID,
		Stage:             stage,
		MinConfidence:     float32(minConfidence),
		IncludeNonViable:  includeNonViable,
		RelationshipTypes: types,
		ControlId:         controlID,
	}, nil
}

// parseRelationshipType accepts a relationship type with or without the
// RELATIONSHIP_TYPE_ prefix, in any case. UNSPECIFIED is not a filter.
func parseRelationshipType(name string) (pb.RelationshipType, error) {
	full := strings.ToUpper(strings.TrimSpace(name))
	if !strings.HasPrefix(full, relationshipTypePrefix) {
		full = relationshipTypePrefix + full
	}
	if v, ok := pb.RelationshipType_value[full]; ok && v != int32(pb.RelationshipType_RELATIONSHIP_TYPE_UNSPECIFIED) {
		return pb.RelationshipType(v), nil
	}

	valid := make([]string, 0, len(pb.RelationshipType_name))
	for v, n := range pb.RelationshipType_name {
		if v != int32(pb.RelationshipType_RELATIONSHIP_TYPE_UNSPECIFIED) {
			valid = append(valid, strings.ToLower(strings.TrimPrefix(n, relationshipTypePrefix)))
		}
	}
	slices.Sort(valid)
	return 0, fmt.Errorf("invalid --type %q: use one of %s", name, strings.Join(valid, ", "))
}

// listAllMappings reads every page of the request's result set. It mutates
// req.Pagination to walk the page tokens.
func listAllMappings(ctx context.Context, client crosscodexv1connect.GatewayServiceClient, req *pb.ListJobMappingsRequest) ([]*pb.ControlMapping, error) {
	var all []*pb.ControlMapping
	pageToken := ""
	seen := map[string]bool{}
	for {
		req.Pagination = &pb.Pagination{PageSize: mappingPageSize, PageToken: pageToken}
		resp, err := client.ListJobMappings(ctx, connectrpc.NewRequest(req))
		if err != nil {
			return nil, fmt.Errorf("list mappings for job %s: %w", req.GetJobId(), err)
		}
		all = append(all, resp.Msg.GetMappings()...)
		seen[pageToken] = true
		pageToken = resp.Msg.GetPageInfo().GetNextPageToken()
		if pageToken == "" {
			return all, nil
		}
		if seen[pageToken] {
			return nil, fmt.Errorf("list mappings for job %s: server returned page token %q again; stopping to avoid an endless loop. Report this as a crosscodexd bug", req.GetJobId(), pageToken)
		}
	}
}

// exportFormats lists the --format values export accepts.
var exportFormats = []string{"json", "csv"}

func newResultsExportCmd(state *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "export <job-id>",
		Aliases: []string{"dump"},
		Short:   "Export mappings to JSON or CSV",
		Long:    `Export a job's control mappings to JSON or CSV format.`,
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			jobID := args[0]
			format, _ := cmd.Flags().GetString("format")
			outputFile, _ := cmd.Flags().GetString("output")

			if !slices.Contains(exportFormats, format) {
				return fmt.Errorf("invalid --format %q: use one of %s", format, strings.Join(exportFormats, ", "))
			}

			req, err := mappingRequestFromFlags(cmd, jobID)
			if err != nil {
				return err
			}
			mappings, err := listAllMappings(cmd.Context(), state.client, req)
			if err != nil {
				return err
			}

			var out io.Writer
			if outputFile != "" {
				f, createErr := os.Create(outputFile)
				if createErr != nil {
					return fmt.Errorf("failed to create output file: %w", createErr)
				}
				defer func() {
					if closeErr := f.Close(); closeErr != nil {
						err = errors.Join(err, fmt.Errorf("close output file %s: %w", outputFile, closeErr))
					}
				}()
				out = f
			} else {
				out = cmd.OutOrStdout()
			}

			switch format {
			case "json":
				// encoding/json would print enums as numbers and drop false and
				// zero fields; protojson with EmitUnpopulated keeps every field
				// and the enum names.
				raw := make([]json.RawMessage, len(mappings))
				for i, m := range mappings {
					b, err := protojson.MarshalOptions{EmitUnpopulated: true}.Marshal(m)
					if err != nil {
						return fmt.Errorf("encode mapping %s as JSON: %w", m.GetMappingId(), err)
					}
					raw[i] = b
				}
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]any{
					"job_id":   jobID,
					"mappings": raw,
				})
			case "csv":
				w := csv.NewWriter(out)
				defer w.Flush()

				if err := w.Write([]string{"mapping_id", "source_control_id", "target_control_id", "relationship_type", "confidence", "viability_score", "is_viable"}); err != nil {
					return fmt.Errorf("write CSV header: %w", err)
				}
				for _, m := range mappings {
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
				return fmt.Errorf("unsupported format %q: use one of %s", format, strings.Join(exportFormats, ", "))
			}
		},
	}

	guidedArgs(cmd, cobra.ExactArgs(1), argGuide{
		noun:  "a job-id",
		find:  "crosscodex results export <job-id>",
		usage: "crosscodex results export <job-id> [--format json|csv] [--output file] [--stage synthesized|analyzer] [--type <type>]... [--control <id>] [--min-confidence <0-1>] [--include-non-viable]",
		examples: []string{
			"crosscodex results export abc123",
			"crosscodex results export abc123 --format csv --output mappings.csv",
		},
	})

	cmd.Flags().String("format", "json", "output format (json or csv)")
	cmd.Flags().String("output", "", "output file path (default: stdout)")
	addMappingFlags(cmd)

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

			resp, err := state.client.QueryGraph(cmd.Context(), connectrpc.NewRequest(req))
			if err != nil {
				return fmt.Errorf("failed to query graph: %w", err)
			}

			return emit(cmd,
				func(w io.Writer, color bool) {
					if resp.Msg.GetResponse() != nil {
						fmt.Fprintln(w, "Query results:")
						fmt.Fprintln(w, resp.Msg.GetResponse())
					}
				},
				map[string]any{
					"cypher":  cypher,
					"results": resp.Msg.GetResponse(),
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

			req := &pb.ListJobMappingsRequest{
				JobId:     jobID,
				ControlId: controlID,
				Stage:     pb.MappingStage_MAPPING_STAGE_SYNTHESIZED,
			}
			mappings, err := listAllMappings(cmd.Context(), state.client, req)
			if err != nil {
				return err
			}

			return emit(cmd,
				func(w io.Writer, color bool) {
					fmt.Fprintf(w, "Debug for job %s, control %s\n\n", jobID, controlID)
					fmt.Fprintf(w, "Found %d mapping(s)\n\n", len(mappings))
					for _, m := range mappings {
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
					"mappings":   mappings,
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

			resp, err := state.client.GetReviewQueue(cmd.Context(), connectrpc.NewRequest(req))
			if err != nil {
				return fmt.Errorf("failed to get review queue: %w", err)
			}

			return emit(cmd,
				func(w io.Writer, color bool) {
					fmt.Fprintf(w, "Review queue for job %s\n\n", jobID)
					fmt.Fprintf(w, "Items in queue: %d\n\n", len(resp.Msg.GetItems()))
					for i, item := range resp.Msg.GetItems() {
						fmt.Fprintf(w, "%d. %s\n", i+1, item)
					}
				},
				map[string]any{
					"job_id": jobID,
					"items":  resp.Msg.GetItems(),
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
