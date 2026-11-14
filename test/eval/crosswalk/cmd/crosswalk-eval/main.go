// Command crosswalk-eval prepares, inspects and scores a crosswalk evaluation
// (#260). It reads catalog and result files only. Exit codes: 0 ok, 1 input or
// validation error, 2 usage.
//
//	crosswalk-eval subset   --root R --profile P --out FILE   (prints the absolute path of the profile's target catalog)
//	crosswalk-eval discover --source S --target T [--json]
//	crosswalk-eval score    --root R --profile P --subset FILE --mappings M
//	                        --candidates C --status S [--baseline B] [--label L] --out DIR
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/complytime-labs/crosscodex/test/eval/crosswalk"
)

// commit is set at build time: -ldflags "-X main.commit=<sha>".
var commit = "unknown"

const (
	exitOK       = 0
	exitInput    = 1
	exitUsage    = 2
	dirMode      = 0o755
	fileMode     = 0o644
	topUsage     = "Usage: crosswalk-eval <subset|discover|score> [flags]"
	notAvailable = "n/a"
	unlabelled   = "unlabelled"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, topUsage)
		return exitUsage
	}
	var err error
	switch args[0] {
	case "subset":
		err = runSubset(args[1:], stdout, stderr)
	case "discover":
		err = runDiscover(args[1:], stdout, stderr)
	case "score":
		err = runScore(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprintln(stderr, topUsage)
		return exitOK
	default:
		fmt.Fprintf(stderr, "unknown subcommand %q\n%s\n", args[0], topUsage)
		return exitUsage
	}
	var ue *usageError
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, flag.ErrHelp):
		return exitOK
	case errors.As(err, &ue):
		return exitUsage
	default:
		fmt.Fprintf(stderr, "crosswalk-eval %s: %v\n", args[0], err)
		return exitInput
	}
}

// usageError marks an error already reported on stderr with the subcommand's
// usage, so run only maps it to the usage exit code.
type usageError struct{}

func (*usageError) Error() string { return "usage error" }

// parse parses args into fs and requires every named flag to be non-empty.
func parse(fs *flag.FlagSet, args []string, stderr io.Writer, required map[string]*string) error {
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return &usageError{}
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected argument %q\n", fs.Arg(0))
		fs.Usage()
		return &usageError{}
	}
	for name, v := range required {
		if *v == "" {
			fmt.Fprintf(stderr, "missing required flag --%s\n", name)
			fs.Usage()
			return &usageError{}
		}
	}
	return nil
}

// rooted resolves a repo-root-relative path under root, refusing a path that
// escapes it.
func rooted(root, rel, what string) (string, error) {
	if !filepath.IsLocal(rel) {
		return "", fmt.Errorf("%s path %q must be relative to the repository root and stay inside it (no absolute paths or ..)", what, rel)
	}
	return filepath.Join(root, rel), nil
}

// loaded is a profile with its source and target catalogs.
type loaded struct {
	profile                *crosswalk.Profile
	source, target         *crosswalk.Catalog
	sourcePath, targetPath string
}

func loadProfile(root, profilePath string) (*loaded, error) {
	p, err := crosswalk.LoadProfile(profilePath)
	if err != nil {
		return nil, err
	}
	l := &loaded{profile: p}
	if l.sourcePath, err = rooted(root, p.Source, "profile source"); err != nil {
		return nil, err
	}
	if l.targetPath, err = rooted(root, p.Target, "profile target"); err != nil {
		return nil, err
	}
	if p.References.CSV != "" {
		if _, err := rooted(root, p.References.CSV, "profile csv"); err != nil {
			return nil, err
		}
	}
	if l.source, err = crosswalk.LoadCatalog(l.sourcePath); err != nil {
		return nil, err
	}
	if l.target, err = crosswalk.LoadCatalog(l.targetPath); err != nil {
		return nil, err
	}
	return l, nil
}

func writeFile(path string, data []byte) error {
	if err := os.WriteFile(path, data, fileMode); err != nil {
		return fmt.Errorf("writing %q: %w", path, err)
	}
	return nil
}

// runSubset writes the subset catalog to --out and prints the absolute path of
// the profile's target catalog on stdout, so the caller imports exactly the
// file this tool validated and hashed instead of re-parsing the profile.
func runSubset(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("subset", flag.ContinueOnError)
	root := fs.String("root", ".", "repository root that profile paths are relative to")
	profile := fs.String("profile", "", "profile YAML (required)")
	out := fs.String("out", "", "subset catalog JSON to write (required)")
	if err := parse(fs, args, stderr, map[string]*string{"profile": profile, "out": out}); err != nil {
		return err
	}
	l, err := loadProfile(*root, *profile)
	if err != nil {
		return err
	}
	sub, err := crosswalk.Subset(l.source, l.profile.Controls)
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(map[string]any{"catalog": sub}, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding subset: %w", err)
	}
	if err := writeFile(*out, append(raw, '\n')); err != nil {
		return err
	}
	target, err := filepath.Abs(l.targetPath)
	if err != nil {
		return fmt.Errorf("resolving target catalog path %q: %w", l.targetPath, err)
	}
	fmt.Fprintln(stdout, target)
	return nil
}

