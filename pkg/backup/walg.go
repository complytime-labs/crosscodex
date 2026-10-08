package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

// walgBinary is resolved through PATH; both images install the pinned
// binary as /usr/local/bin/wal-g (deploy/db/fetch-walg.sh).
const walgBinary = "wal-g"

// Check statuses printed by `wal-g wal-verify --json`.
const (
	WALStatusOK      = "OK"
	WALStatusWarning = "WARNING" // segments still uploading or delayed
	WALStatusFailure = "FAILURE" // segments lost: PITR across the gap is impossible
)

var walSegmentPattern = regexp.MustCompile(`^[0-9A-F]{24}$`)

// pgEnvParams maps the DSN query parameters WAL-G can honour to the libpq
// environment variables pgx reads (WAL-G connects with pgx.ParseConfig("")).
// Other parameters are rejected so a setting is never silently dropped.
var pgEnvParams = map[string]string{
	"connect_timeout": "PGCONNECT_TIMEOUT",
	"sslcert":         "PGSSLCERT",
	"sslkey":          "PGSSLKEY",
	"sslmode":         "PGSSLMODE",
	"sslrootcert":     "PGSSLROOTCERT",
}

// CommandRunner runs name with args under exactly the environment env and
// returns stdout. A non-zero exit is an error carrying the command's stderr.
type CommandRunner func(ctx context.Context, env []string, name string, args ...string) ([]byte, error)

// ExecRunner is the production CommandRunner. It uses an argument vector,
// never a shell; WALG passes credentials in env, never in args. On context
// cancellation it sends SIGTERM (not Kill) and gives the process up to
// WaitDelay to exit cleanly, so backup-push can finish an in-flight upload
// step rather than leaving a half-written base backup at the destination.
func ExecRunner(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 30 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// cmd.Run's error doesn't wrap ctx.Err() on its own (it reports the
		// signal, e.g. "signal: terminated"), so errors.Is(err,
		// context.Canceled) would otherwise never match a canceled run.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return stdout.Bytes(), fmt.Errorf("%s %s: %w: %w: %s", name, strings.Join(args, " "), err, ctxErr, tail(stderr.String(), 4096))
		}
		return stdout.Bytes(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, tail(stderr.String(), 4096))
	}
	return stdout.Bytes(), nil
}

// tail keeps the last n bytes of s; WAL-G prints its actual error last.
func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return "..." + s[len(s)-n:]
	}
	return s
}

// WALG runs WAL-G as backup_user against the configured destination.
type WALG struct {
	env []string // settings derived from config; see walgEnv
	run CommandRunner
}

// NewWALG derives WAL-G's environment from backup.dsn and the destination.
func NewWALG(dsn string, dest config.BackupDestinationConfig, run CommandRunner) (*WALG, error) {
	env, err := walgEnv(dsn, dest)
	if err != nil {
		return nil, err
	}
	return &WALG{env: env, run: run}, nil
}

