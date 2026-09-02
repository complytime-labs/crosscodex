package retention_test

import (
	"context"
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/retention"
)

// auditFakeBus is a fake natsbus.Client that captures Publish calls.
// Named distinctly from other fakes in this package to avoid redeclaration.
type auditFakeBus struct {
	subject string
	data    []byte
	calls   int
}

func (b *auditFakeBus) Publish(_ context.Context, subject string, data []byte) error {
	b.calls++
	b.subject = subject
	b.data = data
	return nil
}

func (b *auditFakeBus) PublishWithHeaders(_ context.Context, _ string, _ []byte, _ map[string][]string) error {
	return nil
}

func (b *auditFakeBus) Subscribe(_ context.Context, _ string, _ natsbus.MessageHandler) (natsbus.Subscription, error) {
	return nil, nil
}

func (b *auditFakeBus) QueueSubscribe(_ context.Context, _, _ string, _ natsbus.MessageHandler) (natsbus.Subscription, error) {
	return nil, nil
}

func (b *auditFakeBus) CreateStream(_ context.Context, _ natsbus.StreamConfig) error { return nil }
func (b *auditFakeBus) DeleteStream(_ context.Context, _ string) error               { return nil }
func (b *auditFakeBus) AuditStreamRetention(_ context.Context) (map[string]time.Duration, error) {
	return nil, nil
}
func (b *auditFakeBus) Close() error { return nil }

var _ = Describe("AuditPublisher", func() {
	var (
		bus       *auditFakeBus
		publisher retention.AuditPublisher
		ctx       context.Context
		record    retention.AuditRecord
	)

	BeforeEach(func() {
		bus = &auditFakeBus{}
		publisher = retention.NewAuditPublisher(bus)
		ctx = context.Background()
		record = retention.AuditRecord{
			Action:    retention.ActionArchive,
			Actor:     "engine",
			TenantID:  "tenant1",
			JobID:     "job-abc",
			DataClass: "job_results",
			Outcome:   "ok",
			Detail:    "archived 1 item",
			At:        time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		}
	})

	Describe("Publish", func() {
		Context("with a valid record", func() {
			It("publishes to the expected decisions subject", func() {
				expectedSubject, err := natsbus.AuditSubject(record.TenantID, natsbus.AuditDecisions, record.JobID)
				Expect(err).NotTo(HaveOccurred())

				Expect(publisher.Publish(ctx, record)).To(Succeed())
				Expect(bus.subject).To(Equal(expectedSubject))
				Expect(bus.calls).To(Equal(1))
			})

			It("publishes a JSON body that round-trips the record", func() {
				Expect(publisher.Publish(ctx, record)).To(Succeed())

				var got retention.AuditRecord
				Expect(json.Unmarshal(bus.data, &got)).To(Succeed())
				Expect(got).To(Equal(record))
			})
		})

		Context("with an empty JobID", func() {
			BeforeEach(func() {
				record.JobID = ""
			})

			It("returns an error without calling the bus", func() {
				Expect(publisher.Publish(ctx, record)).To(HaveOccurred())
				Expect(bus.calls).To(Equal(0))
			})
		})

		Context("with a JobID containing a NATS delimiter", func() {
			BeforeEach(func() {
				record.JobID = "job.bad"
			})

			It("returns an error without calling the bus", func() {
				Expect(publisher.Publish(ctx, record)).To(HaveOccurred())
				Expect(bus.calls).To(Equal(0))
			})
		})
	})
})
