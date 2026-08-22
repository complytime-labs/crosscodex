package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/complytime-labs/crosscodex/internal/analysis"
	"github.com/complytime-labs/crosscodex/internal/gateway"
	"github.com/complytime-labs/crosscodex/internal/graph"
	"github.com/complytime-labs/crosscodex/internal/pipeline"
	pipelineattestation "github.com/complytime-labs/crosscodex/internal/pipeline/attestation"
	"github.com/complytime-labs/crosscodex/internal/synthesis"
	"github.com/complytime-labs/crosscodex/internal/worker"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/config"
	dbpkg "github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/prompt"
	"github.com/complytime-labs/crosscodex/pkg/storage"
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
}

// close releases every resource bootstrap constructed. Safe to call on a
// partially-populated runtime; safe to call after stop.
func (rt *runtime) close() {
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
		if err := attachGatewayServer(ctx, cfg, shared, rt, pipelineSvc); err != nil {
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

// attachWorker wires the LLM worker into rt. No database resources are
// involved -- a worker replica scales horizontally without Postgres
// credentials.
func attachWorker(cfg *config.Config, shared *sharedResources, rt *runtime) error {
	workerCfg := cfg.Worker
	workerCfg.LLM = cfg.LLM
	rt.workerService = worker.New(shared.natsClient, shared.llmClient, workerCfg)
	rt.healthServer = newHealthServer(cfg.Health.Addr)
	return nil
}

// attachGraph wires the graph-materialization subscriber into rt. This is
// the same construction the daemon has always performed, now reachable via
// explicit role selection instead of unconditionally.
func attachGraph(cfg *config.Config, shared *sharedResources, rt *runtime) error {
	tenantConn := dbpkg.NewTenantPool(shared.appPool)
	resolver := graph.NewPGResolver(tenantConn)
	rt.graphService = graph.New(shared.graphDB_, shared.vectorDB, shared.natsClient, graph.WithResolver(resolver))
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

	tenantConn := dbpkg.NewTenantPool(shared.appPool)

	prompts, err := prompt.NewRegistry(cfg.Prompt)
	if err != nil {
		return nil, fmt.Errorf("create prompt registry: %w", err)
	}

	storageProvider, err := storage.NewLocal(cfg.Storage.Objects.BasePath, cfg.Tenants.DefaultTenant)
	if err != nil {
		return nil, fmt.Errorf("create storage provider: %w", err)
	}

	store := pipeline.NewPGStore(tenantConn, shared.appPool)

	registry, err := pipeline.NewProductionRegistry(shared.llmClient, shared.vectorDB, storageProvider, prompts, tenantConn, cfg.Analysis)
	if err != nil {
		return nil, fmt.Errorf("build analyzer registry: %w", err)
	}

	dbReporter := pipeline.NewDBStageReporter(analysis.NewNATSStageReporter(shared.natsClient), store)
	engine := analysis.NewWithNATS(registry, shared.natsClient, cfg.Analysis.Engine, taskTypesByAnalyzer, analysis.WithStageReporter(dbReporter))

	candRegistry, err := pipeline.BuildCandidateRegistry(cfg.Analysis.Candidates)
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

	synth := synthesis.New(tenantConn, cfg.Synthesis, cfg.Analysis.Relationship.ActionableTypes)

	attestor, err := attestation.NewGenerator(&attestation.FileKeyProvider{
		PrivateKeyPath: cfg.Attestation.PrivateKeyPath,
		PublicKeyPath:  cfg.Attestation.PublicKeyPath,
	})
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

// attachGatewayServer wires the gateway HTTP server into rt using the
// given PipelineBackend, which may be an in-process pipeline.Service
// (RoleAll) or a network Connect client (attachGateway, standalone role).
func attachGatewayServer(ctx context.Context, cfg *config.Config, shared *sharedResources, rt *runtime, backend gateway.PipelineBackend) error {
	gatewaySvc := gateway.NewService(
		gateway.WithAdminBackend(gateway.NewPoolAdminBackend(shared.appPool)),
		gateway.WithPipelineBackend(backend),
		gateway.WithMaxUploadSize(cfg.Server.MaxUploadSize),
	)

	server, err := gateway.NewServer(ctx, gateway.ServerConfig{
		Addr:    cfg.Server.Addr,
		TLS:     cfg.TLS,
		Service: gatewaySvc,
	})
	if err != nil {
		return fmt.Errorf("create gateway server: %w", err)
	}
	rt.gatewayServer = server
	return nil
}

// attachGateway wires the standalone gateway role: an HTTP server whose
// PipelineBackend is a Connect client reaching a separately-deployed
// pipeline role over the network. Fails closed if cfg.Pipeline.Endpoint
// isn't configured -- there is no in-process pipeline.Service to fall back
// to once gateway and pipeline are split into independent roles.
func attachGateway(ctx context.Context, cfg *config.Config, shared *sharedResources, rt *runtime) error {
	if cfg.Pipeline.Endpoint == "" {
		return errors.New("gateway role: pipeline.endpoint must be set to reach a standalone pipeline role")
	}

	backend, err := gateway.NewConnectPipelineBackend(ctx, cfg.Pipeline.Endpoint, cfg.TLS)
	if err != nil {
		return fmt.Errorf("create pipeline backend: %w", err)
	}

	return attachGatewayServer(ctx, cfg, shared, rt, backend)
}
