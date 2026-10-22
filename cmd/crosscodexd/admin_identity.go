package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/complytime-labs/crosscodex/internal/analyzer/artifacts"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/config"
	dbpkg "github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
)

// Seams: specs replace these to exercise parsing, signing and output
// without a database.
var (
	runIdentityActionFn = daemonIdentityAction
	runReviewerListFn   = daemonReviewerList
	runTraceFn          = daemonTrace
	runIdentityExportFn = daemonIdentityExport
)

// identityResult is what a tenant or reviewer action returns: the
// pseudonym of a hide, or whether an add registered a new reviewer.
type identityResult struct {
	Pseudonym string
	Created   bool
}

// identityCell sanitizes a stored identity value (reviewer ID, operator,
// pseudonym) for one table cell. Reviewer IDs are not pattern-checked like
// tenant IDs, so format characters are removed as well as control ones.
func identityCell(s string) string { return artifacts.CleanDisplay(s, maxReviewerDisplayBytes) }

// newIdentitySigner builds the generator that signs identity requests. It
// fails closed: an identity change without its signed record is refused.
func newIdentitySigner(cmd string, cfg *config.Config) (attestation.Generator, bool) {
	if cfg.Attestation.PrivateKeyPath == "" || cfg.Attestation.PublicKeyPath == "" {
		fmt.Fprintf(os.Stderr, "%s requires attestation keys because every identity change is signed. Set attestation.private_key_path and attestation.public_key_path.\n", cmd)
		return nil, false
	}
	tr, mt := telemetry.Instrumentation("pkg/attestation")
	gen, err := attestation.NewGenerator(&attestation.FileKeyProvider{
		PrivateKeyPath: cfg.Attestation.PrivateKeyPath,
		PublicKeyPath:  cfg.Attestation.PublicKeyPath,
	}, attestation.WithTelemetry(tr, mt), attestation.WithFIPSMode(cfg.TLS.FIPS.Enabled))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", cmd, err)
		return nil, false
	}
	return gen, true
}

// signIdentityRequest signs req's canonical JSON as the single material of
// an identity.<action> link. The database stores both, so a verifier needs
// only the event row and the public key.
func signIdentityRequest(ctx context.Context, gen attestation.Generator, req dbpkg.IdentityRequest) (dbpkg.SignedRequest, error) {
	body, err := req.Canonical()
	if err != nil {
		return dbpkg.SignedRequest{}, err
	}
	sum := sha256.Sum256(body)
	link, err := gen.CreateLink(ctx, "identity."+req.Action,
		[]attestation.Artifact{{URI: "crosscodex:identity-request", Digest: hex.EncodeToString(sum[:])}}, nil)
	if err != nil {
		return dbpkg.SignedRequest{}, fmt.Errorf("sign identity request: %w", err)
	}
	return dbpkg.SignedRequest{Request: body, Attestation: link.Raw}, nil
}

// runSigned loads config, signs req as of now and runs fn with the
// tenant_admin DSN and the signed request, printing any failure. It refuses
// before fn when the DSN or the attestation keys are missing, or signing
// fails, so no identity change reaches the database unsigned. It returns a
// process exit code.
func runSigned(cmd string, req dbpkg.IdentityRequest, fn func(ctx context.Context, dsn string, req dbpkg.IdentityRequest, s dbpkg.SignedRequest) error) int {
	ctx := context.Background()
	cfg, ok := loadAdminConfig(ctx, cmd)
	if !ok {
		return 1
	}
	dsn, ok := requireTenantAdminDSN(cmd, cfg)
	if !ok {
		return 1
	}
	gen, ok := newIdentitySigner(cmd, cfg)
	if !ok {
		return 1
	}
	req.RequestedAt = time.Now().UTC()
	s, err := signIdentityRequest(ctx, gen, req)
	if err == nil {
		err = fn(ctx, dsn, req, s)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", cmd, err)
		return 1
	}
	return 0
}

// auditFlags defines --by and --reason, which every signed command takes.
func auditFlags(fs *flag.FlagSet) (by, reason *string) {
	by = fs.String("by", "", "ID of the operator making this change, kept in the audit log (required)")
	reason = fs.String("reason", "", "why this change is made, kept in the audit log (required)")
	return by, reason
}