func runDiscover(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("discover", flag.ContinueOnError)
	source := fs.String("source", "", "source OSCAL catalog (required)")
	target := fs.String("target", "", "target OSCAL catalog (required)")
	asJSON := fs.Bool("json", false, "print rows as JSON")
	if err := parse(fs, args, stderr, map[string]*string{"source": source, "target": target}); err != nil {
		return err
	}
	src, err := crosswalk.LoadCatalog(*source)
	if err != nil {
		return err
	}
	tgt, err := crosswalk.LoadCatalog(*target)
	if err != nil {
		return err
	}
	rows := crosswalk.Discover(src, crosswalk.NewTargetIndex(tgt))
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	tw := tabwriter.NewWriter(stdout, 0, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "REL\tRESOURCE\tUUID\tMODE\tLINKS\tIDENTIFIERS\tRESOLVED\tRATE")
	for _, r := range rows {
		rate := 0.0
		if r.Identifiers > 0 {
			rate = 100 * float64(r.Resolved) / float64(r.Identifiers)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%d\t%d\t%.1f%%\n",
			r.Rel, r.ResourceTitle, r.ResourceUUID, r.Mode, r.Links, r.Identifiers, r.Resolved, rate)
	}
	return tw.Flush()
}

func runScore(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("score", flag.ContinueOnError)
	root := fs.String("root", ".", "repository root that profile paths are relative to")
	profile := fs.String("profile", "", "profile YAML (required)")
	subset := fs.String("subset", "", "subset catalog the job ran on (required)")
	mappings := fs.String("mappings", "", "mappings export JSON (required)")
	candidates := fs.String("candidates", "", "candidates export JSON (required)")
	status := fs.String("status", "", "job status JSON (required)")
	baseline := fs.String("baseline", "", "report.json of an earlier run, for agreement")
	labels := fs.String("labels", "", "labels YAML of judged candidate pairs, for labelled precision")
	label := fs.String("label", "", "name for this run in the report")
	out := fs.String("out", "", "directory for report.json and report.md (required)")
	if err := parse(fs, args, stderr, map[string]*string{
		"profile": profile, "subset": subset, "mappings": mappings,
		"candidates": candidates, "status": status, "out": out,
	}); err != nil {
		return err
	}

	l, err := loadProfile(*root, *profile)
	if err != nil {
		return err
	}
	sub, err := crosswalk.LoadCatalog(*subset)
	if err != nil {
		return err
	}
	run, err := crosswalk.LoadRun(*mappings, *candidates, *status)
	if err != nil {
		return err
	}
	var base *crosswalk.Report
	if *baseline != "" {
		if base, err = crosswalk.LoadReport(*baseline); err != nil {
			return err
		}
	}
	var labelled map[crosswalk.Pair]crosswalk.Label
	var labelsDigest string
	if *labels != "" {
		if labelled, err = crosswalk.LoadLabels(*labels); err != nil {
			return err
		}
		if labelsDigest, err = crosswalk.LabelsDigest(*labels); err != nil {
			return err
		}
	}
	raw, err := crosswalk.ExtractReferences(l.profile, *root, l.source)
	if err != nil {
		return err
	}
	csvDigest, err := crosswalk.CSVDigest(l.profile, *root)
	if err != nil {
		return err
	}
	refs := crosswalk.ResolveReferences(raw, crosswalk.NewTargetIndex(l.target))
	rep, err := crosswalk.Score(crosswalk.ScoreInput{
		Profile: l.profile, Source: l.source, Subset: sub, Target: l.target,
		Refs: refs, Run: run, Baseline: base, CSVDigest: csvDigest, Label: *label, Commit: commit, Labels: labelled, LabelsDigest: labelsDigest,
	})
	if err != nil {
		return err
	}

	js, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding report: %w", err)
	}
	if err := os.MkdirAll(*out, dirMode); err != nil {
		return fmt.Errorf("creating output directory %q: %w", *out, err)
	}
	if err := writeFile(filepath.Join(*out, "report.json"), append(js, '\n')); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(*out, "report.md"), []byte(rep.Markdown())); err != nil {
		return err
	}
	fmt.Fprintln(stdout, summary(rep))
	return nil
}

// summary is the one-line result Task 9's run script prints.
func summary(r *crosswalk.Report) string {
	n := len(r.References)
	rc := r.Recall
	calls, wall := notAvailable, notAvailable
	if r.Cost.TotalSeconds != nil {
		wall = fmt.Sprintf("%.1fs", *r.Cost.TotalSeconds)
	}
	if total, ok := r.Cost.ChatCalls(); ok {
		calls = fmt.Sprint(total)
	}
	label := r.Provenance.Label
	if label == "" {
		label = unlabelled
	}
	return fmt.Sprintf("%s %s: refs %d (resolved %d/%d), found exact %d/%d base %d/%d, reachable exact %d/%d base %d/%d, related %d, chat calls %s, wall %s",
		r.Provenance.Profile.Name, label, n, r.Resolution.Hit, r.Resolution.Total,
		rc.FoundExact.Hit, n, rc.FoundBase.Hit, n, rc.ReachableExact.Hit, n, rc.ReachableBase.Hit, n,
		len(r.Related), calls, wall)
}
