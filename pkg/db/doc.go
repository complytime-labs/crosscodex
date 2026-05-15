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
//	ctx := db.ContextWithTenant(ctx, tenantID)
//	ctx  = db.ContextWithUser(ctx, userID)  // optional: limits to user's own jobs
//
//	tx, err := tenantConn.Begin(ctx)
//	if err != nil { return err }
//	defer tx.Rollback()
//
//	rows, err := tx.Query(ctx, "SELECT job_id, status FROM jobs")
//	// ... process rows ...
//	return tx.Commit()
//
// # Security Model
//
// The migration role (superuser) owns all tables and is used only for
// schema changes. Application connections use the app_user role, which
// has SELECT/INSERT/UPDATE/DELETE but no DDL privileges. Row-Level
// Security policies on every table enforce tenant isolation at the
// database level. Immutability triggers prevent modification of
// completed job data. This defense-in-depth means that even if
// application code has a bug, PostgreSQL itself blocks cross-tenant
// access and completed-data tampering.
package db
