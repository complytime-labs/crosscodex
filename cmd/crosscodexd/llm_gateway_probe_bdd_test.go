//go:build !integration

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/types"

	"github.com/complytime-labs/crosscodex/internal/testcerts"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	pkigen "github.com/complytime-labs/crosscodex/pkg/tlsconfig/pki"
)

// startTLSGateway starts an LLM gateway stand-in serving cert over TLS,
// requiring a client certificate signed by the suite CA when clientAuth asks
// for one. Handshake failures are logged to GinkgoWriter, not stderr.
func startTLSGateway(cert tls.Certificate, clientAuth tls.ClientAuthType, handler http.Handler) string {
	caPEM, err := os.ReadFile(testTLSWiringCAPath)
	Expect(err).NotTo(HaveOccurred())
	clientCAs := x509.NewCertPool()
	Expect(clientCAs.AppendCertsFromPEM(caPEM)).To(BeTrue())

	gw := httptest.NewUnstartedServer(handler)
	gw.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, ClientAuth: clientAuth, ClientCAs: clientCAs}
	gw.Config.ErrorLog = log.New(GinkgoWriter, "", 0)
	gw.StartTLS()
	DeferCleanup(gw.Close)
	return gw.URL
}

// suiteServerCert is the server certificate of the suite PKI (tlswiring
// BeforeSuite), valid for 127.0.0.1 and signed by testTLSWiringCAPath.
func suiteServerCert() tls.Certificate {
	dir := filepath.Dir(testTLSWiringCAPath)
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "server.pem"), filepath.Join(dir, "server-key.pem"))
	Expect(err).NotTo(HaveOccurred())
	return cert
}

// wrapsError matches an error whose chain holds a T.
func wrapsError[T error]() types.GomegaMatcher {
	return WithTransform(func(err error) bool {
		var target T
		return errors.As(err, &target)
	}, BeTrue())
}

