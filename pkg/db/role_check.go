package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"

	"github.com/complytime-labs/crosscodex/pkg/db/migrations"
)

// RowQueryer is the query method CheckSchemaVersion and CheckRestrictedRole
// need; *sql.DB, *sql.Conn and *sql.Tx implement it.
type RowQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// LatestMigrationVersion returns the highest version among the embedded
// migrations: the schema version this binary needs.
func LatestMigrationVersion() (uint, error) {
	names, err := fs.Glob(migrations.FS, "*.up.sql")
	if err != nil {
		return 0, fmt.Errorf("list migrations: %w", err)
	}
	var latest uint
	for _, n := range names {
		prefix, _, ok := strings.Cut(n, "_")
		if !ok {
			return 0, fmt.Errorf("migration %s has no NNN_ version prefix", n)
		}
		v, err := strconv.ParseUint(prefix, 10, 32)
		if err != nil {
			return 0, fmt.Errorf("migration %s: version prefix %q is not a number: %w", n, prefix, err)
		}
		latest = max(latest, uint(v))
	}
	if latest == 0 {
		return 0, errors.New("no embedded migrations found")
	}
	return latest, nil
}

// CheckSchemaVersion refuses a database whose schema is not at
// LatestMigrationVersion, whose last migration failed (dirty), or that was
// created from an earlier edition of the baseline (baselineColumns). The daemon
// never migrates; `crosscodexd migrate` does, as the schema owner. Call it on
// the app_user (database.dsn) connection; other restricted roles lack the
// SELECT grant on schema_migrations.
func CheckSchemaVersion(ctx context.Context, q RowQueryer) error {
	want, err := LatestMigrationVersion()
	if err != nil {
		return err
	}
	var version int64
	var dirty bool
	if err := q.QueryRowContext(ctx, "SELECT version, dirty FROM schema_migrations").Scan(&version, &dirty); err != nil {
		return fmt.Errorf("cannot read the schema version (%w). Run `crosscodexd migrate` with database.migration_dsn set to the schema owner, then start crosscodexd again", err)
	}
	switch {
	case dirty:
		return fmt.Errorf("%w: schema migration %d failed part-way. Inspect and repair the database as the schema owner, then rerun `crosscodexd migrate`", ErrMigrationDirty, version)
	case version < int64(want):
		return fmt.Errorf("the database schema is at version %d but this crosscodexd needs version %d. Run `crosscodexd migrate` with database.migration_dsn set to the schema owner, then start crosscodexd again", version, want)
	case version > int64(want):
		return fmt.Errorf("the database schema is at version %d, newer than this crosscodexd (version %d). Upgrade crosscodexd; do not run an older binary against a newer schema", version, want)
	}
	for _, c := range baselineColumns {
		var present bool
		if err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2)`, c.table, c.column).Scan(&present); err != nil {
			return fmt.Errorf("cannot check the schema for %s.%s (%w). Check the database connection, then start crosscodexd again", c.table, c.column, err)
		}
		if !present {
			return staleBaselineError(version, c.table+"."+c.column)
		}
	}
	for _, c := range baselineFunctionColumns {
		var present bool
		if err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.routines r
			JOIN information_schema.parameters p ON p.specific_schema = r.specific_schema AND p.specific_name = r.specific_name
			WHERE r.routine_schema = 'public' AND r.routine_name = $1 AND p.parameter_mode = 'OUT' AND p.parameter_name = $2)`,
			c.function, c.column).Scan(&present); err != nil {
			return fmt.Errorf("cannot check the schema for %s() column %s (%w). Check the database connection, then start crosscodexd again", c.function, c.column, err)
		}
		if !present {
			return staleBaselineError(version, c.function+"() column "+c.column)
		}
	}
	return nil
}

// staleBaselineError reports a database created from an earlier edition of
// the baseline, which lacks what.
func staleBaselineError(version int64, what string) error {
	return fmt.Errorf("the database schema is at version %d but was created by an earlier build of crosscodexd: %s is missing. "+
		"The pre-release baseline migration was changed in place, so this database cannot be upgraded. "+
		"Recreate it (for the compose deployment, `task deploy:down VOLUMES=1`), then run `crosscodexd migrate` and start crosscodexd again",
		version, what)
}

// baselineColumns are columns folded into the pre-release baseline
// migration after databases had already been created from it
// (docs/dev/migrations.md allows editing it in place until the first
// release). Such a database reports the current version but lacks them, so
// CheckSchemaVersion looks for each; the daemon would otherwise start and
// fail at its first write. Add a column here when folding one into the
// baseline; the list empties once migrations are append-only.
var baselineColumns = []struct{ table, column string }{
	{"jobs", "error_class"},
}

// baselineFunctionColumns are result columns of set-returning functions
// folded into the baseline the same way.
var baselineFunctionColumns = []struct{ function, column string }{
	{"resumable_jobs", "error_class"},
}

// CheckRestrictedRole refuses a connection whose role bypasses row-level
// security: a superuser, a BYPASSRLS role, or the owner of a public table
// (RLS is enabled but not forced, so owners skip it, and only the owner can
// disable the refusal triggers). Membership counts as
// identity: PostgreSQL applies an INHERIT member of such a role the same
// bypass, and a member of a superuser can SET ROLE to it. Tenant isolation
// and the immutability triggers hold only for the restricted app_user,
// graph_user and purge_user roles. field names the config field the
// connection came from; the refusal names the role that field must use.
//
// Only the public schema is checked: graph_user owns the per-tenant AGE graph
// schemas by design (create_tenant_graph). Add any new application
// schema to the ownership test below.
func CheckRestrictedRole(ctx context.Context, q RowQueryer, field string) error {
	const query = `
		SELECT current_user, r.rolsuper, r.rolbypassrls,
			EXISTS (SELECT 1 FROM pg_tables t
				WHERE t.schemaname = 'public'
				AND pg_has_role(current_user, t.tableowner, 'MEMBER')),
			EXISTS (SELECT 1 FROM pg_roles s
				WHERE (s.rolsuper OR s.rolbypassrls) AND s.oid <> r.oid
				AND pg_has_role(current_user, s.oid, 'MEMBER'))
		FROM pg_roles r WHERE r.rolname = current_user`
	var role string
	var super, bypass, owner, privilegedMember bool
	if err := q.QueryRowContext(ctx, query).Scan(&role, &super, &bypass, &owner, &privilegedMember); err != nil {
		return fmt.Errorf("%s: check the connection role: %w", field, err)
	}
	var why string
	switch {
	case super:
		why = "is a superuser"
	case bypass:
		why = "can bypass row-level security (BYPASSRLS)"
	case privilegedMember:
		why = "is a member of a superuser or BYPASSRLS role"
	case owner:
		why = "owns, or is a member of the owner of, the schema's tables"
	default:
		return nil
	}
	want, ok := restrictedRoles[field]
	if !ok {
		want = "its restricted role"
	}
	return fmt.Errorf("%s connects as %q, which %s, so tenant isolation would not apply. crosscodexd refuses to run as such a role: connect %s as %s, and run migrations with `crosscodexd migrate` (database.migration_dsn)",
		field, role, why, field, want)
}

// restrictedRoles names the role each config field CheckRestrictedRole is
// called with must connect as, for the refusal message.
var restrictedRoles = map[string]string{
	"database.dsn":       "app_user",
	"database.graph_dsn": "graph_user",
	"database.purge_dsn": "purge_user",
}
