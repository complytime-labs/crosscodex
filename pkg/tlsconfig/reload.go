package tlsconfig

import (
	"crypto/tls"
	"fmt"
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
