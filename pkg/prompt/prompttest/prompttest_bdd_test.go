package prompttest_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/prompt"
	"github.com/complytime-labs/crosscodex/pkg/prompt/prompttest"
)

func TestPromptTest(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "PromptTest Suite")
}

var errScripted = errors.New("scripted")

var _ = Describe("Fake", func() {
	var (
		ctx  context.Context
		fake *prompttest.Fake
	)

	BeforeEach(func() {
		ctx = context.Background()
		fake = &prompttest.Fake{}
	})

	Describe("boundary parity with prompt.NewRegistry", func() {
		// The Fake does no input validation because the real registry
		// rejects no input: odd names and nil vars are answered by
		// resolution (ErrPromptNotFound), not by a boundary check.
		It("answers an empty name with ErrPromptNotFound, not an input error", func() {
			reg, err := prompt.NewRegistry(config.PromptConfig{})
			Expect(err).NotTo(HaveOccurred())

			_, err = reg.Resolve(ctx, "")
			Expect(err).To(MatchError(prompt.ErrPromptNotFound))
			_, err = reg.Render(ctx, "", nil)
			Expect(err).To(MatchError(prompt.ErrPromptNotFound))
		})

		It("passes an empty name and nil vars through to the scripted Func", func() {
			var gotName string
			var gotVars map[string]string
			fake.RenderFunc = func(_ context.Context, name string, vars map[string]string) (*prompt.ResolvedPrompt, error) {
				gotName, gotVars = name, vars
				return &prompt.ResolvedPrompt{}, nil
			}
			_, err := fake.Render(ctx, "", nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(gotName).To(BeEmpty())
			Expect(gotVars).To(BeNil())
		})
	})

	Describe("unscripted calls", func() {
		It("returns ErrUnscripted naming the Func for Resolve", func() {
			spec, err := fake.Resolve(ctx, "p")
			Expect(spec).To(BeNil())
			Expect(err).To(MatchError(prompttest.ErrUnscripted))
			Expect(err).To(MatchError(ContainSubstring("Fake.ResolveFunc is not set")))
		})

		It("returns ErrUnscripted naming the Func for Render, and still records the call", func() {
			rp, err := fake.Render(ctx, "p", map[string]string{"k": "v"})
			Expect(rp).To(BeNil())
			Expect(err).To(MatchError(prompttest.ErrUnscripted))
			Expect(err).To(MatchError(ContainSubstring("Fake.RenderFunc is not set")))
			Expect(fake.Renders()).To(Equal([]prompttest.RenderCall{{Name: "p", Vars: map[string]string{"k": "v"}}}))
		})

		It("returns ErrUnscripted naming the Func for List", func() {
			names, err := fake.List(ctx)
			Expect(names).To(BeNil())
			Expect(err).To(MatchError(prompttest.ErrUnscripted))
			Expect(err).To(MatchError(ContainSubstring("Fake.ListFunc is not set")))
		})

		It("returns ErrUnscripted naming the Func for Layers", func() {
			infos, err := fake.Layers(ctx, "p")
			Expect(infos).To(BeNil())
			Expect(err).To(MatchError(prompttest.ErrUnscripted))
			Expect(err).To(MatchError(ContainSubstring("Fake.LayersFunc is not set")))
		})
	})

	Describe("scripted calls", func() {
		It("lets each Func alone decide its method's result", func() {
			spec := &prompt.PromptSpec{Name: "p"}
			rp := &prompt.ResolvedPrompt{Name: "p", ContentHash: "h"}
			layers := []prompt.LayerInfo{{ID: "embedded", HasPrompt: true}}
			fake.ResolveFunc = func(_ context.Context, name string) (*prompt.PromptSpec, error) {
				Expect(name).To(Equal("p"))
				return spec, nil
			}
			fake.RenderFunc = func(context.Context, string, map[string]string) (*prompt.ResolvedPrompt, error) {
				return rp, nil
			}
			fake.ListFunc = func(context.Context) ([]string, error) { return []string{"p"}, nil }
			fake.LayersFunc = func(context.Context, string) ([]prompt.LayerInfo, error) { return layers, nil }

			gotSpec, err := fake.Resolve(ctx, "p")
			Expect(err).NotTo(HaveOccurred())
			Expect(gotSpec).To(BeIdenticalTo(spec))
			gotRP, err := fake.Render(ctx, "p", nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(gotRP).To(BeIdenticalTo(rp))
			names, err := fake.List(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(names).To(Equal([]string{"p"}))
			gotLayers, err := fake.Layers(ctx, "p")
			Expect(err).NotTo(HaveOccurred())
			Expect(gotLayers).To(Equal(layers))
		})

		It("returns the error a Func scripts, wrapped sentinels intact", func() {
			fake.ResolveFunc = func(context.Context, string) (*prompt.PromptSpec, error) {
				return nil, errScripted
			}
			fake.RenderFunc = func(context.Context, string, map[string]string) (*prompt.ResolvedPrompt, error) {
				return nil, prompt.ErrMissingPlaceholder
			}
			_, err := fake.Resolve(ctx, "p")
			Expect(err).To(MatchError(errScripted))
			_, err = fake.Render(ctx, "p", nil)
			Expect(err).To(MatchError(prompt.ErrMissingPlaceholder))
		})

		It("hands the Func the caller's ctx", func() {
			type key struct{}
			marked := context.WithValue(ctx, key{}, "v")
			fake.ListFunc = func(c context.Context) ([]string, error) {
				Expect(c.Value(key{})).To(Equal("v"))
				return nil, nil
			}
			_, err := fake.List(marked)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Renders recorder", func() {
		It("records every Render call in order, even when RenderFunc fails", func() {
			fake.RenderFunc = func(_ context.Context, name string, _ map[string]string) (*prompt.ResolvedPrompt, error) {
				if name == "bad" {
					return nil, errScripted
				}
				return &prompt.ResolvedPrompt{Name: name}, nil
			}
			_, _ = fake.Render(ctx, "a", map[string]string{"x": "1"})
			_, err := fake.Render(ctx, "bad", nil)
			Expect(err).To(MatchError(errScripted))
			Expect(fake.Renders()).To(Equal([]prompttest.RenderCall{
				{Name: "a", Vars: map[string]string{"x": "1"}},
				{Name: "bad"},
			}))
		})

		It("does not record Resolve, List or Layers calls", func() {
			fake.ResolveFunc = func(context.Context, string) (*prompt.PromptSpec, error) { return nil, nil }
			_, _ = fake.Resolve(ctx, "p")
			_, _ = fake.List(ctx)
			_, _ = fake.Layers(ctx, "p")
			Expect(fake.Renders()).To(BeEmpty())
		})

		It("is unaffected by the caller mutating its vars after the call", func() {
			vars := map[string]string{"k": "v"}
			_, _ = fake.Render(ctx, "p", vars)
			vars["k"] = "mutated"
			vars["extra"] = "x"
			Expect(fake.Renders()[0].Vars).To(Equal(map[string]string{"k": "v"}))
		})

		It("returns copies, so editing the result leaves the record intact", func() {
			_, _ = fake.Render(ctx, "p", map[string]string{"k": "v"})
			got := fake.Renders()
			got[0].Name = "mutated"
			got[0].Vars["k"] = "mutated"
			Expect(fake.Renders()).To(Equal([]prompttest.RenderCall{{Name: "p", Vars: map[string]string{"k": "v"}}}))
		})

		It("keeps a nil vars map nil", func() {
			_, _ = fake.Render(ctx, "p", nil)
			Expect(fake.Renders()[0].Vars).To(BeNil())
		})
	})

	Describe("concurrency", func() {
		It("records every call from 20 goroutines", func() {
			fake.RenderFunc = func(context.Context, string, map[string]string) (*prompt.ResolvedPrompt, error) {
				return &prompt.ResolvedPrompt{}, nil
			}
			var wg sync.WaitGroup
			for range 20 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer GinkgoRecover()
					_, err := fake.Render(ctx, "p", map[string]string{"k": "v"})
					Expect(err).NotTo(HaveOccurred())
					_ = fake.Renders()
				}()
			}
			wg.Wait()
			Expect(fake.Renders()).To(HaveLen(20))
		})
	})
})
