package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"go.opentelemetry.io/otel"

	crosscodexv1connect "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/complytime-labs/crosscodex/internal/admin"
	"github.com/complytime-labs/crosscodex/internal/analysis"
	"github.com/complytime-labs/crosscodex/internal/catalog"
	"github.com/complytime-labs/crosscodex/internal/gateway"
	"github.com/complytime-labs/crosscodex/internal/gateway/backend"
	"github.com/complytime-labs/crosscodex/internal/graph"
	"github.com/complytime-labs/crosscodex/internal/pipeline"
	pipelineattestation "github.com/complytime-labs/crosscodex/internal/pipeline/attestation"
	"github.com/complytime-labs/crosscodex/internal/results"
	"github.com/complytime-labs/crosscodex/internal/synthesis"
	"github.com/complytime-labs/crosscodex/internal/worker"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/config"
	dbpkg "github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/oscal"
	"github.com/complytime-labs/crosscodex/pkg/prompt"
	"github.com/complytime-labs/crosscodex/pkg/retention"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
	"github.com/complytime-labs/crosscodex/pkg/tlsconfig"
)

// runtime holds every service this process started, for orderly start/stop.
// Exactly which fields are non-nil depends on the resolved role.
type runtime struct {
	shared *sharedResources

	graphService    *graph.Service
	workerService   *worker.Worker
	pipelineService *pipeline.Service
	pipelineServer  *pipeline.Server
	gatewayServer   *gateway.Server
	healthServer    *http.Server

	// retentionPurgePool is a dedicated purge_user pool built from
	// config.Database.PurgeDSN for the retention Purger. It is nil when
	// retention is not wired or when PurgeDSN is unset (in which case the
	// Purger reuses the shared app pool and fails closed at the DB trigger).
	retentionPurgePool dbpkg.Pool
}

// close releases every resource bootstrap constructed. Safe to call on a
// partially-populated runtime; safe to call after stop.
func (rt *runtime) close() {
	// The retention purge pool is owned by runtime (not sharedResources) and
	// is only ever a distinct pool from shared.appPool, so closing it here
	// never double-closes the app pool.
	if rt.retentionPurgePool != nil {
		rt.retentionPurgePool.Close()
	}
	if rt.shared != nil {
		rt.shared.close()
	}
}

// start starts every non-nil service in rt. All Start methods here are
// non-blocking (they return once listeners/subscriptions are established),
// so start returns as soon as everything is up.
func (rt *runtime) start(ctx context.Context) error {
	if rt.graphService != nil {
		if err := rt.graphService.Start(ctx); err != nil {
			return fmt.Errorf("start graph service: %w", err)
		}
	}
	if rt.workerService != nil {
		if err := rt.workerService.Start(ctx); err != nil {
			return fmt.Errorf("start worker service: %w", err)
		}
	}
	if rt.gatewayServer != nil {
		if err := rt.gatewayServer.Start(); err != nil {
			return fmt.Errorf("start gateway server: %w", err)
		}
	}
	if rt.pipelineService != nil {
		if err := rt.pipelineService.Start(ctx); err != nil {
			return fmt.Errorf("start pipeline service: %w", err)
		}
	}
	if rt.pipelineServer != nil {
		if err := rt.pipelineServer.Start(); err != nil {
			return fmt.Errorf("start pipeline server: %w", err)
		}
	}
	if rt.healthServer != nil {
		go func() {
			if err := rt.healthServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				fmt.Printf("health server error: %v\n", err)
			}
		}()
	}
	return nil
}

// stop stops every non-nil service in rt, collecting every error rather than
// stopping at the first one, so a failure to stop one service doesn't skip
// draining the others.
//
// Order matters: the inbound network listeners (health, gateway HTTP, pipeline
// RPC) are drained first, then the background services those listeners hand
// work to (pipeline, worker, graph). Each listener's Shutdown blocks until its
// in-flight requests finish, so by the time pipelineService.Stop runs no new
// RPC or gateway request can reach an already-torn-down service. Stopping the
// services first (as an earlier version did) let a request accepted mid-drain
// reach a stopped pipeline.Service.
func (rt *runtime) stop(ctx context.Context) error {
	var errs []error
	if rt.healthServer != nil {
		if err := rt.healthServer.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("stop health server: %w", err))
		}
	}
	if rt.gatewayServer != nil {
		if err := rt.gatewayServer.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("stop gateway server: %w", err))
		}
	}
	if rt.pipelineServer != nil {
		if err := rt.pipelineServer.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("stop pipeline server: %w", err))
		}
	}
	if rt.pipelineService != nil {
		if err := rt.pipelineService.Stop(ctx); err != nil {
			errs = append(errs, fmt.Errorf("stop pipeline service: %w", err))
		}
	}
	if rt.workerService != nil {
		if err := rt.workerService.Stop(ctx); err != nil {
			errs = append(errs, fmt.Errorf("stop worker service: %w", err))
		}
	}
	if rt.graphService != nil {
		if err := rt.graphService.Stop(ctx); err != nil {
			errs = append(errs, fmt.Errorf("stop graph service: %w", err))
		}
	}
	return errors.Join(errs...)
}

