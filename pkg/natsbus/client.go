package natsbus

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// client implements the Client interface for both embedded and external NATS.
type client struct {
	conn     *nats.Conn
	js       jetstream.JetStream
	embedded *embeddedServer
	opts     clientOptions

	// ackTimeout bounds PublishDurable's wait for the JetStream PubAck
	// (nats.publish_ack_timeout).
	ackTimeout time.Duration

	mu     sync.Mutex
	closed bool

	// Telemetry (optional, nil-safe)
	tracer         trace.Tracer
	meter          metric.Meter
	publishCounter metric.Int64Counter
	publishLatency metric.Int64Histogram
	processCounter metric.Int64Counter
	processLatency metric.Int64Histogram
}

// connectOptions builds the nats.go options shared by New and DialExternal.
func connectOptions(cfg config.NATSConfig, o clientOptions) []nats.Option {
	natsOpts := []nats.Option{
		nats.Timeout(o.connectTimeout),
		nats.ReconnectWait(o.reconnectWait),
		nats.MaxReconnects(o.maxReconnects),
		nats.Name("crosscodex"),
	}
	if o.tlsConfig != nil {
		natsOpts = append(natsOpts, nats.Secure(o.tlsConfig))
	} else if cfg.TLS {
		natsOpts = append(natsOpts, nats.Secure())
	}
	return natsOpts
}

// New creates a new NATS Client. When cfg.URL is empty, an embedded
// NATS server is started. When cfg.URL is set, the client connects
// to the external server.
//
// Three audit streams (AUDIT_LLM, AUDIT_DECISIONS, AUDIT_EVENTS) are
// created or updated on startup, unless WithExistingStreams is set, in which
// case they must already exist and are left unchanged.
func New(cfg config.NATSConfig, opts ...Option) (Client, error) {
	o := defaultClientOptions()
	for _, opt := range opts {
		opt(&o)
	}
	if o.existingStreams && isEmbeddedMode(cfg.URL) {
		return nil, fmt.Errorf("%w: this process attaches to the daemon's NATS server and must not start an embedded one, which would share the daemon's JetStream store directory. Set nats.url to the daemon's NATS server", ErrExternalURLRequired)
	}

	c := &client{
		opts:       o,
		tracer:     o.tracer,
		meter:      o.meter,
		ackTimeout: cfg.PublishAckTimeout,
	}

	var connectURL string

	if isEmbeddedMode(cfg.URL) {
		es, err := startEmbedded(cfg.Embedded, o.logger)
		if err != nil {
			return nil, err
		}
		c.embedded = es
		connectURL = es.clientURL()
	} else {
		connectURL = cfg.URL
	}

	nc, err := nats.Connect(connectURL, connectOptions(cfg, o)...)
	if err != nil {
		if c.embedded != nil {
			c.embedded.shutdown()
		}
		return nil, fmt.Errorf("connecting to NATS at %s: %w", connectURL, err)
	}
	c.conn = nc

	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		if c.embedded != nil {
			c.embedded.shutdown()
		}
		return nil, fmt.Errorf("creating JetStream context: %w", err)
	}
	c.js = js

	if o.existingStreams {
		err = c.verifyStreams(context.Background())
	} else {
		err = c.ensureStreams(context.Background(), cfg.Streams)
	}
	if err != nil {
		nc.Close()
		if c.embedded != nil {
			c.embedded.shutdown()
		}
		return nil, err
	}

	// Instrument creation logs warnings on failure rather than returning an
	// error. NATS connectivity is operationally critical; telemetry degradation
	// must not prevent the messaging client from starting. The nil-guarded
	// recording sites handle missing instruments gracefully.
	if o.meter != nil {
		var err error
		c.publishCounter, err = o.meter.Int64Counter("natsbus.publish.total",
			metric.WithDescription("Total messages published"))
		if err != nil {
			o.logger.Warn("failed to create publish counter", "error", err)
		}
		c.publishLatency, err = o.meter.Int64Histogram("natsbus.publish.duration_ms",
			metric.WithDescription("Publish operation duration in milliseconds"))
		if err != nil {
			o.logger.Warn("failed to create publish latency histogram", "error", err)
		}
		c.processCounter, err = o.meter.Int64Counter("natsbus.process.total",
			metric.WithDescription("Total messages processed by subscriber handlers"))
		if err != nil {
			o.logger.Warn("failed to create process counter", "error", err)
		}
		c.processLatency, err = o.meter.Int64Histogram("natsbus.process.duration_ms",
			metric.WithDescription("Subscriber handler execution duration in milliseconds"))
		if err != nil {
			o.logger.Warn("failed to create process latency histogram", "error", err)
		}
	}

	o.logger.Info("NATS client connected",
		"url", connectURL,
		"embedded", isEmbeddedMode(cfg.URL),
	)

	return c, nil
}

