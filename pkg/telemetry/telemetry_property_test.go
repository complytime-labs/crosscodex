package telemetry_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/telemetry"
)

// Suite bootstrap lives in telemetry_bdd_test.go — do NOT add RunSpecs here.

var _ = Describe("Property Specifications", Ordered, func() {
	Context("resolveEndpoint — signal override semantics", func() {
		It("non-empty signal endpoint always wins", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				signal := rapid.StringN(1, 50, -1).Draw(t, "signal")
				shared := rapid.String().Draw(t, "shared")
				result := telemetry.ResolveEndpoint(signal, shared)
				if result != signal {
					t.Fatalf("signal endpoint should win: got %q, want %q", result, signal)
				}
			})
		})

		It("empty signal falls back to shared", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				shared := rapid.String().Draw(t, "shared")
				result := telemetry.ResolveEndpoint("", shared)
				if result != shared {
					t.Fatalf("should fall back to shared: got %q, want %q", result, shared)
				}
			})
		})
	})

	Context("resolveProtocol — default to grpc", func() {
		It("both empty returns grpc", func() {
			result := telemetry.ResolveProtocol("", "")
			Expect(result).To(Equal("grpc"))
		})

		It("non-empty signal protocol always wins", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				signal := rapid.SampledFrom([]string{"grpc", "http"}).Draw(t, "signal")
				shared := rapid.String().Draw(t, "shared")
				result := telemetry.ResolveProtocol(signal, shared)
				if result != signal {
					t.Fatalf("signal protocol should win: got %q, want %q", result, signal)
				}
			})
		})
	})
})
