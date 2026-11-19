package main

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"

	connectrpc "connectrpc.com/connect"
	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/cleaning"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// shellSafe matches words that need no quoting in a copy-pasted command line.
var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// shellQuote renders s as one POSIX shell word, so the next-step lines this
// file prints can be pasted back even when a catalog ID or tenant has spaces.
func shellQuote(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func newAdminCleaningCmd(state *cliState) *cobra.Command {
	cleaningCmd := &cobra.Command{
		Use:   "cleaning",
		Short: "Clean the mapping graph of redundant or contradictory edges",
		Long: `Detect and review redundant, contradictory and stale mapping verdicts.

  run        preview a cleaning pass, then apply it with the preview's token
  findings   list recorded findings
  show       show one finding with its evidence, history and state token
  decide     record a reviewer decision on a finding

These commands operate on the tenant supplied via --tenant.`,
		RunE: func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}

	cleaningCmd.AddCommand(newAdminCleaningRunCmd(state))
	cleaningCmd.AddCommand(newAdminCleaningFindingsCmd(state))
	cleaningCmd.AddCommand(newAdminCleaningShowCmd(state))
	cleaningCmd.AddCommand(newAdminCleaningDecideCmd(state))

	return cleaningCmd
}

// addScopeFlags registers the catalog-pair flags run and findings share.
func addScopeFlags(cmd *cobra.Command) {
	cmd.Flags().String("source-catalog", "", "Only findings between this source catalog ID and --target-catalog")
	cmd.Flags().String("target-catalog", "", "Only findings between --source-catalog and this target catalog ID")
}

// scopeFromFlags returns the catalog pair named by the flags, nil when
// neither is set, and an error when only one is.
func scopeFromFlags(cmd *cobra.Command) (*pb.CatalogPairScope, error) {
	src, _ := cmd.Flags().GetString("source-catalog")
	tgt, _ := cmd.Flags().GetString("target-catalog")
	switch {
	case src == "" && tgt == "":
		return nil, nil
	case src == "" || tgt == "":
		return nil, fmt.Errorf("--source-catalog and --target-catalog go together: give both catalog IDs to scope to one pair, or neither for all catalogs")
	}
	return &pb.CatalogPairScope{SourceCatalogId: src, TargetCatalogId: tgt}, nil
}

// scopeArgs renders the scope flags back as command-line words, for the
// next-step lines.
func scopeArgs(scope *pb.CatalogPairScope) string {
	if scope == nil {
		return ""
	}
	return " --source-catalog " + shellQuote(scope.GetSourceCatalogId()) + " --target-catalog " + shellQuote(scope.GetTargetCatalogId())
}

func newAdminCleaningRunCmd(state *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Preview, apply or resume an edge-cleaning pass",
		Long: `Run an edge-cleaning pass over the tenant's mapping verdicts.

Flow: preview, then apply with the preview's token.

  1. crosscodex admin cleaning run --tenant <t>
     Detects findings and prints what a pass would record and retire. Nothing
     changes. The output ends with the exact apply command.
  2. crosscodex admin cleaning run --apply --expect <token> --tenant <t>
     Records the findings and retires the verdicts the policy retires. The
     token binds the apply to the previewed plan; if the data changed since,
     the server refuses and you preview again.
  3. crosscodex admin cleaning run --resume --tenant <t>
     Applies to the graph every recorded change whose graph write did not
     finish: the retirements of an earlier apply, and reviewer decisions
     (retire, dismiss, restore). Takes no token and no catalog scope.

--source-catalog and --target-catalog (given together) limit a preview or
apply to one catalog pair; repeat the same pair when applying.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			tenant, err := requireTenant(cmd)
			if err != nil {
				return err
			}
			apply, _ := cmd.Flags().GetBool("apply")
			resume, _ := cmd.Flags().GetBool("resume")
			token, _ := cmd.Flags().GetString("expect")
			scope, err := scopeFromFlags(cmd)
			if err != nil {
				return err
			}

			mode := pb.CleaningMode_CLEANING_MODE_PREVIEW
			switch {
			case apply && resume:
				return fmt.Errorf("--apply and --resume cannot be combined: --apply writes a previewed plan (needs --expect <token>), --resume finishes every recorded change and takes no token")
			case apply && token == "":
				return fmt.Errorf("--apply requires --expect <token>: run without --apply first and copy the token from the line that starts \"To apply:\"")
			case !apply && token != "":
				return fmt.Errorf("--expect is only valid with --apply: it binds the apply to a previewed plan")
			case resume && scope != nil:
				return fmt.Errorf("--resume cannot be combined with --source-catalog/--target-catalog: it finishes every recorded change, not one catalog pair")
			case apply:
				mode = pb.CleaningMode_CLEANING_MODE_APPLY
			case resume:
				mode = pb.CleaningMode_CLEANING_MODE_RESUME
			}

			resp, err := state.cleaningClient.RunCleaning(cmd.Context(), connectrpc.NewRequest(&pb.RunCleaningRequest{
				TenantContext: &pb.TenantContext{TenantId: tenant},
				Mode:          mode,
				ExpectToken:   token,
				Scope:         scope,
			}))
			if err != nil {
				return err
			}

			msg := resp.Msg
			jsonOut, err := protoJSON(msg)
			if err != nil {
				return err
			}
			return emit(cmd,
				func(w io.Writer, color bool) { writeRunReport(w, msg, mode, tenant, scope) },
				jsonOut,
			)
		},
	}

	cmd.Flags().Bool("apply", false, "Apply the previewed plan; requires --expect")
	cmd.Flags().String("expect", "", "Plan token from the preview (with --apply)")
	cmd.Flags().Bool("resume", false, "Finish the graph write of every recorded change (apply retirements, reviewer decisions); takes no token or scope")
	addScopeFlags(cmd)

	return cmd
}

func newAdminCleaningFindingsCmd(state *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "findings",
		Short: "List edge-cleaning findings",
		Long: `List the tenant's recorded edge-cleaning findings, ordered by finding ID.

Filter by --signal (for example conflicts_with), --status (for example
pending) and a catalog pair (--source-catalog with --target-catalog). The
server rejects an unknown signal or status and names the accepted values.
When more results exist the output ends with the exact command for the next
page. Use "crosscodex admin cleaning show <finding-id>" to read one finding,
then "decide" to record a reviewer decision (it needs the reviewer role).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			tenant, err := requireTenant(cmd)
			if err != nil {
				return err
			}
			scope, err := scopeFromFlags(cmd)
			if err != nil {
				return err
			}
			signal, _ := cmd.Flags().GetString("signal")
			status, _ := cmd.Flags().GetString("status")
			pageToken, _ := cmd.Flags().GetString("page-token")
			pageSize, _ := cmd.Flags().GetInt32("page-size")

			resp, err := state.cleaningClient.ListFindings(cmd.Context(), connectrpc.NewRequest(&pb.ListFindingsRequest{
				TenantContext: &pb.TenantContext{TenantId: tenant},
				Signal:        signal,
				Status:        status,
				Scope:         scope,
				Pagination:    &pb.Pagination{PageSize: pageSize, PageToken: pageToken},
			}))
			if err != nil {
				return err
			}

			msg := resp.Msg
			jsonOut, err := protoJSON(msg)
			if err != nil {
				return err
			}
			return emit(cmd,
				func(w io.Writer, color bool) {
					writeFindingsList(w, msg, func(next string) string {
						line := "crosscodex admin cleaning findings --page-token " + shellQuote(next)
						if signal != "" {
							line += " --signal " + shellQuote(signal)
						}
						if status != "" {
							line += " --status " + shellQuote(status)
						}
						if pageSize != 0 {
							line += fmt.Sprintf(" --page-size %d", pageSize)
						}
						return line + scopeArgs(scope) + " --tenant " + shellQuote(tenant)
					})
				},
				jsonOut,
			)
		},
	}

	cmd.Flags().String("signal", "", "Only findings of this signal (for example conflicts_with)")
	cmd.Flags().String("status", "", "Only findings in this status (for example pending)")
	addScopeFlags(cmd)
	cmd.Flags().String("page-token", "", "Continue from the token a previous page printed")
	cmd.Flags().Int32("page-size", 0, "Findings per page (0 uses the server default)")

	return cmd
}