// DialExternal connects to the external NATS server at cfg.URL with the same
// TLS and timeout options as New, but creates no streams and starts no
// embedded server. Admin one-shots use it: a separate process cannot reach
// the daemon's embedded server, and starting a second embedded server on
// the same store directory would corrupt JetStream (nats-server does not
// lock its store directory). Restore also needs the audit streams absent,
// which New's stream provisioning would prevent.
func DialExternal(cfg config.NATSConfig, opts ...Option) (*nats.Conn, error) {
	if isEmbeddedMode(cfg.URL) {
		return nil, fmt.Errorf("%w: JetStream backup and restore need an external NATS server; set nats.url to it", ErrExternalURLRequired)
	}
	o := defaultClientOptions()
	for _, opt := range opts {
		opt(&o)
	}
	nc, err := nats.Connect(cfg.URL, connectOptions(cfg, o)...)
	if err != nil {
		return nil, fmt.Errorf("connecting to NATS at %s: %w", cfg.URL, err)
	}
	return nc, nil
}

// AuditStreamNames returns the audit stream names in creation order.
func AuditStreamNames() []string {
	configs := auditStreamConfigs(config.NATSStreamsConfig{})
	names := make([]string, 0, len(configs))
	for _, sc := range configs {
		names = append(names, sc.Name)
	}
	return names
}

// Publish sends a message with automatically injected provenance headers.
func (c *client) Publish(ctx context.Context, subject string, data []byte) error {
	return c.PublishWithHeaders(ctx, subject, data, nil)
}

// PublishWithHeaders sends a message with user headers merged with
// provenance headers. Provenance headers take precedence.
func (c *client) PublishWithHeaders(ctx context.Context, subject string, data []byte, headers map[string][]string) error {
	start := time.Now()
	ctx, span := c.startSpan(ctx, "natsbus.Publish")
	defer span.End()

	msg, err := c.buildMessage(ctx, span, subject, data, headers)
	if err != nil {
		return err
	}
	if err := c.conn.PublishMsg(msg); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("publish to %s: %w: %w", subject, ErrPublishFailed, err)
	}
	c.recordPublish(ctx, start)
	span.SetStatus(codes.Ok, "")
	return nil
}

// PublishDurable publishes through JetStream and waits for the stream's
// acknowledgement under the configured publish ack timeout. A subject that
// no stream captures yields no acknowledgement and so fails.
func (c *client) PublishDurable(ctx context.Context, subject string, data []byte, headers map[string][]string) error {
	start := time.Now()
	ctx, span := c.startSpan(ctx, "natsbus.PublishDurable")
	defer span.End()

	msg, err := c.buildMessage(ctx, span, subject, data, headers)
	if err != nil {
		return err
	}
	if c.ackTimeout <= 0 {
		// Programmatic configs bypass pkg/config validation; fail closed
		// rather than publish under an already-expired deadline.
		span.SetStatus(codes.Error, "publish ack timeout not positive")
		return fmt.Errorf("publish to %s: %w: %w: nats.publish_ack_timeout is %s; set it to a positive duration such as 10s",
			subject, ErrPublishFailed, ErrNotStored, c.ackTimeout)
	}

	ackCtx, cancel := context.WithTimeout(ctx, c.ackTimeout)
	defer cancel()
	if _, err := c.js.PublishMsg(ackCtx, msg); err != nil {
		span.SetStatus(codes.Error, err.Error())
		if errors.Is(err, jetstream.ErrNoStreamResponse) {
			return fmt.Errorf("publish to %s: %w: %w: no JetStream stream captures this subject, so the message was not stored; create a stream whose subjects include it: %w",
				subject, ErrPublishFailed, ErrNotStored, err)
		}
		return fmt.Errorf("publish to %s: %w: %w: %w", subject, ErrPublishFailed, ErrNotStored, err)
	}
	c.recordPublish(ctx, start)
	span.SetStatus(codes.Ok, "")
	return nil
}

