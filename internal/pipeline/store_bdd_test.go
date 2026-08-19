package pipeline_test

import (
	"context"
	"errors"
	"strings"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/pipeline"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// fakeStore is an in-memory implementation of Store for testing.
// It uses mutex-guarded maps to support concurrent access.
type fakeStore struct {
	mu sync.Mutex

	jobs   map[string]*pipeline.Job
	stages map[string][]*pipeline.Stage // keyed by jobID

	// getJobHook, if set, is called as the first statement of GetJob, before
	// the lock is acquired, so tests can observe or block concurrent GetJob
	// calls without holding up other fakeStore operations.
	getJobHook func()

	createJobErr       error
	getJobErr          error
	listJobsErr        error
	updateStatusErr    error
	createStagesErr    error
	updateStageErr     error
	updateStageErrErr  error
	getStagesErr       error
	getStagesPanics    bool
	resetStagesErr     error
	getResumableJobErr error
	completeStageErr   error
	writeVoteSummErr   error

	analysisResults map[string]map[string][]byte                      // jobID -> analyzerName -> resultData
	voteSummaries   map[string]map[[2]string]pipeline.VoteSummaryPair // jobID -> (sourceID, targetID) -> pair
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		jobs:            make(map[string]*pipeline.Job),
		stages:          make(map[string][]*pipeline.Stage),
		analysisResults: make(map[string]map[string][]byte),
		voteSummaries:   make(map[string]map[[2]string]pipeline.VoteSummaryPair),
	}
}

var _ pipeline.Store = (*fakeStore)(nil)

func (f *fakeStore) CreateJob(ctx context.Context, job *pipeline.Job) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.createJobErr != nil {
		return f.createJobErr
	}

	if _, exists := f.jobs[job.JobID]; exists {
		return errors.New("job already exists")
	}

	f.jobs[job.JobID] = job
	return nil
}

func (f *fakeStore) GetJob(ctx context.Context, jobID string) (*pipeline.Job, error) {
	if f.getJobHook != nil {
		f.getJobHook()
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.getJobErr != nil {
		return nil, f.getJobErr
	}

	job, exists := f.jobs[jobID]
	if !exists {
		return nil, pipeline.ErrNotFound
	}

	// Return a snapshot, not the shared pointer. A real SQL store scans a fresh
	// row each read; handing out the map's live *Job lets callers read fields
	// (e.g. Status) outside the lock while executeJob mutates them under it.
	jobCopy := *job
	return &jobCopy, nil
}

func (f *fakeStore) ListJobs(ctx context.Context, tenantID string, filter pipeline.JobFilter) ([]*pipeline.Job, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.listJobsErr != nil {
		return nil, 0, f.listJobsErr
	}

	var filtered []*pipeline.Job
	for _, job := range f.jobs {
		if job.TenantID != tenantID {
			continue
		}
		if filter.Status != "" && job.Status != filter.Status {
			continue
		}
		jobCopy := *job
		filtered = append(filtered, &jobCopy)
	}

	total := int64(len(filtered))

	// Apply pagination
	start := filter.Offset
	if start > len(filtered) {
		start = len(filtered)
	}

	end := start + filter.Limit
	if filter.Limit == 0 || end > len(filtered) {
		end = len(filtered)
	}

	return filtered[start:end], total, nil
}

func (f *fakeStore) UpdateJobStatus(ctx context.Context, jobID string, status pipeline.JobStatus, jobErr error) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.updateStatusErr != nil {
		return f.updateStatusErr
	}

	job, exists := f.jobs[jobID]
	if !exists {
		return pipeline.ErrNotFound
	}

	job.Status = status
	return nil
}

func (f *fakeStore) CreateStages(ctx context.Context, jobID string, stageNames []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.createStagesErr != nil {
		return f.createStagesErr
	}

	if _, exists := f.jobs[jobID]; !exists {
		return pipeline.ErrNotFound
	}

	for _, stageName := range stageNames {
		stage := &pipeline.Stage{
			JobID:     jobID,
			StageName: stageName,
			Status:    pipeline.StageStatusPending,
		}
		f.stages[jobID] = append(f.stages[jobID], stage)
	}

	return nil
}

func (f *fakeStore) UpdateStageStatus(ctx context.Context, jobID, stageName string, status pipeline.StageStatus) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.updateStageErr != nil {
		return f.updateStageErr
	}

	stages, exists := f.stages[jobID]
	if !exists {
		return pipeline.ErrNotFound
	}

	for _, stage := range stages {
		if stage.StageName == stageName {
			stage.Status = status
			return nil
		}
	}

	return pipeline.ErrNotFound
}

