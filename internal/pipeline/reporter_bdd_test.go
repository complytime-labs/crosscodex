package pipeline_test

import (
	"context"
	"errors"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/analysis"
	"github.com/complytime-labs/crosscodex/internal/pipeline"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
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

		It("logs DB failure but does not return error", func() {
			store.updateStageErr = errors.New("db write failed")

			err := reporter.ReportStageCompleted(ctx, "analyzer-1", "job-1", nil)
			Expect(err).ToNot(HaveOccurred())

			// NATS reporter should still be called
			natsReporter.mu.Lock()
			defer natsReporter.mu.Unlock()
			Expect(natsReporter.completed).To(HaveLen(1))
		})

		It("returns NATS error to caller", func() {
			natsReporter.completeErr = errors.New("nats publish failed")

			err := reporter.ReportStageCompleted(ctx, "analyzer-1", "job-1", nil)
			Expect(err).To(MatchError("nats publish failed"))
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
			Expect(stages[0].ErrorMessage).To(Equal("stage execution failed"))

			// Check NATS reporter was called
			natsReporter.mu.Lock()
			defer natsReporter.mu.Unlock()
			Expect(natsReporter.failed).To(HaveLen(1))
			Expect(natsReporter.failed[0].analyzerName).To(Equal("analyzer-1"))
			Expect(natsReporter.failed[0].jobID).To(Equal("job-1"))
			Expect(natsReporter.failed[0].err).To(Equal(stageErr))
		})

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

			err = reporter.ReportStageCompleted(ctx, "analyzer-1", "job-1", nil)
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
