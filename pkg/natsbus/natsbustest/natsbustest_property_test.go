package natsbustest_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	. "github.com/onsi/ginkgo/v2"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/natsbus/natsbustest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// subjectGen draws a subject from a small token alphabet so patterns and
// subjects collide often enough to exercise matching.
func subjectGen(label string) *rapid.Generator[string] {
	token := rapid.SampledFrom([]string{"a", "b", "c"})
	return rapid.Custom(func(t *rapid.T) string {
		tokens := rapid.SliceOfN(token, 1, 4).Draw(t, label)
		return joinTokens(tokens)
	})
}

func joinTokens(tokens []string) string {
	out := tokens[0]
	for _, tok := range tokens[1:] {
		out += "." + tok
	}
	return out
}

var _ = Describe("Property Specifications", Ordered, func() {
	It("Published preserves order and content across both publish methods", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			ctx, err := tenant.WithTenant(context.Background(), "tenant-a")
			if err != nil {
				t.Fatalf("tenant context: %v", err)
			}
			type call struct {
				Subject string
				Data    []byte
				Headers map[string][]string
				WithHdr bool
			}
			calls := rapid.SliceOf(rapid.Custom(func(t *rapid.T) call {
				return call{
					Subject: subjectGen("subject").Draw(t, "s"),
					Data:    rapid.SliceOf(rapid.Byte()).Draw(t, "data"),
					Headers: rapid.MapOf(rapid.StringMatching(`[A-Z][a-z]{0,5}`), rapid.SliceOf(rapid.String())).Draw(t, "headers"),
					WithHdr: rapid.Bool().Draw(t, "withHdr"),
				}
			})).Draw(t, "calls")

			fake := &natsbustest.Fake{}
			for _, c := range calls {
				if c.WithHdr {
					err = fake.PublishWithHeaders(ctx, c.Subject, c.Data, c.Headers)
				} else {
					err = fake.Publish(ctx, c.Subject, c.Data)
				}
				if err != nil {
					t.Fatalf("publish: %v", err)
				}
			}

			got := fake.Published()
			if len(got) != len(calls) {
				t.Fatalf("recorded %d publications for %d calls", len(got), len(calls))
			}
			for i, c := range calls {
				if got[i].Subject != c.Subject || !slices.Equal(got[i].Data, c.Data) {
					t.Fatalf("publication %d = %+v, want subject %q data %v", i, got[i], c.Subject, c.Data)
				}
				if !c.WithHdr {
					if got[i].Headers != nil {
						t.Fatalf("publication %d has headers %v, want none", i, got[i].Headers)
					}
					continue
				}
				if len(got[i].Headers) != len(c.Headers) {
					t.Fatalf("publication %d headers = %v, want %v", i, got[i].Headers, c.Headers)
				}
				for k, v := range c.Headers {
					if !slices.Equal(got[i].Headers[k], v) {
						t.Fatalf("publication %d header %q = %v, want %v", i, k, got[i].Headers[k], v)
					}
				}
			}
		})
	})

	It("PublishDurable records each tenant-scoped call once with Durable true", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			ctx, err := tenant.WithTenant(context.Background(), "tenant-a")
			if err != nil {
				t.Fatalf("tenant context: %v", err)
			}
			subjects := rapid.SliceOf(subjectGen("subject")).Draw(t, "subjects")

			fake := &natsbustest.Fake{}
			for _, s := range subjects {
				if err := fake.PublishDurable(ctx, s, []byte(s), nil); err != nil {
					t.Fatalf("PublishDurable: %v", err)
				}
			}

			got := fake.Published()
			if len(got) != len(subjects) {
				t.Fatalf("recorded %d publications for %d calls", len(got), len(subjects))
			}
			for i, s := range subjects {
				if got[i].Subject != s || !got[i].Durable {
					t.Fatalf("publication %d = %+v, want durable publication on %q", i, got[i], s)
				}
			}
		})
	})

	It("Deliver reaches every live matching subscription and no other", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			ctx := context.Background()
			fake := &natsbustest.Fake{}
			patterns := rapid.SliceOfN(rapid.Custom(func(t *rapid.T) string {
				tokens := rapid.SliceOfN(rapid.SampledFrom([]string{"a", "b", "c", "*"}), 1, 4).Draw(t, "tokens")
				if rapid.Bool().Draw(t, "tail") {
					tokens = append(tokens, ">")
				}
				return joinTokens(tokens)
			}), 1, 8).Draw(t, "patterns")

			// Build the subject from one pattern by construction ("*" becomes a
			// concrete token, ">" one to three), so at least that pattern
			// matches without consulting any matcher.
			source := rapid.IntRange(0, len(patterns)-1).Draw(t, "source")
			var subjectTokens []string
			for _, tok := range strings.Split(patterns[source], ".") {
				switch tok {
				case "*":
					subjectTokens = append(subjectTokens, rapid.SampledFrom([]string{"a", "b", "c"}).Draw(t, "star"))
				case ">":
					subjectTokens = append(subjectTokens, rapid.SliceOfN(rapid.SampledFrom([]string{"a", "b", "c"}), 1, 3).Draw(t, "tail")...)
				default:
					subjectTokens = append(subjectTokens, tok)
				}
			}
			subject := joinTokens(subjectTokens)

			reached := make([]bool, len(patterns))
			subs := make([]natsbus.Subscription, len(patterns))
			for i, p := range patterns {
				sub, err := fake.Subscribe(ctx, p, func(context.Context, *natsbus.Message) error {
					reached[i] = true
					return nil
				})
				if err != nil {
					t.Fatalf("subscribe: %v", err)
				}
				subs[i] = sub
			}
			live := make([]bool, len(patterns))
			for i := range subs {
				live[i] = rapid.Bool().Draw(t, "live")
				if !live[i] {
					_ = subs[i].Unsubscribe()
				}
			}

			if err := fake.Deliver(&natsbus.Message{Subject: subject}); err != nil {
				t.Fatalf("deliver: %v", err)
			}
			if live[source] && !reached[source] {
				t.Fatalf("live pattern %q the subject %q was built from was not reached", patterns[source], subject)
			}
			for i, p := range patterns {
				// SubjectMatchesFilter is the NATS server's own matcher, used
				// as ground truth for the remaining patterns; liveness is the
				// property under test.
				want := live[i] && server.SubjectMatchesFilter(subject, p)
				if reached[i] != want {
					t.Fatalf("pattern %q subject %q live=%v: reached=%v, want %v", p, subject, live[i], reached[i], want)
				}
			}
		})
	})
	It("a durable consumer's acked, naked and termed counts sum to its deliveries", func() {
		outcomes := rapid.SampledFrom([]error{nil, natsbus.ErrPermanent, errors.New("transient")})
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			fake := &natsbustest.Fake{}
			script := rapid.SliceOf(outcomes).Draw(t, "script")
			next := 0
			sub, err := fake.ConsumeDurable(context.Background(),
				natsbus.ConsumerConfig{Stream: "S", Durable: "d", MaxDeliver: 5, FilterSubjects: []string{"a.*"}},
				func(context.Context, *natsbus.Message) error { next++; return script[next-1] })
			if err != nil {
				t.Fatalf("consume: %v", err)
			}
			wantAck, wantNak, wantTerm := 0, 0, 0
			for _, o := range script {
				switch {
				case o == nil:
					wantAck++
				case errors.Is(o, natsbus.ErrPermanent):
					wantTerm++
				default:
					wantNak++
				}
				_ = fake.Deliver(&natsbus.Message{Subject: "a.x", Metadata: natsbus.MessageMetadata{TenantID: "tenant-a"}})
			}
			s := sub.(*natsbustest.Sub)
			if s.Acked() != wantAck || s.Naked() != wantNak || s.Termed() != wantTerm {
				t.Fatalf("outcomes acked/naked/termed = %d/%d/%d, want %d/%d/%d",
					s.Acked(), s.Naked(), s.Termed(), wantAck, wantNak, wantTerm)
			}
			if got := s.Acked() + s.Naked() + s.Termed(); got != len(script) {
				t.Fatalf("outcomes sum to %d, want %d deliveries", got, len(script))
			}
		})
	})

	It("LastMessageTime passes the stream and subject to LastMessageTimeFunc and returns its answer unchanged", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			stream := rapid.StringMatching(`[A-Z_]{1,12}`).Draw(t, "stream")
			subject := rapid.StringMatching(`[a-z]{1,8}(\.[a-z]{1,8}){0,4}`).Draw(t, "subject")
			found := rapid.Bool().Draw(t, "found")
			at := time.Unix(rapid.Int64Range(0, 1<<40).Draw(t, "unix"), 0).UTC()
			fake := &natsbustest.Fake{LastMessageTimeFunc: func(_ context.Context, gotStream, gotSubject string) (time.Time, bool, error) {
				if gotStream != stream || gotSubject != subject {
					t.Fatalf("got (%q, %q), want (%q, %q)", gotStream, gotSubject, stream, subject)
				}
				return at, found, nil
			}}
			got, gotFound, err := fake.LastMessageTime(context.Background(), stream, subject)
			if err != nil || gotFound != found || !got.Equal(at) {
				t.Fatalf("LastMessageTime = (%v, %v, %v), want (%v, %v, nil)", got, gotFound, err, at, found)
			}
		})
	})

	It("ConsumeDurableFunc records exactly the consumers it lets succeed", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			script := rapid.SliceOf(rapid.Bool()).Draw(t, "succeed")
			fake := &natsbustest.Fake{ConsumeDurableFunc: func(_ context.Context, cfg natsbus.ConsumerConfig, _ natsbus.MessageHandler) (natsbus.Subscription, error) {
				if strings.HasPrefix(cfg.Durable, "fail-") {
					return nil, errors.New("scripted")
				}
				return nil, nil
			}}
			var want []string
			for i, ok := range script {
				name := fmt.Sprintf("ok-%d", i)
				if !ok {
					name = fmt.Sprintf("fail-%d", i)
				}
				_, err := fake.ConsumeDurable(context.Background(), natsbus.ConsumerConfig{Stream: "S", Durable: name, MaxDeliver: 1}, nil)
				if ok != (err == nil) {
					t.Fatalf("consumer %s: err=%v, want success=%v", name, err, ok)
				}
				if ok {
					want = append(want, name)
				}
			}
			var got []string
			for _, s := range fake.Subscriptions() {
				got = append(got, s.Config.Durable)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("recorded consumers %v, want %v", got, want)
			}
		})
	})

	It("a tick subscription is signalled exactly by deliveries to WorkerProgressSubject", func() {
		subjects := rapid.SampledFrom([]string{
			natsbus.WorkerProgressSubject,
			natsbus.WorkerProgressSubject + ".a",
			"crosscodex.workers",
			"crosscodex.workers.a",
			"crosscodex.results.tenant-a.classify.job-1",
			"a.b",
		})
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			fake := &natsbustest.Fake{}
			ticks := 0
			if _, err := fake.SubscribeWorkerProgress(context.Background(), func() { ticks++ }); err != nil {
				t.Fatalf("subscribe worker progress: %v", err)
			}
			want := 0
			for _, s := range rapid.SliceOf(subjects).Draw(t, "subjects") {
				if s == natsbus.WorkerProgressSubject {
					want++
				}
				if err := fake.Deliver(&natsbus.Message{Subject: s, Data: []byte(s)}); err != nil {
					t.Fatalf("deliver %q: %v", s, err)
				}
			}
			if ticks != want {
				t.Fatalf("tick handler signalled %d times, want %d", ticks, want)
			}
		})
	})

	It("ticks interleave with publishes in Published, in call order, without data or headers", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			ctx, err := tenant.WithTenant(context.Background(), "tenant-a")
			if err != nil {
				t.Fatalf("tenant context: %v", err)
			}
			fake := &natsbustest.Fake{}
			isTick := rapid.SliceOf(rapid.Bool()).Draw(t, "isTick")
			for i, tick := range isTick {
				if tick {
					err = fake.PublishWorkerProgress(ctx)
				} else {
					err = fake.Publish(ctx, "a.b", []byte{byte(i)})
				}
				if err != nil {
					t.Fatalf("call %d: %v", i, err)
				}
			}
			got := fake.Published()
			if len(got) != len(isTick) {
				t.Fatalf("recorded %d publications for %d calls", len(got), len(isTick))
			}
			for i, tick := range isTick {
				if tick {
					if got[i].Subject != natsbus.WorkerProgressSubject || got[i].Data != nil || got[i].Headers != nil {
						t.Fatalf("publication %d = %+v, want a bare tick", i, got[i])
					}
					continue
				}
				if got[i].Subject != "a.b" || !slices.Equal(got[i].Data, []byte{byte(i)}) {
					t.Fatalf("publication %d = %+v, want subject a.b data %v", i, got[i], []byte{byte(i)})
				}
			}
		})
	})
})
