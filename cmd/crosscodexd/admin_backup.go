package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/complytime-labs/crosscodex/pkg/backup"
	"github.com/complytime-labs/crosscodex/pkg/config"
	dbpkg "github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/storage"
)

// Seams: specs replace these to exercise parsing and exit codes without stores.
var (
	runBackupRunFn     = daemonBackupRun
	runBackupListFn    = daemonBackupList
	runBackupVerifyFn  = daemonBackupVerify
	runBackupRestoreFn = daemonBackupRestore
)

// parseNoArgs parses fs and rejects positional arguments. ok=false means
// return code: 0 for -h, 2 for a usage error.
func parseNoArgs(fs *flag.FlagSet, cmd string, args []string) (code int, ok bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, false
		}
		return 2, false
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "%s: unexpected argument %q\n", cmd, fs.Arg(0))
		return 2, false
	}
	return 0, true
}

// loadAdminConfig loads config for cmd, printing the failure.
func loadAdminConfig(ctx context.Context, cmd string) (*config.Config, bool) {
	cfg, err := config.NewLoader().Load(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: load config: %v\n", cmd, err)
		return nil, false
	}
	return cfg, true
}

// pointFlag validates a --point value; required controls whether "" is allowed.
func pointFlag(cmd, point string, required bool) bool {
	if point == "" {
		if required {
			fmt.Fprintf(os.Stderr, "%s: --point is required; copy an ID from `crosscodexd admin backup list`\n", cmd)
			return false
		}
		return true
	}
	if err := backup.ValidatePointID(point); err != nil {
		fmt.Fprintf(os.Stderr, "%s: --point: %v\n", cmd, err)
		return false
	}
	return true
}

func backupRunCmd(args []string) int {
	const cmd = "admin backup run"
	fs := flag.NewFlagSet("crosscodexd "+cmd, flag.ContinueOnError)
	if code, ok := parseNoArgs(fs, cmd, args); !ok {
		return code
	}
	ctx := context.Background()
	cfg, ok := loadAdminConfig(ctx, cmd)
	if !ok {
		return 1
	}
	m, err := runBackupRunFn(ctx, cfg)
	if m != nil {
		writeBackupRunReport(os.Stdout, m)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", cmd, err)
		return 1
	}
	return 0
}

func backupListCmd(args []string) int {
	const cmd = "admin backup list"
	fs := flag.NewFlagSet("crosscodexd "+cmd, flag.ContinueOnError)
	if code, ok := parseNoArgs(fs, cmd, args); !ok {
		return code
	}
	ctx := context.Background()
	cfg, ok := loadAdminConfig(ctx, cmd)
	if !ok {
		return 1
	}
	res, err := runBackupListFn(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", cmd, err)
		return 1
	}
	writeBackupList(os.Stdout, res)
	return 0
}

func backupVerifyCmd(args []string) int {
	const cmd = "admin backup verify"
	fs := flag.NewFlagSet("crosscodexd "+cmd, flag.ContinueOnError)
	point := fs.String("point", "", "verify only this backup point (default: every complete point)")
	if code, ok := parseNoArgs(fs, cmd, args); !ok {
		return code
	}
	if !pointFlag(cmd, *point, false) {
		return 2
	}
	ctx := context.Background()
	cfg, ok := loadAdminConfig(ctx, cmd)
	if !ok {
		return 1
	}
	rep, err := runBackupVerifyFn(ctx, cfg, backup.VerifyOptions{PointID: *point})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", cmd, err)
		return 1
	}
	writeVerifyReport(os.Stdout, rep)
	if !rep.OK() {
		for _, line := range backupVerifyFailureLines(cmd, rep) {
			fmt.Fprintln(os.Stderr, line)
		}
		return 1
	}
	return 0
}

// backupVerifyFailureLines reports each failing condition in rep
// separately, so the operator knows which of the three checks tripped
// rather than a single lumped-together message.
func backupVerifyFailureLines(cmd string, rep backup.VerifyReport) []string {
	var lines []string
	var badPoints int
	for _, p := range rep.Points {
		if len(p.Issues) > 0 {
			badPoints++
		}
	}
	if badPoints > 0 {
		lines = append(lines, fmt.Sprintf("%s: FAILED: integrity problems in %d point(s) (see report)", cmd, badPoints))
	}
	for _, c := range rep.WAL {
		if c.Status == backup.WALStatusFailure {
			lines = append(lines, fmt.Sprintf("%s: FAILED: WAL archive has a gap (see WAL CHECK)", cmd))
			break
		}
	}
	var stale []string
	for _, s := range rep.Staleness {
		if s.Stale {
			stale = append(stale, s.Store)
		}
	}
	if len(stale) > 0 {
		lines = append(lines, fmt.Sprintf("%s: FAILED: stale stores: %s (staleness is repository-wide: it uses the newest point, not only --point)",
			cmd, strings.Join(stale, ", ")))
	}
	return lines
}

