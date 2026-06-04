package tlsconfig

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

// resolvedConfig holds the merged TLS settings after applying target overrides.
type resolvedConfig struct {
	mode string
	ca   string
	cert string
	key  string
}

// BuildTLSConfig produces a *tls.Config for the given target name.
// An empty target uses global config only. A non-empty target deep-merges
// Targets[target] over global settings. Returns (nil, nil) when the resolved
// mode is "off" or empty.
func BuildTLSConfig(ctx context.Context, cfg config.TLSConfig, target string) (*tls.Config, error) {
	var r Resolver
	return r.BuildTLSConfig(ctx, cfg, target)
}

// BuildTLSConfig is the method form of the package-level function.
func (r Resolver) BuildTLSConfig(ctx context.Context, cfg config.TLSConfig, target string) (*tls.Config, error) {
	start := time.Now()
	resolved := mergeConfig(cfg, target)

	// Start build span if tracer is available.
	var span trace.Span
	if r.Tracer != nil {
		ctx, span = r.Tracer.Start(ctx, "tlsconfig.BuildTLSConfig",
			trace.WithAttributes(
				attribute.String("tls.target", target),
				attribute.String("tls.mode", resolved.mode),
				attribute.Bool("tls.fips.enabled", cfg.FIPS.Enabled),
			),
		)
		defer span.End()
	}

	var result *tls.Config
	var err error

	switch resolved.mode {
	case "", "off":
		result, err = nil, nil
	case "server-only", "mutual":
		result, err = buildTLSConfigFromResolved(ctx, r, cfg, resolved, target)
	default:
		err = fmt.Errorf("target %q: %w: %q", target, ErrInvalidMode, resolved.mode)
	}

	// Record build metrics.
	if r.Meter != nil {
		successAttr := attribute.String("result", "success")
		if err != nil {
			successAttr = attribute.String("result", "failure")
		}
		if counter, cErr := r.Meter.Int64Counter("tlsconfig.build.total",
			metric.WithDescription("Total TLS config builds")); cErr == nil {
			counter.Add(ctx, 1, metric.WithAttributes(successAttr))
		}
		if hist, hErr := r.Meter.Int64Histogram("tlsconfig.build.duration_ms",
			metric.WithDescription("TLS config build duration in milliseconds")); hErr == nil {
			hist.Record(ctx, time.Since(start).Milliseconds(), metric.WithAttributes(successAttr))
		}
	}

	if err != nil && span != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}

	return result, err
}

// mergeConfig applies target overrides to the global TLS config.
// Non-zero override fields replace global values. Unknown targets fall through
// to global config without error.
func mergeConfig(cfg config.TLSConfig, target string) resolvedConfig {
	rc := resolvedConfig{
		mode: cfg.Mode,
		ca:   cfg.CA,
		cert: cfg.Cert,
		key:  cfg.Key,
	}

	if target == "" {
		return rc
	}

	override, ok := cfg.Targets[target]
	if !ok {
		return rc
	}

	if override.Mode != "" {
		rc.mode = override.Mode
	}
	if override.CA != "" {
		rc.ca = override.CA
	}
	if override.Cert != "" {
		rc.cert = override.Cert
	}
	if override.Key != "" {
		rc.key = override.Key
	}

	return rc
}

