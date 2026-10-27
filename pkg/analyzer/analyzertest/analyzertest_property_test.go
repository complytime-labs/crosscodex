package analyzertest_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	"google.golang.org/protobuf/types/known/wrapperspb"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/analyzertest"
)

// nameGen draws names analyzer.ValidateName accepts.
func nameGen() *rapid.Generator[string] {
	return rapid.StringMatching(`^[a-z](?:[a-z0-9_-]{0,10}[a-z0-9])?$`)
}

var _ = Describe("Property Specifications", Ordered, func() {
	It("a registered stub reports the drawn name and dependencies", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			name := nameGen().Draw(t, "name")
			deps := rapid.SliceOfNDistinct(nameGen(), 0, 5, rapid.ID[string]).Draw(t, "deps")
			stub := &analyzertest.Stub[*wrapperspb.StringValue]{AnalyzerName: name, Dependencies: deps}

			reg := analyzer.NewRegistry()
			if err := analyzer.Register[*wrapperspb.StringValue](reg, stub); err != nil {
				t.Fatalf("Register(%q): %v", name, err)
			}
			ra, err := reg.Get(name)
			if err != nil {
				t.Fatalf("Get(%q): %v", name, err)
			}
			if ra.Name() != name {
				t.Fatalf("Name() = %q, want %q", ra.Name(), name)
			}
			got := ra.DependsOn()
			if len(got) != len(deps) {
				t.Fatalf("DependsOn() = %v, want %v", got, deps)
			}
			for i := range deps {
				if got[i] != deps[i] {
					t.Fatalf("DependsOn()[%d] = %q, want %q", i, got[i], deps[i])
				}
			}
		})
	})

	It("Inputs preserves call order for any sequence of inputs", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			values := rapid.SliceOf(rapid.String()).Draw(t, "values")
			stub := &analyzertest.Stub[*wrapperspb.StringValue]{}
			for _, v := range values {
				_, _ = stub.GenerateWork(context.Background(), wrapperspb.String(v), analyzer.AnalyzerConfig{})
			}
			got := stub.Inputs()
			if len(got) != len(values) {
				t.Fatalf("recorded %d inputs, want %d", len(got), len(values))
			}
			for i, v := range values {
				if got[i].GetValue() != v {
					t.Fatalf("Inputs()[%d] = %q, want %q", i, got[i].GetValue(), v)
				}
			}
		})
	})
})
