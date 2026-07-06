package natsbus

import (
	"context"
	"fmt"
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

// New creates a new NATS Client. When cfg.URL is empty, an embedded
// NATS server is started. When cfg.URL is set, the client connects
// to the external server.
//
// Three audit streams (AUDIT_LLM, AUDIT_DECISIONS, AUDIT_EVENTS) are
// created or updated on startup.
func New(cfg config.NATSConfig, opts ...Option) (Client, error) {
	o := defaultClientOptions()
	for _, opt := range opts {
		opt(&o)
	}

	c := &client{
		opts:   o,
		tracer: o.tracer,
		meter:  o.meter,
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

	nc, err := nats.Connect(connectURL, natsOpts...)
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

	if err := c.ensureStreams(context.Background(), cfg.Streams); err != nil {
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
	span.SetAttributes(attribute.String("messaging.subject", subject))

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		span.SetStatus(codes.Error, "connection closed")
		return fmt.Errorf("publish to %s: %w", subject, ErrConnectionClosed)
	}
	c.mu.Unlock()

	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		span.SetStatus(codes.Error, "tenant required")
		return fmt.Errorf("publish to %s: tenant required: %w", subject, err)
	}
	span.SetAttributes(attribute.String("tenant.id", tenantID))

	provHeaders := injectProvenance(ctx, data, tenantID)
	allHeaders := mergeHeaders(headers, provHeaders)

	msg := &nats.Msg{
		Subject: subject,
		Data:    data,
		Header:  nats.Header(allHeaders),
	}

	if err := c.conn.PublishMsg(msg); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("publish to %s: %w: %w", subject, ErrPublishFailed, err)
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

	jsCfg := jetstream.StreamConfig{
		Name:      cfg.Name,
		Subjects:  cfg.Subjects,
		MaxAge:    cfg.MaxAge,
		Replicas:  cfg.Replicas,
		Storage:   jetstream.FileStorage,
		Retention: jetstream.LimitsPolicy,
	}

	if jsCfg.Replicas < 1 {
		jsCfg.Replicas = 1
	}

	_, err := c.js.CreateOrUpdateStream(ctx, jsCfg)
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

// ensureStreams creates or updates the three audit JetStream streams.
func (c *client) ensureStreams(ctx context.Context, streamsCfg config.NATSStreamsConfig) error {
	for _, cfg := range auditStreamConfigs(streamsCfg) {
		if err := c.CreateStream(ctx, cfg); err != nil {
			return err
		}
	}
	return nil
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

// wrapHandler converts a MessageHandler into a nats.MsgHandler.
// Messages missing mandatory provenance headers are rejected — the
// handler is never called and an error is logged with the missing fields.
// Messages with a content hash mismatch are rejected similarly.
func (c *client) wrapHandler(handler MessageHandler) nats.MsgHandler {
	return func(natsMsg *nats.Msg) {
		headers := map[string][]string(natsMsg.Header)
		meta, err := extractProvenance(headers)
		if err != nil {
			c.opts.logger.Error("rejecting message: missing provenance",
				"subject", natsMsg.Subject,
				"error", err,
			)
			return
		}
		meta.Timestamp = time.Now()

		// Reconstruct trace context from provenance headers.
		ctx := context.Background()
		if sc, scErr := reconstructSpanContext(headers); scErr == nil && sc.IsValid() {
			ctx = trace.ContextWithRemoteSpanContext(ctx, sc)
		}

		// Start consumer span — links subscriber processing to the publisher trace.
		ctx, processSpan := c.startSpan(ctx, "natsbus.process",
			trace.WithSpanKind(trace.SpanKindConsumer),
			trace.WithAttributes(
				attribute.String("messaging.subject", natsMsg.Subject),
				attribute.String("tenant.id", meta.TenantID),
			),
		)
		defer processSpan.End()

		// Verify content hash — fail closed on mismatch.
		actualHash := contentHash(natsMsg.Data)
		if actualHash != meta.ContentHash {
			c.opts.logger.Error("rejecting message: content hash mismatch",
				"subject", natsMsg.Subject,
				"expected", meta.ContentHash,
				"actual", actualHash,
			)
			processSpan.SetStatus(codes.Error, "content hash mismatch")
			processSpan.RecordError(ErrContentHashMismatch)
			return
		}

		msg := &Message{
			Subject:  natsMsg.Subject,
			Data:     natsMsg.Data,
			Headers:  headers,
			Metadata: meta,
		}

		start := time.Now()
		if err := handler(ctx, msg); err != nil {
			c.opts.logger.Error("message handler error",
				"subject", natsMsg.Subject,
				"error", err,
			)
			processSpan.RecordError(err)
			processSpan.SetStatus(codes.Error, err.Error())
		} else {
			processSpan.SetStatus(codes.Ok, "")
		}

		// Record subscriber metrics.
		if c.processCounter != nil {
			c.processCounter.Add(ctx, 1)
		}
		if c.processLatency != nil {
			c.processLatency.Record(ctx, time.Since(start).Milliseconds())
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