// auditValid validates --by and --reason, printing why they are unusable.
func auditValid(cmd, by, reason string) bool {
	if err := dbpkg.ValidateActor(by); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", cmd, err)
		return false
	}
	if err := dbpkg.ValidateReason(reason); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", cmd, err)
		return false
	}
	return true
}

// identityActionCmd runs `admin tenant <verb>` (delete, restore, hide,
// unhide) or `admin reviewer <verb>` (add, delete, restore, hide, unhide):
// it validates the flags, signs the request and applies it.
func identityActionCmd(args []string, kind, verb string) int {
	cmd := "admin " + kind + " " + verb
	fs := flag.NewFlagSet("crosscodexd "+cmd, flag.ContinueOnError)
	tenantID := fs.String("tenant", "", "ID of the tenant (required)")
	var reviewer *string
	if kind == dbpkg.IdentityKindReviewer {
		reviewer = fs.String("reviewer", "", "ID of the reviewer to "+verb+" (required)")
	}
	by, reason := auditFlags(fs)
	if code, ok := parseNoArgs(fs, cmd, args); !ok {
		return code
	}
	if !tenantFlag(cmd, *tenantID) {
		return 2
	}
	identity := *tenantID
	if reviewer != nil {
		if err := dbpkg.ValidateReviewerID(*reviewer); err != nil {
			fmt.Fprintf(os.Stderr, "%s: --reviewer: %v\n", cmd, err)
			return 2
		}
		identity = *reviewer
	}
	if !auditValid(cmd, *by, *reason) {
		return 2
	}
	req := dbpkg.IdentityRequest{Action: verb, Kind: kind, TenantID: *tenantID, IdentityID: identity, Actor: *by, Reason: *reason}
	return runSigned(cmd, req, func(ctx context.Context, dsn string, req dbpkg.IdentityRequest, s dbpkg.SignedRequest) error {
		res, err := runIdentityActionFn(ctx, dsn, req, s)
		if err != nil {
			return err
		}
		switch {
		case verb == "delete":
			fmt.Fprintln(os.Stdout, "deleted")
		case verb == "restore" && kind == dbpkg.IdentityKindTenant:
			fmt.Fprintf(os.Stdout, "restored; the tenant is suspended. Run 'crosscodexd admin tenant resume --tenant %s' to reactivate it\n", *tenantID)
		case verb == "restore":
			fmt.Fprintln(os.Stdout, "restored")
		case verb == "hide":
			fmt.Fprintln(os.Stdout, "hidden as "+identityCell(res.Pseudonym))
		case verb == "unhide":
			fmt.Fprintln(os.Stdout, "unhidden")
		case res.Created:
			fmt.Fprintln(os.Stdout, "added")
		default:
			fmt.Fprintln(os.Stdout, "already registered")
		}
		return nil
	})
}

// identityTraceCmd runs `admin identity trace`: it signs the lookup, which
// the database records, and prints the identity behind the pseudonym.
func identityTraceCmd(args []string) int {
	const cmd = "admin identity trace"
	fs := flag.NewFlagSet("crosscodexd "+cmd, flag.ContinueOnError)
	pseudonym := fs.String("pseudonym", "", "pseudonym to resolve, as printed by hide or list (required)")
	by, reason := auditFlags(fs)
	if code, ok := parseNoArgs(fs, cmd, args); !ok {
		return code
	}
	if err := dbpkg.ValidatePseudonym(*pseudonym); err != nil {
		fmt.Fprintf(os.Stderr, "%s: --pseudonym: %v\n", cmd, err)
		return 2
	}
	if !auditValid(cmd, *by, *reason) {
		return 2
	}
	// Kind and tenant stay empty: the operator learns them from the answer.
	req := dbpkg.IdentityRequest{Action: "trace", IdentityID: *pseudonym, Actor: *by, Reason: *reason}
	return runSigned(cmd, req, func(ctx context.Context, dsn string, req dbpkg.IdentityRequest, s dbpkg.SignedRequest) error {
		res, err := runTraceFn(ctx, dsn, req, s)
		if err != nil {
			return err
		}
		writeTraceResult(os.Stdout, res)
		return nil
	})
}

