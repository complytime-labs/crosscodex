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

// Publication is one message accepted by Publish, PublishWithHeaders or
// PublishDurable. Headers is nil when the call carried none (Publish, or a nil
// map). Durable is true only for PublishDurable.
type Publication struct {
	Subject string
	Data    []byte
	Headers map[string][]string
	Durable bool
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
// tenant (PublishDurable included). PublishWorkerProgress needs no tenant, like the real client.
// Publish, PublishWithHeaders, PublishDurable and PublishWorkerProgress record and return nil;
// Subscribe, QueueSubscribe, SubscribeWorkerProgress and ConsumeDurable record
// a Sub and return it, unless their Func decides otherwise; Close returns nil.
// Unset CreateStreamFunc, DeleteStreamFunc, AuditStreamRetentionFunc,
// SubjectMessageCountFunc and LastMessageTimeFunc
// return ErrUnscripted. The zero value is ready to use. Set the exported
// fields before the first call; the methods are then safe for concurrent use.
type Fake struct {
	// PublishFunc, if set, alone decides what Publish returns. Every accepted
	// publish is recorded in Published, whether or not it is set.
	PublishFunc func(ctx context.Context, subject string, data []byte) error
	// PublishWithHeadersFunc, if set, alone decides what PublishWithHeaders
	// returns. Every accepted publish is recorded in Published.
	PublishWithHeadersFunc func(ctx context.Context, subject string, data []byte, headers map[string][]string) error
	// PublishDurableFunc, if set, alone decides what PublishDurable returns.
	// Every accepted publish is recorded in Published with Durable true.
	PublishDurableFunc func(ctx context.Context, subject string, data []byte, headers map[string][]string) error
	// SubscribeFunc, if set, alone decides what Subscribe returns. A nil
	// Subscription with a nil error means "succeed with the recorded Sub". If
	// it returns an error, no subscription exists: the call is not recorded
	// in Subscriptions and Deliver never reaches its handler. Otherwise the
	// recorded Sub keeps receiving Deliver even when the Func returns a
	// different Subscription.
	SubscribeFunc func(ctx context.Context, subject string, handler natsbus.MessageHandler) (natsbus.Subscription, error)
	// QueueSubscribeFunc is SubscribeFunc for QueueSubscribe.
	QueueSubscribeFunc func(ctx context.Context, subject, queue string, handler natsbus.MessageHandler) (natsbus.Subscription, error)
	// ConsumeDurableFunc is SubscribeFunc for ConsumeDurable. It runs only for
	// a config ConsumerConfig.Validate accepts, and is handed the caller's
	// handler; the recorded Sub still applies the consumer's tenant context and
	// ack, nak and term outcomes.
	ConsumeDurableFunc func(ctx context.Context, cfg natsbus.ConsumerConfig, handler natsbus.MessageHandler) (natsbus.Subscription, error)
	// PublishWorkerProgressErr is returned by PublishWorkerProgress, which
	// records the tick in Published whether or not it is set. It affects no
	// other method, so it fails ticks without failing result publishes.
	PublishWorkerProgressErr error
	// SubscribeWorkerProgressFunc is SubscribeFunc for SubscribeWorkerProgress.
	SubscribeWorkerProgressFunc func(ctx context.Context, handler func()) (natsbus.Subscription, error)
	// SubjectMessageCountFunc decides what SubjectMessageCount returns; unset is
	// ErrUnscripted.
	SubjectMessageCountFunc func(ctx context.Context, stream, subject string) (uint64, error)
	// LastMessageTimeFunc decides what LastMessageTime returns; unset is
	// ErrUnscripted.
	LastMessageTimeFunc func(ctx context.Context, stream, subject string) (time.Time, bool, error)
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
func (f *Fake) publish(ctx context.Context, subject string, data []byte, headers map[string][]string, durable bool) error {
	if err := f.checkOpen("publish to " + subject); err != nil {
		return err
	}
	if _, err := tenant.FromContext(ctx); err != nil {
		return fmt.Errorf("publish to %s: tenant required: %w", subject, err)
	}
	pub := clonePublication(Publication{Subject: subject, Data: data, Headers: headers, Durable: durable})
	f.mu.Lock()
	f.published = append(f.published, pub)
	f.mu.Unlock()
	return nil
}

// Publish validates and records the message, then returns what PublishFunc
// decides, or nil when it is unset.
func (f *Fake) Publish(ctx context.Context, subject string, data []byte) error {
	if err := f.publish(ctx, subject, data, nil, false); err != nil {
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
	if err := f.publish(ctx, subject, data, headers, false); err != nil {
		return err
	}
	if f.PublishWithHeadersFunc != nil {
		return f.PublishWithHeadersFunc(ctx, subject, data, headers)
	}
	return nil
}

// PublishDurable validates and records the message with Durable set, then
// returns what PublishDurableFunc decides, or nil when it is unset. Fake does
// not add provenance headers. Script an ErrNotStored failure through
// PublishDurableFunc.
func (f *Fake) PublishDurable(ctx context.Context, subject string, data []byte, headers map[string][]string) error {
	if err := f.publish(ctx, subject, data, headers, true); err != nil {
		return err
	}
	if f.PublishDurableFunc != nil {
		return f.PublishDurableFunc(ctx, subject, data, headers)
	}
	return nil
}

// Published returns every accepted publication from Publish,
// PublishWithHeaders, PublishDurable and PublishWorkerProgress, in call order. The publications are deep copies, so a
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

// PublishWorkerProgress records a bare tick, Publication{Subject:
// natsbus.WorkerProgressSubject} with no data or headers, in Published in
// call order with the other publishes, and returns PublishWorkerProgressErr.
// Like the real client it needs no tenant in ctx.
func (f *Fake) PublishWorkerProgress(_ context.Context) error {
	if err := f.checkOpen("publish worker progress"); err != nil {
		return err
	}
	f.mu.Lock()
	f.published = append(f.published, Publication{Subject: natsbus.WorkerProgressSubject})
	f.mu.Unlock()
	return f.PublishWorkerProgressErr
}

// SubscribeWorkerProgress records a subscription to
// natsbus.WorkerProgressSubject and returns it, unless
// SubscribeWorkerProgressFunc decides otherwise. Deliver signals handler for
// every message on exactly that subject and passes it nothing from the
// message, as the real client does.
func (f *Fake) SubscribeWorkerProgress(ctx context.Context, handler func()) (natsbus.Subscription, error) {
	if err := f.checkOpen("subscribe to worker progress"); err != nil {
		return nil, err
	}
	sub := &Sub{Subject: natsbus.WorkerProgressSubject}
	if handler != nil { // a nil handler stays nil, so Deliver skips it
		sub.handler = func(context.Context, *natsbus.Message) error {
			handler()
			return nil
		}
	}
	if f.SubscribeWorkerProgressFunc == nil {
		return f.register(sub), nil
	}
	got, err := f.SubscribeWorkerProgressFunc(ctx, handler)
	return f.finishSubscribe(sub, got, err)
}

// ConsumeDurable validates cfg like the real client, then records a Sub that
// Deliver matches against every cfg.FilterSubjects pattern (every subject when
// there are none). The Sub counts each delivery by the outcome the real
// consumer would apply to the handler's result: nil is acked, an error
// wrapping natsbus.ErrSkip or natsbus.ErrPermanent is termed, any other error
// (natsbus.ErrNotReady included) is naked. The
// handler's ctx carries msg.Metadata.TenantID; a message whose tenant ID is
// empty or invalid is termed without calling the handler, and Deliver returns
// the tenant error. Fake never redelivers; a test redelivers by calling Deliver again.
func (f *Fake) ConsumeDurable(ctx context.Context, cfg natsbus.ConsumerConfig, handler natsbus.MessageHandler) (natsbus.Subscription, error) {
	if err := f.checkOpen("consume " + cfg.Durable); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	sub := &Sub{Config: cfg}
	sub.handler = func(ctx context.Context, msg *natsbus.Message) error {
		// Like the real consumer: the handler's ctx carries the message's tenant,
		// and a message whose tenant is unusable is termed without a handler call.
		ctx, err := tenant.WithTenant(ctx, msg.Metadata.TenantID)
		if err != nil {
			sub.mu.Lock()
			sub.termed++
			sub.mu.Unlock()
			return fmt.Errorf("terminating message on %s: %w", msg.Subject, err)
		}
		err = handler(ctx, msg)
		sub.mu.Lock()
		defer sub.mu.Unlock()
		switch {
		case err == nil:
			sub.acked++
		case errors.Is(err, natsbus.ErrSkip), errors.Is(err, natsbus.ErrPermanent):
			sub.termed++
		default:
			sub.naked++
		}
		return err
	}
	if f.ConsumeDurableFunc == nil {
		return f.register(sub), nil
	}
	got, err := f.ConsumeDurableFunc(ctx, cfg, handler)
	return f.finishSubscribe(sub, got, err)
}

// SubjectMessageCount returns what SubjectMessageCountFunc decides, or an
// ErrUnscripted error when it is unset.
func (f *Fake) SubjectMessageCount(ctx context.Context, stream, subject string) (uint64, error) {
	if err := f.checkOpen("count messages in " + stream); err != nil {
		return 0, err
	}
	if f.SubjectMessageCountFunc == nil {
		return 0, unscripted("SubjectMessageCount")
	}
	return f.SubjectMessageCountFunc(ctx, stream, subject)
}

// LastMessageTime returns what LastMessageTimeFunc decides, or an
// ErrUnscripted error when it is unset.
func (f *Fake) LastMessageTime(ctx context.Context, stream, subject string) (time.Time, bool, error) {
	if err := f.checkOpen("read last message in " + stream); err != nil {
		return time.Time{}, false, err
	}
	if f.LastMessageTimeFunc == nil {
		return time.Time{}, false, unscripted("LastMessageTime")
	}
	return f.LastMessageTimeFunc(ctx, stream, subject)
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

// Subscriptions returns every subscription Subscribe, QueueSubscribe,
// SubscribeWorkerProgress and ConsumeDurable recorded, in call order, including unsubscribed and drained ones.
func (f *Fake) Subscriptions() []*Sub {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.subs)
}

// Deliver calls, synchronously and in subscription order, the handler of every
// live Sub whose subject pattern matches msg.Subject, passing msg to each (it
// is not copied). Like the real client, whose handler context is built from
// context.Background(), handlers get a context that carries none of the
// values (tenant, user, trace) of any caller context; for Subscribe,
// QueueSubscribe and SubscribeWorkerProgress the tenant reaches a handler only
// through msg.Metadata.TenantID. A ConsumeDurable handler instead gets the
// message's tenant in its context, as the real consumer provides it (see
// ConsumeDurable). Deliver calls every handler
// even when some fail and returns their errors joined with errors.Join. It
// ignores Close and skips subscriptions with a nil handler, and returns an
// error for a nil msg.
func (f *Fake) Deliver(msg *natsbus.Message) error {
	if msg == nil {
		return errors.New("natsbustest: Deliver called with a nil message")
	}
	f.mu.Lock()
	subs := slices.Clone(f.subs)
	f.mu.Unlock()

	var errs []error
	for _, s := range subs {
		if s.handler == nil || !s.live() || !s.matches(msg.Subject) {
			continue
		}
		errs = append(errs, s.handler(context.Background(), msg))
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
// QueueSubscribe, SubscribeWorkerProgress or ConsumeDurable. The methods are
// safe for concurrent use.
type Sub struct {
	// Subject is the subscription's subject pattern; empty for ConsumeDurable,
	// which matches Config.FilterSubjects instead.
	Subject string
	// Config is the ConsumerConfig of a ConsumeDurable Sub, zero otherwise.
	Config natsbus.ConsumerConfig
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
	acked        int
	naked        int
	termed       int
}

// matches reports whether subject reaches this Sub: any filter subject of a
// durable consumer (all subjects when it has none), else the Subject pattern.
func (s *Sub) matches(subject string) bool {
	if s.Config.Durable == "" {
		return server.SubjectMatchesFilter(subject, s.Subject)
	}
	if len(s.Config.FilterSubjects) == 0 {
		return true
	}
	return slices.ContainsFunc(s.Config.FilterSubjects, func(filter string) bool {
		return server.SubjectMatchesFilter(subject, filter)
	})
}

// Acked is how many handled messages the consumer would have acknowledged.
func (s *Sub) Acked() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.acked
}

// Naked is how many handled messages the consumer would have redelivered.
func (s *Sub) Naked() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.naked
}

// Termed is how many handled messages the consumer would have terminated.
func (s *Sub) Termed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.termed
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
// matching and liveness, and returns the handler's error. As in Fake.Deliver,
// the handler gets a context without caller values. Use it to feed a
// handler a subject the broker would never route to it. It returns an error
// when the Sub has no handler.
func (s *Sub) Handle(msg *natsbus.Message) error {
	if s.handler == nil {
		return errors.New("natsbustest: Sub has no handler")
	}
	return s.handler(context.Background(), msg)
}
