package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	connectrpc "connectrpc.com/connect"
	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/spf13/cobra"
)

// crosswalkStages maps the --stage flag values to the API enum.
var crosswalkStages = map[string]pb.CrosswalkStage{
	"synthesized": pb.CrosswalkStage_CROSSWALK_STAGE_SYNTHESIZED,
	"analyzer":    pb.CrosswalkStage_CROSSWALK_STAGE_ANALYZER,
}

// crosswalkEmbeddedMessage is why GetCrosswalk is unavailable in embedded
// mode: the embedded daemon runs no graph service.
const crosswalkEmbeddedMessage = "crosswalks are read from the graph, which embedded mode does not run: connect to a crosscodexd daemon with --endpoint <host:port>"

const crosswalkUsage = "crosscodex crosswalk <catalog-a> [<catalog-b>] [--job <id>] [--all] [--via <hub>] [--format csv|json|xlsx|md] [--output file] [--stage synthesized|analyzer] [--min-confidence <0-1>] [--type <type>]... [--include-non-viable]"

func newCrosswalkCmd(state *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "crosswalk <catalog-a> [<catalog-b>]",
		Short: "Export a shareable crosswalk between catalogs as CSV, JSON, XLSX or Markdown",
		Long: `Export the control mappings between catalogs as one readable table: each row
names both controls (framework, identifier, parent, title, statement), the
relationship, its confidence and how many jobs agreed, so a reader needs no
internal IDs. Every format has the same columns:

  source_framework, source_id, source_parent, source_title, source_text,
  relationship, confidence, direction, hub_id,
  target_framework, target_id, target_parent, target_title, target_text,
  jobs_judged, jobs_agreeing, other_verdicts, latest_job,
  source_node_id, target_node_id

The mode follows the arguments:

  <a> <b>            every pair judged between the two catalogs, from any
                     completed job in either direction, merged
  --job <id>         one completed job's verdicts
  <a> --all          catalog a against every catalog it has completed jobs with
  <a> <c> --via <h>  a to c derived through catalog h's controls (direction
                     "derived"), plus the direct a to c rows ("direct")

Only related pairs are listed: the relationship analyzer does not store "no
relationship" verdicts. The synthesized stage lists only the mappings synthesis
judged viable, as "results export" does; --include-non-viable adds the rest. The summary on stderr says how many completed jobs
the rows came from; the JSON "provenance" records the catalogs, jobs, stage
and server version that produced them. CSV starts with a UTF-8 byte order
mark so spreadsheets read the encoding, and a cell that would start with
= + - or @ gets a leading single quote so it is not run as a formula (XLSX
cells get the same rule and are all text). XLSX is a workbook with sheets
crosswalk, provenance and disagreements, and needs --output; md is a Markdown
table with text cleaned, HTML- and Markdown-escaped and cut to 200 characters.

Crosswalks are read from the graph, so they need a crosscodexd daemon:
embedded mode does not run one. Find catalog IDs with "crosscodex catalog list".`,
	}

	guidedArgs(cmd, cobra.MaximumNArgs(2), argGuide{
		noun:  "a mode: two catalogs, one catalog with --all, or --job",
		find:  "crosscodex catalog list",
		usage: crosswalkUsage,
		examples: []string{
			"crosscodex crosswalk <catalog-a> <catalog-b> --format csv --output crosswalk.csv",
			"crosscodex crosswalk --job <job-id> --format json",
			"crosscodex crosswalk <catalog-a> --all --output a-to-everything.csv",
			"crosscodex crosswalk <catalog-a> <catalog-c> --via <hub-catalog> --min-confidence 0.7",
		},
	})

	cmd.Flags().String("job", "", "export one completed job's verdicts")
	cmd.Flags().Bool("all", false, "compare the catalog against every catalog it has completed jobs with")
	cmd.Flags().String("via", "", "derive catalog a to c rows through this hub catalog")
	cmd.Flags().String("format", "csv", "output format ("+strings.Join(crosswalkFormatNames(), " or ")+")")
	cmd.Flags().String("output", "", "output file path (default: stdout)")
	cmd.Flags().String("stage", "synthesized", "verdicts to read: synthesized or analyzer")
	cmd.Flags().Float64("min-confidence", 0, "only rows with confidence >= this value (0 to 1)")
	cmd.Flags().Bool("include-non-viable", false, "include pairs synthesis filtered out (synthesized stage only)")
	cmd.Flags().StringSlice("type", nil, "only these relationship types (repeatable, case-insensitive)")

	cmd.RunE = func(cmd *cobra.Command, args []string) (err error) {
		format, _ := cmd.Flags().GetString("format")
		render, ok := crosswalkFormats[format]
		if !ok {
			return fmt.Errorf("invalid --format %q: use one of %s", format, strings.Join(crosswalkFormatNames(), ", "))
		}
		outputFile, _ := cmd.Flags().GetString("output")
		if format == "xlsx" && outputFile == "" {
			return fmt.Errorf("xlsx output is binary: pass --output <file>.xlsx")
		}
		req, err := crosswalkRequestFromFlags(cmd, args)
		if err != nil {
			return err
		}
		if state.daemon != nil {
			return errors.New(crosswalkEmbeddedMessage)
		}

		rows, prov, err := listAllCrosswalk(cmd.Context(), state.client, req)
		if err != nil {
			return err
		}

		out, closeOutput, err := openOutput(cmd, outputFile)
		if err != nil {
			return err
		}
		defer closeOutput(&err)
		if err := render(out, prov, rows); err != nil {
			return err
		}

		stderr := cmd.ErrOrStderr()
		fmt.Fprintf(stderr, "%d rows from %d completed jobs\n", len(rows), len(prov.GetJobIds()))
		if len(prov.GetJobIds()) == 0 {
			fmt.Fprintln(stderr, noCompletedJobsAdvice(args))
		} else if len(rows) == 0 {
			fmt.Fprintln(stderr, noRowsAdvice(req.GetStage()))
		}
		if n := prov.GetSkippedMissingControls(); n > 0 {
			fmt.Fprintf(stderr, "%d mappings skipped because a control they name is gone (catalog re-imported or deleted): re-run the analysis on the current catalogs to cover them\n", n)
		}
		return nil
	}

	return cmd
}

