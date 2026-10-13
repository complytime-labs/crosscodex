package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/complytime-labs/crosscodex/internal/analyzer/artifacts"
	"github.com/complytime-labs/crosscodex/pkg/config"
	dbpkg "github.com/complytime-labs/crosscodex/pkg/db"
)

// humanVerdicts is what `admin adjudicate artifacts review|decide` needs: the
// verdict store's human methods and the adjudicator's projection pass.
type humanVerdicts interface {
	DecideHuman(ctx context.Context, tenantID, ref string, h artifacts.HumanVerdict) (artifacts.HumanDecision, error)
	ReviewQueue(ctx context.Context, tenantID string, n int) ([]artifacts.ReviewItem, error)
	SearchPairs(ctx context.Context, tenantID, name string, n int) ([]artifacts.ReviewItem, error)
	Project(ctx context.Context, tenantID string, now func() time.Time) (artifacts.AdjudicateResult, error)
}

// defaultReviewLimit is review's default --limit.
const defaultReviewLimit = 100

// reviewCellBytes caps each untrusted value printed by review.
const reviewCellBytes = 60

// maxReviewerDisplayBytes caps the reviewer echoed by decide; ValidateReviewer
// already bounds it to the same length.
const maxReviewerDisplayBytes = 256

// openHumanVerdictsFn opens the app pool and graph and returns humanVerdicts
// over them plus the func that closes them. A package var so flag parsing and
// output can be unit-tested with a fake; production is daemonOpenHumanVerdicts.
var openHumanVerdictsFn = daemonOpenHumanVerdicts

// daemonHumanVerdicts joins the PG verdict store with an Adjudicator used only
// for its projection pass (no LLM client).
type daemonHumanVerdicts struct {
	*artifacts.PGVerdictStore
	adj *artifacts.Adjudicator
}

func (d daemonHumanVerdicts) Project(ctx context.Context, tenantID string, now func() time.Time) (artifacts.AdjudicateResult, error) {
	return d.adj.Project(ctx, tenantID, now)
}

// daemonOpenHumanVerdicts opens only the database and the graph: neither
// review nor decide calls the LLM or publishes to NATS. cfg is the loader's
// output, so analysis.artifact_adjudication carries its defaults even when
// adjudication is disabled; Project uses only lease_duration from it, which
// the loader validates either way.
func daemonOpenHumanVerdicts(ctx context.Context, cfg *config.Config) (humanVerdicts, func(), error) {
	shared, err := buildSharedResourcesFn(ctx, cfg, requiredResources{db: true, graph: true})
	if err != nil {
		return nil, nil, err
	}
	// No-op today: admin subcommands run before telemetry.Init (see docs/dev/telemetry.md).
	tp, mp := otel.GetTracerProvider(), otel.GetMeterProvider()
	store := artifacts.NewPGVerdictStore(dbpkg.NewTenantPool(shared.appPool), artifacts.WithVerdictStoreTelemetry(tp, mp))
	adj := artifacts.NewAdjudicator(shared.graphDB_, store, nil, nil, cfg.Analysis.ArtifactAdjudication,
		artifacts.WithAdjudicatorTelemetry(tp, mp))
	return daemonHumanVerdicts{PGVerdictStore: store, adj: adj}, shared.close, nil
}

// singleTenant checks review's and decide's tenant flags, printing why they
// are unusable. --all-tenants is defined only to explain that these commands
// work on one tenant. The caller returns 2 on false.
func singleTenant(cmd, verb, tenantID string, all bool) bool {
	if all {
		fmt.Fprintf(os.Stderr, "%s: %s needs --tenant, not --all-tenants: review and decide work on one tenant at a time, so pass --tenant <id>\n", cmd, verb)
		return false
	}
	if tenantID == "" {
		fmt.Fprintf(os.Stderr, "%s: --tenant is required\n", cmd)
		return false
	}
	return true
}