var _ = Describe("buildSharedResources LLM gateway reachability probe", func() {
	var (
		cfg     *config.Config
		natsSrv *natsserver.Server
	)

	BeforeEach(func() {
		var err error
		natsSrv, err = natsserver.NewServer(&natsserver.Options{
			Host: "127.0.0.1", Port: -1, JetStream: true,
			StoreDir: GinkgoT().TempDir(), NoLog: true, NoSigs: true,
		})
		Expect(err).NotTo(HaveOccurred())
		go natsSrv.Start()
		DeferCleanup(natsSrv.Shutdown)
		Expect(natsSrv.ReadyForConnections(10 * time.Second)).To(BeTrue())

		cfg = &config.Config{}
		cfg.NATS.URL = natsSrv.ClientURL()
		cfg.NATS.Streams.AuditLLMRetention = 90 * 24 * time.Hour
		cfg.NATS.Streams.AuditEventsRetention = 30 * 24 * time.Hour
		cfg.LLM.Timeout = 5
	})

	build := func() (*sharedResources, error) {
		return buildSharedResources(context.Background(), cfg, requiredResources{nats: true, llm: true})
	}

	DescribeTable("starts when the gateway answers plain HTTP with any status, without sending the API key",
		func(status int) {
			GinkgoT().Setenv("CROSSCODEXD_PROBE_TEST_KEY", "sk-probe-secret")
			cfg.LLM.APIKeyRef = "env:CROSSCODEXD_PROBE_TEST_KEY"
			authHeaders := make(chan string, 10)
			gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				authHeaders <- r.Header.Get("Authorization")
				w.WriteHeader(status)
			}))
			DeferCleanup(gw.Close)
			cfg.LLM.GatewayURL = gw.URL

			res, err := build()

			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(res.close)
			Expect(res.llmClient).NotTo(BeNil())
			Expect(authHeaders).To(HaveLen(1))
			Expect(<-authHeaders).To(BeEmpty())
		},
		Entry("200 OK", http.StatusOK),
		Entry("404 Not Found", http.StatusNotFound),
		Entry("405 Method Not Allowed", http.StatusMethodNotAllowed),
		Entry("500 Internal Server Error", http.StatusInternalServerError),
	)

	DescribeTable("starts when the gateway's TLS certificate verifies",
		func(mode string, clientAuth tls.ClientAuthType) {
			cfg.TLS = config.TLSConfig{
				Mode: mode,
				CA:   testTLSWiringCAPath,
				Cert: testTLSWiringClientCertPath,
				Key:  testTLSWiringClientKeyPath,
			}
			cfg.LLM.GatewayURL = startTLSGateway(suiteServerCert(), clientAuth, http.NotFoundHandler())

			res, err := build()

			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(res.close)
			Expect(res.llmClient).NotTo(BeNil())
		},
		Entry("server-only TLS", "server-only", tls.NoClientCert),
		Entry("mutual TLS with the configured client certificate", "mutual", tls.RequireAndVerifyClientCert),
	)

	It("keeps refusing an empty gateway_url with ErrNoGateway", func() {
		cfg.LLM.GatewayURL = ""

		res, err := build()

		Expect(res).To(BeNil())
		Expect(err).To(MatchError(llmclient.ErrNoGateway))
		Eventually(natsSrv.NumClients).Should(BeZero())
	})

	It("redacts a password in gateway_url from the refusal", func() {
		gw := httptest.NewServer(http.NotFoundHandler())
		gw.Close()
		cfg.LLM.GatewayURL = "http://probe-user:probe-secret@" + gw.Listener.Addr().String()

		res, err := build()

		Expect(res).To(BeNil())
		Expect(err).To(MatchError(ContainSubstring("cannot reach LLM gateway http://probe-user:xxxxx@")))
		Expect(err.Error()).NotTo(ContainSubstring("probe-secret"))
	})

	It("does not follow a redirect: the response counts as reachable and the Location is never requested", func() {
		var targetHits atomic.Int32
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			targetHits.Add(1)
		}))
		targetURL := target.URL
		target.Close() // unreachable, so following the redirect would fail the probe
		gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, targetURL, http.StatusFound)
		}))
		DeferCleanup(gw.Close)
		cfg.LLM.GatewayURL = gw.URL

		res, err := build()

		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(res.close)
		Expect(targetHits.Load()).To(BeZero())
	})

	It("refuses an unparseable gateway_url as invalid, without echoing it or its password", func() {
		cfg.LLM.GatewayURL = "http://probe-user:probe-secret@gateway.invalid:notaport"

		res, err := build()

		Expect(res).To(BeNil())
		Expect(err).To(MatchError(ContainSubstring("llm.gateway_url is not a valid URL")))
		Expect(err.Error()).NotTo(SatisfyAny(
			ContainSubstring("probe-secret"),
			ContainSubstring("cannot reach"),
			ContainSubstring("notaport"),
		))
		Eventually(natsSrv.NumClients).Should(BeZero(), "the NATS client must be closed")
	})

	It("keeps a query token in gateway_url out of the refusal", func() {
		gw := httptest.NewServer(http.NotFoundHandler())
		gw.Close()
		cfg.LLM.GatewayURL = "http://" + gw.Listener.Addr().String() + "/?token=probe-secret"

		res, err := build()

		Expect(res).To(BeNil())
		Expect(err).To(MatchError(ContainSubstring("cannot reach LLM gateway")))
		Expect(err).To(wrapsError[syscall.Errno]())
		Expect(err.Error()).NotTo(ContainSubstring("probe-secret"))
	})

	DescribeTable("refuses to start with an actionable error and releases what it opened when the gateway is unreachable",
		func(setup func() string, cause types.GomegaMatcher) {
			gatewayURL := setup()
			cfg.LLM.GatewayURL = gatewayURL

			res, err := build()

			Expect(res).To(BeNil())
			Expect(err).To(HaveOccurred())
			Expect(err).To(cause)
			Expect(err.Error()).To(SatisfyAll(
				ContainSubstring("cannot reach LLM gateway "+gatewayURL),
				ContainSubstring("every LLM call would fail"),
				ContainSubstring("compose.gateway-"),
				ContainSubstring("certificate names the gateway host"),
				ContainSubstring("deploy/README.md"),
				ContainSubstring("llm.gateway_url"),
			))
			Eventually(natsSrv.NumClients).Should(BeZero(), "the NATS client must be closed")
		},
		Entry("an unresolvable host",
			func() string { return "http://gateway.invalid:4000" },
			wrapsError[*net.DNSError]()),
		Entry("a refused connection",
			func() string {
				gw := httptest.NewServer(http.NotFoundHandler())
				gw.Close()
				return gw.URL
			},
			MatchError(syscall.ECONNREFUSED)),
		Entry("a certificate that doesn't name the requested host",
			func() string {
				bundle, err := pkigen.GenerateDevPKI(pkigen.WithDNSNames("gateway.invalid"), pkigen.WithIPs())
				Expect(err).NotTo(HaveOccurred())
				caPath := filepath.Join(GinkgoT().TempDir(), "ca.pem")
				Expect(os.WriteFile(caPath, bundle.CA.CertPEM, 0o600)).To(Succeed())
				cfg.TLS = config.TLSConfig{Mode: "server-only", CA: caPath}
				cert, err := tls.X509KeyPair(bundle.Server.CertPEM, bundle.Server.KeyPEM)
				Expect(err).NotTo(HaveOccurred())
				return startTLSGateway(cert, tls.NoClientCert, http.NotFoundHandler())
			},
			wrapsError[x509.HostnameError]()),
		Entry("a certificate from an untrusted CA",
			func() string {
				other, err := testcerts.Generate()
				Expect(err).NotTo(HaveOccurred())
				cfg.TLS = config.TLSConfig{Mode: "server-only", CA: testTLSWiringCAPath}
				cert, err := tls.X509KeyPair(other.ServerCert, other.ServerKey)
				Expect(err).NotTo(HaveOccurred())
				return startTLSGateway(cert, tls.NoClientCert, http.NotFoundHandler())
			},
			wrapsError[x509.UnknownAuthorityError]()),
		Entry("a gateway requiring a client certificate when none is configured",
			func() string {
				cfg.TLS = config.TLSConfig{Mode: "server-only", CA: testTLSWiringCAPath}
				return startTLSGateway(suiteServerCert(), tls.RequireAndVerifyClientCert, http.NotFoundHandler())
			},
			// Go exports no TLS alert type for TCP connections, so the text is all there is to match.
			MatchError(ContainSubstring("certificate required"))),
		Entry("a gateway that never answers within llm.timeout",
			func() string {
				cfg.LLM.Timeout = 1
				release := make(chan struct{})
				gw := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
					select {
					case <-r.Context().Done():
					case <-release:
					}
				}))
				DeferCleanup(gw.Close)
				DeferCleanup(func() { close(release) })
				return gw.URL
			},
			WithTransform(func(err error) bool {
				var netErr net.Error
				return errors.As(err, &netErr) && netErr.Timeout()
			}, BeTrue())),
	)
})