// bootstrap resolves cfg.Role (already canonicalized by config.validate)
// and constructs exactly the services that role needs.
func bootstrap(ctx context.Context, cfg *config.Config, opts ...ResourceOption) (*runtime, error) {
	need := resourcesForRole(cfg.Role)
	shared, err := buildSharedResources(ctx, cfg, need, opts...)
	if err != nil {
		return nil, err
	}
	rt := &runtime{shared: shared}

	switch cfg.Role {
	case config.RoleGraph:
		if err := attachGraph(cfg, shared, rt); err != nil {
			rt.close()
			return nil, err
		}
	case config.RoleWorker:
		if err := attachWorker(cfg, shared, rt); err != nil {
			rt.close()
			return nil, err
		}
	case config.RolePipeline:
		if err := attachPipeline(ctx, cfg, shared, rt); err != nil {
			rt.close()
			return nil, err
		}
	case config.RoleGateway:
		if err := attachGateway(ctx, cfg, shared, rt); err != nil {
			rt.close()
			return nil, err
		}
	case config.RoleAll:
		if err := attachGraph(cfg, shared, rt); err != nil {
			rt.close()
			return nil, err
		}
		if err := attachWorker(cfg, shared, rt); err != nil {
			rt.close()
			return nil, err
		}
		pipelineSvc, err := buildPipelineService(cfg, shared)
		if err != nil {
			rt.close()
			return nil, err
		}
		rt.pipelineService = pipelineSvc
		if err := attachAllGateway(ctx, cfg, shared, rt, pipelineSvc); err != nil {
			rt.close()
			return nil, err
		}
		// The gateway role's own /healthz already covers process liveness
		// for every co-located service; drop the dedicated listener
		// attachGraph/attachWorker each set so "all" doesn't bind
		// cfg.Health.Addr twice or leave a second, redundant listener up.
		rt.healthServer = nil
	default:
		rt.close()
		return nil, fmt.Errorf("role %q: bootstrap does not support this role yet", cfg.Role)
	}

	return rt, nil
}

// noopAuditEmitter satisfies authn.AuditEmitter for allTenantAuthRegistry.
type noopAuditEmitter struct{}

func (noopAuditEmitter) EmitAuthEvent(_ context.Context, _ *authn.AuthEvent) error { return nil }

// allTenantAuthRegistry builds a single-tenant X.509 registry that maps any
// valid client certificate to cfg.Tenants.DefaultTenant. This mirrors the
// embedded CLI's single-node identity model and is what SubmitDocument needs
// to resolve a tenant for both gateway-serving roles (all and gateway).
func allTenantAuthRegistry(defaultTenant string) (*authn.Registry, error) {
	x509Auth, err := authn.NewX509Authenticator(authn.X509Config{
		SingleTenant:  true,
		DefaultTenant: defaultTenant,
	})
	if err != nil {
		return nil, fmt.Errorf("create X.509 authenticator: %w", err)
	}
	tracer, meter := telemetry.Instrumentation("pkg/authn")
	return authn.NewRegistry(noopAuditEmitter{}, []authn.Authenticator{x509Auth}, authn.WithTelemetry(tracer, meter))
}

