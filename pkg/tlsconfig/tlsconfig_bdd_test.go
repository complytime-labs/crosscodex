package tlsconfig_test

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/tlsconfig"
	"github.com/complytime-labs/crosscodex/pkg/tlsconfig/pki"
)

func TestTLSConfigBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "TLSConfig BDD Suite")
}

var _ = BeforeSuite(func() { DeferCleanup(testspecs.RedirectLogsToGinkgo()) })

var _ = Describe("TLSConfig System", Ordered, func() {
	var (
		bundle  *pki.PKIBundle
		certDir string
	)

	BeforeAll(func() {
		testspecs.LogTestProgress("Starting TLSConfig BDD test suite")

		By("generating test PKI for the entire suite")
		var err error
		certDir = GinkgoT().TempDir()
		bundle, err = pki.GenerateDevPKI(pki.WithOutputDir(certDir))
		Expect(err).NotTo(HaveOccurred())
		Expect(bundle).NotTo(BeNil())
	})

	AfterAll(func() {
		testspecs.LogTestProgress("TLSConfig BDD test suite completed")
	})

	// Helper to get cert file paths
	caPath := func() string { return filepath.Join(certDir, "ca.pem") }
	certPath := func() string { return filepath.Join(certDir, "server.pem") }
	keyPath := func() string { return filepath.Join(certDir, "server-key.pem") }
	clientCert := func() string { return filepath.Join(certDir, "client.pem") }
	clientKey := func() string { return filepath.Join(certDir, "client-key.pem") }

	// =================================================================
	// LEVEL 1: CONFIG MERGING
	// =================================================================

	Describe("Config Merging", func() {
		Context("when no target is specified", func() {
			It("uses global config only", func() {
				mode, ca, cert, key := tlsconfig.MergeConfigFields(config.TLSConfig{
					Mode: "mutual",
					CA:   "/global/ca.pem",
					Cert: "/global/cert.pem",
					Key:  "/global/key.pem",
				}, "")

				Expect(mode).To(Equal("mutual"))
				Expect(ca).To(Equal("/global/ca.pem"))
				Expect(cert).To(Equal("/global/cert.pem"))
				Expect(key).To(Equal("/global/key.pem"))
			})
		})

		Context("when a known target is specified", func() {
			It("overlays non-zero override fields", func() {
				mode, ca, cert, key := tlsconfig.MergeConfigFields(config.TLSConfig{
					Mode: "mutual",
					CA:   "/global/ca.pem",
					Cert: "/global/cert.pem",
					Key:  "/global/key.pem",
					Targets: map[string]config.TLSOverride{
						"nats": {
							Cert: "/nats/cert.pem",
							Key:  "/nats/key.pem",
						},
					},
				}, "nats")

				By("keeping global mode and CA")
				Expect(mode).To(Equal("mutual"))
				Expect(ca).To(Equal("/global/ca.pem"))

				By("using nats-specific cert and key")
				Expect(cert).To(Equal("/nats/cert.pem"))
				Expect(key).To(Equal("/nats/key.pem"))
			})
		})

		Context("when target overrides mode", func() {
			It("uses the override mode", func() {
				mode, _, _, _ := tlsconfig.MergeConfigFields(config.TLSConfig{
					Mode: "mutual",
					Targets: map[string]config.TLSOverride{
						"database": {Mode: "server-only"},
					},
				}, "database")

				Expect(mode).To(Equal("server-only"))
			})
		})

		Context("when target overrides CA only", func() {
			It("uses the override CA while keeping global cert and key", func() {
				mode, ca, cert, key := tlsconfig.MergeConfigFields(config.TLSConfig{
					Mode: "mutual",
					CA:   "/global/ca.pem",
					Cert: "/global/cert.pem",
					Key:  "/global/key.pem",
					Targets: map[string]config.TLSOverride{
						"database": {CA: "/database/ca.pem"},
					},
				}, "database")

				Expect(mode).To(Equal("mutual"))
				Expect(ca).To(Equal("/database/ca.pem"))
				Expect(cert).To(Equal("/global/cert.pem"))
				Expect(key).To(Equal("/global/key.pem"))
			})
		})

		Context("when target is unknown", func() {
			It("falls through to global config", func() {
				mode, ca, _, _ := tlsconfig.MergeConfigFields(config.TLSConfig{
					Mode: "mutual",
					CA:   "/global/ca.pem",
					Targets: map[string]config.TLSOverride{
						"nats": {Mode: "server-only"},
					},
				}, "grpc")

				Expect(mode).To(Equal("mutual"))
				Expect(ca).To(Equal("/global/ca.pem"))
			})
		})
	})

	// =================================================================
	// LEVEL 2: TLS MODE BEHAVIOR
	// =================================================================

	Describe("BuildTLSConfig", func() {
		Context("when mode is off", func() {
			It("returns nil, nil", func() {
				tlsCfg, err := tlsconfig.BuildTLSConfig(config.TLSConfig{
					Mode: "off",
				}, "")
				Expect(err).NotTo(HaveOccurred())
				Expect(tlsCfg).To(BeNil())
			})
		})

		Context("when mode is empty", func() {
			It("returns nil, nil", func() {
				tlsCfg, err := tlsconfig.BuildTLSConfig(config.TLSConfig{}, "")
				Expect(err).NotTo(HaveOccurred())
				Expect(tlsCfg).To(BeNil())
			})
		})

		Context("when mode is server-only with valid certs", func() {
			It("returns a tls.Config with certificates and reload callback", func() {
				tlsCfg, err := tlsconfig.BuildTLSConfig(config.TLSConfig{
					Mode: "server-only",
					CA:   caPath(),
					Cert: certPath(),
					Key:  keyPath(),
				}, "")
				Expect(err).NotTo(HaveOccurred())
				Expect(tlsCfg).NotTo(BeNil())

				By("verifying MinVersion is TLS 1.2")                         // DevSkim: ignore DS440001 - test step description
				Expect(tlsCfg.MinVersion).To(Equal(uint16(tls.VersionTLS12))) // DevSkim: ignore DS440001,DS112852 - asserting TLS 1.2 minimum is enforced

				By("verifying static Certificates are loaded")
				Expect(tlsCfg.Certificates).To(HaveLen(1))

				By("verifying GetCertificate reload callback is set")
				Expect(tlsCfg.GetCertificate).NotTo(BeNil())

				By("verifying RootCAs pool is loaded")
				Expect(tlsCfg.RootCAs).NotTo(BeNil())

				By("verifying no client auth is required")
				Expect(tlsCfg.ClientAuth).To(Equal(tls.NoClientCert))

				By("verifying no GetClientCertificate callback")
				Expect(tlsCfg.GetClientCertificate).To(BeNil())
			})
		})

		Context("when mode is mutual with valid certs", func() {
			It("returns a tls.Config with mTLS settings", func() {
				tlsCfg, err := tlsconfig.BuildTLSConfig(config.TLSConfig{
					Mode: "mutual",
					CA:   caPath(),
					Cert: certPath(),
					Key:  keyPath(),
				}, "")
				Expect(err).NotTo(HaveOccurred())
				Expect(tlsCfg).NotTo(BeNil())

				By("verifying RequireAndVerifyClientCert")
				Expect(tlsCfg.ClientAuth).To(Equal(tls.RequireAndVerifyClientCert))

				By("verifying ClientCAs pool is set")
				Expect(tlsCfg.ClientCAs).NotTo(BeNil())

				By("verifying GetClientCertificate callback is set")
				Expect(tlsCfg.GetClientCertificate).NotTo(BeNil())

				By("verifying GetCertificate callback is set")
				Expect(tlsCfg.GetCertificate).NotTo(BeNil())
			})
		})

		Context("when mode is invalid", func() {
			It("returns ErrInvalidMode", func() {
				_, err := tlsconfig.BuildTLSConfig(config.TLSConfig{
					Mode: "invalid",
					Cert: certPath(),
					Key:  keyPath(),
				}, "")
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, tlsconfig.ErrInvalidMode)).To(BeTrue())
			})
		})

		Context("when target overrides mode to off", func() {
			It("returns nil, nil for that target", func() {
				tlsCfg, err := tlsconfig.BuildTLSConfig(config.TLSConfig{
					Mode: "mutual",
					CA:   caPath(),
					Cert: certPath(),
					Key:  keyPath(),
					Targets: map[string]config.TLSOverride{
						"metrics": {Mode: "off"},
					},
				}, "metrics")
				Expect(err).NotTo(HaveOccurred())
				Expect(tlsCfg).To(BeNil())
			})
		})
	})

	// =================================================================
	// LEVEL 3: ERROR HANDLING
	// =================================================================

	Describe("Error Handling", func() {
		Context("when key is specified without cert", func() {
			It("returns ErrMissingCert", func() {
				_, err := tlsconfig.BuildTLSConfig(config.TLSConfig{
					Mode: "server-only",
					Key:  keyPath(),
				}, "")
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, tlsconfig.ErrMissingCert)).To(BeTrue())
			})
		})

		Context("when cert is specified without key", func() {
			It("returns ErrMissingKey", func() {
				_, err := tlsconfig.BuildTLSConfig(config.TLSConfig{
					Mode: "server-only",
					Cert: certPath(),
				}, "")
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, tlsconfig.ErrMissingKey)).To(BeTrue())
			})
		})

		Context("when neither cert nor key is specified", func() {
			It("succeeds with an empty tls.Config (client-side usage)", func() {
				tlsCfg, err := tlsconfig.BuildTLSConfig(config.TLSConfig{
					Mode: "server-only",
					CA:   caPath(),
				}, "")
				Expect(err).NotTo(HaveOccurred())
				Expect(tlsCfg).NotTo(BeNil())
				Expect(tlsCfg.Certificates).To(BeEmpty())
				Expect(tlsCfg.RootCAs).NotTo(BeNil())
			})
		})

		Context("when CA is missing for mutual mode", func() {
			It("returns ErrMissingCA", func() {
				_, err := tlsconfig.BuildTLSConfig(config.TLSConfig{
					Mode: "mutual",
					Cert: certPath(),
					Key:  keyPath(),
				}, "")
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, tlsconfig.ErrMissingCA)).To(BeTrue())
			})
		})

		Context("when cert file does not exist", func() {
			It("returns ErrCertificateLoadFailed with file path", func() {
				_, err := tlsconfig.BuildTLSConfig(config.TLSConfig{
					Mode: "server-only",
					Cert: "/nonexistent/cert.pem",
					Key:  keyPath(),
				}, "myservice")
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, tlsconfig.ErrCertificateLoadFailed)).To(BeTrue())
				Expect(err.Error()).To(ContainSubstring("myservice"))
				Expect(err.Error()).To(ContainSubstring("/nonexistent/cert.pem"))
			})
		})

		Context("when CA file does not exist", func() {
			It("returns an error with the file path", func() {
				_, err := tlsconfig.BuildTLSConfig(config.TLSConfig{
					Mode: "server-only",
					CA:   "/nonexistent/ca.pem",
					Cert: certPath(),
					Key:  keyPath(),
				}, "myservice")
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("/nonexistent/ca.pem"))
				Expect(err.Error()).To(ContainSubstring("myservice"))
			})
		})

		Context("when CA file contains no valid certs", func() {
			It("returns ErrInvalidCertificate", func() {
				badCA := filepath.Join(GinkgoT().TempDir(), "bad-ca.pem")
				Expect(os.WriteFile(badCA, []byte("not a cert"), 0644)).To(Succeed())

				_, err := tlsconfig.BuildTLSConfig(config.TLSConfig{
					Mode: "server-only",
					CA:   badCA,
					Cert: certPath(),
					Key:  keyPath(),
				}, "")
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, tlsconfig.ErrInvalidCertificate)).To(BeTrue())
			})
		})

		Context("when target override cert path does not exist", func() {
			It("fails without falling back to global cert", func() {
				_, err := tlsconfig.BuildTLSConfig(config.TLSConfig{
					Mode: "server-only",
					Cert: certPath(),
					Key:  keyPath(),
					Targets: map[string]config.TLSOverride{
						"nats": {
							Cert: "/nonexistent/nats.pem",
						},
					},
				}, "nats")
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("nats"))
				Expect(err.Error()).To(ContainSubstring("/nonexistent/nats.pem"))
			})
		})

		Context("when error messages include target name", func() {
			DescribeTable("target name appears in error",
				func(target string, cfg config.TLSConfig) {
					_, err := tlsconfig.BuildTLSConfig(cfg, target)
					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring(target))
				},
				Entry("key without cert with target", "database", config.TLSConfig{
					Mode: "server-only",
					Key:  "/some/key.pem",
				}),
				Entry("invalid mode with target", "api", config.TLSConfig{
					Mode: "bogus",
					Cert: "/some/cert.pem",
					Key:  "/some/key.pem",
				}),
			)
		})
	})

	// =================================================================
	// LEVEL 4: CIPHER FILTERING
	// =================================================================

	Describe("Cipher Filtering", func() {
		Context("FIPS cipher suites", func() {
			It("returns only GCM suites", func() {
				ids := tlsconfig.FipsCipherSuites()
				Expect(ids).NotTo(BeEmpty())

				By("verifying all returned suites contain GCM in their name")
				allSuites := tls.CipherSuites()
				nameByID := make(map[uint16]string)
				for _, cs := range allSuites {
					nameByID[cs.ID] = cs.Name
				}
				for _, id := range ids {
					Expect(nameByID[id]).To(ContainSubstring("GCM"),
						"FIPS suite %s should contain GCM", nameByID[id])
				}
			})

			It("excludes non-GCM suites", func() {
				ids := tlsconfig.FipsCipherSuites()
				idSet := make(map[uint16]bool)
				for _, id := range ids {
					idSet[id] = true
				}

				for _, cs := range tls.CipherSuites() {
					if cs.Name == "" {
						continue
					}
					if !idSet[cs.ID] {
						Expect(cs.Name).NotTo(ContainSubstring("GCM"),
							"non-FIPS suite %s should not contain GCM", cs.Name)
					}
				}
			})
		})

		Context("allow filter", func() {
			It("keeps only matching suites", func() {
				base := tlsconfig.AllNonInsecureCipherIDs()
				filtered, err := tlsconfig.FilterCiphers(base, []string{"AES_256"}, nil)
				Expect(err).NotTo(HaveOccurred())
				Expect(filtered).NotTo(BeEmpty())

				allSuites := tls.CipherSuites()
				nameByID := make(map[uint16]string)
				for _, cs := range allSuites {
					nameByID[cs.ID] = cs.Name
				}
				for _, id := range filtered {
					Expect(nameByID[id]).To(ContainSubstring("AES_256"))
				}
			})
		})

		Context("deny filter", func() {
			It("removes matching suites", func() {
				base := tlsconfig.AllNonInsecureCipherIDs()
				filtered, err := tlsconfig.FilterCiphers(base, nil, []string{"SHA256"})
				Expect(err).NotTo(HaveOccurred())

				allSuites := tls.CipherSuites()
				nameByID := make(map[uint16]string)
				for _, cs := range allSuites {
					nameByID[cs.ID] = cs.Name
				}
				for _, id := range filtered {
					Expect(nameByID[id]).NotTo(ContainSubstring("SHA256"))
				}
			})
		})

		Context("combined allow and deny", func() {
			It("applies allow first then deny", func() {
				base := tlsconfig.AllNonInsecureCipherIDs()
				filtered, err := tlsconfig.FilterCiphers(base, []string{"GCM"}, []string{"AES_128"})
				Expect(err).NotTo(HaveOccurred())

				allSuites := tls.CipherSuites()
				nameByID := make(map[uint16]string)
				for _, cs := range allSuites {
					nameByID[cs.ID] = cs.Name
				}
				for _, id := range filtered {
					name := nameByID[id]
					Expect(name).To(ContainSubstring("GCM"))
					Expect(name).NotTo(ContainSubstring("AES_128"))
				}
			})
		})

		Context("when no suites remain", func() {
			It("returns ErrNoCiphersAvailable", func() {
				base := tlsconfig.AllNonInsecureCipherIDs()
				_, err := tlsconfig.FilterCiphers(base,
					[]string{"NONEXISTENT_CIPHER"}, nil)
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, tlsconfig.ErrNoCiphersAvailable)).To(BeTrue())
			})
		})

		Context("cipher filters applied via BuildTLSConfig", func() {
			It("sets CipherSuites when cipher_allow is specified", func() {
				tlsCfg, err := tlsconfig.BuildTLSConfig(config.TLSConfig{
					Mode:        "server-only",
					Cert:        certPath(),
					Key:         keyPath(),
					CipherAllow: []string{"GCM"},
				}, "")
				Expect(err).NotTo(HaveOccurred())
				Expect(tlsCfg.CipherSuites).NotTo(BeEmpty())
			})

			It("sets CipherSuites when cipher_deny is specified", func() {
				tlsCfg, err := tlsconfig.BuildTLSConfig(config.TLSConfig{
					Mode:       "server-only",
					Cert:       certPath(),
					Key:        keyPath(),
					CipherDeny: []string{"SHA256"},
				}, "")
				Expect(err).NotTo(HaveOccurred())
				Expect(tlsCfg.CipherSuites).NotTo(BeEmpty())

				By("verifying denied suites are excluded")
				allSuites := tls.CipherSuites()
				nameByID := make(map[uint16]string)
				for _, cs := range allSuites {
					nameByID[cs.ID] = cs.Name
				}
				for _, id := range tlsCfg.CipherSuites {
					Expect(nameByID[id]).NotTo(ContainSubstring("SHA256"))
				}
			})

			It("leaves CipherSuites nil when no filters are applied", func() {
				tlsCfg, err := tlsconfig.BuildTLSConfig(config.TLSConfig{
					Mode: "server-only",
					Cert: certPath(),
					Key:  keyPath(),
				}, "")
				Expect(err).NotTo(HaveOccurred())
				Expect(tlsCfg.CipherSuites).To(BeNil())
			})
		})
	})

	// =================================================================
	// LEVEL 5: CERTIFICATE RELOAD
	// =================================================================

	Describe("Certificate Reload Callbacks", func() {
		Context("GetCertificate callback", func() {
			It("returns a valid certificate from disk", func() {
				fn := tlsconfig.MakeGetCertificate(certPath(), keyPath())

				cert, err := fn(nil)
				Expect(err).NotTo(HaveOccurred())
				Expect(cert).NotTo(BeNil())
			})

			It("returns an error for missing files", func() {
				fn := tlsconfig.MakeGetCertificate("/nonexistent/cert.pem", "/nonexistent/key.pem")

				_, err := fn(nil)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("/nonexistent/cert.pem"))
			})
		})

		Context("GetClientCertificate callback", func() {
			It("returns a valid certificate from disk", func() {
				fn := tlsconfig.MakeGetClientCertificate(clientCert(), clientKey())

				cert, err := fn(nil)
				Expect(err).NotTo(HaveOccurred())
				Expect(cert).NotTo(BeNil())
			})

			It("returns an error for missing files", func() {
				fn := tlsconfig.MakeGetClientCertificate("/nonexistent/client.pem", "/nonexistent/client-key.pem")

				_, err := fn(nil)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("/nonexistent/client.pem"))
			})
		})

		Context("reload detects cert changes on disk", func() {
			It("loads different certs after file replacement", func() {
				By("creating initial certs")
				dir := GinkgoT().TempDir()
				_, err := pki.GenerateDevPKI(pki.WithOutputDir(dir))
				Expect(err).NotTo(HaveOccurred())

				serverCertPath := filepath.Join(dir, "server.pem")
				serverKeyPath := filepath.Join(dir, "server-key.pem")

				fn := tlsconfig.MakeGetCertificate(serverCertPath, serverKeyPath)

				cert1, err := fn(nil)
				Expect(err).NotTo(HaveOccurred())

				By("replacing the cert with a new one")
				newBundle, err := pki.GenerateDevPKI()
				Expect(err).NotTo(HaveOccurred())
				Expect(os.WriteFile(serverCertPath, newBundle.Server.CertPEM, 0644)).To(Succeed())
				Expect(os.WriteFile(serverKeyPath, newBundle.Server.KeyPEM, 0600)).To(Succeed())

				cert2, err := fn(nil)
				Expect(err).NotTo(HaveOccurred())

				By("verifying the serial numbers differ")
				parsed1, err := x509.ParseCertificate(cert1.Certificate[0])
				Expect(err).NotTo(HaveOccurred())
				parsed2, err := x509.ParseCertificate(cert2.Certificate[0])
				Expect(err).NotTo(HaveOccurred())
				Expect(parsed1.SerialNumber.Cmp(parsed2.SerialNumber)).NotTo(Equal(0),
					"reloaded cert should have different serial number")
			})
		})
	})

	// =================================================================
	// LEVEL 6: FIPS BUILD VERIFICATION
	// =================================================================

	Describe("VerifyFIPSBuild", func() {
		Context("in standard (non-FIPS) build", func() {
			It("returns ErrFIPSNotEnabled with Enabled=false", func() {
				status, err := tlsconfig.VerifyFIPSBuild()
				if err == nil {
					Skip("BoringCrypto is available — skip non-FIPS assertion")
				}

				Expect(errors.Is(err, tlsconfig.ErrFIPSNotEnabled)).To(BeTrue())
				Expect(status.Enabled).To(BeFalse())
				Expect(status.Provider).To(BeEmpty())
			})
		})

		Context("in FIPS build", func() {
			It("returns FIPSStatus with Enabled=true and BoringCrypto provider", func() {
				status, err := tlsconfig.VerifyFIPSBuild()
				if err != nil {
					Skip("BoringCrypto not available — skip FIPS assertion")
				}

				Expect(status.Enabled).To(BeTrue())
				Expect(status.Provider).To(Equal("BoringCrypto"))
			})
		})

		Context("FIPS enforcement in BuildTLSConfig", func() {
			It("fails when FIPS enabled but BoringCrypto not linked", func() {
				// This test only applies in non-FIPS builds
				_, checkErr := tlsconfig.VerifyFIPSBuild()
				if checkErr == nil {
					Skip("BoringCrypto is available — skip non-FIPS test")
				}

				_, err := tlsconfig.BuildTLSConfig(config.TLSConfig{
					Mode: "server-only",
					Cert: certPath(),
					Key:  keyPath(),
					FIPS: config.FIPSConfig{Enabled: true},
				}, "")
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, tlsconfig.ErrFIPSNotEnabled)).To(BeTrue())
			})
		})
	})

	// =================================================================
	// LEVEL 7: END-TO-END TLS HANDSHAKE
	// =================================================================

	Describe("End-to-End TLS Handshake", func() {
		It("completes a mutual TLS handshake using BuildTLSConfig", func() {
			By("building server TLS config")
			serverTLS, err := tlsconfig.BuildTLSConfig(config.TLSConfig{
				Mode: "mutual",
				CA:   caPath(),
				Cert: certPath(),
				Key:  keyPath(),
			}, "")
			Expect(err).NotTo(HaveOccurred())
			Expect(serverTLS).NotTo(BeNil())

			By("starting a TLS listener")
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
				if tlsConn, ok := conn.(*tls.Conn); ok {
					serverDone <- tlsConn.Handshake()
				} else {
					serverDone <- nil
				}
			}()

			By("building client TLS config")
			clientTLS, err := tlsconfig.BuildTLSConfig(config.TLSConfig{
				Mode: "mutual",
				CA:   caPath(),
				Cert: clientCert(),
				Key:  clientKey(),
			}, "")
			Expect(err).NotTo(HaveOccurred())
			Expect(clientTLS).NotTo(BeNil())

			By("dialing with mTLS")
			conn, err := tls.Dial("tcp", ln.Addr().String(), clientTLS)
			Expect(err).NotTo(HaveOccurred())
			defer conn.Close()

			By("waiting for server-side handshake")
			Expect(<-serverDone).NotTo(HaveOccurred())

			By("verifying connection state")
			state := conn.ConnectionState()
			Expect(state.PeerCertificates).NotTo(BeEmpty())
			Expect(state.HandshakeComplete).To(BeTrue())
		})

		It("completes a server-only TLS handshake", func() {
			By("building server TLS config")
			serverTLS, err := tlsconfig.BuildTLSConfig(config.TLSConfig{
				Mode: "server-only",
				Cert: certPath(),
				Key:  keyPath(),
			}, "")
			Expect(err).NotTo(HaveOccurred())

			By("starting a TLS listener")
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
				if tlsConn, ok := conn.(*tls.Conn); ok {
					serverDone <- tlsConn.Handshake()
				} else {
					serverDone <- nil
				}
			}()

			By("building client TLS config with CA trust")
			clientTLS, err := tlsconfig.BuildTLSConfig(config.TLSConfig{
				Mode: "server-only",
				CA:   caPath(),
			}, "")
			Expect(err).NotTo(HaveOccurred())

			By("dialing with server-only TLS")
			conn, err := tls.Dial("tcp", ln.Addr().String(), clientTLS)
			Expect(err).NotTo(HaveOccurred())
			defer conn.Close()

			Expect(<-serverDone).NotTo(HaveOccurred())
		})

		It("rejects a client without a trusted certificate in mutual TLS", func() {
			By("building server TLS config requiring client certs")
			serverTLS, err := tlsconfig.BuildTLSConfig(config.TLSConfig{
				Mode: "mutual",
				CA:   caPath(),
				Cert: certPath(),
				Key:  keyPath(),
			}, "")
			Expect(err).NotTo(HaveOccurred())

			By("starting a TLS listener")
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
				if tlsConn, ok := conn.(*tls.Conn); ok {
					serverDone <- tlsConn.Handshake()
				} else {
					serverDone <- nil
				}
			}()

			By("generating a separate untrusted PKI")
			untrustedBundle, err := pki.GenerateDevPKI()
			Expect(err).NotTo(HaveOccurred())

			By("building client config with untrusted cert")
			untrustedDir := GinkgoT().TempDir()
			err = os.WriteFile(filepath.Join(untrustedDir, "client.pem"), untrustedBundle.Client.CertPEM, 0644)
			Expect(err).NotTo(HaveOccurred())
			err = os.WriteFile(filepath.Join(untrustedDir, "client-key.pem"), untrustedBundle.Client.KeyPEM, 0600)
			Expect(err).NotTo(HaveOccurred())

			// Use the server's CA for trust (so we trust the server), but present
			// an untrusted client cert (signed by a different CA)
			clientTLS, err := tlsconfig.BuildTLSConfig(config.TLSConfig{
				Mode: "mutual",
				CA:   caPath(),
				Cert: filepath.Join(untrustedDir, "client.pem"),
				Key:  filepath.Join(untrustedDir, "client-key.pem"),
			}, "")
			Expect(err).NotTo(HaveOccurred())

			By("dialing — handshake should fail")
			conn, dialErr := tls.Dial("tcp", ln.Addr().String(), clientTLS)
			if dialErr == nil {
				conn.Close()
			}

			By("verifying server rejected the client")
			serverErr := <-serverDone
			// Either the dial or the server handshake must fail
			Expect(dialErr != nil || serverErr != nil).To(BeTrue(),
				"expected handshake failure with untrusted client cert")
		})
	})

	// =================================================================
	// LEVEL 8: RESOLVER STRUCT
	// =================================================================

	Describe("Resolver", func() {
		It("produces identical results to package-level functions", func() {
			var r tlsconfig.Resolver

			By("comparing BuildTLSConfig results")
			cfg := config.TLSConfig{
				Mode: "server-only",
				Cert: certPath(),
				Key:  keyPath(),
			}

			pkgResult, pkgErr := tlsconfig.BuildTLSConfig(cfg, "")
			resolverResult, resolverErr := r.BuildTLSConfig(cfg, "")

			Expect(pkgErr).NotTo(HaveOccurred())
			Expect(resolverErr).NotTo(HaveOccurred())
			Expect(pkgResult).NotTo(BeNil())
			Expect(resolverResult).NotTo(BeNil())

			// Both should have the same structure
			Expect(pkgResult.MinVersion).To(Equal(resolverResult.MinVersion))
			Expect(len(pkgResult.Certificates)).To(Equal(len(resolverResult.Certificates)))
		})
	})
})