func newAdminCleaningShowCmd(state *cliState) *cobra.Command {
	return &cobra.Command{
		Use:   "show <finding-id>",
		Short: "Show one finding with its evidence, history and state token",
		Long: `Show one finding: its signal, status, the verdicts it concerns in readable
form (control identifiers, titles, catalogs, relationship), its decision
history and its state token.

Flow: show, then decide. The output ends with the exact "decide" command,
carrying the state token. The server refuses a decision when the finding
changed since you read it. "decide" needs the reviewer role, which admin
does not include.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			tenant, err := requireTenant(cmd)
			if err != nil {
				return err
			}
			resp, err := state.cleaningClient.GetFinding(cmd.Context(), connectrpc.NewRequest(&pb.GetFindingRequest{
				TenantContext: &pb.TenantContext{TenantId: tenant},
				FindingId:     args[0],
			}))
			if err != nil {
				return err
			}

			msg := resp.Msg
			jsonOut, err := protoJSON(msg)
			if err != nil {
				return err
			}
			return emit(cmd,
				func(w io.Writer, color bool) { writeFindingDetail(w, msg, tenant) },
				jsonOut,
			)
		},
	}
}

func newAdminCleaningDecideCmd(state *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "decide <finding-id>",
		Short: "Record a reviewer decision on a finding",
		Long: `Record a reviewer decision on a finding: retire, dismiss or restore.

Flow: "crosscodex admin cleaning show <finding-id>" first. It prints the
state token and the exact decide command. Which verdicts are allowed depends
on the finding's status; the server refuses the others and says why.

  retire    supersede the finding's target verdicts. For a finding that has
            no fixed targets, name the verdicts with --retire-verdict
            <job>:<source>:<target> (repeatable); show prints each key.
            A stale_prompt or stale_model finding is about a whole job:
            retire without --retire-verdict removes every live verdict
            of the job in scope, uncapped by max_retire_per_run; with it,
            only the named ones. restore recreates what it retired.
  dismiss   judge the finding not a problem.
  restore   reverse an earlier retire.

--rationale is stored with the decision and signed into its attestation.
--expect is the finding's state token; a finding that changed since it was
read is refused. This command needs the reviewer role, which admin does not
include: the daemon grants it through auth.x509_mappings.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			tenant, err := requireTenant(cmd)
			if err != nil {
				return err
			}
			verdict, _ := cmd.Flags().GetString("verdict")
			rationale, _ := cmd.Flags().GetString("rationale")
			token, _ := cmd.Flags().GetString("expect")
			retire, _ := cmd.Flags().GetStringArray("retire-verdict")

			switch verdict {
			case "":
				return fmt.Errorf("--verdict is required: one of %s, %s or %s", cleaning.VerdictRetire, cleaning.VerdictDismiss, cleaning.VerdictRestore)
			case cleaning.VerdictRetire, cleaning.VerdictDismiss, cleaning.VerdictRestore:
			default:
				return fmt.Errorf("--verdict %q is not valid: use %s, %s or %s", verdict, cleaning.VerdictRetire, cleaning.VerdictDismiss, cleaning.VerdictRestore)
			}
			if rationale == "" {
				return fmt.Errorf("--rationale is required: state why, it is stored with the decision")
			}
			if token == "" {
				return fmt.Errorf("--expect is required: the state token printed by \"crosscodex admin cleaning show %s\"", shellQuote(args[0]))
			}
			if len(retire) > 0 && verdict != cleaning.VerdictRetire {
				return fmt.Errorf("--retire-verdict is only valid with --verdict %s", cleaning.VerdictRetire)
			}
			refs := make([]*pb.VerdictRef, len(retire))
			for i, raw := range retire {
				k, err := cleaning.ParseVerdictKey(raw)
				if err != nil {
					return fmt.Errorf("--retire-verdict %q is not valid, use <job>:<source>:<target> as printed by show: %w", raw, err)
				}
				refs[i] = &pb.VerdictRef{JobId: k.JobID, SourceNodeId: k.SourceID, TargetNodeId: k.TargetID}
			}

			resp, err := state.cleaningClient.DecideFinding(cmd.Context(), connectrpc.NewRequest(&pb.DecideFindingRequest{
				TenantContext:    &pb.TenantContext{TenantId: tenant},
				FindingId:        args[0],
				Verdict:          verdict,
				Rationale:        rationale,
				ExpectStateToken: token,
				RetireVerdicts:   refs,
			}))
			if err != nil {
				return err
			}

			msg := resp.Msg
			jsonOut, err := protoJSON(msg)
			if err != nil {
				return err
			}
			return emit(cmd,
				func(w io.Writer, color bool) {
					fmt.Fprintf(w, "Recorded %s decision %s on finding %s.\n", verdict, msg.GetDecisionId(), args[0])
					fmt.Fprintf(w, "Finding is now %s.\n", msg.GetFinding().GetStatus())
					fmt.Fprintf(w, "State token: %s\n", msg.GetStateToken())
				},
				jsonOut,
			)
		},
	}

	cmd.Flags().String("verdict", "", "Decision: retire, dismiss or restore (required)")
	cmd.Flags().String("rationale", "", "Why; stored with the signed decision (required)")
	cmd.Flags().String("expect", "", "State token from \"show\" (required)")
	cmd.Flags().StringArray("retire-verdict", nil, "Verdict to retire as <job>:<source>:<target> (repeatable; retire only)")

	return cmd
}

