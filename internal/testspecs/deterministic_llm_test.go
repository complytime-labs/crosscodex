package testspecs_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/internal/analyzer/artifacts"
	"github.com/complytime-labs/crosscodex/internal/analyzer/classify"
	"github.com/complytime-labs/crosscodex/internal/analyzer/relationship"
	"github.com/complytime-labs/crosscodex/internal/analyzer/requires"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
)

var _ = Describe("DeterministicLLM", func() {
	var (
		ctx context.Context
		det testspecs.DeterministicLLM
	)

	BeforeEach(func() {
		ctx = context.Background()
		det = testspecs.DeterministicLLM{EmbeddingDim: 8}
	})

	complete := func(prompt string) string {
		resp, err := det.Complete(ctx, &llmclient.CompletionRequest{Model: "m", TenantID: "tenant-a", PromptName: prompt})
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Model).To(Equal("m"))
		Expect(resp.Choices).To(HaveLen(1))
		return resp.Choices[0].Message.Content
	}

	Describe("canned replies parse with the real analyzer parsers", func() {
		It("classify", func() {
			r, err := classify.ParseClassification(complete("classify"))
			Expect(err).NotTo(HaveOccurred())
			Expect(r.String()).To(Equal("Technical|Operational"))
		})

		It("requires: a confident YES vote", func() {
			v := requires.ParseResponse("v", complete("requires"))
			Expect(v.Decision).NotTo(BeNil())
			Expect(*v.Decision).To(BeTrue())
			Expect(v.Confidence).To(Equal("HIGH"))
		})

		It("relationship: a parsed NO_RELATIONSHIP vote", func() {
			v := relationship.ParseResponse(complete("relationship"))
			Expect(v.ParseStatus).To(Equal(relationship.ParseOK))
			Expect(v.Relationship).To(Equal(relationship.RelNoRelationship))
			Expect(v.Confidence).To(Equal(relationship.ConfidenceHigh))
		})

		It("artifacts: the empty sentinel", func() {
			arts, status := artifacts.ParseResponse(complete("artifacts"))
			Expect(status).To(Equal(artifacts.ParseNone))
			Expect(arts).To(BeEmpty())
		})
	})

	DescribeTable("refuses an unknown PromptName, naming it",
		func(prompt string) {
			resp, err := det.Complete(ctx, &llmclient.CompletionRequest{TenantID: "tenant-a", PromptName: prompt})
			Expect(resp).To(BeNil())
			Expect(err).To(MatchError(ContainSubstring("unexpected prompt name")))
			Expect(err).To(MatchError(ContainSubstring(prompt)))
		},
		Entry("empty", ""),
		Entry("unknown", "nope"),
	)

	It("returns the same embedding for the same text across calls and instances", func() {
		req := &llmclient.EmbeddingRequest{Model: "m", TenantID: "tenant-a", Input: []string{"alpha", "beta"}}
		first, err := det.Embed(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		second, err := testspecs.DeterministicLLM{EmbeddingDim: 8}.Embed(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(second.Data).To(Equal(first.Data))
		Expect(first.Data[0].Embedding).NotTo(Equal(first.Data[1].Embedding), "different texts embed differently")
		Expect(first.Model).To(Equal("m"))
	})

	DescribeTable("honours EmbeddingDim",
		func(dim int) {
			resp, err := testspecs.DeterministicLLM{EmbeddingDim: dim}.Embed(ctx,
				&llmclient.EmbeddingRequest{TenantID: "tenant-a", Input: []string{"x", "y", "z"}})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Data).To(HaveLen(3))
			for i, d := range resp.Data {
				Expect(d.Index).To(Equal(i))
				Expect(d.Embedding).To(HaveLen(dim))
			}
		},
		Entry("3", 3),
		Entry("2000", 2000),
	)

	DescribeTable("refuses a non-positive EmbeddingDim, naming the field",
		func(dim int) {
			resp, err := testspecs.DeterministicLLM{EmbeddingDim: dim}.Embed(ctx,
				&llmclient.EmbeddingRequest{TenantID: "tenant-a", Input: []string{"x"}})
			Expect(resp).To(BeNil())
			Expect(err).To(MatchError(ContainSubstring("EmbeddingDim")))
		},
		Entry("zero value", 0),
		Entry("negative", -1),
	)

	It("rejects nil requests with ErrInvalidRequest instead of panicking", func() {
		_, err := det.Complete(ctx, nil)
		Expect(err).To(MatchError(llmclient.ErrInvalidRequest))
		_, err = det.Embed(ctx, nil)
		Expect(err).To(MatchError(llmclient.ErrInvalidRequest))
	})

	DescribeTable("rejects a missing or invalid tenant ID with ErrInvalidRequest",
		func(tenantID string) {
			_, err := det.Complete(ctx, &llmclient.CompletionRequest{TenantID: tenantID, PromptName: "classify"})
			Expect(err).To(MatchError(llmclient.ErrInvalidRequest))
			_, err = det.Embed(ctx, &llmclient.EmbeddingRequest{TenantID: tenantID, Input: []string{"x"}})
			Expect(err).To(MatchError(llmclient.ErrInvalidRequest))
		},
		Entry("empty", ""),
		Entry("invalid characters", "bad tenant!"),
	)

	It("reports healthy and closes cleanly", func() {
		Expect(det.Health(ctx)).To(Succeed())
		Expect(det.Close()).To(Succeed())
	})
})

var _ = Describe("DeterministicLLM Property Specifications", Ordered, func() {
	It("embeddings are a pure function of the input text with length EmbeddingDim", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			dim := rapid.IntRange(1, 64).Draw(t, "dim")
			texts := rapid.SliceOf(rapid.String()).Draw(t, "texts")
			req := &llmclient.EmbeddingRequest{TenantID: "tenant-a", Input: texts}

			a, err := testspecs.DeterministicLLM{EmbeddingDim: dim}.Embed(context.Background(), req)
			if err != nil {
				t.Fatalf("Embed failed: %v", err)
			}
			b, err := testspecs.DeterministicLLM{EmbeddingDim: dim}.Embed(context.Background(), req)
			if err != nil {
				t.Fatalf("second Embed failed: %v", err)
			}
			if len(a.Data) != len(texts) {
				t.Fatalf("got %d vectors for %d inputs", len(a.Data), len(texts))
			}
			for i, d := range a.Data {
				if len(d.Embedding) != dim {
					t.Fatalf("vector %d has length %d, want %d", i, len(d.Embedding), dim)
				}
				for j, v := range d.Embedding {
					if v != b.Data[i].Embedding[j] {
						t.Fatalf("vector %d differs between calls for text %q", i, texts[i])
					}
				}
				for j, other := range texts {
					if other != texts[i] {
						continue
					}
					for k, v := range d.Embedding {
						if v != a.Data[j].Embedding[k] {
							t.Fatalf("identical texts at %d and %d embed differently", i, j)
						}
					}
				}
			}
		})
	})
})
