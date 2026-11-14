package pipeline_test

import (
	"context"
	"errors"
	"fmt"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/analysis"
	"github.com/complytime-labs/crosscodex/internal/pipeline"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
)

// fakeNATSReporter implements analysis.StageReporter for testing.
// It records all calls and returns configured errors.
type fakeNATSReporter struct {
	mu sync.Mutex

	started   []call
	completed []call
	failed    []call

	startErr    error
	completeErr error
	failErr     error
}

type call struct {
	analyzerName string
	jobID        string
	output       *analyzer.Output
	err          error
}

var _ analysis.StageReporter = (*fakeNATSReporter)(nil)

func (f *fakeNATSReporter) ReportStageStarted(ctx context.Context, analyzerName, jobID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.started = append(f.started, call{analyzerName: analyzerName, jobID: jobID})
	return f.startErr
}

func (f *fakeNATSReporter) ReportStageCompleted(ctx context.Context, analyzerName, jobID string, output *analyzer.Output) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.completed = append(f.completed, call{analyzerName: analyzerName, jobID: jobID, output: output})
	return f.completeErr
}

func (f *fakeNATSReporter) ReportStageFailed(ctx context.Context, analyzerName, jobID string, stageErr error) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.failed = append(f.failed, call{analyzerName: analyzerName, jobID: jobID, err: stageErr})
	return f.failErr
}

