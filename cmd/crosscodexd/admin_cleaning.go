package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/complytime-labs/crosscodex/internal/feedback"
	"github.com/complytime-labs/crosscodex/pkg/cleaning"
	"github.com/complytime-labs/crosscodex/pkg/config"
)

// cleaningCmdName prefixes the messages of `admin cleaning`.
const cleaningCmdName = "admin cleaning"

// cleaningReprojector re-projects one tenant's edge-cleaning findings and
// mapping reviews onto the graph. ctx must already carry the target tenant.
// Both funcs refuse while their own rows are unprojected.
type cleaningReprojector struct {
	cleaning func(ctx context.Context, tenantID string, dryRun bool) (cleaning.ReprojectReport, error)
	feedback func(ctx context.Context, tenantID string, dryRun bool) (feedback.ReprojectReport, error)
}

// openCleaningReprojectFn opens the resources re-projection needs and returns
// a reprojector over them plus the func that closes them. It is a package var
// so flag parsing and wiring can be unit-tested with a fake. Production
// wiring is daemonOpenCleaningReproject.
var openCleaningReprojectFn = daemonOpenCleaningReproject

// adminCleaningCmd parses `admin cleaning` flags and runs the re-projection.
// --reproject is the only action today and is required, so a later action
// cannot be started by accident with this command's flags.
func adminCleaningCmd(args []string) int {
	fs := flag.NewFlagSet("crosscodexd admin cleaning", flag.ContinueOnError)
	tenantID := fs.String("tenant", "", "tenant ID whose findings and reviews to re-project")
	allTenants := fs.Bool("all-tenants", false, "re-project every active tenant (needs database.tenant_admin_dsn)")
	dryRun := fs.Bool("dry-run", false, "report what would change without writing")
	reproject := fs.Bool("reproject", false, "write the tenant's edge-cleaning findings and mapping reviews back to a rebuilt graph (required)")
	if code, ok := parseNoArgs(fs, cleaningCmdName, args); !ok {
		return code
	}
	if !*reproject {
		fmt.Fprintf(os.Stderr, "%s: --reproject is required: it is the only action, and replays stored findings and reviews onto a rebuilt graph. Run with --reproject --tenant <id> (add --dry-run to preview)\n", cleaningCmdName)
		return 2
	}
	if !tenantSelection(cleaningCmdName, *tenantID, *allTenants) {
		return 2
	}

	return runCleaningReproject(*tenantID, *allTenants, *dryRun)
}

// runCleaningReproject loads the daemon config and re-projects tenantID, or
// every active tenant when all is set, printing each report. It returns a
// process exit code: 0 on success, 1 on any failure.
func runCleaningReproject(tenantID string, all, dryRun bool) int {
	// SIGINT or SIGTERM cancels the run at its next store or graph call; the
	// run is idempotent, so running it again finishes the rest.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, ok := loadAdminConfig(ctx, cleaningCmdName)
	if !ok {
		return 1
	}

	return runForTenants(ctx, cleaningCmdName, cfg, tenantID, all, scopeActive, func(ctx context.Context) (tenantRunner, func(), error) {
		reprojector, closeFn, err := openCleaningReprojectFn(ctx, cfg)
		if err != nil {
			return nil, nil, err
		}
		return func(ctx context.Context, id string) error {
			return reprojectTenant(ctx, os.Stdout, reprojector, id, dryRun)
		}, closeFn, nil
	})
}

// reprojectTenant re-projects one tenant. Both re-projections first run as a
// dry run: each refuses while its own rows are unprojected, and checking only
// at the second would leave the first already written. If either refuses, its
// error is returned and nothing was written. Then cleaning and feedback run
// for real, each report printed as it completes so a failure in the second
// still shows what the first did. Under dryRun each engine runs once, dry,
// and its report is printed.
func reprojectTenant(ctx context.Context, w io.Writer, r cleaningReprojector, tenantID string, dryRun bool) error {
	if dryRun {
		cleaned, err := r.cleaning(ctx, tenantID, true)
		if err != nil {
			return fmt.Errorf("cleaning: %w", err)
		}
		writeCleaningReprojectReport(w, cleaned, true)
		reviewed, err := r.feedback(ctx, tenantID, true)
		if err != nil {
			return fmt.Errorf("feedback: %w", err)
		}
		writeFeedbackReprojectReport(w, reviewed, true)
		return nil
	}

	if _, err := r.cleaning(ctx, tenantID, true); err != nil {
		return fmt.Errorf("cleaning: %w (nothing was written)", err)
	}
	if _, err := r.feedback(ctx, tenantID, true); err != nil {
		return fmt.Errorf("feedback: %w (nothing was written)", err)
	}

	cleaned, err := r.cleaning(ctx, tenantID, false)
	if err != nil {
		return fmt.Errorf("cleaning: %w", err)
	}
	writeCleaningReprojectReport(w, cleaned, false)

	reviewed, err := r.feedback(ctx, tenantID, false)
	if err != nil {
		return fmt.Errorf("feedback (cleaning was already re-projected; run again to finish): %w", err)
	}
	writeFeedbackReprojectReport(w, reviewed, false)
	return nil
}

