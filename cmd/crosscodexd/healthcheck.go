package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/tlsconfig"
)

// healthcheckURL returns the loopback /healthz URL the in-container probe
// dials for role. graph, pipeline and worker serve /healthz on their
// dedicated plain-HTTP listener (health.addr); gateway and all serve it on
// the gateway's TLS listener (server.addr). Only the port of the bind
// address is kept (e.g. ":9091", "0.0.0.0:9091" or "[::]:9091" all probe
// 127.0.0.1:9091); an addr that is not host:port is used verbatim as the port.
func healthcheckURL(role, serverAddr, healthAddr string) string {
	scheme, addr := "https", serverAddr
	switch role {
	case config.RoleGraph, config.RolePipeline, config.RoleWorker:
		scheme, addr = "http", healthAddr
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		port = addr
	}
	return fmt.Sprintf("%s://127.0.0.1:%s/healthz", scheme, port)
}

// probeHealthURL issues a GET to target and returns nil only when the
// response status is 200. For an HTTPS target tlsCfg supplies the CA trust
// (and optional client cert; the gateway listener uses
// VerifyClientCertIfGiven, so a client cert is accepted but not required); an
// HTTP target ignores it.
func probeHealthURL(ctx context.Context, target string, tlsCfg *tls.Config) error {
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
	if err != nil {
		return fmt.Errorf("build healthcheck request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("healthcheck request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: unhealthy status %d", resp.StatusCode)
	}
	return nil
}

// runHealthcheck loads config, resolves the role (roleFlag, the --role flag,
// overrides the configured role exactly as it does for the daemon), rejects a
// split role's invalid health.addr with the daemon's own message, builds
// the probe URL and, for the gateway's HTTPS listener, the TLS trust, and
// returns a process exit code (0 healthy, 1 unhealthy or misconfigured).
func runHealthcheck(roleFlag string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cfg, err := config.NewLoader().Load(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: load config: %v\n", err)
		return 1
	}
	role, err := config.ResolveRole(effectiveRole(roleFlag, cfg.Role))
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: --role: %v\n", err)
		return 1
	}
	if err := config.RequireHealthAddr(role, cfg.Health.Addr); err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	target := healthcheckURL(role, cfg.Server.Addr, cfg.Health.Addr)
	var tlsCfg *tls.Config
	if strings.HasPrefix(target, "https://") {
		tlsCfg, err = tlsconfig.BuildTLSConfig(ctx, cfg.TLS, "")
		if err != nil {
			fmt.Fprintf(os.Stderr, "healthcheck: build TLS config: %v\n", err)
			return 1
		}
	}
	if err := probeHealthURL(ctx, target, tlsCfg); err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	return 0
}