// buildTLSConfigFromResolved constructs a *tls.Config from merged settings.
func buildTLSConfigFromResolved(ctx context.Context, r Resolver, cfg config.TLSConfig, rc resolvedConfig, target string) (*tls.Config, error) {
	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS12, // DevSkim: ignore DS440001,DS112852 - TLS 1.2 minimum enforced
	}

	// --- Cipher suite pipeline ---
	// CipherSuites only controls TLS 1.2 negotiation. Go's TLS 1.3 suites are
	// not configurable via CipherSuites and are always the BoringCrypto-approved
	// set when boring.Enabled() is true. We do NOT set MaxVersion to restrict
	// TLS 1.3 because BoringCrypto constrains TLS 1.3 cipher suites at the C
	// layer, making them FIPS-compliant by construction.
	//
	// Cipher allow/deny lists are global policy (cfg.CipherAllow/CipherDeny),
	// not per-target. Per-target overrides only control mode, CA, cert, and key.
	var baseCiphers []uint16

	if cfg.FIPS.Enabled {
		// Verify BoringCrypto is available
		if _, err := VerifyFIPSBuild(); err != nil {
			return nil, err
		}
		baseCiphers = fipsCipherSuites()
	} else {
		baseCiphers = allNonInsecureCipherIDs()
	}

	// Apply allow/deny filters (always, not just FIPS)
	if cfg.FIPS.Enabled || len(cfg.CipherAllow) > 0 || len(cfg.CipherDeny) > 0 {
		filtered, err := filterCiphers(baseCiphers, cfg.CipherAllow, cfg.CipherDeny)
		if err != nil {
			return nil, fmt.Errorf("target %q: %w", target, err)
		}
		tlsCfg.CipherSuites = filtered
	}
	// If no FIPS and no allow/deny, leave CipherSuites nil (Go default selection)

	// --- Certificate loading ---
	// Cert+key are optional: a server needs them, a client typically doesn't.
	// BuildTLSConfig produces one *tls.Config; the caller decides server vs client.
	// We load what's provided and leave the rest nil.
	if rc.cert != "" && rc.key != "" {
		// Load static certificate for initial validation
		var certSpan trace.Span
		if r.Tracer != nil {
			_, certSpan = r.Tracer.Start(ctx, "tls.LoadCertificate",
				trace.WithAttributes(attribute.String("cert.path", rc.cert)),
			)
		}
		cert, err := tls.LoadX509KeyPair(rc.cert, rc.key)
		if err != nil {
			if certSpan != nil {
				certSpan.RecordError(err)
				certSpan.SetStatus(codes.Error, err.Error())
				certSpan.End()
			}
			return nil, fmt.Errorf("target %q: %w: %s: %w", target, ErrCertificateLoadFailed, rc.cert, err)
		}
		if certSpan != nil {
			certSpan.SetStatus(codes.Ok, "")
			certSpan.End()
		}
		tlsCfg.Certificates = []tls.Certificate{cert}

		// Set reload callback for server-side dynamic reloading
		if r.Meter != nil {
			tlsCfg.GetCertificate = makeGetCertificateWithMeter(rc.cert, rc.key, r.Meter)
		} else {
			tlsCfg.GetCertificate = makeGetCertificate(rc.cert, rc.key)
		}
	} else if rc.cert != "" && rc.key == "" {
		return nil, fmt.Errorf("target %q: cert specified without key: %w", target, ErrMissingKey)
	} else if rc.cert == "" && rc.key != "" {
		return nil, fmt.Errorf("target %q: key specified without cert: %w", target, ErrMissingCert)
	}
	// If both are empty, no certificates are loaded (valid for client-side usage)

	// --- CA loading ---
	if rc.ca != "" {
		var caSpan trace.Span
		if r.Tracer != nil {
			_, caSpan = r.Tracer.Start(ctx, "tls.LoadCA",
				trace.WithAttributes(attribute.String("ca.path", rc.ca)),
			)
		}
		caPEM, err := os.ReadFile(rc.ca)
		if err != nil {
			if caSpan != nil {
				caSpan.RecordError(err)
				caSpan.SetStatus(codes.Error, err.Error())
				caSpan.End()
			}
			return nil, fmt.Errorf("target %q: failed to read CA file %s: %w", target, rc.ca, err)
		}

		caPool := x509.NewCertPool()
		if !caPool.AppendCertsFromPEM(caPEM) {
			caErr := fmt.Errorf("target %q: %w: no valid certificates found in %s", target, ErrInvalidCertificate, rc.ca)
			if caSpan != nil {
				caSpan.RecordError(caErr)
				caSpan.SetStatus(codes.Error, caErr.Error())
				caSpan.End()
			}
			return nil, caErr
		}
		if caSpan != nil {
			caSpan.SetStatus(codes.Ok, "")
			caSpan.End()
		}

		tlsCfg.RootCAs = caPool
	}

	// --- Mutual TLS ---
	if rc.mode == "mutual" {
		if rc.ca == "" {
			return nil, fmt.Errorf("target %q: %w: mutual TLS requires a CA certificate for client verification", target, ErrMissingCA)
		}

		// ClientCAs reuses the RootCAs pool. This means the same CA must sign
		// both server and client certificates. If separate CA hierarchies are
		// needed, a dedicated ClientCA config field should be added.
		tlsCfg.ClientCAs = tlsCfg.RootCAs
		tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert

		// Set client cert reload callback
		if rc.cert != "" && rc.key != "" {
			if r.Meter != nil {
				tlsCfg.GetClientCertificate = makeGetClientCertificateWithMeter(rc.cert, rc.key, r.Meter)
			} else {
				tlsCfg.GetClientCertificate = makeGetClientCertificate(rc.cert, rc.key)
			}
		}
	}

	return tlsCfg, nil
}
