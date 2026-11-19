package natsbus

import (
	"context"
	"errors"
	"slices"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

// fakeStream is a minimal jetstream.Stream whose Info returns a configured
// MaxAge (or a configured error). Unused methods are inherited from the
// embedded nil interface and would panic if called.
type fakeStream struct {
	jetstream.Stream
	maxAge  time.Duration
	infoErr error
	// cfg, when set, is the full live configuration Info returns (maxAge is
	// then ignored).
	cfg *jetstream.StreamConfig
}

func (s *fakeStream) Info(_ context.Context, _ ...jetstream.StreamInfoOpt) (*jetstream.StreamInfo, error) {
	if s.infoErr != nil {
		return nil, s.infoErr
	}
	if s.cfg != nil {
		return &jetstream.StreamInfo{Config: *s.cfg}, nil
	}
	return &jetstream.StreamInfo{Config: jetstream.StreamConfig{MaxAge: s.maxAge}}, nil
}

// fakeJetStream is a minimal jetstream.JetStream: Stream, CreateStream and
// the update calls are exercised. Unused methods are inherited from the
// embedded nil interface. created and updated record every config written, so
// a spec can prove a refused startup changed nothing.
type fakeJetStream struct {
	jetstream.JetStream
	streams   map[string]*fakeStream
	streamErr map[string]error
	created   []jetstream.StreamConfig
	updated   []jetstream.StreamConfig
}

func (f *fakeJetStream) CreateStream(_ context.Context, cfg jetstream.StreamConfig) (jetstream.Stream, error) {
	f.created = append(f.created, cfg)
	if f.streams == nil {
		f.streams = map[string]*fakeStream{}
	}
	f.streams[cfg.Name] = &fakeStream{cfg: &cfg}
	return f.streams[cfg.Name], nil
}

func (f *fakeJetStream) UpdateStream(_ context.Context, cfg jetstream.StreamConfig) (jetstream.Stream, error) {
	f.updated = append(f.updated, cfg)
	return nil, nil
}

func (f *fakeJetStream) CreateOrUpdateStream(_ context.Context, cfg jetstream.StreamConfig) (jetstream.Stream, error) {
	f.updated = append(f.updated, cfg)
	return nil, nil
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

var _ = Describe("ensureStreams", func() {
	var streamsCfg config.NATSStreamsConfig

	BeforeEach(func() {
		streamsCfg = config.NATSStreamsConfig{
			AuditLLMRetention:    90 * 24 * time.Hour,
			AuditEventsRetention: 30 * 24 * time.Hour,
			MaterializeRetention: 168 * time.Hour,
		}
	})

	// liveFrom returns fakes holding exactly the configured streams, which
	// each spec then perturbs.
	liveFrom := func() *fakeJetStream {
		js := &fakeJetStream{streams: map[string]*fakeStream{}}
		for _, sc := range streamConfigs(streamsCfg) {
			jsCfg := jetStreamConfig(sc)
			js.streams[sc.Name] = &fakeStream{cfg: &jsCfg}
		}
		return js
	}

	It("creates a stream that does not exist", func() {
		js := &fakeJetStream{}
		c := &client{js: js}

		Expect(c.ensureStreams(context.Background(), streamsCfg)).To(Succeed())

		names := []string{}
		for _, cfg := range js.created {
			names = append(names, cfg.Name)
		}
		Expect(names).To(ConsistOf("AUDIT_LLM", "AUDIT_DECISIONS", "AUDIT_EVENTS", "MATERIALIZE"))
		Expect(js.updated).To(BeEmpty())
	})

	It("creates MATERIALIZE as a file-backed work queue with the configured retention", func() {
		js := &fakeJetStream{}
		c := &client{js: js}
		Expect(c.ensureStreams(context.Background(), streamsCfg)).To(Succeed())

		var got jetstream.StreamConfig
		for _, cfg := range js.created {
			if cfg.Name == MaterializeStream {
				got = cfg
			}
		}
		Expect(got.Retention).To(Equal(jetstream.WorkQueuePolicy))
		Expect(got.Storage).To(Equal(jetstream.FileStorage))
		Expect(got.MaxAge).To(Equal(168 * time.Hour))
		Expect(got.Subjects).To(ConsistOf(
			"crosscodex.pipeline.*.*.stage.completed",
			"crosscodex.catalog.*.*.parsed",
			"crosscodex.materialize.*.*.barrier",
		))
	})

	It("accepts a live stream that matches, creating and updating nothing", func() {
		js := liveFrom()
		c := &client{js: js}

		Expect(c.ensureStreams(context.Background(), streamsCfg)).To(Succeed())
		Expect(js.created).To(BeEmpty())
		Expect(js.updated).To(BeEmpty())
	})

	It("accepts live subjects in a different order", func() {
		js := liveFrom()
		subjects := js.streams[MaterializeStream].cfg.Subjects
		slices.Reverse(subjects)
		c := &client{js: js}

		Expect(c.ensureStreams(context.Background(), streamsCfg)).To(Succeed())
	})

	DescribeTable("refuses a live stream that differs, naming the field and both values",
		func(stream string, mutate func(*jetstream.StreamConfig), wantParts ...string) {
			js := liveFrom()
			mutate(js.streams[stream].cfg)
			c := &client{js: js}

			err := c.ensureStreams(context.Background(), streamsCfg)

			Expect(err).To(MatchError(ErrStreamDrift))
			Expect(err.Error()).To(ContainSubstring(stream))
			for _, part := range wantParts {
				Expect(err.Error()).To(ContainSubstring(part))
			}
			Expect(err.Error()).To(ContainSubstring("nats stream edit " + stream))
			Expect(js.updated).To(BeEmpty(), "a refused stream must not be updated")
			Expect(js.created).To(BeEmpty())
		},
		Entry("max_age", "AUDIT_LLM",
			func(c *jetstream.StreamConfig) { c.MaxAge = time.Hour },
			"max_age", "live 1h0m0s", "configured 2160h0m0s"),
		Entry("subjects", "AUDIT_EVENTS",
			func(c *jetstream.StreamConfig) { c.Subjects = []string{"crosscodex.audit.*.other.>"} },
			"subjects", "crosscodex.audit.*.other.>", "crosscodex.audit.*.events.>"),
		Entry("retention policy", "MATERIALIZE",
			func(c *jetstream.StreamConfig) { c.Retention = jetstream.LimitsPolicy },
			"retention", "live Limits", "configured WorkQueue"),
		Entry("storage", "AUDIT_DECISIONS",
			func(c *jetstream.StreamConfig) { c.Storage = jetstream.MemoryStorage },
			"storage", "live Memory", "configured File"),
		Entry("replicas", "AUDIT_DECISIONS",
			func(c *jetstream.StreamConfig) { c.Replicas = 3 },
			"replicas", "live 3", "configured 1"),
	)

	It("names every differing field at once", func() {
		js := liveFrom()
		js.streams["AUDIT_LLM"].cfg.MaxAge = time.Hour
		js.streams["AUDIT_LLM"].cfg.Storage = jetstream.MemoryStorage
		c := &client{js: js}

		err := c.ensureStreams(context.Background(), streamsCfg)

		Expect(err).To(MatchError(ErrStreamDrift))
		Expect(err.Error()).To(ContainSubstring("max_age"))
		Expect(err.Error()).To(ContainSubstring("storage"))
	})

	It("propagates a lookup failure without creating anything", func() {
		boom := errors.New("connection reset")
		js := &fakeJetStream{streamErr: map[string]error{"AUDIT_LLM": boom}}
		c := &client{js: js}

		err := c.ensureStreams(context.Background(), streamsCfg)

		Expect(err).To(MatchError(boom))
		Expect(js.created).To(BeEmpty())
	})
})

var _ = Describe("AuditStreamNames", func() {
	It("does not include MATERIALIZE, which is not an audit stream", func() {
		Expect(AuditStreamNames()).NotTo(ContainElement(MaterializeStream))
	})
})
