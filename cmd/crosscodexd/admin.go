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

	"github.com/complytime-labs/crosscodex/internal/analyzer/artifacts"
	"github.com/complytime-labs/crosscodex/pkg/config"
	dbpkg "github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/retention"
)

// adminUsage lists every `crosscodexd admin` subcommand.
const adminUsage = `usage:
  crosscodexd admin retention scan (--tenant <id> | --all-tenants) [--dry-run]
  crosscodexd admin reconcile artifacts (--tenant <id> | --all-tenants) [--dry-run] [--max-edges <n>]
  crosscodexd admin backup run
  crosscodexd admin backup list
  crosscodexd admin backup verify [--point <id>]
  crosscodexd admin backup restore --point <id>
  crosscodexd admin tenant create --tenant <id> --display-name <name>
  crosscodexd admin tenant list
  crosscodexd admin tenant inspect --tenant <id>
  crosscodexd admin tenant suspend --tenant <id>
  crosscodexd admin tenant resume --tenant <id>
  crosscodexd admin tenant import --file <path>`

// runAdmin dispatches the on-host `admin` subcommand tree. args are the tokens
// after "admin" (e.g. ["retention", "scan", "--tenant", "acme"]). It returns a
// process exit code: 2 for usage/argument errors, otherwise the code from the
// resolved handler. Admin subcommands are unauthenticated on-host operations,
// consistent with the `healthcheck` and `version` one-shots.
func runAdmin(args []string) int {
	switch {
	case len(args) >= 2 && args[0] == "retention" && args[1] == "scan":
		return retentionScanCmd(args[2:])
	case len(args) >= 2 && args[0] == "reconcile" && args[1] == "artifacts":
		return reconcileArtifactsCmd(args[2:])
	case len(args) >= 2 && args[0] == "backup" && args[1] == "run":
		return backupRunCmd(args[2:])
	case len(args) >= 2 && args[0] == "backup" && args[1] == "list":
		return backupListCmd(args[2:])
	case len(args) >= 2 && args[0] == "backup" && args[1] == "verify":
		return backupVerifyCmd(args[2:])
	case len(args) >= 2 && args[0] == "backup" && args[1] == "restore":
		return backupRestoreCmd(args[2:])
	case len(args) >= 2 && args[0] == "tenant" && args[1] == "create":
		return tenantCreateCmd(args[2:])
	case len(args) >= 2 && args[0] == "tenant" && args[1] == "list":
		return tenantListCmd(args[2:])
	case len(args) >= 2 && args[0] == "tenant" && args[1] == "inspect":
		return tenantInspectCmd(args[2:])
	case len(args) >= 2 && args[0] == "tenant" && args[1] == "suspend":
		return tenantSetStatusCmd(args[2:], "suspend", dbpkg.TenantStatusSuspended)
	case len(args) >= 2 && args[0] == "tenant" && args[1] == "resume":
		return tenantSetStatusCmd(args[2:], "resume", dbpkg.TenantStatusActive)
	case len(args) >= 2 && args[0] == "tenant" && args[1] == "import":
		return tenantImportCmd(args[2:])
	}
	fmt.Fprintln(os.Stderr, adminUsage)
	return 2
}

// retentionScanCmd parses `admin retention scan` flags and runs the scan.
func retentionScanCmd(args []string) int {
	fs := flag.NewFlagSet("crosscodexd admin retention scan", flag.ContinueOnError)
	tenantID := fs.String("tenant", "", "tenant ID whose data to scan")
	allTenants := fs.Bool("all-tenants", false, "scan every active tenant (needs database.tenant_admin_dsn)")
	dryRun := fs.Bool("dry-run", false, "report what would change without mutating any data")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if !tenantSelection("admin retention scan", *tenantID, *allTenants) {
		return 2
	}

	return runRetentionScan(*tenantID, *allTenants, *dryRun)
}

