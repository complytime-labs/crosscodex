package main

import (
	"context"
	"net/http"
	"strings"

	"github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/tlsconfig"
)

// parseEndpoint splits an optional scheme from a gateway address. It returns the
// bare host:port and whether the caller requested TLS via an https scheme.
func parseEndpoint(raw string) (hostPort string, useTLS bool) {
	switch {
	case strings.HasPrefix(raw, "https://"):
		return strings.TrimPrefix(raw, "https://"), true
	case strings.HasPrefix(raw, "http://"):
		return strings.TrimPrefix(raw, "http://"), false
	default:
		return raw, false
	}
}

// applyClientTLSFlags overlays --tls-ca/--tls-cert/--tls-key onto the client TLS
// config and derives a mode when cert material is supplied without one. Empty
// flags never clobber existing config; an explicit mode is preserved.
func applyClientTLSFlags(ca, cert, key string, cfg *config.ClientConfig) {
	if cfg == nil {
		return
	}
	if ca != "" {
		cfg.TLS.CA = ca
	}
	if cert != "" {
		cfg.TLS.Cert = cert
	}
	if key != "" {
		cfg.TLS.Key = key
	}
	// Derive a TLS mode from the supplied cert material. An unset mode is derived
	// whenever a CA is present. Explicit --tls-* flags additionally override a
	// config-defaulted "off" mode: the loaded client config defaults TLS.Mode to
	// "off", and without this override BuildTLSConfig returns a nil config for
	// mode "off", so the client silently falls back to system roots and rejects
	// the server certificate as "unknown authority". Empty flags never upgrade
	// "off" -- that stays a deliberate opt-out.
	flagsProvided := ca != "" || cert != "" || key != ""
	if (cfg.TLS.Mode == "" || (flagsProvided && cfg.TLS.Mode == "off")) && cfg.TLS.CA != "" {
		if cfg.TLS.Cert != "" && cfg.TLS.Key != "" {
			cfg.TLS.Mode = "mutual"
		} else {
			cfg.TLS.Mode = "server-only"
		}
	}
}

// clientUsesTLS reports whether the explicit-endpoint dial should use TLS.
func clientUsesTLS(schemeTLS bool, cfg config.ClientConfig) bool {
	return schemeTLS || cfg.TLS.Mode == "mutual" || cfg.TLS.Mode == "server-only"
}

// tlsHTTPClient builds an *http.Client whose transport enforces the resolved
// client TLS config (CA trust + client cert for mutual auth).
func tlsHTTPClient(ctx context.Context, cfg config.TLSConfig) (*http.Client, error) {
	tc, err := tlsconfig.BuildTLSConfig(ctx, cfg, "")
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: tc}}, nil
}

// newTLSGatewayClient dials the gateway over https with the given TLS client.
func newTLSGatewayClient(hostPort string, httpClient *http.Client) crosscodexv1connect.GatewayServiceClient {
	return crosscodexv1connect.NewGatewayServiceClient(httpClient, "https://"+hostPort)
}