// buildIngestGateway builds the gateway.Service that serves catalog import for
// role (all or gateway): single-tenant X.509 authn, pass-through ingestion
// into local storage, and a catalog service that persists controls and
// publishes them for graph projection. It holds no graph credentials; the
// graph role projects Control nodes. Refuses a default tenant that
// `crosscodexd migrate` has not provisioned or that is deleted. extra
// appends role-specific backends (role all adds graph queries).
func buildIngestGateway(ctx context.Context, role string, cfg *config.Config, shared *sharedResources, pipelineBackend gateway.PipelineBackend, extra ...gateway.ServiceOption) (*gateway.Service, error) {
	if cfg.Tenants.DefaultTenant == "" {
		return nil, fmt.Errorf("%s role: tenants.default_tenant must be set", role)
	}
	if cfg.Storage.Objects.BasePath == "" {
		return nil, fmt.Errorf("%s role: storage.objects.base_path must be set", role)
	}
	// Catalog import resolves the caller's tenant from its X.509 client
	// certificate. Resolve the same "gateway-server" target gateway.NewServer
	// listens with: without a client CA (tls.mode off or server-only) the
	// listener never verifies a client cert, so every import would fail
	// Unauthenticated at request time instead of here.
	resolvedTLS, err := tlsconfig.BuildTLSConfig(ctx, cfg.TLS, "gateway-server")
	if err != nil {
		return nil, fmt.Errorf("%s role: resolve gateway-server TLS config: %w", role, err)
	}
	if resolvedTLS == nil || resolvedTLS.ClientCAs == nil {
		return nil, fmt.Errorf(`%s role: catalog import authenticates clients by X.509 certificate, so tls.mode (or tls.targets.gateway-server.mode) must be "mutual" with a CA configured`, role)
	}

	// The default tenant is provisioned by `crosscodexd migrate` (as the
	// schema owner); the daemon only checks it. enter_tenant refuses a
	// deleted tenant.
	tctx, err := tenant.WithTenant(ctx, cfg.Tenants.DefaultTenant)
	if err != nil {
		return nil, fmt.Errorf("%s role: tenants.default_tenant: %w", role, err)
	}
	tx, err := dbpkg.NewTenantPool(shared.appPool).Begin(tctx)
	if dbpkg.IsPgErrorCode(err, "42501") {
		return nil, fmt.Errorf("%s role: default tenant %q is deleted. Restore it with crosscodexd admin tenant restore --tenant %s --by <operator> --reason <text>, or set tenants.default_tenant to an active tenant: %w",
			role, cfg.Tenants.DefaultTenant, cfg.Tenants.DefaultTenant, err)
	}
	if err != nil {
		return nil, fmt.Errorf("%s role: check default tenant %q: %w", role, cfg.Tenants.DefaultTenant, err)
	}
	var found int
	err = tx.QueryRow(tctx, "SELECT 1 FROM tenants WHERE tenant_id = $1", cfg.Tenants.DefaultTenant).Scan(&found)
	_ = tx.Rollback()
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%s role: default tenant %q does not exist. Run `crosscodexd migrate`, which provisions tenants.default_tenant, then start crosscodexd again", role, cfg.Tenants.DefaultTenant)
	}
	if err != nil {
		return nil, fmt.Errorf("%s role: check default tenant %q: %w", role, cfg.Tenants.DefaultTenant, err)
	}

	registry, err := allTenantAuthRegistry(cfg.Tenants.DefaultTenant)
	if err != nil {
		return nil, fmt.Errorf("create auth registry: %w", err)
	}

	storageTracer, storageMeter := telemetry.Instrumentation("pkg/storage")
	storageProvider, err := storage.NewLocal(cfg.Storage.Objects.BasePath, cfg.Tenants.DefaultTenant,
		storage.WithLocalTelemetry(storageTracer, storageMeter))
	if err != nil {
		return nil, fmt.Errorf("create storage provider: %w", err)
	}

	oscalTracer, _ := telemetry.Instrumentation("pkg/oscal")
	catalogTracer, catalogMeter := telemetry.Instrumentation("internal/catalog")
	catalogSvc := catalog.NewService(
		catalog.WithParser(oscal.NewParser("", oscal.WithParserTracer(oscalTracer))),
		catalog.WithStore(catalog.NewPGStore(dbpkg.NewTenantPool(shared.appPool))),
		catalog.WithStorage(storageProvider),
		catalog.WithBus(shared.natsClient),
		catalog.WithLogger(slog.Default()),
		catalog.WithTracer(catalogTracer),
		catalog.WithMeter(catalogMeter),
	)
	ingestion := backend.NewPassthroughIngestion(storageProvider)

	gatewayTracer, gatewayMeter := telemetry.Instrumentation("internal/gateway")
	opts := []gateway.ServiceOption{
		gateway.WithAuthn(registry),
		gateway.WithIngestionBackend(ingestion),
		gateway.WithCatalogBackend(catalogSvc),
		gateway.WithPipelineBackend(pipelineBackend),
		gateway.WithMappingsBackend(newMappingsBackend(shared)),
		gateway.WithAdminBackend(gateway.NewPoolAdminBackend(shared.appPool)),
		gateway.WithMaxUploadSize(cfg.Server.MaxUploadSize),
		gateway.WithLogger(slog.Default()),
		gateway.WithTelemetry(gatewayTracer, gatewayMeter),
	}
	return gateway.NewService(append(opts, extra...)...), nil
}

