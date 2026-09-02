package main

import (
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	connectrpc "connectrpc.com/connect"
	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func newAdminCmd(state *cliState) *cobra.Command {
	adminCmd := &cobra.Command{
		Use:   "admin",
		Short: "Administer data retention and legal holds",
		Long: `Administer server-side data retention lifecycle and legal holds.

These commands operate on the tenant supplied via --tenant. The server
authorizes every request against the caller's tenant, so --tenant must match
the tenant the caller is authenticated for.`,
		RunE: func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}

	adminCmd.PersistentFlags().String("tenant", "", "Tenant ID for the request (required)")

	adminCmd.AddCommand(newAdminRetentionCmd(state))
	adminCmd.AddCommand(newAdminHoldCmd(state))

	return adminCmd
}

func newAdminRetentionCmd(state *cliState) *cobra.Command {
	retentionCmd := &cobra.Command{
		Use:   "retention",
		Short: "Scan and inspect the data retention lifecycle",
		RunE:  func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}

	retentionCmd.AddCommand(newAdminRetentionScanCmd(state))
	retentionCmd.AddCommand(newAdminRetentionStatsCmd(state))

	return retentionCmd
}

func newAdminRetentionScanCmd(state *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "scan",
		Short: "Scan and enforce the retention lifecycle",
		Long: `Scan retention-eligible data and enforce the retention lifecycle.

With --dry-run the server reports what it would archive or purge without
mutating any data.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			tenant, err := requireTenant(cmd)
			if err != nil {
				return err
			}
			dryRun, _ := cmd.Flags().GetBool("dry-run")

			resp, err := state.adminClient.ScanRetention(cmd.Context(), connectrpc.NewRequest(&pb.ScanRetentionRequest{
				TenantContext: &pb.TenantContext{TenantId: tenant},
				DryRun:        dryRun,
			}))
			if err != nil {
				return err
			}

			report := resp.Msg.GetReport()
			return emit(cmd,
				func(w io.Writer, color bool) {
					writeRetentionReport(w, report)
				},
				map[string]any{"report": retentionReportToJSON(report)},
			)
		},
	}

	cmd.Flags().Bool("dry-run", false, "Report what would change without mutating data")

	return cmd
}

func newAdminRetentionStatsCmd(state *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Show retention-eligible counts and active hold total",
		RunE: func(cmd *cobra.Command, args []string) error {
			tenant, err := requireTenant(cmd)
			if err != nil {
				return err
			}

			resp, err := state.adminClient.GetRetentionStats(cmd.Context(), connectrpc.NewRequest(&pb.GetRetentionStatsRequest{
				TenantContext: &pb.TenantContext{TenantId: tenant},
			}))
			if err != nil {
				return err
			}

			report := resp.Msg.GetEligible()
			activeHolds := resp.Msg.GetActiveHolds()
			return emit(cmd,
				func(w io.Writer, color bool) {
					writeRetentionReport(w, report)
					fmt.Fprintf(w, "\nActive holds: %d\n", activeHolds)
				},
				map[string]any{
					"eligible":     retentionReportToJSON(report),
					"active_holds": activeHolds,
				},
			)
		},
	}

	return cmd
}

func newAdminHoldCmd(state *cliState) *cobra.Command {
	holdCmd := &cobra.Command{
		Use:   "hold",
		Short: "Create, release, and list legal holds",
		RunE:  func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}

	holdCmd.AddCommand(newAdminHoldCreateCmd(state))
	holdCmd.AddCommand(newAdminHoldReleaseCmd(state))
	holdCmd.AddCommand(newAdminHoldListCmd(state))

	return holdCmd
}

func newAdminHoldCreateCmd(state *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a legal hold preventing data deletion",
		RunE: func(cmd *cobra.Command, args []string) error {
			tenant, err := requireTenant(cmd)
			if err != nil {
				return err
			}
			name, _ := cmd.Flags().GetString("name")
			job, _ := cmd.Flags().GetString("job")
			tenantScope, _ := cmd.Flags().GetString("tenant-scope")
			createdBefore, _ := cmd.Flags().GetString("created-before")
			expiresAt, _ := cmd.Flags().GetString("expires-at")

			scope := &pb.HoldScope{
				TenantId: tenantScope,
				JobId:    job,
			}
			if createdBefore != "" {
				ts, err := time.Parse(time.RFC3339, createdBefore)
				if err != nil {
					return fmt.Errorf("invalid --created-before: %w", err)
				}
				scope.CreatedBefore = timestamppb.New(ts)
			}

			req := &pb.CreateHoldRequest{
				TenantContext: &pb.TenantContext{TenantId: tenant},
				Name:          name,
				Scope:         scope,
			}
			if expiresAt != "" {
				ts, err := time.Parse(time.RFC3339, expiresAt)
				if err != nil {
					return fmt.Errorf("invalid --expires-at: %w", err)
				}
				req.ExpiresAt = timestamppb.New(ts)
			}

			resp, err := state.adminClient.CreateHold(cmd.Context(), connectrpc.NewRequest(req))
			if err != nil {
				return err
			}

			hold := resp.Msg.GetHold()
			return emit(cmd,
				func(w io.Writer, color bool) {
					fmt.Fprintf(w, "Hold created\n")
					fmt.Fprintf(w, "ID: %s\n", hold.GetId())
					fmt.Fprintf(w, "Name: %s\n", hold.GetName())
				},
				holdToJSON(hold),
			)
		},
	}

	cmd.Flags().String("name", "", "Unique hold name (required)")
	cmd.Flags().String("job", "", "Restrict the hold to a single job ID")
	cmd.Flags().String("tenant-scope", "", "Advisory scope tenant ID; the server forces this to the caller's tenant")
	cmd.Flags().String("created-before", "", "Only hold data created before this RFC3339 timestamp")
	cmd.Flags().String("expires-at", "", "Automatically release the hold at this RFC3339 timestamp")
	if err := cmd.MarkFlagRequired("name"); err != nil {
		panic(err)
	}

	return cmd
}

func newAdminHoldReleaseCmd(state *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "release",
		Short: "Release a legal hold by name",
		RunE: func(cmd *cobra.Command, args []string) error {
			tenant, err := requireTenant(cmd)
			if err != nil {
				return err
			}
			name, _ := cmd.Flags().GetString("name")

			_, err = state.adminClient.ReleaseHold(cmd.Context(), connectrpc.NewRequest(&pb.ReleaseHoldRequest{
				TenantContext: &pb.TenantContext{TenantId: tenant},
				Name:          name,
			}))
			if err != nil {
				return err
			}

			return emit(cmd,
				func(w io.Writer, color bool) {
					fmt.Fprintf(w, "Hold %q released\n", name)
				},
				map[string]any{"name": name, "released": true},
			)
		},
	}

	cmd.Flags().String("name", "", "Name of the hold to release (required)")
	if err := cmd.MarkFlagRequired("name"); err != nil {
		panic(err)
	}

	return cmd
}

func newAdminHoldListCmd(state *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List active legal holds",
		RunE: func(cmd *cobra.Command, args []string) error {
			tenant, err := requireTenant(cmd)
			if err != nil {
				return err
			}

			resp, err := state.adminClient.ListHolds(cmd.Context(), connectrpc.NewRequest(&pb.ListHoldsRequest{
				TenantContext: &pb.TenantContext{TenantId: tenant},
			}))
			if err != nil {
				return err
			}

			holds := resp.Msg.GetHolds()
			if len(holds) == 0 {
				return emit(cmd,
					func(w io.Writer, color bool) {
						fmt.Fprintln(w, "No holds found")
					},
					[]any{},
				)
			}

			return emit(cmd,
				func(w io.Writer, color bool) {
					tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
					fmt.Fprintln(tw, "ID\tNAME\tJOB\tCREATED\tEXPIRES")
					for _, h := range holds {
						fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
							h.GetId(),
							h.GetName(),
							h.GetScope().GetJobId(),
							formatTimestamp(h.GetCreatedAt()),
							formatTimestamp(h.GetExpiresAt()),
						)
					}
					if err := tw.Flush(); err != nil {
						fmt.Fprintf(w, "Error flushing output: %v\n", err)
					}
				},
				holdsToJSON(holds),
			)
		},
	}

	return cmd
}

// requireTenant reads the persistent --tenant flag shared by the admin subtree
// and errors when it is empty. The server rejects admin RPCs whose tenant
// context does not match the caller's authenticated tenant, so an empty value
// can never succeed.
func requireTenant(cmd *cobra.Command) (string, error) {
	tenant, _ := cmd.Flags().GetString("tenant")
	if tenant == "" {
		return "", fmt.Errorf("--tenant is required")
	}
	return tenant, nil
}

// formatTimestamp renders an optional protobuf timestamp as RFC3339, or the
// empty string when unset.
func formatTimestamp(ts *timestamppb.Timestamp) string {
	if ts == nil {
		return ""
	}
	return ts.AsTime().Format(time.RFC3339)
}

func writeRetentionReport(w io.Writer, report *pb.RetentionReport) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "METRIC\tVALUE")
	fmt.Fprintf(tw, "scanned\t%d\n", report.GetScanned())
	fmt.Fprintf(tw, "held\t%d\n", report.GetHeld())
	fmt.Fprintf(tw, "archived\t%d\n", report.GetArchived())
	fmt.Fprintf(tw, "purged\t%d\n", report.GetPurged())
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(w, "Error flushing output: %v\n", err)
	}

	perClass := report.GetPerClass()
	if len(perClass) > 0 {
		fmt.Fprintln(w, "\nPer class:")
		ctw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(ctw, "CLASS\tSCANNED\tHELD\tARCHIVED\tPURGED")
		for class, c := range perClass {
			fmt.Fprintf(ctw, "%s\t%d\t%d\t%d\t%d\n",
				class,
				c.GetScanned(),
				c.GetHeld(),
				c.GetArchived(),
				c.GetPurged(),
			)
		}
		if err := ctw.Flush(); err != nil {
			fmt.Fprintf(w, "Error flushing output: %v\n", err)
		}
	}

	for _, e := range report.GetErrors() {
		fmt.Fprintf(w, "error: %s\n", e)
	}
}

func retentionReportToJSON(report *pb.RetentionReport) map[string]any {
	perClass := make(map[string]any, len(report.GetPerClass()))
	for class, c := range report.GetPerClass() {
		perClass[class] = map[string]any{
			"scanned":  c.GetScanned(),
			"held":     c.GetHeld(),
			"archived": c.GetArchived(),
			"purged":   c.GetPurged(),
		}
	}
	return map[string]any{
		"scanned":   report.GetScanned(),
		"held":      report.GetHeld(),
		"archived":  report.GetArchived(),
		"purged":    report.GetPurged(),
		"per_class": perClass,
		"errors":    report.GetErrors(),
	}
}

func holdToJSON(hold *pb.Hold) map[string]any {
	result := map[string]any{
		"id":          hold.GetId(),
		"name":        hold.GetName(),
		"created_at":  formatTimestamp(hold.GetCreatedAt()),
		"created_by":  hold.GetCreatedBy(),
		"expires_at":  formatTimestamp(hold.GetExpiresAt()),
		"released_at": formatTimestamp(hold.GetReleasedAt()),
		"released_by": hold.GetReleasedBy(),
	}
	if scope := hold.GetScope(); scope != nil {
		result["scope"] = map[string]any{
			"tenant_id":      scope.GetTenantId(),
			"job_id":         scope.GetJobId(),
			"created_before": formatTimestamp(scope.GetCreatedBefore()),
		}
	}
	return result
}

func holdsToJSON(holds []*pb.Hold) []map[string]any {
	result := make([]map[string]any, len(holds))
	for i, h := range holds {
		result[i] = holdToJSON(h)
	}
	return result
}
