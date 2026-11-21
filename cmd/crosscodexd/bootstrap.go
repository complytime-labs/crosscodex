package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"slices"

	"go.opentelemetry.io/otel"

	crosscodexv1connect "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/complytime-labs/crosscodex/internal/admin"
	"github.com/complytime-labs/crosscodex/internal/analysis"
	"github.com/complytime-labs/crosscodex/internal/catalog"
	intcleaning "github.com/complytime-labs/crosscodex/internal/cleaning"
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
	"github.com/complytime-labs/crosscodex/pkg/cleaning"
	"github.com/complytime-labs/crosscodex/pkg/config"
	dbpkg "github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/oscal"
	"github.com/complytime-labs/crosscodex/pkg/prompt"
	"github.com/complytime-labs/crosscodex/pkg/resultcache"
	"github.com/complytime-labs/crosscodex/pkg/resultcache/pgstore"
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
	graphServer     *graph.Server
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
	if rt.graphServer != nil {
		if err := rt.graphServer.Start(); err != nil {
			return fmt.Errorf("start graph server: %w", err)
		}
	}
	if rt.healthServer != nil {
		// Bind here, not in the goroutine, so a taken port fails startup
		// instead of leaving a role running with no health endpoint.
		ln, err := net.Listen("tcp", rt.healthServer.Addr)
		if err != nil {
			return fmt.Errorf("start health listener on health.addr %s: %w. "+
				"Split roles all default to :9091; if another role on this host already listens there, "+
				"give each role on one host a distinct health.addr (CROSSCODEX_HEALTH_ADDR)", rt.healthServer.Addr, err)
		}
		go func() {
			if err := rt.healthServer.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
// RPC, graph RPC) are drained first, then the background services those listeners hand
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
	if rt.graphServer != nil {
		if err := rt.graphServer.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("stop graph server: %w", err))
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
		if err := attachGraphListener(ctx, cfg, shared, rt); err != nil {
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
		// The gateway's own /healthz on server.addr already carries this
		// role's dependency checks (roleHealthChecks via buildIngestGateway);
		// drop the dedicated listener attachGraph/attachWorker each set so
		// "all" doesn't bind cfg.Health.Addr twice or leave a second,
		// redundant listener up.
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

// errNoX509Mappings refuses a gateway-serving role whose auth.x509_mappings is
// empty: roles come only from mappings, so every caller would be refused.
var errNoX509Mappings = errors.New("no client certificate can sign in: auth.x509_mappings is empty, " +
	"and certificates no longer get admin implicitly. Add a mapping, e.g. for the certificates from deploy/certgen: " +
	"auth.x509_mappings: [{match: {cn: client, organization: CrossCodex}, roles: [admin]}]")

// errNoServiceIdentities refuses a role serving RPCs (pipeline, graph) whose
// tls.service_identities is empty: no client certificate could assert a
// tenant, so every request would be denied. caller names the role whose
// client certificate must be listed.
func errNoServiceIdentities(role, caller string) error {
	return fmt.Errorf("%s role: no client certificate may assert a tenant: tls.service_identities is empty, "+
		"so the %s would deny every request. List the %s's client certificate subject, for example "+
		"tls.service_identities: [{cn: client, organization: CrossCodex}]", role, role, caller)
}

// x509Match converts a configured certificate matcher to its authn form.
func x509Match(m config.X509MatchConfig) authn.X509Match {
	return authn.X509Match{
		CN:           m.CN,
		Organization: m.Organization,
		OrgUnit:      m.OrgUnit,
		SANEmail:     m.SANEmail,
		SANDNS:       m.SANDNS,
		SANURI:       m.SANURI,
	}
}

// allTenantAuthRegistry builds the X.509 registry both gateway-serving roles
// (all and gateway) authenticate callers with. A client certificate gets the
// roles of the first auth.x509_mappings entry it matches and no identity if it
// matches none. With tenants.enabled false every identity's tenant is
// tenants.default_tenant; otherwise it is the matching mapping's tenant.
func allTenantAuthRegistry(cfg *config.Config) (*authn.Registry, error) {
	mappings := make([]authn.X509Mapping, len(cfg.Auth.X509Mappings))
	for i, m := range cfg.Auth.X509Mappings {
		mappings[i] = authn.X509Mapping{
			Match:  x509Match(m.Match),
			Tenant: m.Tenant,
			Roles:  m.Roles,
		}
	}
	x509Auth, err := authn.NewX509Authenticator(authn.X509Config{
		SingleTenant:  !cfg.Tenants.Enabled,
		DefaultTenant: cfg.Tenants.DefaultTenant,
		Mappings:      mappings,
	})
	if err != nil {
		return nil, fmt.Errorf("create X.509 authenticator: %w", err)
	}
	tracer, meter := telemetry.Instrumentation("pkg/authn")
	return authn.NewRegistry(noopAuditEmitter{}, []authn.Authenticator{x509Auth}, authn.WithTelemetry(tracer, meter))
}

// checkDefaultTenant refuses to start role unless tenants.default_tenant is
// provisioned and not deleted. The default tenant is provisioned by
// `crosscodexd migrate` (as the schema owner) and the embedded CLI; the daemon
// only checks it. enter_tenant refuses a deleted tenant.
func checkDefaultTenant(ctx context.Context, role string, cfg *config.Config, pool dbpkg.Pool) error {
	tctx, err := tenant.WithTenant(ctx, cfg.Tenants.DefaultTenant)
	if err != nil {
		return fmt.Errorf("%s role: tenants.default_tenant: %w", role, err)
	}
	tx, err := dbpkg.NewTenantPool(pool).Begin(tctx)
	if dbpkg.IsPgErrorCode(err, "42501") {
		return fmt.Errorf("%s role: default tenant %q is deleted. Restore it with crosscodexd admin tenant restore --tenant %s --by <operator> --reason <text>, or set tenants.default_tenant to an active tenant: %w",
			role, cfg.Tenants.DefaultTenant, cfg.Tenants.DefaultTenant, err)
	}
	if err != nil {
		return fmt.Errorf("%s role: check default tenant %q: %w", role, cfg.Tenants.DefaultTenant, err)
	}
	var found int
	err = tx.QueryRow(tctx, "SELECT 1 FROM tenants WHERE tenant_id = $1", cfg.Tenants.DefaultTenant).Scan(&found)
	_ = tx.Rollback()
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%s role: default tenant %q does not exist. Run `crosscodexd migrate`, which provisions tenants.default_tenant, then start crosscodexd again", role, cfg.Tenants.DefaultTenant)
	}
	if err != nil {
		return fmt.Errorf("%s role: check default tenant %q: %w", role, cfg.Tenants.DefaultTenant, err)
	}
	return nil
}

// buildIngestGateway builds the gateway.Service that serves catalog import for
// role (all or gateway): X.509 authn through auth.x509_mappings (refusing
// to start without one), pass-through ingestion
// into the caller's tenant's object storage (objectStore), and a catalog
// service that persists controls and publishes them for graph projection.
// Each import's in-toto ingest attestation is signed with its tenant's
// attestation key pair (pipeline.NewAttestors, so a key pair that does not
// load refuses startup) and stored in the same object storage. It holds no graph credentials; the
// graph role projects Control nodes. Refuses a default tenant that
// `crosscodexd migrate` has not provisioned or that is deleted. retire, when
// non-nil, lets the catalog service retire catalog versions (role all with
// database.purge_dsn); nil makes retire fail closed. extra appends
// role-specific backends (role all adds graph queries).
func buildIngestGateway(ctx context.Context, role string, cfg *config.Config, shared *sharedResources, pipelineBackend gateway.PipelineBackend, retire *catalog.Retention, extra ...gateway.ServiceOption) (*gateway.Service, error) {
	if cfg.Tenants.DefaultTenant == "" {
		return nil, fmt.Errorf("%s role: tenants.default_tenant must be set", role)
	}
	if err := checkObjectStorage(cfg.Storage.Objects); err != nil {
		return nil, fmt.Errorf("%s role: %w", role, err)
	}
	// Every catalog import is attested with its tenant's key pair, so a
	// missing or unloadable one refuses startup rather than every import.
	attTracer, attMeter := telemetry.Instrumentation("pkg/attestation")
	attestors, err := pipeline.NewAttestors(cfg.Attestation, cfg.TLS.FIPS.Enabled, attTracer, attMeter)
	if err != nil {
		return nil, fmt.Errorf("%s role: %w", role, err)
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
	if len(cfg.Auth.X509Mappings) == 0 {
		return nil, fmt.Errorf("%s role: %w", role, errNoX509Mappings)
	}

	// The registry below rejects a malformed default tenant with a less
	// specific message, so name the setting first.
	if err := tenant.ValidateTenantID(cfg.Tenants.DefaultTenant); err != nil {
		return nil, fmt.Errorf("%s role: tenants.default_tenant: %w", role, err)
	}
	// Built before the database check so a mapping it rejects refuses
	// startup without a database round trip.
	registry, err := allTenantAuthRegistry(cfg)
	if err != nil {
		return nil, fmt.Errorf("%s role: create auth registry: %w", role, err)
	}
	if err := checkDefaultTenant(ctx, role, cfg, shared.appPool); err != nil {
		return nil, err
	}

	storageProvider := objectStore(cfg)

	oscalTracer, _ := telemetry.Instrumentation("pkg/oscal")
	catalogTracer, catalogMeter := telemetry.Instrumentation("internal/catalog")
	catalogOpts := []catalog.ServiceOption{
		catalog.WithParser(oscal.NewParser("", oscal.WithParserTracer(oscalTracer),
			oscal.WithRequirementPartNames(cfg.Catalog.RequirementPartNames))),
		catalog.WithStore(catalog.NewPGStore(dbpkg.NewTenantPool(shared.appPool))),
		catalog.WithStorage(storageProvider),
		catalog.WithBus(shared.natsClient),
		catalog.WithLogger(slog.Default()),
		catalog.WithTracer(catalogTracer),
		catalog.WithMeter(catalogMeter),
	}
	if retire != nil {
		catalogOpts = append(catalogOpts, catalog.WithRetention(*retire))
	}
	catalogSvc := catalog.NewService(catalogOpts...)
	ingestion := backend.NewPassthroughIngestion(storageProvider)

	resultsSvc := newMappingsBackend(shared)

	gatewayTracer, gatewayMeter := telemetry.Instrumentation("internal/gateway")
	opts := []gateway.ServiceOption{
		gateway.WithAuthn(registry),
		gateway.WithIngestionBackend(ingestion),
		gateway.WithCatalogBackend(catalogSvc),
		gateway.WithPipelineBackend(pipelineBackend),
		gateway.WithMappingsBackend(resultsSvc),
		gateway.WithCandidatesBackend(resultsSvc),
		gateway.WithAdminBackend(gateway.NewPoolAdminBackend(shared.appPool,
			roleHealthChecks(role, shared.appPool, shared.natsClient, shared.graphDB)...)),
		gateway.WithAttestor(func(tenantID string) attestation.Generator {
			if att := attestors.ForTenant(tenantID); att != nil {
				return att.Generator
			}
			return nil
		}, storageProvider),
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
	wiring, err := buildRetentionWiring(ctx, cfg, shared, slog.Default())
	if err != nil {
		return fmt.Errorf("build retention wiring: %w", err)
	}
	rt.retentionPurgePool = wiring.purgePool

	gatewaySvc, err := buildIngestGateway(ctx, config.RoleAll, cfg, shared, backendPipeline, wiring.retire,
		gateway.WithGraphBackend(rt.graphService),
		gateway.WithCrosswalkBackend(rt.graphService))
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

	adminSvc := buildRetentionAdmin(cfg, shared, wiring)

	cleaningEngine, err := newCleaningEngine(cfg, shared, rt.pipelineService.CurrentStageRecord)
	if err != nil {
		return err
	}

	server, err := gateway.NewServer(ctx, gateway.ServerConfig{
		TenantStatus: gateway.NewPoolTenantStatus(shared.appPool),
		Addr:         cfg.Server.Addr,
		TLS:          cfg.TLS,
		Service:      gatewaySvc,
		Admin:        adminSvc,
		Cleaning:     intcleaning.NewService(cleaningEngine),
	})
	if err != nil {
		return fmt.Errorf("create gateway server: %w", err)
	}
	rt.gatewayServer = server
	return nil
}

// newCleaningEngine builds the edge-cleaning engine over the app pool and the
// graph, signing with newCleaningSigner. current reads a stage's current
// prompt and models: in-process for role all, from the pipeline role for role
// graph.
func newCleaningEngine(cfg *config.Config, shared *sharedResources, current cleaning.CurrentFunc) (*cleaning.Engine, error) {
	signer, err := newCleaningSigner(cfg)
	if err != nil {
		return nil, err
	}
	tp, mp := otel.GetTracerProvider(), otel.GetMeterProvider()
	store := cleaning.NewPGFindingStore(dbpkg.NewTenantPool(shared.appPool), cleaning.WithFindingStoreTelemetry(tp, mp))
	return cleaning.NewEngine(store, shared.graphDB_, &cfg.Cleaning, current, signer, cleaning.WithEngineTelemetry(tp, mp)), nil
}

// newCleaningSigner builds the generator that signs edge-cleaning decisions.
// It returns no signer, and no error, when either attestation key path is
// unset: the engine then refuses every write with an actionable message. Key
// paths that are set but do not load are an error, so a typo stops the daemon
// instead of silently disabling signed cleaning.
func newCleaningSigner(cfg *config.Config) (attestation.Generator, error) {
	priv, pub := cfg.Attestation.PrivateKeyPath, cfg.Attestation.PublicKeyPath
	if priv == "" || pub == "" {
		return nil, nil
	}
	keys, err := attestation.LoadFileKeys(priv, pub, cfg.TLS.FIPS.Enabled)
	if err != nil {
		return nil, fmt.Errorf("cleaning signer: load attestation.private_key_path and attestation.public_key_path: %w", err)
	}
	tr, mt := telemetry.Instrumentation("pkg/attestation")
	gen, err := attestation.NewGenerator(keys, attestation.WithTelemetry(tr, mt), attestation.WithFIPSMode(cfg.TLS.FIPS.Enabled))
	if err != nil {
		return nil, fmt.Errorf("cleaning signer: %w", err)
	}
	return gen, nil
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
// role from wiring (see buildRetentionWiring, whose caller owns the purge_user
// pool).
func buildRetentionAdmin(cfg *config.Config, shared *sharedResources, wiring retentionWiring) crosscodexv1connect.AdminServiceHandler {
	driftSource := func(ctx context.Context) ([]retention.StreamDrift, error) {
		configured := natsbus.ConfiguredAuditRetention(cfg.NATS.Streams)
		live, err := shared.natsClient.AuditStreamRetention(ctx)
		if err != nil {
			return nil, err
		}
		return retention.VerifyStreamRetention(configured, live), nil
	}
	return admin.NewService(wiring.engineFor, wiring.defaultPolicy, wiring.holds, driftSource)
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
	// retire is what the catalog service needs to retire catalog versions;
	// nil when PurgeDSN is unset, so retire fails closed.
	retire *catalog.Retention
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

	// backendsFor builds the tenant's primary store, archiver and purger,
	// shared by the engine and by catalog retire.
	backendsFor := func(tenantID string) (storage.Provider, retention.Archiver, retention.Purger, error) {
		primary, err := storage.NewFromConfig(cfg.Storage.Objects, tenantID)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("retention: build primary storage for tenant %q: %w", tenantID, err)
		}
		archiveProvider, err := storage.NewArchiveFromConfig(cfg.Retention.Archive, tenantID)
		if err != nil {
			if !errors.Is(err, storage.ErrArchiveDisabled) {
				return nil, nil, nil, fmt.Errorf("retention: build archive storage for tenant %q: %w", tenantID, err)
			}
			logger.Warn("retention archive backend disabled; archive+purge will fail closed until retention.archive is configured (dry-run scans, stats, and legal holds remain available)")
			archiveProvider = disabledArchive{}
		}
		return primary, retention.NewArchiver(shared.appPool, primary, archiveProvider), retention.NewPurger(purgeConn, primary), nil
	}

	engineFor := func(tenantID string, policy retention.Policy) (*retention.Engine, error) {
		primary, archiver, purger, err := backendsFor(tenantID)
		if err != nil {
			return nil, err
		}
		collectors := []retention.Collector{
			retention.NewDBCollector(tenantAppConn),
			retention.NewCatalogCollector(tenantAppConn),
			retention.NewObjectCollector(primary, retention.ClassAttestation, tenantID),
		}
		// Session advisory lock on the app database, shared with `admin backup
		// run` (which takes it as backup_user on the same database).
		scanLock := dbpkg.NewAdvisoryLocker(cfg.Database.DSN, dbpkg.BackupRetentionLockName)
		return retention.NewEngine(collectors, holds, archiver, purger, auditPublisher, policy, scanLock), nil
	}

	// Catalog retire deletes through the purger directly, so it is offered
	// only with a real purge_user pool; without one it fails closed.
	var retire *catalog.Retention
	if purgePool != nil {
		retire = &catalog.Retention{
			Holds: holds,
			Backends: func(tenantID string) (retention.Archiver, retention.Purger, error) {
				_, archiver, purger, err := backendsFor(tenantID)
				return archiver, purger, err
			},
			Audit: auditPublisher,
		}
	}

	return retentionWiring{
		engineFor:     engineFor,
		defaultPolicy: defaultPolicy,
		holds:         holds,
		retire:        retire,
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
		worker.WithTelemetry(otel.GetTracerProvider(), otel.GetMeterProvider()),
		worker.WithAbandonTTL(cfg.Pipeline.StageTimeout))
	rt.healthServer = newHealthServer(cfg.Health.Addr, roleHealthChecks(cfg.Role, shared.appPool, shared.natsClient, shared.graphDB)...)
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
		graph.WithMaxSimilarityCatalogs(cfg.Graph.MaxSimilarityCatalogs),
		graph.WithMaxSimilarityLimit(cfg.Graph.MaxSimilarityLimit),
		graph.WithMaterializeConfig(cfg.NATS.Materialize),
		graph.WithControlsReader(graph.NewPGControlsReader(tenantConn, graph.WithPGControlsReaderTelemetry(tp, mp))))
	rt.healthServer = newHealthServer(cfg.Health.Addr, roleHealthChecks(cfg.Role, shared.appPool, shared.natsClient, shared.graphDB)...)
	return nil
}

// attachGraphListener gives the graph role (not role all, which serves these
// in process) a Connect RPC listener on graph.addr serving the gateway's
// read-only graph, crosswalk and edge-cleaning RPCs. attachGraph must run
// first. Every precondition is checked before any listener is built, and the
// configuration ones before the database is touched.
func attachGraphListener(ctx context.Context, cfg *config.Config, shared *sharedResources, rt *runtime) error {
	if rt.graphService == nil {
		return errors.New("graph role: graph service is not built; attachGraph must run before attachGraphListener")
	}
	if cfg.Graph.Addr == "" {
		return errors.New("graph role: graph.addr must be set so the gateway can reach the graph listener")
	}

	// graph.Server requires mTLS (RequireAndVerifyClientCert) on every RPC
	// and has no unauthenticated fallback. rpcserver.Listen only applies that
	// client-auth setting when a *tls.Config is actually resolved, so if TLS
	// is off (the default) the listener would silently serve plaintext,
	// authenticated only by a self-asserted tenant header. Resolve the same
	// "graph-server" target rpcserver.Listen uses and fail closed unless it
	// produced a mutual-TLS config with a CA to verify client certs against
	// (a nil ClientCAs means tls.mode is "server-only", which would otherwise
	// fail every handshake at runtime rather than here).
	resolvedTLS, err := tlsconfig.BuildTLSConfig(ctx, cfg.TLS, "graph-server")
	if err != nil {
		return fmt.Errorf("graph role: resolve graph-server TLS config: %w", err)
	}
	if resolvedTLS == nil || resolvedTLS.ClientCAs == nil {
		return errors.New(`graph role: tls.mode (or tls.targets.graph-server.mode) must be "mutual" with a CA configured — graph.Server requires mTLS on every RPC and has no unauthenticated fallback`)
	}

	// Any certificate the CA signed could otherwise name any tenant and end
	// user in request headers, so only the gateway's own client certificate
	// may.
	if len(cfg.TLS.ServiceIdentities) == 0 {
		return errNoServiceIdentities("graph", "gateway")
	}
	serviceIdentities := make([]authn.X509Match, len(cfg.TLS.ServiceIdentities))
	for i, m := range cfg.TLS.ServiceIdentities {
		serviceIdentities[i] = x509Match(m)
	}

	if cfg.Tenants.DefaultTenant == "" {
		return errors.New("graph role: tenants.default_tenant must be set")
	}
	// Edge cleaning judges verdicts against each stage's current prompt and
	// models, which only the pipeline role knows.
	if cfg.Pipeline.Endpoint == "" {
		return errors.New("graph role: pipeline.endpoint must be set: edge cleaning reads each stage's current prompt and models from the pipeline role")
	}
	// Construction only resolves TLS material and never dials.
	pipelineClient, err := gateway.NewPipelineClient(ctx, cfg.Pipeline.Endpoint, cfg.TLS)
	if err != nil {
		return fmt.Errorf("graph role: create pipeline client: %w", err)
	}
	if err := checkDefaultTenant(ctx, "graph", cfg, shared.appPool); err != nil {
		return err
	}

	cleaningEngine, err := newCleaningEngine(cfg, shared, remoteStageRecord(pipelineClient))
	if err != nil {
		return err
	}
	server, err := graph.NewServer(ctx, graph.ServerConfig{
		Addr:              cfg.Graph.Addr,
		TLS:               cfg.TLS,
		Service:           rt.graphService,
		Cleaning:          intcleaning.NewService(cleaningEngine),
		ServiceIdentities: serviceIdentities,
		Logger:            slog.Default(),
	})
	if err != nil {
		return fmt.Errorf("create graph server: %w", err)
	}
	rt.graphServer = server
	return nil
}

// dbPingCheck returns a health check reporting whether pool is connected:
// the "database" dependency of roleHealthChecks for the split roles that use
// the app database (graph, pipeline).
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
	"critique":     natsbus.TaskCritique,
}

// objectStore is the daemon's object storage. It opens storage.objects for
// the tenant on each call's context, once per tenant, so every tenant's
// objects stay under its own directory or key prefix; a call without a
// tenant on its context fails with tenant.ErrNoTenant. The returned
// Provider owns the per-tenant providers it opens.
func objectStore(cfg *config.Config) storage.Provider {
	objects := cfg.Storage.Objects
	tracer, meter := telemetry.Instrumentation("pkg/storage")
	return storage.NewTenantRouter(func(tenantID string) (storage.Provider, error) {
		return storage.NewFromConfig(objects, tenantID, storage.WithTelemetry(tracer, meter))
	})
}

// checkObjectStorage refuses a storage.objects the daemon cannot start
// with. Only the local backend needs base_path: left empty,
// storage.NewFromConfig would fall back to a per-user data directory, which
// the daemon must not pick silently. pkg/config validation refuses an s3
// backend without a bucket.
func checkObjectStorage(objects config.ObjectStorageConfig) error {
	switch storage.ProviderType(objects.Backend) {
	case storage.ProviderTypeLocal:
		if objects.BasePath == "" {
			return errors.New("storage.objects.base_path must be set")
		}
	case storage.ProviderTypeS3:
	default:
		return fmt.Errorf(`storage.objects.backend %q is not supported. Set it to "local" or "s3"`, objects.Backend)
	}
	return nil
}

// buildPipelineService constructs a production pipeline.Service. Fails
// closed if attestation is enabled for any tenant without a key pair that
// loads (pipeline.NewAttestors), since every attested job would then fail,
// or if a default tenant or usable object storage (checkObjectStorage)
// aren't configured. Object storage is opened per tenant from each job's
// context (objectStore). Shared by
// attachPipeline (standalone pipeline role) and bootstrap's RoleAll case
// (in-process, no separate listener), so this construction logic exists in
// exactly one place.
func buildPipelineService(cfg *config.Config, shared *sharedResources) (*pipeline.Service, error) {
	attTracer, attMeter := telemetry.Instrumentation("pkg/attestation")
	attestors, err := pipeline.NewAttestors(cfg.Attestation, cfg.TLS.FIPS.Enabled, attTracer, attMeter)
	if err != nil {
		return nil, fmt.Errorf("pipeline: %w", err)
	}
	logAttestationPosture(attestors)
	if cfg.Tenants.DefaultTenant == "" {
		return nil, errors.New("pipeline: tenants.default_tenant must be set")
	}
	if err := checkObjectStorage(cfg.Storage.Objects); err != nil {
		return nil, fmt.Errorf("pipeline: %w", err)
	}

	tp, mp := otel.GetTracerProvider(), otel.GetMeterProvider()
	tenantConn := dbpkg.NewTenantPool(shared.appPool)

	prompts, err := prompt.NewRegistry(cfg.Prompt)
	if err != nil {
		return nil, fmt.Errorf("create prompt registry: %w", err)
	}

	storageProvider := objectStore(cfg)

	store := pipeline.NewPGStore(tenantConn, shared.appPool, pipeline.WithStoreTelemetry(tp, mp))

	// Result reuse (#243): one store over the app_user tenant pool, which
	// the engine looks results up in and writes them to. The pipeline
	// probes models through the daemon's shared, audited LLM client.
	cacheStore, err := pgstore.New(tenantConn, pgstore.WithTelemetry(tp, mp))
	if err != nil {
		return nil, fmt.Errorf("pipeline: create the result cache store: %w", err)
	}

	registry, err := pipeline.NewProductionRegistry(shared.llmClient, shared.vectorDB, storageProvider, prompts, tenantConn, cfg.Analysis,
		tp, mp)
	if err != nil {
		return nil, fmt.Errorf("build analyzer registry: %w", err)
	}

	dbReporter := pipeline.NewDBStageReporter(
		analysis.NewNATSStageReporter(shared.natsClient, analysis.WithReporterTelemetry(tp, mp)),
		store, pipeline.WithReporterTracer(tp))
	engine := analysis.NewWithNATS(registry, shared.natsClient, cfg.Analysis.Engine, taskTypesByAnalyzer,
		analysis.WithStageReporter(dbReporter), analysis.WithTelemetry(tp, mp),
		analysis.WithStageTimeout(cfg.Pipeline.StageTimeout), analysis.WithResultCache(cacheStore))

	candRegistry, err := pipeline.BuildCandidateRegistry(cfg.Analysis.Candidates, tp, mp)
	if err != nil {
		return nil, fmt.Errorf("build candidate registry: %w", err)
	}
	candGen := pipeline.NewCandidateGenerator(
		pipeline.NewPGControlsReader(tenantConn),
		pipeline.NewPGEmbeddingsReader(tenantConn, pipeline.WithPGEmbeddingsReaderTelemetry(tp, mp)),
		candRegistry,
		pipeline.NewPGCandidateWriter(tenantConn),
		cfg.Analysis.Candidates,
		cfg.Analysis.Candidates.EmbedModel,
	)

	synth := synthesis.New(tenantConn, cfg.Synthesis, cfg.Analysis.Relationship.ActionableTypes, synthesis.WithTelemetry(tp, mp))

	converter := pipelineattestation.NewConverter()

	probeOpts, err := llmProbeOptions(cfg, cacheStore, shared)
	if err != nil {
		return nil, err
	}
	opts := append([]pipeline.Option{
		pipeline.WithCatalogControlsReader(pipeline.NewPGCatalogControlsReader(tenantConn)),
		pipeline.WithCatalogNameReader(pipeline.NewPGCatalogNameReader(tenantConn, pipeline.WithPGCatalogNameReaderTelemetry(tp, mp))),
		pipeline.WithEmbeddingsReader(pipeline.NewPGEmbeddingsReader(tenantConn, pipeline.WithPGEmbeddingsReaderTelemetry(tp, mp))),
		pipeline.WithEmbeddingConfig(cfg.Analysis.Embedding),
		pipeline.WithTelemetry(tp, mp),
		pipeline.WithPromptRegistry(prompts),
		pipeline.WithStageReporter(dbReporter),
	}, probeOpts...)
	return pipeline.New(
		store, engine, registry, synth, attestors, converter, shared.natsClient, storageProvider,
		cfg.Pipeline, candGen, opts...,
	), nil
}

// llmProbeOptions are the pipeline options that probe the LLM gateway before
// a job's analysis: the result cache's model probe and the panel diversity
// check (#260). Both use the daemon's probe client. Dropping
// WithPanelProbe silently turns the panel check off, so
// llm_probe_options_bdd_test.go asserts it is here.
func llmProbeOptions(cfg *config.Config, cacheStore resultcache.Store, shared *sharedResources) ([]pipeline.Option, error) {
	panelProbe, ok := shared.probeClient.(pipeline.PanelProbeClient)
	if !ok {
		return nil, fmt.Errorf("pipeline: the LLM probe client %T cannot read the gateway's model catalog, "+
			"which the panel diversity check needs", shared.probeClient)
	}
	return []pipeline.Option{
		pipeline.WithResultCache(cacheStore, cfg.Analysis.ResultCache, cfg.LLM, shared.probeClient),
		pipeline.WithPanelProbe(panelProbe),
	}, nil
}

// logAttestationPosture logs, once at startup, each scope whose pipeline
// jobs are not attested, so running without attestation is never silent.
// Such jobs reuse no cached LLM result either: only a signed cache
// checkpoint makes a cached result trusted (#243).
func logAttestationPosture(attestors pipeline.Attestors) {
	if attestors.Default == nil {
		slog.Warn("attestation is disabled (attestation.enabled: false): pipeline jobs complete without in-toto attestations " +
			"and reuse no cached LLM results, since only a signed cache checkpoint makes one trusted")
	}
	for _, tenantID := range slices.Sorted(maps.Keys(attestors.Tenants)) {
		switch {
		case attestors.Tenants[tenantID] == nil && attestors.Default != nil:
			slog.Warn("attestation is disabled for a tenant (attestation.tenant_overrides): its pipeline jobs complete without "+
				"in-toto attestations and reuse no cached LLM results, since only a signed cache checkpoint makes one trusted",
				"tenant_id", tenantID)
		case attestors.Tenants[tenantID] != nil && attestors.Default == nil:
			slog.Info("attestation is enabled for a tenant (attestation.tenant_overrides)", "tenant_id", tenantID)
		}
	}
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

	// Any certificate the CA signed could otherwise name any tenant in the
	// x-tenant-id header, so only the gateway's own client certificate may.
	if len(cfg.TLS.ServiceIdentities) == 0 {
		return errNoServiceIdentities("pipeline", "gateway")
	}
	serviceIdentities := make([]authn.X509Match, len(cfg.TLS.ServiceIdentities))
	for i, m := range cfg.TLS.ServiceIdentities {
		serviceIdentities[i] = x509Match(m)
	}

	if cfg.Tenants.DefaultTenant == "" {
		return errors.New("pipeline role: tenants.default_tenant must be set")
	}
	if err := checkDefaultTenant(ctx, "pipeline", cfg, shared.appPool); err != nil {
		return err
	}

	pipelineSvc, err := buildPipelineService(cfg, shared)
	if err != nil {
		return err
	}
	rt.pipelineService = pipelineSvc

	server, err := pipeline.NewServer(ctx, pipeline.ServerConfig{
		Addr:              cfg.Pipeline.Addr,
		TLS:               cfg.TLS,
		Service:           pipelineSvc,
		ServiceIdentities: serviceIdentities,
	})
	if err != nil {
		return fmt.Errorf("create pipeline server: %w", err)
	}
	rt.pipelineServer = server

	rt.healthServer = newHealthServer(cfg.Health.Addr, roleHealthChecks(cfg.Role, shared.appPool, shared.natsClient, shared.graphDB)...)
	return nil
}

// Compile-time proof that the results service can back the gateway. It lives
// here, the composition root, because internal/results must not import
// internal/gateway.
var (
	_ gateway.MappingsBackend   = (*results.Service)(nil)
	_ gateway.CandidatesBackend = (*results.Service)(nil)
)

// newMappingsBackend builds the results reader both gateway wirings share. It
// backs the mappings and the candidates RPCs.
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
// for the graph role to project, so this role holds no graph credentials:
// graph, crosswalk and edge-cleaning RPCs go to the graph role's listener
// through a Connect client. Fails closed if cfg.Pipeline.Endpoint or
// cfg.Graph.Endpoint isn't configured -- there is no in-process
// pipeline.Service or graph.Service to fall back to once the roles are split.
func attachGateway(ctx context.Context, cfg *config.Config, shared *sharedResources, rt *runtime) error {
	if cfg.Pipeline.Endpoint == "" {
		return errors.New("gateway role: pipeline.endpoint must be set to reach a standalone pipeline role")
	}
	if cfg.Graph.Endpoint == "" {
		return errors.New("gateway role: graph.endpoint must be set to reach a standalone graph role, which serves the graph, crosswalk and edge-cleaning RPCs")
	}

	// Built before buildIngestGateway's config checks, which need it as an
	// argument. Construction only resolves TLS material and never dials, so
	// a later refusal discards nothing but an unused client.
	pipelineBackend, err := gateway.NewConnectPipelineBackend(ctx, cfg.Pipeline.Endpoint, cfg.TLS)
	if err != nil {
		return fmt.Errorf("create pipeline backend: %w", err)
	}

	graphBackend, err := gateway.NewConnectGraphBackend(ctx, cfg.Graph.Endpoint, cfg.TLS)
	if err != nil {
		return fmt.Errorf("create graph backend: %w", err)
	}

	// No retention wiring: the standalone gateway role holds no purge_user
	// connection, so catalog retire fails closed there.
	gatewaySvc, err := buildIngestGateway(ctx, config.RoleGateway, cfg, shared, pipelineBackend, nil,
		gateway.WithGraphBackend(graphBackend),
		gateway.WithCrosswalkBackend(graphBackend))
	if err != nil {
		return err
	}

	server, err := gateway.NewServer(ctx, gateway.ServerConfig{
		TenantStatus: gateway.NewPoolTenantStatus(shared.appPool),
		Addr:         cfg.Server.Addr,
		TLS:          cfg.TLS,
		Service:      gatewaySvc,
		Cleaning:     graphBackend.Cleaning(),
	})
	if err != nil {
		return fmt.Errorf("create gateway server: %w", err)
	}
	rt.gatewayServer = server
	return nil
}
