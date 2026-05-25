package testcerts_test

import (
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/testcerts"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
)

func TestTestCertsBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "TestCerts BDD Suite")
}

// Redirect slog output to GinkgoWriter so log noise only appears on failure.
var _ = BeforeSuite(func() { DeferCleanup(testspecs.RedirectLogsToGinkgo()) })

var _ = Describe("TestCerts PKI Generation", Ordered, func() {

	BeforeAll(func() {
		testspecs.LogTestProgress("Starting TestCerts BDD test suite")
	})

	AfterAll(func() {
		testspecs.LogTestProgress("TestCerts BDD test suite completed")
	})

	// =================================================================
	// LEVEL 1: CERTIFICATE GENERATION
	// =================================================================

	Describe("Generate", func() {
		var pki *testcerts.PKI

		BeforeEach(func() {
			var err error
			pki, err = testcerts.Generate()
			Expect(err).NotTo(HaveOccurred())
		})

		Context("when producing PEM-encoded output", func() {
			It("returns non-empty, valid PEM for all six fields", func() {
				fields := map[string][]byte{
					"CACert":     pki.CACert,
					"CAKey":      pki.CAKey,
					"ServerCert": pki.ServerCert,
					"ServerKey":  pki.ServerKey,
					"ClientCert": pki.ClientCert,
					"ClientKey":  pki.ClientKey,
				}
				for name, data := range fields {
					By("checking " + name + " is non-empty PEM")
					Expect(data).NotTo(BeEmpty(), "%s should not be empty", name)
					block, _ := pem.Decode(data)
					Expect(block).NotTo(BeNil(), "%s should be valid PEM", name)
				}
			})
		})

		Context("when inspecting the CA certificate", func() {
			It("is marked as a certificate authority with the correct CN", func() {
				By("parsing the CA certificate from PEM")
				caBlock, _ := pem.Decode(pki.CACert)
				Expect(caBlock).NotTo(BeNil())
				caCert, err := x509.ParseCertificate(caBlock.Bytes)
				Expect(err).NotTo(HaveOccurred())

				By("verifying IsCA is true")
				Expect(caCert.IsCA).To(BeTrue())

				By("verifying the CommonName")
				Expect(caCert.Subject.CommonName).To(Equal("CrossCodex Test CA"))
			})
		})

		Context("when inspecting the server certificate", func() {
			It("is a leaf certificate with localhost SANs", func() {
				By("parsing the server certificate from PEM")
				srvBlock, _ := pem.Decode(pki.ServerCert)
				Expect(srvBlock).NotTo(BeNil())
				srvCert, err := x509.ParseCertificate(srvBlock.Bytes)
				Expect(err).NotTo(HaveOccurred())

				By("verifying it is not a CA")
				Expect(srvCert.IsCA).To(BeFalse())

				By("verifying localhost is in DNSNames")
				Expect(srvCert.DNSNames).To(ContainElement("localhost"))

				By("verifying 127.0.0.1 is in IPAddresses")
				found := false
				for _, ip := range srvCert.IPAddresses {
					if ip.Equal(net.IPv4(127, 0, 0, 1)) {
						found = true
						break
					}
				}
				Expect(found).To(BeTrue(), "server cert should contain IP SAN 127.0.0.1")
			})

			It("uses an ECDSA P-256 private key", func() {
				By("parsing the server private key from PEM")
				srvKeyBlock, _ := pem.Decode(pki.ServerKey)
				Expect(srvKeyBlock).NotTo(BeNil())
				srvKey, err := x509.ParseECPrivateKey(srvKeyBlock.Bytes)
				Expect(err).NotTo(HaveOccurred())

				By("verifying the curve is P-256 (256-bit)")
				Expect(srvKey.Curve.Params().BitSize).To(Equal(256))
			})
		})

		Context("when inspecting the client certificate", func() {
			It("is a leaf certificate with the correct CN", func() {
				By("parsing the client certificate from PEM")
				cliBlock, _ := pem.Decode(pki.ClientCert)
				Expect(cliBlock).NotTo(BeNil())
				cliCert, err := x509.ParseCertificate(cliBlock.Bytes)
				Expect(err).NotTo(HaveOccurred())

				By("verifying it is not a CA")
				Expect(cliCert.IsCA).To(BeFalse())

				By("verifying the CommonName")
				Expect(cliCert.Subject.CommonName).To(Equal("crosscodex-test-client"))
			})
		})

		Context("when verifying CA trust chain", func() {
			It("signs both server and client certificates with the CA", func() {
				By("parsing the CA certificate")
				caBlock, _ := pem.Decode(pki.CACert)
				Expect(caBlock).NotTo(BeNil())
				caCert, err := x509.ParseCertificate(caBlock.Bytes)
				Expect(err).NotTo(HaveOccurred())

				pool := x509.NewCertPool()
				pool.AddCert(caCert)

				By("verifying the server certificate against the CA")
				srvBlock, _ := pem.Decode(pki.ServerCert)
				Expect(srvBlock).NotTo(BeNil())
				srvCert, err := x509.ParseCertificate(srvBlock.Bytes)
				Expect(err).NotTo(HaveOccurred())

				_, err = srvCert.Verify(x509.VerifyOptions{Roots: pool})
				Expect(err).NotTo(HaveOccurred(), "server cert should be signed by the CA")

				By("verifying the client certificate against the CA")
				cliBlock, _ := pem.Decode(pki.ClientCert)
				Expect(cliBlock).NotTo(BeNil())
				cliCert, err := x509.ParseCertificate(cliBlock.Bytes)
				Expect(err).NotTo(HaveOccurred())

				_, err = cliCert.Verify(x509.VerifyOptions{
					Roots:     pool,
					KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
				})
				Expect(err).NotTo(HaveOccurred(), "client cert should be signed by the CA")
			})
		})
	})

	// =================================================================
	// LEVEL 2: FILE PERSISTENCE
	// =================================================================

	Describe("WriteToDir", func() {
		var pki *testcerts.PKI

		BeforeEach(func() {
			var err error
			pki, err = testcerts.Generate()
			Expect(err).NotTo(HaveOccurred())
		})

		Context("when writing to an existing directory", func() {
			It("creates all six PEM files with correct permissions", func() {
				dir := GinkgoT().TempDir()
				Expect(pki.WriteToDir(dir)).To(Succeed())

				wantPerms := map[string]os.FileMode{
					"ca.pem":         0644,
					"ca-key.pem":     0600,
					"server.pem":     0644,
					"server-key.pem": 0600,
					"client.pem":     0644,
					"client-key.pem": 0600,
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
		})

		Context("when the target directory does not exist", func() {
			It("creates nested directories and writes files", func() {
				dir := filepath.Join(GinkgoT().TempDir(), "nested", "certs")

				By("writing to a non-existent nested path")
				Expect(pki.WriteToDir(dir)).To(Succeed())

				By("verifying the CA cert was created in the nested directory")
				_, err := os.Stat(filepath.Join(dir, "ca.pem"))
				Expect(err).NotTo(HaveOccurred())
			})
		})
	})

	// =================================================================
	// LEVEL 3: MUTUAL TLS HANDSHAKE
	// =================================================================

	Describe("TLS Handshake", func() {
		It("completes a mutual TLS handshake using the generated PKI", func() {
			By("generating a fresh PKI")
			pki, err := testcerts.Generate()
			Expect(err).NotTo(HaveOccurred())

			By("building the CA trust pool")
			caPool := x509.NewCertPool()
			ok := caPool.AppendCertsFromPEM(pki.CACert)
			Expect(ok).To(BeTrue(), "CA cert should be added to pool")

			By("loading the server TLS certificate")
			srvCert, err := tls.X509KeyPair(pki.ServerCert, pki.ServerKey)
			Expect(err).NotTo(HaveOccurred())

			serverTLS := &tls.Config{
				Certificates: []tls.Certificate{srvCert},
				ClientCAs:    caPool,
				ClientAuth:   tls.RequireAndVerifyClientCert,
				MinVersion:   tls.VersionTLS12,
			}

			By("starting a TLS listener on localhost")
			ln, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
			Expect(err).NotTo(HaveOccurred())
			defer ln.Close()

			By("accepting a connection on the server side")
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
			cliCert, err := tls.X509KeyPair(pki.ClientCert, pki.ClientKey)
			Expect(err).NotTo(HaveOccurred())

			clientTLS := &tls.Config{
				Certificates: []tls.Certificate{cliCert},
				RootCAs:      caPool,
				MinVersion:   tls.VersionTLS12,
			}

			By("dialing the server with mTLS")
			conn, err := tls.Dial("tcp", ln.Addr().String(), clientTLS)
			Expect(err).NotTo(HaveOccurred())
			defer conn.Close()

			By("waiting for the server-side handshake to complete")
			Expect(<-serverDone).NotTo(HaveOccurred())

			By("verifying the server presented an ECDSA certificate")
			state := conn.ConnectionState()
			Expect(state.PeerCertificates).NotTo(BeEmpty(), "server should present peer certificates")
			_, isECDSA := state.PeerCertificates[0].PublicKey.(*ecdsa.PublicKey)
			Expect(isECDSA).To(BeTrue(), "server cert should use ECDSA")
		})
	})
})
