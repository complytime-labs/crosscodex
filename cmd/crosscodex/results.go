package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	connectrpc "connectrpc.com/connect"
	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	intanalyzer "github.com/complytime-labs/crosscodex/internal/analyzer"
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
	resultsCmd.AddCommand(newResultsCandidatesCmd(state))
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

			if includeVetoed, _ := cmd.Flags().GetBool("include-vetoed"); includeVetoed {
				return errSummaryIncludeVetoed
			}
			req, err := mappingRequestFromFlags(cmd, jobID)
			if err != nil {
				return err
			}
			mappings, _, err := listAllMappings(cmd.Context(), state.client, req)
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
					if len(mappings) == 0 {
						fmt.Fprintf(w, "\nNo mappings. A job produces mappings only if the relationship analyzer was "+
							"enabled on the daemon; see which analyzers the job ran with \"crosscodex run status %s\".\n", jobID)
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
	// Shared with export, where it lists vetoed pairs; summary refuses it
	// (errSummaryIncludeVetoed), so its help does not offer it.
	if err := cmd.Flags().MarkHidden("include-vetoed"); err != nil {
		panic(err)
	}

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
	cmd.Flags().Bool("include-vetoed", false, "include pairs the critique stage vetoed, with its votes and reasons (analyzer stage only)")
}

