package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/complytime-labs/crosscodex/internal/graph"
	"github.com/complytime-labs/crosscodex/pkg/config"
	dbpkg "github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/vectordb"
)

// runtime holds every resource bootstrap constructs, for orderly shutdown.
//
// FOLLOW-UP(#128): this is a slice of the daemon. pipeline.Service, the
// analyzer registry, the LLM worker, and the gateway HTTP server are not
// wired here — this bootstrap exists solely to give the tenant-aware
// PGResolver (internal/graph/resolver_pg.go) a real running call site via
// graph.Service's NATS subscriber.
type runtime struct {
	appDB, graphDB *sql.DB
	appPool        dbpkg.Pool
	natsClient     natsbus.Client
	graphService   *graph.Service
}

// close releases every resource bootstrap constructed, in reverse order of
// acquisition. Safe to call on a partially-populated runtime.
func (rt *runtime) close() {
	if rt.natsClient != nil {
		rt.natsClient.Close()
	}
	if rt.graphDB != nil {
		rt.graphDB.Close()
	}
	if rt.appDB != nil {
		rt.appDB.Close()
	}
	if rt.appPool != nil {
		rt.appPool.Close()
	}
}

// bootstrap constructs the resources the graph-materialization subscriber
// needs to run: the app_user pool (shared by the tenant-aware PGResolver and
// the vector store), the graph_user *sql.DB (AGE cypher queries only), and
// the NATS client. Every partially-constructed resource is released on any
// error return path.
func bootstrap(ctx context.Context, cfg *config.Config) (*runtime, error) {
	migrator, err := dbpkg.NewMigrator(cfg.Database.DSN)
	if err != nil {
		return nil, fmt.Errorf("create migrator: %w", err)
	}
	if err := migrator.Up(ctx); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		migrator.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}
	migrator.Close()

	// app_user pool: backs both the tenant-aware PGResolver (RLS-enforced
	// reads of analysis_results) and the vector store. See pkg/db/doc.go's
	// three-role model — app_user has relational access, graph_user does not.
	appPool, err := dbpkg.NewPool(dbpkg.NewPoolConfigFrom(
		cfg.Database.DSN, cfg.Database.GraphDSN, cfg.Database.MaxConns, cfg.Database.SSLMode, cfg.Database.Extensions,
	))
	if err != nil {
		return nil, fmt.Errorf("connect app_user pool: %w", err)
	}
	if err := appPool.VerifyExtensions(ctx); err != nil {
		appPool.Close()
		return nil, fmt.Errorf("verify extensions: %w", err)
	}

	// graphdb.New and vectordb.NewPgVectorStore take a raw *sql.DB, not the
	// db.Pool abstraction -- neither has a production constructor before this
	// task (pkg/db/doc.go documents the role split; this is the first real
	// caller of both).
	appDB, err := sql.Open("pgx", cfg.Database.DSN)
	if err != nil {
		appPool.Close()
		return nil, fmt.Errorf("open app_user sql.DB: %w", err)
	}
	graphDB, err := sql.Open("pgx", cfg.Database.GraphDSN)
	if err != nil {
		appPool.Close()
		appDB.Close()
		return nil, fmt.Errorf("open graph_user sql.DB: %w", err)
	}
	if err := graphDB.PingContext(ctx); err != nil {
		appPool.Close()
		appDB.Close()
		graphDB.Close()
		return nil, fmt.Errorf("ping graph_user sql.DB: %w", err)
	}

	gdb, err := graphdb.New(graphDB)
	if err != nil {
		appPool.Close()
		appDB.Close()
		graphDB.Close()
		return nil, fmt.Errorf("create graphdb client: %w", err)
	}
	vdb, err := vectordb.NewPgVectorStore(appDB)
	if err != nil {
		appPool.Close()
		appDB.Close()
		graphDB.Close()
		return nil, fmt.Errorf("create vectordb store: %w", err)
	}

	natsClient, err := natsbus.New(cfg.NATS)
	if err != nil {
		appPool.Close()
		appDB.Close()
		graphDB.Close()
		return nil, fmt.Errorf("create NATS client: %w", err)
	}

	// Tenant-aware resolver: reads through db.TenantConnection so RLS on
	// analysis_results applies (see internal/graph/resolver_pg.go).
	tenantConn := dbpkg.NewTenantPool(appPool)
	resolver := graph.NewPGResolver(tenantConn)

	graphSvc := graph.New(gdb, vdb, natsClient, graph.WithResolver(resolver))

	return &runtime{
		appDB:        appDB,
		graphDB:      graphDB,
		appPool:      appPool,
		natsClient:   natsClient,
		graphService: graphSvc,
	}, nil
}
