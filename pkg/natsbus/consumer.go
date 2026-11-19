package natsbus

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// Outcome attribute values on the natsbus.process.total counter for durable
// consumers.
const (
	outcomeAck  = "ack"
	outcomeNak  = "nak"
	outcomeTerm = "term"
)

// ConsumeDurable creates or updates the durable pull consumer and delivers its
// messages to handler. See Client.ConsumeDurable for the contract.
func (c *client) ConsumeDurable(ctx context.Context, cfg ConsumerConfig, handler MessageHandler) (Subscription, error) {
	ctx, span := c.startSpan(ctx, "natsbus.ConsumeDurable", trace.WithAttributes(
		attribute.String("messaging.stream", cfg.Stream),
		attribute.String("messaging.consumer", cfg.Durable),
	))
	defer span.End()

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		span.SetStatus(codes.Error, "connection closed")
		return nil, fmt.Errorf("consume %s: %w", cfg.Durable, ErrConnectionClosed)
	}
	c.mu.Unlock()

	if err := cfg.Validate(); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	// The consumer is the caller's to define, so an existing one is updated to
	// this config rather than refused. Explicit acks: the handler outcome, not
	// delivery, decides what the server does with the message.
	consumer, err := c.js.CreateOrUpdateConsumer(ctx, cfg.Stream, jetstream.ConsumerConfig{
		Durable:        cfg.Durable,
		FilterSubjects: cfg.FilterSubjects,
		AckPolicy:      jetstream.AckExplicitPolicy,
		AckWait:        cfg.AckWait,
		MaxDeliver:     cfg.MaxDeliver,
	})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		if errors.Is(err, jetstream.ErrStreamNotFound) {
			return nil, fmt.Errorf("consume %s: %w: %w: stream %s does not exist; create it before consuming: %w",
				cfg.Durable, ErrSubscribeFailed, ErrStreamNotFound, cfg.Stream, err)
		}
		return nil, fmt.Errorf("consume %s on %s: %w: %w", cfg.Durable, cfg.Stream, ErrSubscribeFailed, err)
	}

	// Handlers run under a context of their own, as in wrapHandler: it must
	// outlive the caller's ctx and end when the subscription does.
	handlerCtx, cancel := context.WithCancel(context.Background())
	cc, err := consumer.Consume(func(msg jetstream.Msg) {
		c.handleDurable(handlerCtx, cfg, handler, msg)
	})
	if err != nil {
		cancel()
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("consume %s on %s: %w: %w", cfg.Durable, cfg.Stream, ErrSubscribeFailed, err)
	}
	go func() {
		<-cc.Closed()
		cancel()
	}()
	span.SetStatus(codes.Ok, "")
	return &durableSubscription{cc: cc, cancel: cancel}, nil
}

// handleDurable applies provenance and content-hash checks to msg, runs
// handler, and acknowledges msg by the handler's outcome. A message failing a
// check is terminated: redelivery cannot fix it.
func (c *client) handleDurable(parent context.Context, cfg ConsumerConfig, handler MessageHandler, msg jetstream.Msg) {
	start := time.Now()
	headers := map[string][]string(msg.Headers())

	meta, err := extractProvenance(headers)
	if err != nil {
		c.opts.logger.Error("terminating message: missing provenance",
			"stream", cfg.Stream, "consumer", cfg.Durable, "subject", msg.Subject(), "error", err)
		c.finishDurable(parent, cfg, msg, outcomeTerm, start)
		return
	}
	ctx, err := tenant.WithTenant(parent, meta.TenantID)
	if err != nil {
		c.opts.logger.Error("terminating message: invalid tenant",
			"stream", cfg.Stream, "consumer", cfg.Durable, "subject", msg.Subject(), "error", err)
		c.finishDurable(parent, cfg, msg, outcomeTerm, start)
		return
	}
	if sc, scErr := reconstructSpanContext(headers); scErr == nil && sc.IsValid() {
		ctx = trace.ContextWithRemoteSpanContext(ctx, sc)
	}

	ctx, span := c.startSpan(ctx, "natsbus.process",
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("messaging.subject", msg.Subject()),
			attribute.String("messaging.stream", cfg.Stream),
			attribute.String("messaging.consumer", cfg.Durable),
			attribute.String("tenant.id", meta.TenantID),
		),
	)
	defer span.End()

	if actual := contentHash(msg.Data()); actual != meta.ContentHash {
		c.opts.logger.Error("terminating message: content hash mismatch",
			"stream", cfg.Stream, "consumer", cfg.Durable, "subject", msg.Subject(),
			"expected", meta.ContentHash, "actual", actual)
		span.SetStatus(codes.Error, "content hash mismatch")
		span.RecordError(ErrContentHashMismatch)
		c.finishDurable(ctx, cfg, msg, outcomeTerm, start)
		return
	}

	js, err := msg.Metadata()
	if err != nil {
		c.opts.logger.Error("terminating message: no JetStream metadata",
			"stream", cfg.Stream, "consumer", cfg.Durable, "subject", msg.Subject(), "error", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		c.finishDurable(ctx, cfg, msg, outcomeTerm, start)
		return
	}
	meta.Stream = js.Stream
	meta.Sequence = js.Sequence.Stream
	meta.Timestamp = js.Timestamp
	meta.NumDelivered = js.NumDelivered
	span.SetAttributes(attribute.Int64("messaging.delivery_attempt", int64(js.NumDelivered)))

	handlerErr := handler(ctx, &Message{Subject: msg.Subject(), Data: msg.Data(), Headers: headers, Metadata: meta})
	outcome := outcomeAck
	switch {
	case errors.Is(handlerErr, ErrPermanent):
		outcome = outcomeTerm
	case handlerErr != nil:
		outcome = outcomeNak
	}
	if handlerErr != nil {
		c.opts.logger.Error("durable message handler error",
			"stream", cfg.Stream, "consumer", cfg.Durable, "subject", msg.Subject(),
			"outcome", outcome, "delivery", js.NumDelivered, "error", handlerErr)
		span.RecordError(handlerErr)
		span.SetStatus(codes.Error, handlerErr.Error())
	} else {
		span.SetStatus(codes.Ok, "")
	}
	span.SetAttributes(attribute.String("messaging.outcome", outcome))
	c.finishDurable(ctx, cfg, msg, outcome, start)
}

