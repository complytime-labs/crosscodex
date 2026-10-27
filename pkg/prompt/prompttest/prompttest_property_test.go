package prompttest_test

import (
	"context"
	"maps"

	. "github.com/onsi/ginkgo/v2"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/prompt"
	"github.com/complytime-labs/crosscodex/pkg/prompt/prompttest"
)

var _ = Describe("Property Specifications", Ordered, func() {
	It("Renders preserves call order and content for any sequence of calls", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			names := rapid.SliceOfN(rapid.String(), 0, 8).Draw(t, "names")
			varsFor := rapid.MapOf(rapid.String(), rapid.String())
			fake := &prompttest.Fake{
				RenderFunc: func(context.Context, string, map[string]string) (*prompt.ResolvedPrompt, error) {
					return &prompt.ResolvedPrompt{}, nil
				},
			}
			want := make([]prompttest.RenderCall, len(names))
			for i, name := range names {
				vars := varsFor.Draw(t, "vars")
				want[i] = prompttest.RenderCall{Name: name, Vars: maps.Clone(vars)}
				if _, err := fake.Render(context.Background(), name, vars); err != nil {
					t.Fatalf("Render(%q): %v", name, err)
				}
				// Mutating the caller's map must not reach the record.
				vars["mutated-after-call"] = "x"
			}

			got := fake.Renders()
			if len(got) != len(want) {
				t.Fatalf("Renders() has %d calls, want %d", len(got), len(want))
			}
			for i := range want {
				if got[i].Name != want[i].Name {
					t.Fatalf("Renders()[%d].Name = %q, want %q", i, got[i].Name, want[i].Name)
				}
				if !maps.Equal(got[i].Vars, want[i].Vars) {
					t.Fatalf("Renders()[%d].Vars = %v, want %v", i, got[i].Vars, want[i].Vars)
				}
			}
		})
	})
})
