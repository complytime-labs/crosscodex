package natsbustest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/nats-io/nats-server/v2/server"

	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// ErrUnscripted is wrapped by every error a fake returns for a call the test
// did not script. Match it with errors.Is; the message names the field to set.
var ErrUnscripted = errors.New("natsbustest: call not scripted")

func unscripted(method string) error {
	return fmt.Errorf("%w: %s called but Fake.%sFunc is not set; set it to script this call",
		ErrUnscripted, method, method)
}

var (
	_ natsbus.Client       = (*Fake)(nil)
	_ natsbus.Subscription = (*Sub)(nil)
)

// Publication is one message accepted by Publish or PublishWithHeaders.
// Headers is nil when the call carried none (Publish, or a nil map).
type Publication struct {
	Subject string
	Data    []byte
	Headers map[string][]string
}

func clonePublication(p Publication) Publication {
	p.Data = slices.Clone(p.Data)
	if p.Headers != nil {
		headers := make(map[string][]string, len(p.Headers))
		for k, v := range p.Headers {
			headers[k] = slices.Clone(v)
		}
		p.Headers = headers
	}
	return p
}

// Fake fakes natsbus.Client. It rejects what the real client rejects at its
// boundary before any Func runs, without recording the call: every method but
// Close returns natsbus.ErrConnectionClosed after Close, and Publish and
// PublishWithHeaders return the tenant error when the context carries no
// tenant. Publish and PublishWithHeaders record and return nil;
// Subscribe and QueueSubscribe record a Sub and return it; Close returns nil.
// Unset CreateStreamFunc, DeleteStreamFunc and AuditStreamRetentionFunc
// return ErrUnscripted. The zero value is ready to use. Set the exported
// fields before the first call; the methods are then safe for concurrent use.
type Fake struct {
	// PublishFunc, if set, alone decides what Publish returns. Every accepted
	// publish is recorded in Published, whether or not it is set.
	PublishFunc func(ctx context.Context, subject string, data []byte) error
	// PublishWithHeadersFunc, if set, alone decides what PublishWithHeaders
	// returns. Every accepted publish is recorded in Published.
	PublishWithHeadersFunc func(ctx context.Context, subject string, data []byte, headers map[string][]string) error
	// SubscribeFunc, if set, alone decides what Subscribe returns. A nil
	// Subscription with a nil error means "succeed with the recorded Sub". If
	// it returns an error, no subscription exists: the call is not recorded
	// in Subscriptions and Deliver never reaches its handler. Otherwise the
	// recorded Sub keeps receiving Deliver even when the Func returns a
	// different Subscription.
	SubscribeFunc func(ctx context.Context, subject string, handler natsbus.MessageHandler) (natsbus.Subscription, error)
	// QueueSubscribeFunc is SubscribeFunc for QueueSubscribe.
	QueueSubscribeFunc func(ctx context.Context, subject, queue string, handler natsbus.MessageHandler) (natsbus.Subscription, error)
	// CreateStreamFunc decides what CreateStream returns; unset is ErrUnscripted.
	CreateStreamFunc func(ctx context.Context, cfg natsbus.StreamConfig) error
	// DeleteStreamFunc decides what DeleteStream returns; unset is ErrUnscripted.
	DeleteStreamFunc func(ctx context.Context, name string) error
	// AuditStreamRetentionFunc decides what AuditStreamRetention returns;
	// unset is ErrUnscripted.
	AuditStreamRetentionFunc func(ctx context.Context) (map[string]time.Duration, error)

	mu        sync.Mutex
	closed    bool
	published []Publication // deep copies, see clonePublication
	subs      []*Sub
}

// checkOpen mirrors the closed-connection check at the top of every real
// client method; wrap is the method's error prefix.
func (f *Fake) checkOpen(wrap string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return fmt.Errorf("%s: %w", wrap, natsbus.ErrConnectionClosed)
	}
	return nil
}