// attachAllGateway wires the all-in-one role's gateway: buildIngestGateway's
// authn, pass-through ingestion and catalog import, the in-process pipeline,
// and retention admin. It also confirms the default tenant's AGE graph exists
// so the co-located graph role can project into it.
func attachAllGateway(ctx context.Context, cfg *config.Config, shared *sharedResources, rt *runtime, backendPipeline gateway.PipelineBackend) error {
	if rt.graphService == nil {
		return errors.New("attach gateway: graph service is not built; attachGraph must run before attachAllGateway so the gateway can serve graph queries")
	}
	gatewaySvc, err := buildIngestGateway(ctx, config.RoleAll, cfg, shared, backendPipeline,
		gateway.WithGraphBackend(rt.graphService))
	if err != nil {
		return err
	}
	// The tenants insert trigger created the tenant's AGE graph; graph_user
	// cannot create one, so CreateGraph only confirms it exists for the
	// co-located graph role to project into. Only role all holds graph
	// credentials here; the standalone gateway role does not.
	if err := shared.graphDB_.CreateGraph(ctx, cfg.Tenants.DefaultTenant); err != nil {
		return fmt.Errorf("provision tenant graph: %w", err)
	}

	adminSvc, purgePool, err := buildRetentionAdmin(ctx, cfg, shared, slog.Default())
	if err != nil {
		return fmt.Errorf("build retention admin: %w", err)
	}
	rt.retentionPurgePool = purgePool

	server, err := gateway.NewServer(ctx, gateway.ServerConfig{
		TenantStatus: gateway.NewPoolTenantStatus(shared.appPool),
		Addr:         cfg.Server.Addr,
		TLS:          cfg.TLS,
		Service:      gatewaySvc,
		Admin:        adminSvc,
	})
	if err != nil {
		return fmt.Errorf("create gateway server: %w", err)
	}
	rt.gatewayServer = server
	return nil
}

// disabledArchive is a storage.Provider used when no archive backend is
// configured. Reads and writes fail closed with storage.ErrArchiveDisabled so
// the retention Engine's archive-before-purge invariant holds: a candidate can
// never be purged when there is nowhere to archive it. Dry-run scans, stats,
// and legal-hold management remain fully functional; only the archive+purge
// path is inert until an archive backend is configured.
type disabledArchive struct{}

func (disabledArchive) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, storage.ErrArchiveDisabled
}
func (disabledArchive) Put(context.Context, string, io.Reader) error {
	return storage.ErrArchiveDisabled
}
func (disabledArchive) Delete(context.Context, string) error { return storage.ErrArchiveDisabled }
func (disabledArchive) List(context.Context, string) ([]storage.ObjectMetadata, error) {
	return nil, storage.ErrArchiveDisabled
}
func (disabledArchive) Exists(context.Context, string) (bool, error) {
	return false, storage.ErrArchiveDisabled
}
func (disabledArchive) Stat(context.Context, string) (*storage.ObjectMetadata, error) {
	return nil, storage.ErrArchiveDisabled
}
func (disabledArchive) Close() error { return nil }

// buildRetentionAdmin constructs the AdminService handler for the all-in-one
// role. It returns the handler plus a dedicated purge_user pool (nil when
// PurgeDSN is unset, in which case the Purger reuses the app pool and fails
// closed at the DB immutability trigger). The caller owns the returned pool and
// must close it via runtime.close.
func buildRetentionAdmin(ctx context.Context, cfg *config.Config, shared *sharedResources, logger *slog.Logger) (crosscodexv1connect.AdminServiceHandler, dbpkg.Pool, error) {
	wiring, err := buildRetentionWiring(ctx, cfg, shared, logger)
	if err != nil {
		return nil, nil, err
	}
	driftSource := func(ctx context.Context) ([]retention.StreamDrift, error) {
		configured := natsbus.ConfiguredAuditRetention(cfg.NATS.Streams)
		live, err := shared.natsClient.AuditStreamRetention(ctx)
		if err != nil {
			return nil, err
		}
		return retention.VerifyStreamRetention(configured, live), nil
	}
	return admin.NewService(wiring.engineFor, wiring.defaultPolicy, wiring.holds, driftSource), wiring.purgePool, nil
}