func backupRestoreCmd(args []string) int {
	const cmd = "admin backup restore"
	fs := flag.NewFlagSet("crosscodexd "+cmd, flag.ContinueOnError)
	point := fs.String("point", "", "backup point to restore (required; see `admin backup list`)")
	if code, ok := parseNoArgs(fs, cmd, args); !ok {
		return code
	}
	if !pointFlag(cmd, *point, true) {
		return 2
	}
	ctx := context.Background()
	cfg, ok := loadAdminConfig(ctx, cmd)
	if !ok {
		return 1
	}
	if err := runBackupRestoreFn(ctx, cfg, *point); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", cmd, err)
		return 1
	}
	fmt.Fprintf(os.Stdout, "restored objects and audit streams from backup point %s\n", *point)
	return 0
}

// backupNeeds selects the connections a command opens; every command opens
// the repository and the object-store opener (which connects lazily).
type backupNeeds struct {
	walg bool // WAL-G (backup.dsn + destination)
	db   bool // tenant lister + backup/retention lock (backup.dsn)
	nats bool // external JetStream
}

// openBackup builds a backup.Service with only what the command needs. The
// returned close func releases everything it opened, in reverse order.
func openBackup(ctx context.Context, cfg *config.Config, need backupNeeds) (svc *backup.Service, closeFn func() error, err error) {
	var closers []func() error
	cleanup := func() error {
		var errs []error
		for i := len(closers) - 1; i >= 0; i-- {
			errs = append(errs, closers[i]())
		}
		return errors.Join(errs...)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, cleanup())
		}
	}()

	repo, err := backup.OpenRepository(cfg.Backup.Destination)
	if err != nil {
		return nil, nil, err
	}
	closers = append(closers, repo.Close)
	deps := backup.Deps{
		Repo: repo,
		// Same constructor the daemon's retention wiring uses, so tenant
		// prefix and path checks apply to backup reads and restore writes.
		Objects: func(tenantID string) (storage.Provider, error) {
			return storage.NewFromConfig(cfg.Storage.Objects, tenantID)
		},
		MaxAge: backup.MaxAge{Postgres: cfg.Backup.MaxAge.Postgres, Objects: cfg.Backup.MaxAge.Objects, NATS: cfg.Backup.MaxAge.NATS},
	}
	if need.walg {
		if deps.WALG, err = backup.NewWALG(cfg.Backup.DSN, cfg.Backup.Destination, backup.ExecRunner); err != nil {
			return nil, nil, err
		}
	}
	if need.db {
		var lister *backup.SQLTenantLister
		lister, err = backup.OpenSQLTenantLister(cfg.Backup.DSN)
		if err != nil {
			return nil, nil, err
		}
		closers = append(closers, lister.Close)
		deps.Tenants = lister
		// backup_user and the retention engine's app_user share the lock
		// because advisory locks are per database, not per role.
		deps.Lock = dbpkg.NewAdvisoryLocker(cfg.Backup.DSN, dbpkg.BackupRetentionLockName)
	}
	if need.nats {
		var opts []natsbus.Option
		var tlsCfg *tls.Config
		tlsCfg, err = natsTLSConfig(ctx, cfg)
		if err != nil {
			return nil, nil, err
		}
		if tlsCfg != nil {
			opts = append(opts, natsbus.WithTLSConfig(tlsCfg))
		}
		var nc *nats.Conn
		nc, err = natsbus.DialExternal(cfg.NATS, opts...)
		if err != nil {
			return nil, nil, err
		}
		closers = append(closers, func() error { nc.Close(); return nil })
		deps.Streams, err = backup.NewJSMStreamStore(nc)
		if err != nil {
			return nil, nil, err
		}
		deps.StreamNames = natsbus.AuditStreamNames()
	}
	if svc, err = backup.New(deps); err != nil {
		return nil, nil, err
	}
	return svc, cleanup, nil
}

func daemonBackupRun(ctx context.Context, cfg *config.Config) (m *backup.Manifest, err error) {
	svc, closeFn, err := openBackup(ctx, cfg, backupNeeds{walg: true, db: true, nats: true})
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, closeFn()) }()
	return svc.Run(ctx)
}

func daemonBackupList(ctx context.Context, cfg *config.Config) (res backup.ListResult, err error) {
	svc, closeFn, err := openBackup(ctx, cfg, backupNeeds{})
	if err != nil {
		return backup.ListResult{}, err
	}
	defer func() { err = errors.Join(err, closeFn()) }()
	return svc.List(ctx)
}

