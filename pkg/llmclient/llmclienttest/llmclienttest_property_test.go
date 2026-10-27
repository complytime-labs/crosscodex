package llmclienttest_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/llmclient/llmclienttest"
)

var _ = Describe("Property Specifications", Ordered, func() {
	It("Fake records every Complete request in order whether or not CompleteFunc is set", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			models := rapid.SliceOf(rapid.String()).Draw(t, "models")
			fake := &llmclienttest.Fake{}
			if rapid.Bool().Draw(t, "scripted") {
				fake.CompleteFunc = func(context.Context, *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
					return &llmclient.CompletionResponse{}, nil
				}
			}
			for _, m := range models {
				_, _ = fake.Complete(context.Background(), &llmclient.CompletionRequest{Model: m, TenantID: "tenant-a", Messages: []llmclient.ChatMessage{{Role: llmclient.RoleUser, Content: "hi"}}})
			}
			got := fake.CompleteRequests()
			if len(got) != len(models) {
				t.Fatalf("recorded %d requests for %d calls", len(got), len(models))
			}
			for i, r := range got {
				if r.Model != models[i] {
					t.Fatalf("request %d has model %q, want %q", i, r.Model, models[i])
				}
			}
		})
	})
})