// retentionWiring bundles the shared retention resources both the gateway
// AdminService handler and the on-host `admin retention scan` one-shot build
// engines from: the configured default policy, the legal-hold store, the
// per-tenant engine factory, and the dedicated purge_user pool the caller must
// Close.
type retentionWiring struct {
	engineFor     func(tenantID string, policy retention.Policy) (*retention.Engine, error)
	defaultPolicy retention.Policy
	holds         retention.HoldStore
	// purgePool is the dedicated purge_user pool (nil when PurgeDSN is unset, in
	// which case the Purger reuses the app pool and fails closed at the DB
	// immutability trigger). The caller owns it and must Close it.
	purgePool dbpkg.Pool
}

// buildRetentionWiring assembles the shared retention resources and the
// per-tenant engine factory used by both buildRetentionAdmin (per-request
// engines for the gateway admin RPCs) and the on-host one-shot scan, so the
// tenant-scoping, archive/purge, and audit wiring lives in exactly one place.
//
// The engine is built PER TENANT (engineFor): object collectors and storage
// providers are constructed for that tenant, the DBCollector and hold store run
// on a tenant-scoped pool (RLS keyed on the request-context tenant), and the
// archiver/purger self-scope per candidate via c.TenantID. This ensures a caller
// only ever scans and mutates its own tenant.
func buildRetentionWiring(ctx context.Context, cfg *config.Config, shared *sharedResources, logger *slog.Logger) (retentionWiring, error) {
	defaultPolicy, err := retention.NewPolicy(cfg.Retention)
	if err != nil {
		return retentionWiring{}, fmt.Errorf("build retention policy: %w", err)
	}

	// Purger requires purge_user (member of retention_purge); the DB
	// immutability triggers reject deletes from any other role. Without a
	// PurgeDSN we reuse the app pool so scans still run, but purges then fail
	// closed at the trigger rather than silently succeeding as app_user.
	var purgeConn dbpkg.Connection = shared.appPool
	var purgePool dbpkg.Pool
	if cfg.Database.PurgeDSN != "" {
		// A superuser or owner purge_dsn would bypass RLS and satisfy the
		// retention_purge triggers for every tenant, so refuse it up front.
		purgeDB, err := sql.Open("pgx", cfg.Database.PurgeDSN)
		if err != nil {
			return retentionWiring{}, fmt.Errorf("open purge_user sql.DB: %w", err)
		}
		err = dbpkg.CheckRestrictedRole(ctx, purgeDB, "database.purge_dsn")
		purgeDB.Close()
		if err != nil {
			return retentionWiring{}, fmt.Errorf("check the purge_user connection: %w", err)
		}
		dbTracer, dbMeter := telemetry.Instrumentation("pkg/db")
		pool, err := dbpkg.NewPool(dbpkg.NewPoolConfigFrom(
			cfg.Database.PurgeDSN, cfg.Database.GraphDSN, cfg.Database.MaxConns, cfg.Database.SSLMode, cfg.Database.Extensions,
		), dbpkg.WithTelemetry(dbTracer, dbMeter))
		if err != nil {
			return retentionWiring{}, fmt.Errorf("connect purge_user pool: %w", err)
		}
		purgePool = pool
		purgeConn = pool
	} else {
		logger.Warn("database.purge_dsn is unset; retention purges will fail closed at the DB immutability trigger (configure purge_dsn as purge_user to enable purging)")
	}

	// tenantAppConn scopes the DBCollector and hold store to the tenant carried
	// in each request's context via RLS. The hold store is shared across
	// requests because it is stateless and derives its tenant from the context.
	// It uses the retention pool so a deleted tenant's data still follows
	// retention; every other app path refuses deleted tenants.
	tenantAppConn := dbpkg.NewRetentionTenantPool(shared.appPool)
	holds := retention.NewPgHoldStore(tenantAppConn)
	auditPublisher := retention.NewAuditPublisher(shared.natsClient)

	engineFor := func(tenantID string, policy retention.Policy) (*retention.Engine, error) {
		primary, err := storage.NewFromConfig(cfg.Storage.Objects, tenantID)
		if err != nil {
			return nil, fmt.Errorf("retention: build primary storage for tenant %q: %w", tenantID, err)
		}
		archiveProvider, err := storage.NewArchiveFromConfig(cfg.Retention.Archive, tenantID)
		if err != nil {
			if !errors.Is(err, storage.ErrArchiveDisabled) {
				return nil, fmt.Errorf("retention: build archive storage for tenant %q: %w", tenantID, err)
			}
			logger.Warn("retention archive backend disabled; archive+purge will fail closed until retention.archive is configured (dry-run scans, stats, and legal holds remain available)")
			archiveProvider = disabledArchive{}
		}
		collectors := []retention.Collector{
			retention.NewDBCollector(tenantAppConn),
			retention.NewCatalogCollector(tenantAppConn),
			retention.NewObjectCollector(primary, retention.ClassAttestation, tenantID),
		}
		archiver := retention.NewArchiver(shared.appPool, primary, archiveProvider)
		purger := retention.NewPurger(purgeConn, primary)
		// Session advisory lock on the app database, shared with `admin backup
		// run` (which takes it as backup_user on the same database).
		scanLock := dbpkg.NewAdvisoryLocker(cfg.Database.DSN, dbpkg.BackupRetentionLockName)
		return retention.NewEngine(collectors, holds, archiver, purger, auditPublisher, policy, scanLock), nil
	}

	return retentionWiring{
		engineFor:     engineFor,
		defaultPolicy: defaultPolicy,
		holds:         holds,
		purgePool:     purgePool,
	}, nil
}

