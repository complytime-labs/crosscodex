package pipeline

import (
	"context"
	"time"

	"github.com/complytime-labs/crosscodex/internal/synthesis"
)

type JobStatus string

const (
	JobStatusPending   JobStatus = "pending"
	JobStatusRunning   JobStatus = "running"
	JobStatusCompleted JobStatus = "completed"
	JobStatusFailed    JobStatus = "failed"
	JobStatusCancelled JobStatus = "cancelled"
)

type StageStatus string

const (
	StageStatusPending   StageStatus = "pending"
	StageStatusRunning   StageStatus = "running"
	StageStatusCompleted StageStatus = "completed"
	StageStatusFailed    StageStatus = "failed"
)

type Job struct {
	JobID        string
	TenantID     string
	Status       JobStatus
	Config       []byte
	CreatedBy    string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	ErrorMessage string
}

type Stage struct {
	JobID        string
	StageName    string
	Status       StageStatus
	StartedAt    *time.Time
	CompletedAt  *time.Time
	RetryCount   int
	ErrorMessage string
	TenantID     string
}

type JobFilter struct {
	Status JobStatus
	Limit  int
	Offset int
}

type SynthesisExecutor interface {
	Execute(ctx context.Context, jobID string, inputs []synthesis.SynthesisInput,
		classifications map[string]synthesis.Classification) (*synthesis.ExecuteResult, error)
}
