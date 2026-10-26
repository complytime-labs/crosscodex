package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	_ "github.com/jackc/pgx/v5/stdlib"
	"go.opentelemetry.io/otel"

	"github.com/complytime-labs/crosscodex/internal/worker"
	"github.com/complytime-labs/crosscodex/pkg/config"
	dbpkg "github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/graphdb/agedriver"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/vectordb"
)

// requiredResources describes which shared infrastructure a role needs, so
// bootstrap constructs exactly that and nothing more -- in particular, so a
// worker replica never opens a database connection.
type requiredResources struct {
	db    bool // appPool + appDB + vectordb (gateway, graph, all)
	graph bool // graphDB + graphdb.GraphDB, in addition to db (graph, all)
	nats  bool // gateway, worker, graph, all
	// attachNATS, with nats: attach to the daemon's NATS server and its
	// existing audit streams (natsbus.WithExistingStreams). Admin one-shots
	// set it: embedding would start a second JetStream server on the daemon's
	// store directory, and provisioning would overwrite the daemon's stream
	// retention with this process's nats.streams values.
	attachNATS bool
	llm        bool // pipeline, worker, all; requires nats, which carries the LLM audit events
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
		return requiredResources{db: true, nats: true}
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
// It never migrates: `crosscodexd migrate` does, as the schema owner. It
// refuses a database.dsn or database.graph_dsn role that bypasses row-level
// security (superuser, BYPASSRLS, table owner), because tenant isolation
// holds only for the restricted app_user and graph_user roles, and refuses a
// schema not at the version this binary needs.
// The LLM client publishes an audit event for every call to NATS, so need.llm
// without need.nats is refused rather than run unaudited.
func buildSharedResources(ctx context.Context, cfg *config.Config, need requiredResources, opts ...ResourceOption) (*sharedResources, error) {
	if need.llm && !need.nats {
		return nil, errors.New("cannot open the LLM client without NATS: LLM audit events are published to NATS, and an LLM call must not go unaudited. Have the caller request the nats resource alongside llm")
	}
	res := &sharedResources{}

	if need.db {
		dbTracer, dbMeter := telemetry.Instrumentation("pkg/db")
		appPool, err := dbpkg.NewPool(dbpkg.NewPoolConfigFrom(
			cfg.Database.DSN, cfg.Database.GraphDSN, cfg.Database.MaxConns, cfg.Database.SSLMode, cfg.Database.Extensions,
		), dbpkg.WithTelemetry(dbTracer, dbMeter))
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
		if err := dbpkg.CheckRestrictedRole(ctx, appDB, "database.dsn"); err != nil {
			res.close()
			return nil, fmt.Errorf("check the app_user connection: %w", err)
		}
		if err := dbpkg.CheckSchemaVersion(ctx, appDB); err != nil {
			res.close()
			return nil, fmt.Errorf("check schema version: %w", err)
		}

		vdbTracer, vdbMeter := telemetry.Instrumentation("pkg/vectordb")
		vdb, err := vectordb.NewPgVectorStore(appDB, vectordb.WithTelemetry(vdbTracer, vdbMeter))
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
		if err := dbpkg.CheckRestrictedRole(ctx, graphDB, "database.graph_dsn"); err != nil {
			res.close()
			return nil, fmt.Errorf("check the graph_user connection: %w", err)
		}

		graphTracer, graphMeter := telemetry.Instrumentation("pkg/graphdb")
		gdb, err := agedriver.New(graphDB, agedriver.WithTelemetry(graphTracer, graphMeter))
		if err != nil {
			res.close()
			return nil, fmt.Errorf("create graphdb client: %w", err)
		}
		res.graphDB_ = gdb
	}

	if need.nats {
		natsTracer, natsMeter := telemetry.Instrumentation("pkg/natsbus")
		natsOpts := []natsbus.Option{natsbus.WithTelemetry(natsTracer, natsMeter)}
		if need.attachNATS {
			natsOpts = append(natsOpts, natsbus.WithExistingStreams())
		}
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
		auditor := worker.NewNATSAuditEmitterWithMetrics(res.natsClient, slog.Default(), otel.GetMeterProvider())
		llmOpts := []llmclient.Option{
			llmclient.WithTelemetry(otel.GetTracerProvider(), otel.GetMeterProvider()),
			llmclient.WithAuditEmitter(auditor),
		}
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
