package retention_test

import (
	"context"
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/natsbus/natsbustest"
	"github.com/complytime-labs/crosscodex/pkg/retention"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

var _ = Describe("AuditPublisher", func() {
	var (
		bus       *natsbustest.Fake
		publisher retention.AuditPublisher
		ctx       context.Context
		record    retention.AuditRecord
	)

	BeforeEach(func() {
		bus = &natsbustest.Fake{}
		publisher = retention.NewAuditPublisher(bus)
		// The real client refuses a publish whose context carries no tenant.
		var err error
		ctx, err = tenant.WithTenant(context.Background(), "tenant1")
		Expect(err).NotTo(HaveOccurred())
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
				Expect(bus.Published()).To(HaveLen(1))
				Expect(bus.Published()[0].Subject).To(Equal(expectedSubject))
			})

			It("publishes a JSON body that round-trips the record", func() {
				Expect(publisher.Publish(ctx, record)).To(Succeed())

				var got retention.AuditRecord
				Expect(json.Unmarshal(bus.Published()[0].Data, &got)).To(Succeed())
				Expect(got).To(Equal(record))
			})
		})

		Context("with an empty JobID", func() {
			BeforeEach(func() {
				record.JobID = ""
			})

			It("returns an error without calling the bus", func() {
				Expect(publisher.Publish(ctx, record)).To(HaveOccurred())
				Expect(bus.Published()).To(BeEmpty())
			})
		})

		Context("with a JobID containing a NATS delimiter", func() {
			BeforeEach(func() {
				record.JobID = "job.bad"
			})

			It("returns an error without calling the bus", func() {
				Expect(publisher.Publish(ctx, record)).To(HaveOccurred())
				Expect(bus.Published()).To(BeEmpty())
			})
		})
	})
})