// buildMessage applies the checks and header merge common to every publish:
// the connection must be open and ctx must carry a tenant. Provenance
// headers override user headers of the same name. On error it marks span
// failed.
func (c *client) buildMessage(ctx context.Context, span trace.Span, subject string, data []byte, headers map[string][]string) (*nats.Msg, error) {
	span.SetAttributes(attribute.String("messaging.subject", subject))

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		span.SetStatus(codes.Error, "connection closed")
		return nil, fmt.Errorf("publish to %s: %w", subject, ErrConnectionClosed)
	}
	c.mu.Unlock()

	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		span.SetStatus(codes.Error, "tenant required")
		return nil, fmt.Errorf("publish to %s: tenant required: %w", subject, err)
	}
	span.SetAttributes(attribute.String("tenant.id", tenantID))

	provHeaders := injectProvenance(ctx, data, tenantID)
	return &nats.Msg{
		Subject: subject,
		Data:    data,
		Header:  nats.Header(mergeHeaders(headers, provHeaders)),
	}, nil
}

// recordPublish records the publish counter and latency for a successful
// publish that began at start.
func (c *client) recordPublish(ctx context.Context, start time.Time) {
	if c.publishCounter != nil {
		c.publishCounter.Add(ctx, 1)
	}
	if c.publishLatency != nil {
		c.publishLatency.Record(ctx, time.Since(start).Milliseconds())
	}
}

// Subscribe creates a subscription to the specified subject.
func (c *client) Subscribe(ctx context.Context, subject string, handler MessageHandler) (Subscription, error) {
	_, span := c.startSpan(ctx, "natsbus.Subscribe")
	defer span.End()
	span.SetAttributes(attribute.String("messaging.subject", subject))

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		span.SetStatus(codes.Error, "connection closed")
		return nil, fmt.Errorf("subscribe to %s: %w", subject, ErrConnectionClosed)
	}
	c.mu.Unlock()

	sub, err := c.conn.Subscribe(subject, c.wrapHandler(handler))
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("subscribe to %s: %w: %w", subject, ErrSubscribeFailed, err)
	}
	span.SetStatus(codes.Ok, "")
	return &subscription{sub: sub}, nil
}

// QueueSubscribe creates a queue group subscription for work distribution.
func (c *client) QueueSubscribe(ctx context.Context, subject, queue string, handler MessageHandler) (Subscription, error) {
	_, span := c.startSpan(ctx, "natsbus.QueueSubscribe")
	defer span.End()
	span.SetAttributes(
		attribute.String("messaging.subject", subject),
		attribute.String("messaging.queue", queue),
	)

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		span.SetStatus(codes.Error, "connection closed")
		return nil, fmt.Errorf("queue subscribe to %s: %w", subject, ErrConnectionClosed)
	}
	c.mu.Unlock()

	sub, err := c.conn.QueueSubscribe(subject, queue, c.wrapHandler(handler))
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("queue subscribe to %s queue %s: %w: %w", subject, queue, ErrSubscribeFailed, err)
	}
	span.SetStatus(codes.Ok, "")
	return &subscription{sub: sub}, nil
}

