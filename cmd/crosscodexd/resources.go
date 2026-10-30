package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

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
// without need.nats is refused rather than run unaudited. With need.llm it
// also refuses an LLM gateway it can't reach (see probeLLMGateway), even when
// a ResourceOption replaces the client.
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
		if err := probeLLMGateway(ctx, cfg.LLM, httpClient); err != nil {
			res.close()
			return nil, err
		}
	}

	for _, opt := range opts {
		opt(res)
	}

	return res, nil
}

// probeLLMGateway sends one GET to the LLM gateway's base URL so the daemon
// refuses to start, rather than starting healthy and failing every LLM call,
// when the gateway can't be reached. Any HTTP response counts as reachable,
// whatever its status: LiteLLM's own status for / or Praxis's 405 for a
// non-POST. A redirect is such a response and is not followed, because
// following it would touch another host or scheme than the one configured.
// Only a transport error fails: DNS, connect, TLS verification, an mTLS
// rejection or a timeout. llmclient.Health isn't used because its
// GET /v1/models isn't served by the default Praxis config.
//
// httpClient is the TLS client from llmHTTPClient, or nil for the default
// client. The probe sends no API key and is bounded by llm.timeout, with
// llmclient's default when that is zero. The error never carries the URL's
// password or query, which may hold credentials.
func probeLLMGateway(ctx context.Context, llm config.LLMConfig, httpClient *http.Client) error {
	// The client is shared with llmclient, so change the redirect policy on a copy.
	probe := http.Client{}
	if httpClient != nil {
		probe = *httpClient
	}
	probe.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	timeout := time.Duration(llm.Timeout) * time.Second
	if timeout <= 0 {
		timeout = llmclient.DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// NewRequestWithContext's error quotes the raw URL, so it is not wrapped.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, llm.GatewayURL, http.NoBody)
	if err != nil {
		return errInvalidGatewayURL()
	}
	resp, err := probe.Do(req)
	if err == nil {
		// The status is irrelevant and the body unread, so a close
		// error can't change the outcome.
		_ = resp.Body.Close()
		return nil
	}

	shownURL := redactURL(req.URL)
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		// *url.Error repeats the request URL, query included.
		urlErr.URL = shownURL
	}
	return fmt.Errorf("cannot reach LLM gateway %s: %w. crosscodexd refuses to start because every LLM call would fail. "+
		"Check that the gateway service is running (with compose, include its fragment, e.g. -f deploy/compose.gateway-litellm.yaml "+
		"or -f deploy/compose.gateway-praxis.yaml), that its TLS certificate names the gateway host (after switching LLM_GATEWAY, "+
		"regenerate the certificates as the \"LLM gateway\" section of deploy/README.md describes), and that llm.gateway_url is right",
		shownURL, err)
}

// errInvalidGatewayURL is deliberately free of the configured value, which may hold a password.
func errInvalidGatewayURL() error {
	return errors.New("llm.gateway_url is not a valid URL. crosscodexd refuses to start because every LLM call would fail. " +
		"Set llm.gateway_url to an absolute http:// or https:// URL such as https://llm-gateway:4000")
}

// redactURL renders u without its password, query or fragment.
func redactURL(u *url.URL) string {
	c := *u
	c.RawQuery, c.ForceQuery, c.Fragment, c.RawFragment = "", false, "", ""
	return c.Redacted()
}
