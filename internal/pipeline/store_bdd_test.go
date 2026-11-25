package pipeline_test

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/analysis"
	"github.com/complytime-labs/crosscodex/internal/pipeline"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// fakeStore is an in-memory implementation of Store for testing.
// It uses mutex-guarded maps to support concurrent access. Like PGStore,
// whose every method begins a transaction on ctx, it refuses a cancelled or
// expired context, so a write made on a job's own context after the job is
// cancelled is lost here as it is against PostgreSQL.
type fakeStore struct {
	mu sync.Mutex

	jobs   map[string]*pipeline.Job
	stages map[string][]*pipeline.Stage // keyed by jobID

	// getJobHook, if set, is called as the first statement of GetJob, before
	// the lock is acquired, so tests can observe or block concurrent GetJob
	// calls without holding up other fakeStore operations.
	getJobHook func()

	// beforeUpdateJobStatus and afterUpdateStageStatus, if set, are called
	// outside the lock: the first before UpdateJobStatus does anything, the
	// second after UpdateStageStatus has stored a status. Specs use them to
	// act at an exact point of a job's lifecycle.
	beforeUpdateJobStatus  func(jobID string, status pipeline.JobStatus)
	afterUpdateStageStatus func(jobID, stageName string, status pipeline.StageStatus)

	createJobErr    error
	getJobErr       error
	listJobsErr     error
	updateStatusErr error
	// updateStatusErrOn, if set, limits updateStatusErr to updates to this
	// status, so a spec can fail one transition (such as cancelled) while
	// the job still starts.
	updateStatusErrOn  pipeline.JobStatus
	createStagesErr    error
	updateStageErr     error
	updateStageErrErr  error
	getStagesErr       error
	getStagesPanics    bool
	resetStagesErr     error
	getResumableJobErr error
	completeStageErr   error
	writeVoteSummErr   error
	getProgressErr     error

	analysisResults map[string]map[string][]byte                      // jobID -> analyzerName -> resultData
	voteSummaries   map[string]map[[3]string]pipeline.VoteSummaryPair // jobID -> (sourceID, targetID, contributionType) -> row
	progress        map[string]map[string]pipeline.StageProgress      // jobID -> stageName -> stored progress

	// failures records each job's last failure error as UpdateJobStatus
	// received it; see failureCause.
	failures map[string]error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		jobs:            make(map[string]*pipeline.Job),
		stages:          make(map[string][]*pipeline.Stage),
		analysisResults: make(map[string]map[string][]byte),
		voteSummaries:   make(map[string]map[[3]string]pipeline.VoteSummaryPair),
		progress:        make(map[string]map[string]pipeline.StageProgress),
		failures:        make(map[string]error),
	}
}

// failureCause returns the error job jobID failed with: the one failJob
// logs in full. The job's ErrorMessage is the tenant's message instead,
// and the error failJob records unwraps to the cause.
func (f *fakeStore) failureCause(jobID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	cause := errors.Unwrap(f.failures[jobID])
	Expect(cause).NotTo(BeNil(), "job %s failed through failJob", jobID)
	return cause.Error()
}

var _ pipeline.Store = (*fakeStore)(nil)

func (f *fakeStore) CreateJob(ctx context.Context, job *pipeline.Job) error {
	if err := ctx.Err(); err != nil {
		return err
	}
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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
	// As PGStore does, a ctx without a tenant is refused and, under RLS,
	// another tenant's job is not found.
	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		return nil, err
	}
	if tenantID != job.TenantID {
		return nil, pipeline.ErrNotFound
	}

	// Return a snapshot, not the shared pointer. A real SQL store scans a fresh
	// row each read; handing out the map's live *Job lets callers read fields
	// (e.g. Status) outside the lock while executeJob mutates them under it.
	jobCopy := *job
	return &jobCopy, nil
}

func (f *fakeStore) ListJobs(ctx context.Context, tenantID string, filter pipeline.JobFilter) ([]*pipeline.Job, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
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
	if f.beforeUpdateJobStatus != nil {
		f.beforeUpdateJobStatus(jobID, status)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.updateStatusErr != nil && (f.updateStatusErrOn == "" || f.updateStatusErrOn == status) {
		return f.updateStatusErr
	}

	job, exists := f.jobs[jobID]
	if !exists {
		return pipeline.ErrNotFound
	}

	job.Status = status
	job.ErrorMessage = ""
	if jobErr != nil {
		job.ErrorMessage = jobErr.Error()
		f.failures[jobID] = jobErr
	}
	job.ErrorClass = pipeline.ClassifyJobError(jobErr)
	return nil
}

func (f *fakeStore) CreateStages(ctx context.Context, jobID string, stageNames []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
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
	if err := ctx.Err(); err != nil {
		return err
	}
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
			if f.afterUpdateStageStatus != nil {
				f.mu.Unlock()
				f.afterUpdateStageStatus(jobID, stageName, status)
				f.mu.Lock()
			}
			return nil
		}
	}

	return pipeline.ErrNotFound
}

