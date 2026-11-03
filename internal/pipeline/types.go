package pipeline

import (
	"context"
	"time"

	"github.com/complytime-labs/crosscodex/internal/synthesis"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
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
	// StageStatusCancelled marks a stage that was running when its job was
	// cancelled: it neither completed nor failed on its own.
	StageStatusCancelled StageStatus = "cancelled"
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
	// ErrorClass classifies ErrorMessage's cause; see ClassifyJobError.
	ErrorClass JobErrorClass
}

// JobCreator returns the created_by of a job created in ctx: the calling
// user, else tenantID for a system call that carries no user. The jobs RLS
// policy refuses a row whose created_by differs from a set app.current_user,
// and the gateway lists a non-admin only the jobs it created.
func JobCreator(ctx context.Context, tenantID string) string {
	if user := tenant.UserFromContext(ctx); user != "" {
		return user
	}
	return tenantID
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
	// CacheAttempt is the result-cache attempt whose checkpoint sealed this
	// analyzer stage's completion (analysis.MetadataCacheAttempt), or "".
	// Untrusted: a reader verifies the checkpoint it names, and a database
	// writer could repoint it to another signed attempt of the same job and
	// analyzer.
	CacheAttempt string
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
