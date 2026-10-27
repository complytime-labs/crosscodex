package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"

	"github.com/jackc/pgx/v5"

	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// Environment variables carrying role passwords for one integration run.
// .taskfiles/test/integration.yml (app_user, purge_user) and
// .taskfiles/test/e2e.yml (graph_user) generate a fresh random hex value for
// each on every task run, so every package and parallel Ginkgo node in that
// run agrees on it.
const (
	AppUserPasswordEnv   = "TEST_APP_USER_PASSWORD"
	PurgeUserPasswordEnv = "TEST_PURGE_USER_PASSWORD"
	GraphUserPasswordEnv = "TEST_GRAPH_USER_PASSWORD"
)

// NewRolePassword returns a random 32-character lowercase hex password.
func NewRolePassword() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate role password: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// RolePassword returns the role password in the environment variable env.
// It refuses an unset or non-hex value, because AlterRolePasswordSQL
// interpolates the password.
func RolePassword(env string) (string, error) {
	pw := os.Getenv(env)
	if pw == "" {
		return "", fmt.Errorf("%s is not set: run the suites through task test:integration:<name> or task test:e2e:venom, which generate a role password for each run", env)
	}
	if err := checkHex(pw); err != nil {
		return "", fmt.Errorf("%s: %w", env, err)
	}
	return pw, nil
}

// AlterRolePasswordSQL returns the statement that sets role's password.
// ALTER ROLE takes no bound parameters, so both values are interpolated:
// role is quoted with pgx.Identifier, and password must be hex, which
// cannot contain a quote.
func AlterRolePasswordSQL(role, password string) (string, error) {
	if err := checkHex(password); err != nil {
		return "", fmt.Errorf("set %s password: %w", role, err)
	}
	return fmt.Sprintf("ALTER ROLE %s WITH PASSWORD '%s'", pgx.Identifier{role}.Sanitize(), password), nil
}

// RoleDSN returns dsn with its userinfo replaced by role and password. The
// password must be hex, as NewRolePassword and RolePassword return. dsn must
// be a postgres:// or postgresql:// URL; the host, port, database and query
// (TLS parameters included) are kept. Errors never contain dsn, which
// usually carries the superuser's password, or password.
func RoleDSN(dsn, role, password string) (string, error) {
	if err := checkHex(password); err != nil {
		return "", fmt.Errorf("build %s DSN: %w", role, err)
	}
	u, err := url.Parse(dsn)
	// An opaque URL (postgres:x@host) parses, but String drops its userinfo.
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Opaque != "" {
		return "", fmt.Errorf("build %s DSN: the DSN passed to RoleDSN is not a postgres://user:password@host:port/dbname URL (it is not echoed, because it carries credentials); pass the suite's database DSN", role)
	}
	u.User = url.UserPassword(role, password)
	return u.String(), nil
}

// SetRolePassword connects to adminDSN, which must authenticate as a role
// allowed to ALTER ROLE (the integration superuser), and sets role's password
// with the statement AlterRolePasswordSQL builds, so that
// RoleDSN(dsn, role, password) authenticates afterwards.
func SetRolePassword(ctx context.Context, adminDSN, role, password string) error {
	stmt, err := AlterRolePasswordSQL(role, password)
	if err != nil {
		return err
	}
	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return fmt.Errorf("set %s password: connect as the admin role: %w", role, err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("set %s password: %w", role, err)
	}
	return nil
}

// TestID returns prefix, a dash and 8 random hex characters, a unique
// identifier for one integration fixture.
func TestID(prefix string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error (Go 1.24+); it crashes instead.
	return fmt.Sprintf("%s-%x", prefix, b)
}

func checkHex(password string) error {
	if password == "" {
		return errors.New("refusing an empty password; generate one with dbtest.NewRolePassword")
	}
	if _, err := hex.DecodeString(password); err != nil {
		return errors.New("refusing a non-hex password; generate one with dbtest.NewRolePassword")
	}
	return nil
}

// HardDeleteTenantSQL returns a superuser-only statement that removes a test
// tenant, every public row carrying its tenant_id, and its graph. The schema
// makes tenants and verdicts undeletable for every role through triggers;
// session_replication_role = replica (superuser only) skips those and FK
// triggers, which is acceptable only for disposable test databases. The ID is
// validated before it is interpolated, because DO blocks take no parameters.
func HardDeleteTenantSQL(id string) (string, error) {
	if err := tenant.ValidateTenantID(id); err != nil {
		return "", fmt.Errorf("hard delete test tenant: %w", err)
	}
	return fmt.Sprintf(`DO $hd$
DECLARE t record;
BEGIN
    SET LOCAL session_replication_role = replica;
    FOR t IN
        SELECT c.table_name FROM information_schema.columns c
        JOIN information_schema.tables tb ON tb.table_schema = c.table_schema AND tb.table_name = c.table_name
        WHERE c.table_schema = 'public' AND c.column_name = 'tenant_id'
          AND tb.table_type = 'BASE TABLE' AND c.table_name <> 'tenants'
    LOOP
        EXECUTE format('DELETE FROM public.%%I WHERE tenant_id = %%L', t.table_name, '%[1]s');
    END LOOP;
    DELETE FROM public.tenants WHERE tenant_id = '%[1]s';
    IF EXISTS (SELECT 1 FROM ag_catalog.ag_graph WHERE name::text = 'crosscodex_%[1]s') THEN
        PERFORM ag_catalog.drop_graph('crosscodex_%[1]s', true);
    END IF;
END
$hd$`, id), nil
}