// finishDurable sends outcome to the server and records the process metrics. A
// failed ack is only logged: the server redelivers after AckWait, so nothing is
// lost, at worst handled twice, which handlers must tolerate anyway.
func (c *client) finishDurable(ctx context.Context, cfg ConsumerConfig, msg jetstream.Msg, outcome string, start time.Time) {
	var err error
	switch outcome {
	case outcomeAck:
		err = msg.Ack()
	case outcomeNak:
		err = msg.NakWithDelay(cfg.RedeliverDelay)
	default:
		err = msg.Term()
	}
	if err != nil {
		c.opts.logger.Error("acknowledging message failed; the server will redeliver it after AckWait",
			"subject", msg.Subject(), "outcome", outcome, "error", err)
	}
	attrs := metric.WithAttributes(attribute.String("outcome", outcome))
	if c.processCounter != nil {
		c.processCounter.Add(ctx, 1, attrs)
	}
	if c.processLatency != nil {
		c.processLatency.Record(ctx, time.Since(start).Milliseconds(), attrs)
	}
}

// SubjectMessageCount sums the messages stream holds on subject.
func (c *client) SubjectMessageCount(ctx context.Context, stream, subject string) (uint64, error) {
	ctx, span := c.startSpan(ctx, "natsbus.SubjectMessageCount", trace.WithAttributes(
		attribute.String("messaging.stream", stream),
		attribute.String("messaging.subject", subject),
	))
	defer span.End()

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		span.SetStatus(codes.Error, "connection closed")
		return 0, fmt.Errorf("count messages in %s: %w", stream, ErrConnectionClosed)
	}
	c.mu.Unlock()

	js, err := c.js.Stream(ctx, stream)
	if err == nil {
		var info *jetstream.StreamInfo
		if info, err = js.Info(ctx, jetstream.WithSubjectFilter(subject)); err == nil {
			var total uint64
			for _, n := range info.State.Subjects {
				total += n
			}
			span.SetStatus(codes.Ok, "")
			return total, nil
		}
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
	if errors.Is(err, jetstream.ErrStreamNotFound) {
		return 0, fmt.Errorf("count messages in %s: %w: %w", stream, ErrStreamNotFound, err)
	}
	return 0, fmt.Errorf("count messages in %s on %s: %w", stream, subject, err)
}

// durableSubscription adapts a jetstream.ConsumeContext to Subscription.
type durableSubscription struct {
	cc     jetstream.ConsumeContext
	cancel context.CancelFunc
}

// Unsubscribe stops fetching at once and cancels the handler context;
// unacknowledged messages are redelivered after AckWait.
func (s *durableSubscription) Unsubscribe() error {
	s.cc.Stop()
	s.cancel()
	return nil
}

// Drain stops fetching and lets in-flight messages finish; the handler context
// is cancelled once the consumer has closed.
func (s *durableSubscription) Drain() error {
	s.cc.Drain()
	return nil
}
