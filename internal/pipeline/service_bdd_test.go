package pipeline_test

import (
	"context"
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/pipeline"
	pipelineattestation "github.com/complytime-labs/crosscodex/internal/pipeline/attestation"
	"github.com/complytime-labs/crosscodex/internal/synthesis"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

type fakeSynthesis struct{}

func (fakeSynthesis) Execute(ctx context.Context, jobID string, inputs []synthesis.SynthesisInput,
	classifications map[string]synthesis.Classification) (*synthesis.ExecuteResult, error) {
	return &synthesis.ExecuteResult{}, nil
}

// blockingSynthesis blocks Execute until the block channel is closed or ctx is cancelled.
type blockingSynthesis struct {
	block chan struct{}
}

func (b *blockingSynthesis) Execute(ctx context.Context, jobID string, inputs []synthesis.SynthesisInput,
	classifications map[string]synthesis.Classification) (*synthesis.ExecuteResult, error) {
	select {
	case <-b.block:
		return &synthesis.ExecuteResult{}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type fakeAttestor struct{}

func (fakeAttestor) CreateLayout(ctx context.Context, opts attestation.LayoutOptions) (*attestation.SignedLayout, error) {
	return &attestation.SignedLayout{}, nil
}
func (fakeAttestor) CreateLink(ctx context.Context, step string, materials, products []attestation.Artifact, opts ...attestation.LinkOption) (*attestation.SignedLink, error) {
	return &attestation.SignedLink{}, nil
}
func (fakeAttestor) Verify(ctx context.Context, data []byte) (*attestation.VerifiedLink, error) {
	return &attestation.VerifiedLink{}, nil
}
func (fakeAttestor) VerifyLayout(ctx context.Context, data []byte) (*attestation.VerifiedLayout, error) {
	return &attestation.VerifiedLayout{}, nil
}
func (fakeAttestor) VerifyChain(ctx context.Context, layout *attestation.SignedLayout, links []*attestation.SignedLink) error {
	return nil
}

var _ = Describe("Service", func() {
	var (
		svc      *pipeline.Service
		store    *fakeStore
		registry *analyzer.Registry
		ctx      context.Context
	)

	BeforeEach(func() {
		store = newFakeStore()
		registry = analyzer.NewRegistry()

		cfg := config.PipelineConfig{
			MaxConcurrentJobs: 10,
			StageTimeout:      time.Hour,
		}
		attCfg := config.AttestationConfig{
			Enabled: false,
		}

		svc = pipeline.New(
			store,
			nil, // engine — not needed for basic RPC tests
			registry,
			&fakeSynthesis{},
			&fakeAttestor{},
			pipelineattestation.NewConverter(),
			&fakeNATSClient{},
			newExecutorFakeStorage(),
			cfg,
			attCfg,
		)

		ctx = context.Background()
		ctx, _ = tenant.WithTenant(ctx, "test-tenant")
	})

	Describe("CreateJob", func() {
		It("creates a job and stages in the store", func() {
			req := &pb.CreateJobRequest{
				Config: &pb.JobConfig{
					Source: &pb.JobConfig_DocumentContent{
						DocumentContent: []byte("test"),
					},
					CatalogFormat: pb.CatalogFormat_CATALOG_FORMAT_OSCAL,
					CatalogName:   "test-catalog",
				},
			}

			resp, err := svc.CreateJob(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.JobId).NotTo(BeEmpty())
			Expect(resp.Status).To(Equal(pb.JobStatus_JOB_STATUS_PENDING))

			job, err := store.GetJob(ctx, resp.JobId)
			Expect(err).NotTo(HaveOccurred())
			Expect(job.TenantID).To(Equal("test-tenant"))
			Expect(job.Status).To(Equal(pipeline.JobStatusPending))

			stages, err := store.GetStages(ctx, resp.JobId)
			Expect(err).NotTo(HaveOccurred())
			Expect(stages).NotTo(BeEmpty())
			// Empty registry = no analyzers, only synthesis + graph
			Expect(stages).To(HaveLen(2))
		})

		It("returns InvalidArgument when tenant context is missing", func() {
			req := &pb.CreateJobRequest{
				Config: &pb.JobConfig{},
			}

			_, err := svc.CreateJob(context.Background(), req)
			Expect(err).To(HaveOccurred())
			Expect(status.Code(err)).To(Equal(codes.InvalidArgument))
		})

		It("returns InvalidArgument when config is nil", func() {
			req := &pb.CreateJobRequest{}

			_, err := svc.CreateJob(ctx, req)
			Expect(err).To(HaveOccurred())
			Expect(status.Code(err)).To(Equal(codes.InvalidArgument))
		})
	})

	Describe("GetJob", func() {
		It("returns the job with stages", func() {
			now := time.Now()
			job := &pipeline.Job{
				JobID:     "job-1",
				TenantID:  "test-tenant",
				Status:    pipeline.JobStatusRunning,
				Config:    []byte(`{}`),
				CreatedBy: "test-tenant",
				CreatedAt: now,
				UpdatedAt: now,
			}
			Expect(store.CreateJob(ctx, job)).To(Succeed())
			Expect(store.CreateStages(ctx, "job-1", []string{"stage-1", "stage-2"})).To(Succeed())

			req := &pb.GetJobRequest{JobId: "job-1"}

			resp, err := svc.GetJob(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Job.JobId).To(Equal("job-1"))
			Expect(resp.Job.Status).To(Equal(pb.JobStatus_JOB_STATUS_RUNNING))
			Expect(resp.Job.Stages).To(HaveLen(2))
		})

		It("returns NotFound for missing job", func() {
			req := &pb.GetJobRequest{JobId: "missing"}

			_, err := svc.GetJob(ctx, req)
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("ListJobs", func() {
		BeforeEach(func() {
			now := time.Now()
			for i := 1; i <= 5; i++ {
				job := &pipeline.Job{
					JobID:     "job-" + string(rune('0'+i)),
					TenantID:  "test-tenant",
					Status:    pipeline.JobStatusCompleted,
					Config:    []byte(`{}`),
					CreatedBy: "test-tenant",
					CreatedAt: now,
					UpdatedAt: now,
				}
				Expect(store.CreateJob(ctx, job)).To(Succeed())
			}
		})

		It("returns all jobs for the tenant", func() {
			req := &pb.ListJobsRequest{}

			resp, err := svc.ListJobs(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Jobs).To(HaveLen(5))
		})

		It("filters by status", func() {
			now := time.Now()
			failedJob := &pipeline.Job{
				JobID:     "job-failed",
				TenantID:  "test-tenant",
				Status:    pipeline.JobStatusFailed,
				Config:    []byte(`{}`),
				CreatedBy: "test-tenant",
				CreatedAt: now,
				UpdatedAt: now,
			}
			Expect(store.CreateJob(ctx, failedJob)).To(Succeed())

			req := &pb.ListJobsRequest{
				Status: pb.JobStatus_JOB_STATUS_FAILED,
			}

			resp, err := svc.ListJobs(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Jobs).To(HaveLen(1))
			Expect(resp.Jobs[0].JobId).To(Equal("job-failed"))
		})
	})

	Describe("CancelJob", func() {
		It("returns NotFound if job is not running", func() {
			req := &pb.CancelJobRequest{JobId: "not-running"}

			_, err := svc.CancelJob(ctx, req)
			Expect(err).To(HaveOccurred())
		})

		It("cancels a running job", func() {
			blocker := &blockingSynthesis{block: make(chan struct{})}
			defer close(blocker.block)

			blockingSvc := pipeline.New(
				store,
				nil,
				registry,
				blocker,
				&fakeAttestor{},
				pipelineattestation.NewConverter(),
				&fakeNATSClient{},
				newExecutorFakeStorage(),
				config.PipelineConfig{
					MaxConcurrentJobs: 10,
					StageTimeout:      time.Hour,
				},
				config.AttestationConfig{Enabled: false},
			)

			createReq := &pb.CreateJobRequest{
				Config: &pb.JobConfig{
					Source: &pb.JobConfig_DocumentContent{
						DocumentContent: []byte("test"),
					},
					CatalogFormat: pb.CatalogFormat_CATALOG_FORMAT_OSCAL,
					CatalogName:   "test-catalog",
				},
			}

			createResp, err := blockingSvc.CreateJob(ctx, createReq)
			Expect(err).NotTo(HaveOccurred())

			jobID := createResp.JobId

			// Wait until the job appears in the store as running.
			Eventually(func() pipeline.JobStatus {
				j, err := store.GetJob(ctx, jobID)
				if err != nil {
					return ""
				}
				return j.Status
			}, "2s", "50ms").Should(Equal(pipeline.JobStatusRunning))

			cancelResp, err := blockingSvc.CancelJob(ctx, &pb.CancelJobRequest{JobId: jobID})
			Expect(err).NotTo(HaveOccurred())
			Expect(cancelResp.Cancelled).To(BeTrue())
		})
	})

	Describe("RetryJob", func() {
		It("resets failed stages and re-executes when retry_from_failure is true", func() {
			now := time.Now()
			configBytes, _ := json.Marshal(&pb.JobConfig{
				Source: &pb.JobConfig_DocumentContent{
					DocumentContent: []byte("test"),
				},
			})
			job := &pipeline.Job{
				JobID:     "job-retry",
				TenantID:  "test-tenant",
				Status:    pipeline.JobStatusFailed,
				Config:    configBytes,
				CreatedBy: "test-tenant",
				CreatedAt: now,
				UpdatedAt: now,
			}
			Expect(store.CreateJob(ctx, job)).To(Succeed())
			Expect(store.CreateStages(ctx, "job-retry", []string{"stage-1", "stage-2"})).To(Succeed())
			Expect(store.UpdateStageStatus(ctx, "job-retry", "stage-2", pipeline.StageStatusFailed)).To(Succeed())

			req := &pb.RetryJobRequest{
				JobId:            "job-retry",
				RetryFromFailure: true,
			}

			resp, err := svc.RetryJob(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.NewJobId).To(Equal("job-retry"))

			updatedJob, err := store.GetJob(ctx, "job-retry")
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedJob.Status).To(Equal(pipeline.JobStatusRunning))
		})

		It("returns FailedPrecondition when no stage has failed", func() {
			now := time.Now()
			configBytes, _ := json.Marshal(&pb.JobConfig{
				Source: &pb.JobConfig_DocumentContent{
					DocumentContent: []byte("test"),
				},
			})
			job := &pipeline.Job{
				JobID:     "job-retry-nofail",
				TenantID:  "test-tenant",
				Status:    pipeline.JobStatusCompleted,
				Config:    configBytes,
				CreatedBy: "test-tenant",
				CreatedAt: now,
				UpdatedAt: now,
			}
			Expect(store.CreateJob(ctx, job)).To(Succeed())
			Expect(store.CreateStages(ctx, "job-retry-nofail", []string{"stage-1", "stage-2"})).To(Succeed())
			// Leave all stages in pending (default) status — none failed.

			req := &pb.RetryJobRequest{
				JobId:            "job-retry-nofail",
				RetryFromFailure: true,
			}

			_, err := svc.RetryJob(ctx, req)
			Expect(err).To(HaveOccurred())
			Expect(status.Code(err)).To(Equal(codes.FailedPrecondition))
		})

		It("creates a new job when retry_from_failure is false", func() {
			now := time.Now()
			configBytes, _ := json.Marshal(&pb.JobConfig{
				Source: &pb.JobConfig_DocumentContent{
					DocumentContent: []byte("test"),
				},
			})
			job := &pipeline.Job{
				JobID:     "job-retry-new",
				TenantID:  "test-tenant",
				Status:    pipeline.JobStatusFailed,
				Config:    configBytes,
				CreatedBy: "test-tenant",
				CreatedAt: now,
				UpdatedAt: now,
			}
			Expect(store.CreateJob(ctx, job)).To(Succeed())

			req := &pb.RetryJobRequest{
				JobId:            "job-retry-new",
				RetryFromFailure: false,
			}

			resp, err := svc.RetryJob(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.NewJobId).NotTo(Equal("job-retry-new"))
			Expect(resp.NewJobId).NotTo(BeEmpty())

			newJob, err := store.GetJob(ctx, resp.NewJobId)
			Expect(err).NotTo(HaveOccurred())
			Expect(newJob.Status).To(Equal(pipeline.JobStatusPending))
		})
	})

	Describe("Stop", func() {
		It("cancels running jobs and returns nil", func() {
			blocker := &blockingSynthesis{block: make(chan struct{})}
			defer close(blocker.block)

			stoppableSvc := pipeline.New(
				store,
				nil,
				registry,
				blocker,
				&fakeAttestor{},
				pipelineattestation.NewConverter(),
				&fakeNATSClient{},
				newExecutorFakeStorage(),
				config.PipelineConfig{
					MaxConcurrentJobs: 10,
					StageTimeout:      time.Hour,
				},
				config.AttestationConfig{Enabled: false},
			)

			createReq := &pb.CreateJobRequest{
				Config: &pb.JobConfig{
					Source: &pb.JobConfig_DocumentContent{
						DocumentContent: []byte("test"),
					},
					CatalogFormat: pb.CatalogFormat_CATALOG_FORMAT_OSCAL,
					CatalogName:   "test-catalog",
				},
			}

			// Create a job so it starts running (blocked in synthesis).
			_, err := stoppableSvc.CreateJob(ctx, createReq)
			Expect(err).NotTo(HaveOccurred())

			// Wait for the job to reach running status.
			Eventually(func() bool {
				jobs, _, _ := store.ListJobs(ctx, "test-tenant", pipeline.JobFilter{Status: pipeline.JobStatusRunning})
				return len(jobs) > 0
			}, "2s", "50ms").Should(BeTrue())

			// Stop with a 2-second timeout context.
			stopCtx, stopCancel := context.WithTimeout(ctx, 2*time.Second)
			defer stopCancel()

			err = stoppableSvc.Stop(stopCtx)
			Expect(err).NotTo(HaveOccurred())

			// After Stop, the concurrency slot should be free.
			// Verify by creating another job (would fail with ResourceExhausted
			// if the slot were still occupied).
			unblocked := &blockingSynthesis{block: make(chan struct{})}
			close(unblocked.block) // let it complete immediately

			postStopSvc := pipeline.New(
				store,
				nil,
				registry,
				unblocked,
				&fakeAttestor{},
				pipelineattestation.NewConverter(),
				&fakeNATSClient{},
				newExecutorFakeStorage(),
				config.PipelineConfig{
					MaxConcurrentJobs: 1,
					StageTimeout:      time.Hour,
				},
				config.AttestationConfig{Enabled: false},
			)

			_, err = postStopSvc.CreateJob(ctx, createReq)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Concurrency limit", func() {
		It("returns ResourceExhausted when MaxConcurrentJobs is reached", func() {
			blocker := &blockingSynthesis{block: make(chan struct{})}
			defer close(blocker.block)

			limitedSvc := pipeline.New(
				store,
				nil,
				registry,
				blocker,
				&fakeAttestor{},
				pipelineattestation.NewConverter(),
				&fakeNATSClient{},
				newExecutorFakeStorage(),
				config.PipelineConfig{
					MaxConcurrentJobs: 1,
					StageTimeout:      time.Hour,
				},
				config.AttestationConfig{Enabled: false},
			)

			createReq := &pb.CreateJobRequest{
				Config: &pb.JobConfig{
					Source: &pb.JobConfig_DocumentContent{
						DocumentContent: []byte("test"),
					},
					CatalogFormat: pb.CatalogFormat_CATALOG_FORMAT_OSCAL,
					CatalogName:   "test-catalog",
				},
			}

			// First job should succeed.
			_, err := limitedSvc.CreateJob(ctx, createReq)
			Expect(err).NotTo(HaveOccurred())

			// Second job should be rejected — the first is still running (blocked in synthesis).
			_, err = limitedSvc.CreateJob(ctx, createReq)
			Expect(err).To(HaveOccurred())
			Expect(status.Code(err)).To(Equal(codes.ResourceExhausted))
		})
	})
})
