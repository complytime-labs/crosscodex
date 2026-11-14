package pipeline_test

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/analyzer/critique"
	"github.com/complytime-labs/crosscodex/internal/pipeline"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// countingResults is a fakeStore that counts its analysis-result reads.
type countingResults struct {
	*fakeStore
	reads int
}

func (c *countingResults) GetCompletedAnalysisResults(ctx context.Context, jobID string, names []string) (map[string]*analyzer.Output, error) {
	c.reads++
	return c.fakeStore.GetCompletedAnalysisResults(ctx, jobID, names)
}

// failingResults is a fakeStore whose analysis-result reads fail.
type failingResults struct{ *fakeStore }

func (failingResults) GetCompletedAnalysisResults(context.Context, string, []string) (map[string]*analyzer.Output, error) {
	return nil, errors.New("connection reset")
}

var _ = Describe("CritiqueVerdictProvider", func() {
	var (
		ctx   context.Context
		store *fakeStore
	)

	BeforeEach(func() {
		ctx = testspecs.SetupTenantContext("tenant-a")
		store = newFakeStore()
	})

	stored := func(analyzerName, resultData string) {
		if store.analysisResults["job-1"] == nil {
			store.analysisResults["job-1"] = map[string][]byte{}
		}
		store.analysisResults["job-1"][analyzerName] = []byte(resultData)
	}

	It("proposes every stored relationship and requires result with its justifications", func() {
		stored("relationship", `[
			{"source_id":"AC-1","target_id":"PR-1","relationship_type":"SUPPORTS","confidence":0.7,"properties":{},"justifications":["j1","j2"]},
			{"source_id":"AC-2","target_id":"PR-2","relationship_type":"NO_RELATIONSHIP","confidence":0.9,"properties":{}}
		]`)
		stored("requires", `[{"source_id":"AC-1","target_id":"PR-1","confidence":1,"unanimous":true,"valid_votes":3,"total_votes":3,"models":["m"],"justifications":["needs it"]}]`)

		got, err := pipeline.NewCritiqueVerdictProvider(store).Verdicts(ctx, "tenant-a", "job-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal([]critique.ProposedVerdict{
			{Kind: "relationship", SourceControlID: "AC-1", TargetControlID: "PR-1", Verdict: "SUPPORTS", Justifications: []string{"j1", "j2"}},
			{Kind: "requires", SourceControlID: "AC-1", TargetControlID: "PR-1", Verdict: "REQUIRES", Justifications: []string{"needs it"}},
		}))
	})

	DescribeTable("proposes nothing for a stage with nothing to review",
		func(results map[string]string) {
			for name, data := range results {
				stored(name, data)
			}
			got, err := pipeline.NewCritiqueVerdictProvider(store).Verdicts(ctx, "tenant-a", "job-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(BeEmpty())
		},
		Entry("no stored result (the analyzer is disabled)", map[string]string{}),
		Entry("a zero-task analyzer's null result", map[string]string{"relationship": "null", "requires": "null"}),
		Entry("empty results", map[string]string{"relationship": "[]", "requires": "[]"}),
	)

	It("reads results stored before justifications were recorded, with none", func() {
		stored("requires", `[{"source_id":"AC-1","target_id":"PR-1","confidence":1,"unanimous":true,"valid_votes":1,"total_votes":1,"models":["m"]}]`)
		got, err := pipeline.NewCritiqueVerdictProvider(store).Verdicts(ctx, "tenant-a", "job-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(HaveLen(1))
		Expect(got[0].Justifications).To(BeEmpty())
	})

	It("refuses a corrupt stored result, naming the analyzer and the job", func() {
		stored("relationship", `{"not":"a list"}`)
		_, err := pipeline.NewCritiqueVerdictProvider(store).Verdicts(ctx, "tenant-a", "job-1")
		Expect(err).To(MatchError(ContainSubstring("the stored relationship result of job job-1 does not decode")))
	})

	It("reports a failed read", func() {
		_, err := pipeline.NewCritiqueVerdictProvider(failingResults{store}).Verdicts(ctx, "tenant-a", "job-1")
		Expect(err).To(MatchError(ContainSubstring("connection reset")))
	})

	It("refuses a tenant other than the context's, without reading", func() {
		stored("requires", `[{"source_id":"AC-1","target_id":"PR-1","models":["m"]}]`)
		reader := &countingResults{fakeStore: store}
		// The same reader answers the context's own tenant, so a zero count
		// below means the mismatch stopped the read, not the fake.
		got, err := pipeline.NewCritiqueVerdictProvider(reader).Verdicts(ctx, "tenant-a", "job-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(HaveLen(1))
		reader.reads = 0

		got, err = pipeline.NewCritiqueVerdictProvider(reader).Verdicts(ctx, "tenant-b", "job-1")
		Expect(err).To(MatchError(tenant.ErrTenantMismatch))
		Expect(got).To(BeNil())
		Expect(reader.reads).To(BeZero())
	})
})
