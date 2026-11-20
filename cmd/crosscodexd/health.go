package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/complytime-labs/crosscodex/internal/gateway"
	"github.com/complytime-labs/crosscodex/pkg/config"
	dbpkg "github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
)

// healthCheckHandler returns an http.Handler for GET /healthz that runs each
// check in order and reports the first failure as a 503 naming that
// dependency. The check's error is logged, never written to the body:
// /healthz is unauthenticated and the error can carry hosts, ports and roles
// (CWE-209). With zero checks it is a pure liveness probe.
func healthCheckHandler(checks ...gateway.DependencyCheck) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, c := range checks {
			err := c.Check(r.Context())
			if err == nil {
				continue
			}
			slog.WarnContext(r.Context(), "healthz: dependency check failed", "dependency", c.Name, "error", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"status": "unhealthy",
				"error":  c.Name + " unreachable",
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "healthy"})
	})
}

// newHealthServer builds an *http.Server serving GET /healthz on addr,
// backed by healthCheckHandler(checks...). runtime.start binds addr
// synchronously and serves it in a goroutine; stop calls Shutdown, matching
// internal/gateway.Server's Start/Shutdown shape.
func newHealthServer(addr string, checks ...gateway.DependencyCheck) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", healthCheckHandler(checks...))
	return &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
}

// pinger is the one *sql.DB method the graph database check needs.
type pinger interface {
	PingContext(ctx context.Context) error
}

// errNATSNotConnected is the nats check's error while the client is
// reconnecting or closed.
var errNATSNotConnected = errors.New("nats client is not connected to the server")

// roleHealthChecks returns the dependency checks role's /healthz runs: only
// the dependencies that role actually uses (see resourcesForRole), so a
// failing check means the role cannot do its job and restarting or rerouting
// it is the right response.
//
// gateway and all omit the app database because they serve /healthz through
// gateway.PoolAdminBackend, which always reports it; the split roles serve
// healthCheckHandler and list it themselves.
//
// The LLM is deliberately not a health dependency for any role: restarting a
// process does not fix a down LLM, the per-job LLM probe already reports
// llm_unavailable on the job, and Praxis does not serve /v1/models, so a
// generic probe would mark a working deployment unhealthy.
func roleHealthChecks(role string, pool dbpkg.Pool, bus natsbus.Client, graphDB pinger) []gateway.DependencyCheck {
	database := gateway.DependencyCheck{Name: "database", Check: dbPingCheck(pool)}
	nats := gateway.DependencyCheck{Name: "nats", Check: func(context.Context) error {
		if !bus.Connected() {
			return errNATSNotConnected
		}
		return nil
	}}
	graph := gateway.DependencyCheck{Name: "graph_database", Check: func(ctx context.Context) error {
		return graphDB.PingContext(ctx)
	}}

	switch role {
	case config.RoleWorker:
		return []gateway.DependencyCheck{nats}
	case config.RolePipeline:
		return []gateway.DependencyCheck{database, nats}
	case config.RoleGraph:
		return []gateway.DependencyCheck{database, nats, graph}
	case config.RoleGateway:
		return []gateway.DependencyCheck{nats}
	case config.RoleAll:
		return []gateway.DependencyCheck{nats, graph}
	default:
		return nil
	}
}