var _ = Describe("DBStageReporter", func() {
	var (
		ctx          context.Context
		store        *fakeStore
		natsReporter *fakeNATSReporter
		reporter     *pipeline.DBStageReporter
	)

	BeforeEach(func() {
		ctx = context.Background()
		store = newFakeStore()
		natsReporter = &fakeNATSReporter{}
		reporter = pipeline.NewDBStageReporter(natsReporter, store)

		// Create a job and stage for testing
		job := &pipeline.Job{
			JobID:     "job-1",
			TenantID:  "tenant-1",
			Status:    pipeline.JobStatusPending,
			CreatedBy: "test-user",
		}
		Expect(store.CreateJob(ctx, job)).To(Succeed())
		Expect(store.CreateStages(ctx, "job-1", []string{"analyzer-1"})).To(Succeed())
	})

	Describe("ReportStageStarted", func() {
		It("updates DB to running and calls NATS reporter", func() {
			err := reporter.ReportStageStarted(ctx, "analyzer-1", "job-1")
			Expect(err).ToNot(HaveOccurred())

			// Check DB was updated
			stages, _ := store.GetStages(ctx, "job-1")
			Expect(stages).To(HaveLen(1))
			Expect(stages[0].Status).To(Equal(pipeline.StageStatusRunning))

			// Check NATS reporter was called
			natsReporter.mu.Lock()
			defer natsReporter.mu.Unlock()
			Expect(natsReporter.started).To(HaveLen(1))
			Expect(natsReporter.started[0].analyzerName).To(Equal("analyzer-1"))
			Expect(natsReporter.started[0].jobID).To(Equal("job-1"))
		})

		It("logs DB failure but does not return error", func() {
			store.updateStageErr = errors.New("db write failed")

			err := reporter.ReportStageStarted(ctx, "analyzer-1", "job-1")
			Expect(err).ToNot(HaveOccurred())

			// NATS reporter should still be called
			natsReporter.mu.Lock()
			defer natsReporter.mu.Unlock()
			Expect(natsReporter.started).To(HaveLen(1))
		})

		It("returns NATS error to caller", func() {
			natsReporter.startErr = errors.New("nats publish failed")

			err := reporter.ReportStageStarted(ctx, "analyzer-1", "job-1")
			Expect(err).To(MatchError("nats publish failed"))
		})
	})

	Describe("ReportStageCompleted", func() {
		It("updates DB to completed and calls NATS reporter", func() {
			output := &analyzer.Output{
				Metadata: map[string]string{"foo": "bar"},
			}

			err := reporter.ReportStageCompleted(ctx, "analyzer-1", "job-1", output)
			Expect(err).ToNot(HaveOccurred())

			// Check DB was updated
			stages, _ := store.GetStages(ctx, "job-1")
			Expect(stages).To(HaveLen(1))
			Expect(stages[0].Status).To(Equal(pipeline.StageStatusCompleted))

			// Check NATS reporter was called
			natsReporter.mu.Lock()
			defer natsReporter.mu.Unlock()
			Expect(natsReporter.completed).To(HaveLen(1))
			Expect(natsReporter.completed[0].analyzerName).To(Equal("analyzer-1"))
			Expect(natsReporter.completed[0].jobID).To(Equal("job-1"))
			Expect(natsReporter.completed[0].output).To(Equal(output))
		})

		It("persists a JSON null and marks the stage completed when ResultData is empty", func() {
			err := reporter.ReportStageCompleted(ctx, "analyzer-1", "job-1", &analyzer.Output{})
			Expect(err).ToNot(HaveOccurred())

			// A zero-task analyzer still gets a durable analysis_results row
			// (as JSON null), so a resumed job can find it via
			// GetCompletedAnalysisResults instead of redundantly re-running it.
			store.mu.Lock()
			Expect(store.analysisResults["job-1"]["analyzer-1"]).To(Equal([]byte("null")))
			store.mu.Unlock()

			stages, _ := store.GetStages(ctx, "job-1")
			Expect(stages[0].Status).To(Equal(pipeline.StageStatusCompleted))

			natsReporter.mu.Lock()
			defer natsReporter.mu.Unlock()
			Expect(natsReporter.completed).To(HaveLen(1))
		})

		It("returns an error and does not call NATS when persisting a zero-ResultData stage fails", func() {
			store.completeStageErr = errors.New("db write failed")

			err := reporter.ReportStageCompleted(ctx, "analyzer-1", "job-1", &analyzer.Output{})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("db write failed"))

			natsReporter.mu.Lock()
			defer natsReporter.mu.Unlock()
			Expect(natsReporter.completed).To(BeEmpty())
		})

		It("returns NATS error to caller", func() {
			natsReporter.completeErr = errors.New("nats publish failed")

			err := reporter.ReportStageCompleted(ctx, "analyzer-1", "job-1", &analyzer.Output{})
			Expect(err).To(MatchError("nats publish failed"))
		})

		It("persists the result and calls NATS reporter when ResultData is present", func() {
			output := &analyzer.Output{
				ResultData: []byte(`[{"source_id":"AC-1","target_id":"AC-2"}]`),
			}

			err := reporter.ReportStageCompleted(ctx, "analyzer-1", "job-1", output)
			Expect(err).ToNot(HaveOccurred())

			// CompleteAnalysisStage persisted the result and marked the stage completed.
			store.mu.Lock()
			Expect(store.analysisResults["job-1"]["analyzer-1"]).To(Equal(output.ResultData))
			store.mu.Unlock()

			stages, _ := store.GetStages(ctx, "job-1")
			Expect(stages[0].Status).To(Equal(pipeline.StageStatusCompleted))

			// NATS reporter should still be called.
			natsReporter.mu.Lock()
			defer natsReporter.mu.Unlock()
			Expect(natsReporter.completed).To(HaveLen(1))
		})

		It("persists the attempt that sealed the stage, from the output's metadata, with the completion", func() {
			output := &analyzer.Output{Metadata: map[string]string{analysis.MetadataCacheAttempt: "attempt-1"}}

			Expect(reporter.ReportStageCompleted(ctx, "analyzer-1", "job-1", output)).To(Succeed())

			stages, _ := store.GetStages(ctx, "job-1")
			Expect(stages[0].Status).To(Equal(pipeline.StageStatusCompleted))
			Expect(stages[0].CacheAttempt).To(Equal("attempt-1"))
		})

		It("returns an error and does not call NATS when persisting the result fails", func() {
			store.completeStageErr = errors.New("db write failed")
			output := &analyzer.Output{
				ResultData: []byte(`[{"source_id":"AC-1","target_id":"AC-2"}]`),
			}

			err := reporter.ReportStageCompleted(ctx, "analyzer-1", "job-1", output)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("db write failed"))

			// NATS reporter must NOT be called: persistence failure blocks publishing.
			natsReporter.mu.Lock()
			defer natsReporter.mu.Unlock()
			Expect(natsReporter.completed).To(BeEmpty())
		})
	})

	Describe("graph deferral", func() {
		withStages := func(names ...string) {
			Expect(store.CreateStages(ctx, "job-2", names)).To(Succeed())
		}
		completedMetadata := func() map[string]string {
			natsReporter.mu.Lock()
			defer natsReporter.mu.Unlock()
			Expect(natsReporter.completed).To(HaveLen(1))
			return natsReporter.completed[0].output.Metadata
		}

		BeforeEach(func() {
			Expect(store.CreateJob(ctx, &pipeline.Job{JobID: "job-2", TenantID: "tenant-1", Status: pipeline.JobStatusRunning, CreatedBy: "test-user"})).To(Succeed())
		})

		DescribeTable("marks a pair analyzer's completed event graph_deferred when the job has a critique stage, keeping its metadata",
			func(analyzerName string) {
				withStages("classify", analyzerName, results.CritiqueAnalyzer, "synthesis")
				output := &analyzer.Output{ResultData: []byte(`[]`), Metadata: map[string]string{"total_count": "4"}}

				Expect(reporter.ReportStageCompleted(ctx, analyzerName, "job-2", output)).To(Succeed())

				Expect(completedMetadata()).To(Equal(map[string]string{"total_count": "4", results.GraphDeferredMetadataKey: "true"}))
				Expect(output.Metadata).To(Equal(map[string]string{"total_count": "4"}), "the engine's output must not change")
			},
			Entry("relationship", results.RelationshipAnalyzer),
			Entry("requires", results.RequiresAnalyzer),
		)

		DescribeTable("does not mark an event graph_deferred",
			func(analyzerName string, stages []string) {
				withStages(stages...)
				Expect(reporter.ReportStageCompleted(ctx, analyzerName, "job-2", &analyzer.Output{ResultData: []byte(`[]`)})).To(Succeed())
				Expect(completedMetadata()).NotTo(HaveKey(results.GraphDeferredMetadataKey))
			},
			Entry("for relationship when the job has no critique stage",
				results.RelationshipAnalyzer, []string{results.RelationshipAnalyzer, results.RequiresAnalyzer, "synthesis"}),
			Entry("for requires when the job has no critique stage",
				results.RequiresAnalyzer, []string{results.RequiresAnalyzer, "synthesis"}),
			Entry("for another analyzer of a job with a critique stage",
				"classify", []string{"classify", results.RelationshipAnalyzer, results.CritiqueAnalyzer}),
			Entry("for the critique stage itself",
				results.CritiqueAnalyzer, []string{results.RelationshipAnalyzer, results.CritiqueAnalyzer}),
		)

		It("fails without persisting or publishing when the job's stages cannot be read", func() {
			withStages(results.RelationshipAnalyzer, results.CritiqueAnalyzer)
			store.getStagesErr = errors.New("connection lost")

			err := reporter.ReportStageCompleted(ctx, results.RelationshipAnalyzer, "job-2", &analyzer.Output{ResultData: []byte(`[]`)})
			Expect(err).To(MatchError(ContainSubstring("connection lost")))
			Expect(err).To(MatchError(ContainSubstring(`cannot tell whether job job-2 runs the critique stage`)))
			Expect(err).To(MatchError(ContainSubstring("resume the job")))

			store.mu.Lock()
			Expect(store.analysisResults["job-2"]).NotTo(HaveKey(results.RelationshipAnalyzer))
			store.mu.Unlock()
			store.getStagesErr = nil
			stages, err := store.GetStages(ctx, "job-2")
			Expect(err).NotTo(HaveOccurred())
			Expect(stages[0].Status).NotTo(Equal(pipeline.StageStatusCompleted))
			natsReporter.mu.Lock()
			defer natsReporter.mu.Unlock()
			Expect(natsReporter.completed).To(BeEmpty())
		})
	})

	Describe("ReportStageFailed", func() {
		It("updates DB with error and calls NATS reporter", func() {
			stageErr := errors.New("stage execution failed")

			err := reporter.ReportStageFailed(ctx, "analyzer-1", "job-1", stageErr)
			Expect(err).ToNot(HaveOccurred())

			// Check DB was updated
			stages, _ := store.GetStages(ctx, "job-1")
			Expect(stages).To(HaveLen(1))
			Expect(stages[0].Status).To(Equal(pipeline.StageStatusFailed))

			// Check NATS reporter was called
			natsReporter.mu.Lock()
			defer natsReporter.mu.Unlock()
			Expect(natsReporter.failed).To(HaveLen(1))
			Expect(natsReporter.failed[0].analyzerName).To(Equal("analyzer-1"))
			Expect(natsReporter.failed[0].jobID).To(Equal("job-1"))
			Expect(natsReporter.failed[0].err).To(Equal(stageErr))
		})

		DescribeTable("records and publishes nothing when the job's context is done, leaving the outcome to the pipeline",
			func(cause error) {
				Expect(reporter.ReportStageStarted(ctx, "analyzer-1", "job-1")).To(Succeed())
				jobCtx, cancel := context.WithCancelCause(ctx)
				cancel(cause)

				err := reporter.ReportStageFailed(jobCtx, "analyzer-1", "job-1", fmt.Errorf("await results: %w", context.Canceled))
				Expect(err).NotTo(HaveOccurred())

				stages, err := store.GetStages(ctx, "job-1")
				Expect(err).NotTo(HaveOccurred())
				Expect(stages).To(ConsistOf(HaveField("Status", pipeline.StageStatusRunning)))
				natsReporter.mu.Lock()
				defer natsReporter.mu.Unlock()
				Expect(natsReporter.failed).To(BeEmpty())
			},
			Entry("cancelled by CancelJob", pipeline.ErrJobCancelled),
			Entry("cancelled by a shutdown", errors.New("service stopping")),
		)

		// GetJob returns the stage's message to the tenant (CWE-209): an
		// analyzer's raw error is withheld there. The NATS reporter, which
		// logs it in full, still gets the error itself.
		DescribeTable("records the tenant's message on the stage and publishes the error itself",
			func(stageErr error, want string) {
				Expect(reporter.ReportStageFailed(ctx, "analyzer-1", "job-1", stageErr)).To(Succeed())

				stages, err := store.GetStages(ctx, "job-1")
				Expect(err).NotTo(HaveOccurred())
				Expect(stages[0].ErrorMessage).To(Equal(want))
				natsReporter.mu.Lock()
				defer natsReporter.mu.Unlock()
				Expect(natsReporter.failed).To(HaveLen(1))
				Expect(natsReporter.failed[0].err).To(BeIdenticalTo(stageErr))
			},
			Entry("an analyzer's raw error: the generic message",
				fmt.Errorf("analysis: analyzer %q GenerateWork failed: %w: %w", "analyzer-1", analysis.ErrAnalyzerFailed,
					errors.New(`SECRET pq: relation "controls" does not exist`)),
				`job job-1 failed because of an internal error (error class retryable). `+
					`Resume it with "crosscodex run retry --from-failure job-1", which keeps its completed stages, `+
					`or start a new job: crosscodex run start --source <source-catalog-id> --target <target-catalog-id>. `+
					`If it fails again, give your operator job ID job-1 (tracing is off, so there is no trace ID).`),
			Entry("a stage error: its message",
				&analysis.StageError{Analyzer: "analyzer-1", Sentinel: pipeline.ErrLLMUnavailable,
					Cause: errors.New("SECRET dial tcp"), Message: "Stage \"analyzer-1\" stopped."},
				`Stage "analyzer-1" stopped.`),
		)

		It("logs DB failure but does not return error", func() {
			store.updateStageErrErr = errors.New("db write failed")
			stageErr := errors.New("stage execution failed")

			err := reporter.ReportStageFailed(ctx, "analyzer-1", "job-1", stageErr)
			Expect(err).ToNot(HaveOccurred())

			// NATS reporter should still be called
			natsReporter.mu.Lock()
			defer natsReporter.mu.Unlock()
			Expect(natsReporter.failed).To(HaveLen(1))
		})

		It("returns NATS error to caller", func() {
			natsReporter.failErr = errors.New("nats publish failed")
			stageErr := errors.New("stage execution failed")

			err := reporter.ReportStageFailed(ctx, "analyzer-1", "job-1", stageErr)
			Expect(err).To(MatchError("nats publish failed"))
		})
	})

	Describe("integration behavior", func() {
		It("DB failure does not prevent NATS publish", func() {
			// Simulate DB being down
			store.updateStageErr = errors.New("db connection lost")
			store.updateStageErrErr = errors.New("db connection lost")

			// All three operations should succeed at NATS level
			err := reporter.ReportStageStarted(ctx, "analyzer-1", "job-1")
			Expect(err).ToNot(HaveOccurred())

			err = reporter.ReportStageCompleted(ctx, "analyzer-1", "job-1", &analyzer.Output{})
			Expect(err).ToNot(HaveOccurred())

			err = reporter.ReportStageFailed(ctx, "analyzer-1", "job-1", errors.New("test"))
			Expect(err).ToNot(HaveOccurred())

			// NATS should have received all calls
			natsReporter.mu.Lock()
			defer natsReporter.mu.Unlock()
			Expect(natsReporter.started).To(HaveLen(1))
			Expect(natsReporter.completed).To(HaveLen(1))
			Expect(natsReporter.failed).To(HaveLen(1))
		})
	})
})