// PublishWorkerProgress publishes a bare tick on WorkerProgressSubject. It
// deliberately bypasses PublishWithHeaders: injecting provenance would put the
// caller's tenant ID and trace context on a subject every tenant's
// collections receive.
func (c *client) PublishWorkerProgress(ctx context.Context) error {
	start := time.Now()
	ctx, span := c.startSpan(ctx, "natsbus.PublishWorkerProgress")
	defer span.End()
	span.SetAttributes(attribute.String("messaging.subject", WorkerProgressSubject))

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		span.SetStatus(codes.Error, "connection closed")
		return fmt.Errorf("publish worker progress: %w", ErrConnectionClosed)
	}
	c.mu.Unlock()

	if err := c.conn.Publish(WorkerProgressSubject, nil); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("publish worker progress: %w: %w", ErrPublishFailed, err)
	}
	if c.publishCounter != nil {
		c.publishCounter.Add(ctx, 1)
	}
	if c.publishLatency != nil {
		c.publishLatency.Record(ctx, time.Since(start).Milliseconds())
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// SubscribeWorkerProgress subscribes handler to WorkerProgressSubject without
// wrapHandler's provenance check, which a tick cannot pass. The NATS callback
// discards the message, so neither its payload nor its headers reach handler.
func (c *client) SubscribeWorkerProgress(ctx context.Context, handler func()) (Subscription, error) {
	_, span := c.startSpan(ctx, "natsbus.SubscribeWorkerProgress")
	defer span.End()
	span.SetAttributes(attribute.String("messaging.subject", WorkerProgressSubject))

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		span.SetStatus(codes.Error, "connection closed")
		return nil, fmt.Errorf("subscribe to worker progress: %w", ErrConnectionClosed)
	}
	c.mu.Unlock()

	sub, err := c.conn.Subscribe(WorkerProgressSubject, func(*nats.Msg) { handler() })
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("subscribe to worker progress: %w: %w", ErrSubscribeFailed, err)
	}
	span.SetStatus(codes.Ok, "")
	return &subscription{sub: sub}, nil
}

// CreateStream creates or updates a JetStream stream.
func (c *client) CreateStream(ctx context.Context, cfg StreamConfig) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("create stream %s: %w", cfg.Name, ErrConnectionClosed)
	}
	c.mu.Unlock()

	ctx, span := c.startSpan(ctx, "natsbus.CreateStream",
		trace.WithAttributes(attribute.String("messaging.stream", cfg.Name)),
	)
	defer span.End()

	_, err := c.js.CreateOrUpdateStream(ctx, jetStreamConfig(cfg))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("create stream %s: %w: %w", cfg.Name, ErrStreamCreate, err)
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// DeleteStream removes a JetStream stream.
func (c *client) DeleteStream(ctx context.Context, name string) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("delete stream %s: %w", name, ErrConnectionClosed)
	}
	c.mu.Unlock()

	ctx, span := c.startSpan(ctx, "natsbus.DeleteStream",
		trace.WithAttributes(attribute.String("messaging.stream", name)),
	)
	defer span.End()

	if err := c.js.DeleteStream(ctx, name); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("delete stream %s: %w: %w", name, ErrStreamNotFound, err)
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// AuditStreamRetention reads the live MaxAge of each audit stream from the
// server, keyed by stream name. The stream set is derived from
// auditStreamConfigs so the single audit-stream mapping stays authoritative.
func (c *client) AuditStreamRetention(ctx context.Context) (map[string]time.Duration, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("audit stream retention: %w", ErrConnectionClosed)
	}
	c.mu.Unlock()

	ctx, span := c.startSpan(ctx, "natsbus.AuditStreamRetention")
	defer span.End()

	configs := auditStreamConfigs(config.NATSStreamsConfig{})
	out := make(map[string]time.Duration, len(configs))
	for _, sc := range configs {
		// An absent stream IS drift, not a source fault: omit it so the caller
		// reports Live=0/OK=false. Only genuine failures (transport, timeout)
		// fail the whole read.
		stream, err := c.js.Stream(ctx, sc.Name)
		if err != nil {
			if errors.Is(err, jetstream.ErrStreamNotFound) {
				continue
			}
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return nil, fmt.Errorf("get stream %s: %w", sc.Name, err)
		}
		info, err := stream.Info(ctx)
		if err != nil {
			if errors.Is(err, jetstream.ErrStreamNotFound) {
				continue
			}
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return nil, fmt.Errorf("stream info %s: %w", sc.Name, err)
		}
		out[sc.Name] = info.Config.MaxAge
	}
	span.SetStatus(codes.Ok, "")
	return out, nil
}