// reviewerListCmd runs `admin reviewer list`, which reads only and is not
// signed; hidden reviewers appear by pseudonym.
func reviewerListCmd(args []string) int {
	const cmd = "admin reviewer list"
	fs := flag.NewFlagSet("crosscodexd "+cmd, flag.ContinueOnError)
	tenantID := fs.String("tenant", "", "ID of the tenant whose reviewers to list (required)")
	if code, ok := parseNoArgs(fs, cmd, args); !ok {
		return code
	}
	if !tenantFlag(cmd, *tenantID) {
		return 2
	}
	ctx := context.Background()
	dsn, ok := tenantAdminDSN(ctx, cmd)
	if !ok {
		return 1
	}
	records, err := runReviewerListFn(ctx, dsn, *tenantID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", cmd, err)
		return 1
	}
	writeReviewerList(os.Stdout, records)
	return 0
}

// identityExportCmd runs `admin identity export`, which reads only and is
// not signed, like `admin reviewer list`.
func identityExportCmd(args []string) int {
	const cmd = "admin identity export"
	fs := flag.NewFlagSet("crosscodexd "+cmd, flag.ContinueOnError)
	out := fs.String("out", "", "JSONL file to create; it must not exist. It holds the real IDs of hidden tenants and reviewers: store it securely (required)")
	if code, ok := parseNoArgs(fs, cmd, args); !ok {
		return code
	}
	if *out == "" {
		fmt.Fprintf(os.Stderr, "%s: --out is required; name a file that does not exist yet\n", cmd)
		return 2
	}
	ctx := context.Background()
	dsn, ok := tenantAdminDSN(ctx, cmd)
	if !ok {
		return 1
	}
	n, err := writeIdentityExport(ctx, dsn, *out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", cmd, err)
		return 1
	}
	fmt.Fprintf(os.Stdout, "exported %d identity events to %s\n", n, *out)
	return 0
}

// identityExportLine is one line of an export. encoding/json writes []byte
// as standard base64, so Request and Attestation keep their exact bytes and
// sha256 can be recomputed from them.
type identityExportLine struct {
	EventID           int64  `json:"event_id"`
	OccurredAt        string `json:"occurred_at"`
	Action            string `json:"action"`
	Kind              string `json:"kind"`
	TenantID          string `json:"tenant_id"`
	IdentityID        string `json:"identity_id"`
	Actor             string `json:"actor"`
	DBUser            string `json:"db_user"`
	Reason            string `json:"reason"`
	Request           []byte `json:"request"`
	Attestation       []byte `json:"attestation"`
	AttestationSHA256 string `json:"attestation_sha256"`
}

// writeIdentityExport writes the whole audit log as JSONL to out and
// returns the event count. It writes to out.partial (mode 0600, never an
// existing file) and links it to out only once complete, removing the
// partial file on any failure, so a file at out is always a whole export.
// Linking, unlike renaming, fails rather than replace a file created at out
// meanwhile.
func writeIdentityExport(ctx context.Context, dsn, out string) (n int, err error) {
	if _, err := os.Lstat(out); err == nil {
		return 0, fmt.Errorf("%s already exists; nothing was written. Choose a new --out path: an export never overwrites a file", out)
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, fmt.Errorf("check %s: %w", out, err)
	}
	partial := out + ".partial"
	f, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return 0, fmt.Errorf("%s already exists, probably left by an interrupted export, and is incomplete; nothing was written. Remove it and run the export again", partial)
	}
	if err != nil {
		return 0, fmt.Errorf("create %s: %w. Check that the directory exists and is writable", partial, err)
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	err = runIdentityExportFn(ctx, dsn, func(e dbpkg.IdentityEvent) error {
		if err := enc.Encode(identityExportLine{
			EventID: e.EventID, OccurredAt: e.OccurredAt.UTC().Format(time.RFC3339Nano),
			Action: e.Action, Kind: e.Kind, TenantID: e.TenantID, IdentityID: e.IdentityID, Actor: e.Actor,
			DBUser: e.DBUser, Reason: e.Reason, Request: e.Request, Attestation: e.Attestation,
			AttestationSHA256: e.AttestationSHA256,
		}); err != nil {
			return fmt.Errorf("write %s: %w", partial, err)
		}
		n++
		return nil
	})
	if err == nil {
		err = w.Flush()
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Link(partial, out)
		if errors.Is(err, os.ErrExist) {
			err = fmt.Errorf("%s was created by something else during the export; it was left untouched. Choose a new --out path and run the export again", out)
		}
	}
	if err != nil {
		return 0, errors.Join(err, os.Remove(partial))
	}
	if err := os.Remove(partial); err != nil {
		return 0, fmt.Errorf("the export is complete in %s, but removing %s failed: %w. Remove it by hand", out, partial, err)
	}
	return n, nil
}