// protoJSON converts msg to a generic JSON value keyed by the proto field
// names (snake_case), with zero values and empty lists present, so --json
// output matches the API contract. protojson renders int64 fields
// (summary.skipped_edges) as strings.
func protoJSON(msg proto.Message) (any, error) {
	b, err := protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("encoding the response as JSON: %w", err)
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("re-reading the encoded response: %w", err)
	}
	return v, nil
}

// controlName renders a control for a reader: label or identifier, the title
// when there is one, and the catalog in brackets.
func controlName(c *pb.ControlRef) string {
	name := c.GetLabel()
	if name == "" {
		name = c.GetIdentifier()
	}
	if t := c.GetTitle(); t != "" {
		if name == "" {
			name = t
		} else {
			name += " " + fmt.Sprintf("%q", t)
		}
	}
	if name == "" {
		name = "(unnamed control)"
	}
	if cat := c.GetCatalog(); cat != "" {
		name += " [" + cat + "]"
	}
	return name
}

func verdictLine(v *pb.VerdictEvidence) string {
	rel := v.GetRelationship()
	if rel == "" {
		rel = "(no relationship)"
	}
	return fmt.Sprintf("%s --%s--> %s", controlName(v.GetSource()), rel, controlName(v.GetTarget()))
}

// jobLine renders a job-level finding's job for a reader: its catalogs, when
// it completed and how many of its verdicts the run read.
func jobLine(j *pb.FindingJob) string {
	return fmt.Sprintf("job %s: %s -> %s, completed %s, %d verdict(s) in scope",
		j.GetJobId(), j.GetSourceCatalog(), j.GetTargetCatalog(), formatTimestamp(j.GetCompletedAt()), j.GetVerdictsInScope())
}