// reconcileArtifactsCmd parses `admin reconcile artifacts` flags and runs the
// reconciler.
func reconcileArtifactsCmd(args []string) int {
	fs := flag.NewFlagSet("crosscodexd admin reconcile artifacts", flag.ContinueOnError)
	tenantID := fs.String("tenant", "", "tenant ID whose artifacts to reconcile")
	allTenants := fs.Bool("all-tenants", false, "reconcile every active tenant (needs database.tenant_admin_dsn)")
	dryRun := fs.Bool("dry-run", false, "report what would be written without writing")
	maxEdges := fs.Int("max-edges", artifacts.DefaultReconcileMaxEdges, "fail before writing if the run would add more SAME_AS edges than this")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if !tenantSelection("admin reconcile artifacts", *tenantID, *allTenants) {
		return 2
	}
	if *maxEdges <= 0 {
		fmt.Fprintln(os.Stderr, "admin reconcile artifacts: --max-edges must be positive")
		return 2
	}

	return runReconcileArtifacts(*tenantID, *allTenants, *dryRun, *maxEdges)
}

// runRetentionScan loads the daemon config and runs one in-process retention
// scan for tenantID, or for every active tenant when all is set, printing each
// report. It returns a process exit code (0 success, 1 on any failure).
// runForTenants scopes the context via tenant.WithTenant before each scan so
// the RLS-keyed DBCollector, archive, and purge paths resolve the target
// tenant; scoping first also fails a malformed tenant fast, before any pool is
// opened.
func runRetentionScan(tenantID string, all, dryRun bool) int {
	const cmd = "admin retention scan"
	ctx := context.Background()

	cfg, ok := loadAdminConfig(ctx, cmd)
	if !ok {
		return 1
	}

	return runForTenants(ctx, cmd, cfg, tenantID, all, func(ctx context.Context) (tenantRunner, func(), error) {
		scan, closeFn, err := openRetentionScanFn(ctx, cfg)
		if err != nil {
			return nil, nil, err
		}
		return func(ctx context.Context, id string) error {
			report, err := scan(ctx, id, retention.ScanOptions{
				DryRun: dryRun,
				Now:    time.Now().UTC(),
			})
			if err != nil {
				return err
			}
			writeRetentionReport(os.Stdout, report)
			return nil
		}, closeFn, nil
	})
}

// retentionScanner runs one tenant's retention scan on resources opened once
// for the whole command. ctx must already carry the target tenant.
type retentionScanner func(ctx context.Context, tenantID string, opts retention.ScanOptions) (retention.Report, error)

// openRetentionScanFn opens the resources a retention scan needs and returns
// a scanner over them plus the func that closes them. It is a package var so
// the one-shot's config load, tenant scoping, and dry-run propagation can be
// unit-tested with a fake that records its arguments, without a live
// database. Production wiring is daemonOpenRetentionScan.
var openRetentionScanFn = daemonOpenRetentionScan