func walgEnv(dsn string, dest config.BackupDestinationConfig) ([]string, error) {
	// None of these errors echo dsn: url.Error (from a malformed DSN like a
	// stray space in the password) formats its Error() with the input URL,
	// so that error itself must never be surfaced, only that parsing failed.
	u, err := url.Parse(dsn)
	if err != nil {
		return nil, errors.New("backup.dsn is not a valid URL (value not shown; it may hold a password)")
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return nil, fmt.Errorf("backup.dsn scheme %q must be postgres:// or postgresql://", u.Scheme)
	}
	if strings.Contains(u.Host, ",") {
		return nil, errors.New("backup.dsn must name exactly one host; multi-host DSNs are not supported")
	}
	if u.User == nil || u.User.Username() == "" {
		return nil, errors.New("backup.dsn must include a user (backup_user)")
	}
	// url.Values.Query() silently drops a parameter it cannot decode
	// instead of erroring (e.g. a truncated %-escape in sslmode), which
	// would downgrade TLS without a trace. ParseQuery surfaces that error.
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, errors.New("backup.dsn query string is malformed")
	}
	var env []string
	if h := u.Hostname(); h != "" {
		env = append(env, "PGHOST="+h)
	}
	if p := u.Port(); p != "" {
		env = append(env, "PGPORT="+p)
	}
	env = append(env, "PGUSER="+u.User.Username())
	if pw, ok := u.User.Password(); ok {
		env = append(env, "PGPASSWORD="+pw)
	}
	if db := strings.TrimPrefix(u.Path, "/"); db != "" {
		env = append(env, "PGDATABASE="+db)
	}
	for _, k := range slices.Sorted(maps.Keys(q)) {
		if len(q[k]) > 1 {
			return nil, fmt.Errorf("backup.dsn query parameter %q is given more than once", k)
		}
		v, ok := pgEnvParams[k]
		if !ok {
			return nil, fmt.Errorf("backup.dsn query parameter %q is not supported; allowed: %s", k, strings.Join(slices.Sorted(maps.Keys(pgEnvParams)), ", "))
		}
		env = append(env, v+"="+q.Get(k))
	}
	switch dest.Backend {
	case "local":
		env = append(env, "WALG_FILE_PREFIX="+filepath.Join(dest.Local.Path, "postgres"))
	case "s3":
		env = append(env, "WALG_S3_PREFIX=s3://"+dest.S3.Bucket+"/postgres")
		if dest.S3.Region != "" {
			env = append(env, "AWS_REGION="+dest.S3.Region)
		}
		if dest.S3.Endpoint != "" {
			// pkg/storage uses path-style addressing whenever an endpoint is set; match it.
			env = append(env, "AWS_ENDPOINT="+dest.S3.Endpoint, "AWS_S3_FORCE_PATH_STYLE=true")
		}
		if dest.S3.StorageClass != "" {
			env = append(env, "WALG_S3_STORAGE_CLASS="+dest.S3.StorageClass)
		}
	default:
		return nil, fmt.Errorf("backup.destination.backend %q is not local or s3", dest.Backend)
	}
	return env, nil
}

// envAllowlist is the set of inherited environment variables environ keeps
// verbatim. envAllowlistPrefixes extends it to whole prefixes.
//
// This is an allowlist, not a denylist, because WAL-G reads far more than
// the WALG_ prefix: every WALG_ setting falls back to a WALE_ one of the
// same name (config.GetWaleCompatibleSetting), several storage backends
// let their unprefixed settings (S3_*, GPG_*, YC_*, OSS_*, GS_*, AZ_*,
// SWIFT_*, SSH_*) override the WALG_ ones (internal.StorageAdapter.
// loadSettings), and PGSSLROOTCERT and friends feed pgx the same way the
// DSN does. A denylist would have to name every one of those, and miss the
// next one wal-g adds; an allowlist is reviewed once, and an unrecognized
// inherited setting is dropped by default instead of silently taking
// effect. WAL-G also reads ~/.walg.json if HOME is set; that file is the
// operator's responsibility, and the images this runs in ship none.
var envAllowlist = map[string]bool{
	"PATH":          true,
	"HOME":          true,
	"TMPDIR":        true,
	"TZ":            true,
	"SSL_CERT_FILE": true,
	"SSL_CERT_DIR":  true,

	// AWS credential-chain variables: these let the AWS SDK's normal
	// credential discovery (env vars, shared config/credentials files,
	// SSO, container and EC2 instance-metadata roles) keep working.
	"AWS_ACCESS_KEY_ID":           true,
	"AWS_SECRET_ACCESS_KEY":       true,
	"AWS_SESSION_TOKEN":           true,
	"AWS_PROFILE":                 true,
	"AWS_SHARED_CREDENTIALS_FILE": true,
	"AWS_CONFIG_FILE":             true,
	"AWS_WEB_IDENTITY_TOKEN_FILE": true,
	"AWS_ROLE_ARN":                true,
	"AWS_ROLE_SESSION_NAME":       true,
}

var envAllowlistPrefixes = []string{"AWS_CONTAINER_", "AWS_EC2_METADATA_"}

// environ is the inherited environment filtered to envAllowlist (plus
// AWS_REGION, see below), with w.env appended. w.env's settings always win
// because they're appended last and WAL-G, like libpq, takes the last
// value of a repeated environment variable.
func (w *WALG) environ() []string {
	hasRegion := slices.ContainsFunc(w.env, func(kv string) bool { return strings.HasPrefix(kv, "AWS_REGION=") })
	inherited := os.Environ()
	env := make([]string, 0, len(inherited)+len(w.env))
	for _, kv := range inherited {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case k == "AWS_REGION":
			// Keep the inherited region only when the config sets none;
			// otherwise the config's value is appended below and the
			// inherited one would just be a duplicate of a stale setting.
			if !hasRegion {
				env = append(env, kv)
			}
		case envAllowlist[k]:
			env = append(env, kv)
		default:
			for _, p := range envAllowlistPrefixes {
				if strings.HasPrefix(k, p) {
					env = append(env, kv)
					break
				}
			}
		}
	}
	return append(env, w.env...)
}