func writeRunReport(w io.Writer, resp *pb.RunCleaningResponse, mode pb.CleaningMode, tenant string, scope *pb.CatalogPairScope) {
	if mode == pb.CleaningMode_CLEANING_MODE_RESUME {
		fmt.Fprintf(w, "Resumed: projected %d recorded finding change(s) to the graph (retirements, restores and reviewer decisions).\n", resp.GetProjected())
		return
	}
	verb := "Preview (nothing was changed)"
	if mode == pb.CleaningMode_CLEANING_MODE_APPLY {
		verb = "Applied"
	}
	fmt.Fprintf(w, "%s: %d finding(s)\n", verb, len(resp.GetFindings()))
	for _, pf := range resp.GetFindings() {
		f := pf.GetFinding()
		fmt.Fprintf(w, "\n  %s -> %s  %s  finding %s\n", pf.GetChange(), pf.GetToStatus(), f.GetSignal(), f.GetFindingId())
		if j := f.GetJob(); j != nil {
			fmt.Fprintf(w, "    %s\n", jobLine(j))
		}
		for _, v := range f.GetVerdicts() {
			fmt.Fprintf(w, "    %s\n", verdictLine(v))
		}
	}
	s := resp.GetSummary()
	fmt.Fprintf(w, "\nSummary: %d verdict(s) across %d catalog pair(s)\n", s.GetVerdicts(), s.GetPairs())
	if s.GetUnknownPrompt() != 0 || s.GetUnknownModel() != 0 {
		fmt.Fprintf(w, "  unknown prompt: %d, unknown model: %d\n", s.GetUnknownPrompt(), s.GetUnknownModel())
	}
	if s.GetSkippedEdges() != 0 {
		fmt.Fprintf(w, "  skipped edges: %d\n", s.GetSkippedEdges())
	}
	if s.GetUnaddressableVerdicts() != 0 {
		fmt.Fprintf(w, "  unaddressable verdicts: %d (found, but not retirable by the policy)\n", s.GetUnaddressableVerdicts())
	}
	fmt.Fprintf(w, "Verdicts to retire: %d\n", resp.GetRetireVerdicts())
	if mode == pb.CleaningMode_CLEANING_MODE_PREVIEW {
		fmt.Fprintf(w, "\nTo apply: crosscodex admin cleaning run --apply --expect %s%s --tenant %s\n",
			shellQuote(resp.GetPlanToken()), scopeArgs(scope), shellQuote(tenant))
	}
}

