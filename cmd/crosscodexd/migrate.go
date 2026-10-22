package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/complytime-labs/crosscodex/pkg/config"
	dbpkg "github.com/complytime-labs/crosscodex/pkg/db"
)

// roleDSN is one validated restricted-role DSN from the config. verifier is
// empty when the DSN carries no password.
type roleDSN struct {
	field, role, verifier string
}

// runMigrate implements `crosscodexd migrate`. args are the tokens after
// "migrate". It returns 0 on success, 1 on failure and 2 on a usage error.
func runMigrate(args []string) int {
	const cmd = "migrate"
	fs := flag.NewFlagSet("crosscodexd "+cmd, flag.ContinueOnError)
	if code, ok := parseNoArgs(fs, cmd, args); !ok {
		return code
	}
	ctx := context.Background()
	cfg, ok := loadAdminConfig(ctx, cmd)
	if !ok {
		return 1
	}
	if err := migrateDatabase(ctx, cfg, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", cmd, err)
		return 1
	}
	return 0
}

// migrateDatabase does, as the schema owner (database.migration_dsn), the
// work the daemon must not be able to do: it applies the schema migrations,
// sets each restricted role's password from the password in that role's
// configured DSN, and provisions tenants.default_tenant. The daemon itself
// connects only as the restricted roles, which RLS applies to.
//
// Every role DSN is validated before the database is touched, so a bad
// entry fails the run with nothing changed. Passwords are sent as
// SCRAM-SHA-256 verifiers, never in plaintext, and are never printed.
func migrateDatabase(ctx context.Context, cfg *config.Config, out io.Writer) error {
	if cfg.Database.MigrationDSN == "" {
		return errors.New("database.migration_dsn is not set. crosscodexd migrate applies schema migrations as the schema owner; set database.migration_dsn (or CROSSCODEX_DATABASE_MIGRATION_DSN) to a postgres:// URL for the owner role. The daemon itself never uses it")
	}
	// golang-migrate returns the raw url.Parse error, which quotes the whole
	// DSN, password included. Parse it first and drop that error, so every
	// later library sees a URL that parses (pgconn then redacts it itself).
	if u, err := url.Parse(cfg.Database.MigrationDSN); err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
		return errors.New("database.migration_dsn is not a valid postgres:// URL; check it for characters that must be percent-encoded (passwords should be hex). The value is not shown because it contains a password")
	}
	roles, err := roleDSNs(cfg)
	if err != nil {
		return err
	}

	migrator, err := dbpkg.NewMigrator(cfg.Database.MigrationDSN)
	if err != nil {
		return fmt.Errorf("create migrator: %w", err)
	}
	if err := migrator.Up(ctx); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		migrator.Close()
		return fmt.Errorf("run migrations: %w", err)
	}
	if err := migrator.Close(); err != nil {
		return fmt.Errorf("close migrator: %w", err)
	}

	if err := setRolePasswords(ctx, cfg.Database.MigrationDSN, roles, out); err != nil {
		return err
	}

	if t := cfg.Tenants.DefaultTenant; t != "" {
		pool, err := dbpkg.NewPool(dbpkg.PoolConfig{DSN: cfg.Database.MigrationDSN, MaxOpenConns: 2})
		if err != nil {
			return fmt.Errorf("connect as database.migration_dsn: %w", err)
		}
		err = dbpkg.EnsureTenant(ctx, pool, t, t)
		pool.Close()
		if err != nil {
			return fmt.Errorf("provision default tenant: %w", err)
		}
		// EnsureTenant is idempotent and does not report whether it created
		// the row, so the message does not claim it did.
		fmt.Fprintf(out, "ensured default tenant %s\n", t)
	}

	version, err := dbpkg.LatestMigrationVersion()
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "schema at version %d\n", version)
	return nil
}

// roleDSNs validates every configured restricted-role DSN and computes the
// verifier for each password, without touching the database. Only a
// password written in the URL userinfo is used (a password query parameter
// is refused, since the daemon would use it while this ignored it): PGPASSWORD or ~/.pgpass may hold the
// owner's password, which must never become a restricted role's.
func roleDSNs(cfg *config.Config) ([]roleDSN, error) {
	entries := []struct{ field, role, dsn string }{
		{"database.dsn", "app_user", cfg.Database.DSN},
		{"database.graph_dsn", "graph_user", cfg.Database.GraphDSN},
		{"database.purge_dsn", "purge_user", cfg.Database.PurgeDSN},
		{"database.tenant_admin_dsn", "tenant_admin", cfg.Database.TenantAdminDSN},
		{"backup.dsn", "backup_user", cfg.Backup.DSN},
	}
	var roles []roleDSN
	for _, e := range entries {
		if e.dsn == "" {
			continue
		}
		// The url.Parse error is dropped: it quotes the DSN, password included.
		u, err := url.Parse(e.dsn)
		if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
			return nil, fmt.Errorf("%s must be a postgres:// URL so crosscodexd migrate can read its role; write it as postgres://%s:<password>@<host>:<port>/<database>", e.field, e.role)
		}
		if u.Query().Has("password") {
			return nil, fmt.Errorf("%s carries a password query parameter, which crosscodexd migrate cannot use to set %s's password; put the password in the URL userinfo (postgres://role:password@host/...)", e.field, e.role)
		}
		if user := u.User.Username(); user != e.role {
			return nil, fmt.Errorf("%s connects as %q, but it must connect as %s; set the user in %s to %s (crosscodexd migrate sets %s's password from that URL)", e.field, user, e.role, e.field, e.role, e.role)
		}
		r := roleDSN{field: e.field, role: e.role}
		if password, ok := u.User.Password(); ok && password != "" {
			salt := make([]byte, 16)
			if _, err := rand.Read(salt); err != nil {
				return nil, fmt.Errorf("%s: generate SCRAM salt: %w", e.field, err)
			}
			r.verifier, err = dbpkg.SCRAMVerifier(password, salt)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", e.field, err)
			}
		}
		roles = append(roles, r)
	}
	return roles, nil
}

// setRolePasswords sets each role's password to its verifier and reports
// the roles whose DSN had no password.
func setRolePasswords(ctx context.Context, migrationDSN string, roles []roleDSN, out io.Writer) error {
	ownerDB, err := sql.Open("pgx", migrationDSN)
	if err != nil {
		return fmt.Errorf("open database.migration_dsn: %w", err)
	}
	defer ownerDB.Close()
	for _, r := range roles {
		if r.verifier == "" {
			fmt.Fprintf(out, "%s has no password; left %s's password unchanged\n", r.field, r.role)
			continue
		}
		// ALTER ROLE takes no bound parameters. The verifier is base64 and
		// '$'/':' separators (dbpkg.SCRAMVerifier), so it cannot hold a quote.
		stmt := fmt.Sprintf("ALTER ROLE %s PASSWORD '%s'", pgx.Identifier{r.role}.Sanitize(), r.verifier)
		if _, err := ownerDB.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("set the password of %s from %s: %w", r.role, r.field, err)
		}
		fmt.Fprintf(out, "set the password of %s from %s\n", r.role, r.field)
	}
	return nil
}