// attachWorker wires the LLM worker into rt. No database resources are
// involved -- a worker replica scales horizontally without Postgres
// credentials.
func attachWorker(cfg *config.Config, shared *sharedResources, rt *runtime) error {
	workerCfg := cfg.Worker
	workerCfg.LLM = cfg.LLM
	rt.workerService = worker.New(shared.natsClient, shared.llmClient, workerCfg,
		worker.WithTelemetry(otel.GetTracerProvider(), otel.GetMeterProvider()))
	rt.healthServer = newHealthServer(cfg.Health.Addr)
	return nil
}

// attachGraph wires the graph-materialization subscriber into rt. This is
// the same construction the daemon has always performed, now reachable via
// explicit role selection instead of unconditionally.
func attachGraph(cfg *config.Config, shared *sharedResources, rt *runtime) error {
	tp, mp := otel.GetTracerProvider(), otel.GetMeterProvider()
	tenantConn := dbpkg.NewTenantPool(shared.appPool)
	resolver := graph.NewPGResolver(tenantConn, graph.WithPGResolverTelemetry(tp, mp))
	rt.graphService = graph.New(shared.graphDB_, shared.vectorDB, shared.natsClient,
		graph.WithResolver(resolver), graph.WithTelemetry(tp, mp),
		graph.WithMaxBulkEdges(cfg.Graph.MaxBulkEdges),
		graph.WithControlsReader(graph.NewPGControlsReader(tenantConn, graph.WithPGControlsReaderTelemetry(tp, mp))))
	rt.healthServer = newHealthServer(cfg.Health.Addr, dbPingCheck(shared.appPool))
	return nil
}

// dbPingCheck returns a health check reporting whether pool is connected,
// shared by every role with a dedicated health listener backed by the app
// database pool (graph, pipeline).
func dbPingCheck(pool dbpkg.Pool) func(context.Context) error {
	return func(ctx context.Context) error {
		status, err := pool.Health(ctx)
		if err != nil {
			return err
		}
		if !status.Connected {
			return errors.New("database not connected")
		}
		return nil
	}
}

// taskTypesByAnalyzer maps every production analyzer name to the NATS task
// type the worker role dispatches it as. Mirrors
// internal/pipeline/pipeline_e2e_integration_test.go's proven wiring.
var taskTypesByAnalyzer = map[string]natsbus.TaskType{
	"classify":     natsbus.TaskClassify,
	"embedding":    natsbus.TaskEmbed,
	"artifacts":    natsbus.TaskArtifacts,
	"requires":     natsbus.TaskRequires,
	"relationship": natsbus.TaskRelate,
}

