//go:build !integration

package main

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/testcerts"
)

var _ = Describe("runHealthcheck", func() {
	var certDir string

	// writeConfig materializes a user config.yaml under a temp XDG_CONFIG_HOME
	// so runHealthcheck's option-less config.NewLoader().Load picks it up as
	// the user-config layer. GinkgoT().Setenv restores XDG_CONFIG_HOME after
	// the spec.
	writeConfig := func(body string) {
		xdg := GinkgoT().TempDir()
		cfgDir := filepath.Join(xdg, "crosscodex")
		Expect(os.MkdirAll(cfgDir, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(body), 0o600)).To(Succeed())
		GinkgoT().Setenv("XDG_CONFIG_HOME", xdg)
	}

	// tlsBlock renders a valid mutual-TLS stanza pointing at the client
	// identity and CA the probe trusts.
	tlsBlock := func() string {
		return fmt.Sprintf("tls:\n  mode: mutual\n  ca: %s\n  cert: %s\n  key: %s\n",
			filepath.Join(certDir, "ca.pem"),
			filepath.Join(certDir, "client.pem"),
			filepath.Join(certDir, "client-key.pem"))
	}

	// startHealthServer stands up an HTTPS server presenting the test PKI's
	// server cert (SAN 127.0.0.1), so the CA-trusting probe verifies it.
	startHealthServer := func(status int) *httptest.Server {
		srvCert, err := tls.LoadX509KeyPair(
			filepath.Join(certDir, "server.pem"),
			filepath.Join(certDir, "server-key.pem"))
		Expect(err).NotTo(HaveOccurred())
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))
		srv.TLS = &tls.Config{ //nolint:gosec // MinVersion default acceptable for an in-process test server
			Certificates: []tls.Certificate{srvCert},
		}
		srv.StartTLS()
		return srv
	}

	portOf := func(srv *httptest.Server) int {
		return srv.Listener.Addr().(*net.TCPAddr).Port
	}

	BeforeEach(func() {
		certDir = GinkgoT().TempDir()
		pki, err := testcerts.Generate()
		Expect(err).NotTo(HaveOccurred(), "generate test PKI")
		Expect(pki.WriteToDir(certDir)).To(Succeed(), "write test PKI")
	})

	It("returns 0 when the gateway is healthy", func() {
		srv := startHealthServer(http.StatusOK)
		defer srv.Close()
		writeConfig(fmt.Sprintf("server:\n  addr: \":%d\"\n%s", portOf(srv), tlsBlock()))
		Expect(runHealthcheck()).To(Equal(0))
	})

	It("returns 1 when the gateway reports unhealthy", func() {
		srv := startHealthServer(http.StatusServiceUnavailable)
		defer srv.Close()
		writeConfig(fmt.Sprintf("server:\n  addr: \":%d\"\n%s", portOf(srv), tlsBlock()))
		Expect(runHealthcheck()).To(Equal(1))
	})

	It("returns 1 when the gateway is unreachable", func() {
		srv := startHealthServer(http.StatusOK)
		port := portOf(srv)
		srv.Close() // nothing listens on port now -> probe dial fails
		writeConfig(fmt.Sprintf("server:\n  addr: \":%d\"\n%s", port, tlsBlock()))
		Expect(runHealthcheck()).To(Equal(1))
	})

	It("returns 1 when the TLS material cannot be loaded", func() {
		writeConfig(fmt.Sprintf("server:\n  addr: \":50051\"\ntls:\n  mode: mutual\n  ca: %s\n  cert: %s\n  key: %s\n",
			filepath.Join(certDir, "ca.pem"),
			filepath.Join(certDir, "missing.pem"),
			filepath.Join(certDir, "client-key.pem")))
		Expect(runHealthcheck()).To(Equal(1))
	})

	It("returns 1 when the config is invalid", func() {
		writeConfig("tls:\n  mode: not-a-real-mode\n")
		Expect(runHealthcheck()).To(Equal(1))
	})
})
