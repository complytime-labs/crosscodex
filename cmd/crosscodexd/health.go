package main

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// healthCheckHandler returns an http.Handler for GET /healthz that runs each
// check in order and reports the first failure. With zero checks, it is a
// pure liveness probe (the process is up and serving) -- used by the worker
// role, which has no dependency this handler can meaningfully check (there
// is no NATS connectivity probe in pkg/natsbus.Client today).
func healthCheckHandler(checks ...func(context.Context) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, check := range checks {
			if err := check(r.Context()); err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(map[string]string{
					"status": "unhealthy",
					"error":  err.Error(),
				})
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "healthy"})
	})
}

// newHealthServer builds an *http.Server serving GET /healthz on addr,
// backed by healthCheckHandler(checks...). The caller starts it with
// ListenAndServe in a goroutine and stops it with Shutdown, matching
// internal/gateway.Server's Start/Shutdown shape.
func newHealthServer(addr string, checks ...func(context.Context) error) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", healthCheckHandler(checks...))
	return &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
}
