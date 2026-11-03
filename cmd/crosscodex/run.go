package main

import (
	"bufio"
	"fmt"
	"io"
	"slices"
	"strings"

	connectrpc "connectrpc.com/connect"
	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/pipeline"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
	"github.com/spf13/cobra"
)

func newRunCmd(state *cliState) *cobra.Command {
	runCmd := &cobra.Command{
		Use:   "run",
		Short: "Start analysis jobs and monitor progress",
		RunE:  func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}

	runCmd.AddCommand(newRunStartCmd(state))
	runCmd.AddCommand(newRunStatusCmd(state))
	runCmd.AddCommand(newRunCancelCmd(state))
	runCmd.AddCommand(newRunRetryCmd(state))

	return runCmd
}

func newRunStartCmd(state *cliState) *cobra.Command {
	const usage = "crosscodex run start --source <catalog-id> --target <catalog-id> [--bidirectional]"
	examples := []string{
		"crosscodex run start --source 3f2a9c1e --target 7b41d0aa",
		"crosscodex run start --source 3f2a9c1e --target 7b41d0aa --bidirectional",
		"crosscodex run start --source 3f2a9c1e --target 3f2a9c1e",
		"crosscodex run start --source 3f2a9c1e --target 7b41d0aa --tenant acme",
		"crosscodex run start --source 3f2a9c1e --target 7b41d0aa --no-cache",
	}
	cmd := &cobra.Command{
		Use:   "start --source <catalog-id> --target <catalog-id>",
		Short: "Map an imported catalog's controls onto another catalog's",
		Long: `Map the controls of an imported source catalog onto a target catalog.

Queues a full analysis job. The job classifies and embeds the controls of
both catalogs, then pairs each source control with its most similar target
controls (source to target). --bidirectional also pairs each target control
with its most similar source controls, so the pair analyzers' LLM calls scale
with source plus target controls, not source controls alone: mapping a
100-control catalog onto a 1,000-control one costs about 11 times as many.

To analyse within one catalog, pass its ID to both --source and --target.
Within one catalog, controls are paired in both directions already, so
--bidirectional is refused there.

LLM results are reused across your tenant's jobs: a request identical to
one an earlier job sent (same text, prompt, model and settings) is answered
from the cache, so re-running an unchanged analysis costs only one probe
call per model. --no-cache sends every call to the model for a fresh run;
its results are still stored for later jobs. "crosscodex run status" shows
what was reused.

Find catalog IDs with "crosscodex catalog list". Analysis runs in the
crosscodexd daemon; embedded mode refuses it.`,
		Example: exampleBlock(examples),
		// Args runs before the connection is opened, so usage errors need no daemon.
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("'%s' takes no file: import the catalog first with \"crosscodex catalog import <file>\", then pass its ID with --source\n\n  Usage: %s",
					cmd.CommandPath(), usage)
			}
			source, _ := cmd.Flags().GetString("source")
			if source == "" {
				return fmt.Errorf("'%s' requires --source <catalog-id>\n\n  Usage: %s\n\n  Find available values:\n    crosscodex catalog list\n\n  Examples:\n%s",
					cmd.CommandPath(), usage, exampleBlock(examples))
			}
			target, _ := cmd.Flags().GetString("target")
			if target == "" {
				return fmt.Errorf("'%s' requires --target <catalog-id>: the catalog whose controls the source's are mapped onto. "+
					"To analyse within one catalog, pass its ID as the target too: --source %s --target %s\n\n  Usage: %s\n\n  Find available values:\n    crosscodex catalog list\n\n  Examples:\n%s",
					cmd.CommandPath(), source, source, usage, exampleBlock(examples))
			}
			if bidirectional, _ := cmd.Flags().GetBool("bidirectional"); bidirectional && target == source {
				return fmt.Errorf("'%s' cannot combine --bidirectional with --target equal to --source: an analysis within one catalog "+
					"already pairs its controls in both directions. To analyse within the catalog, drop --bidirectional, or pass a different --target\n\n  Usage: %s",
					cmd.CommandPath(), usage)
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			source, _ := cmd.Flags().GetString("source")
			target, _ := cmd.Flags().GetString("target")
			bidirectional, _ := cmd.Flags().GetBool("bidirectional")
			noCache, _ := cmd.Flags().GetBool("no-cache")
			tenant, _ := cmd.Flags().GetString("tenant")

			req := &pb.StartAnalysisRequest{SourceCatalogId: source, TargetCatalogId: target, Bidirectional: bidirectional, NoCache: noCache}
			if tenant != "" {
				req.TenantContext = &pb.TenantContext{TenantId: tenant}
			}

			resp, err := state.client.StartAnalysis(cmd.Context(), connectrpc.NewRequest(req))
			if err != nil {
				return fmt.Errorf("failed to start analysis: %w", err)
			}

			return emit(cmd,
				func(w io.Writer, color bool) {
					fmt.Fprintf(w, "Analysis started\n")
					fmt.Fprintf(w, "Job ID: %s\n", resp.Msg.GetJobId())
					fmt.Fprintf(w, "Status: %s\n", formatJobStatus(resp.Msg.GetStatus()))
				},
				map[string]string{
					"job_id": resp.Msg.GetJobId(),
					"status": resp.Msg.GetStatus().String(),
				},
			)
		},
	}

	cmd.Flags().String("source", "", "ID of the imported catalog whose controls are mapped (see 'crosscodex catalog list')")
	cmd.Flags().String("target", "", "ID of the imported catalog the source's controls are mapped onto; the source's ID analyses within one catalog")
	cmd.Flags().Bool("bidirectional", false, "also map the target's controls onto the source's (pair LLM calls scale with source plus target controls; refused within one catalog)")
	cmd.Flags().Bool("no-cache", false, "skip result-cache lookups: send every LLM call to the model for a fresh run (results are still stored for later jobs)")
	cmd.Flags().String("tenant", "", "Tenant ID for the job")

	return cmd
}

