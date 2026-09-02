package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/retention"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// runAdmin dispatches the on-host `admin` subcommand tree. args are the tokens
// after "admin" (e.g. ["retention", "scan", "--tenant", "acme"]). It returns a
// process exit code: 2 for usage/argument errors, otherwise the code from the
// resolved handler. Only `retention scan` exists today; it is an unauthenticated
// on-host operation, consistent with the `healthcheck` and `version` one-shots.
func runAdmin(args []string) int {
	if len(args) < 2 || args[0] != "retention" || args[1] != "scan" {
		fmt.Fprintln(os.Stderr, "usage: crosscodexd admin retention scan --tenant <id> [--dry-run]")
		return 2
	}

	fs := flag.NewFlagSet("crosscodexd admin retention scan", flag.ContinueOnError)
	tenantID := fs.String("tenant", "", "tenant ID whose data to scan (required)")
	dryRun := fs.Bool("dry-run", false, "report what would change without mutating any data")
	if err := fs.Parse(args[2:]); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if *tenantID == "" {
		fmt.Fprintln(os.Stderr, "admin retention scan: --tenant is required")
		return 2
	}

	return runRetentionScan(*tenantID, *dryRun)
}

// runRetentionScan loads the daemon config, scopes the context to tenantID, runs
// one in-process retention scan against the daemon's configured resources, and
// prints the report. It returns a process exit code (0 success, 1 on any
// failure). The context is scoped via tenant.WithTenant before the scan so the
// RLS-keyed DBCollector, archive, and purge paths resolve the target tenant;
// scoping first also fails a malformed tenant fast, before any pool is opened.
func runRetentionScan(tenantID string, dryRun bool) int {
	ctx := context.Background()

	cfg, err := config.NewLoader().Load(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "admin retention scan: load config: %v\n", err)
		return 1
	}

	ctx, err = tenant.WithTenant(ctx, tenantID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "admin retention scan: %v\n", err)
		return 1
	}

	report, err := runRetentionScanFn(ctx, cfg, tenantID, retention.ScanOptions{
		DryRun: dryRun,
		Now:    time.Now().UTC(),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "admin retention scan: %v\n", err)
		return 1
	}

	writeRetentionReport(os.Stdout, report)
	return 0
}

// runRetentionScanFn executes a tenant-scoped retention scan against the
// daemon's configured resources. It is a package var so the one-shot's config
// load, tenant scoping, and dry-run propagation can be unit-tested with a fake
// that records its arguments, without a live database. Production wiring is
// daemonRetentionScan.
var runRetentionScanFn = daemonRetentionScan

// daemonRetentionScan builds the shared resources and a per-tenant
// retention.Engine via the same seam the gateway AdminService uses
// (buildRetentionWiring), runs one scan with the configured default policy, and
// closes every pool it opened before returning. ctx must already carry the
// target tenant.
func daemonRetentionScan(ctx context.Context, cfg *config.Config, tenantID string, opts retention.ScanOptions) (retention.Report, error) {
	shared, err := buildSharedResources(ctx, cfg, requiredResources{db: true, nats: true})
	if err != nil {
		return retention.Report{}, err
	}
	defer shared.close()

	wiring, err := buildRetentionWiring(cfg, shared, slog.Default())
	if err != nil {
		return retention.Report{}, err
	}
	if wiring.purgePool != nil {
		defer wiring.purgePool.Close()
	}

	engine, err := wiring.engineFor(tenantID, wiring.defaultPolicy)
	if err != nil {
		return retention.Report{}, err
	}
	return engine.Scan(ctx, opts)
}

// writeRetentionReport renders a retention.Report to w in a human-readable form:
// the aggregate counters, a per-class breakdown (class order sorted for stable
// output), and any non-fatal errors the scan accumulated.
func writeRetentionReport(w io.Writer, report retention.Report) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "METRIC\tVALUE")
	fmt.Fprintf(tw, "scanned\t%d\n", report.Scanned)
	fmt.Fprintf(tw, "held\t%d\n", report.Held)
	fmt.Fprintf(tw, "archived\t%d\n", report.Archived)
	fmt.Fprintf(tw, "purged\t%d\n", report.Purged)
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(w, "error flushing output: %v\n", err)
	}

	if len(report.PerClass) > 0 {
		classes := make([]string, 0, len(report.PerClass))
		for class := range report.PerClass {
			classes = append(classes, string(class))
		}
		sort.Strings(classes)

		fmt.Fprintln(w, "\nPer class:")
		ctw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(ctw, "CLASS\tSCANNED\tHELD\tARCHIVED\tPURGED")
		for _, class := range classes {
			c := report.PerClass[retention.DataClass(class)]
			fmt.Fprintf(ctw, "%s\t%d\t%d\t%d\t%d\n", class, c.Scanned, c.Held, c.Archived, c.Purged)
		}
		if err := ctw.Flush(); err != nil {
			fmt.Fprintf(w, "error flushing output: %v\n", err)
		}
	}

	for _, e := range report.Errors {
		fmt.Fprintf(w, "error: %s\n", e)
	}
}
