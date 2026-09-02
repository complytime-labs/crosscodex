package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/jackc/pgx/v5/stdlib"
	"go.opentelemetry.io/otel"

	"github.com/complytime-labs/crosscodex/pkg/config"
	dbpkg "github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/graphdb/agedriver"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/vectordb"
)

// requiredResources describes which shared infrastructure a role needs, so
// bootstrap constructs exactly that and nothing more -- in particular, so a
// worker replica never opens a database connection.
type requiredResources struct {
	db    bool // appPool + appDB + vectordb (gateway, graph, all)
	graph bool // graphDB + graphdb.GraphDB, in addition to db (graph, all)
	nats  bool // gateway, worker, graph, all
	llm   bool // gateway, worker, all
}

// ResourceOption customizes sharedResources construction. Used by tests to
// override a resource buildSharedResources would otherwise construct itself
// (e.g. injecting a fake LLM client instead of dialing a real gateway).
// Production code (main.go) never supplies any option.
type ResourceOption func(*sharedResources)

// WithLLMClient overrides the LLM client with client instead of constructing
// one via llmclient.NewClient(cfg.LLM).
func WithLLMClient(client llmclient.Client) ResourceOption {
	return func(r *sharedResources) { r.llmClient = client }
}

// resourcesForRole returns the zero value for any role it doesn't
// recognize; bootstrap's role switch is the single source of truth for
// which roles are actually supported; config.ResolveRole is the single
// source of truth for which role names are valid at all.
func resourcesForRole(role string) requiredResources {
	switch role {
	case config.RoleGateway:
		return requiredResources{db: true}
	case config.RolePipeline:
		return requiredResources{db: true, nats: true, llm: true}
	case config.RoleWorker:
		return requiredResources{nats: true, llm: true}
	case config.RoleGraph:
		return requiredResources{db: true, graph: true, nats: true}
	case config.RoleAll:
		return requiredResources{db: true, graph: true, nats: true, llm: true}
	default:
		return requiredResources{}
	}
}

// sharedResources holds every dependency more than one role's attach
// function might need, constructed at most once per process regardless of
// how many roles are active (relevant for the "all" role).
type sharedResources struct {
	appDB, graphDB *sql.DB
	appPool        dbpkg.Pool
	vectorDB       vectordb.VectorDB
	graphDB_       graphdb.GraphDB
	natsClient     natsbus.Client
	llmClient      llmclient.Client
}

// close releases every resource sharedResources constructed, in reverse
// order of acquisition. Safe to call on a partially-populated value.
func (r *sharedResources) close() {
	if r.llmClient != nil {
		r.llmClient.Close()
	}
	if r.natsClient != nil {
		r.natsClient.Close()
	}
	if r.graphDB != nil {
		r.graphDB.Close()
	}
	if r.appDB != nil {
		r.appDB.Close()
	}
	if r.appPool != nil {
		r.appPool.Close()
	}
}

// buildSharedResources constructs exactly the resources need declares.
// Every partially-constructed resource is released on any error return path.
func buildSharedResources(ctx context.Context, cfg *config.Config, need requiredResources, opts ...ResourceOption) (*sharedResources, error) {
	res := &sharedResources{}

	if need.db {
		migrator, err := dbpkg.NewMigrator(cfg.Database.DSN)
		if err != nil {
			return nil, fmt.Errorf("create migrator: %w", err)
		}
		if err := migrator.Up(ctx); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			migrator.Close()
			return nil, fmt.Errorf("run migrations: %w", err)
		}
		migrator.Close()

		appPool, err := dbpkg.NewPool(dbpkg.NewPoolConfigFrom(
			cfg.Database.DSN, cfg.Database.GraphDSN, cfg.Database.MaxConns, cfg.Database.SSLMode, cfg.Database.Extensions,
		))
		if err != nil {
			return nil, fmt.Errorf("connect app_user pool: %w", err)
		}
		res.appPool = appPool
		if err := appPool.VerifyExtensions(ctx); err != nil {
			res.close()
			return nil, fmt.Errorf("verify extensions: %w", err)
		}

		appDB, err := sql.Open("pgx", cfg.Database.DSN)
		if err != nil {
			res.close()
			return nil, fmt.Errorf("open app_user sql.DB: %w", err)
		}
		res.appDB = appDB

		vdb, err := vectordb.NewPgVectorStore(appDB)
		if err != nil {
			res.close()
			return nil, fmt.Errorf("create vectordb store: %w", err)
		}
		res.vectorDB = vdb
	}

	if need.graph {
		graphDB, err := sql.Open("pgx", cfg.Database.GraphDSN)
		if err != nil {
			res.close()
			return nil, fmt.Errorf("open graph_user sql.DB: %w", err)
		}
		res.graphDB = graphDB
		if err := graphDB.PingContext(ctx); err != nil {
			res.close()
			return nil, fmt.Errorf("ping graph_user sql.DB: %w", err)
		}

		gdb, err := agedriver.New(
			graphDB,
			agedriver.WithTelemetry(
				otel.Tracer("graphdb"),
				otel.GetMeterProvider().Meter("graphdb"),
			),
		)
		if err != nil {
			res.close()
			return nil, fmt.Errorf("create graphdb client: %w", err)
		}
		res.graphDB_ = gdb
	}

	if need.nats {
		var natsOpts []natsbus.Option
		tlsCfg, err := natsTLSConfig(ctx, cfg)
		if err != nil {
			res.close()
			return nil, err
		}
		if tlsCfg != nil {
			natsOpts = append(natsOpts, natsbus.WithTLSConfig(tlsCfg))
		}
		natsClient, err := natsbus.New(cfg.NATS, natsOpts...)
		if err != nil {
			res.close()
			return nil, fmt.Errorf("create NATS client: %w", err)
		}
		res.natsClient = natsClient
	}

	if need.llm {
		var llmOpts []llmclient.Option
		httpClient, err := llmHTTPClient(ctx, cfg)
		if err != nil {
			res.close()
			return nil, err
		}
		if httpClient != nil {
			llmOpts = append(llmOpts, llmclient.WithHTTPClient(httpClient))
		}
		llmClient, err := llmclient.NewClient(cfg.LLM, llmOpts...)
		if err != nil {
			res.close()
			return nil, fmt.Errorf("create LLM client: %w", err)
		}
		res.llmClient = llmClient
	}

	for _, opt := range opts {
		opt(res)
	}

	return res, nil
}
