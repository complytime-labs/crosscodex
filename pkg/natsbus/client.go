package natsbus

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// client implements the Client interface for both embedded and external NATS.
type client struct {
	conn     *nats.Conn
	js       jetstream.JetStream
	embedded *embeddedServer
	opts     clientOptions

	mu     sync.Mutex
	closed bool
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
		opts: o,
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
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("publish to %s: %w", subject, ErrConnectionClosed)
	}
	c.mu.Unlock()

	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		return fmt.Errorf("publish to %s: tenant required: %w", subject, err)
	}

	provHeaders := injectProvenance(ctx, data, tenantID)
	allHeaders := mergeHeaders(headers, provHeaders)

	msg := &nats.Msg{
		Subject: subject,
		Data:    data,
		Header:  nats.Header(allHeaders),
	}

	if err := c.conn.PublishMsg(msg); err != nil {
		return fmt.Errorf("publish to %s: %w: %w", subject, ErrPublishFailed, err)
	}
	return nil
}

// Subscribe creates a subscription to the specified subject.
func (c *client) Subscribe(ctx context.Context, subject string, handler MessageHandler) (Subscription, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("subscribe to %s: %w", subject, ErrConnectionClosed)
	}
	c.mu.Unlock()

	sub, err := c.conn.Subscribe(subject, c.wrapHandler(handler))
	if err != nil {
		return nil, fmt.Errorf("subscribe to %s: %w: %w", subject, ErrSubscribeFailed, err)
	}
	return &subscription{sub: sub}, nil
}

// QueueSubscribe creates a queue group subscription for work distribution.
func (c *client) QueueSubscribe(ctx context.Context, subject string, queue string, handler MessageHandler) (Subscription, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("queue subscribe to %s: %w", subject, ErrConnectionClosed)
	}
	c.mu.Unlock()

	sub, err := c.conn.QueueSubscribe(subject, queue, c.wrapHandler(handler))
	if err != nil {
		return nil, fmt.Errorf("queue subscribe to %s queue %s: %w: %w", subject, queue, ErrSubscribeFailed, err)
	}
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
		return fmt.Errorf("create stream %s: %w: %w", cfg.Name, ErrStreamCreate, err)
	}
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

	if err := c.js.DeleteStream(ctx, name); err != nil {
		return fmt.Errorf("delete stream %s: %w: %w", name, ErrStreamNotFound, err)
	}
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

		msg := &Message{
			Subject:  natsMsg.Subject,
			Data:     natsMsg.Data,
			Headers:  headers,
			Metadata: meta,
		}

		if err := handler(msg); err != nil {
			c.opts.logger.Error("message handler error",
				"subject", natsMsg.Subject,
				"error", err,
			)
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
