package graph_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/graph"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/natsbus/natsbustest"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
)

var _ = Describe("Materialization barrier", func() {
	const (
		tenantID     = "test-tenant"
		jobID        = "job-1"
		stageSubject = "crosscodex.pipeline." + tenantID + "." + jobID + ".stage.completed"
		failedEvents = "crosscodex.audit." + tenantID + ".events." + jobID + ".graph.failed"
		barrierSubj  = "crosscodex.materialize." + tenantID + "." + jobID + ".barrier"
	)

	type countKey struct{ stream, subject string }

	var (
		bus    *natsbustest.Fake
		mu     sync.Mutex
		counts map[countKey]uint64
		asked  []countKey
		tp     *telemetrytest.TestProvider
		logs   *bytes.Buffer
	)

	barrierMsg := func(subject, provenance, payloadJob string, deadline time.Time) *natsbus.Message {
		data, err := json.Marshal(natsbus.MaterializeBarrier{JobID: payloadJob, Deadline: deadline})
		Expect(err).NotTo(HaveOccurred())
		return &natsbus.Message{Subject: subject, Data: data,
			Metadata: natsbus.MessageMetadata{TenantID: provenance, NumDelivered: 1}}
	}

	barrier := func() *natsbustest.Sub {
		sub := consumerSub(bus, barrierConsumer)
		Expect(sub).NotTo(BeNil(), "no %s consumer", barrierConsumer)
		return sub
	}

	outcome := func(p natsbustest.Publication) natsbus.GraphOutcome {
		var o natsbus.GraphOutcome
		Expect(json.Unmarshal(p.Data, &o)).To(Succeed())
		return o
	}

	BeforeEach(func() {
		counts = map[countKey]uint64{}
		asked = nil
		bus = &natsbustest.Fake{SubjectMessageCountFunc: func(_ context.Context, stream, subject string) (uint64, error) {
			mu.Lock()
			defer mu.Unlock()
			k := countKey{stream, subject}
			asked = append(asked, k)
			return counts[k], nil
		}}
		var err error
		tp, err = telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = tp.Shutdown(context.Background()) })
		logs = &bytes.Buffer{}
		svc := graph.New(newMemGraph(context.Background(), tenantID), &mockVectorDB{}, bus,
			graph.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()),
			graph.WithLogger(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))))
		Expect(svc.Start(context.Background())).To(Succeed())
		DeferCleanup(func() { _ = svc.Stop(context.Background()) })
	})

	It("waits while the job still has materialization events queued", func() {
		counts[countKey{natsbus.MaterializeStream, stageSubject}] = 2

		err := bus.Deliver(barrierMsg(barrierSubj, tenantID, jobID, time.Now().Add(time.Hour)))

		Expect(err).To(HaveOccurred())
		Expect(err).NotTo(MatchError(natsbus.ErrPermanent))
		Expect(err.Error()).To(ContainSubstring("2 materialization events"))
		Expect(barrier().Naked()).To(Equal(1))
		Expect(bus.Published()).To(BeEmpty())
		Expect(eventCount(tp, "barrier", "waiting")).To(Equal(int64(1)))
	})

	It("publishes materialized once the queue is empty", func() {
		Expect(bus.Deliver(barrierMsg(barrierSubj, tenantID, jobID, time.Now().Add(time.Hour)))).To(Succeed())

		Expect(barrier().Acked()).To(Equal(1))
		Expect(asked).To(ContainElements(
			countKey{natsbus.MaterializeStream, stageSubject},
			countKey{natsbus.AuditEventsStream, failedEvents},
		))
		pubs := bus.Published()
		Expect(pubs).To(HaveLen(1))
		Expect(pubs[0].Subject).To(Equal("crosscodex.audit." + tenantID + ".events." + jobID + ".graph.materialized"))
		Expect(pubs[0].Durable).To(BeTrue())
		Expect(outcome(pubs[0])).To(Equal(natsbus.GraphOutcome{JobID: jobID, Outcome: natsbus.GraphOutcomeMaterialized}))
		Expect(eventCount(tp, "barrier", natsbus.GraphOutcomeMaterialized)).To(Equal(int64(1)))
	})

	It("publishes failed when the job has a failure event", func() {
		counts[countKey{natsbus.AuditEventsStream, failedEvents}] = 1

		Expect(bus.Deliver(barrierMsg(barrierSubj, tenantID, jobID, time.Now().Add(time.Hour)))).To(Succeed())

		Expect(barrier().Acked()).To(Equal(1))
		pubs := bus.Published()
		Expect(pubs).To(HaveLen(1))
		Expect(pubs[0].Subject).To(Equal(failedEvents))
		Expect(pubs[0].Durable).To(BeTrue())
		got := outcome(pubs[0])
		Expect(got.JobID).To(Equal(jobID))
		Expect(got.Outcome).To(Equal(natsbus.GraphOutcomeFailed))
		Expect(got.Error).To(ContainSubstring("see the earlier graph failure event"))
	})

	It("drops a barrier past its deadline", func() {
		err := bus.Deliver(barrierMsg(barrierSubj, tenantID, jobID, time.Now().Add(-time.Second)))

		Expect(err).To(MatchError(natsbus.ErrPermanent))
		Expect(err.Error()).To(ContainSubstring("deadline"))
		Expect(barrier().Termed()).To(Equal(1))
		Expect(asked).To(BeEmpty())
		Expect(bus.Published()).To(BeEmpty())
		Expect(eventCount(tp, "barrier", "expired")).To(Equal(int64(1)))
	})

	It("retries when the queue cannot be counted", func() {
		bus.SubjectMessageCountFunc = func(context.Context, string, string) (uint64, error) {
			return 0, fmt.Errorf("count: %w", natsbus.ErrConnectionClosed)
		}

		err := bus.Deliver(barrierMsg(barrierSubj, tenantID, jobID, time.Now().Add(time.Hour)))

		Expect(err).To(MatchError(natsbus.ErrConnectionClosed))
		Expect(err).NotTo(MatchError(natsbus.ErrPermanent))
		Expect(barrier().Naked()).To(Equal(1))
		Expect(bus.Published()).To(BeEmpty())
	})

	It("retries when the outcome is not stored", func() {
		bus.PublishDurableFunc = func(context.Context, string, []byte, map[string][]string) error {
			return fmt.Errorf("publish: %w: %w", natsbus.ErrPublishFailed, natsbus.ErrNotStored)
		}

		err := bus.Deliver(barrierMsg(barrierSubj, tenantID, jobID, time.Now().Add(time.Hour)))

		Expect(err).To(MatchError(natsbus.ErrNotStored))
		Expect(barrier().Naked()).To(Equal(1))
	})

	It("terms a barrier whose subject names another tenant than its provenance, counting nothing", func() {
		const other = "tenant-b"
		err := bus.Deliver(barrierMsg("crosscodex.materialize."+other+"."+jobID+".barrier", tenantID, jobID, time.Now().Add(time.Hour)))

		Expect(err).To(MatchError(natsbus.ErrPermanent))
		Expect(barrier().Termed()).To(Equal(1))
		Expect(asked).To(BeEmpty())
		Expect(bus.Published()).To(BeEmpty())
		Expect(eventCount(tp, "barrier", "rejected")).To(Equal(int64(1)))
		Expect(logs.String()).To(ContainSubstring("level=WARN"))
	})

	DescribeTable("terms a barrier it can never answer, without counting or publishing",
		func(msg func() *natsbus.Message) {
			Expect(barrier().Handle(msg())).To(MatchError(natsbus.ErrPermanent))

			Expect(barrier().Termed()).To(Equal(1))
			Expect(asked).To(BeEmpty())
			Expect(bus.Published()).To(BeEmpty())
			Expect(eventCount(tp, "barrier", "skipped")).To(Equal(int64(1)))
		},
		Entry("a malformed subject", func() *natsbus.Message {
			return barrierMsg("crosscodex.materialize."+tenantID+".barrier", tenantID, jobID, time.Now().Add(time.Hour))
		}),
		Entry("a payload naming another job than the subject", func() *natsbus.Message {
			return barrierMsg(barrierSubj, tenantID, "job-2", time.Now().Add(time.Hour))
		}),
		Entry("a payload without a deadline", func() *natsbus.Message {
			return barrierMsg(barrierSubj, tenantID, jobID, time.Time{})
		}),
		Entry("a payload that is not JSON", func() *natsbus.Message {
			m := barrierMsg(barrierSubj, tenantID, jobID, time.Now().Add(time.Hour))
			m.Data = []byte("{not json")
			return m
		}),
	)

	It("never reports materialized while an event of the job is still queued", func() {
		counts[countKey{natsbus.MaterializeStream, stageSubject}] = 1
		Expect(bus.Deliver(barrierMsg(barrierSubj, tenantID, jobID, time.Now().Add(time.Hour)))).NotTo(Succeed())

		counts[countKey{natsbus.MaterializeStream, stageSubject}] = 0
		Expect(bus.Deliver(barrierMsg(barrierSubj, tenantID, jobID, time.Now().Add(time.Hour)))).To(Succeed())

		pubs := bus.Published()
		Expect(pubs).To(HaveLen(1), "only the delivery that found the queue empty publishes")
		Expect(outcome(pubs[0]).Outcome).To(Equal(natsbus.GraphOutcomeMaterialized))
	})
})