// Close drains the connection, stops the embedded server if applicable,
// and releases all resources. Safe to call multiple times.
func (c *client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil
	}
	c.closed = true

	if err := c.conn.Drain(); err != nil {
		c.opts.logger.Warn("NATS drain error", "error", err)
	}
	c.conn.Close()

	if c.embedded != nil {
		c.embedded.shutdown()
	}

	c.opts.logger.Info("NATS client closed")
	return nil
}

func (c *client) startSpan(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	if c.tracer != nil {
		return c.tracer.Start(ctx, name, opts...)
	}
	return trace.SpanFromContext(ctx).TracerProvider().Tracer("natsbus").Start(ctx, name, opts...)
}

// jetStreamConfig maps a StreamConfig to the JetStream configuration it is
// created with (and compared against): file storage, and at least one replica.
func jetStreamConfig(cfg StreamConfig) jetstream.StreamConfig {
	jsCfg := jetstream.StreamConfig{
		Name:      cfg.Name,
		Subjects:  cfg.Subjects,
		MaxAge:    cfg.MaxAge,
		Replicas:  cfg.Replicas,
		Storage:   jetstream.FileStorage,
		Retention: jetstream.LimitsPolicy,
	}
	if cfg.WorkQueue {
		jsCfg.Retention = jetstream.WorkQueuePolicy
	}
	if jsCfg.Replicas < 1 {
		jsCfg.Replicas = 1
	}
	return jsCfg
}

// ensureStreams makes every stream this package owns exist. Any role may
// create a missing stream, but none updates an existing one: with several
// roles sharing a NATS server and each carrying its own nats.streams, an
// update would let the last role to start overwrite the others' retention.
// A live stream that differs from the configuration is refused with
// ErrStreamDrift.
func (c *client) ensureStreams(ctx context.Context, streamsCfg config.NATSStreamsConfig) error {
	for _, sc := range streamConfigs(streamsCfg) {
		if err := c.ensureStream(ctx, sc); err != nil {
			return err
		}
	}
	return nil
}

func (c *client) ensureStream(ctx context.Context, sc StreamConfig) error {
	want := jetStreamConfig(sc)
	live, err := c.liveStreamConfig(ctx, sc.Name)
	if errors.Is(err, jetstream.ErrStreamNotFound) {
		_, err = c.js.CreateStream(ctx, want)
		if err == nil {
			return nil
		}
		if !errors.Is(err, jetstream.ErrStreamNameAlreadyInUse) {
			return fmt.Errorf("create stream %s: %w: %w", sc.Name, ErrStreamCreate, err)
		}
		// Another role created it between the lookup and the create: verify its
		// settings like any live stream.
		live, err = c.liveStreamConfig(ctx, sc.Name)
		if errors.Is(err, jetstream.ErrStreamNotFound) {
			return fmt.Errorf("stream %s was deleted while crosscodexd was starting; restart to create it: %w", sc.Name, err)
		}
	}
	if err != nil {
		return err
	}
	if diffs := streamDrift(live, want); len(diffs) > 0 {
		return fmt.Errorf("%w: stream %s on the NATS server differs from this process's nats.streams configuration, and startup never overwrites a live stream: %s. "+
			"To fix: make nats.streams in every role's config agree with the live stream, or update the stream (nats stream edit %s ...) and restart",
			ErrStreamDrift, sc.Name, strings.Join(diffs, "; "), sc.Name)
	}
	return nil
}

func (c *client) liveStreamConfig(ctx context.Context, name string) (jetstream.StreamConfig, error) {
	stream, err := c.js.Stream(ctx, name)
	if err != nil {
		if errors.Is(err, jetstream.ErrStreamNotFound) {
			return jetstream.StreamConfig{}, err
		}
		return jetstream.StreamConfig{}, fmt.Errorf("look up stream %s: %w", name, err)
	}
	info, err := stream.Info(ctx)
	if err != nil {
		return jetstream.StreamConfig{}, fmt.Errorf("stream info %s: %w", name, err)
	}
	return info.Config, nil
}

