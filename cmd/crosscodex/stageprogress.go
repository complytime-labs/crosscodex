package main

import (
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	intanalyzer "github.com/complytime-labs/crosscodex/internal/analyzer"
)

// stageProgressJSON is a stage's task progress in run status --json, named
// as in the StageProgress message; last_success_at is "" when unset.
type stageProgressJSON struct {
	TasksTotal     int32  `json:"tasks_total"`
	TasksDone      int32  `json:"tasks_done"`
	TasksSucceeded int32  `json:"tasks_succeeded"`
	TasksFailed    int32  `json:"tasks_failed"`
	TasksRetried   int32  `json:"tasks_retried"`
	TasksCacheHits int32  `json:"tasks_cache_hits"`
	LastSuccessAt  string `json:"last_success_at"`
}

func stageProgressToJSON(p *pb.StageProgress) stageProgressJSON {
	return stageProgressJSON{
		TasksTotal: p.GetTasksTotal(), TasksDone: p.GetTasksDone(), TasksSucceeded: p.GetTasksSucceeded(),
		TasksFailed: p.GetTasksFailed(), TasksRetried: p.GetTasksRetried(), TasksCacheHits: p.GetTasksCacheHits(),
		LastSuccessAt: formatStageTime(p.GetLastSuccessAt()),
	}
}

// writeProgressLines prints the "Task progress:" section, one aligned row
// per stage with progress; nothing when no stage has any. The time since the
// last success is measured to now; a last success after now (a daemon clock
// ahead of this one) shows as 0s ago.
func writeProgressLines(w io.Writer, stages []*pb.JobStage, now time.Time) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	header := false
	for _, st := range stages {
		p := st.GetProgress()
		if p == nil {
			continue
		}
		if !header {
			fmt.Fprintf(w, "Task progress:\n")
			header = true
		}
		last := "never"
		if p.GetLastSuccessAt() != nil {
			last = max(now.Sub(p.GetLastSuccessAt().AsTime()), 0).Round(time.Second).String() + " ago"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%d/%d\tok %d\tcached %d\tfailed %d\tretried %d\tlast success %s\n",
			intanalyzer.CleanDisplay(st.GetStageName(), maxUntrustedDisplayBytes), formatJobStatus(st.GetStatus()),
			p.GetTasksDone(), p.GetTasksTotal(), p.GetTasksSucceeded(), p.GetTasksCacheHits(), p.GetTasksFailed(),
			p.GetTasksRetried(), last)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(w, "Error flushing output: %v\n", err)
	}
}