// daemonIdentityAction applies req through the TenantAdmin method for its
// kind and action. identityActionCmd only builds the pairs handled here.
func daemonIdentityAction(ctx context.Context, dsn string, req dbpkg.IdentityRequest, s dbpkg.SignedRequest) (res identityResult, err error) {
	err = withTenantAdmin(dsn, func(a *dbpkg.TenantAdmin) (err error) {
		t, id, by, why := req.TenantID, req.IdentityID, req.Actor, req.Reason
		switch req.Kind + " " + req.Action {
		case "tenant delete":
			return a.DeleteTenant(ctx, id, by, why, s)
		case "tenant restore":
			return a.RestoreTenant(ctx, id, by, why, s)
		case "tenant hide":
			res.Pseudonym, err = a.HideTenant(ctx, id, by, why, s)
			return err
		case "tenant unhide":
			return a.UnhideTenant(ctx, id, by, why, s)
		case "reviewer add":
			res.Created, err = a.AddReviewer(ctx, t, id, by, why, s)
			return err
		case "reviewer delete":
			return a.DeleteReviewer(ctx, t, id, by, why, s)
		case "reviewer restore":
			return a.RestoreReviewer(ctx, t, id, by, why, s)
		case "reviewer hide":
			res.Pseudonym, err = a.HideReviewer(ctx, t, id, by, why, s)
			return err
		case "reviewer unhide":
			return a.UnhideReviewer(ctx, t, id, by, why, s)
		}
		return fmt.Errorf("no identity action %q for kind %q; nothing was changed", req.Action, req.Kind)
	})
	return res, err
}

// daemonReviewerList lists tenantID's reviewers as tenant_admin.
func daemonReviewerList(ctx context.Context, dsn, tenantID string) (records []dbpkg.ReviewerRecord, err error) {
	err = withTenantAdmin(dsn, func(a *dbpkg.TenantAdmin) (err error) {
		records, err = a.ListReviewers(ctx, tenantID)
		return err
	})
	return records, err
}

// daemonTrace resolves req's pseudonym (req.IdentityID) as tenant_admin;
// the database records the signed lookup in identity_events.
func daemonTrace(ctx context.Context, dsn string, req dbpkg.IdentityRequest, s dbpkg.SignedRequest) (res dbpkg.TraceResult, err error) {
	err = withTenantAdmin(dsn, func(a *dbpkg.TenantAdmin) (err error) {
		res, err = a.Trace(ctx, req.IdentityID, req.Actor, req.Reason, s)
		return err
	})
	return res, err
}

// daemonIdentityExport passes every identity_events row to fn, in event_id
// order, as tenant_admin.
func daemonIdentityExport(ctx context.Context, dsn string, fn func(dbpkg.IdentityEvent) error) error {
	return withTenantAdmin(dsn, func(a *dbpkg.TenantAdmin) error { return a.ExportIdentityEvents(ctx, fn) })
}

func writeReviewerList(w io.Writer, records []dbpkg.ReviewerRecord) {
	if len(records) == 0 {
		fmt.Fprintln(w, "no reviewers")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "REVIEWER\tSTATUS\tCREATED\tBY\tDELETED\tHIDDEN")
	for _, r := range records {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", identityCell(r.ID), identityCell(r.Status), r.CreatedAt.UTC().Format(time.RFC3339),
			identityCell(r.CreatedBy), timeOrDash(r.DeletedAt), yesNo(r.Hidden))
	}
	flushTable(tw, w)
}

func writeTraceResult(w io.Writer, r dbpkg.TraceResult) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "FIELD\tVALUE")
	fmt.Fprintf(tw, "kind\t%s\n", identityCell(r.Kind))
	fmt.Fprintf(tw, "tenant\t%s\n", identityCell(r.TenantID))
	fmt.Fprintf(tw, "identity\t%s\n", identityCell(r.IdentityID))
	fmt.Fprintf(tw, "status\t%s\n", identityCell(r.Status))
	fmt.Fprintf(tw, "hidden\t%s\n", yesNo(r.Hidden))
	flushTable(tw, w)
}