// streamDrift lists, one entry per field, how live differs from want on the
// fields startup verifies. Subjects compare as a set.
func streamDrift(live, want jetstream.StreamConfig) []string {
	var diffs []string
	if !slices.Equal(slices.Sorted(slices.Values(live.Subjects)), slices.Sorted(slices.Values(want.Subjects))) {
		diffs = append(diffs, fmt.Sprintf("subjects (live %v, configured %v)", live.Subjects, want.Subjects))
	}
	if live.Retention != want.Retention {
		diffs = append(diffs, fmt.Sprintf("retention (live %s, configured %s)", live.Retention, want.Retention))
	}
	if live.Storage != want.Storage {
		diffs = append(diffs, fmt.Sprintf("storage (live %s, configured %s)", live.Storage, want.Storage))
	}
	if live.MaxAge != want.MaxAge {
		diffs = append(diffs, fmt.Sprintf("max_age (live %s, configured %s)", live.MaxAge, want.MaxAge))
	}
	if live.Replicas != want.Replicas {
		diffs = append(diffs, fmt.Sprintf("replicas (live %d, configured %d)", live.Replicas, want.Replicas))
	}
	return diffs
}

// verifyStreams checks that every audit stream exists, without creating or
// updating any.
func (c *client) verifyStreams(ctx context.Context) error {
	for _, name := range AuditStreamNames() {
		_, err := c.js.Stream(ctx, name)
		if errors.Is(err, jetstream.ErrStreamNotFound) {
			return fmt.Errorf("%w: %s is missing on the NATS server, and this process only attaches to existing streams so it never changes their retention. Start the daemon, which creates them, or set nats.url to the daemon's NATS server", ErrStreamNotFound, name)
		}
		if err != nil {
			return fmt.Errorf("look up stream %s: %w", name, err)
		}
	}
	return nil
}

// ConfiguredAuditRetention returns the intended MaxAge for each audit stream,
// keyed by stream name. It reuses auditStreamConfigs so the config-to-stream
// mapping lives in exactly one place.
func ConfiguredAuditRetention(cfg config.NATSStreamsConfig) map[string]time.Duration {
	configs := auditStreamConfigs(cfg)
	out := make(map[string]time.Duration, len(configs))
	for _, sc := range configs {
		out[sc.Name] = sc.MaxAge
	}
	return out
}

// streamConfigs returns every stream a role provisions at startup: the audit
// streams, then MATERIALIZE.
func streamConfigs(cfg config.NATSStreamsConfig) []StreamConfig {
	return append(auditStreamConfigs(cfg), StreamConfig{
		Name: MaterializeStream,
		Subjects: []string{
			subjectPrefix + ".pipeline.*.*.stage.completed",
			subjectPrefix + ".catalog.*.*.parsed",
			subjectPrefix + ".materialize.*.*.barrier",
		},
		MaxAge:    cfg.MaterializeRetention,
		Replicas:  1,
		WorkQueue: true,
	})
}

// auditStreamConfigs returns the three audit stream configurations.
func auditStreamConfigs(cfg config.NATSStreamsConfig) []StreamConfig {
	return []StreamConfig{
		{
			Name:     "AUDIT_LLM",
			Subjects: []string{subjectPrefix + ".audit.*.llm.>"},
			MaxAge:   cfg.AuditLLMRetention,
			Replicas: 1,
		},
		{
			Name:     "AUDIT_DECISIONS",
			Subjects: []string{subjectPrefix + ".audit.*.decisions.>"},
			MaxAge:   0, // indefinite
			Replicas: 1,
		},
		{
			Name:     "AUDIT_EVENTS",
			Subjects: []string{subjectPrefix + ".audit.*.events.>"},
			MaxAge:   cfg.AuditEventsRetention,
			Replicas: 1,
		},
	}
}

// verifiedMessage is a message that passed verifyMessage.
type verifiedMessage struct {
	ctx  context.Context // carries the publisher's trace context and the process span
	span trace.Span      // the process span; the caller ends it
	meta MessageMetadata // provenance fields; JetStream fields are the caller's to fill
}

