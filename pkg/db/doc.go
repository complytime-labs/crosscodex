// Package db provides PostgreSQL connection pooling, tenant-scoped Row-Level
// Security (RLS), schema migrations, and extension verification for the
// CrossCodex platform.
//
// # Startup Lifecycle
//
// A typical service initializes the database layer in three stages:
//
//  1. Run migrations (superuser connection, owns the schema):
//
//     migrator, err := db.NewMigrator(superuserDSN)
//     if err != nil { return err }
//     defer migrator.Close()
//     if err := migrator.Up(ctx); err != nil { return err }
//
//  2. Open the application pool and verify extensions:
//
//     pool, err := db.NewPool(db.PoolConfig{
//     DSN:          appUserDSN,
//     MaxOpenConns: 20,
//     Extensions:   []string{"age", "vector"},
//     })
//     if err != nil { return err }
//     defer pool.Close()
//     if err := pool.VerifyExtensions(ctx); err != nil { return err }
//
//  3. Wrap the pool for tenant-scoped access:
//
//     tenantConn := db.NewTenantPool(pool)
//
// # Tenant-Scoped Queries
//
// Every tenant-scoped operation must go through a transaction. The
// TenantPool sets app.current_tenant (and optionally app.current_user)
// via SET LOCAL, which PostgreSQL automatically reverts on rollback or
// commit. Direct Query/Exec calls on TenantPool return ErrTenantRequired
// because SET LOCAL has no effect outside a transaction.
//
//	ctx, err := tenant.WithTenant(ctx, tenantID)
//	ctx  = tenant.WithUser(ctx, userID)  // optional: limits to user's own jobs
//
//	tx, err := tenantConn.Begin(ctx)
//	if err != nil { return err }
//	defer tx.Rollback()
//
//	rows, err := tx.Query(ctx, "SELECT job_id, status FROM jobs")
//	// ... process rows ...
//	return tx.Commit()
//
// # Security Model — Three Roles
//
// Three PostgreSQL roles enforce defense-in-depth at the database level.
// Each role has the minimum privileges needed for its purpose, and none
// can escalate to another's capabilities.
//
//   - postgres (superuser): Owns all tables and extensions. Used only for
//     schema migrations and tenant provisioning (INSERT INTO tenants).
//     Never used by application code at runtime.
//
//   - app_user: SELECT/INSERT/UPDATE/DELETE on public-schema tables, with
//     Row-Level Security (RLS) policies enforcing tenant isolation per
//     transaction. Has NO access to graph schemas or ag_catalog. Cannot
//     perform DDL. This is the role used by the relational connection pool
//     (configured via database.dsn).
//
//   - graph_user: Owns per-tenant graph schemas (crosscodex_{tenant_id})
//     created by the tenant provisioning trigger. AGE cypher commands
//     internally perform DDL (creating label tables, updating sequences),
//     which requires ownership — GRANT INSERT/UPDATE/DELETE is not
//     sufficient. graph_user has USAGE/SELECT/EXECUTE on ag_catalog for
//     graph metadata queries, but has NO access to public-schema relational
//     tables. This is the role used by the graph connection pool (configured
//     via database.graph_dsn).
//
// This separation means a bug in graph-handling code cannot read or modify
// relational data (jobs, classifications, vote summaries), and a bug in
// relational code cannot modify graph structure. Even if application code
// is compromised, PostgreSQL itself blocks cross-boundary access.
//
// # Privilege Matrix
//
// The matrix below summarizes what each role can and cannot do. The
// integration tests in integration_test.go ("Role Isolation" section)
// verify every cell marked "no" and every cell marked "yes" — making
// this matrix an executable specification, not just documentation.
//
//	Capability                     postgres  app_user  graph_user
//	─────────────────────────────  ────────  ────────  ──────────
//	DDL (CREATE/ALTER/DROP)        yes       no        graph schemas only
//	Relational DML (public)        yes       yes+RLS   no
//	ag_catalog read                yes       no        yes
//	ag_catalog execute (cypher)    yes       no        yes
//	Graph schema DML               yes       no        yes (owner)
//	LOAD shared library            yes       no        no
//	Tenant provisioning (INSERT)   yes       via RLS   no
//	TRUNCATE relational tables     yes       no        no
//	Disable RLS / triggers         yes       no        no
//
// The AGE shared library is loaded at server startup via
// shared_preload_libraries=age in postgresql.conf. Application code must
// NOT use the LOAD command — PostgreSQL restricts LOAD to superusers.
// shared_preload_libraries makes the library available to all sessions
// without per-session LOAD calls.
package db
