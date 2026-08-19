package analysis

import (
	"context"
	"time"

	"google.golang.org/protobuf/proto"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
)

// Dispatcher publishes analyzer tasks to NATS work subjects.
type Dispatcher interface {
	Dispatch(ctx context.Context, tasks []analyzer.Task, taskType natsbus.TaskType, jobID string) error
	Redispatch(ctx context.Context, task analyzer.Task, taskType natsbus.TaskType, jobID string, retryCount int) error
}

// ExecutionRequest specifies which analyzers to run and their input.
type ExecutionRequest struct {
	JobID         string
	AnalyzerNames []string
	Input         proto.Message

	// Controls, when non-empty, makes executeAnalyzer call each analyzer's
	// GenerateWork once per control instead of once with Input. Every real
	// analyzer is registered as Analyzer[*pb.Control]; Task.TaskID already
	// embeds the control ID, so per-control task sets never collide.
	Controls []*pb.Control

	// CatalogID is threaded into context (via analyzer.WithCatalogID) before
	// GenerateWork/Aggregate for every analyzer in this request. Only
	// embedding.Aggregate reads it today. Empty for document-backed jobs.
	CatalogID string

	AnalyzerConfig map[string]analyzer.AnalyzerConfig

	// CompletedOutputs carries outputs of analyzers that already completed in a
	// prior Execute call for this same job (two-pass execution). Their entries
	// are pre-seeded as completed so they satisfy dependency gating for the
	// requested analyzers without being re-executed. A nil map is the common
	// single-pass case.
	CompletedOutputs map[string]*analyzer.Output
}

// ExecutionResult holds the outcome of an Engine.Execute call.
type ExecutionResult struct {
	JobID     string
	Outputs   map[string]*analyzer.Output
	Errors    map[string]error
	Completed []string
	Failed    []string
	Skipped   []string
}

// CollectRequest configures a Collector.Collect call.
type CollectRequest struct {
	TaskType    natsbus.TaskType
	JobID       string
	ExpectedIDs []string
	Tasks       []analyzer.Task
	Timeout     time.Duration
	MaxRetries  int
	Backoff     time.Duration
	Dispatcher  Dispatcher
}
