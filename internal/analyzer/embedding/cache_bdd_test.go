package embedding_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/analyzer/embedding"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/resultcache"
)

var _ = Describe("embedding result reuse", func() {
	a := func() *embedding.EmbeddingAnalyzer {
		return embedding.New(nil, nil, nil, config.EmbeddingConfig{Enabled: true, Models: []string{"m1", "m2"}, MaxChars: 1500, BatchSize: 50})
	}
	control := &pb.Control{ControlId: "c1", Identifier: "C1", Statement: "Encrypt data at rest.", Parts: map[string]string{"class": "compliance-requirement"}}

	It("names every configured model as an embedding model", func() {
		Expect(a().CacheModels()).To(Equal([]analyzer.ModelUse{
			{Alias: "m1", Kind: analyzer.ModelEmbedding}, {Alias: "m2", Kind: analyzer.ModelEmbedding},
		}))
	})

	It("gives each model its own key, and the same control the same keys in two jobs", func() {
		keysIn := func(jobID string) []string {
			tasks, err := a().GenerateWork(testspecs.SetupTenantContext("test-tenant"), control,
				analyzer.AnalyzerConfig{Parameters: map[string]string{"job_id": jobID}})
			Expect(err).NotTo(HaveOccurred())
			var keys []string
			for _, t := range tasks {
				req, ok := a().CacheIdentity(t)
				Expect(ok).To(BeTrue())
				Expect(req.Identity.Temperature).To(BeNil())
				keys = append(keys, resultcache.Key("embedding", req.Identity, "observed", ""))
			}
			return keys
		}
		first := keysIn("job-1")
		Expect(first).To(HaveLen(2))
		Expect(first[0]).NotTo(Equal(first[1]))
		Expect(keysIn("job-2")).To(Equal(first))
	})

	It("changes the keys when the control text changes", func() {
		keysOf := func(c *pb.Control) []string {
			tasks, err := a().GenerateWork(testspecs.SetupTenantContext("test-tenant"), c, analyzer.AnalyzerConfig{})
			Expect(err).NotTo(HaveOccurred())
			var keys []string
			for _, t := range tasks {
				req, ok := a().CacheIdentity(t)
				Expect(ok).To(BeTrue())
				keys = append(keys, resultcache.Key("embedding", req.Identity, "observed", ""))
			}
			return keys
		}
		edited := &pb.Control{ControlId: "c1", Identifier: "C1", Statement: "Encrypt data in transit.", Parts: control.Parts}
		for _, k := range keysOf(control) {
			Expect(keysOf(edited)).NotTo(ContainElement(k))
		}
	})
})
