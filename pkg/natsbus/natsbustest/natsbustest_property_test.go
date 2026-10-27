package natsbustest_test

import (
	"context"
	"slices"
	"strings"

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
})