// crosswalkFormatNames lists the --format values, sorted.
func crosswalkFormatNames() []string {
	names := make([]string, 0, len(crosswalkFormats))
	for name := range crosswalkFormats {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// crosswalkRequestFromFlags resolves the mode from the arguments and flags
// and validates the filters, so mistakes surface here with guidance before
// any network call.
func crosswalkRequestFromFlags(cmd *cobra.Command, args []string) (*pb.GetCrosswalkRequest, error) {
	jobID, _ := cmd.Flags().GetString("job")
	all, _ := cmd.Flags().GetBool("all")
	via, _ := cmd.Flags().GetString("via")

	req := &pb.GetCrosswalkRequest{}
	modes := 0
	for _, set := range []bool{jobID != "", all, via != ""} {
		if set {
			modes++
		}
	}
	if modes > 1 {
		return nil, fmt.Errorf("--job, --all and --via each select a different mode: pass only one of them")
	}

	switch {
	case jobID != "":
		if len(args) > 0 {
			return nil, fmt.Errorf("--job takes no catalogs: crosscodex crosswalk --job <job-id>")
		}
		req.Mode = &pb.GetCrosswalkRequest_JobId{JobId: jobID}
	case all:
		if len(args) != 1 {
			return nil, fmt.Errorf("--all needs exactly one catalog: crosscodex crosswalk <a> --all")
		}
		req.Mode = &pb.GetCrosswalkRequest_OneToManyCatalogId{OneToManyCatalogId: args[0]}
	case via != "":
		if len(args) != 2 {
			return nil, fmt.Errorf("--via needs two catalogs: crosscodex crosswalk <a> <c> --via <hub>")
		}
		req.Mode = &pb.GetCrosswalkRequest_Transitive{Transitive: &pb.CrosswalkTransitive{
			CatalogA: args[0], CatalogC: args[1], HubCatalogId: via,
		}}
	case len(args) == 2:
		req.Mode = &pb.GetCrosswalkRequest_Pair{Pair: &pb.CrosswalkPair{CatalogA: args[0], CatalogB: args[1]}}
	default:
		return nil, fmt.Errorf("choose what to export: two catalogs (crosscodex crosswalk <a> <b>), one catalog with --all, or --job <job-id>")
	}

	stageName, _ := cmd.Flags().GetString("stage")
	stage, ok := crosswalkStages[strings.ToLower(stageName)]
	if !ok {
		return nil, fmt.Errorf("invalid --stage %q: use one of synthesized, analyzer", stageName)
	}
	req.Stage = stage

	minConfidence, _ := cmd.Flags().GetFloat64("min-confidence")
	if !(minConfidence >= 0 && minConfidence <= 1) {
		return nil, fmt.Errorf("invalid --min-confidence %v: use a value between 0 and 1", minConfidence)
	}
	req.MinConfidence = float32(minConfidence)

	includeNonViable, _ := cmd.Flags().GetBool("include-non-viable")
	if includeNonViable && stage == pb.CrosswalkStage_CROSSWALK_STAGE_ANALYZER {
		return nil, fmt.Errorf("--include-non-viable only applies to the synthesized stage: analyzer mappings have no viability, so drop the flag or use --stage synthesized")
	}
	req.IncludeNonViable = includeNonViable

	typeNames, _ := cmd.Flags().GetStringSlice("type")
	for _, name := range typeNames {
		t, err := parseRelationshipType(name)
		if err != nil {
			return nil, err
		}
		if t == pb.RelationshipType_RELATIONSHIP_TYPE_NO_RELATIONSHIP {
			return nil, fmt.Errorf("no-relationship verdicts are not stored: the relationship analyzer omits them, so a crosswalk lists related pairs only")
		}
		req.RelationshipTypes = append(req.RelationshipTypes, t)
	}
	return req, nil
}

// noCompletedJobsAdvice says what to run when the catalogs a crosswalk names
// have no completed job: an empty crosswalk is not an error.
func noCompletedJobsAdvice(args []string) string {
	switch len(args) {
	case 0:
		return "the job has no completed stages to read: check it with \"crosscodex run status <job-id>\""
	case 1:
		return fmt.Sprintf("no completed jobs involving %s: run \"crosscodex run start --source %s --target <other-catalog>\" first", args[0], args[0])
	default:
		return fmt.Sprintf("no completed jobs between %s and %s: run \"crosscodex run start --source %s --target %s\" first", args[0], args[1], args[0], args[1])
	}
}

// noRowsAdvice says why a crosswalk drawn from completed jobs can still be
// empty: the filters, or the graph content of the stage.
func noRowsAdvice(stage pb.CrosswalkStage) string {
	if stage == pb.CrosswalkStage_CROSSWALK_STAGE_ANALYZER {
		return "no rows: the completed jobs have no related pairs that pass the filters (--min-confidence, --type)"
	}
	return "no rows: the filters may exclude every pair, synthesis may have rejected them (--include-non-viable shows those), " +
		"or the jobs completed before synthesized edges were written to the graph (re-run them, or read the analyzer's verdicts with --stage analyzer)"
}

// listAllCrosswalk reads every page of the request's result set and returns
// the rows and the provenance, which the server sends on every page; the
// first page's is kept. It mutates req.Pagination to walk the page tokens.
func listAllCrosswalk(ctx context.Context, client crosscodexv1connect.GatewayServiceClient, req *pb.GetCrosswalkRequest) ([]*pb.CrosswalkRow, *pb.CrosswalkProvenance, error) {
	var all []*pb.CrosswalkRow
	var prov *pb.CrosswalkProvenance
	pageToken := ""
	seen := map[string]bool{}
	for {
		req.Pagination = &pb.Pagination{PageSize: mappingPageSize, PageToken: pageToken}
		resp, err := client.GetCrosswalk(ctx, connectrpc.NewRequest(req))
		if err != nil {
			return nil, nil, fmt.Errorf("get crosswalk: %w", err)
		}
		all = append(all, resp.Msg.GetRows()...)
		if prov == nil {
			prov = resp.Msg.GetProvenance()
		}
		seen[pageToken] = true
		pageToken = resp.Msg.GetPageInfo().GetNextPageToken()
		if pageToken == "" {
			return all, prov, nil
		}
		if seen[pageToken] {
			return nil, nil, fmt.Errorf("get crosswalk: server returned page token %q again; stopping to avoid an endless loop. Report this as a crosscodexd bug", pageToken)
		}
	}
}