// publish applies the real client's closed and tenant checks, records the
// publication, and reports whether it was accepted.
func (f *Fake) publish(ctx context.Context, subject string, data []byte, headers map[string][]string) error {
	if err := f.checkOpen("publish to " + subject); err != nil {
		return err
	}
	if _, err := tenant.FromContext(ctx); err != nil {
		return fmt.Errorf("publish to %s: tenant required: %w", subject, err)
	}
	pub := clonePublication(Publication{Subject: subject, Data: data, Headers: headers})
	f.mu.Lock()
	f.published = append(f.published, pub)
	f.mu.Unlock()
	return nil
}

// Publish validates and records the message, then returns what PublishFunc
// decides, or nil when it is unset.
func (f *Fake) Publish(ctx context.Context, subject string, data []byte) error {
	if err := f.publish(ctx, subject, data, nil); err != nil {
		return err
	}
	if f.PublishFunc != nil {
		return f.PublishFunc(ctx, subject, data)
	}
	return nil
}

// PublishWithHeaders validates and records the message, then returns what
// PublishWithHeadersFunc decides, or nil when it is unset. Fake does not add
// provenance headers.
func (f *Fake) PublishWithHeaders(ctx context.Context, subject string, data []byte, headers map[string][]string) error {
	if err := f.publish(ctx, subject, data, headers); err != nil {
		return err
	}
	if f.PublishWithHeadersFunc != nil {
		return f.PublishWithHeadersFunc(ctx, subject, data, headers)
	}
	return nil
}

// Published returns every accepted publication from Publish and
// PublishWithHeaders, in call order. The publications are deep copies, so a
// caller mutating one does not alter the record.
func (f *Fake) Published() []Publication {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Publication, len(f.published))
	for i, p := range f.published {
		out[i] = clonePublication(p)
	}
	return out
}

// Subscribe records a subscription to subject and returns it, unless
// SubscribeFunc decides otherwise.
func (f *Fake) Subscribe(ctx context.Context, subject string, handler natsbus.MessageHandler) (natsbus.Subscription, error) {
	if err := f.checkOpen("subscribe to " + subject); err != nil {
		return nil, err
	}
	sub := &Sub{Subject: subject, handler: handler}
	if f.SubscribeFunc == nil {
		return f.register(sub), nil
	}
	got, err := f.SubscribeFunc(ctx, subject, handler)
	return f.finishSubscribe(sub, got, err)
}

// QueueSubscribe records a queue-group subscription and returns it, unless
// QueueSubscribeFunc decides otherwise. Fake delivers to every matching Sub
// rather than round-robining across a queue group.
func (f *Fake) QueueSubscribe(ctx context.Context, subject, queue string, handler natsbus.MessageHandler) (natsbus.Subscription, error) {
	if err := f.checkOpen("queue subscribe to " + subject); err != nil {
		return nil, err
	}
	sub := &Sub{Subject: subject, Queue: queue, handler: handler}
	if f.QueueSubscribeFunc == nil {
		return f.register(sub), nil
	}
	got, err := f.QueueSubscribeFunc(ctx, subject, queue, handler)
	return f.finishSubscribe(sub, got, err)
}

// finishSubscribe applies a subscribe Func's result: an error means no
// subscription exists; otherwise the Func's Subscription, or sub when it
// returned none, is what the caller gets, and sub is always recorded.
func (f *Fake) finishSubscribe(sub *Sub, got natsbus.Subscription, err error) (natsbus.Subscription, error) {
	if err != nil {
		return got, err
	}
	f.register(sub)
	if got != nil {
		return got, nil
	}
	return sub, nil
}

func (f *Fake) register(sub *Sub) *Sub {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subs = append(f.subs, sub)
	return sub
}

// Subscriptions returns every subscription Subscribe and QueueSubscribe
// recorded, in call order, including unsubscribed and drained ones.
func (f *Fake) Subscriptions() []*Sub {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.subs)
}

