package natsbus

import (
	"context"
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nats-io/nats.go/jetstream"
)

// fakeStream is a minimal jetstream.Stream whose Info returns a configured
// MaxAge (or a configured error). Unused methods are inherited from the
// embedded nil interface and would panic if called.
type fakeStream struct {
	jetstream.Stream
	maxAge  time.Duration
	infoErr error
}

func (s *fakeStream) Info(_ context.Context, _ ...jetstream.StreamInfoOpt) (*jetstream.StreamInfo, error) {
	if s.infoErr != nil {
		return nil, s.infoErr
	}
	return &jetstream.StreamInfo{Config: jetstream.StreamConfig{MaxAge: s.maxAge}}, nil
}

// fakeJetStream is a minimal jetstream.JetStream: only Stream is exercised.
// Unused methods are inherited from the embedded nil interface.
type fakeJetStream struct {
	jetstream.JetStream
	streams   map[string]*fakeStream
	streamErr map[string]error
}

func (f *fakeJetStream) Stream(_ context.Context, name string) (jetstream.Stream, error) {
	if err, ok := f.streamErr[name]; ok {
		return nil, err
	}
	s, ok := f.streams[name]
	if !ok {
		return nil, jetstream.ErrStreamNotFound
	}
	return s, nil
}

var _ = Describe("AuditStreamRetention", func() {
	It("reports all present streams", func() {
		c := &client{js: &fakeJetStream{streams: map[string]*fakeStream{
			"AUDIT_LLM":       {maxAge: 90 * 24 * time.Hour},
			"AUDIT_DECISIONS": {maxAge: 0},
			"AUDIT_EVENTS":    {maxAge: 30 * 24 * time.Hour},
		}}}

		live, err := c.AuditStreamRetention(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(live).To(HaveLen(3))
		Expect(live["AUDIT_LLM"]).To(Equal(90 * 24 * time.Hour))
		Expect(live["AUDIT_EVENTS"]).To(Equal(30 * 24 * time.Hour))
	})

	It("omits a stream not found rather than erroring", func() {
		// AUDIT_EVENTS is absent (Stream returns ErrStreamNotFound); it must be
		// omitted from the map, NOT surfaced as an error, so VerifyStreamRetention
		// reports it as Live=0/OK=false.
		c := &client{js: &fakeJetStream{streams: map[string]*fakeStream{
			"AUDIT_LLM":       {maxAge: 90 * 24 * time.Hour},
			"AUDIT_DECISIONS": {maxAge: 0},
		}}}

		live, err := c.AuditStreamRetention(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(live).NotTo(HaveKey("AUDIT_EVENTS"))
		Expect(live).To(HaveLen(2))
	})

	It("omits a stream whose Info reports not-found rather than erroring", func() {
		// A stream that exists at lookup but reports not-found on Info (deleted
		// concurrently) is likewise omitted rather than failing the read.
		c := &client{js: &fakeJetStream{streams: map[string]*fakeStream{
			"AUDIT_LLM":       {maxAge: 90 * 24 * time.Hour},
			"AUDIT_DECISIONS": {maxAge: 0},
			"AUDIT_EVENTS":    {infoErr: jetstream.ErrStreamNotFound},
		}}}

		live, err := c.AuditStreamRetention(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(live).NotTo(HaveKey("AUDIT_EVENTS"))
	})

	It("propagates a genuine Stream error", func() {
		boom := errors.New("connection reset")
		c := &client{js: &fakeJetStream{
			streams:   map[string]*fakeStream{"AUDIT_DECISIONS": {maxAge: 0}, "AUDIT_EVENTS": {maxAge: 0}},
			streamErr: map[string]error{"AUDIT_LLM": boom},
		}}

		live, err := c.AuditStreamRetention(context.Background())
		Expect(err).To(HaveOccurred())
		Expect(errors.Is(err, boom)).To(BeTrue())
		Expect(live).To(BeNil())
	})

	It("propagates a genuine Info error", func() {
		boom := errors.New("request timeout")
		c := &client{js: &fakeJetStream{streams: map[string]*fakeStream{
			"AUDIT_LLM":       {infoErr: boom},
			"AUDIT_DECISIONS": {maxAge: 0},
			"AUDIT_EVENTS":    {maxAge: 0},
		}}}

		live, err := c.AuditStreamRetention(context.Background())
		Expect(err).To(HaveOccurred(), "genuine Info error must propagate, got live=%v", live)
		Expect(errors.Is(err, boom)).To(BeTrue())
	})
})
