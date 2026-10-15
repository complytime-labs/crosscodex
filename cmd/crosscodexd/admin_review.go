package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/complytime-labs/crosscodex/internal/analyzer/artifacts"
	"github.com/complytime-labs/crosscodex/pkg/config"
	dbpkg "github.com/complytime-labs/crosscodex/pkg/db"
)

// humanVerdicts is what `admin adjudicate artifacts review|show|decide` needs: the
// verdict store's human methods and the adjudicator's projection pass.
type humanVerdicts interface {
	DecideHuman(ctx context.Context, tenantID, ref string, h artifacts.HumanVerdict) (artifacts.HumanDecision, error)
	ReviewQueue(ctx context.Context, tenantID string, n int) ([]artifacts.ReviewItem, error)
	SearchPairs(ctx context.Context, tenantID, name string, n int) ([]artifacts.ReviewItem, error)
	Project(ctx context.Context, tenantID string, now func() time.Time) (artifacts.AdjudicateResult, error)
	PairDetail(ctx context.Context, tenantID, ref string, n int) (artifacts.PairDetail, error)
}

// defaultReviewLimit is review's default --limit.
const defaultReviewLimit = 100

// reviewCellBytes caps each untrusted value printed by review.
const reviewCellBytes = 60

// showWrapWidth is the column at which show wraps justifications and errors.
const showWrapWidth = 100

// cell sanitizes one untrusted value for a single table cell or line.
func cell(s string) string { return artifacts.CleanDisplay(s, reviewCellBytes) }

