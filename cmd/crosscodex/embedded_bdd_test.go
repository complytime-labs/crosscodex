package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/gateway"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/config"
)

var _ = Describe("ensurePKI", func() {
	It("generates a complete PKI in an empty directory", func() {
		dir := GinkgoT().TempDir()
		pkiDir := filepath.Join(dir, "pki")

		Expect(ensurePKI(pkiDir)).To(Succeed())

		// Directory must be 0700
		info, err := os.Stat(pkiDir)
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o700)))

		// All 6 PEM files must exist
		for _, name := range []string{"ca.pem", "ca-key.pem", "server.pem", "server-key.pem", "client.pem", "client-key.pem"} {
			path := filepath.Join(pkiDir, name)
			_, err := os.Stat(path)
			Expect(err).NotTo(HaveOccurred(), "missing: "+name)
		}

		// Key files must be 0600
		for _, name := range []string{"ca-key.pem", "server-key.pem", "client-key.pem"} {
			info, err := os.Stat(filepath.Join(pkiDir, name))
			Expect(err).NotTo(HaveOccurred())
			Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o600)), name+" should be 0600")
		}

		// Fingerprint must exist
		_, err = os.Stat(filepath.Join(pkiDir, ".fingerprint"))
		Expect(err).NotTo(HaveOccurred())
	})

	It("reuses existing valid certs", func() {
		dir := GinkgoT().TempDir()
		pkiDir := filepath.Join(dir, "pki")

		Expect(ensurePKI(pkiDir)).To(Succeed())

		// Record fingerprint
		fp1, err := os.ReadFile(filepath.Join(pkiDir, ".fingerprint"))
		Expect(err).NotTo(HaveOccurred())

		// Second call should reuse
		Expect(ensurePKI(pkiDir)).To(Succeed())

		fp2, err := os.ReadFile(filepath.Join(pkiDir, ".fingerprint"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(fp2)).To(Equal(string(fp1)))
	})

	It("regenerates when certs are corrupted", func() {
		dir := GinkgoT().TempDir()
		pkiDir := filepath.Join(dir, "pki")

		Expect(ensurePKI(pkiDir)).To(Succeed())

		fp1, err := os.ReadFile(filepath.Join(pkiDir, ".fingerprint"))
		Expect(err).NotTo(HaveOccurred())

		// Corrupt a cert file
		Expect(os.WriteFile(filepath.Join(pkiDir, "ca.pem"), []byte("corrupted"), 0o644)).To(Succeed())

		// Should regenerate
		Expect(ensurePKI(pkiDir)).To(Succeed())

		fp2, err := os.ReadFile(filepath.Join(pkiDir, ".fingerprint"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(fp2)).NotTo(Equal(string(fp1)))
	})
})

var _ = Describe("connectClientWithTLS", func() {
	It("returns an error when cert files do not exist", func() {
		paths := embeddedTLSPaths{
			CACert:     "/nonexistent/ca.pem",
			ClientCert: "/nonexistent/client.pem",
			ClientKey:  "/nonexistent/client-key.pem",
			ServerCert: "/nonexistent/server.pem",
			ServerKey:  "/nonexistent/server-key.pem",
		}
		client, err := connectClientWithTLS("localhost:0", paths)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("read CA cert"))
		Expect(client).To(BeNil())
	})

	It("returns an error when CA cert is invalid PEM", func() {
		dir := GinkgoT().TempDir()
		pkiDir := filepath.Join(dir, "pki")
		Expect(ensurePKI(pkiDir)).To(Succeed())

		paths := pkiPaths(pkiDir)
		Expect(os.WriteFile(paths.CACert, []byte("not-valid-pem"), 0o644)).To(Succeed())

		client, err := connectClientWithTLS("localhost:0", paths)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("failed to parse CA cert"))
		Expect(client).To(BeNil())
	})

	It("returns an error when client cert/key pair is invalid", func() {
		dir := GinkgoT().TempDir()
		pkiDir := filepath.Join(dir, "pki")
		Expect(ensurePKI(pkiDir)).To(Succeed())

		paths := pkiPaths(pkiDir)
		Expect(os.WriteFile(paths.ClientCert, []byte("not-a-cert"), 0o644)).To(Succeed())

		client, err := connectClientWithTLS("localhost:0", paths)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("load client cert"))
		Expect(client).To(BeNil())
	})

	It("creates a non-nil client with valid certs", func() {
		dir := GinkgoT().TempDir()
		pkiDir := filepath.Join(dir, "pki")
		Expect(ensurePKI(pkiDir)).To(Succeed())

		paths := pkiPaths(pkiDir)
		client, err := connectClientWithTLS("localhost:0", paths)
		Expect(err).NotTo(HaveOccurred())
		Expect(client).NotTo(BeNil())
	})
})

var _ = Describe("embeddedAuthRegistry", func() {
	It("creates a registry that authenticates with embedded client certs", func() {
		dir := GinkgoT().TempDir()
		pkiDir := filepath.Join(dir, "pki")
		Expect(ensurePKI(pkiDir)).To(Succeed())

		registry, err := embeddedAuthRegistry()
		Expect(err).NotTo(HaveOccurred())
		Expect(registry).NotTo(BeNil())

		// Load and parse client cert to build TLS state
		paths := pkiPaths(pkiDir)
		certPEM, err := os.ReadFile(paths.ClientCert)
		Expect(err).NotTo(HaveOccurred())

		block, _ := pem.Decode(certPEM)
		Expect(block).NotTo(BeNil())
		leaf, err := x509.ParseCertificate(block.Bytes)
		Expect(err).NotTo(HaveOccurred())

		req := &authn.Request{
			Method: authn.AuthMethodMTLS,
			TLSState: &tls.ConnectionState{
				PeerCertificates: []*x509.Certificate{leaf},
			},
		}

		identity, err := registry.Authenticate(context.Background(), req)
		Expect(err).NotTo(HaveOccurred())
		Expect(identity.TenantID).To(Equal("embedded"))
		Expect(identity.Roles).To(ContainElement("admin"))
		Expect(identity.Method).To(Equal(authn.AuthMethodMTLS))
	})
})

var _ = Describe("dbAdminBackend", func() {
	It("implements gateway.AdminBackend", func() {
		var _ gateway.AdminBackend = (*dbAdminBackend)(nil)
	})
})

var _ gateway.IngestionBackend = (*localIngestionBackend)(nil)
var _ gateway.PipelineBackend = (*localPipelineBackend)(nil)

var _ = Describe("buildEmbeddedService", func() {
	It("returns an error when database DSN is empty", func() {
		cfg := &config.Config{}
		_, _, err := buildEmbeddedService(context.Background(), cfg, nil)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("database not configured"))
	})
})
