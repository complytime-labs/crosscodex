package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("healthCheckHandler", func() {
	It("returns 200 with zero checks (pure liveness)", func() {
		req := httptest.NewRequest(http.MethodGet, "/healthz", http.NoBody)
		rec := httptest.NewRecorder()
		healthCheckHandler().ServeHTTP(rec, req)
		Expect(rec.Code).To(Equal(http.StatusOK))
		Expect(rec.Body.String()).To(ContainSubstring(`"status":"healthy"`))
	})

	It("returns 200 when every check passes", func() {
		req := httptest.NewRequest(http.MethodGet, "/healthz", http.NoBody)
		rec := httptest.NewRecorder()
		passA := func(context.Context) error { return nil }
		passB := func(context.Context) error { return nil }
		healthCheckHandler(passA, passB).ServeHTTP(rec, req)
		Expect(rec.Code).To(Equal(http.StatusOK))
	})

	It("returns 503 with an actionable body when a check fails", func() {
		req := httptest.NewRequest(http.MethodGet, "/healthz", http.NoBody)
		rec := httptest.NewRecorder()
		fail := func(context.Context) error { return errors.New("database not connected") }
		healthCheckHandler(fail).ServeHTTP(rec, req)
		Expect(rec.Code).To(Equal(http.StatusServiceUnavailable))
		Expect(rec.Body.String()).To(ContainSubstring("database not connected"))
	})

	It("stops at the first failing check", func() {
		req := httptest.NewRequest(http.MethodGet, "/healthz", http.NoBody)
		rec := httptest.NewRecorder()
		called := false
		fail := func(context.Context) error { return errors.New("first check failed") }
		neverCalled := func(context.Context) error { called = true; return nil }
		healthCheckHandler(fail, neverCalled).ServeHTTP(rec, req)
		Expect(rec.Code).To(Equal(http.StatusServiceUnavailable))
		Expect(called).To(BeFalse())
	})
})
