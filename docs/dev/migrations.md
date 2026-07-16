# Database Migrations

This document covers creating, running, and troubleshooting schema migrations for the `pkg/db` package.

## Overview

CrossCodex uses [golang-migrate/migrate](https://github.com/golang-migrate/migrate) (v4) with the `iofs` source driver. Migration SQL files live in `pkg/db/migrations/` and are embedded into the binary at compile time via `//go:embed`. There is no external migration CLI dependency at runtime.

## Migration File Structure

```
pkg/db/migrations/
  embed.go                           # //go:embed *.sql → migrations.FS
  001_create_tenants.up.sql
  001_create_tenants.down.sql
  002_create_jobs.up.sql
  002_create_jobs.down.sql
  ...
  009_tenant_graph_lifecycle.up.sql
  009_tenant_graph_lifecycle.down.sql
  010_create_controls.up.sql
  010_create_controls.down.sql
  011_create_requires_tables.up.sql
  011_create_requires_tables.down.sql
```

Each migration is a pair of SQL files:

- `NNN_description.up.sql` — applies the change (create table, add column, enable RLS, etc.)
- `NNN_description.down.sql` — reverses the change (drop table, remove column, disable RLS, etc.)

The numeric prefix (`NNN`) determines execution order. Use zero-padded, sequential integers. Never reuse or reorder a number that has been applied to any environment.

## Creating a New Migration

1. Determine the next sequence number by checking the highest existing prefix in `pkg/db/migrations/`.

2. Create both files:

   ```bash
   touch pkg/db/migrations/012_your_description.up.sql
   touch pkg/db/migrations/012_your_description.down.sql
   ```

3. Write the SQL. The up migration should be idempotent where possible (`CREATE TABLE IF NOT EXISTS`, `DO $$ ... $$`). The down migration should cleanly reverse the up migration.

4. The `embed.go` file uses a glob (`//go:embed *.sql`) so new `.sql` files in the directory are picked up automatically. No code changes are needed to register a new migration.

5. Build and run tests to verify the migration applies and reverses cleanly.

### Naming Conventions

- Use lowercase with underscores: `012_add_audit_log.up.sql`
- Start with a verb describing the action: `create_`, `add_`, `enable_`, `drop_`
- Be specific: `013_add_retry_count_to_jobs` not `013_update_jobs`

### Writing Down Migrations

Every up migration must have a corresponding down migration. The down migration should reverse the up migration completely so that `Up` followed by the reverse produces the original schema state. Use `CASCADE` on `DROP TABLE` when the table has foreign key dependents. For data-destructive operations (dropping columns with data), add a comment acknowledging the data loss.

## Running Migrations

Migrations run programmatically through the `Migrator` interface. There is no standalone migration CLI.

### Application Startup

The standard startup sequence runs migrations with the superuser (schema owner) DSN, then opens the application pool with the restricted `app_user` DSN:

```go
// 1. Run migrations as superuser.
migrator, err := db.NewMigrator(superuserDSN)
if err != nil {
    return fmt.Errorf("create migrator: %w", err)
}
defer migrator.Close()

if err := migrator.Up(ctx); err != nil {
    return fmt.Errorf("run migrations: %w", err)
}

// 2. Open application pool as app_user.
pool, err := db.NewPool(db.PoolConfig{
    DSN:          appUserDSN,
    MaxOpenConns: 20,
    Extensions:   []string{"age", "vector"},
}) // signature: NewPool(cfg PoolConfig, opts ...Option)
if err != nil {
    return fmt.Errorf("create pool: %w", err)
}
defer pool.Close()
```

`Migrator.Up` is idempotent. If all migrations have already been applied, it returns nil (not an error).

### Checking Current Version

```go
version, dirty, err := migrator.Version(ctx)
```

- `version` — the sequence number of the last successfully applied migration (0 if none applied).
- `dirty` — true if the previous migration failed partway through. See [Dirty State Recovery](#dirty-state-recovery).

## Integration Test Environment

Integration tests run against a containerized PostgreSQL instance with AGE and pgvector extensions.

### Running Database Integration Tests

```bash
task test:integration:db
```

This builds a custom PostgreSQL container image from `pkg/db/testdata/Containerfile` (which includes the AGE and pgvector extensions), starts it on port 15432, runs the database integration tests, and tears down the container on completion.

### Running All Integration Tests

```bash
task test:integration:all
```

This starts the required containers, sets `TEST_DATABASE_DSN`, and runs tests tagged with `//go:build integration`.

### Cleaning Up Test Containers

```bash
task test:integration:clean
```

### Manual Connection

To inspect the test database manually:

```bash
psql "postgres://username:password@localhost:15432/dbname?sslmode=verify-full&sslrootcert=test/certs/ca.pem&sslcert=test/certs/client.pem&sslkey=test/certs/client-key.pem"
```

## Rollback Procedures

### Rolling Back the Last Migration

golang-migrate does not expose a single-step rollback through the `Migrator` interface in this codebase. The `Migrator` interface provides `Up` (apply all pending) and `Version` (check state) but not `Down` or `Steps`.

To roll back in an emergency, connect to the database directly and:

1. Run the contents of the relevant `NNN_description.down.sql` file manually.
2. Update the `schema_migrations` table to reflect the new version:

   ```sql
   UPDATE schema_migrations SET version = <previous_version>, dirty = false;
   ```

If you need programmatic rollback capability, extend the `Migrator` interface to expose the underlying `migrate.Migrate.Steps(-1)` method.

### Dirty State Recovery

A migration is "dirty" when it failed partway through, leaving the `schema_migrations` table marked with `dirty = true`. The `Migrator.Up` method detects this and returns `ErrMigrationDirty` with a message indicating manual intervention is required.

To recover:

1. Inspect the database to determine what the partially-applied migration did.
2. Either complete the migration manually or reverse the partial changes.
3. Update the `schema_migrations` table:

   ```sql
   -- If you reversed the partial changes (reset to previous version):
   UPDATE schema_migrations SET version = <previous_version>, dirty = false;

   -- If you completed the migration manually:
   UPDATE schema_migrations SET dirty = false;
   ```

4. Re-run `Migrator.Up` to continue with remaining migrations.

### Checksum Verification Failure

golang-migrate tracks applied migration versions in the `schema_migrations` table but does not store file checksums. If
a migration SQL file is edited after it has been applied, the database schema will silently diverge from what the file
describes. This is dangerous because future rollbacks will execute SQL that does not match the actual schema state, and
developers reading the migration history will have a false picture of the database.

**Rule: never edit a migration file that has been applied to any environment.** If you need to change something an earlier migration created, write a new migration with the next sequence number.

If a migration file was edited accidentally:

1. Compare the current file contents against what was actually executed. If the database preserves query logs or you have the original file in version control, use `git log -p -- pkg/db/migrations/NNN_description.up.sql` to find the original SQL.
2. Determine whether the edit changed behavior (whitespace-only changes are harmless; DDL changes are not).
3. If the edit changed behavior, choose one of:
   - **Restore the original file** from version control and create a new corrective migration with the intended change.
   - **Create a corrective migration** that brings the schema from its current state to the desired state, and update a comment in the edited file noting it no longer matches what was applied.

Never force-apply a modified migration by resetting the version in `schema_migrations`. That trades one inconsistency for another.

## Security Notes

- Migrations run as the superuser role, which owns all tables and bypasses RLS. This is intentional: the migration role is the only role with DDL privileges.
- The application role (`app_user`) cannot run migrations, alter tables, disable RLS, or disable triggers. This privilege separation is enforced by PostgreSQL GRANTs configured in the migration files themselves.
- Never embed credentials in migration SQL. Database roles and passwords are configured through DSN connection strings
  passed to `NewMigrator` and `NewPool`.