func (f *fakeStore) SetStagePanelRecord(ctx context.Context, jobID, stageName, record string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, stage := range f.stages[jobID] {
		if stage.StageName == stageName {
			stage.PanelRecord = record
			return nil
		}
	}
	return pipeline.ErrNotFound
}

func (f *fakeStore) UpdateStageError(ctx context.Context, jobID, stageName string, stageErr error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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
	if err := ctx.Err(); err != nil {
		return err
	}
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
			stage.CacheAttempt = ""
			stage.StageRecord = ""
			stage.RetryCount++
		}
	}

	return nil
}

func (f *fakeStore) CompleteAnalysisStage(ctx context.Context, jobID, analyzerName string, resultData []byte, cacheAttempt, stageRecord string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
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
		if stage.StageName != analyzerName {
			continue
		}
		stage.Status = pipeline.StageStatusCompleted
		stage.CacheAttempt = cacheAttempt
		stage.StageRecord = stageRecord
		found = true
		break
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

func (f *fakeStore) JobsCreatedBy(ctx context.Context, jobIDs []string, user string) ([]string, error) {
	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var owned []string
	for _, id := range jobIDs {
		if j, ok := f.jobs[id]; ok && user != "" && j.TenantID == tenantID && j.CreatedBy == user {
			owned = append(owned, id)
		}
	}
	slices.Sort(owned)
	return owned, nil
}

func (f *fakeStore) GetCompletedAnalysisResults(ctx context.Context, jobID string, analyzerNames []string) (map[string]*analyzer.Output, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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

// GetStageProgress returns, as PGStore does under RLS, the stored progress
// of jobID's stages when the job is the ctx tenant's, and none otherwise.
func (f *fakeStore) GetStageProgress(ctx context.Context, jobID string) (map[string]pipeline.StageProgress, error) {
	if jobID == "" {
		return nil, pipeline.ErrInvalidJobID
	}
	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.getProgressErr != nil {
		return nil, f.getProgressErr
	}
	out := make(map[string]pipeline.StageProgress)
	if job, ok := f.jobs[jobID]; !ok || job.TenantID != tenantID {
		return out, nil
	}
	for name, p := range f.progress[jobID] {
		out[name] = p
	}
	return out, nil
}

// setStageProgress stores p as jobID's progress of stage stageName.
func (f *fakeStore) setStageProgress(jobID, stageName string, p pipeline.StageProgress) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.progress[jobID] == nil {
		f.progress[jobID] = make(map[string]pipeline.StageProgress)
	}
	f.progress[jobID][stageName] = p
}

func (f *fakeStore) WriteVoteSummaries(ctx context.Context, tenantID, jobID string, pairs []pipeline.VoteSummaryPair) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.writeVoteSummErr != nil {
		return f.writeVoteSummErr
	}
	if err := tenant.ValidateTenantID(tenantID); err != nil {
		return err
	}

	if f.voteSummaries[jobID] == nil {
		f.voteSummaries[jobID] = make(map[[3]string]pipeline.VoteSummaryPair)
	}
	for _, p := range pairs {
		key := [3]string{p.SourceID, p.TargetID, p.Analyzer}
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
		var err error
		ctx, err = tenant.WithTenant(context.Background(), "tenant-1")
		Expect(err).NotTo(HaveOccurred())
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

		It("answers another tenant's job as not found, and refuses a ctx without a tenant", func() {
			Expect(store.CreateJob(ctx, &pipeline.Job{JobID: "job-1", TenantID: "tenant-1"})).To(Succeed())
			otherCtx, err := tenant.WithTenant(context.Background(), "tenant-2")
			Expect(err).NotTo(HaveOccurred())
			_, err = store.GetJob(otherCtx, "job-1")
			Expect(err).To(MatchError(pipeline.ErrNotFound))
			_, err = store.GetJob(context.Background(), "job-1")
			Expect(err).To(MatchError(tenant.ErrNoTenant))
		})
	})

	Describe("GetStageProgress", func() {
		BeforeEach(func() {
			Expect(store.CreateJob(ctx, &pipeline.Job{JobID: "job-1", TenantID: "tenant-1"})).To(Succeed())
			store.setStageProgress("job-1", "classify", pipeline.StageProgress{Total: 3, Done: 1, Succeeded: 1})
		})

		It("returns the stored progress of the ctx tenant's job", func() {
			got, err := store.GetStageProgress(ctx, "job-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(map[string]pipeline.StageProgress{"classify": {Total: 3, Done: 1, Succeeded: 1}}))
		})

		It("returns none for another tenant's job", func() {
			otherCtx, err := tenant.WithTenant(context.Background(), "tenant-2")
			Expect(err).NotTo(HaveOccurred())
			got, err := store.GetStageProgress(otherCtx, "job-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(BeEmpty())
		})

		It("refuses a ctx without a tenant and an empty job ID", func() {
			_, err := store.GetStageProgress(context.Background(), "job-1")
			Expect(err).To(MatchError(tenant.ErrNoTenant))
			_, err = store.GetStageProgress(ctx, "")
			Expect(err).To(MatchError(pipeline.ErrInvalidJobID))
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

var _ = Describe("PGStore.ResetStagesFrom", func() {
	It("clears the stage record of every stage it resets, so a rerun is not judged by the old prompt", func() {
		ctx, err := tenant.WithTenant(context.Background(), "tenant-1")
		Expect(err).NotTo(HaveOccurred())
		var resetQueries []string
		mockDB := &dbtest.Conn{Tx: &dbtest.Tx{
			ExecFunc: func(ctx context.Context, query string, args ...any) error {
				if strings.Contains(query, "UPDATE job_stages") {
					resetQueries = append(resetQueries, query)
				}
				return nil
			},
		}}

		Expect(pipeline.NewPGStore(mockDB, nil).ResetStagesFrom(ctx, "job-1", "a", []string{"a", "b"})).To(Succeed())

		Expect(resetQueries).To(HaveLen(2))
		for _, q := range resetQueries {
			Expect(q).To(ContainSubstring("stage_record = NULL"))
		}
	})
})

var _ = Describe("CompleteAnalysisStage", func() {
	var (
		mockDB *dbtest.Conn
		store  *pipeline.PGStore
		ctx    context.Context
	)

	BeforeEach(func() {
		mockDB = &dbtest.Conn{}
		store = pipeline.NewPGStore(mockDB, nil)

		var err error
		ctx, err = tenant.WithTenant(context.Background(), "tenant-1")
		Expect(err).NotTo(HaveOccurred())
	})

	It("upserts analysis_results and marks the stage completed in one transaction", func() {
		var queries []string
		mockTx := &dbtest.Tx{
			ExecFunc: func(ctx context.Context, query string, args ...any) error {
				queries = append(queries, query)
				return nil
			},
		}
		mockDB.Tx = mockTx

		err := store.CompleteAnalysisStage(ctx, "job-1", "requires", []byte(`[]`), "", "")
		Expect(err).NotTo(HaveOccurred())

		// SET tenant + upsert + stage update, all in the same transaction.
		Expect(queries).To(HaveLen(3))
		Expect(queries[1]).To(ContainSubstring("analysis_results"))
		Expect(queries[1]).To(ContainSubstring("ON CONFLICT (tenant_id, job_id, analyzer_name)"))
		Expect(queries[2]).To(ContainSubstring("job_stages"))
	})

	DescribeTable("marks the stage completed with the attempt that sealed it, or NULL",
		func(attempt string, want OmegaMatcher) {
			var stageArgs []any
			mockDB.Tx = &dbtest.Tx{
				ExecFunc: func(ctx context.Context, query string, args ...any) error {
					if strings.Contains(query, "job_stages") {
						Expect(query).To(ContainSubstring("cache_attempt"))
						stageArgs = args
					}
					return nil
				},
			}
			Expect(store.CompleteAnalysisStage(ctx, "job-1", "requires", []byte(`[]`), attempt, "")).To(Succeed())
			Expect(stageArgs).To(ContainElement(want))
		},
		Entry("a sealed attempt", "0b6f2b4e-6c1a-4f43-9b8e-6d2c1f0a9e11", Equal("0b6f2b4e-6c1a-4f43-9b8e-6d2c1f0a9e11")),
		Entry("no attempt", "", BeNil()),
	)

	DescribeTable("marks the stage completed with its stage record, or NULL",
		func(record string, want OmegaMatcher) {
			var stageQuery string
			var stageArgs []any
			mockDB.Tx = &dbtest.Tx{
				ExecFunc: func(ctx context.Context, query string, args ...any) error {
					if strings.Contains(query, "job_stages") {
						stageQuery, stageArgs = query, args
					}
					return nil
				},
			}
			Expect(store.CompleteAnalysisStage(ctx, "job-1", "requires", []byte(`[]`), "", record)).To(Succeed())
			Expect(stageQuery).To(ContainSubstring("stage_record = "))
			Expect(stageArgs).To(ContainElement(want))
		},
		Entry("a record", `{"prompt_name":"requires","prompt_version":"1","models":{}}`,
			Equal(`{"prompt_name":"requires","prompt_version":"1","models":{}}`)),
		Entry("no record", "", BeNil()),
	)

	It("rolls back and returns an error when the upsert fails", func() {
		mockTx := &dbtest.Tx{
			ExecFunc: func(ctx context.Context, query string, args ...any) error {
				if strings.Contains(query, "analysis_results") {
					return errors.New("insert failed")
				}
				return nil
			},
		}
		mockDB.Tx = mockTx

		err := store.CompleteAnalysisStage(ctx, "job-1", "requires", []byte(`[]`), "", "")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("upserting result"))
		Expect(mockTx.Rollbacks()).To(Equal(1))
	})

	It("does not update job_stages when the upsert fails", func() {
		var stageUpdated bool
		mockTx := &dbtest.Tx{
			ExecFunc: func(ctx context.Context, query string, args ...any) error {
				if strings.Contains(query, "analysis_results") {
					return errors.New("insert failed")
				}
				if strings.Contains(query, "job_stages") {
					stageUpdated = true
				}
				return nil
			},
		}
		mockDB.Tx = mockTx

		err := store.CompleteAnalysisStage(ctx, "job-1", "requires", []byte(`[]`), "", "")
		Expect(err).To(HaveOccurred())
		Expect(stageUpdated).To(BeFalse())
	})

	It("returns an error when the job_stages update fails", func() {
		mockTx := &dbtest.Tx{
			ExecFunc: func(ctx context.Context, query string, args ...any) error {
				if strings.Contains(query, "job_stages") {
					return errors.New("update failed")
				}
				return nil
			},
		}
		mockDB.Tx = mockTx

		err := store.CompleteAnalysisStage(ctx, "job-1", "requires", []byte(`[]`), "", "")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("updating stage status"))
	})

	It("returns an error when jobID is empty", func() {
		err := store.CompleteAnalysisStage(ctx, "", "requires", []byte(`[]`), "", "")
		Expect(err).To(HaveOccurred())
	})

	It("returns an error when commit fails", func() {
		mockTx := &dbtest.Tx{
			ExecFunc:  acceptExec,
			CommitErr: errors.New("commit failed"),
		}
		mockDB.Tx = mockTx

		err := store.CompleteAnalysisStage(ctx, "job-1", "requires", []byte(`[]`), "", "")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("commit failed"))
	})
})

var _ = Describe("PGStore stage progress", func() {
	var (
		mockDB *dbtest.Conn
		store  *pipeline.PGStore
		ctx    context.Context
	)

	BeforeEach(func() {
		mockDB = &dbtest.Conn{}
		store = pipeline.NewPGStore(mockDB, nil)
		var err error
		ctx, err = tenant.WithTenant(context.Background(), "tenant-1")
		Expect(err).NotTo(HaveOccurred())
	})

	Describe("AddStageProgress", func() {
		It("adds the delta to the stage's row in the ctx tenant, overwriting only the total", func() {
			last := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
			var query string
			var args []any
			mockDB.Tx = &dbtest.Tx{
				ExecFunc: acceptExec,
				QueryRowFunc: func(_ context.Context, q string, a ...any) db.Row {
					query, args = q, a
					return dbtest.Row(nil, "tenant-1")
				},
			}

			Expect(store.AddStageProgress(ctx, "job-1", "classify", analysis.ProgressDelta{
				Total: 10, Succeeded: 3, Failed: 1, Retried: 2, CacheHits: 1, LastSuccessAt: &last,
			})).To(Succeed())

			Expect(mockDB.Tx.Execs()[0].Args).To(ConsistOf("tenant-1"), "enters the ctx tenant first")
			Expect(query).To(ContainSubstring("ON CONFLICT (tenant_id, job_id, stage_name) DO UPDATE"))
			Expect(query).To(ContainSubstring("tasks_total = EXCLUDED.tasks_total"))
			Expect(query).To(ContainSubstring("tasks_succeeded = job_stage_progress.tasks_succeeded + EXCLUDED.tasks_succeeded"))
			Expect(query).To(ContainSubstring("GREATEST(job_stage_progress.last_success_at, EXCLUDED.last_success_at)"))
			Expect(args).To(Equal([]any{"tenant-1", "job-1", "classify", 10, 4, 3, 1, 2, 1, &last}),
				"done is succeeded plus failed")
			Expect(mockDB.Tx.Commits()).To(Equal(1))
		})

		It("refuses a job the ctx tenant does not have, writing nothing", func() {
			mockDB.Tx = &dbtest.Tx{
				ExecFunc:     acceptExec,
				QueryRowFunc: func(context.Context, string, ...any) db.Row { return dbtest.Row(sql.ErrNoRows) },
			}

			err := store.AddStageProgress(ctx, "job-other", "classify", analysis.ProgressDelta{Total: 1, Succeeded: 1})
			Expect(err).To(MatchError(pipeline.ErrNotFound))
			Expect(err.Error()).To(ContainSubstring(`"job-other"`))
			Expect(mockDB.Tx.Commits()).To(BeZero())
		})

		It("fails closed without a tenant, before touching the database", func() {
			err := store.AddStageProgress(context.Background(), "job-1", "classify", analysis.ProgressDelta{Total: 1})
			Expect(err).To(MatchError(tenant.ErrNoTenant))
			Expect(mockDB.Begins()).To(BeZero())
		})

		It("rejects an empty job ID", func() {
			Expect(store.AddStageProgress(ctx, "", "classify", analysis.ProgressDelta{})).To(MatchError(pipeline.ErrInvalidJobID))
			Expect(mockDB.Begins()).To(BeZero())
		})

		It("returns the write's error and does not commit", func() {
			mockDB.Tx = &dbtest.Tx{
				ExecFunc:     acceptExec,
				QueryRowFunc: func(context.Context, string, ...any) db.Row { return dbtest.Row(errors.New("disk full")) },
			}
			err := store.AddStageProgress(ctx, "job-1", "classify", analysis.ProgressDelta{Total: 1})
			Expect(err).To(MatchError(ContainSubstring("disk full")))
			Expect(mockDB.Tx.Commits()).To(BeZero())
		})
	})

	Describe("GetStageProgress", func() {
		It("returns each stage's counts keyed by stage name, in the ctx tenant", func() {
			last := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
			var args []any
			mockDB.Tx = &dbtest.Tx{
				ExecFunc: acceptExec,
				QueryFunc: func(_ context.Context, _ string, a ...any) (db.Rows, error) {
					args = a
					return dbtest.Rows(nil,
						[]any{"classify", 10, 4, 3, 1, 2, 1, &last},
						[]any{"requires", 5, 0, 0, 0, 0, 0, (*time.Time)(nil)},
					), nil
				},
			}

			got, err := store.GetStageProgress(ctx, "job-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(args).To(Equal([]any{"job-1", "tenant-1"}))
			Expect(got).To(Equal(map[string]pipeline.StageProgress{
				"classify": {Total: 10, Done: 4, Succeeded: 3, Failed: 1, Retried: 2, CacheHits: 1, LastSuccessAt: &last},
				"requires": {Total: 5},
			}))
		})

		It("returns an empty map for a job with no progress rows", func() {
			mockDB.Tx = &dbtest.Tx{
				ExecFunc:  acceptExec,
				QueryFunc: func(context.Context, string, ...any) (db.Rows, error) { return dbtest.Rows(nil), nil },
			}
			got, err := store.GetStageProgress(ctx, "job-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(BeEmpty())
			Expect(got).NotTo(BeNil())
		})

		It("fails closed without a tenant, before touching the database", func() {
			_, err := store.GetStageProgress(context.Background(), "job-1")
			Expect(err).To(MatchError(tenant.ErrNoTenant))
			Expect(mockDB.Begins()).To(BeZero())
		})

		It("rejects an empty job ID", func() {
			_, err := store.GetStageProgress(ctx, "")
			Expect(err).To(MatchError(pipeline.ErrInvalidJobID))
		})
	})
})
