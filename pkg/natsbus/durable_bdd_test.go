package natsbus_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// provenanceHeaders returns a full, valid provenance header set for data.
func provenanceHeaders(tenantID string, data []byte) nats.Header {
	return nats.Header{
		natsbus.HeaderTraceID:       []string{"0af7651916cd43dd8448eb211c80319c"}, // DevSkim: ignore DS173237 - OTel trace ID test fixture, not a credential
		natsbus.HeaderSpanID:        []string{"b7ad6b7169203331"},
		natsbus.HeaderTenantID:      []string{tenantID},
		natsbus.HeaderTimestamp:     []string{time.Now().UTC().Format(time.RFC3339Nano)},
		natsbus.HeaderContentSHA256: []string{natsbus.ContentHash(data)},
	}
}

// stubMsg is a jetstream.Msg recording how it was acknowledged. Methods it
// does not define panic through the nil embedded interface.
type stubMsg struct {
	jetstream.Msg
	subject string
	data    []byte
	headers nats.Header
	ackErr  error
	calls   []string
}

func (m *stubMsg) Subject() string      { return m.subject }
func (m *stubMsg) Data() []byte         { return m.data }
func (m *stubMsg) Headers() nats.Header { return m.headers }
func (m *stubMsg) Metadata() (*jetstream.MsgMetadata, error) {
	md := &jetstream.MsgMetadata{Stream: "S", NumDelivered: 2, Timestamp: time.Now()}
	md.Sequence.Stream = 9
	return md, nil
}
func (m *stubMsg) Ack() error { m.calls = append(m.calls, "ack"); return m.ackErr }
func (m *stubMsg) NakWithDelay(time.Duration) error {
	m.calls = append(m.calls, "nak")
	return m.ackErr
}
func (m *stubMsg) Term() error { m.calls = append(m.calls, "term"); return m.ackErr }

var _ = Describe("ConsumerConfig.Validate", func() {
	DescribeTable("rejects what the server would misread, naming the field",
		func(cfg natsbus.ConsumerConfig, field string) {
			err := cfg.Validate()
			Expect(err).To(MatchError(natsbus.ErrSubscribeFailed))
			Expect(err).To(MatchError(ContainSubstring(field)))
		},
		Entry("no stream", natsbus.ConsumerConfig{Durable: "d", MaxDeliver: 5}, "Stream"),
		Entry("no durable name", natsbus.ConsumerConfig{Stream: "S", MaxDeliver: 5}, "Durable"),
		Entry("MaxDeliver unset (0) would retry forever", natsbus.ConsumerConfig{Stream: "S", Durable: "d"}, "MaxDeliver"),
		Entry("MaxDeliver below -1", natsbus.ConsumerConfig{Stream: "S", Durable: "d", MaxDeliver: -2}, "MaxDeliver"),
	)

	DescribeTable("accepts unlimited and positive MaxDeliver",
		func(maxDeliver int) {
			Expect(natsbus.ConsumerConfig{Stream: "S", Durable: "d", MaxDeliver: maxDeliver}.Validate()).To(Succeed())
		},
		Entry("-1", -1),
		Entry("1", 1),
	)
})

var _ = Describe("core subscriber tenant validation", func() {
	It("drops a message with a well-formed hash but an invalid tenant header, and logs why", func() {
		logs := &bytes.Buffer{}
		called := false
		handler := natsbus.ExportWrapHandlerWithLogger(slog.New(slog.NewTextHandler(logs, nil)),
			func(context.Context, *natsbus.Message) error { called = true; return nil })
		data := []byte("payload")

		handler(&nats.Msg{Subject: "crosscodex.work.x.classify.j", Data: data, Header: provenanceHeaders("Not A Tenant!", data)})

		Expect(called).To(BeFalse(), "a message with an invalid tenant reached the handler")
		Expect(logs.String()).To(ContainSubstring("rejecting message: invalid tenant"))
	})
})

var _ = Describe("durable message handling", func() {
	var (
		logs  *bytes.Buffer
		tp    *telemetrytest.TestProvider
		cfg   = natsbus.ConsumerConfig{Stream: "S", Durable: "d", MaxDeliver: 5, RedeliverDelay: time.Millisecond}
		runIt = func(msg *stubMsg, handler natsbus.MessageHandler) {
			natsbus.ExportHandleDurable(slog.New(slog.NewTextHandler(logs, nil)),
				tp.TracerProvider().Tracer("t"), tp.MeterProvider().Meter("m"), cfg, handler, msg)
		}
		outcomes = func() map[string]int64 {
			m := telemetrytest.FindMetric(tp.GetMetrics(), "natsbus.process.total")
			if m == nil {
				return nil
			}
			got := map[string]int64{}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				v, _ := dp.Attributes.Value("outcome")
				got[v.AsString()] += dp.Value
			}
			return got
		}
	)

	BeforeEach(func() {
		logs = &bytes.Buffer{}
		var err error
		tp, err = telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = tp.Shutdown(context.Background()) })
	})

	It("gives the handler a context carrying the message's tenant", func() {
		data := []byte("payload")
		var got string
		msg := &stubMsg{subject: "s", data: data, headers: provenanceHeaders("acme-corp", data)}
		runIt(msg, func(ctx context.Context, _ *natsbus.Message) error {
			got, _ = tenant.FromContext(ctx)
			return nil
		})
		Expect(got).To(Equal("acme-corp"))
		Expect(msg.calls).To(Equal([]string{"ack"}))
	})

	DescribeTable("terminates a message that fails verification, traced and counted, never calling the handler",
		func(tenantID string, mutate func(*stubMsg), logged string) {
			data := []byte("payload")
			msg := &stubMsg{subject: "s", data: data, headers: provenanceHeaders(tenantID, data)}
			mutate(msg)
			called := false
			runIt(msg, func(context.Context, *natsbus.Message) error { called = true; return nil })

			Expect(called).To(BeFalse())
			Expect(msg.calls).To(Equal([]string{"term"}))
			Expect(logs.String()).To(ContainSubstring("terminating message: " + logged))
			Expect(outcomes()).To(Equal(map[string]int64{"term": 1}))
			span := telemetrytest.FindSpan(tp.GetSpans(), "natsbus.process")
			Expect(span).NotTo(BeNil(), "a rejected message must still be traced")
			Expect(span.Status().Code.String()).To(Equal("Error"))
		},
		Entry("missing provenance", "acme-corp", func(m *stubMsg) { m.headers = nats.Header{} }, "missing provenance"),
		Entry("invalid tenant", "Not A Tenant!", func(*stubMsg) {}, "invalid tenant"),
		Entry("content hash mismatch", "acme-corp", func(m *stubMsg) { m.data = []byte("tampered") }, "content hash mismatch"),
	)

	DescribeTable("counts a failed acknowledgement under <outcome>_failed",
		func(handlerErr error, want string) {
			data := []byte("payload")
			msg := &stubMsg{subject: "s", data: data, headers: provenanceHeaders("acme-corp", data), ackErr: errors.New("conn lost")}
			runIt(msg, func(context.Context, *natsbus.Message) error { return handlerErr })
			Expect(outcomes()).To(Equal(map[string]int64{want: 1}))
		},
		Entry("ack", nil, "ack_failed"),
		Entry("nak", errors.New("transient"), "nak_failed"),
		Entry("term", natsbus.ErrPermanent, "term_failed"),
	)
})