func newRunStatusCmd(state *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status <job-id>",
		Short: "Check the status of an analysis job",
		Long: `Check the status of an analysis job by its job ID.

Shows the job's source and target catalogs (name and version as recorded
when the job was created, ID and control count) and the direction it pairs
their controls in. Lists the analyzers the
job runs: those enabled on the daemon when the job was created. Mappings
come only from the relationship analyzer; a job without it produces none,
and the output says so.

Shows, per analyzer, how many LLM results were reused from earlier jobs and
how many calls were sent to the model, and which model answered each alias.
Only figures a signed cache record vouches for are shown; an analyzer
without one shows as not attested, or in progress while the job runs.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			jobID := args[0]
			tenant, _ := cmd.Flags().GetString("tenant")

			req := &pb.GetJobRequest{
				JobId:                 jobID,
				IncludeCacheSummaries: true,
			}
			if tenant != "" {
				req.TenantContext = &pb.TenantContext{
					TenantId: tenant,
				}
			}

			resp, err := state.client.GetJob(cmd.Context(), connectrpc.NewRequest(req))
			if err != nil {
				return fmt.Errorf("failed to get job status: %w", err)
			}

			job := resp.Msg.GetJob()
			catalogs := readJobCatalogs(cmd.Context(), state.client, job.GetConfig(), tenant)

			// The stages show which analyzers the daemon runs the job with:
			// a disabled analyzer gets none. Mappings come only from the
			// relationship analyzer, so without it the job completes with no
			// mappings, which otherwise looks like success.
			analyzers := []string{}
			for _, stage := range job.GetStages() {
				if pipeline.IsAnalyzerStage(stage.GetStageName()) {
					analyzers = append(analyzers, stage.GetStageName())
				}
			}

			return emit(cmd,
				func(w io.Writer, color bool) {
					fmt.Fprintf(w, "Job: %s\n", job.GetJobId())
					fmt.Fprintf(w, "Status: %s\n", formatJobStatus(job.GetStatus()))
					if catalogs != nil {
						fmt.Fprintf(w, "Source catalog: %s\n", catalogs.Source.describe())
						fmt.Fprintf(w, "Target catalog: %s\n", catalogs.Target.describe())
						fmt.Fprintf(w, "Direction: %s\n", directionText[catalogs.Direction])
					}
					if job.GetAudit() != nil && job.GetAudit().GetCreatedAt() != nil {
						fmt.Fprintf(w, "Created: %s\n", job.GetAudit().GetCreatedAt().AsTime().Format("2006-01-02 15:04:05"))
					}
					if job.GetError() != nil {
						fmt.Fprintf(w, "Error: %s\n", job.GetError().GetMessage())
					}
					if p := job.GetProgress(); p != nil {
						fmt.Fprintf(w, "Stages: %d of %d completed, %d failed, %d cancelled\n",
							p.GetCompletedSteps(), p.GetTotalSteps(), p.GetFailedSteps(), p.GetCancelledSteps())
					}
					if len(analyzers) == 0 {
						fmt.Fprintf(w, "Analyzers: none\n")
					} else {
						fmt.Fprintf(w, "Analyzers: %s\n", strings.Join(analyzers, ", "))
					}
					if !slices.Contains(analyzers, results.RelationshipAnalyzer) {
						fmt.Fprintf(w, "Note: this job has no relationship analyzer, so it produces no mappings. "+
							"To get mappings, set analysis.relationship.enabled to true on the daemon "+
							"(with analysis.relationship.models and an enabled analysis.candidates.generators entry), "+
							"then start a new job.\n")
					}
					writeCacheLines(w, job.GetCacheSummaries(), state.daemon != nil, len(analyzers))
				},
				map[string]any{
					"job_id":    job.GetJobId(),
					"status":    job.GetStatus().String(),
					"audit":     job.GetAudit(),
					"error":     job.GetError(),
					"progress":  job.GetProgress(),
					"analyzers": analyzers,
					"analysis":  catalogs.toJSON(),
					"cache":     cacheToJSON(job.GetCacheSummaries()),
				},
			)
		},
	}

	guidedArgs(cmd, cobra.ExactArgs(1), argGuide{
		noun:  "a job-id",
		find:  "crosscodex run status <job-id>",
		usage: "crosscodex run status <job-id>",
		examples: []string{
			"crosscodex run status abc123",
			"crosscodex run status --tenant acme abc123",
		},
	})

	cmd.Flags().String("tenant", "", "Tenant ID for the job")

	return cmd
}

func newRunCancelCmd(state *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "cancel <job-id>",
		Aliases: []string{"stop"},
		Short:   "Cancel a running analysis job",
		Long: `Cancel a running analysis job.

This command prompts for confirmation unless --yes is given. Without a
terminal on stdin (a script or pipe) it refuses instead of prompting; pass
--yes to confirm.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			jobID := args[0]
			yes, _ := cmd.Flags().GetBool("yes")
			tenant, _ := cmd.Flags().GetString("tenant")

			if !yes {
				// Without a terminal nobody can answer the prompt: reading a
				// pipe that never closes would hang, and a piped "y" would
				// let a script confirm on a person's behalf.
				if !stdinIsTerminal(cmd.InOrStdin()) {
					return fmt.Errorf("refusing to cancel job %s without confirmation: stdin is not a terminal, so the prompt cannot be answered; pass --yes to confirm (crosscodex run cancel --yes %s)",
						jobID, jobID)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Cancel job %s? (y/N): ", jobID)
				reader := bufio.NewReader(cmd.InOrStdin())
				response, err := reader.ReadString('\n')
				if err != nil {
					return fmt.Errorf("failed to read confirmation: %w", err)
				}
				response = strings.TrimSpace(strings.ToLower(response))
				if response != "y" && response != "yes" {
					fmt.Fprintln(cmd.OutOrStdout(), "Cancelled")
					return nil
				}
			}

			req := &pb.CancelJobRequest{
				JobId: jobID,
			}
			if tenant != "" {
				req.TenantContext = &pb.TenantContext{
					TenantId: tenant,
				}
			}

			_, err := state.client.CancelJob(cmd.Context(), connectrpc.NewRequest(req))
			if err != nil {
				return fmt.Errorf("failed to cancel job: %w", err)
			}

			return emit(cmd,
				func(w io.Writer, color bool) {
					fmt.Fprintf(w, "Job %s cancellation requested\n", jobID)
				},
				map[string]string{
					"job_id":    jobID,
					"cancelled": "true",
				},
			)
		},
	}

	guidedArgs(cmd, cobra.ExactArgs(1), argGuide{
		noun:  "a job-id",
		find:  "crosscodex run cancel <job-id>",
		usage: "crosscodex run cancel <job-id>",
		examples: []string{
			"crosscodex run cancel abc123",
			"crosscodex run cancel --yes abc123",
		},
	})

	cmd.Flags().BoolP("yes", "y", false, "Skip confirmation prompt")
	cmd.Flags().String("tenant", "", "Tenant ID for the job")

	return cmd
}