// buildPipelineService constructs a production pipeline.Service. Fails
// closed if attestation key paths, a default tenant, or a storage base
// path aren't configured -- pipeline.New requires a real
// attestation.Generator, and storage.Provider is bound to one tenant and
// one root directory at construction (see the design doc's storage
// tenant-scoping note; this mirrors cmd/crosscodex's embedded mode, which
// has the same single-tenant storage limitation). Without this check, an
// empty base path would resolve via filepath.Abs("") to the process's
// current working directory instead of failing fast. Shared by
// attachPipeline (standalone pipeline role) and bootstrap's RoleAll case
// (in-process, no separate listener), so this construction logic exists in
// exactly one place.
func buildPipelineService(cfg *config.Config, shared *sharedResources) (*pipeline.Service, error) {
	if cfg.Attestation.PrivateKeyPath == "" || cfg.Attestation.PublicKeyPath == "" {
		return nil, errors.New("pipeline: attestation.private_key_path and attestation.public_key_path must be set")
	}
	if cfg.Tenants.DefaultTenant == "" {
		return nil, errors.New("pipeline: tenants.default_tenant must be set")
	}
	if cfg.Storage.Objects.BasePath == "" {
		return nil, errors.New("pipeline: storage.objects.base_path must be set")
	}

	tp, mp := otel.GetTracerProvider(), otel.GetMeterProvider()
	tenantConn := dbpkg.NewTenantPool(shared.appPool)

	prompts, err := prompt.NewRegistry(cfg.Prompt)
	if err != nil {
		return nil, fmt.Errorf("create prompt registry: %w", err)
	}

	storageTracer, storageMeter := telemetry.Instrumentation("pkg/storage")
	storageProvider, err := storage.NewLocal(cfg.Storage.Objects.BasePath, cfg.Tenants.DefaultTenant,
		storage.WithLocalTelemetry(storageTracer, storageMeter))
	if err != nil {
		return nil, fmt.Errorf("create storage provider: %w", err)
	}

	store := pipeline.NewPGStore(tenantConn, shared.appPool, pipeline.WithStoreTelemetry(tp, mp))

	registry, err := pipeline.NewProductionRegistry(shared.llmClient, shared.vectorDB, storageProvider, prompts, tenantConn, cfg.Analysis,
		tp, mp)
	if err != nil {
		return nil, fmt.Errorf("build analyzer registry: %w", err)
	}

	dbReporter := pipeline.NewDBStageReporter(
		analysis.NewNATSStageReporter(shared.natsClient, analysis.WithReporterTelemetry(tp, mp)),
		store, pipeline.WithReporterTracer(tp))
	engine := analysis.NewWithNATS(registry, shared.natsClient, cfg.Analysis.Engine, taskTypesByAnalyzer,
		analysis.WithStageReporter(dbReporter), analysis.WithTelemetry(tp, mp))

	candRegistry, err := pipeline.BuildCandidateRegistry(cfg.Analysis.Candidates, tp, mp)
	if err != nil {
		return nil, fmt.Errorf("build candidate registry: %w", err)
	}
	candGen := pipeline.NewCandidateGenerator(
		pipeline.NewPGControlsReader(tenantConn),
		pipeline.NewPGEmbeddingsReader(tenantConn),
		candRegistry,
		pipeline.NewPGCandidateWriter(tenantConn),
		cfg.Analysis.Candidates,
		cfg.Analysis.Candidates.EmbedModel,
	)

	synth := synthesis.New(tenantConn, cfg.Synthesis, cfg.Analysis.Relationship.ActionableTypes, synthesis.WithTelemetry(tp, mp))

	attTracer, attMeter := telemetry.Instrumentation("pkg/attestation")
	attestor, err := attestation.NewGenerator(&attestation.FileKeyProvider{
		PrivateKeyPath: cfg.Attestation.PrivateKeyPath,
		PublicKeyPath:  cfg.Attestation.PublicKeyPath,
	}, attestation.WithTelemetry(attTracer, attMeter))
	if err != nil {
		return nil, fmt.Errorf("create attestation generator: %w", err)
	}
	converter := pipelineattestation.NewConverter()

	return pipeline.New(
		store, engine, registry, synth, attestor, converter, shared.natsClient, storageProvider,
		cfg.Pipeline, cfg.Attestation, candGen,
		pipeline.WithCatalogControlsReader(pipeline.NewPGCatalogControlsReader(tenantConn)),
		pipeline.WithEmbeddingsReader(pipeline.NewPGEmbeddingsReader(tenantConn)),
		pipeline.WithEmbeddingConfig(cfg.Analysis.Embedding),
		pipeline.WithTelemetry(tp, mp),
	), nil
}