func daemonBackupVerify(ctx context.Context, cfg *config.Config, opts backup.VerifyOptions) (rep backup.VerifyReport, err error) {
	svc, closeFn, err := openBackup(ctx, cfg, backupNeeds{walg: true})
	if err != nil {
		return backup.VerifyReport{}, err
	}
	defer func() { err = errors.Join(err, closeFn()) }()
	return svc.Verify(ctx, opts)
}

func daemonBackupRestore(ctx context.Context, cfg *config.Config, pointID string) (err error) {
	svc, closeFn, err := openBackup(ctx, cfg, backupNeeds{walg: true, nats: true})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closeFn()) }()
	return svc.Restore(ctx, pointID)
}

func flushTable(tw *tabwriter.Writer, w io.Writer) {
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(w, "error flushing output: %v\n", err)
	}
}

func formatWindow(win backup.Window) string {
	return win.Start.UTC().Format(time.RFC3339) + " .. " + win.End.UTC().Format(time.RFC3339)
}

// writeStoreRows writes one row per store of m; first is the leading cell
// of the first row (the point ID in list output, empty in run output).
func writeStoreRows(tw io.Writer, first string, m *backup.Manifest) {
	var objects int
	for _, entries := range m.Objects.Tenants {
		objects += len(entries)
	}
	var msgs uint64
	for _, s := range m.NATS.Streams {
		msgs += s.State.Msgs
	}
	fmt.Fprintf(tw, "%s\tpostgres\t%s\t%d\t%s (timeline %d)\n", first, formatWindow(m.Postgres.Window), m.Postgres.Bytes, m.Postgres.BackupName, m.Postgres.Timeline)
	fmt.Fprintf(tw, "\tobjects\t%s\t%d\t%d tenants, %d objects\n", formatWindow(m.Objects.Window), m.Objects.Bytes, len(m.Objects.Tenants), objects)
	fmt.Fprintf(tw, "\tnats\t%s\t%d\t%d streams, %d messages\n", formatWindow(m.NATS.Window), m.NATS.Bytes, len(m.NATS.Streams), msgs)
}

func writeBackupRunReport(w io.Writer, m *backup.Manifest) {
	fmt.Fprintf(w, "backup point %s committed\n", m.ID)
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "\tSTORE\tCAPTURED (UTC)\tBYTES\tDETAIL")
	writeStoreRows(tw, "", m)
	flushTable(tw, w)
}

func writeBackupList(w io.Writer, res backup.ListResult) {
	if len(res.Complete) == 0 {
		fmt.Fprintln(w, "no complete backup points")
	} else {
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "POINT\tSTORE\tCAPTURED (UTC)\tBYTES\tDETAIL")
		for _, m := range res.Complete {
			writeStoreRows(tw, m.ID, m)
		}
		flushTable(tw, w)
	}
	if len(res.Incomplete) > 0 {
		fmt.Fprintln(w, "\nincomplete points (run failed or still running; not restorable):")
		for _, id := range res.Incomplete {
			fmt.Fprintf(w, "  %s\n", id)
		}
	}
}

func writeVerifyReport(w io.Writer, rep backup.VerifyReport) {
	if len(rep.Points) == 0 {
		fmt.Fprintln(w, "no complete backup points")
	} else {
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "POINT\tRESULT\tEST. RESTORE")
		for _, p := range rep.Points {
			result := "ok"
			if len(p.Issues) > 0 {
				result = "FAILED"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\n", p.ID, result, p.EstimatedRestore.Round(time.Second))
		}
		flushTable(tw, w)
		for _, p := range rep.Points {
			for _, issue := range p.Issues {
				fmt.Fprintf(w, "  %s: %s\n", p.ID, issue)
			}
		}
	}

	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "WAL CHECK\tSTATUS")
	for _, c := range rep.WAL {
		fmt.Fprintf(tw, "%s\t%s\n", c.Name, c.Status)
	}
	flushTable(tw, w)

	fmt.Fprintln(w)
	tw = tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "STORE\tNEWEST POINT\tAGE\tMAX AGE\tSTATUS")
	for _, s := range rep.Staleness {
		newest, age, status := "(none)", "-", "ok"
		if s.NewestPoint != "" {
			newest, age = s.NewestPoint, s.Age.Round(time.Second).String()
		}
		if s.Stale {
			status = "STALE"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", s.Store, newest, age, s.MaxAge, status)
	}
	flushTable(tw, w)

	if len(rep.Incomplete) > 0 {
		fmt.Fprintln(w, "\nincomplete points (not verified):")
		for _, id := range rep.Incomplete {
			fmt.Fprintf(w, "  %s\n", id)
		}
	}
}
