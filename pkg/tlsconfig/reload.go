package tlsconfig

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"math"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// makeGetCertificate returns a GetCertificate callback that re-reads certFile
// and keyFile from disk on each TLS handshake. This enables certificate
// rotation without process restart.
func makeGetCertificate(certFile, keyFile string) func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return func(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("reload certificate %s: %w", certFile, err)
		}
		return &cert, nil
	}
}

// makeGetCertificateWithMeter returns a GetCertificate callback that re-reads
// certFile and keyFile from disk on each TLS handshake and records reload metrics.
func makeGetCertificateWithMeter(certFile, keyFile string, m metric.Meter) func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return func(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)

		pathAttr := attribute.String("cert.path", certFile)
		if err != nil {
			recordReloadMetrics(m, pathAttr, false, nil)
			return nil, fmt.Errorf("reload certificate %s: %w", certFile, err)
		}

		// Parse leaf to extract NotAfter for expiry gauge.
		var leaf *x509.Certificate
		if len(cert.Certificate) > 0 {
			leaf, _ = x509.ParseCertificate(cert.Certificate[0])
		}

		recordReloadMetrics(m, pathAttr, true, leaf)
		return &cert, nil
	}
}

// makeGetClientCertificate returns a GetClientCertificate callback that re-reads
// certFile and keyFile from disk on each TLS handshake. Used in mutual TLS mode
// for client certificate reloading.
func makeGetClientCertificate(certFile, keyFile string) func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return func(_ *tls.CertificateRequestInfo) (*tls.Certificate, error) {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("reload client certificate %s: %w", certFile, err)
		}
		return &cert, nil
	}
}

// makeGetClientCertificateWithMeter returns a GetClientCertificate callback that
// re-reads certFile and keyFile from disk on each TLS handshake and records
// reload metrics.
func makeGetClientCertificateWithMeter(certFile, keyFile string, m metric.Meter) func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return func(_ *tls.CertificateRequestInfo) (*tls.Certificate, error) {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)

		pathAttr := attribute.String("cert.path", certFile)
		if err != nil {
			recordReloadMetrics(m, pathAttr, false, nil)
			return nil, fmt.Errorf("reload client certificate %s: %w", certFile, err)
		}

		var leaf *x509.Certificate
		if len(cert.Certificate) > 0 {
			leaf, _ = x509.ParseCertificate(cert.Certificate[0])
		}

		recordReloadMetrics(m, pathAttr, true, leaf)
		return &cert, nil
	}
}

// recordReloadMetrics records the cert reload counter and expiry gauge.
func recordReloadMetrics(m metric.Meter, pathAttr attribute.KeyValue, success bool, leaf *x509.Certificate) {
	if m == nil {
		return
	}

	resultAttr := attribute.String("result", "success")
	if !success {
		resultAttr = attribute.String("result", "failure")
	}

	ctx := context.Background()
	if counter, err := m.Int64Counter("tlsconfig.cert.reload.total",
		metric.WithDescription("Total certificate reload attempts")); err == nil {
		counter.Add(ctx, 1, metric.WithAttributes(pathAttr, resultAttr))
	}

	if success && leaf != nil {
		daysUntilExpiry := int64(math.Ceil(time.Until(leaf.NotAfter).Hours() / 24))
		if gauge, err := m.Int64Gauge("tlsconfig.cert.expiry_days",
			metric.WithDescription("Days until certificate expires")); err == nil {
			gauge.Record(ctx, daysUntilExpiry, metric.WithAttributes(pathAttr))
		}
	}
}
