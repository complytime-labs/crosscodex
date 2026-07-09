package analysis

import (
	"context"
	"time"

	"google.golang.org/protobuf/proto"

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
	JobID          string
	AnalyzerNames  []string
	Input          proto.Message
	AnalyzerConfig map[string]analyzer.AnalyzerConfig
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