// reviewArtifactsCmd parses `admin adjudicate artifacts review` flags and
// lists the tenant's pairs that need a human or, with --name, the pairs of
// any status whose name contains the text, so a panel-decided pair's ref can
// be found for decide.
func reviewArtifactsCmd(args []string) int {
	const cmd = "admin adjudicate artifacts review"
	fs := flag.NewFlagSet("crosscodexd "+cmd, flag.ContinueOnError)
	tenantID := fs.String("tenant", "", "tenant ID whose pairs to list")
	all := fs.Bool("all-tenants", false, "not supported: review is per tenant")
	limit := fs.Int("limit", defaultReviewLimit, "most pairs to list")
	name := fs.String("name", "", "list pairs in any status whose LOW or HIGH name contains this text (case-insensitive)")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if !singleTenant(cmd, "review", *tenantID, *all) {
		return 2
	}
	if *limit <= 0 {
		fmt.Fprintf(os.Stderr, "%s: --limit must be positive\n", cmd)
		return 2
	}
	// Visit, not *name != "": an explicit --name "" is a usage error, not a
	// request for the review queue.
	search := false
	fs.Visit(func(f *flag.Flag) { search = search || f.Name == "name" })
	if search {
		if err := artifacts.ValidateSearchName(*name); err != nil {
			fmt.Fprintf(os.Stderr, "%s: --name: %v\n", cmd, err)
			return 2
		}
	}
	return runHumanVerdicts(cmd, *tenantID, func(ctx context.Context, _ *config.Config, hv humanVerdicts, id string) error {
		if search {
			items, err := hv.SearchPairs(ctx, id, *name, *limit)
			if err != nil {
				return err
			}
			writeReviewQueue(os.Stdout, items, fmt.Sprintf("no pairs match %q", artifacts.CleanDisplay(*name, artifacts.MaxSearchNameBytes)))
			return nil
		}
		items, err := hv.ReviewQueue(ctx, id, *limit)
		if err != nil {
			return err
		}
		writeReviewQueue(os.Stdout, items, "no pairs need review")
		return nil
	})
}

// decideArtifactsCmd parses `admin adjudicate artifacts decide` flags,
// records a human verdict on one pair and projects it to the graph.
func decideArtifactsCmd(args []string) int {
	const cmd = "admin adjudicate artifacts decide"
	fs := flag.NewFlagSet("crosscodexd "+cmd, flag.ContinueOnError)
	tenantID := fs.String("tenant", "", "tenant ID that owns the pair")
	all := fs.Bool("all-tenants", false, "not supported: decide is per tenant")
	pair := fs.String("pair", "", "pair ref from the PAIR column of review")
	verdict := fs.String("verdict", "", "confirmed or rejected")
	reviewer := fs.String("reviewer", "", "ID of the person recording this verdict, stored as determined_by")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if !singleTenant(cmd, "decide", *tenantID, *all) {
		return 2
	}
	if *pair == "" {
		fmt.Fprintf(os.Stderr, "%s: --pair is required: copy the PAIR column from \"crosscodexd admin adjudicate artifacts review --tenant <id>\"\n", cmd)
		return 2
	}
	if err := artifacts.ValidatePairRef(*pair); err != nil {
		fmt.Fprintf(os.Stderr, "%s: --pair: %v\n", cmd, err)
		return 2
	}
	status := artifacts.VerdictStatus(*verdict)
	switch {
	case *verdict == "":
		fmt.Fprintf(os.Stderr, "%s: --verdict is required: use confirmed or rejected\n", cmd)
		return 2
	case status != artifacts.VerdictConfirmed && status != artifacts.VerdictRejected:
		fmt.Fprintf(os.Stderr, "%s: --verdict %q is not a human verdict: use confirmed or rejected\n", cmd, *verdict)
		return 2
	}
	if *reviewer == "" {
		fmt.Fprintf(os.Stderr, "%s: --reviewer is required: pass the ID of the person recording this verdict; it is stored as determined_by\n", cmd)
		return 2
	}
	if err := artifacts.ValidateReviewer(*reviewer); err != nil {
		fmt.Fprintf(os.Stderr, "%s: --reviewer: %v\n", cmd, err)
		return 2
	}

	return runHumanVerdicts(cmd, *tenantID, func(ctx context.Context, cfg *config.Config, hv humanVerdicts, id string) error {
		now := func() time.Time { return time.Now().UTC() }
		d, err := hv.DecideHuman(ctx, id, *pair, artifacts.HumanVerdict{Status: status, Reviewer: *reviewer, DecidedAt: now()})
		switch {
		case errors.Is(err, artifacts.ErrPairNotFound):
			return fmt.Errorf("no pair with ref %s in tenant %s; nothing was recorded. Run \"crosscodexd admin adjudicate artifacts review --tenant %s\" to list pairs that need review, or \"crosscodexd admin adjudicate artifacts review --tenant %s --name <text>\" to find a pair the panel already decided", *pair, id, id, id)
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return errors.New("interrupted; nothing was recorded. Re-run the command")
		case errors.Is(err, artifacts.ErrPairRefAmbiguous):
			return fmt.Errorf("pair ref %s matches more than one pair in tenant %s; nothing was recorded. Please report this as a bug", *pair, id)
		case err != nil:
			return err
		}
		by := artifacts.CleanDisplay(*reviewer, maxReviewerDisplayBytes)
		switch d.Outcome {
		case artifacts.HumanUnchanged:
			fmt.Fprintf(os.Stdout, "pair %s: unchanged: already %s by a human; determined_by was not changed\n", *pair, status)
		case artifacts.HumanReaffirmed:
			fmt.Fprintf(os.Stdout, "pair %s: %s by %s (re-affirmed over the panel's later dissent)\n", *pair, status, by)
		default:
			fmt.Fprintf(os.Stdout, "pair %s: %s by %s (recorded)\n", *pair, status, by)
		}
		if !d.ClaimedUntil.IsZero() {
			// An unchanged verdict was recorded by an earlier decide, not now.
			stored := "the verdict is recorded"
			if d.Outcome == artifacts.HumanUnchanged {
				stored = "the verdict is in the store"
			}
			fmt.Fprintf(os.Stdout, "pair %s: %s, but an adjudicator run is projecting this pair; it reaches the graph when that run finishes, or by re-running this decide command after %s\n",
				*pair, stored, d.ClaimedUntil.UTC().Format(time.RFC3339))
		}
		res, err := hv.Project(ctx, id, now)
		writeProjectReport(os.Stdout, res)
		if err != nil {
			// A pass that failed after a graph write keeps its claim on
			// the pair for lease_duration; a re-run before then skips it.
			return fmt.Errorf("verdict recorded but not yet in the graph: %w. To finish, re-run this command after %s (a failed projection may hold the pair for analysis.artifact_adjudication.lease_duration); it will not record the verdict twice",
				err, now().Add(cfg.Analysis.ArtifactAdjudication.LeaseDuration).Format(time.RFC3339))
		}
		return nil
	})
}

