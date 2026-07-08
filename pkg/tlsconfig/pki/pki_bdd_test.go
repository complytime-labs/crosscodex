package pki_test

import (
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/tlsconfig/pki"
)

func TestPKIBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "PKI BDD Suite")
}

var _ = BeforeSuite(func() { DeferCleanup(testspecs.RedirectLogsToGinkgo()) })

var _ = Describe("PKI Certificate Generation", Ordered, func() {

	BeforeAll(func() {
		testspecs.LogTestProgress("Starting PKI BDD test suite")
	})

	AfterAll(func() {
		testspecs.LogTestProgress("PKI BDD test suite completed")
	})

	// =================================================================
	// LEVEL 1: CA GENERATION
	// =================================================================

	Describe("GenerateCA", func() {
		Context("with default options", func() {
			It("produces a valid self-signed CA certificate", func() {
				By("generating a CA with defaults")
				ca, err := pki.GenerateCA()
				Expect(err).NotTo(HaveOccurred())
				Expect(ca).NotTo(BeNil())

				By("verifying CertPEM is valid PEM")
				Expect(ca.CertPEM).NotTo(BeEmpty())
				block, _ := pem.Decode(ca.CertPEM)
				Expect(block).NotTo(BeNil())

				By("verifying KeyPEM is valid PEM")
				Expect(ca.KeyPEM).NotTo(BeEmpty())
				keyBlock, _ := pem.Decode(ca.KeyPEM)
				Expect(keyBlock).NotTo(BeNil())

				By("verifying the parsed certificate is a CA")
				Expect(ca.Cert).NotTo(BeNil())
				Expect(ca.Cert.IsCA).To(BeTrue())

				By("verifying the default organization")
				Expect(ca.Cert.Subject.Organization).To(ContainElement("CrossCodex Dev"))

				By("verifying the CN contains CA")
				Expect(ca.Cert.Subject.CommonName).To(ContainSubstring("CA"))

				By("verifying ECDSA P-256 key")
				pubKey, ok := ca.Cert.PublicKey.(*ecdsa.PublicKey)
				Expect(ok).To(BeTrue())
				Expect(pubKey.Curve.Params().BitSize).To(Equal(256))
			})
		})

		Context("with custom options", func() {
			It("applies WithOrganization and WithValidDuration", func() {
				ca, err := pki.GenerateCA(
					pki.WithOrganization("Test Org"),
					pki.WithValidDuration(24*time.Hour),
				)
				Expect(err).NotTo(HaveOccurred())

				By("verifying custom organization")
				Expect(ca.Cert.Subject.Organization).To(ContainElement("Test Org"))

				By("verifying validity duration is approximately 24 hours")
				duration := ca.Cert.NotAfter.Sub(ca.Cert.NotBefore)
				Expect(duration).To(BeNumerically("~", 24*time.Hour, time.Minute))
			})
		})
	})

	// =================================================================
	// LEVEL 2: LEAF CERTIFICATE GENERATION
	// =================================================================

	Describe("GenerateCert", func() {
		var ca *pki.CertKeyPair

		BeforeEach(func() {
			var err error
			ca, err = pki.GenerateCA()
			Expect(err).NotTo(HaveOccurred())
		})

		Context("with default options", func() {
			It("produces a leaf certificate signed by the CA", func() {
				By("generating a leaf cert")
				leaf, err := pki.GenerateCert(ca)
				Expect(err).NotTo(HaveOccurred())
				Expect(leaf).NotTo(BeNil())

				By("verifying it is not a CA")
				Expect(leaf.Cert.IsCA).To(BeFalse())

				By("verifying localhost is in DNSNames")
				Expect(leaf.Cert.DNSNames).To(ContainElement("localhost"))

				By("verifying 127.0.0.1 is in IPAddresses")
				found := false
				for _, ip := range leaf.Cert.IPAddresses {
					if ip.Equal(net.IPv4(127, 0, 0, 1)) {
						found = true
						break
					}
				}
				Expect(found).To(BeTrue(), "leaf cert should contain IP SAN 127.0.0.1")

				By("verifying the cert validates against the CA")
				pool := x509.NewCertPool()
				pool.AddCert(ca.Cert)
				_, err = leaf.Cert.Verify(x509.VerifyOptions{
					Roots:     pool,
					KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
				})
				Expect(err).NotTo(HaveOccurred())
			})
		})

		Context("with custom SANs", func() {
			It("applies WithDNSNames and WithIPs", func() {
				leaf, err := pki.GenerateCert(ca,
					pki.WithDNSNames("myservice.local", "api.internal"),
					pki.WithIPs(net.ParseIP("10.0.0.1")),
				)
				Expect(err).NotTo(HaveOccurred())

				By("verifying custom DNS names")
				Expect(leaf.Cert.DNSNames).To(ConsistOf("myservice.local", "api.internal"))

				By("verifying custom IP")
				found := false
				for _, ip := range leaf.Cert.IPAddresses {
					if ip.Equal(net.ParseIP("10.0.0.1")) {
						found = true
						break
					}
				}
				Expect(found).To(BeTrue())
			})
		})

		Context("with nil CA", func() {
			It("returns an error", func() {
				_, err := pki.GenerateCert(nil)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("CA certificate"))
			})
		})

		Context("with partially constructed CA", func() {
			It("returns an error when CA.Cert is nil", func() {
				_, err := pki.GenerateCert(&pki.CertKeyPair{
					CertPEM: nil,
					KeyPEM:  []byte("fake-key"),
					Cert:    nil,
				})
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("CA certificate"))
			})

			It("returns an error when CA.KeyPEM is nil", func() {
				ca, err := pki.GenerateCA()
				Expect(err).NotTo(HaveOccurred())

				_, err = pki.GenerateCert(&pki.CertKeyPair{
					CertPEM: ca.CertPEM,
					KeyPEM:  nil,
					Cert:    ca.Cert,
				})
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("CA certificate"))
			})
		})
	})

	// =================================================================
	// LEVEL 3: FULL PKI BUNDLE
	// =================================================================

	Describe("GenerateDevPKI", func() {
		Context("with in-memory generation", func() {
			It("produces CA, server, and client certificates", func() {
				bundle, err := pki.GenerateDevPKI()
				Expect(err).NotTo(HaveOccurred())

				By("verifying all three certs are present")
				Expect(bundle.CA).NotTo(BeNil())
				Expect(bundle.Server).NotTo(BeNil())
				Expect(bundle.Client).NotTo(BeNil())

				By("verifying CA is a CA")
				Expect(bundle.CA.Cert.IsCA).To(BeTrue())

				By("verifying server is not a CA")
				Expect(bundle.Server.Cert.IsCA).To(BeFalse())

				By("verifying client is not a CA")
				Expect(bundle.Client.Cert.IsCA).To(BeFalse())

				By("verifying Dir is empty (in-memory)")
				Expect(bundle.Dir).To(BeEmpty())

				By("verifying server cert validates against CA")
				pool := x509.NewCertPool()
				pool.AddCert(bundle.CA.Cert)
				_, err = bundle.Server.Cert.Verify(x509.VerifyOptions{
					Roots:     pool,
					KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
				})
				Expect(err).NotTo(HaveOccurred())

				By("verifying client cert validates against CA")
				_, err = bundle.Client.Cert.Verify(x509.VerifyOptions{
					Roots:     pool,
					KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
				})
				Expect(err).NotTo(HaveOccurred())
			})
		})

		Context("with output directory", func() {
			It("writes PEM files to disk with correct permissions", func() {
				dir := GinkgoT().TempDir()

				bundle, err := pki.GenerateDevPKI(pki.WithOutputDir(dir))
				Expect(err).NotTo(HaveOccurred())
				Expect(bundle.Dir).To(Equal(dir))

				wantPerms := map[string]os.FileMode{
					"ca.pem":         0o644,
					"ca-key.pem":     0o600,
					"server.pem":     0o644,
					"server-key.pem": 0o600,
					"client.pem":     0o644,
					"client-key.pem": 0o600,
				}

				for name, wantPerm := range wantPerms {
					By("checking file " + name)
					path := filepath.Join(dir, name)
					info, err := os.Stat(path)
					Expect(err).NotTo(HaveOccurred(), "file %s should exist", name)
					Expect(info.Mode().Perm()).To(Equal(wantPerm),
						"file %s should have permissions %o", name, wantPerm)
				}
			})

			It("produces files loadable by tls.LoadX509KeyPair", func() {
				dir := GinkgoT().TempDir()

				_, err := pki.GenerateDevPKI(pki.WithOutputDir(dir))
				Expect(err).NotTo(HaveOccurred())

				By("loading server cert/key pair")
				_, err = tls.LoadX509KeyPair(
					filepath.Join(dir, "server.pem"),
					filepath.Join(dir, "server-key.pem"),
				)
				Expect(err).NotTo(HaveOccurred())

				By("loading client cert/key pair")
				_, err = tls.LoadX509KeyPair(
					filepath.Join(dir, "client.pem"),
					filepath.Join(dir, "client-key.pem"),
				)
				Expect(err).NotTo(HaveOccurred())
			})
		})

		Context("with nested non-existent directory", func() {
			It("creates the directory tree", func() {
				dir := filepath.Join(GinkgoT().TempDir(), "nested", "certs")

				bundle, err := pki.GenerateDevPKI(pki.WithOutputDir(dir))
				Expect(err).NotTo(HaveOccurred())
				Expect(bundle.Dir).To(Equal(dir))

				_, err = os.Stat(filepath.Join(dir, "ca.pem"))
				Expect(err).NotTo(HaveOccurred())
			})
		})
	})

	// =================================================================
	// LEVEL 4: MUTUAL TLS HANDSHAKE
	// =================================================================

	Describe("TLS Handshake", func() {
		It("completes a mutual TLS handshake using GenerateDevPKI", func() {
			By("generating a fresh PKI bundle")
			bundle, err := pki.GenerateDevPKI()
			Expect(err).NotTo(HaveOccurred())

			By("building the CA trust pool")
			caPool := x509.NewCertPool()
			ok := caPool.AppendCertsFromPEM(bundle.CA.CertPEM)
			Expect(ok).To(BeTrue())

			By("loading the server TLS certificate")
			srvCert, err := tls.X509KeyPair(bundle.Server.CertPEM, bundle.Server.KeyPEM)
			Expect(err).NotTo(HaveOccurred())

			serverTLS := &tls.Config{
				Certificates: []tls.Certificate{srvCert},
				ClientCAs:    caPool,
				ClientAuth:   tls.RequireAndVerifyClientCert,
				MinVersion:   tls.VersionTLS12, // DevSkim: ignore DS440001,DS112852
			}

			By("starting a TLS listener on localhost")
			ln, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
			Expect(err).NotTo(HaveOccurred())
			defer ln.Close()

			serverDone := make(chan error, 1)
			go func() {
				conn, acceptErr := ln.Accept()
				if acceptErr != nil {
					serverDone <- acceptErr
					return
				}
				defer conn.Close()
				if tlsConn, tlsOk := conn.(*tls.Conn); tlsOk {
					serverDone <- tlsConn.Handshake()
				} else {
					serverDone <- nil
				}
			}()

			By("loading the client TLS certificate")
			cliCert, err := tls.X509KeyPair(bundle.Client.CertPEM, bundle.Client.KeyPEM)
			Expect(err).NotTo(HaveOccurred())

			clientTLS := &tls.Config{
				Certificates: []tls.Certificate{cliCert},
				RootCAs:      caPool,
				MinVersion:   tls.VersionTLS12, // DevSkim: ignore DS440001,DS112852
			}

			By("dialing the server with mTLS")
			conn, err := tls.Dial("tcp", ln.Addr().String(), clientTLS)
			Expect(err).NotTo(HaveOccurred())
			defer conn.Close()

			By("waiting for the server-side handshake to complete")
			Expect(<-serverDone).NotTo(HaveOccurred())

			By("verifying the server presented an ECDSA certificate")
			state := conn.ConnectionState()
			Expect(state.PeerCertificates).NotTo(BeEmpty())
			_, isECDSA := state.PeerCertificates[0].PublicKey.(*ecdsa.PublicKey)
			Expect(isECDSA).To(BeTrue())
		})
	})
})
