package worker

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/natsbus/natsbustest"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
)

var _ = Describe("worker.abandoned_collections", func() {
	It("drops to zero once the only remembered abandon expires, when the next task is checked", func() {
		tp, err := telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(tp.Shutdown(context.Background())).To(Succeed()) })
		w := New(&natsbustest.Fake{}, nil, WorkerConfig{MaxAbandonedCollections: 10, MaxInFlight: 1},
			WithAbandonTTL(time.Minute), WithTelemetry(tp.TracerProvider(), tp.MeterProvider()))
		now := time.Unix(0, 0)
		w.abandoned.now = func() time.Time { return now }
		c := natsbus.AbandonedCollection{TenantID: "tenant-a", TaskType: natsbus.TaskRelate, JobID: "job-1",
			CollectionID: "6f1c0e2a-3b4d-4e5f-8a9b-0c1d2e3f4a5b"}
		subject, err := natsbus.AbandonedSubject(c)
		Expect(err).NotTo(HaveOccurred())
		gauge := func() int64 {
			m := telemetrytest.FindMetric(tp.GetMetrics(), "worker.abandoned_collections")
			Expect(m).NotTo(BeNil())
			v, err := telemetrytest.GaugeValue(m)
			Expect(err).NotTo(HaveOccurred())
			return v
		}

		Expect(w.handleAbandoned(context.Background(), &natsbus.Message{
			Subject: subject, Metadata: natsbus.MessageMetadata{TenantID: c.TenantID},
		})).To(Succeed())
		Expect(gauge()).To(Equal(int64(1)))

		now = now.Add(2 * time.Minute)
		Expect(w.abandoned.contains(c)).To(BeFalse())
		Expect(gauge()).To(BeZero())
	})
})