// runHumanVerdicts loads the config and runs fn for one tenant on
// connections opened once. It does not require
// analysis.artifact_adjudication.enabled: a human verdict needs no panel, and
// an operator who turned the panel off still needs to resolve its leftovers.
// runForTenants fails a malformed tenant before any connection is opened.
// SIGINT/SIGTERM cancel it. It returns a process exit code.
func runHumanVerdicts(cmd, tenantID string, fn func(ctx context.Context, cfg *config.Config, hv humanVerdicts, id string) error) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg, ok := loadAdminConfig(ctx, cmd)
	if !ok {
		return 1
	}
	return runForTenants(ctx, cmd, cfg, tenantID, false, func(ctx context.Context) (tenantRunner, func(), error) {
		hv, closeFn, err := openHumanVerdictsFn(ctx, cfg)
		if err != nil {
			return nil, nil, err
		}
		return func(ctx context.Context, id string) error { return fn(ctx, cfg, hv, id) }, closeFn, nil
	})
}

// writeReviewQueue renders review's listing to w, or empty when there are no
// items. Names, type and dissent come from stored, untrusted values, so each
// goes through CleanDisplay, which also turns tabs into spaces so no value can
// shift tabwriter columns.
func writeReviewQueue(w io.Writer, items []artifacts.ReviewItem, empty string) {
	if len(items) == 0 {
		fmt.Fprintln(w, empty)
		return
	}
	cell := func(s string) string { return artifacts.CleanDisplay(s, reviewCellBytes) }
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PAIR\tSTATUS\tBY\tTYPE\tLOW\tHIGH\tSCORE\tDISSENT")
	for _, it := range items {
		dissent := "-"
		if it.DissentStatus != "" {
			dissent = cell(string(it.DissentStatus))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%.2f\t%s\n", it.Ref, it.Status, it.DeterminationType,
			cell(it.Context.Type), cell(it.Context.LowName), cell(it.Context.HighName), it.SimilarityScore, dissent)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(w, "error flushing output: %v\n", err)
	}
}

// writeProjectReport renders the counters of decide's projection pass to w.
func writeProjectReport(w io.Writer, res artifacts.AdjudicateResult) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "METRIC\tVALUE")
	fmt.Fprintf(tw, "recovered\t%d\n", res.Recovered)
	fmt.Fprintf(tw, "orphaned\t%d\n", res.Orphaned)
	fmt.Fprintf(tw, "fenced\t%d\n", res.Fenced)
	fmt.Fprintf(tw, "busy\t%d\n", res.Busy)
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(w, "error flushing output: %v\n", err)
	}
}