// reprojectLine renders one non-in_sync item: the action (prefixed "would" in
// a dry run), what it applies to, the status read and the stored edge IDs.
func reprojectLine(dryRun bool, action, kind, id, status string, edgeIDs, missing []string) string {
	var b strings.Builder
	b.WriteString("  ")
	if dryRun {
		b.WriteString("would ")
	}
	fmt.Fprintf(&b, "%s %s %s (status %s)", cell(action), kind, cell(id), cell(status))
	if len(edgeIDs) > 0 {
		fmt.Fprintf(&b, " edges: %s", cellList(edgeIDs))
	}
	if len(missing) > 0 {
		fmt.Fprintf(&b, " missing verdicts: %s", cellList(missing))
	}
	return b.String()
}

// cellList cleans each untrusted ID, since a stored ID can carry control
// characters, and joins them for one line.
func cellList(ss []string) string {
	cleaned := make([]string, len(ss))
	for i, s := range ss {
		cleaned[i] = cell(s)
	}
	return strings.Join(cleaned, ", ")
}

// writeCleaningReprojectReport renders a cleaning.ReprojectReport: the counts,
// then one line per finding that is not in sync.
func writeCleaningReprojectReport(w io.Writer, rep cleaning.ReprojectReport, dryRun bool) {
	fmt.Fprintf(w, "cleaning findings: retired %d, relinked %d, staled %d, in_sync %d\n", rep.Retired, rep.Relinked, rep.Staled, rep.InSync)
	for _, it := range rep.Items {
		if it.Action == cleaning.ReprojectInSync {
			continue
		}
		missing := make([]string, len(it.Missing))
		for i, k := range it.Missing {
			missing[i] = k.String()
		}
		fmt.Fprintln(w, reprojectLine(dryRun, string(it.Action), "finding", it.FindingID, string(it.Status), it.EdgeIDs, missing))
	}
}

// writeFeedbackReprojectReport renders a feedback.ReprojectReport like
// writeCleaningReprojectReport, per mapping review.
func writeFeedbackReprojectReport(w io.Writer, rep feedback.ReprojectReport, dryRun bool) {
	fmt.Fprintf(w, "mapping reviews: retired %d, relinked %d, staled %d, in_sync %d\n", rep.Retired, rep.Relinked, rep.Staled, rep.InSync)
	for _, it := range rep.Items {
		if it.Action == cleaning.ReprojectInSync {
			continue
		}
		fmt.Fprintln(w, reprojectLine(dryRun, string(it.Action), "mapping", it.MappingID, string(it.Status), it.EdgeIDs, nil))
	}
}

// daemonOpenCleaningReproject opens the app pool and the graph connection
// and builds the cleaning engine and feedback service over them, as the
// daemon does but with no CurrentFunc (re-projection needs no stage records)
// and no LLM client or NATS connection, so it runs on a host without an LLM
// gateway.
func daemonOpenCleaningReproject(ctx context.Context, cfg *config.Config) (cleaningReprojector, func(), error) {
	signer, err := newCleaningSigner(cfg)
	if err != nil {
		return cleaningReprojector{}, nil, err
	}
	shared, err := buildSharedResourcesFn(ctx, cfg, requiredResources{db: true, graph: true})
	if err != nil {
		return cleaningReprojector{}, nil, err
	}
	engine := newCleaningEngine(cfg, shared, nil, signer)
	service := newFeedbackService(cfg, shared, engine, signer)
	return cleaningReprojector{cleaning: engine.Reproject, feedback: service.Reproject}, shared.close, nil
}