// attachPipeline wires a standalone pipeline.Service behind its own
// Connect RPC listener into rt. Requires cfg.Pipeline.Addr in addition to
// buildPipelineService's own fail-closed preconditions.
func attachPipeline(ctx context.Context, cfg *config.Config, shared *sharedResources, rt *runtime) error {
	if cfg.Pipeline.Addr == "" {
		return errors.New("pipeline role: pipeline.addr must be set")
	}

	// pipeline.Server requires mTLS (RequireAndVerifyClientCert) on every
	// RPC and has no unauthenticated fallback. rpcserver.Listen only applies
	// that client-auth setting when a *tls.Config is actually resolved, so if
	// TLS is off (the default) the listener would silently serve plaintext,
	// authenticated only by a self-asserted tenant header. Resolve the same
	// "pipeline-server" target rpcserver.Listen uses and fail closed unless
	// it produced a mutual-TLS config with a CA to verify client certs
	// against (a nil ClientCAs means tls.mode is "server-only", which would
	// otherwise fail every handshake at runtime rather than here).
	resolvedTLS, err := tlsconfig.BuildTLSConfig(ctx, cfg.TLS, "pipeline-server")
	if err != nil {
		return fmt.Errorf("pipeline role: resolve pipeline-server TLS config: %w", err)
	}
	if resolvedTLS == nil || resolvedTLS.ClientCAs == nil {
		return errors.New(`pipeline role: tls.mode (or tls.targets.pipeline-server.mode) must be "mutual" with a CA configured — pipeline.Server requires mTLS on every RPC and has no unauthenticated fallback`)
	}

	pipelineSvc, err := buildPipelineService(cfg, shared)
	if err != nil {
		return err
	}
	rt.pipelineService = pipelineSvc

	server, err := pipeline.NewServer(ctx, pipeline.ServerConfig{
		Addr:    cfg.Pipeline.Addr,
		TLS:     cfg.TLS,
		Service: pipelineSvc,
	})
	if err != nil {
		return fmt.Errorf("create pipeline server: %w", err)
	}
	rt.pipelineServer = server

	rt.healthServer = newHealthServer(cfg.Health.Addr, dbPingCheck(shared.appPool))
	return nil
}

// Compile-time proof that the results service can back the gateway. It lives
// here, the composition root, because internal/results must not import
// internal/gateway.
var _ gateway.MappingsBackend = (*results.Service)(nil)

// newMappingsBackend builds the mappings reader both gateway wirings share.
func newMappingsBackend(shared *sharedResources) *results.Service {
	tracer, meter := telemetry.Instrumentation("internal/results")
	return results.NewService(
		results.NewPGStore(dbpkg.NewTenantPool(shared.appPool),
			results.WithStoreTelemetry(otel.GetTracerProvider(), otel.GetMeterProvider())),
		results.WithLogger(slog.Default()),
		results.WithTelemetry(tracer, meter),
	)
}

// attachGateway wires the standalone gateway role: buildIngestGateway's
// authn, pass-through ingestion and catalog import, with a PipelineBackend
// that is a Connect client reaching a separately-deployed pipeline role over
// the network. Catalog import persists controls and publishes them over NATS
// for the graph role to project, so this role holds no graph credentials.
// Fails closed if cfg.Pipeline.Endpoint isn't configured -- there is no
// in-process pipeline.Service to fall back to once gateway and pipeline are
// split into independent roles.
func attachGateway(ctx context.Context, cfg *config.Config, shared *sharedResources, rt *runtime) error {
	if cfg.Pipeline.Endpoint == "" {
		return errors.New("gateway role: pipeline.endpoint must be set to reach a standalone pipeline role")
	}

	// Built before buildIngestGateway's config checks, which need it as an
	// argument. Construction only resolves TLS material and never dials, so
	// a later refusal discards nothing but an unused client.
	pipelineBackend, err := gateway.NewConnectPipelineBackend(ctx, cfg.Pipeline.Endpoint, cfg.TLS)
	if err != nil {
		return fmt.Errorf("create pipeline backend: %w", err)
	}

	gatewaySvc, err := buildIngestGateway(ctx, config.RoleGateway, cfg, shared, pipelineBackend)
	if err != nil {
		return err
	}

	server, err := gateway.NewServer(ctx, gateway.ServerConfig{
		TenantStatus: gateway.NewPoolTenantStatus(shared.appPool),
		Addr:         cfg.Server.Addr,
		TLS:          cfg.TLS,
		Service:      gatewaySvc,
	})
	if err != nil {
		return fmt.Errorf("create gateway server: %w", err)
	}
	rt.gatewayServer = server
	return nil
}
