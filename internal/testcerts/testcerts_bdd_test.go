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
				Expect(cliCert.Subject.CommonName).To(Equal("client"))
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
	// LEVEL 3: CERTIFICATE VERIFICATION
	// =================================================================

	Describe("VerifyDir", func() {
		Context("when certs are freshly generated", func() {
			It("returns nil for a valid cert directory", func() {
				pki, err := testcerts.Generate()
				Expect(err).NotTo(HaveOccurred())

				dir := GinkgoT().TempDir()
				Expect(pki.WriteToDir(dir)).To(Succeed())

				Expect(testcerts.VerifyDir(dir)).To(Succeed())
			})
		})

		Context("when the CA cert is missing", func() {
			It("returns an actionable error", func() {
				pki, err := testcerts.Generate()
				Expect(err).NotTo(HaveOccurred())

				dir := GinkgoT().TempDir()
				Expect(pki.WriteToDir(dir)).To(Succeed())
				Expect(os.Remove(filepath.Join(dir, "ca.pem"))).To(Succeed())

				err = testcerts.VerifyDir(dir)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("ca.pem"))
			})
		})

		Context("when a leaf cert is corrupt", func() {
			It("returns an actionable error mentioning the file", func() {
				pki, err := testcerts.Generate()
				Expect(err).NotTo(HaveOccurred())

				dir := GinkgoT().TempDir()
				Expect(pki.WriteToDir(dir)).To(Succeed())

				By("corrupting server.pem")
				Expect(os.WriteFile(filepath.Join(dir, "server.pem"), []byte("not a cert"), 0o644)).To(Succeed())

				err = testcerts.VerifyDir(dir)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("server.pem"))
			})
		})

		Context("when server cert is signed by a different CA", func() {
			It("returns a CA chain verification error", func() {
				By("generating two independent PKIs")
				pki1, err := testcerts.Generate()
				Expect(err).NotTo(HaveOccurred())
				pki2, err := testcerts.Generate()
				Expect(err).NotTo(HaveOccurred())

				dir := GinkgoT().TempDir()
				Expect(pki1.WriteToDir(dir)).To(Succeed())

				By("replacing server cert with one from a different CA")
				Expect(os.WriteFile(filepath.Join(dir, "server.pem"), pki2.ServerCert, 0o644)).To(Succeed())

				err = testcerts.VerifyDir(dir)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("CA chain verification failed"))
			})
		})

		Context("when a key file is corrupt", func() {
			It("returns an actionable error mentioning the key file", func() {
				pki, err := testcerts.Generate()
				Expect(err).NotTo(HaveOccurred())

				dir := GinkgoT().TempDir()
				Expect(pki.WriteToDir(dir)).To(Succeed())

				By("corrupting client-key.pem")
				Expect(os.WriteFile(filepath.Join(dir, "client-key.pem"), []byte("not a key"), 0o600)).To(Succeed())

				err = testcerts.VerifyDir(dir)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("client-key.pem"))
			})
		})
	})

	// =================================================================
	// LEVEL 4: FINGERPRINTING
	// =================================================================

	Describe("Fingerprint", func() {
		Context("when computing fingerprints", func() {
			It("is deterministic for the same cert files", func() {
				pki, err := testcerts.Generate()
				Expect(err).NotTo(HaveOccurred())

				dir := GinkgoT().TempDir()
				Expect(pki.WriteToDir(dir)).To(Succeed())

				fp1, err := testcerts.ComputeFingerprint(dir)
				Expect(err).NotTo(HaveOccurred())
				Expect(fp1).NotTo(BeEmpty())

				fp2, err := testcerts.ComputeFingerprint(dir)
				Expect(err).NotTo(HaveOccurred())
				Expect(fp2).To(Equal(fp1))
			})

			It("differs for different cert files", func() {
				pki1, err := testcerts.Generate()
				Expect(err).NotTo(HaveOccurred())
				pki2, err := testcerts.Generate()
				Expect(err).NotTo(HaveOccurred())

				dir1 := GinkgoT().TempDir()
				dir2 := GinkgoT().TempDir()
				Expect(pki1.WriteToDir(dir1)).To(Succeed())
				Expect(pki2.WriteToDir(dir2)).To(Succeed())

				fp1, err := testcerts.ComputeFingerprint(dir1)
				Expect(err).NotTo(HaveOccurred())
				fp2, err := testcerts.ComputeFingerprint(dir2)
				Expect(err).NotTo(HaveOccurred())

				Expect(fp1).NotTo(Equal(fp2))
			})
		})

		Context("when writing and reading fingerprints", func() {
			It("round-trips correctly", func() {
				pki, err := testcerts.Generate()
				Expect(err).NotTo(HaveOccurred())

				dir := GinkgoT().TempDir()
				Expect(pki.WriteToDir(dir)).To(Succeed())
				Expect(testcerts.WriteFingerprint(dir)).To(Succeed())

				stored, err := testcerts.ReadFingerprint(dir)
				Expect(err).NotTo(HaveOccurred())

				computed, err := testcerts.ComputeFingerprint(dir)
				Expect(err).NotTo(HaveOccurred())

				Expect(stored).To(Equal(computed))
			})

			It("returns empty string for missing fingerprint file", func() {
				dir := GinkgoT().TempDir()

				fp, err := testcerts.ReadFingerprint(dir)
				Expect(err).NotTo(HaveOccurred())
				Expect(fp).To(BeEmpty())
			})
		})
	})

	// =================================================================
	// LEVEL 5: MUTUAL TLS HANDSHAKE
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
				MinVersion:   tls.VersionTLS12, // DevSkim: ignore DS440001,DS112852 - TLS 1.2 minimum for test security
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
				MinVersion:   tls.VersionTLS12, // DevSkim: ignore DS440001,DS112852 - TLS 1.2 minimum for test security
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
