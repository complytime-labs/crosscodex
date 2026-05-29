//go:build integration_authn

package authn_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/authn"
)

// No separate TestXxx entry point or BeforeSuite here — this file registers
// its Describe block with the suite runner in authn_bdd_test.go when the
// integration_authn build tag is active.

var _ = Describe("Cross-Stack mTLS Interop (nginx + OpenSSL)", Ordered, func() {
	var (
		proxyURL string
		caFile   string
		certFile string
		keyFile  string
		caPool   *x509.CertPool
	)

	BeforeAll(func() {
		testspecs.LogTestProgress("Starting Authn container integration suite")

		proxyURL = os.Getenv("TEST_AUTHN_URL")
		if proxyURL == "" {
			Skip("TEST_AUTHN_URL not set; skipping container tests")
		}
		caFile = os.Getenv("TEST_AUTHN_CA")
		certFile = os.Getenv("TEST_AUTHN_CERT")
		keyFile = os.Getenv("TEST_AUTHN_KEY")

		Expect(caFile).NotTo(BeEmpty(), "TEST_AUTHN_CA must be set")
		Expect(certFile).NotTo(BeEmpty(), "TEST_AUTHN_CERT must be set")
		Expect(keyFile).NotTo(BeEmpty(), "TEST_AUTHN_KEY must be set")

		// Load CA pool
		caPEM, err := os.ReadFile(caFile)
		Expect(err).NotTo(HaveOccurred())
		caPool = x509.NewCertPool()
		Expect(caPool.AppendCertsFromPEM(caPEM)).To(BeTrue())
	})

	AfterAll(func() {
		testspecs.LogTestProgress("Authn container integration suite completed")
	})

	buildClient := func(certPath, keyPath string) *http.Client {
		cert, err := tls.LoadX509KeyPair(certPath, keyPath)
		Expect(err).NotTo(HaveOccurred())

		return &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{ // DevSkim: ignore DS440000,DS440001,DS112852 - test client; MinVersion floor is intentional, MaxVersion omitted so Go picks highest
					Certificates: []tls.Certificate{cert},
					RootCAs:      caPool,
					MinVersion:   tls.VersionTLS12, // DevSkim: ignore DS440001,DS112852 - TLS 1.2 floor is correct for mTLS interop testing
				},
			},
		}
	}

	Context("TLS handshake with nginx OpenSSL stack", func() {
		It("completes mTLS handshake and returns 200", func() {
			client := buildClient(certFile, keyFile)
			resp, err := client.Get(proxyURL + "/")
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			Expect(err).NotTo(HaveOccurred())

			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			Expect(string(body)).To(ContainSubstring("mTLS OK"))
		})

		It("receives client certificate details in response headers", func() {
			client := buildClient(certFile, keyFile)
			resp, err := client.Get(proxyURL + "/")
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()

			// nginx sets X-Client-DN and X-Client-Verify
			clientDN := resp.Header.Get("X-Client-DN")
			clientVerify := resp.Header.Get("X-Client-Verify")

			Expect(clientDN).NotTo(BeEmpty(), "nginx should forward client DN")
			Expect(clientVerify).To(Equal("SUCCESS"), "nginx should verify client cert")
		})
	})

	Context("authenticator with cross-stack ConnectionState", func() {
		It("resolves identity from real cross-stack TLS handshake", func() {
			// Build TLS config for raw connection
			clientCert, err := tls.LoadX509KeyPair(certFile, keyFile)
			Expect(err).NotTo(HaveOccurred())

			tlsCfg := &tls.Config{ // DevSkim: ignore DS440000,DS440001,DS112852 - test client; MinVersion floor is intentional, MaxVersion omitted so Go picks highest
				Certificates: []tls.Certificate{clientCert},
				RootCAs:      caPool,
				MinVersion:   tls.VersionTLS12, // DevSkim: ignore DS440001,DS112852 - TLS 1.2 floor is correct for mTLS interop testing
			}

			// Strip scheme from URL for tls.Dial
			host := proxyURL[len("https://"):]
			conn, err := tls.Dial("tcp", host, tlsCfg)
			Expect(err).NotTo(HaveOccurred())
			defer conn.Close()

			// Get the ConnectionState from the Go side of the handshake
			connState := conn.ConnectionState()

			// Use the authenticator to resolve identity
			x509Auth, err := authn.NewX509Authenticator(authn.X509Config{
				SingleTenant:  true,
				DefaultTenant: "container-test",
			})
			Expect(err).NotTo(HaveOccurred())

			identity, err := x509Auth.Authenticate(context.Background(), &authn.Request{
				TLSState: &connState,
				ClientIP: "127.0.0.1",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(identity.TenantID).To(Equal("container-test"))
			Expect(identity.Roles).To(ConsistOf("admin"))
			Expect(identity.Method).To(Equal(authn.AuthMethodMTLS))

			fmt.Fprintf(GinkgoWriter, "Cross-stack identity: Subject=%s, Tenant=%s\n",
				identity.Subject, identity.TenantID)
		})
	})

	Context("rejection scenarios", func() {
		It("rejects connections without client certificate", func() {
			client := &http.Client{
				Transport: &http.Transport{
					TLSClientConfig: &tls.Config{ // DevSkim: ignore DS440000,DS112852 - test client; MinVersion floor is intentional, MaxVersion omitted so Go picks highest
						RootCAs:    caPool,
						MinVersion: tls.VersionTLS12, // DevSkim: ignore DS112852 - TLS 1.2 floor is correct for mTLS interop testing
					},
				},
			}
			resp, err := client.Get(proxyURL + "/")
			// With TLS 1.2 the handshake fails (err != nil) because the // DevSkim: ignore DS440001 - prose description of protocol behavior, not a version selection
			// server demands a client cert.  With TLS 1.3 the handshake // DevSkim: ignore DS440001
			// succeeds (post-handshake auth) but nginx returns HTTP 400
			// ("No required SSL certificate was sent").  Either outcome
			// proves the server rejects unauthenticated clients.
			if err == nil {
				defer resp.Body.Close()
				Expect(resp.StatusCode).To(Equal(http.StatusBadRequest),
					"expected HTTP 400 when no client cert is presented over TLS 1.3") // DevSkim: ignore DS440001 - version reference in test assertion message, not a protocol selection
			}
			// err != nil is also acceptable (TLS 1.2 rejection) // DevSkim: ignore DS440001
		})
	})
})