// errSummaryIncludeVetoed refuses --include-vetoed on summary: a vetoed pair
// is not a mapping, so counting it would overstate the job's mappings.
var errSummaryIncludeVetoed = errors.New("--include-vetoed lists vetoed pairs in `results export --stage analyzer`; summary counts only mappings, so drop the flag")

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

	includeVetoed, _ := cmd.Flags().GetBool("include-vetoed")
	if includeVetoed && stage != pb.MappingStage_MAPPING_STAGE_ANALYZER {
		return nil, fmt.Errorf("--include-vetoed only applies to the analyzer stage: synthesis never receives vetoed pairs, so add --stage analyzer or drop the flag")
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
		IncludeVetoed:     includeVetoed,
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

// listAllMappings reads every page of the request's result set and returns
// the mappings and the job's analysis, which the server sends on every page;
// the first page's is kept. It mutates req.Pagination to walk the page
// tokens.
func listAllMappings(ctx context.Context, client crosscodexv1connect.GatewayServiceClient, req *pb.ListJobMappingsRequest) ([]*pb.ControlMapping, *pb.JobAnalysis, error) {
	var all []*pb.ControlMapping
	var analysis *pb.JobAnalysis
	pageToken := ""
	seen := map[string]bool{}
	for {
		req.Pagination = &pb.Pagination{PageSize: mappingPageSize, PageToken: pageToken}
		resp, err := client.ListJobMappings(ctx, connectrpc.NewRequest(req))
		if err != nil {
			return nil, nil, fmt.Errorf("list mappings for job %s: %w", req.GetJobId(), err)
		}
		all = append(all, resp.Msg.GetMappings()...)
		if analysis == nil {
			analysis = resp.Msg.GetAnalysis()
		}
		seen[pageToken] = true
		pageToken = resp.Msg.GetPageInfo().GetNextPageToken()
		if pageToken == "" {
			return all, analysis, nil
		}
		if seen[pageToken] {
			return nil, nil, fmt.Errorf("list mappings for job %s: server returned page token %q again; stopping to avoid an endless loop. Report this as a crosscodexd bug", req.GetJobId(), pageToken)
		}
	}
}

// analysisColumns are the job-level CSV columns export appends to every row:
// the job's catalogs and direction (ListJobMappingsResponse.analysis), not
// the row's own.
var analysisColumns = []string{
	"job_source_catalog_id", "job_source_catalog_name", "job_source_catalog_version",
	"job_target_catalog_id", "job_target_catalog_name", "job_target_catalog_version", "job_bidirectional",
	"job_source_catalog_snapshot", "job_target_catalog_snapshot",
}

// analysisValues returns a's values for analysisColumns: all empty when the
// response carries no analysis. Catalog IDs, names and versions are
// uploader- or OSCAL-controlled, so each goes through csvCell.
func analysisValues(a *pb.JobAnalysis) []string {
	if a == nil {
		return make([]string, len(analysisColumns))
	}
	s, t := a.GetSourceCatalog(), a.GetTargetCatalog()
	return []string{
		csvCell(s.GetCatalogId()), csvCell(s.GetName()), csvCell(s.GetVersion()),
		csvCell(t.GetCatalogId()), csvCell(t.GetName()), csvCell(t.GetVersion()),
		strconv.FormatBool(a.GetBidirectional()),
		strconv.FormatBool(s.GetSnapshot()), strconv.FormatBool(t.GetSnapshot()),
	}
}

// critiqueCell renders a mapping's critique for the CSV critique column:
// "<vetoed|kept>: <u> upheld, <r> rejected, <f> failed; <reason> | ...", or ""
// for a mapping without one. The reasons are model output, so the cell goes
// through csvCellUpTo, uncapped: the server already bounds each reason.
func critiqueCell(c *pb.MappingCritique) string {
	if c == nil {
		return ""
	}
	verdict := "kept"
	if c.GetVetoed() {
		verdict = "vetoed"
	}
	cell := fmt.Sprintf("%s: %d upheld, %d rejected, %d failed", verdict, c.GetUpheld(), c.GetRejected(), c.GetFailed())
	if reasons := c.GetReasons(); len(reasons) > 0 {
		cell += "; " + strings.Join(reasons, " | ")
	}
	return csvCellUpTo(cell, math.MaxInt)
}

// csvCell makes an uploader-derived value safe as a CSV cell. CSV is often
// read on a terminal, so the value is cleaned to one line; a spreadsheet runs
// a cell starting with = + - @ (or tab or CR, which cleaning already trims)
// as a formula (CWE-1236), so such a value gets a leading single quote.
// Server-generated cells (mapping IDs, numbers, enums) do not need it.
func csvCell(s string) string {
	return csvCellUpTo(s, maxUntrustedDisplayBytes)
}

// csvCellUpTo is csvCell with the cleaned value capped at maxBytes, for
// cells whose full text matters (a crosswalk's control statements).
func csvCellUpTo(s string, maxBytes int) string {
	s = intanalyzer.CleanDisplay(s, maxBytes)
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

// openOutput returns the writer for --output: the file at path, created or
// truncated, or the command's stdout when path is empty. The returned close
// function must be deferred with a pointer to the command's named error
// result; it joins a close error into it, since a failed close can mean the
// file was not fully written.
func openOutput(cmd *cobra.Command, path string) (io.Writer, func(err *error), error) {
	if path == "" {
		return cmd.OutOrStdout(), func(*error) {}, nil
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create output file: %w", err)
	}
	return f, func(runErr *error) {
		if closeErr := f.Close(); closeErr != nil {
			*runErr = errors.Join(*runErr, fmt.Errorf("close output file %s: %w", path, closeErr))
		}
	}, nil
}

// exportFormats lists the --format values export accepts.
var exportFormats = []string{"json", "csv"}

func newResultsExportCmd(state *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "export <job-id>",
		Aliases: []string{"dump"},
		Short:   "Export mappings to JSON or CSV",
		Long: `Export a job's control mappings to JSON or CSV format.

JSON wraps the mappings with the job's source and target catalogs (ID, name,
version) and direction under "analysis". Names and versions are the ones the
job recorded when it was created ("snapshot": true); a job created before
those records shows the catalogs' current ones ("snapshot": false). CSV
repeats them on every row in the job_source_catalog_*, job_target_catalog_*
and job_bidirectional columns, with job_source_catalog_snapshot and
job_target_catalog_snapshot saying which kind of label each side has: they
describe the job, so in a bidirectional job a row's source control can
belong to the job's target catalog. A CSV cell from a catalog
(control and catalog IDs, names, versions) that would start with = + - or @
gets a leading single quote, so a spreadsheet does not run it as a formula.
With --stage analyzer, which has no viability, JSON omits viabilityScore and
isViable and CSV leaves viability_score and is_viable empty.

When the job ran the critique stage, the pairs it vetoed are not mappings and
are left out. With --stage analyzer --include-vetoed they are listed too: JSON
carries each mapping's critique (vetoed, upheld, rejected, failed, reasons)
and CSV adds a critique column ("vetoed: 0 upheld, 3 rejected, 0 failed;
<reasons>"; empty for a mapping the critique did not review). The reasons are
the critics' own words.`,
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
			mappings, analysis, err := listAllMappings(cmd.Context(), state.client, req)
			if err != nil {
				return err
			}
			// Analyzer mappings have no viability; the server's zero values
			// are not a verdict, so they are left out.
			noViability := req.GetStage() == pb.MappingStage_MAPPING_STAGE_ANALYZER

			out, closeOutput, err := openOutput(cmd, outputFile)
			if err != nil {
				return err
			}
			defer closeOutput(&err)

			switch format {
			case "json":
				// encoding/json would print enums as numbers and drop false and
				// zero fields; protojson with EmitUnpopulated keeps every field
				// and the enum names. Fields that are not a verdict are then
				// dropped: viability on the analyzer stage, and the critique of
				// a mapping the critique did not review (null would read as one).
				raw := make([]json.RawMessage, len(mappings))
				for i, m := range mappings {
					b, err := protojson.MarshalOptions{EmitUnpopulated: true}.Marshal(m)
					if err != nil {
						return fmt.Errorf("encode mapping %s as JSON: %w", m.GetMappingId(), err)
					}
					var drop []string
					if noViability {
						drop = append(drop, "viabilityScore", "isViable")
					}
					if m.GetCritique() == nil {
						drop = append(drop, "critique")
					}
					if len(drop) > 0 {
						var fields map[string]json.RawMessage
						if err := json.Unmarshal(b, &fields); err != nil {
							return fmt.Errorf("encode mapping %s as JSON: %w", m.GetMappingId(), err)
						}
						for _, f := range drop {
							delete(fields, f)
						}
						if b, err = json.Marshal(fields); err != nil {
							return fmt.Errorf("encode mapping %s as JSON: %w", m.GetMappingId(), err)
						}
					}
					raw[i] = b
				}
				// null when the response carries no analysis. JSON keeps the
				// stored names; the encoder escapes their control characters.
				analysisJSON := json.RawMessage("null")
				if analysis != nil {
					b, err := protojson.MarshalOptions{EmitUnpopulated: true}.Marshal(analysis)
					if err != nil {
						return fmt.Errorf("encode the analysis of job %s as JSON: %w", jobID, err)
					}
					analysisJSON = b
				}
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]any{
					"job_id":   jobID,
					"analysis": analysisJSON,
					"mappings": raw,
				})
			case "csv":
				w := csv.NewWriter(out)
				defer w.Flush()

				// The critique column is added only on request, so the default
				// columns stay where scripts expect them.
				header := []string{"mapping_id", "source_control_id", "target_control_id", "relationship_type", "confidence", "viability_score", "is_viable"}
				if req.GetIncludeVetoed() {
					header = append(header, "critique")
				}
				header = append(header, analysisColumns...)
				if err := w.Write(header); err != nil {
					return fmt.Errorf("write CSV header: %w", err)
				}
				jobValues := analysisValues(analysis)
				for _, m := range mappings {
					viabilityScore, isViable := "", ""
					if !noViability {
						viabilityScore, isViable = fmt.Sprintf("%.3f", m.GetViabilityScore()), fmt.Sprintf("%t", m.GetIsViable())
					}
					row := []string{
						m.GetMappingId(),
						csvCell(m.GetSourceControlId()),
						csvCell(m.GetTargetControlId()),
						m.GetRelationshipType().String(),
						fmt.Sprintf("%.3f", m.GetConfidence()),
						viabilityScore,
						isViable,
					}
					if req.GetIncludeVetoed() {
						row = append(row, critiqueCell(m.GetCritique()))
					}
					if err := w.Write(append(row, jobValues...)); err != nil {
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
		usage: "crosscodex results export <job-id> [--format json|csv] [--output file] [--stage synthesized|analyzer] [--type <type>]... [--control <id>] [--min-confidence <0-1>] [--include-non-viable] [--include-vetoed]",
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

// listAllCandidates reads every page of a job's relationship candidates and
// the job's analysis, which the server sends on every page; the first page's
// is kept. It mutates req.Pagination to walk the page tokens.
func listAllCandidates(ctx context.Context, client crosscodexv1connect.GatewayServiceClient, req *pb.ListJobCandidatesRequest) ([]*pb.RelationshipCandidate, *pb.JobAnalysis, error) {
	var all []*pb.RelationshipCandidate
	var analysis *pb.JobAnalysis
	pageToken := ""
	seen := map[string]bool{}
	for {
		req.Pagination = &pb.Pagination{PageSize: mappingPageSize, PageToken: pageToken}
		resp, err := client.ListJobCandidates(ctx, connectrpc.NewRequest(req))
		if err != nil {
			return nil, nil, fmt.Errorf("list candidates for job %s: %w", req.GetJobId(), err)
		}
		all = append(all, resp.Msg.GetCandidates()...)
		if analysis == nil {
			analysis = resp.Msg.GetAnalysis()
		}
		seen[pageToken] = true
		pageToken = resp.Msg.GetPageInfo().GetNextPageToken()
		if pageToken == "" {
			return all, analysis, nil
		}
		if seen[pageToken] {
			return nil, nil, fmt.Errorf("list candidates for job %s: server returned page token %q again; stopping to avoid an endless loop. Report this as a crosscodexd bug", req.GetJobId(), pageToken)
		}
	}
}

func newResultsCandidatesCmd(state *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "candidates <job-id>",
		Short: "Export the pairs candidate generation proposed, to JSON or CSV",
		Long: `Export every source-target pair candidate generation proposed to the
relationship analyzer for a completed job, whether or not the analyzer later
mapped it. Use it to check how well candidate generation covers a crosswalk.

similarity_score is the embedding cosine similarity on a 0 to 100 scale; it is
0 when no embedding exists for the pair. Contributions list one entry per
generator that proposed the pair, with the generator's own score and the
weight rank fusion gave it. They are empty unless the candidate strategy is
rank fusion, so with another strategy JSON shows "contributions": [] and the
CSV generators column is empty.

JSON wraps the candidates with the job's source and target catalogs and
direction under "analysis", as results export does. CSV has the columns
source_control_id, target_control_id, similarity_score and generators, where
generators joins the proposing generators with ";". A CSV control ID that
would start with = + - or @ gets a leading single quote, so a spreadsheet does
not run it as a formula.`,
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			jobID := args[0]
			format, _ := cmd.Flags().GetString("format")
			outputFile, _ := cmd.Flags().GetString("output")
			controlID, _ := cmd.Flags().GetString("control")

			if !slices.Contains(exportFormats, format) {
				return fmt.Errorf("invalid --format %q: use one of %s", format, strings.Join(exportFormats, ", "))
			}

			candidates, analysis, err := listAllCandidates(cmd.Context(), state.client, &pb.ListJobCandidatesRequest{JobId: jobID, ControlId: controlID})
			if err != nil {
				return err
			}

			out, closeOutput, err := openOutput(cmd, outputFile)
			if err != nil {
				return err
			}
			defer closeOutput(&err)

			if format == "csv" {
				w := csv.NewWriter(out)
				defer w.Flush()

				if err := w.Write([]string{"source_control_id", "target_control_id", "similarity_score", "generators"}); err != nil {
					return fmt.Errorf("write CSV header: %w", err)
				}
				for _, c := range candidates {
					generators := make([]string, len(c.GetContributions()))
					for i, contribution := range c.GetContributions() {
						generators[i] = contribution.GetGenerator()
					}
					if err := w.Write([]string{
						csvCell(c.GetSourceControlId()),
						csvCell(c.GetTargetControlId()),
						fmt.Sprintf("%.3f", c.GetSimilarityScore()),
						strings.Join(generators, ";"),
					}); err != nil {
						return fmt.Errorf("write CSV row: %w", err)
					}
				}
				w.Flush()
				if err := w.Error(); err != nil {
					return fmt.Errorf("write CSV: %w", err)
				}
				return nil
			}

			// protojson with EmitUnpopulated keeps zero scores and empty
			// contributions that encoding/json would drop.
			raw := make([]json.RawMessage, len(candidates))
			for i, c := range candidates {
				b, err := protojson.MarshalOptions{EmitUnpopulated: true}.Marshal(c)
				if err != nil {
					return fmt.Errorf("encode candidate %s -> %s as JSON: %w", c.GetSourceControlId(), c.GetTargetControlId(), err)
				}
				raw[i] = b
			}
			analysisJSON := json.RawMessage("null")
			if analysis != nil {
				b, err := protojson.MarshalOptions{EmitUnpopulated: true}.Marshal(analysis)
				if err != nil {
					return fmt.Errorf("encode the analysis of job %s as JSON: %w", jobID, err)
				}
				analysisJSON = b
			}
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			return enc.Encode(map[string]any{
				"job_id":     jobID,
				"analysis":   analysisJSON,
				"candidates": raw,
			})
		},
	}

	guidedArgs(cmd, cobra.ExactArgs(1), argGuide{
		noun:  "a job-id",
		find:  "crosscodex results candidates <job-id>",
		usage: "crosscodex results candidates <job-id> [--format json|csv] [--output file] [--control <id>]",
		examples: []string{
			"crosscodex results candidates abc123",
			"crosscodex results candidates abc123 --format csv --output candidates.csv",
			"crosscodex results candidates abc123 --control AC-1",
		},
	})

	cmd.Flags().String("format", "json", "output format (json or csv)")
	cmd.Flags().String("output", "", "output file path (default: stdout)")
	cmd.Flags().String("control", "", "only candidates whose source or target is this control id")

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
			mappings, _, err := listAllMappings(cmd.Context(), state.client, req)
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
		Use:   "verify <bundle-file> --public-key <path> --tenant <tenant> --job-id <job-id>",
		Short: "Verify a job's attestation bundle",
		Long: `Verify the attestation bundle the pipeline stored for one job, offline.

The bundle is the job's bundle.json, stored under the daemon's object
storage (storage.objects.base_path) at attestations/<tenant>/<job-id>/bundle.json;
copy it out of storage first. --public-key is the attestation public key the
pipeline signed with (attestation.public_key_path, or the tenant's override).
--tenant and --job-id name the job the bundle must be for: the pipeline binds
every layout step and link to "job:<tenant>/<job-id>", so a bundle copied or
replayed from another job fails.

Verification checks that the bundle is bound to that job, with one job
config digest throughout (printed, so it can be compared with the job's
stored config), the layout's signature and that it has not expired
(attestation.expiry_duration), every link's signature, that every layout
step has exactly one link, and that every artifact one step consumes has
the digest the step that produced it recorded. It does not read the
database: it proves the bundle is intact and complete, not that the
database still holds the results it attests.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			bundleFile := args[0]
			publicKeyPath, _ := cmd.Flags().GetString("public-key")
			if publicKeyPath == "" {
				return fmt.Errorf("results verify needs --public-key: the attestation public key the pipeline signed " +
					"the bundle with (attestation.public_key_path; deploy/certs/attestation-pub.pem in a deploy/ stack). Nothing was verified")
			}
			tenantID, _ := cmd.Flags().GetString("tenant")
			jobID, _ := cmd.Flags().GetString("job-id")
			if tenantID == "" || jobID == "" {
				return fmt.Errorf("results verify needs --tenant and --job-id: the job the bundle must be for, as in " +
					"its path attestations/<tenant>/<job-id>/bundle.json. A bundle proves nothing about a job it is not bound to. Nothing was verified")
			}

			bundleData, err := os.ReadFile(bundleFile)
			if err != nil {
				return fmt.Errorf("failed to read bundle file: %w", err)
			}
			layout, links, err := attestation.ParseBundle(bundleData)
			if err != nil {
				return fmt.Errorf("%s: %w. Pass a job's bundle, stored at attestations/<tenant>/<job-id>/bundle.json "+
					"under the daemon's storage.objects.base_path", bundleFile, err)
			}

			gen, err := attestation.NewGenerator(&attestation.FileKeyProvider{PublicKeyPath: publicKeyPath})
			if err != nil {
				return fmt.Errorf("failed to create attestation verifier: %w", err)
			}
			subject := attestation.Artifact{URI: fmt.Sprintf("job:%s/%s", tenantID, jobID)}
			chain, err := gen.VerifyChain(cmd.Context(), layout, links, subject)
			if err != nil {
				return fmt.Errorf("verification failed for %s: %w", bundleFile, err)
			}
			verified := chain.Layout
			steps := make([]string, len(verified.Steps))
			for i, step := range verified.Steps {
				steps[i] = step.Name
			}

			return emit(cmd,
				func(w io.Writer, color bool) {
					fmt.Fprintf(w, "Bundle file: %s\n", bundleFile)
					fmt.Fprintf(w, "Status: Verified\n")
					fmt.Fprintf(w, "Tenant: %s\n", tenantID)
					fmt.Fprintf(w, "Job: %s\n", jobID)
					fmt.Fprintf(w, "Job config digest: %s\n", chain.Subject.Digest)
					fmt.Fprintf(w, "Steps: %s\n", strings.Join(steps, ", "))
					fmt.Fprintf(w, "Layout expires: %s\n", verified.Expires.Format(time.RFC3339))
				},
				map[string]any{
					"bundle_file":       bundleFile,
					"status":            "verified",
					"tenant":            tenantID,
					"job_id":            jobID,
					"job_config_digest": chain.Subject.Digest,
					"steps":             steps,
					"layout_expires":    verified.Expires.Format(time.RFC3339),
				},
			)
		},
	}

	guidedArgs(cmd, cobra.ExactArgs(1), argGuide{
		noun:  "a bundle file path",
		find:  "copy attestations/<tenant>/<job-id>/bundle.json out of the daemon's object storage",
		usage: "crosscodex results verify <bundle-file> --public-key <path> --tenant <tenant> --job-id <job-id>",
		examples: []string{
			"crosscodex results verify bundle.json --public-key deploy/certs/attestation-pub.pem --tenant default --job-id 0b6e...",
		},
	})

	cmd.Flags().String("public-key", "", "attestation public key PEM the pipeline signed with (required)")
	cmd.Flags().String("tenant", "", "tenant of the job the bundle must be for (required)")
	cmd.Flags().String("job-id", "", "ID of the job the bundle must be for (required)")

	return cmd
}