// daemonOpenRetentionScan builds the shared resources and the retention wiring
// the gateway AdminService uses (buildRetentionWiring) once, so --all-tenants
// runs every tenant on one set of connections. Each scan builds that tenant's
// retention.Engine and runs it with the configured default policy.
func daemonOpenRetentionScan(ctx context.Context, cfg *config.Config) (retentionScanner, func(), error) {
	shared, err := buildSharedResources(ctx, cfg, requiredResources{db: true, nats: true})
	if err != nil {
		return nil, nil, err
	}

	wiring, err := buildRetentionWiring(cfg, shared, slog.Default())
	if err != nil {
		shared.close()
		return nil, nil, err
	}

	scan := func(ctx context.Context, tenantID string, opts retention.ScanOptions) (retention.Report, error) {
		engine, err := wiring.engineFor(tenantID, wiring.defaultPolicy)
		if err != nil {
			return retention.Report{}, err
		}
		return engine.Scan(ctx, opts)
	}
	closeFn := func() {
		if wiring.purgePool != nil {
			wiring.purgePool.Close()
		}
		shared.close()
	}
	return scan, closeFn, nil
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

// runReconcileArtifacts loads the daemon config and runs one artifact
// reconcile for tenantID, or for every active tenant when all is set, printing
// each result. runForTenants scopes the context first, failing a malformed
// tenant before any connection is opened. It returns a process exit code: 0 on
// success, 1 on any failure. Bulk writes use graph.max_bulk_edges as the chunk
// size, the same bound the graph RPC enforces.
func runReconcileArtifacts(tenantID string, all, dryRun bool, maxEdges int) int {
	const cmd = "admin reconcile artifacts"
	ctx := context.Background()

	cfg, ok := loadAdminConfig(ctx, cmd)
	if !ok {
		return 1
	}

	return runForTenants(ctx, cmd, cfg, tenantID, all, func(ctx context.Context) (tenantRunner, func(), error) {
		reconcile, closeFn, err := openReconcileArtifactsFn(ctx, cfg)
		if err != nil {
			return nil, nil, err
		}
		return func(ctx context.Context, id string) error {
			res, err := reconcile(ctx, id, artifacts.ReconcileOptions{
				DryRun:    dryRun,
				MaxEdges:  maxEdges,
				ChunkSize: cfg.Graph.MaxBulkEdges,
				Now:       time.Now().UTC(),
			})
			if err != nil {
				return err
			}
			writeReconcileReport(os.Stdout, res)
			return nil
		}, closeFn, nil
	})
}

// artifactReconciler runs one tenant's artifact reconcile on a graph
// connection opened once for the whole command. ctx must already carry the
// target tenant.
type artifactReconciler func(ctx context.Context, tenantID string, opts artifacts.ReconcileOptions) (artifacts.ReconcileResult, error)

// openReconcileArtifactsFn opens the daemon's graph connection and app pool
// and returns a reconciler over them plus the func that closes them. It is a
// package var so flag parsing, config load and tenant scoping can be
// unit-tested with a fake, without a live database. Production wiring is
// daemonOpenReconcileArtifacts.
var openReconcileArtifactsFn = daemonOpenReconcileArtifacts

// daemonOpenReconcileArtifacts opens the graph connection and the app pool
// that backs the SAME_AS verdict store; each reconcile runs
// artifacts.Reconcile on them.
func daemonOpenReconcileArtifacts(ctx context.Context, cfg *config.Config) (artifactReconciler, func(), error) {
	shared, err := buildSharedResources(ctx, cfg, requiredResources{db: true, graph: true})
	if err != nil {
		return nil, nil, err
	}
	store := artifacts.NewPGVerdictStore(dbpkg.NewTenantPool(shared.appPool))
	reconcile := func(ctx context.Context, tenantID string, opts artifacts.ReconcileOptions) (artifacts.ReconcileResult, error) {
		return artifacts.Reconcile(ctx, shared.graphDB_, store, tenantID, opts)
	}
	return reconcile, shared.close, nil
}

// writeReconcileReport renders an artifacts.ReconcileResult to w. Under
// --dry-run the new_* rows count what the run would have written.
func writeReconcileReport(w io.Writer, res artifacts.ReconcileResult) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "METRIC\tVALUE")
	fmt.Fprintf(tw, "artifacts\t%d\n", res.Artifacts)
	fmt.Fprintf(tw, "skipped\t%d\n", res.Skipped)
	fmt.Fprintf(tw, "groups\t%d\n", res.Groups)
	fmt.Fprintf(tw, "new_groups\t%d\n", res.NewGroups)
	fmt.Fprintf(tw, "new_memberships\t%d\n", res.NewMemberships)
	fmt.Fprintf(tw, "matches\t%d\n", res.Matches)
	fmt.Fprintf(tw, "closed\t%d\n", res.Closed)
	fmt.Fprintf(tw, "new_matches\t%d\n", res.NewMatches)
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(w, "error flushing output: %v\n", err)
	}
}