func (w *WALG) exec(ctx context.Context, args ...string) ([]byte, error) {
	return w.run(ctx, w.environ(), walgBinary, args...)
}

// BackupPush takes a full base backup over the replication protocol
// (remote mode: no PGDATA argument, so crosscodexd needs no access to the
// database's files).
func (w *WALG) BackupPush(ctx context.Context) error {
	_, err := w.exec(ctx, "backup-push")
	return err
}

// WALGBackup is one entry of `wal-g backup-list --json --detail`.
type WALGBackup struct {
	Name           string    `json:"backup_name"`
	WALFileName    string    `json:"wal_file_name"`
	StartTime      time.Time `json:"start_time"`
	FinishTime     time.Time `json:"finish_time"`
	StartLSN       uint64    `json:"start_lsn"`
	FinishLSN      uint64    `json:"finish_lsn"`
	CompressedSize int64     `json:"compressed_size"`
}

// Timeline is the PostgreSQL timeline: the first 8 hex digits of the WAL
// file name. WAL-G's listing has no timeline field.
func (b WALGBackup) Timeline() (uint32, error) {
	if !walSegmentPattern.MatchString(b.WALFileName) {
		return 0, fmt.Errorf("timeline of WAL-G backup %s: wal_file_name %q is not a 24-digit hex WAL segment name", b.Name, b.WALFileName)
	}
	tl, err := strconv.ParseUint(b.WALFileName[:8], 16, 32)
	if err != nil {
		return 0, fmt.Errorf("timeline of WAL-G backup %s: %w", b.Name, err)
	}
	if tl == 0 {
		return 0, fmt.Errorf("timeline of WAL-G backup %s: timeline 0 is not a valid PostgreSQL timeline", b.Name)
	}
	return uint32(tl), nil
}

// Backups lists the base backups at the destination.
func (w *WALG) Backups(ctx context.Context) ([]WALGBackup, error) {
	out, err := w.exec(ctx, "backup-list", "--json", "--detail")
	if err != nil {
		return nil, err
	}
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return nil, errors.New("wal-g backup-list printed no JSON")
	}
	var list []WALGBackup
	if err := json.Unmarshal(trimmed, &list); err != nil {
		return nil, fmt.Errorf("parse wal-g backup-list output: %w", err)
	}
	// "[]" and "null" are both valid, empty results: backup-list runs
	// against an empty destination before the first backup-push.
	for _, b := range list {
		if !walgBackupPattern.MatchString(b.Name) || !walSegmentPattern.MatchString(b.WALFileName) {
			return nil, fmt.Errorf("wal-g backup-list returned a malformed entry (backup_name %q, wal_file_name %q)", b.Name, b.WALFileName)
		}
	}
	return list, nil
}

// WALCheck is one `wal-g wal-verify` check result.
type WALCheck struct {
	Name   string
	Status string
}

// VerifyWAL runs the integrity and timeline checks. wal-verify exits 0
// whatever it finds, so the JSON status is the result. It connects to
// Postgres (as backup_user) to learn the current WAL segment.
func (w *WALG) VerifyWAL(ctx context.Context) ([]WALCheck, error) {
	out, err := w.exec(ctx, "wal-verify", "integrity", "timeline", "--json")
	if err != nil {
		return nil, err
	}
	var res map[string]struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &res); err != nil {
		return nil, fmt.Errorf("parse wal-g wal-verify output: %w", err)
	}
	checks := make([]WALCheck, 0, 2)
	for _, name := range []string{"integrity", "timeline"} {
		r, ok := res[name]
		if !ok {
			return nil, fmt.Errorf("wal-g wal-verify output lacks the %q check", name)
		}
		switch r.Status {
		case WALStatusOK, WALStatusWarning, WALStatusFailure:
		default:
			return nil, fmt.Errorf("wal-g wal-verify %s check has unknown status %q", name, r.Status)
		}
		checks = append(checks, WALCheck{Name: name, Status: r.Status})
	}
	return checks, nil
}
