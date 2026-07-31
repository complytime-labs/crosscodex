package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	connectrpc "connectrpc.com/connect"
	"github.com/spf13/cobra"
)

func newRunCmd(state *cliState) *cobra.Command {
	runCmd := &cobra.Command{
		Use:   "run",
		Short: "Submit analysis jobs and monitor progress",
		RunE:  func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}

	runCmd.AddCommand(newRunStartCmd(state))
	runCmd.AddCommand(newRunStatusCmd(state))
	runCmd.AddCommand(newRunCancelCmd(state))

	return runCmd
}

func newRunStartCmd(state *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "start <file>",
		Aliases: []string{"submit"},
		Short:   "Start an analysis job by submitting a document",
		Long: `Start an analysis job by submitting a document.

Submits a document for full compliance analysis. The document will be
parsed, imported as a catalog, and queued for analysis.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) < 1 {
				return fmt.Errorf("'%s' requires a file path\n\n  Usage: %s\n\n  Examples:\n%s",
					cmd.CommandPath(),
					"crosscodex run start <file>",
					exampleBlock([]string{
						"crosscodex run start nist-800-53.json",
						"crosscodex run start oscal-catalog.json",
					}))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			filePath := args[0]

			content, err := os.ReadFile(filePath)
			if err != nil {
				return fmt.Errorf("failed to read file: %w", err)
			}

			req := &pb.SubmitDocumentRequest{
				Source: &pb.SubmitDocumentRequest_Content{
					Content: content,
				},
				CatalogFormat: pb.CatalogFormat_CATALOG_FORMAT_OSCAL,
			}

			resp, err := state.client.SubmitDocument(cmd.Context(), connectrpc.NewRequest(req))
			if err != nil {
				return fmt.Errorf("failed to submit document: %w", err)
			}

			return emit(cmd,
				func(w io.Writer, color bool) {
					fmt.Fprintf(w, "Job submitted successfully\n")
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

	cmd.Flags().String("tenant", "", "Tenant ID for the job")

	return cmd
}

func newRunStatusCmd(state *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status <job-id>",
		Short: "Check the status of an analysis job",
		Long:  `Check the status of an analysis job by its job ID.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			jobID := args[0]
			tenant, _ := cmd.Flags().GetString("tenant")

			req := &pb.GetJobRequest{
				JobId: jobID,
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

			return emit(cmd,
				func(w io.Writer, color bool) {
					fmt.Fprintf(w, "Job: %s\n", job.GetJobId())
					fmt.Fprintf(w, "Status: %s\n", formatJobStatus(job.GetStatus()))
					if job.GetAudit() != nil && job.GetAudit().GetCreatedAt() != nil {
						fmt.Fprintf(w, "Created: %s\n", job.GetAudit().GetCreatedAt().AsTime().Format("2006-01-02 15:04:05"))
					}
					if job.GetError() != nil {
						fmt.Fprintf(w, "Error: %s\n", job.GetError().GetMessage())
					}
				},
				map[string]any{
					"job_id": job.GetJobId(),
					"status": job.GetStatus().String(),
					"audit":  job.GetAudit(),
					"error":  job.GetError(),
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

This command will prompt for confirmation unless the --yes flag is provided.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			jobID := args[0]
			yes, _ := cmd.Flags().GetBool("yes")
			tenant, _ := cmd.Flags().GetString("tenant")

			if !yes {
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
