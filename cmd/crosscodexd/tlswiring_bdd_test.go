//go:build !integration

package main

import (
	"context"
	"net/http"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/testcerts"
	"github.com/complytime-labs/crosscodex/pkg/config"
)

var (
	testTLSWiringCAPath         string
	testTLSWiringClientCertPath string
	testTLSWiringClientKeyPath  string
)

var _ = BeforeSuite(func() {
	certDir := GinkgoT().TempDir()
	pki, err := testcerts.Generate()
	Expect(err).NotTo(HaveOccurred(), "generate test PKI")
	Expect(pki.WriteToDir(certDir)).To(Succeed(), "write test PKI")
	testTLSWiringCAPath = filepath.Join(certDir, "ca.pem")
	testTLSWiringClientCertPath = filepath.Join(certDir, "client.pem")
	testTLSWiringClientKeyPath = filepath.Join(certDir, "client-key.pem")
})

var _ = Describe("llmHTTPClient", func() {
	var cfg *config.Config

	BeforeEach(func() {
		cfg = &config.Config{}
		cfg.TLS = config.TLSConfig{
			Mode: "mutual",
			CA:   testTLSWiringCAPath,
			Cert: testTLSWiringClientCertPath,
			Key:  testTLSWiringClientKeyPath,
		}
		cfg.LLM.Timeout = 30
	})

	When("the gateway URL is https", func() {
		It("returns a client with an mTLS transport", func() {
			cfg.LLM.GatewayURL = "https://litellm:4443"
			c, err := llmHTTPClient(context.Background(), cfg)
			Expect(err).NotTo(HaveOccurred())
			Expect(c).NotTo(BeNil())
			tr, ok := c.Transport.(*http.Transport)
			Expect(ok).To(BeTrue())
			Expect(tr.TLSClientConfig).NotTo(BeNil())
			Expect(tr.TLSClientConfig.RootCAs).NotTo(BeNil())
			Expect(tr.TLSClientConfig.GetClientCertificate).NotTo(BeNil())
		})
	})

	When("the gateway URL is http", func() {
		It("returns nil so the default plaintext client is used", func() {
			cfg.LLM.GatewayURL = "http://litellm:4000"
			c, err := llmHTTPClient(context.Background(), cfg)
			Expect(err).NotTo(HaveOccurred())
			Expect(c).To(BeNil())
		})
	})
})

var _ = Describe("natsTLSConfig", func() {
	var cfg *config.Config

	BeforeEach(func() {
		cfg = &config.Config{}
		cfg.TLS = config.TLSConfig{
			Mode: "mutual",
			CA:   testTLSWiringCAPath,
			Cert: testTLSWiringClientCertPath,
			Key:  testTLSWiringClientKeyPath,
		}
	})

	When("NATS is external with TLS enabled", func() {
		It("returns an mTLS config", func() {
			cfg.NATS.URL = "tls://nats:4222"
			cfg.NATS.TLS = true
			c, err := natsTLSConfig(context.Background(), cfg)
			Expect(err).NotTo(HaveOccurred())
			Expect(c).NotTo(BeNil())
			Expect(c.RootCAs).NotTo(BeNil())
			Expect(c.GetClientCertificate).NotTo(BeNil())
		})
	})

	When("NATS is embedded (empty URL)", func() {
		It("returns nil", func() {
			cfg.NATS.URL = ""
			cfg.NATS.TLS = true
			c, err := natsTLSConfig(context.Background(), cfg)
			Expect(err).NotTo(HaveOccurred())
			Expect(c).To(BeNil())
		})
	})

	When("NATS TLS is disabled", func() {
		It("returns nil", func() {
			cfg.NATS.URL = "nats://nats:4222"
			cfg.NATS.TLS = false
			c, err := natsTLSConfig(context.Background(), cfg)
			Expect(err).NotTo(HaveOccurred())
			Expect(c).To(BeNil())
		})
	})
})
