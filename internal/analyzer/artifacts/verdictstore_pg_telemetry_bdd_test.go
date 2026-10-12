//go:build !integration

package artifacts_test

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/codes"

	"github.com/complytime-labs/crosscodex/internal/analyzer/artifacts"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

var _ = Describe("PGVerdictStore telemetry", func() {
	var (
		tp    *telemetrytest.TestProvider
		store *artifacts.PGVerdictStore
		ctxB  context.Context
	)
	BeforeEach(func() {
		var err error
		tp, err = telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(tp.Shutdown(context.Background())).To(Succeed()) })
		// A nil connection: every call below returns before using it.
		store = artifacts.NewPGVerdictStore(nil, artifacts.WithVerdictStoreTelemetry(tp.TracerProvider(), tp.MeterProvider()))
		ctxB, err = tenant.WithTenant(context.Background(), "tenant-b")
		Expect(err).NotTo(HaveOccurred())
	})

	expectObserved := func(op string, code codes.Code, status string) {
		span := telemetrytest.FindSpan(tp.GetSpans(), "artifacts.verdictstore."+op)
		Expect(span).NotTo(BeNil())
		Expect(span.Status().Code).To(Equal(code))
		v, ok := telemetrytest.SpanAttribute(span, "tenant.id")
		Expect(ok).To(BeTrue())
		Expect(v.AsString()).To(Equal("tenant-a"))
		Expect(counterByAttr(tp, "artifacts.verdictstore.operations.total", "status", status)).To(Equal(int64(1)))
		Expect(counterByAttr(tp, "artifacts.verdictstore.operations.total", "operation", op)).To(Equal(int64(1)))
		m := telemetrytest.FindMetric(tp.GetMetrics(), "artifacts.verdictstore.operation.duration_ms")
		Expect(m).NotTo(BeNil())
		Expect(telemetrytest.HistogramCount(m)).To(Equal(uint64(1)))
	}

	DescribeTable("traces, counts and times every method, including a refused call",
		func(op string, call func() error) {
			Expect(call()).To(MatchError(ContainSubstring("does not match")))
			expectObserved(op, codes.Error, "error")
		},
		Entry("Enqueue", "Enqueue", func() error {
			return store.Enqueue(ctxB, "tenant-a", []artifacts.PairCandidate{{LowGroupID: "a", HighGroupID: "b"}}, time.Time{})
		}),
		Entry("ClosedPairs", "ClosedPairs", func() error { _, err := store.ClosedPairs(ctxB, "tenant-a"); return err }),
		Entry("Lease", "Lease", func() error { _, err := store.Lease(ctxB, "tenant-a", 1, time.Minute, time.Time{}); return err }),
		Entry("Decide", "Decide", func() error {
			_, err := store.Decide(ctxB, "tenant-a", "k", artifacts.Verdict{Status: artifacts.VerdictConfirmed})
			return err
		}),
		Entry("RecordDissent", "RecordDissent", func() error {
			return store.RecordDissent(ctxB, "tenant-a", "k", artifacts.Verdict{Status: artifacts.VerdictConfirmed})
		}),
		Entry("RecordFailure", "RecordFailure", func() error { return store.RecordFailure(ctxB, "tenant-a", "k", artifacts.Failure{}) }),
		Entry("Unprojected", "Unprojected", func() error { _, err := store.Unprojected(ctxB, "tenant-a", 1); return err }),
		Entry("MarkProjected", "MarkProjected", func() error { return store.MarkProjected(ctxB, "tenant-a", "k") }),
		Entry("ResetProjection", "ResetProjection", func() error { _, err := store.ResetProjection(ctxB, "tenant-a"); return err }),
		Entry("Counts", "Counts", func() error { _, err := store.Counts(ctxB, "tenant-a", time.Time{}); return err }),
	)

	It("counts a successful call as ok", func() {
		Expect(store.Enqueue(ctxB, "tenant-a", nil, time.Time{})).To(Succeed())
		expectObserved("Enqueue", codes.Ok, "ok")
	})
})