func writeFindingsList(w io.Writer, resp *pb.ListFindingsResponse, nextCmd func(token string) string) {
	if len(resp.GetFindings()) == 0 {
		fmt.Fprintln(w, "No findings.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "FINDING\tSIGNAL\tSTATUS\tUPDATED\tCONTROLS")
	for _, f := range resp.GetFindings() {
		first := ""
		if j := f.GetJob(); j != nil {
			first = jobLine(j)
		} else if vs := f.GetVerdicts(); len(vs) > 0 {
			first = verdictLine(vs[0])
			if len(vs) > 1 {
				first += fmt.Sprintf(" (+%d more)", len(vs)-1)
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", f.GetFindingId(), f.GetSignal(), f.GetStatus(), formatTimestamp(f.GetUpdatedAt()), first)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(w, "Error flushing output: %v\n", err)
	}
	if next := resp.GetPageInfo().GetNextPageToken(); next != "" {
		fmt.Fprintf(w, "\nMore findings. Next page: %s\n", nextCmd(next))
	}
}

func writeFindingDetail(w io.Writer, resp *pb.GetFindingResponse, tenant string) {
	f := resp.GetFinding()
	fmt.Fprintf(w, "Finding %s\n", f.GetFindingId())
	fmt.Fprintf(w, "  signal:        %s\n", f.GetSignal())
	fmt.Fprintf(w, "  status:        %s\n", f.GetStatus())
	fmt.Fprintf(w, "  updated:       %s\n", formatTimestamp(f.GetUpdatedAt()))
	fmt.Fprintf(w, "  evidence hash: %s\n", f.GetEvidenceHash())

	if len(f.GetDetail()) > 0 {
		keys := make([]string, 0, len(f.GetDetail()))
		for k := range f.GetDetail() {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintln(w, "\nDetail:")
		for _, k := range keys {
			fmt.Fprintf(w, "  %s: %s\n", k, f.GetDetail()[k])
		}
	}

	if j := f.GetJob(); j != nil {
		fmt.Fprintf(w, "\nJob:\n  %s\n", jobLine(j))
	} else {
		fmt.Fprintf(w, "\nVerdicts (%d):\n", len(f.GetVerdicts()))
	}
	for _, v := range f.GetVerdicts() {
		ref := v.GetRef()
		fmt.Fprintf(w, "  %s\n", verdictLine(v))
		fmt.Fprintf(w, "    key: %s:%s:%s", ref.GetJobId(), ref.GetSourceNodeId(), ref.GetTargetNodeId())
		if v.Agreement != nil {
			fmt.Fprintf(w, "  agreement: %.2f", v.GetAgreement())
		}
		if v.GetJobCompletedAt() != nil {
			fmt.Fprintf(w, "  job completed: %s", formatTimestamp(v.GetJobCompletedAt()))
		}
		fmt.Fprintln(w)
	}
	if len(f.GetTargets()) > 0 {
		fmt.Fprintf(w, "\nTargets (retired when the finding is):\n")
		for _, t := range f.GetTargets() {
			fmt.Fprintf(w, "  %s:%s:%s\n", t.GetJobId(), t.GetSourceNodeId(), t.GetTargetNodeId())
		}
	}

	fmt.Fprintf(w, "\nDecisions (%d):\n", len(resp.GetDecisions()))
	for _, d := range resp.GetDecisions() {
		fmt.Fprintf(w, "  %s  %s by %s\n", formatTimestamp(d.GetDecidedAt()), d.GetVerdict(), d.GetDecidedBy())
		fmt.Fprintf(w, "    rationale: %s\n", d.GetRationale())
		if d.GetRunId() != "" {
			fmt.Fprintf(w, "    run: %s\n", d.GetRunId())
		}
		fmt.Fprintf(w, "    attestation sha256: %s\n", d.GetAttestationSha256())
	}

	fmt.Fprintf(w, "\nState token: %s\n", resp.GetStateToken())
	fmt.Fprintf(w, "To decide: crosscodex admin cleaning decide %s --verdict <%s|%s|%s> --rationale \"<text>\" --expect %s --tenant %s\n",
		shellQuote(f.GetFindingId()), cleaning.VerdictRetire, cleaning.VerdictDismiss, cleaning.VerdictRestore,
		shellQuote(resp.GetStateToken()), shellQuote(tenant))
}
