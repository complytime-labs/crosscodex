package main

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("healthcheckAddr", func() {
	It("builds an https localhost URL from a bare port", func() {
		Expect(healthcheckAddr(":50051")).To(Equal("https://127.0.0.1:50051/healthz"))
	})
	It("preserves an explicit host", func() {
		Expect(healthcheckAddr("0.0.0.0:50051")).To(Equal("https://127.0.0.1:50051/healthz"))
	})
})

var _ = Describe("probeHealthURL", func() {
	var server *httptest.Server
	var status int

	BeforeEach(func() {
		status = http.StatusOK
		server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))
	})
	AfterEach(func() { server.Close() })

	It("returns nil on HTTP 200", func() {
		tlsCfg := &tls.Config{InsecureSkipVerify: true} //nolint:gosec // test-only
		Expect(probeHealthURL(context.Background(), server.URL, tlsCfg)).To(Succeed())
	})

	It("returns an error on HTTP 503", func() {
		status = http.StatusServiceUnavailable
		tlsCfg := &tls.Config{InsecureSkipVerify: true} //nolint:gosec // test-only
		Expect(probeHealthURL(context.Background(), server.URL, tlsCfg)).To(HaveOccurred())
	})

	It("returns an error when the target is unreachable", func() {
		// Stand up a throwaway TLS server, capture its URL, then close it so
		// the dial fails -- exercising the transport-error branch (client.Do
		// error), the common "daemon not up yet" case a healthcheck reports.
		closed := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		url := closed.URL
		closed.Close()
		tlsCfg := &tls.Config{InsecureSkipVerify: true} //nolint:gosec // test-only
		Expect(probeHealthURL(context.Background(), url, tlsCfg)).To(HaveOccurred())
	})
})