// maxReviewerDisplayBytes caps the reviewer echoed by decide;
// ValidateReviewerID already bounds it to the same length.
const maxReviewerDisplayBytes = dbpkg.MaxReviewerBytes

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
	expect := fs.String("expect", "", "EXPECT token printed by show; decide refuses if the pair changed since")
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
	if err := dbpkg.ValidateReviewerID(*reviewer); err != nil {
		fmt.Fprintf(os.Stderr, "%s: --reviewer: %v\n", cmd, err)
		return 2
	}
	if *expect == "" {
		// *tenantID is not validated yet (runForTenants does that later) and is echoed, so clean it.
		fmt.Fprintf(os.Stderr, "%s: --expect is required: run \"crosscodexd admin adjudicate artifacts show --tenant %s --pair %s\", review the reasoning, and copy its EXPECT value\n",
			cmd, artifacts.CleanDisplay(*tenantID, reviewCellBytes), artifacts.CleanDisplay(*pair, reviewCellBytes))
		return 2
	}
	if err := artifacts.ValidateExpect(*expect); err != nil {
		fmt.Fprintf(os.Stderr, "%s: --expect: %v\n", cmd, err)
		return 2
	}

	return runHumanVerdicts(cmd, *tenantID, func(ctx context.Context, cfg *config.Config, hv humanVerdicts, id string) error {
		now := func() time.Time { return time.Now().UTC() }
		d, err := hv.DecideHuman(ctx, id, *pair, artifacts.HumanVerdict{Status: status, Reviewer: *reviewer, DecidedAt: now(), Expect: *expect})
		var changed *artifacts.PairChangedError
		switch {
		case errors.Is(err, artifacts.ErrPairNotFound):
			return fmt.Errorf("no pair with ref %s in tenant %s; nothing was recorded. Run \"crosscodexd admin adjudicate artifacts review --tenant %s\" to list pairs that need review, or \"crosscodexd admin adjudicate artifacts review --tenant %s --name <text>\" to find a pair the panel already decided", *pair, id, id, id)
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return errors.New("interrupted; nothing was recorded. Re-run the command")
		case errors.Is(err, artifacts.ErrPairRefAmbiguous):
			return fmt.Errorf("pair ref %s matches more than one pair in tenant %s; nothing was recorded. Please report this as a bug", *pair, id)
		case errors.As(err, &changed):
			at := "never"
			if !changed.DecidedAt.IsZero() {
				at = changed.DecidedAt.UTC().Format(time.RFC3339Nano)
			}
			return fmt.Errorf("pair %s changed since you viewed it: it is now %s by %s (%s) at %s; nothing was recorded. Run \"crosscodexd admin adjudicate artifacts show --tenant %s --pair %s\", review the current reasoning, and decide again with its EXPECT value",
				*pair, cell(string(changed.Status)), artifacts.CleanDisplay(changed.DisplayBy, maxReviewerDisplayBytes), cell(changed.DeterminationType), at, id, *pair)
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
	return runForTenants(ctx, cmd, cfg, tenantID, false, scopeActive, func(ctx context.Context) (tenantRunner, func(), error) {
		hv, closeFn, err := openHumanVerdictsFn(ctx, cfg)
		if err != nil {
			return nil, nil, err
		}
		return func(ctx context.Context, id string) error { return fn(ctx, cfg, hv, id) }, closeFn, nil
	})
}

// writeReviewQueue renders review's listing to w, or empty when there are no
// items. The VOTES column summarizes the current llm_panel vote split ("-"
// when none, "unreadable" when the stored evidence cannot be decoded). Names,
// type and dissent come from stored, untrusted values, so each goes through
// CleanDisplay, which also turns tabs into spaces so no value can shift
// tabwriter columns.
func writeReviewQueue(w io.Writer, items []artifacts.ReviewItem, empty string) {
	if len(items) == 0 {
		fmt.Fprintln(w, empty)
		return
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PAIR\tSTATUS\tBY\tVOTES\tTYPE\tLOW\tHIGH\tSCORE\tDISSENT")
	for _, it := range items {
		dissent := "-"
		if it.DissentStatus != "" {
			dissent = cell(string(it.DissentStatus))
		}
		votes := "-"
		switch {
		case it.VotesUnreadable:
			votes = "unreadable"
		case it.Votes != nil:
			votes = fmt.Sprintf("%d/%d same", it.Votes.Same, it.Votes.Valid)
			if failed := it.Votes.Total - it.Votes.Valid; failed > 0 {
				votes += fmt.Sprintf(" (%d failed)", failed)
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%.2f\t%s\n", it.Ref, it.Status, it.DeterminationType, votes,
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

// defaultRelatedLimit is show's default --related.
const defaultRelatedLimit = 20

// Display caps for show: no stored value is cut below the length the
// adjudicator stores (adjudicate.go maxJustificationChars, maxErrorChars).
const (
	showJustificationBytes = 2000
	showErrorBytes         = 1000
)

// showArtifactsCmd parses `admin adjudicate artifacts show` flags and prints
// one pair with everything a reviewer decides it from.
func showArtifactsCmd(args []string) int {
	const cmd = "admin adjudicate artifacts show"
	fs := flag.NewFlagSet("crosscodexd "+cmd, flag.ContinueOnError)
	tenantID := fs.String("tenant", "", "tenant ID that owns the pair")
	all := fs.Bool("all-tenants", false, "not supported: show is per tenant")
	pair := fs.String("pair", "", "pair ref from the PAIR column of review")
	related := fs.Int("related", defaultRelatedLimit, "most related pairs to list")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if !singleTenant(cmd, "show", *tenantID, *all) {
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
	if *related <= 0 {
		fmt.Fprintf(os.Stderr, "%s: --related must be positive\n", cmd)
		return 2
	}
	return runHumanVerdicts(cmd, *tenantID, func(ctx context.Context, _ *config.Config, hv humanVerdicts, id string) error {
		d, err := hv.PairDetail(ctx, id, *pair, *related)
		switch {
		case errors.Is(err, artifacts.ErrPairNotFound):
			return fmt.Errorf("no pair with ref %s in tenant %s; nothing was changed. Run \"crosscodexd admin adjudicate artifacts review --tenant %s\" to list pairs that need review, or \"crosscodexd admin adjudicate artifacts review --tenant %s --name <text>\" to find a pair the panel already decided", *pair, id, id, id)
		case errors.Is(err, artifacts.ErrPairRefAmbiguous):
			return fmt.Errorf("pair ref %s matches more than one pair in tenant %s; nothing was changed. Please report this as a bug", *pair, id)
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return errors.New("interrupted; nothing was changed. Re-run the command")
		case err != nil:
			return err
		}
		writePairDetail(os.Stdout, d, id)
		if d.EvidenceErr != nil {
			return fmt.Errorf("pair %s: stored evidence cannot be decoded: %s. The pair can still be decided; run decide with --expect %s", *pair, artifacts.CleanDisplay(d.EvidenceErr.Error(), showErrorBytes), d.Expect)
		}
		return nil
	})
}

// writePairDetail renders show's output. Every stored string is untrusted
// (artifact names, reviewer IDs, LLM output), so each goes through
// CleanDisplay; justifications and errors keep their stored length.
func writePairDetail(w io.Writer, d artifacts.PairDetail, tenantID string) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	kv := func(k, v string) { fmt.Fprintf(tw, "%s\t%s\n", k, v) }
	kv("PAIR", d.Ref)
	kv("TYPE", cell(d.Context.Type))
	kv("LOW", cell(d.Context.LowName))
	kv("HIGH", cell(d.Context.HighName))
	kv("SCORE", fmt.Sprintf("%.2f", d.SimilarityScore))
	kv("STATUS", cell(string(d.Status)))
	by := cell(d.DisplayBy)
	if by == "" {
		by = "-"
	}
	kv("BY", cell(d.DeterminationType)+" / "+by)
	confidence, decided := "-", "-"
	if d.Confidence != nil {
		confidence = fmt.Sprintf("%.2f", *d.Confidence)
	}
	if !d.DecidedAt.IsZero() {
		decided = d.DecidedAt.UTC().Format(time.RFC3339Nano)
	}
	kv("CONFIDENCE", confidence)
	kv("DECIDED", decided)
	if d.Evidence != nil {
		kv("PROMPT", cell(d.Evidence.PromptName)+" v"+cell(d.Evidence.PromptVersion))
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(w, "error flushing output: %v\n", err)
	}
	if d.EvidenceErr != nil {
		fmt.Fprintf(w, "\nVOTES\n%s\n", wrapIndent("unreadable: "+artifacts.CleanDisplay(d.EvidenceErr.Error(), showErrorBytes), "  ", showWrapWidth))
	} else if d.Evidence != nil {
		s := d.Evidence.Split()
		fmt.Fprintf(w, "\nVOTES (%d same, %d distinct, %d failed; %d valid of %d)\n", s.Same, s.Valid-s.Same, s.Total-s.Valid, s.Valid, s.Total)
		writeVotes(w, d.Evidence.Votes)
	}
	writeDissent(w, d)
	if d.LastError != "" {
		fmt.Fprintf(w, "\nLAST ERROR\n%s\n", wrapIndent(artifacts.CleanDisplay(d.LastError, showErrorBytes), "  ", showWrapWidth))
	}
	if d.RelatedTotal == 0 {
		fmt.Fprint(w, "\nRELATED PAIRS\n  none\n")
	} else {
		fmt.Fprintf(w, "\nRELATED PAIRS (showing %d of %d that share a group)\n", len(d.Related), d.RelatedTotal)
		rw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		for _, r := range d.Related {
			fmt.Fprintf(rw, "  %s\t%s\t%s\t%s | %s\n", r.Ref, cell(string(r.Status)), cell(r.DeterminationType), cell(r.LowName), cell(r.HighName))
		}
		if err := rw.Flush(); err != nil {
			fmt.Fprintf(w, "error flushing output: %v\n", err)
		}
	}
	fmt.Fprintf(w, "\nEXPECT    %s\n", d.Expect)
	fmt.Fprintf(w, "To decide: crosscodexd admin adjudicate artifacts decide --tenant %s --pair %s --verdict <confirmed|rejected> --reviewer <id> --expect %s\n",
		tenantID, d.Ref, d.Expect)
}

// writeDissent renders the panel's dissent from a human verdict. A dissent
// record that could not be decoded has no status, time or reviewer, so only
// the error is shown; Disputed still comes from the pair row and is honored.
func writeDissent(w io.Writer, d artifacts.PairDetail) {
	fmt.Fprint(w, "\nPANEL DISSENT")
	dis := d.Dissent
	if dis == nil {
		fmt.Fprint(w, "\n  none\n")
		return
	}
	unreadable := dis.DecidedAt.IsZero()
	heading := "overridden by the human verdict"
	switch {
	case d.Disputed:
		heading = "given after the human verdict (disputed)"
	case unreadable:
		heading = "unreadable"
	case dis.DecidedAt.After(d.DecidedAt):
		heading = "given after the human verdict (agrees)"
	}
	fmt.Fprintf(w, ": %s\n", heading)
	if unreadable {
		status := cell(string(dis.Status))
		if status == "" {
			status = "-"
		}
		fmt.Fprintf(w, "  %s\n", status)
		if dis.EvidenceErr != nil {
			fmt.Fprintln(w, wrapIndent("dissent not readable: "+artifacts.CleanDisplay(dis.EvidenceErr.Error(), showErrorBytes), "  ", showWrapWidth))
		}
		return
	}
	prompt := ""
	if dis.Evidence != nil {
		prompt = ", " + cell(dis.Evidence.PromptName) + " v" + cell(dis.Evidence.PromptVersion)
	}
	fmt.Fprintf(w, "  %s by %s at %s (confidence %.2f%s)\n", cell(string(dis.Status)), cell(dis.DeterminedBy),
		dis.DecidedAt.UTC().Format(time.RFC3339Nano), dis.Confidence, prompt)
	if dis.EvidenceErr != nil {
		fmt.Fprintln(w, wrapIndent("evidence not readable: "+artifacts.CleanDisplay(dis.EvidenceErr.Error(), showErrorBytes), "  ", showWrapWidth))
	} else if dis.Evidence != nil {
		writeVotes(w, dis.Evidence.Votes)
	}
}

// writeVotes renders votes, one block per vote: voter, decision and
// confidence on one line, then the justification indented on its own line.
func writeVotes(w io.Writer, votes []artifacts.VoteRecord) {
	for _, v := range votes {
		decision := "failed"
		if v.Decision != nil {
			decision = "distinct"
			if *v.Decision {
				decision = "same"
			}
		}
		confidence := cell(v.Confidence)
		if confidence == "" {
			confidence = "-"
		}
		fmt.Fprintf(w, "  %s  %s  %s\n", cell(v.VoterID), decision, confidence)
		if j := wrapIndent(artifacts.CleanDisplay(v.Justification, showJustificationBytes), "    ", showWrapWidth); j != "" {
			fmt.Fprintln(w, j)
		}
	}
}

// wrapIndent splits s into words and packs them into lines of at most width
// columns including indent, each line prefixed with indent and joined by
// "\n" (no trailing newline). A word longer than the line gets its own
// unbroken line. It only inserts whitespace between words, so callers pass
// text already sanitized by CleanDisplay.
func wrapIndent(s, indent string, width int) string {
	var lines []string
	line := ""
	for _, word := range strings.Fields(s) {
		switch {
		case line == "":
			line = indent + word
		case len(line)+1+len(word) <= width:
			line += " " + word
		default:
			lines = append(lines, line)
			line = indent + word
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}
