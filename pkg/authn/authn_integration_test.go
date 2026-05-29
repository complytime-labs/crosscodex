//go:build integration_authn

package authn_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/tlsconfig"
	"github.com/complytime-labs/crosscodex/pkg/tlsconfig/pki"
)

// ---------------------------------------------------------------------------
// Integration tests: real Go-to-Go TLS handshakes
// ---------------------------------------------------------------------------

var _ = Describe("Authn Integration: Real TLS Handshakes", Ordered, func() {

	var (
		// PKI bundle written to disk for tlsconfig.BuildTLSConfig
		certDir string
		bundle  *pki.PKIBundle

		// Client certificates signed by the same CA
		acmeClient    *pki.CertKeyPair
		partnerClient *pki.CertKeyPair
		unknownClient *pki.CertKeyPair
	)

	// performTLSHandshake starts a TLS listener, dials it with the given
	// client certificate, completes the handshake, and returns the
	// server-side ConnectionState captured from the accepted connection.
	performTLSHandshake := func(serverCfg *tls.Config, clientPair *pki.CertKeyPair) *tls.ConnectionState {
		GinkgoHelper()

		// Parse the client certificate PEM into a tls.Certificate.
		clientCert, err := tls.X509KeyPair(clientPair.CertPEM, clientPair.KeyPEM)
		Expect(err).NotTo(HaveOccurred(), "failed to parse client cert PEM")

		// Build a CA pool from the bundle CA for the client to verify
		// the server certificate.
		caPool := x509.NewCertPool()
		ok := caPool.AppendCertsFromPEM(bundle.CA.CertPEM)
		Expect(ok).To(BeTrue(), "failed to append CA cert to pool")

		clientCfg := &tls.Config{
			Certificates: []tls.Certificate{clientCert},
			RootCAs:      caPool,
			MinVersion:   tls.VersionTLS12, // DevSkim: ignore DS440001,DS112852
			ServerName:   "localhost",
		}

		// Listen on a random loopback port.
		ln, err := tls.Listen("tcp", "127.0.0.1:0", serverCfg)
		Expect(err).NotTo(HaveOccurred(), "failed to start TLS listener")
		defer ln.Close()

		// Accept and handshake in a goroutine; capture the ConnectionState.
		var (
			connState *tls.ConnectionState
			acceptErr error
			wg        sync.WaitGroup
		)
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, aErr := ln.Accept()
			if aErr != nil {
				acceptErr = aErr
				return
			}
			defer conn.Close()

			tlsConn, ok := conn.(*tls.Conn)
			if !ok {
				acceptErr = errors.New("accepted connection is not *tls.Conn")
				return
			}
			if hErr := tlsConn.Handshake(); hErr != nil {
				acceptErr = hErr
				return
			}
			cs := tlsConn.ConnectionState()
			connState = &cs
		}()

		// Client dials, handshakes, then closes.
		clientConn, err := tls.Dial("tcp", ln.Addr().String(), clientCfg)
		Expect(err).NotTo(HaveOccurred(), "client TLS dial failed")
		clientConn.Close()

		wg.Wait()
		Expect(acceptErr).NotTo(HaveOccurred(), "server-side accept/handshake failed")
		Expect(connState).NotTo(BeNil(), "server ConnectionState was not captured")
		return connState
	}

	// buildServerTLSConfig uses tlsconfig.BuildTLSConfig with the on-disk
	// PKI bundle in mutual TLS mode.
	buildServerTLSConfig := func() *tls.Config {
		GinkgoHelper()

		cfg := config.TLSConfig{
			Mode: "mutual",
			CA:   filepath.Join(certDir, "ca.pem"),
			Cert: filepath.Join(certDir, "server.pem"),
			Key:  filepath.Join(certDir, "server-key.pem"),
		}
		serverTLS, err := tlsconfig.BuildTLSConfig(cfg, "")
		Expect(err).NotTo(HaveOccurred(), "failed to build server TLS config")
		Expect(serverTLS).NotTo(BeNil())
		return serverTLS
	}

	BeforeAll(func() {
		var err error

		// Generate a full PKI bundle and write to a temp directory so
		// tlsconfig.BuildTLSConfig can load from disk.
		certDir, err = os.MkdirTemp("", "authn-integration-*")
		Expect(err).NotTo(HaveOccurred())

		bundle, err = pki.GenerateDevPKI(
			pki.WithOrganization("Integration Test CA"),
			pki.WithDNSNames("localhost"),
			pki.WithIPs(net.IPv4(127, 0, 0, 1), net.IPv6loopback),
			pki.WithOutputDir(certDir),
		)
		Expect(err).NotTo(HaveOccurred())

		// Generate three client certs signed by the same CA.
		acmeClient, err = pki.GenerateCert(bundle.CA,
			pki.WithOrganization("Acme Corp"),
			pki.WithOrgUnit("Engineering"),
			pki.WithDNSNames("acme-client"),
			pki.WithExtKeyUsage(x509.ExtKeyUsageClientAuth),
		)
		Expect(err).NotTo(HaveOccurred())

		partnerClient, err = pki.GenerateCert(bundle.CA,
			pki.WithOrganization("Partner Inc"),
			pki.WithDNSNames("partner-client"),
			pki.WithEmailAddresses("ops@partner.com"),
			pki.WithExtKeyUsage(x509.ExtKeyUsageClientAuth),
		)
		Expect(err).NotTo(HaveOccurred())

		unknownClient, err = pki.GenerateCert(bundle.CA,
			pki.WithOrganization("Unknown Org"),
			pki.WithDNSNames("unknown-client"),
			pki.WithExtKeyUsage(x509.ExtKeyUsageClientAuth),
		)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterAll(func() {
		if certDir != "" {
			os.RemoveAll(certDir)
		}
	})

	// =====================================================================
	// Single-tenant mode
	// =====================================================================

	Context("single-tenant mode", func() {
		It("authenticates any valid client cert", func() {
			serverCfg := buildServerTLSConfig()
			connState := performTLSHandshake(serverCfg, acmeClient)

			auth, err := authn.NewX509Authenticator(authn.X509Config{
				SingleTenant:  true,
				DefaultTenant: "default-tenant",
			})
			Expect(err).NotTo(HaveOccurred())

			identity, err := auth.Authenticate(context.Background(), &authn.Request{
				TLSState: connState,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(identity.TenantID).To(Equal("default-tenant"))
			Expect(identity.Roles).To(Equal([]string{"admin"}))
			Expect(identity.Subject).To(Equal("acme-client"))
			Expect(identity.Method).To(Equal(authn.AuthMethodMTLS))
		})
	})

	// =====================================================================
	// Multi-tenant mode
	// =====================================================================

	Context("multi-tenant mode", Ordered, func() {
		var auth *authn.X509Authenticator

		BeforeAll(func() {
			var err error
			auth, err = authn.NewX509Authenticator(authn.X509Config{
				SingleTenant: false,
				Mappings: []authn.X509Mapping{
					{
						Match:  authn.X509Match{Organization: "Acme*", OrgUnit: "Engineering"},
						Tenant: "acme-engineering",
						Roles:  []string{"admin"},
					},
					{
						Match:  authn.X509Match{SANEmail: "*@partner.com"},
						Tenant: "partner-org",
						Roles:  []string{"reader"},
					},
				},
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("resolves Acme Engineering identity", func() {
			serverCfg := buildServerTLSConfig()
			connState := performTLSHandshake(serverCfg, acmeClient)

			identity, err := auth.Authenticate(context.Background(), &authn.Request{
				TLSState: connState,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(identity.TenantID).To(Equal("acme-engineering"))
			Expect(identity.Roles).To(Equal([]string{"admin"}))
			Expect(identity.Subject).To(Equal("acme-client"))
		})

		It("resolves Partner identity via email SAN", func() {
			serverCfg := buildServerTLSConfig()
			connState := performTLSHandshake(serverCfg, partnerClient)

			identity, err := auth.Authenticate(context.Background(), &authn.Request{
				TLSState: connState,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(identity.TenantID).To(Equal("partner-org"))
			Expect(identity.Roles).To(Equal([]string{"reader"}))
			Expect(identity.Subject).To(Equal("partner-client"))
		})

		It("rejects unknown organization", func() {
			serverCfg := buildServerTLSConfig()
			connState := performTLSHandshake(serverCfg, unknownClient)

			_, err := auth.Authenticate(context.Background(), &authn.Request{
				TLSState: connState,
			})
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, authn.ErrAuthenticationFailed)).To(BeTrue())
		})
	})

	// =====================================================================
	// Full Registry integration
	// =====================================================================

	Context("full Registry integration", func() {
		It("dispatches through GSSAPI+SAML stubs to X509 with real handshake", func() {
			serverCfg := buildServerTLSConfig()
			connState := performTLSHandshake(serverCfg, acmeClient)

			x509Auth, err := authn.NewX509Authenticator(authn.X509Config{
				SingleTenant:  true,
				DefaultTenant: "registry-tenant",
			})
			Expect(err).NotTo(HaveOccurred())

			recorder := &recordingEmitter{}
			registry := authn.NewRegistry(recorder,
				authn.NewGSSAPIAuthenticator(),
				authn.NewSAMLAuthenticator(),
				x509Auth,
			)

			identity, err := registry.Authenticate(context.Background(), &authn.Request{
				TLSState:  connState,
				ClientIP:  "127.0.0.1",
				SessionID: "integration-session-001",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(identity.TenantID).To(Equal("registry-tenant"))
			Expect(identity.Method).To(Equal(authn.AuthMethodMTLS))

			// Verify audit event was emitted
			event := recorder.lastEvent()
			Expect(event).NotTo(BeNil())
			Expect(event.Success).To(BeTrue())
			Expect(event.Principal).To(Equal("acme-client"))
			Expect(event.TenantID).To(Equal("registry-tenant"))
			Expect(event.Method).To(Equal(authn.AuthMethodMTLS))
			Expect(event.ClientIP).To(Equal("127.0.0.1"))
			Expect(event.SessionID).To(Equal("integration-session-001"))
		})
	})
})
