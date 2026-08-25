package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/testcerts"
	"github.com/complytime-labs/crosscodex/pkg/config"
	pkigen "github.com/complytime-labs/crosscodex/pkg/tlsconfig/pki"
)

var _ = Describe("parseEndpoint", func() {
	DescribeTable("scheme handling",
		func(raw, wantHost string, wantTLS bool) {
			host, useTLS := parseEndpoint(raw)
			Expect(host).To(Equal(wantHost))
			Expect(useTLS).To(Equal(wantTLS))
		},
		Entry("https strips scheme, enables TLS", "https://host:50051", "host:50051", true),
		Entry("http strips scheme, plaintext", "http://host:50051", "host:50051", false),
		Entry("bare host:port, plaintext", "host:50051", "host:50051", false),
	)
})

var _ = Describe("applyClientTLSFlags", func() {
	It("derives mutual mode when ca+cert+key are supplied", func() {
		cfg := &config.ClientConfig{}
		applyClientTLSFlags("/ca.pem", "/client.pem", "/client-key.pem", cfg)
		Expect(cfg.TLS.Mode).To(Equal("mutual"))
		Expect(cfg.TLS.CA).To(Equal("/ca.pem"))
		Expect(cfg.TLS.Cert).To(Equal("/client.pem"))
		Expect(cfg.TLS.Key).To(Equal("/client-key.pem"))
	})
	It("derives server-only mode when only ca is supplied", func() {
		cfg := &config.ClientConfig{}
		applyClientTLSFlags("/ca.pem", "", "", cfg)
		Expect(cfg.TLS.Mode).To(Equal("server-only"))
	})
	It("preserves an explicit mode and does not clobber config with empty flags", func() {
		cfg := &config.ClientConfig{}
		cfg.TLS.Mode = "off"
		cfg.TLS.CA = "/existing-ca.pem"
		applyClientTLSFlags("", "", "", cfg)
		Expect(cfg.TLS.Mode).To(Equal("off"))
		Expect(cfg.TLS.CA).To(Equal("/existing-ca.pem"))
	})
	It("upgrades a config-defaulted off mode when explicit ca+cert+key flags are given", func() {
		// The loaded client config defaults TLS.Mode to "off". Passing the TLS
		// flags must enable mutual TLS; otherwise BuildTLSConfig returns a nil
		// config for mode "off" and the client silently falls back to system
		// roots, rejecting the server cert as "unknown authority".
		cfg := &config.ClientConfig{}
		cfg.TLS.Mode = "off"
		applyClientTLSFlags("/ca.pem", "/client.pem", "/client-key.pem", cfg)
		Expect(cfg.TLS.Mode).To(Equal("mutual"))
	})
	It("upgrades a config-defaulted off mode to server-only when only the ca flag is given", func() {
		cfg := &config.ClientConfig{}
		cfg.TLS.Mode = "off"
		applyClientTLSFlags("/ca.pem", "", "", cfg)
		Expect(cfg.TLS.Mode).To(Equal("server-only"))
	})
})

var _ = Describe("clientUsesTLS", func() {
	It("is true for https scheme, mutual, or server-only; false otherwise", func() {
		Expect(clientUsesTLS(true, config.ClientConfig{})).To(BeTrue())
		Expect(clientUsesTLS(false, config.ClientConfig{TLS: config.TLSConfig{Mode: "mutual"}})).To(BeTrue())
		Expect(clientUsesTLS(false, config.ClientConfig{TLS: config.TLSConfig{Mode: "server-only"}})).To(BeTrue())
		Expect(clientUsesTLS(false, config.ClientConfig{})).To(BeFalse())
		Expect(clientUsesTLS(false, config.ClientConfig{TLS: config.TLSConfig{Mode: "off"}})).To(BeFalse())
	})
})

var _ = Describe("tlsHTTPClient (mutual TLS enforced)", func() {
	var dir string

	BeforeEach(func() {
		dir = GinkgoT().TempDir()
		bundle, err := pkigen.GenerateDevPKI(
			pkigen.WithOrganization("CrossCodex Test"),
			pkigen.WithValidDuration(time.Hour),
			pkigen.WithDNSNames("localhost"),
			pkigen.WithIPs(net.IPv4(127, 0, 0, 1), net.IPv6loopback),
		)
		Expect(err).NotTo(HaveOccurred())
		pki := &testcerts.PKI{
			CACert:     bundle.CA.CertPEM,
			CAKey:      bundle.CA.KeyPEM,
			ServerCert: bundle.Server.CertPEM,
			ServerKey:  bundle.Server.KeyPEM,
			ClientCert: bundle.Client.CertPEM,
			ClientKey:  bundle.Client.KeyPEM,
		}
		Expect(pki.WriteToDir(dir)).To(Succeed())
	})

	newServer := func() *httptest.Server {
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		caPEM, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
		Expect(err).NotTo(HaveOccurred())
		pool := mustCertPool(caPEM)
		serverCert := mustLoadKeyPair(filepath.Join(dir, "server.pem"), filepath.Join(dir, "server-key.pem"))
		srv.TLS = mustMutualServerTLS(serverCert, pool)
		srv.StartTLS()
		return srv
	}

	It("connects when the client presents a CA-trusted client cert", func() {
		srv := newServer()
		defer srv.Close()
		httpClient, err := tlsHTTPClient(context.Background(), config.TLSConfig{
			Mode: "mutual",
			CA:   filepath.Join(dir, "ca.pem"),
			Cert: filepath.Join(dir, "client.pem"),
			Key:  filepath.Join(dir, "client-key.pem"),
		})
		Expect(err).NotTo(HaveOccurred())
		resp, err := httpClient.Get(srv.URL)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
	})

	It("is rejected when the client presents no client cert", func() {
		srv := newServer()
		defer srv.Close()
		httpClient, err := tlsHTTPClient(context.Background(), config.TLSConfig{
			Mode: "server-only",
			CA:   filepath.Join(dir, "ca.pem"),
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = httpClient.Get(srv.URL)
		Expect(err).To(HaveOccurred())
	})
})

// mustCertPool loads a CA certificate into a cert pool and fails the test on error.
func mustCertPool(caPEM []byte) *x509.CertPool {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		Expect(pool.AppendCertsFromPEM(caPEM)).To(BeTrue(), "failed to append CA cert to pool")
	}
	return pool
}

// mustLoadKeyPair loads a certificate and key pair and fails the test on error.
func mustLoadKeyPair(certPath, keyPath string) tls.Certificate {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	Expect(err).To(Succeed())
	return cert
}

// mustMutualServerTLS returns a *tls.Config for a mutual TLS server.
func mustMutualServerTLS(cert tls.Certificate, pool *x509.CertPool) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}
}