// verifyMessage is the one fail-closed gate every received message passes,
// core or durable: mandatory provenance headers, a valid tenant ID, and a
// content hash matching the payload. It starts the consumer span first, linked
// to the publisher's trace, so a rejected message is traced too; the caller
// owns the span and must end it. On failure it logs "<action> message: <why>"
// (action is the caller's verb, "rejecting" or "terminating"), marks the span
// failed, and returns the error, which wraps ErrMissingProvenance,
// tenant.ErrInvalidTenant or ErrContentHashMismatch. attrs are added to the span.
func (c *client) verifyMessage(parent context.Context, action, subject string, data []byte, headers map[string][]string, attrs ...attribute.KeyValue) (verifiedMessage, error) {
	if sc, err := reconstructSpanContext(headers); err == nil && sc.IsValid() {
		parent = trace.ContextWithRemoteSpanContext(parent, sc)
	}
	ctx, span := c.startSpan(parent, "natsbus.process",
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(append([]attribute.KeyValue{attribute.String("messaging.subject", subject)}, attrs...)...),
	)
	v := verifiedMessage{ctx: ctx, span: span}

	reject := func(why string, err error, logAttrs ...any) (verifiedMessage, error) {
		c.opts.logger.Error(action+" message: "+why, append([]any{"subject", subject, "error", err}, logAttrs...)...)
		span.SetStatus(codes.Error, why)
		span.RecordError(err)
		return v, err
	}

	meta, err := extractProvenance(headers)
	if err != nil {
		return reject("missing provenance", err)
	}
	if err := tenant.ValidateTenantID(meta.TenantID); err != nil {
		return reject("invalid tenant", err)
	}
	span.SetAttributes(attribute.String("tenant.id", meta.TenantID))
	if actual := contentHash(data); actual != meta.ContentHash {
		return reject("content hash mismatch", ErrContentHashMismatch, "expected", meta.ContentHash, "actual", actual)
	}
	v.meta = meta
	return v, nil
}

// wrapHandler converts a MessageHandler into a nats.MsgHandler. A message that
// fails verifyMessage (missing provenance headers, invalid tenant, content hash
// mismatch) is dropped: the handler is never called and the reason is logged.
func (c *client) wrapHandler(handler MessageHandler) nats.MsgHandler {
	return func(natsMsg *nats.Msg) {
		headers := map[string][]string(natsMsg.Header)
		if natsMsg.Subject == WorkerProgressSubject {
			if _, err := extractProvenance(headers); err != nil {
				// A catch-all generic subscriber (crosscodex.>) receives every
				// worker progress tick, which never has provenance. Drop it like
				// any unprovenanced message, without an error log per tick.
				c.opts.logger.Debug("dropping worker progress tick at a generic subscriber; use SubscribeWorkerProgress to receive ticks",
					"subject", natsMsg.Subject)
				return
			}
		}

		v, err := c.verifyMessage(context.Background(), "rejecting", natsMsg.Subject, natsMsg.Data, headers)
		defer v.span.End()
		if err != nil {
			return
		}
		v.meta.Timestamp = time.Now()
		msg := &Message{
			Subject:  natsMsg.Subject,
			Data:     natsMsg.Data,
			Headers:  headers,
			Metadata: v.meta,
		}

		start := time.Now()
		if err := handler(v.ctx, msg); err != nil {
			c.opts.logger.Error("message handler error",
				"subject", natsMsg.Subject,
				"error", err,
			)
			v.span.RecordError(err)
			v.span.SetStatus(codes.Error, err.Error())
		} else {
			v.span.SetStatus(codes.Ok, "")
		}

		// Record subscriber metrics.
		if c.processCounter != nil {
			c.processCounter.Add(v.ctx, 1)
		}
		if c.processLatency != nil {
			c.processLatency.Record(v.ctx, time.Since(start).Milliseconds())
		}
	}
}

// subscription wraps a nats.Subscription to implement the Subscription interface.
type subscription struct {
	sub *nats.Subscription
}

func (s *subscription) Unsubscribe() error {
	return s.sub.Unsubscribe()
}

func (s *subscription) Drain() error {
	return s.sub.Drain()
}
