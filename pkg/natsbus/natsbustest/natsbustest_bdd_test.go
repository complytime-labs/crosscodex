package natsbustest_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/natsbus/natsbustest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

func TestNATSBusTest(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "NATSBusTest Suite")
}

var errScripted = errors.New("scripted")

// record is a handler that appends the subject of each message it receives.
func record(into *[]string, mu *sync.Mutex, ret error) natsbus.MessageHandler {
	return func(_ context.Context, msg *natsbus.Message) error {
		mu.Lock()
		defer mu.Unlock()
		*into = append(*into, msg.Subject)
		return ret
	}
}

var _ = Describe("Fake", func() {
	var (
		ctx  context.Context
		fake *natsbustest.Fake
	)

	BeforeEach(func() {
		var err error
		ctx, err = tenant.WithTenant(context.Background(), "tenant-a")
		Expect(err).NotTo(HaveOccurred())
		fake = &natsbustest.Fake{}
	})

	Describe("publishing", func() {
		It("records Publish and PublishWithHeaders in call order", func() {
			Expect(fake.Publish(ctx, "a.b", []byte("one"))).To(Succeed())
			Expect(fake.PublishWithHeaders(ctx, "c.d", []byte("two"), map[string][]string{"K": {"v1", "v2"}})).To(Succeed())
			Expect(fake.Publish(ctx, "e.f", nil)).To(Succeed())

			Expect(fake.Published()).To(Equal([]natsbustest.Publication{
				{Subject: "a.b", Data: []byte("one")},
				{Subject: "c.d", Data: []byte("two"), Headers: map[string][]string{"K": {"v1", "v2"}}},
				{Subject: "e.f"},
			}))
		})

		It("copies data and headers on record, so later caller mutation does not alter the record", func() {
			data := []byte("payload")
			headers := map[string][]string{"K": {"v"}}
			Expect(fake.PublishWithHeaders(ctx, "s", data, headers)).To(Succeed())
			data[0] = 'X'
			headers["K"][0] = "mutated"
			headers["Other"] = []string{"x"}

			got := fake.Published()
			Expect(got[0].Data).To(Equal([]byte("payload")))
			Expect(got[0].Headers).To(Equal(map[string][]string{"K": {"v"}}))
		})

		It("copies data and headers on return, so mutating a returned record does not alter the next", func() {
			Expect(fake.PublishWithHeaders(ctx, "s", []byte("payload"), map[string][]string{"K": {"v"}})).To(Succeed())
			first := fake.Published()
			first[0].Data[0] = 'X'
			first[0].Headers["K"][0] = "mutated"
			delete(first[0].Headers, "K")

			second := fake.Published()
			Expect(second[0].Data).To(Equal([]byte("payload")))
			Expect(second[0].Headers).To(Equal(map[string][]string{"K": {"v"}}))
		})

		DescribeTable("scripted Funcs alone decide the result and the call is still recorded",
			func(call func(*natsbustest.Fake) error) {
				fake.PublishFunc = func(context.Context, string, []byte) error { return errScripted }
				fake.PublishWithHeadersFunc = func(context.Context, string, []byte, map[string][]string) error { return errScripted }
				Expect(call(fake)).To(MatchError(errScripted))
				Expect(fake.Published()).To(HaveLen(1))
			},
			Entry("Publish", func(f *natsbustest.Fake) error { return f.Publish(ctx, "s", []byte("d")) }),
			Entry("PublishWithHeaders", func(f *natsbustest.Fake) error {
				return f.PublishWithHeaders(ctx, "s", []byte("d"), nil)
			}),
		)

		It("rejects a publish without a tenant in the context, before the Func and without recording", func() {
			reached := false
			fake.PublishFunc = func(context.Context, string, []byte) error { reached = true; return nil }
			fake.PublishWithHeadersFunc = func(context.Context, string, []byte, map[string][]string) error {
				reached = true
				return nil
			}

			err := fake.Publish(context.Background(), "s", []byte("d"))
			Expect(err).To(MatchError(tenant.ErrNoTenant))
			Expect(err.Error()).To(ContainSubstring("publish to s: tenant required"))
			Expect(fake.PublishWithHeaders(context.Background(), "s", nil, nil)).To(MatchError(tenant.ErrNoTenant))

			Expect(reached).To(BeFalse())
			Expect(fake.Published()).To(BeEmpty())
		})
	})

	Describe("subscribing", func() {
		It("records Subscribe and QueueSubscribe and returns the recorded Sub", func() {
			var seen []string
			var mu sync.Mutex
			sub, err := fake.Subscribe(ctx, "a.b", record(&seen, &mu, nil))
			Expect(err).NotTo(HaveOccurred())
			qsub, err := fake.QueueSubscribe(ctx, "c.d", "workers", record(&seen, &mu, nil))
			Expect(err).NotTo(HaveOccurred())

			subs := fake.Subscriptions()
			Expect(subs).To(HaveLen(2))
			Expect(subs[0]).To(BeIdenticalTo(sub))
			Expect(subs[0].Subject).To(Equal("a.b"))
			Expect(subs[0].Queue).To(BeEmpty())
			Expect(subs[1]).To(BeIdenticalTo(qsub))
			Expect(subs[1].Subject).To(Equal("c.d"))
			Expect(subs[1].Queue).To(Equal("workers"))
		})

		It("returns a copy of the subscription list", func() {
			_, err := fake.Subscribe(ctx, "a", func(context.Context, *natsbus.Message) error { return nil })
			Expect(err).NotTo(HaveOccurred())
			subs := fake.Subscriptions()
			subs[0] = nil
			Expect(fake.Subscriptions()[0]).NotTo(BeNil())
		})

		It("lets SubscribeFunc fail the call, creating no subscription", func() {
			fake.SubscribeFunc = func(context.Context, string, natsbus.MessageHandler) (natsbus.Subscription, error) {
				return nil, errScripted
			}
			sub, err := fake.Subscribe(ctx, "a", func(context.Context, *natsbus.Message) error { return nil })
			Expect(err).To(MatchError(errScripted))
			Expect(sub).To(BeNil())
			Expect(fake.Subscriptions()).To(BeEmpty())
		})

		It("lets QueueSubscribeFunc fail selected calls and still records the rest", func() {
			fake.QueueSubscribeFunc = func(_ context.Context, subject, _ string, _ natsbus.MessageHandler) (natsbus.Subscription, error) {
				if subject == "bad" {
					return nil, errScripted
				}
				return nil, nil
			}
			h := func(context.Context, *natsbus.Message) error { return nil }
			sub, err := fake.QueueSubscribe(ctx, "good", "q", h)
			Expect(err).NotTo(HaveOccurred())
			Expect(sub).To(BeIdenticalTo(fake.Subscriptions()[0]))
			_, err = fake.QueueSubscribe(ctx, "bad", "q", h)
			Expect(err).To(MatchError(errScripted))
			Expect(fake.Subscriptions()).To(HaveLen(1))
		})

		It("returns the subscription a Func hands back instead of the recorded Sub", func() {
			custom := &natsbustest.Sub{Subject: "custom"}
			fake.SubscribeFunc = func(context.Context, string, natsbus.MessageHandler) (natsbus.Subscription, error) {
				return custom, nil
			}
			sub, err := fake.Subscribe(ctx, "a", func(context.Context, *natsbus.Message) error { return nil })
			Expect(err).NotTo(HaveOccurred())
			Expect(sub).To(BeIdenticalTo(custom))
			Expect(fake.Subscriptions()).To(HaveLen(1))
		})

		It("accepts a nil handler and never panics delivering to it", func() {
			_, err := fake.Subscribe(ctx, "a", nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.Deliver(&natsbus.Message{Subject: "a"})).To(Succeed())
			Expect(fake.Subscriptions()[0].Handle(&natsbus.Message{Subject: "a"})).To(HaveOccurred())
		})
	})

	Describe("Deliver", func() {
		var (
			seen []string
			mu   sync.Mutex
		)

		BeforeEach(func() { seen = nil })

		DescribeTable("matches subjects like NATS",
			func(pattern, subject string, want bool) {
				_, err := fake.Subscribe(ctx, pattern, record(&seen, &mu, nil))
				Expect(err).NotTo(HaveOccurred())
				Expect(fake.Deliver(&natsbus.Message{Subject: subject})).To(Succeed())
				if want {
					Expect(seen).To(Equal([]string{subject}))
				} else {
					Expect(seen).To(BeEmpty())
				}
			},
			Entry("exact", "a.b.c", "a.b.c", true),
			Entry("different token", "a.b.c", "a.b.d", false),
			Entry("prefix is not a match", "a.b", "a.b.c", false),
			Entry("longer pattern is not a match", "a.b.c", "a.b", false),
			Entry("star matches one token", "a.*.c", "a.x.c", true),
			Entry("star does not match two tokens", "a.*.c", "a.x.y.c", false),
			Entry("star does not match zero tokens", "a.*.c", "a.c", false),
			Entry("leading and trailing stars", "*.b.*", "x.b.y", true),
			Entry("full wildcard pattern", "crosscodex.catalog.*.*.parsed", "crosscodex.catalog.t.c.parsed", true),
			Entry("full wildcard pattern, too many tokens", "crosscodex.catalog.*.*.parsed", "crosscodex.catalog.t.c.parsed.x", false),
			Entry("full wildcard pattern, too few tokens", "crosscodex.catalog.*.*.parsed", "crosscodex.catalog.x", false),
			Entry("tail matches one trailing token", "a.>", "a.b", true),
			Entry("tail matches many trailing tokens", "a.>", "a.b.c.d", true),
			Entry("tail needs at least one token", "a.>", "a", false),
			Entry("tail respects the prefix", "a.>", "b.c", false),
			Entry("lone tail matches everything", ">", "x.y", true),
			Entry("star inside a token is literal", "a*", "ab", false),
		)

		It("reaches only live subscriptions, in subscription order", func() {
			s1, _ := fake.Subscribe(ctx, "a", record(&seen, &mu, nil))
			_, _ = fake.Subscribe(ctx, "a", record(&seen, &mu, nil))
			s3, _ := fake.QueueSubscribe(ctx, "a", "q", record(&seen, &mu, nil))
			_, _ = fake.Subscribe(ctx, "other", record(&seen, &mu, nil))

			Expect(s1.Unsubscribe()).To(Succeed())
			Expect(s3.Drain()).To(Succeed())
			Expect(fake.Deliver(&natsbus.Message{Subject: "a"})).To(Succeed())
			Expect(seen).To(HaveLen(1))
		})

		It("passes the message to the handler", func() {
			var gotMsg *natsbus.Message
			_, _ = fake.Subscribe(ctx, "a", func(_ context.Context, m *natsbus.Message) error {
				gotMsg = m
				return nil
			})
			msg := &natsbus.Message{Subject: "a", Data: []byte("d")}
			Expect(fake.Deliver(msg)).To(Succeed())
			Expect(gotMsg).To(BeIdenticalTo(msg))
		})

		DescribeTable("gives the handler a context without the subscriber's tenant, as the real client does",
			func(deliver func(*natsbustest.Fake, *natsbus.Message) error) {
				// ctx carries tenant-a; production handler contexts never do.
				var handlerErr error
				_, _ = fake.Subscribe(ctx, "a", func(c context.Context, _ *natsbus.Message) error {
					_, handlerErr = tenant.FromContext(c)
					return nil
				})
				Expect(deliver(fake, &natsbus.Message{Subject: "a",
					Metadata: natsbus.MessageMetadata{TenantID: "tenant-a"}})).To(Succeed())
				Expect(handlerErr).To(MatchError(tenant.ErrNoTenant))
			},
			Entry("Deliver", func(f *natsbustest.Fake, m *natsbus.Message) error { return f.Deliver(m) }),
			Entry("Sub.Handle", func(f *natsbustest.Fake, m *natsbus.Message) error { return f.Subscriptions()[0].Handle(m) }),
		)

		It("joins every handler error and still calls every handler", func() {
			errOther := errors.New("other")
			_, _ = fake.Subscribe(ctx, "a", record(&seen, &mu, errScripted))
			_, _ = fake.Subscribe(ctx, "a", record(&seen, &mu, nil))
			_, _ = fake.Subscribe(ctx, "a", record(&seen, &mu, errOther))

			err := fake.Deliver(&natsbus.Message{Subject: "a"})
			Expect(err).To(MatchError(errScripted))
			Expect(err).To(MatchError(errOther))
			Expect(seen).To(HaveLen(3))
		})

		It("returns nil when no subscription matches", func() {
			Expect(fake.Deliver(&natsbus.Message{Subject: "a"})).To(Succeed())
		})

		It("returns an error for a nil message instead of panicking", func() {
			_, _ = fake.Subscribe(ctx, "a", record(&seen, &mu, nil))
			Expect(fake.Deliver(nil)).To(HaveOccurred())
			Expect(seen).To(BeEmpty())
		})

		It("lets a handler call back into the fake", func() {
			_, _ = fake.Subscribe(ctx, "a", func(context.Context, *natsbus.Message) error {
				return fake.Publish(ctx, "reply", []byte("r"))
			})
			Expect(fake.Deliver(&natsbus.Message{Subject: "a"})).To(Succeed())
			Expect(fake.Published()).To(HaveLen(1))
		})
	})

	Describe("Sub", func() {
		It("starts live and reports Unsubscribe and Drain", func() {
			sub, _ := fake.Subscribe(ctx, "a", func(context.Context, *natsbus.Message) error { return nil })
			s := fake.Subscriptions()[0]
			Expect(s.Unsubscribed()).To(BeFalse())
			Expect(s.Drained()).To(BeFalse())

			Expect(sub.Drain()).To(Succeed())
			Expect(s.Drained()).To(BeTrue())
			Expect(s.Unsubscribed()).To(BeFalse())
			Expect(sub.Unsubscribe()).To(Succeed())
			Expect(s.Unsubscribed()).To(BeTrue())
		})

		It("returns the injected errors and still records the call", func() {
			sub, _ := fake.Subscribe(ctx, "a", func(context.Context, *natsbus.Message) error { return nil })
			s := fake.Subscriptions()[0]
			s.UnsubscribeErr = errScripted
			s.DrainErr = errors.New("drain failed")

			Expect(sub.Unsubscribe()).To(MatchError(errScripted))
			Expect(sub.Drain()).To(MatchError("drain failed"))
			Expect(s.Unsubscribed()).To(BeTrue())
			Expect(s.Drained()).To(BeTrue())
		})

		It("Handle bypasses matching and liveness", func() {
			var seen []string
			var mu sync.Mutex
			sub, _ := fake.Subscribe(ctx, "a.b", record(&seen, &mu, errScripted))
			Expect(sub.Unsubscribe()).To(Succeed())

			err := fake.Subscriptions()[0].Handle(&natsbus.Message{Subject: "totally.different"})
			Expect(err).To(MatchError(errScripted))
			Expect(seen).To(Equal([]string{"totally.different"}))
		})

		It("is safe on the zero value", func() {
			var s natsbustest.Sub
			Expect(s.Unsubscribe()).To(Succeed())
			Expect(s.Drain()).To(Succeed())
			Expect(s.Handle(&natsbus.Message{})).To(HaveOccurred())
		})
	})

	Describe("streams", func() {
		DescribeTable("refuses an unscripted call, naming the field to set",
			func(call func(*natsbustest.Fake) error, field string) {
				err := call(fake)
				Expect(err).To(MatchError(natsbustest.ErrUnscripted))
				Expect(err.Error()).To(ContainSubstring(field))
			},
			Entry("CreateStream", func(f *natsbustest.Fake) error {
				return f.CreateStream(ctx, natsbus.StreamConfig{Name: "S"})
			}, "Fake.CreateStreamFunc"),
			Entry("DeleteStream", func(f *natsbustest.Fake) error { return f.DeleteStream(ctx, "S") }, "Fake.DeleteStreamFunc"),
			Entry("AuditStreamRetention", func(f *natsbustest.Fake) error {
				got, err := f.AuditStreamRetention(ctx)
				Expect(got).To(BeNil())
				return err
			}, "Fake.AuditStreamRetentionFunc"),
		)

		It("lets the Funcs alone decide the result", func() {
			var gotCfg natsbus.StreamConfig
			var gotName string
			fake.CreateStreamFunc = func(_ context.Context, cfg natsbus.StreamConfig) error { gotCfg = cfg; return nil }
			fake.DeleteStreamFunc = func(_ context.Context, name string) error { gotName = name; return errScripted }
			fake.AuditStreamRetentionFunc = func(context.Context) (map[string]time.Duration, error) {
				return map[string]time.Duration{"AUDIT_LLM": time.Hour}, nil
			}

			Expect(fake.CreateStream(ctx, natsbus.StreamConfig{Name: "S", MaxAge: time.Minute})).To(Succeed())
			Expect(gotCfg).To(Equal(natsbus.StreamConfig{Name: "S", MaxAge: time.Minute}))
			Expect(fake.DeleteStream(ctx, "S")).To(MatchError(errScripted))
			Expect(gotName).To(Equal("S"))
			got, err := fake.AuditStreamRetention(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(map[string]time.Duration{"AUDIT_LLM": time.Hour}))
		})
	})

	Describe("Close", func() {
		BeforeEach(func() {
			fake.CreateStreamFunc = func(context.Context, natsbus.StreamConfig) error { return nil }
			fake.DeleteStreamFunc = func(context.Context, string) error { return nil }
			fake.AuditStreamRetentionFunc = func(context.Context) (map[string]time.Duration, error) { return nil, nil }
		})

		It("returns nil, repeatedly", func() {
			Expect(fake.Close()).To(Succeed())
			Expect(fake.Close()).To(Succeed())
		})

		It("makes every other method fail with ErrConnectionClosed, before the Func and without recording", func() {
			reached := false
			fake.PublishFunc = func(context.Context, string, []byte) error { reached = true; return nil }
			h := func(context.Context, *natsbus.Message) error { return nil }
			Expect(fake.Close()).To(Succeed())

			Expect(fake.Publish(ctx, "s", nil)).To(MatchError(natsbus.ErrConnectionClosed))
			Expect(fake.PublishWithHeaders(ctx, "s", nil, nil)).To(MatchError(natsbus.ErrConnectionClosed))
			_, err := fake.Subscribe(ctx, "s", h)
			Expect(err).To(MatchError(natsbus.ErrConnectionClosed))
			Expect(err.Error()).To(ContainSubstring("subscribe to s"))
			_, err = fake.QueueSubscribe(ctx, "s", "q", h)
			Expect(err).To(MatchError(natsbus.ErrConnectionClosed))
			Expect(fake.CreateStream(ctx, natsbus.StreamConfig{Name: "S"})).To(MatchError(natsbus.ErrConnectionClosed))
			Expect(fake.DeleteStream(ctx, "S")).To(MatchError(natsbus.ErrConnectionClosed))
			_, err = fake.AuditStreamRetention(ctx)
			Expect(err).To(MatchError(natsbus.ErrConnectionClosed))

			Expect(reached).To(BeFalse())
			Expect(fake.Published()).To(BeEmpty())
			Expect(fake.Subscriptions()).To(BeEmpty())
		})
	})

	It("is safe for concurrent use", func() {
		var seen []string
		var mu sync.Mutex
		var wg sync.WaitGroup
		for range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer GinkgoRecover()
				_, err := fake.Subscribe(ctx, "a", record(&seen, &mu, nil))
				Expect(err).NotTo(HaveOccurred())
				Expect(fake.Publish(ctx, "a", []byte("x"))).To(Succeed())
				Expect(fake.PublishWithHeaders(ctx, "a", []byte("y"), map[string][]string{"K": {"v"}})).To(Succeed())
				Expect(fake.Deliver(&natsbus.Message{Subject: "a"})).To(Succeed())
				_ = fake.Published()
				for _, s := range fake.Subscriptions() {
					_ = s.Drained()
				}
			}()
		}
		wg.Wait()
		Expect(fake.Published()).To(HaveLen(40))
		Expect(fake.Subscriptions()).To(HaveLen(20))
	})
})