func (f *fakeStore) UpdateStageError(ctx context.Context, jobID, stageName string, stageErr error) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.updateStageErrErr != nil {
		return f.updateStageErrErr
	}

	stages, exists := f.stages[jobID]
	if !exists {
		return pipeline.ErrNotFound
	}

	for _, stage := range stages {
		if stage.StageName == stageName {
			stage.Status = pipeline.StageStatusFailed
			if stageErr != nil {
				stage.ErrorMessage = stageErr.Error()
			}
			return nil
		}
	}

	return pipeline.ErrNotFound
}

func (f *fakeStore) GetStages(ctx context.Context, jobID string) ([]*pipeline.Stage, error) {
	if f.getStagesPanics {
		panic("simulated panic in GetStages")
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.getStagesErr != nil {
		return nil, f.getStagesErr
	}

	stages, exists := f.stages[jobID]
	if !exists {
		return []*pipeline.Stage{}, nil
	}

	// Snapshot each stage: executeJob mutates stored stages under the lock,
	// so callers must not receive live pointers into the map.
	stagesCopy := make([]*pipeline.Stage, len(stages))
	for i, stage := range stages {
		s := *stage
		stagesCopy[i] = &s
	}
	return stagesCopy, nil
}

func (f *fakeStore) ResetStagesFrom(ctx context.Context, jobID, fromStage string, allStages []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.resetStagesErr != nil {
		return f.resetStagesErr
	}

	stages, exists := f.stages[jobID]
	if !exists {
		return pipeline.ErrNotFound
	}

	// Find index of fromStage in allStages
	fromIndex := -1
	for i, stage := range allStages {
		if stage == fromStage {
			fromIndex = i
			break
		}
	}

	if fromIndex == -1 {
		return errors.New("stage not found in allStages")
	}

	// Reset fromStage and all stages after it
	for i := fromIndex; i < len(allStages); i++ {
		for _, stage := range stages {
			if stage.StageName != allStages[i] {
				continue
			}
			stage.Status = pipeline.StageStatusPending
			stage.StartedAt = nil
			stage.CompletedAt = nil
			stage.ErrorMessage = ""
			stage.RetryCount++
		}
	}

	return nil
}

func (f *fakeStore) CompleteAnalysisStage(ctx context.Context, jobID, analyzerName string, resultData []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.completeStageErr != nil {
		return f.completeStageErr
	}

	stages, exists := f.stages[jobID]
	if !exists {
		return pipeline.ErrNotFound
	}

	found := false
	for _, stage := range stages {
		if stage.StageName == analyzerName {
			stage.Status = pipeline.StageStatusCompleted
			found = true
			break
		}
	}
	if !found {
		return pipeline.ErrNotFound
	}

	if f.analysisResults[jobID] == nil {
		f.analysisResults[jobID] = make(map[string][]byte)
	}
	f.analysisResults[jobID][analyzerName] = resultData

	return nil
}

func (f *fakeStore) GetCompletedAnalysisResults(ctx context.Context, jobID string, analyzerNames []string) (map[string]*analyzer.Output, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	outputs := make(map[string]*analyzer.Output, len(analyzerNames))
	byAnalyzer := f.analysisResults[jobID]
	for _, name := range analyzerNames {
		if resultData, ok := byAnalyzer[name]; ok {
			outputs[name] = &analyzer.Output{AnalyzerName: name, ResultData: resultData}
		}
	}
	return outputs, nil
}

func (f *fakeStore) GetResumableJobs(ctx context.Context) ([]*pipeline.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.getResumableJobErr != nil {
		return nil, f.getResumableJobErr
	}

	var resumable []*pipeline.Job
	for _, job := range f.jobs {
		if job.Status == pipeline.JobStatusRunning {
			jobCopy := *job
			resumable = append(resumable, &jobCopy)
		}
	}

	return resumable, nil
}

func (f *fakeStore) WriteVoteSummaries(ctx context.Context, tenantID, jobID string, pairs []pipeline.VoteSummaryPair) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.writeVoteSummErr != nil {
		return f.writeVoteSummErr
	}
	if err := tenant.ValidateTenantID(tenantID); err != nil {
		return err
	}

	if f.voteSummaries[jobID] == nil {
		f.voteSummaries[jobID] = make(map[[2]string]pipeline.VoteSummaryPair)
	}
	for _, p := range pairs {
		key := [2]string{p.SourceID, p.TargetID}
		if _, exists := f.voteSummaries[jobID][key]; exists {
			continue // ON CONFLICT DO NOTHING: leave the existing row untouched.
		}
		f.voteSummaries[jobID][key] = p
	}

	return nil
}