// Deliver calls, synchronously and in subscription order, the handler of every
// live Sub whose subject pattern matches msg.Subject, passing ctx and msg to
// each (they are not copied). It calls every handler even when some fail and
// returns their errors joined with errors.Join. Deliver ignores Close and
// skips subscriptions with a nil handler. It returns an error for a nil msg.
func (f *Fake) Deliver(ctx context.Context, msg *natsbus.Message) error {
	if msg == nil {
		return errors.New("natsbustest: Deliver called with a nil message")
	}
	f.mu.Lock()
	subs := slices.Clone(f.subs)
	f.mu.Unlock()

	var errs []error
	for _, s := range subs {
		if s.handler == nil || !s.live() || !server.SubjectMatchesFilter(msg.Subject, s.Subject) {
			continue
		}
		errs = append(errs, s.handler(ctx, msg))
	}
	return errors.Join(errs...)
}

// CreateStream returns what CreateStreamFunc decides, or an ErrUnscripted
// error when it is unset.
func (f *Fake) CreateStream(ctx context.Context, cfg natsbus.StreamConfig) error {
	if err := f.checkOpen("create stream " + cfg.Name); err != nil {
		return err
	}
	if f.CreateStreamFunc == nil {
		return unscripted("CreateStream")
	}
	return f.CreateStreamFunc(ctx, cfg)
}

// DeleteStream returns what DeleteStreamFunc decides, or an ErrUnscripted
// error when it is unset.
func (f *Fake) DeleteStream(ctx context.Context, name string) error {
	if err := f.checkOpen("delete stream " + name); err != nil {
		return err
	}
	if f.DeleteStreamFunc == nil {
		return unscripted("DeleteStream")
	}
	return f.DeleteStreamFunc(ctx, name)
}

// AuditStreamRetention returns what AuditStreamRetentionFunc decides, or an
// ErrUnscripted error when it is unset.
func (f *Fake) AuditStreamRetention(ctx context.Context) (map[string]time.Duration, error) {
	if err := f.checkOpen("audit stream retention"); err != nil {
		return nil, err
	}
	if f.AuditStreamRetentionFunc == nil {
		return nil, unscripted("AuditStreamRetention")
	}
	return f.AuditStreamRetentionFunc(ctx)
}

// Close marks the fake closed and returns nil. Like the real client, it is
// safe to call repeatedly.
func (f *Fake) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// Sub fakes natsbus.Subscription. Fake creates one per accepted Subscribe or
// QueueSubscribe. The methods are safe for concurrent use.
type Sub struct {
	// Subject is the subscription's subject pattern.
	Subject string
	// Queue is the queue group, empty for Subscribe.
	Queue string
	// UnsubscribeErr is returned by Unsubscribe.
	UnsubscribeErr error
	// DrainErr is returned by Drain. Set UnsubscribeErr and DrainErr before the
	// method is called; they are read when it runs.
	DrainErr error

	handler natsbus.MessageHandler

	mu           sync.Mutex
	unsubscribed bool
	drained      bool
}

// Unsubscribe marks the Sub unsubscribed, even when it returns UnsubscribeErr.
func (s *Sub) Unsubscribe() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unsubscribed = true
	return s.UnsubscribeErr
}

// Drain marks the Sub drained, even when it returns DrainErr.
func (s *Sub) Drain() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drained = true
	return s.DrainErr
}

// Unsubscribed reports whether Unsubscribe has been called.
func (s *Sub) Unsubscribed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unsubscribed
}

// Drained reports whether Drain has been called.
func (s *Sub) Drained() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.drained
}

func (s *Sub) live() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.unsubscribed && !s.drained
}

// Handle calls this Sub's handler directly with msg, bypassing subject
// matching and liveness, and returns the handler's error. Use it to feed a
// handler a subject the broker would never route to it. It returns an error
// when the Sub has no handler.
func (s *Sub) Handle(ctx context.Context, msg *natsbus.Message) error {
	if s.handler == nil {
		return errors.New("natsbustest: Sub has no handler")
	}
	return s.handler(ctx, msg)
}