func newRunRetryCmd(state *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "retry <job-id>",
		Short: "Retry a failed or cancelled analysis job",
		Long: `Retry an analysis job.

With --from-failure, the job resumes in place from its first failed or
cancelled stage, keeping the stages that completed; a job whose stages all
completed (its attestation could not be stored) only finishes. The daemon
refuses a job whose failure a retry cannot fix, such as an attestation that
does not verify, and says to start a new job instead.

Without --from-failure, a new job starts over the same catalog and config,
and its ID is printed.

Neither deletes results: a resumed job re-runs only stages that did not
complete, so it asks for no confirmation.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			jobID := args[0]
			fromFailure, _ := cmd.Flags().GetBool("from-failure")

			resp, err := state.client.RetryJob(cmd.Context(), connectrpc.NewRequest(&pb.RetryJobRequest{
				JobId:            jobID,
				RetryFromFailure: fromFailure,
			}))
			if err != nil {
				return fmt.Errorf("failed to retry job %s: %w", jobID, err)
			}
			newJobID := resp.Msg.GetNewJobId()

			return emit(cmd,
				func(w io.Writer, color bool) {
					if fromFailure {
						fmt.Fprintf(w, "Job %s resumed from its failure\n", newJobID)
					} else {
						fmt.Fprintf(w, "Started job %s, a retry of job %s\n", newJobID, jobID)
					}
					fmt.Fprintf(w, "Follow it with: crosscodex run status %s\n", newJobID)
				},
				map[string]any{
					"job_id":         newJobID,
					"retried_job_id": jobID,
					"from_failure":   fromFailure,
				},
			)
		},
	}

	guidedArgs(cmd, cobra.ExactArgs(1), argGuide{
		noun:  "a job-id",
		find:  "the job ID crosscodex run start printed; crosscodex run status <job-id> shows whether it failed",
		usage: "crosscodex run retry [--from-failure] <job-id>",
		examples: []string{
			"crosscodex run retry --from-failure abc123",
			"crosscodex run retry abc123",
		},
	})

	cmd.Flags().Bool("from-failure", false, "Resume the job in place from its first failed or cancelled stage instead of starting a new job")

	return cmd
}

func formatJobStatus(status pb.JobStatus) string {
	switch status {
	case pb.JobStatus_JOB_STATUS_PENDING:
		return "PENDING"
	case pb.JobStatus_JOB_STATUS_RUNNING:
		return "RUNNING"
	case pb.JobStatus_JOB_STATUS_COMPLETED:
		return "COMPLETED"
	case pb.JobStatus_JOB_STATUS_FAILED:
		return "FAILED"
	case pb.JobStatus_JOB_STATUS_CANCELLED:
		return "CANCELLED"
	default:
		return status.String()
	}
}