var _ = Describe("fakeStore", func() {
	var (
		store *fakeStore
		ctx   context.Context
	)

	BeforeEach(func() {
		store = newFakeStore()
		ctx = context.Background()
	})

	It("implements the Store interface", func() {
		var _ pipeline.Store = store
	})

	Describe("CreateJob and GetJob", func() {
		It("stores and retrieves a job", func() {
			job := &pipeline.Job{
				JobID:     "job-1",
				TenantID:  "tenant-1",
				Status:    pipeline.JobStatusPending,
				CreatedBy: "user-1",
			}

			err := store.CreateJob(ctx, job)
			Expect(err).ToNot(HaveOccurred())

			retrieved, err := store.GetJob(ctx, "job-1")
			Expect(err).ToNot(HaveOccurred())
			Expect(retrieved.JobID).To(Equal("job-1"))
			Expect(retrieved.TenantID).To(Equal("tenant-1"))
			Expect(retrieved.Status).To(Equal(pipeline.JobStatusPending))
		})

		It("returns ErrNotFound when job does not exist", func() {
			_, err := store.GetJob(ctx, "nonexistent")
			Expect(err).To(Equal(pipeline.ErrNotFound))
		})
	})

	Describe("ListJobs", func() {
		BeforeEach(func() {
			Expect(store.CreateJob(ctx, &pipeline.Job{JobID: "job-1", TenantID: "tenant-1", Status: pipeline.JobStatusPending})).To(Succeed())
			Expect(store.CreateJob(ctx, &pipeline.Job{JobID: "job-2", TenantID: "tenant-1", Status: pipeline.JobStatusRunning})).To(Succeed())
			Expect(store.CreateJob(ctx, &pipeline.Job{JobID: "job-3", TenantID: "tenant-2", Status: pipeline.JobStatusPending})).To(Succeed())
		})

		It("lists all jobs for a tenant", func() {
			jobs, total, err := store.ListJobs(ctx, "tenant-1", pipeline.JobFilter{})
			Expect(err).ToNot(HaveOccurred())
			Expect(total).To(Equal(int64(2)))
			Expect(jobs).To(HaveLen(2))
		})

		It("filters jobs by status", func() {
			jobs, total, err := store.ListJobs(ctx, "tenant-1", pipeline.JobFilter{Status: pipeline.JobStatusRunning})
			Expect(err).ToNot(HaveOccurred())
			Expect(total).To(Equal(int64(1)))
			Expect(jobs).To(HaveLen(1))
			Expect(jobs[0].JobID).To(Equal("job-2"))
		})

		It("applies pagination", func() {
			jobs, total, err := store.ListJobs(ctx, "tenant-1", pipeline.JobFilter{Limit: 1, Offset: 0})
			Expect(err).ToNot(HaveOccurred())
			Expect(total).To(Equal(int64(2)))
			Expect(jobs).To(HaveLen(1))
		})
	})

	Describe("UpdateJobStatus", func() {
		It("updates job status", func() {
			job := &pipeline.Job{JobID: "job-1", TenantID: "tenant-1", Status: pipeline.JobStatusPending}
			Expect(store.CreateJob(ctx, job)).To(Succeed())

			err := store.UpdateJobStatus(ctx, "job-1", pipeline.JobStatusRunning, nil)
			Expect(err).ToNot(HaveOccurred())

			retrieved, _ := store.GetJob(ctx, "job-1")
			Expect(retrieved.Status).To(Equal(pipeline.JobStatusRunning))
		})

		It("returns ErrNotFound when job does not exist", func() {
			err := store.UpdateJobStatus(ctx, "nonexistent", pipeline.JobStatusRunning, nil)
			Expect(err).To(Equal(pipeline.ErrNotFound))
		})
	})

	Describe("CreateStages and GetStages", func() {
		BeforeEach(func() {
			Expect(store.CreateJob(ctx, &pipeline.Job{JobID: "job-1", TenantID: "tenant-1"})).To(Succeed())
		})

		It("creates and retrieves stages", func() {
			stageNames := []string{"stage-1", "stage-2", "stage-3"}
			err := store.CreateStages(ctx, "job-1", stageNames)
			Expect(err).ToNot(HaveOccurred())

			stages, err := store.GetStages(ctx, "job-1")
			Expect(err).ToNot(HaveOccurred())
			Expect(stages).To(HaveLen(3))
			Expect(stages[0].StageName).To(Equal("stage-1"))
			Expect(stages[0].Status).To(Equal(pipeline.StageStatusPending))
		})
	})

	Describe("UpdateStageStatus", func() {
		BeforeEach(func() {
			Expect(store.CreateJob(ctx, &pipeline.Job{JobID: "job-1", TenantID: "tenant-1"})).To(Succeed())
			Expect(store.CreateStages(ctx, "job-1", []string{"stage-1"})).To(Succeed())
		})

		It("updates stage status", func() {
			err := store.UpdateStageStatus(ctx, "job-1", "stage-1", pipeline.StageStatusRunning)
			Expect(err).ToNot(HaveOccurred())

			stages, _ := store.GetStages(ctx, "job-1")
			Expect(stages[0].Status).To(Equal(pipeline.StageStatusRunning))
		})
	})

	Describe("UpdateStageError", func() {
		BeforeEach(func() {
			Expect(store.CreateJob(ctx, &pipeline.Job{JobID: "job-1", TenantID: "tenant-1"})).To(Succeed())
			Expect(store.CreateStages(ctx, "job-1", []string{"stage-1"})).To(Succeed())
		})

		It("marks stage as failed with error message", func() {
			testErr := errors.New("test error")
			err := store.UpdateStageError(ctx, "job-1", "stage-1", testErr)
			Expect(err).ToNot(HaveOccurred())

			stages, _ := store.GetStages(ctx, "job-1")
			Expect(stages[0].Status).To(Equal(pipeline.StageStatusFailed))
			Expect(stages[0].ErrorMessage).To(Equal("test error"))
		})
	})

	Describe("ResetStagesFrom", func() {
		BeforeEach(func() {
			Expect(store.CreateJob(ctx, &pipeline.Job{JobID: "job-1", TenantID: "tenant-1"})).To(Succeed())
			Expect(store.CreateStages(ctx, "job-1", []string{"stage-1", "stage-2", "stage-3"})).To(Succeed())
			Expect(store.UpdateStageStatus(ctx, "job-1", "stage-1", pipeline.StageStatusCompleted)).To(Succeed())
			Expect(store.UpdateStageStatus(ctx, "job-1", "stage-2", pipeline.StageStatusCompleted)).To(Succeed())
			Expect(store.UpdateStageStatus(ctx, "job-1", "stage-3", pipeline.StageStatusFailed)).To(Succeed())
		})

		It("resets fromStage and all subsequent stages", func() {
			allStages := []string{"stage-1", "stage-2", "stage-3"}
			err := store.ResetStagesFrom(ctx, "job-1", "stage-2", allStages)
			Expect(err).ToNot(HaveOccurred())

			stages, _ := store.GetStages(ctx, "job-1")
			Expect(stages[0].Status).To(Equal(pipeline.StageStatusCompleted)) // stage-1 unchanged
			Expect(stages[1].Status).To(Equal(pipeline.StageStatusPending))   // stage-2 reset
			Expect(stages[2].Status).To(Equal(pipeline.StageStatusPending))   // stage-3 reset
			Expect(stages[1].RetryCount).To(Equal(1))
			Expect(stages[2].RetryCount).To(Equal(1))
		})

		It("returns error when fromStage is not in allStages", func() {
			allStages := []string{"stage-1", "stage-2", "stage-3"}
			err := store.ResetStagesFrom(ctx, "job-1", "nonexistent", allStages)
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("GetResumableJobs", func() {
		BeforeEach(func() {
			Expect(store.CreateJob(ctx, &pipeline.Job{JobID: "job-1", TenantID: "tenant-1", Status: pipeline.JobStatusRunning})).To(Succeed())
			Expect(store.CreateJob(ctx, &pipeline.Job{JobID: "job-2", TenantID: "tenant-1", Status: pipeline.JobStatusCompleted})).To(Succeed())
			Expect(store.CreateJob(ctx, &pipeline.Job{JobID: "job-3", TenantID: "tenant-2", Status: pipeline.JobStatusRunning})).To(Succeed())
		})

		It("returns all running jobs across tenants", func() {
			jobs, err := store.GetResumableJobs(ctx)
			Expect(err).ToNot(HaveOccurred())
			Expect(jobs).To(HaveLen(2))
			Expect(jobs[0].Status).To(Equal(pipeline.JobStatusRunning))
			Expect(jobs[1].Status).To(Equal(pipeline.JobStatusRunning))
		})
	})
})

var _ = Describe("PGStore compile-time check", func() {
	It("verifies PGStore implements Store interface", func() {
		var _ pipeline.Store = (*pipeline.PGStore)(nil)
	})
})

var _ = Describe("CompleteAnalysisStage", func() {
	var (
		mockDB *mockTenantConnection
		store  *pipeline.PGStore
		ctx    context.Context
	)

	BeforeEach(func() {
		mockDB = &mockTenantConnection{}
		store = pipeline.NewPGStore(mockDB, nil)

		var err error
		ctx, err = tenant.WithTenant(context.Background(), "tenant-1")
		Expect(err).NotTo(HaveOccurred())
	})

	It("upserts analysis_results and marks the stage completed in one transaction", func() {
		var queries []string
		mockTx := &mockTransaction{
			execFunc: func(ctx context.Context, query string, args ...any) error {
				queries = append(queries, query)
				return nil
			},
			commitFunc: func() error { return nil },
		}
		mockDB.beginFunc = func(ctx context.Context) (db.Transaction, error) { return mockTx, nil }

		err := store.CompleteAnalysisStage(ctx, "job-1", "requires", []byte(`[]`))
		Expect(err).NotTo(HaveOccurred())

		// SET tenant + upsert + stage update, all in the same transaction.
		Expect(queries).To(HaveLen(3))
		Expect(queries[1]).To(ContainSubstring("analysis_results"))
		Expect(queries[1]).To(ContainSubstring("ON CONFLICT (tenant_id, job_id, analyzer_name)"))
		Expect(queries[2]).To(ContainSubstring("job_stages"))
	})

	It("rolls back and returns an error when the upsert fails", func() {
		rolledBack := false
		mockTx := &mockTransaction{
			execFunc: func(ctx context.Context, query string, args ...any) error {
				if strings.Contains(query, "analysis_results") {
					return errors.New("insert failed")
				}
				return nil
			},
			rollbackFunc: func() error {
				rolledBack = true
				return nil
			},
		}
		mockDB.beginFunc = func(ctx context.Context) (db.Transaction, error) { return mockTx, nil }

		err := store.CompleteAnalysisStage(ctx, "job-1", "requires", []byte(`[]`))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("upserting result"))
		Expect(rolledBack).To(BeTrue())
	})

	It("does not update job_stages when the upsert fails", func() {
		var stageUpdated bool
		mockTx := &mockTransaction{
			execFunc: func(ctx context.Context, query string, args ...any) error {
				if strings.Contains(query, "analysis_results") {
					return errors.New("insert failed")
				}
				if strings.Contains(query, "job_stages") {
					stageUpdated = true
				}
				return nil
			},
		}
		mockDB.beginFunc = func(ctx context.Context) (db.Transaction, error) { return mockTx, nil }

		err := store.CompleteAnalysisStage(ctx, "job-1", "requires", []byte(`[]`))
		Expect(err).To(HaveOccurred())
		Expect(stageUpdated).To(BeFalse())
	})

	It("returns an error when the job_stages update fails", func() {
		mockTx := &mockTransaction{
			execFunc: func(ctx context.Context, query string, args ...any) error {
				if strings.Contains(query, "job_stages") {
					return errors.New("update failed")
				}
				return nil
			},
		}
		mockDB.beginFunc = func(ctx context.Context) (db.Transaction, error) { return mockTx, nil }

		err := store.CompleteAnalysisStage(ctx, "job-1", "requires", []byte(`[]`))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("updating stage status"))
	})

	It("returns an error when jobID is empty", func() {
		err := store.CompleteAnalysisStage(ctx, "", "requires", []byte(`[]`))
		Expect(err).To(HaveOccurred())
	})

	It("returns an error when commit fails", func() {
		mockTx := &mockTransaction{
			execFunc: func(ctx context.Context, query string, args ...any) error { return nil },
			commitFunc: func() error {
				return errors.New("commit failed")
			},
		}
		mockDB.beginFunc = func(ctx context.Context) (db.Transaction, error) { return mockTx, nil }

		err := store.CompleteAnalysisStage(ctx, "job-1", "requires", []byte(`[]`))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("commit failed"))
	})
})
